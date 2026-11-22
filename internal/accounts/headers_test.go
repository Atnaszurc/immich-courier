package accounts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"immich-courier/internal/config"
	"immich-courier/internal/immich"
)

const proxyAuth = "Basic cHJveHk6c2VjcmV0" // proxy:secret

// behindProxy is an Immich stand-in behind a proxy that wants basic auth
// on every request, ping included.
func behindProxy() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != proxyAuth {
			w.Header().Set("WWW-Authenticate", `Basic realm="proxy"`)
			http.Error(w, "401 Authorization Required", 401)
			return
		}
		switch r.URL.Path {
		case "/api/server/ping":
			w.Write([]byte(`{"res":"pong"}`))
		case "/api/server/version":
			w.Write([]byte(`{"major":3,"minor":3,"patch":5}`))
		case "/api/server/features":
			w.Write([]byte(`{"oauth":false,"passwordLogin":true}`))
		case "/api/users/me":
			if r.Header.Get("x-api-key") != "good-key" {
				http.Error(w, `{"message":"Invalid API key"}`, 401)
				return
			}
			w.Write([]byte(`{"id":"u1","email":"a@example.com","name":"A"}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestHeadersStoredInKeychainOnly(t *testing.T) {
	keyring.MockInit()
	srv := behindProxy()
	defer srv.Close()
	ctx := context.Background()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	hs := []immich.Header{{Name: "Authorization", Value: proxyAuth}}

	// Without the header the probe explains what's in the way.
	if _, err := Probe(ctx, srv.URL, nil); err == nil || !strings.Contains(err.Error(), "custom headers") {
		t.Fatalf("probe without headers: %v", err)
	}
	if _, err := Probe(ctx, srv.URL, hs); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(ctx, cfgPath, "home", srv.URL, "good-key", hs); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(raw), "cHJveHk") || strings.Contains(strings.ToLower(string(raw)), "authorization") {
		t.Fatalf("header in the config file:\n%s", raw)
	}
	a := config.Account{Name: "home", Server: srv.URL}
	got, err := LoadHeaders(cfgPath, a)
	if err != nil || len(got) != 1 || got[0].Value != proxyAuth {
		t.Fatalf("LoadHeaders = %v, %v", got, err)
	}

	// Re-login without headers (nil) keeps them; an empty value keeps
	// that header's stored value.
	if r, _ := ResolveHeaders(cfgPath, "home", srv.URL, nil); len(r) != 1 || r[0].Value != proxyAuth {
		t.Fatalf("nil didn't keep the headers: %v", r)
	}
	r, err := ResolveHeaders(cfgPath, "home", srv.URL, []immich.Header{{Name: "authorization"}, {Name: "X-New", Value: "n"}})
	if err != nil || len(r) != 2 || r[0].Value != proxyAuth || r[1].Value != "n" {
		t.Fatalf("blank value didn't keep the stored one: %v %v", r, err)
	}
	// Stored values are never handed to another server or account name.
	other := "https://elsewhere.example"
	if _, err := ResolveHeaders(cfgPath, "home", other, []immich.Header{{Name: "Authorization"}}); err == nil {
		t.Fatal("stored value reused for another server")
	}
	if _, err := ResolveHeaders(cfgPath, "other", srv.URL, []immich.Header{{Name: "Authorization"}}); err == nil {
		t.Fatal("stored value reused for another account")
	}

	// SetHeaders checks the new set before saving it.
	if _, err := SetHeaders(ctx, cfgPath, "home", []immich.Header{{Name: "Authorization", Value: "Basic d3Jvbmc="}}); err == nil {
		t.Fatal("headers the proxy refuses were saved")
	}
	if got, _ := LoadHeaders(cfgPath, a); len(got) != 1 || got[0].Value != proxyAuth {
		t.Fatalf("refused change altered the stored headers: %v", got)
	}
	if _, err := SetHeaders(ctx, cfgPath, "home", []immich.Header{{Name: "Authorization"}, {Name: "X-Extra", Value: "e"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadHeaders(cfgPath, a); len(got) != 2 {
		t.Fatalf("after SetHeaders: %v", got)
	}

	// Removing the account removes the headers from the keychain too.
	if err := Remove(cfgPath, "home", false); err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Get(KeyringService, headersName(cfgPath, "home", srv.URL)); err == nil {
		t.Fatal("headers left in the keychain")
	}
}

// A damaged keychain entry is reported, not sent.
func TestDamagedHeadersEntry(t *testing.T) {
	keyring.MockInit()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	a := config.Account{Name: "home", Server: "https://photos.example"}
	keyring.Set(KeyringService, headersName(cfgPath, a.Name, a.Server), `[{"name":"X-Bad","value":"a\r\nb"}]`)
	if hs, err := LoadHeaders(cfgPath, a); err == nil {
		t.Fatalf("damaged entry loaded: %v", hs)
	}
}
