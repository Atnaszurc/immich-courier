package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"immich-courier/internal/immich"
)

// fakeImmich mimics the OAuth, api-keys and logout endpoints and records
// what the client sent.
type fakeImmich struct {
	mu            sync.Mutex
	challenge     string
	state         string
	loggedOut     bool
	keyPerms      []string
	sessionTokens map[string]bool
}

func (f *fakeImmich) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.challenge, f.state = in["codeChallenge"], in["state"]
		f.mu.Unlock()
		// The "identity provider" immediately redirects back with a code.
		u := in["redirectUri"] + "?code=abc&state=" + url.QueryEscape(in["state"])
		json.NewEncoder(w).Encode(map[string]string{"url": u})
	})
	mux.HandleFunc("POST /api/oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		sum := sha256.Sum256([]byte(in["codeVerifier"]))
		f.mu.Lock()
		defer f.mu.Unlock()
		if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
			http.Error(w, `{"message":"PKCE mismatch"}`, 400)
			return
		}
		u, _ := url.Parse(in["url"])
		if u.Query().Get("code") != "abc" || in["state"] != f.state {
			http.Error(w, `{"message":"bad callback"}`, 400)
			return
		}
		f.sessionTokens["session-1"] = true
		json.NewEncoder(w).Encode(map[string]any{"accessToken": "session-1", "userEmail": "a@example.com", "name": "A"})
	})
	mux.HandleFunc("POST /api/api-keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer session-1" {
			http.Error(w, `{"message":"unauthorized"}`, 401)
			return
		}
		var in struct {
			Name        string   `json:"name"`
			Permissions []string `json:"permissions"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.keyPerms = in.Permissions
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{
			"secret": "the-secret",
			"apiKey": map[string]any{"id": "k1", "name": in.Name, "permissions": in.Permissions},
		})
	})
	mux.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.loggedOut = r.Header.Get("Authorization") == "Bearer session-1"
		f.mu.Unlock()
		w.Write([]byte(`{"successful":true}`))
	})
	return mux
}

func TestOIDCLoginAndMintKey(t *testing.T) {
	f := &fakeImmich{sessionTokens: map[string]bool{}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := immich.New(srv.URL)
	// Stand-in for the browser: follow the provider URL, which lands on the
	// loopback callback.
	browser := func(u string) error {
		go func() {
			resp, err := http.Get(u)
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	ctx := context.Background()
	s, err := OIDCLogin(ctx, c, OIDCOptions{Port: 47999, OpenBrowser: browser, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if s.Token != "session-1" || s.Email != "a@example.com" {
		t.Fatalf("unexpected session %+v", s)
	}

	key, err := MintAPIKey(ctx, srv.URL, s, "test")
	if err != nil {
		t.Fatal(err)
	}
	if key.Secret != "the-secret" {
		t.Fatalf("secret = %q", key.Secret)
	}
	if !f.loggedOut {
		t.Fatal("temporary session was not logged out")
	}
	if len(f.keyPerms) != len(Permissions) {
		t.Fatalf("key permissions = %v, want %v", f.keyPerms, Permissions)
	}
}

func TestOIDCLoginRejectsWrongState(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		json.NewEncoder(w).Encode(map[string]string{"url": in["redirectUri"] + "?code=abc&state=forged"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	browser := func(u string) error {
		go func() {
			if resp, err := http.Get(u); err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	_, err := OIDCLogin(context.Background(), immich.New(srv.URL), OIDCOptions{Port: 47998, OpenBrowser: browser, Timeout: time.Second})
	if err == nil {
		t.Fatal("a callback with a forged state was accepted")
	}
}

// A request to the callback port that isn't the answer to this login (no
// or wrong state) must not end the login or put its text in the result.
func TestOIDCCallbackIgnoresUnboundRequests(t *testing.T) {
	f := &fakeImmich{sessionTokens: map[string]bool{}}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	browser := func(u string) error {
		go func() {
			for _, q := range []string{"?error=access_denied&error_description=call+evil.example", "?state=nope&code=x"} {
				if resp, err := http.Get("http://127.0.0.1:47997/callback" + q); err == nil {
					resp.Body.Close()
				}
			}
			if resp, err := http.Get(u); err == nil { // the real provider round trip
				resp.Body.Close()
			}
		}()
		return nil
	}
	s, err := OIDCLogin(context.Background(), immich.New(srv.URL), OIDCOptions{Port: 47997, OpenBrowser: browser, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("login ended by an unbound callback: %v", err)
	}
	if s.Token == "" {
		t.Fatal("no session")
	}
}

func TestOpenBrowserOnlyWebURLs(t *testing.T) {
	for _, u := range []string{`C:\Windows\System32\calc.exe`, "file:///etc/passwd", "ms-settings:", "javascript:alert(1)", "https://"} {
		if err := OpenBrowser(u); err == nil {
			t.Errorf("OpenBrowser(%q) was allowed", u)
		}
	}
}
