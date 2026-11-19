package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"immich-courier/internal/daemon"
)

func fakeImmich() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/server/ping", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"res":"pong"}`)) })
	mux.HandleFunc("GET /api/server/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"major":3,"minor":3,"patch":5}`))
	})
	mux.HandleFunc("GET /api/server/features", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"oauth":false,"passwordLogin":true}`))
	})
	mux.HandleFunc("GET /api/users/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "good-key" {
			http.Error(w, `{"message":"Invalid API key"}`, 401)
			return
		}
		w.Write([]byte(`{"id":"u1","email":"pri@example.com","name":"Pri"}`))
	})
	return httptest.NewServer(mux)
}

type client struct {
	t    *testing.T
	hc   *http.Client
	base string
}

func (c *client) do(method, path string, body any, csrf bool) (*http.Response, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if csrf {
		req.Header.Set("X-Courier-Request", "1")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func setup(t *testing.T) (*Server, *client, string) {
	keyring.MockInit()
	base, _ := os.MkdirTemp("", "icw")
	t.Cleanup(func() { os.RemoveAll(base) })
	cfgPath := filepath.Join(base, "config.toml")
	d, err := daemon.Open(cfgPath, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctxDone := make(chan struct{})
	go func() { d.Run(t.Context()); close(ctxDone) }()
	t.Cleanup(func() { d.Quit(); <-ctxDone })

	s := New(d)
	t.Cleanup(s.Close)
	u, err := s.URL()
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar}
	pu, _ := url.Parse(u)
	return s, &client{t: t, hc: hc, base: pu.Scheme + "://" + pu.Host}, u
}

func TestGuard(t *testing.T) {
	_, c, loginURL := setup(t)

	// No cookie: refused.
	if resp, _ := c.do("GET", "/api/state", nil, false); resp.StatusCode != 401 {
		t.Fatalf("no cookie: %d", resp.StatusCode)
	}
	// Wrong token: refused.
	if resp, _ := c.do("GET", "/?t=nope", nil, false); resp.StatusCode != 403 {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	// Right token: cookie set, redirected to a clean URL.
	resp, err := c.hc.Get(loginURL)
	if err != nil || resp.StatusCode != 200 || resp.Request.URL.RawQuery != "" {
		t.Fatalf("login: %v %v %v", err, resp.StatusCode, resp.Request.URL)
	}
	resp.Body.Close()
	if resp, _ := c.do("GET", "/api/state", nil, false); resp.StatusCode != 200 {
		t.Fatalf("with cookie: %d", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("missing CSP: %q", csp)
	}

	// POST without the custom header: refused (cross-site form/fetch can't add it).
	if resp, _ := c.do("POST", "/api/pause", map[string]bool{"paused": true}, false); resp.StatusCode != 403 {
		t.Fatalf("POST without header: %d", resp.StatusCode)
	}
	// POST from another origin: refused.
	req, _ := http.NewRequest("POST", c.base+"/api/pause", strings.NewReader(`{"paused":true}`))
	req.Header.Set("X-Courier-Request", "1")
	req.Header.Set("Origin", "https://evil.example")
	if resp, err := c.hc.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("foreign origin: %v %v", err, resp.StatusCode)
	}
	// DNS rebinding: right port, wrong Host header.
	req, _ = http.NewRequest("GET", c.base+"/api/state", nil)
	req.Host = "evil.example:" + strings.Split(c.base, ":")[2]
	if resp, err := c.hc.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("bad host: %v %v", err, resp.StatusCode)
	}
	// Same-origin POST with header: allowed.
	if resp, _ := c.do("POST", "/api/pause", map[string]bool{"paused": false}, true); resp.StatusCode != 200 {
		t.Fatalf("good POST: %d", resp.StatusCode)
	}
}

func TestSetupFlow(t *testing.T) {
	im := fakeImmich()
	defer im.Close()
	_, c, loginURL := setup(t)
	if resp, err := c.hc.Get(loginURL); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}

	// Probe normalizes the address and reports login methods.
	resp, out := c.do("POST", "/api/probe", map[string]string{"server": im.URL + "/api/"}, true)
	if resp.StatusCode != 200 || out["server"] != im.URL || out["passwordLogin"] != true || out["version"] != "3.3.5" {
		t.Fatalf("probe: %d %v", resp.StatusCode, out)
	}

	// A bad key is rejected with a helpful message and nothing is saved.
	resp, out = c.do("POST", "/api/login/apikey", map[string]string{"name": "pri", "server": im.URL, "apiKey": "bad"}, true)
	if resp.StatusCode != 400 || !strings.Contains(out["error"].(string), "asset.upload") {
		t.Fatalf("bad key: %d %v", resp.StatusCode, out)
	}
	// A good key: account saved, key in keychain.
	resp, out = c.do("POST", "/api/login/apikey", map[string]string{"name": "pri", "server": im.URL, "apiKey": "good-key"}, true)
	if resp.StatusCode != 200 || out["email"] != "pri@example.com" {
		t.Fatalf("good key: %d %v", resp.StatusCode, out)
	}
	if k, _ := keyring.Get("immich-courier", "pri"); k != "good-key" {
		t.Fatalf("keychain has %q", k)
	}

	// Browse, create a folder, watch it.
	home := t.TempDir()
	resp, out = c.do("GET", "/api/dirs?path="+url.QueryEscape(home), nil, false)
	if resp.StatusCode != 200 || out["path"] != home {
		t.Fatalf("dirs: %d %v", resp.StatusCode, out)
	}
	if resp, out = c.do("POST", "/api/dirs/create", map[string]string{"parent": home, "name": "../escape"}, true); resp.StatusCode != 400 {
		t.Fatalf("mkdir with separator accepted: %v", out)
	}
	resp, out = c.do("POST", "/api/dirs/create", map[string]string{"parent": home, "name": "ToImmich"}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("mkdir: %d %v", resp.StatusCode, out)
	}
	newDir := out["path"].(string)
	resp, out = c.do("POST", "/api/folders/add", map[string]string{"path": newDir, "account": "pri"}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("add folder: %d %v", resp.StatusCode, out)
	}
	// Nested folder is refused.
	sub := filepath.Join(newDir, "sub")
	os.Mkdir(sub, 0o755)
	if resp, _ = c.do("POST", "/api/folders/add", map[string]string{"path": sub, "account": "pri"}, true); resp.StatusCode != 400 {
		t.Fatal("nested folder accepted")
	}

	// State shows the account (with key) and the folder.
	_, out = c.do("GET", "/api/state", nil, false)
	accts := out["accounts"].([]any)
	folders := out["folders"].([]any)
	if len(accts) != 1 || len(folders) != 1 || accts[0].(map[string]any)["hasKey"] != true {
		t.Fatalf("state: %v", out)
	}

	// The account can't be removed while a folder uses it; it can afterwards.
	if resp, _ = c.do("POST", "/api/accounts/remove", map[string]string{"name": "pri"}, true); resp.StatusCode != 400 {
		t.Fatal("removed an account in use")
	}
	c.do("POST", "/api/folders/remove", map[string]string{"path": newDir}, true)
	if resp, out = c.do("POST", "/api/accounts/remove", map[string]string{"name": "pri"}, true); resp.StatusCode != 200 {
		t.Fatalf("remove account: %v", out)
	}
	if _, err := keyring.Get("immich-courier", "pri"); err == nil {
		t.Fatal("key left in keychain")
	}
}
