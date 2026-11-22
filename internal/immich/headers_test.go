package immich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidateHeaders(t *testing.T) {
	ok := [][]Header{
		nil,
		{{Name: "Authorization", Value: "Basic dXNlcjpwYXNz"}},
		{{Name: " X-Proxy-Token ", Value: " abc "}, {Name: "Cookie", Value: "CF_Authorization=x"}},
		{{Name: "X-Tab", Value: "a\tb"}},
	}
	for _, hs := range ok {
		if err := ValidateHeaders(hs); err != nil {
			t.Errorf("%v rejected: %v", hs, err)
		}
	}
	long := strings.Repeat("x", MaxHeaderBytes)
	bad := map[string][]Header{
		"empty name":        {{Name: "", Value: "v"}},
		"space in name":     {{Name: "X Token", Value: "v"}},
		"colon in name":     {{Name: "X:Token", Value: "v"}},
		"empty value":       {{Name: "X-Token", Value: "  "}},
		"CRLF in value":     {{Name: "X-Token", Value: "a\r\nX-Api-Key: stolen"}},
		"LF in value":       {{Name: "X-Token", Value: "a\nb"}},
		"NUL in value":      {{Name: "X-Token", Value: "a\x00b"}},
		"non-ASCII value":   {{Name: "X-Token", Value: "lösen"}},
		"duplicate":         {{Name: "X-Token", Value: "a"}, {Name: "x-token", Value: "b"}},
		"api key override":  {{Name: "X-Api-Key", Value: "other"}},
		"user token":        {{Name: "x-immich-user-token", Value: "t"}},
		"host":              {{Name: "Host", Value: "evil.example"}},
		"content-length":    {{Name: "Content-Length", Value: "0"}},
		"transfer-encoding": {{Name: "Transfer-Encoding", Value: "chunked"}},
		"accept-encoding":   {{Name: "Accept-Encoding", Value: "br"}},
		"too long":          {{Name: "X-Token", Value: long}},
	}
	for name, hs := range bad {
		if err := ValidateHeaders(hs); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "stolen") || strings.Contains(err.Error(), "dXNlcjpwYXNz") {
			t.Errorf("%s: error repeats the value: %v", name, err)
		}
	}
	many := make([]Header, MaxHeaders+1)
	for i := range many {
		many[i] = Header{Name: "X-H" + string(rune('a'+i)), Value: "v"}
	}
	if ValidateHeaders(many) == nil {
		t.Error("too many headers accepted")
	}
}

// recorder answers like Immich and records the headers of each request.
type recorder struct {
	mu   sync.Mutex
	seen map[string]http.Header // path -> headers
}

func (rec *recorder) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.seen[r.URL.Path] = r.Header.Clone()
		rec.mu.Unlock()
		switch r.URL.Path {
		case "/api/server/ping":
			w.Write([]byte(`{"res":"pong"}`))
		case "/api/assets":
			w.Write([]byte(`{"id":"a1","status":"created"}`))
		default:
			w.Write([]byte(`{"id":"u1"}`))
		}
	}))
}

func (rec *recorder) get(path string) http.Header {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.seen[path]
}

// Custom headers go with every request (ping and uploads too); Immich
// credentials don't go with ping, and custom headers can't replace them.
func TestCustomHeadersAreSent(t *testing.T) {
	rec := &recorder{seen: map[string]http.Header{}}
	srv := rec.server()
	defer srv.Close()
	c := New(srv.URL)
	c.APIKey = "the-key"
	c.Headers = []Header{{Name: "Authorization", Value: "Basic cHJveHk6cHc="}, {Name: "X-Gate", Value: "g"}}
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Me(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Upload(ctx, strings.NewReader("data"), "a.jpg", "abc", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/api/server/ping", "/api/users/me", "/api/assets"} {
		h := rec.get(p)
		if h.Get("Authorization") != "Basic cHJveHk6cHc=" || h.Get("X-Gate") != "g" {
			t.Errorf("%s: custom headers missing: %v", p, h)
		}
	}
	if k := rec.get("/api/server/ping").Get("x-api-key"); k != "" {
		t.Errorf("ping sent the API key")
	}
	if k := rec.get("/api/users/me").Get("x-api-key"); k != "the-key" {
		t.Errorf("x-api-key = %q", k)
	}
	if ck := rec.get("/api/assets").Get("x-immich-checksum"); ck != "abc" {
		t.Errorf("checksum header = %q", ck)
	}
}

// A session token uses Authorization: Bearer, unless a custom header has
// taken Authorization (proxy basic auth); then it goes in Immich's own
// x-immich-user-token header so both reach their target.
func TestSessionTokenWithCustomAuthorization(t *testing.T) {
	rec := &recorder{seen: map[string]http.Header{}}
	srv := rec.server()
	defer srv.Close()
	ctx := context.Background()

	c := New(srv.URL)
	c.AccessToken = "sess"
	c.Me(ctx)
	if h := rec.get("/api/users/me"); h.Get("Authorization") != "Bearer sess" || h.Get("x-immich-user-token") != "" {
		t.Fatalf("without custom headers: %v", h)
	}

	c.Headers = []Header{{Name: "authorization", Value: "Basic cHJveHk6cHc="}}
	c.Me(ctx)
	if h := rec.get("/api/users/me"); h.Get("Authorization") != "Basic cHJveHk6cHc=" || h.Get("x-immich-user-token") != "sess" {
		t.Fatalf("with custom Authorization: %v", h)
	}
}

// Custom headers are credentials too: never sent along a redirect to
// another host.
func TestCustomHeadersNotSentOffOrigin(t *testing.T) {
	var mu sync.Mutex
	leaked := ""
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked += r.Header.Get("X-Gate")
		mu.Unlock()
		w.Write([]byte(`{"res":"pong"}`))
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	defer origin.Close()
	c := New(origin.URL)
	c.Headers = []Header{{Name: "X-Gate", Value: "secret"}}
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("followed an off-origin redirect")
	}
	mu.Lock()
	defer mu.Unlock()
	if leaked != "" {
		t.Fatalf("custom header reached the other host: %q", leaked)
	}
}
