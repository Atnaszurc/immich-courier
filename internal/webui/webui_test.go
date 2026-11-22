package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"immich-courier/internal/accounts"
	"immich-courier/internal/config"
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
		switch r.Header.Get("x-api-key") {
		case "good-key":
			w.Write([]byte(`{"id":"u1","email":"pri@example.com","name":"Pri"}`))
		case "other-user-key":
			w.Write([]byte(`{"id":"u2","email":"fred@example.com","name":"Fred"}`))
		default:
			http.Error(w, `{"message":"Invalid API key"}`, 401)
		}
	})
	return httptest.NewServer(mux)
}

type client struct {
	t       *testing.T
	hc      *http.Client
	base    string
	session string
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
	if c.session != "" {
		req.Header.Set(sessionHeader, c.session)
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

// login trades the ticket in a settings link for a session, like app.js.
func (c *client) login(link string) (*http.Response, map[string]any) {
	c.t.Helper()
	ticket, _ := strings.CutPrefix(link[strings.Index(link, "#"):], "#t=")
	resp, out := c.do("POST", "/api/session", map[string]string{"ticket": ticket}, true)
	if s, ok := out["session"].(string); ok {
		c.session = s
	}
	return resp, out
}

func TestGuard(t *testing.T) {
	s, c, loginURL := setup(t)

	// The link carries the ticket in the fragment, never in the query.
	if !strings.Contains(loginURL, "/#t=") || strings.Contains(loginURL, "?") {
		t.Fatalf("settings link %q", loginURL)
	}
	// The page itself is public; the API isn't.
	if resp, _ := c.do("GET", "/", nil, false); resp.StatusCode != 200 {
		t.Fatalf("page: %d", resp.StatusCode)
	}
	if resp, _ := c.do("GET", "/api/state", nil, false); resp.StatusCode != 401 {
		t.Fatalf("no session: %d", resp.StatusCode)
	}
	// Wrong ticket: refused.
	if resp, _ := c.login("x#t=nope"); resp.StatusCode != 401 {
		t.Fatalf("wrong ticket: %d", resp.StatusCode)
	}
	// Right ticket: a session, and no cookie (browsers send cookies to every
	// port on 127.0.0.1).
	resp, out := c.login(loginURL)
	if resp.StatusCode != 200 || c.session == "" {
		t.Fatalf("login: %d %v", resp.StatusCode, out)
	}
	if sc := resp.Header.Values("Set-Cookie"); len(sc) > 0 {
		t.Fatalf("settings page set a cookie: %v", sc)
	}
	good := c.session
	// The ticket works once.
	c.session = ""
	if resp, _ := c.login(loginURL); resp.StatusCode != 401 {
		t.Fatalf("ticket reused: %d", resp.StatusCode)
	}
	// Each link is new; old one-time links from earlier calls stay single-use.
	if l2, _ := s.URL(); l2 == loginURL {
		t.Fatal("URL returned the same ticket twice")
	}
	c.session = good
	resp, _ = c.do("GET", "/api/state", nil, false)
	if resp.StatusCode != 200 {
		t.Fatalf("with session: %d", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("missing CSP: %q", csp)
	}
	// Wrong session: refused.
	c.session = "nope"
	if resp, _ := c.do("GET", "/api/state", nil, false); resp.StatusCode != 401 {
		t.Fatalf("wrong session: %d", resp.StatusCode)
	}
	c.session = good

	// POST without the custom header: refused (cross-site form/fetch can't add it).
	if resp, _ := c.do("POST", "/api/pause", map[string]bool{"paused": true}, false); resp.StatusCode != 403 {
		t.Fatalf("POST without header: %d", resp.StatusCode)
	}
	// POST from another origin: refused.
	req, _ := http.NewRequest("POST", c.base+"/api/pause", strings.NewReader(`{"paused":true}`))
	req.Header.Set("X-Courier-Request", "1")
	req.Header.Set(sessionHeader, good)
	req.Header.Set("Origin", "https://evil.example")
	if resp, err := c.hc.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("foreign origin: %v %v", err, resp.StatusCode)
	}
	// DNS rebinding: right port, wrong Host header.
	req, _ = http.NewRequest("GET", c.base+"/api/state", nil)
	req.Header.Set(sessionHeader, good)
	req.Host = "evil.example:" + strings.Split(c.base, ":")[2]
	if resp, err := c.hc.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("bad host: %v %v", err, resp.StatusCode)
	}
	// Same-origin POST with header and session: allowed.
	if resp, _ := c.do("POST", "/api/pause", map[string]bool{"paused": false}, true); resp.StatusCode != 200 {
		t.Fatalf("good POST: %d", resp.StatusCode)
	}
}

func TestSetupFlow(t *testing.T) {
	im := fakeImmich()
	defer im.Close()
	s, c, loginURL := setup(t)
	if resp, out := c.login(loginURL); resp.StatusCode != 200 {
		t.Fatalf("login: %v", out)
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
	keyName := accounts.KeyName(s.d.CfgPath, "pri", im.URL)
	if k, _ := keyring.Get(accounts.KeyringService, keyName); k != "good-key" {
		t.Fatalf("keychain has %q", k)
	}

	// Logging in again under an existing name can't move the account to
	// another server or Immich user, or create a case-variant twin.
	im2 := fakeImmich()
	defer im2.Close()
	for _, tc := range []struct{ name, server, key, want string }{
		{"pri", im2.URL, "good-key", "uploads to"},
		{"pri", im.URL, "other-user-key", "another Immich user"},
		{"PRI", im.URL, "good-key", "already an account"},
	} {
		resp, out = c.do("POST", "/api/login/apikey", map[string]string{"name": tc.name, "server": tc.server, "apiKey": tc.key}, true)
		if resp.StatusCode != 400 || !strings.Contains(out["error"].(string), tc.want) {
			t.Fatalf("login %s@%s with %s: %d %v", tc.name, tc.server, tc.key, resp.StatusCode, out)
		}
	}
	if k, _ := keyring.Get(accounts.KeyringService, keyName); k != "good-key" {
		t.Fatalf("refused login changed the key to %q", k)
	}
	// The same user on the same server can log in again.
	if resp, out = c.do("POST", "/api/login/apikey", map[string]string{"name": "pri", "server": im.URL, "apiKey": "good-key"}, true); resp.StatusCode != 200 {
		t.Fatalf("re-login: %d %v", resp.StatusCode, out)
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
	// Field names must match what app.js reads (f.path, f.account).
	if f := folders[0].(map[string]any); f["path"] != newDir || f["account"] != "pri" {
		t.Fatalf("folder JSON fields: %v", f)
	}

	// The account can't be removed while a folder uses it; it can afterwards.
	if resp, _ = c.do("POST", "/api/accounts/remove", map[string]string{"name": "pri"}, true); resp.StatusCode != 400 {
		t.Fatal("removed an account in use")
	}
	c.do("POST", "/api/folders/remove", map[string]string{"path": newDir}, true)
	if resp, out = c.do("POST", "/api/accounts/remove", map[string]string{"name": "pri"}, true); resp.StatusCode != 200 {
		t.Fatalf("remove account: %v", out)
	}
	if _, err := keyring.Get(accounts.KeyringService, keyName); err == nil {
		t.Fatal("key left in keychain")
	}

	// Removing an account together with its folders in one step.
	c.do("POST", "/api/login/apikey", map[string]string{"name": "pri", "server": im.URL, "apiKey": "good-key"}, true)
	c.do("POST", "/api/folders/add", map[string]string{"path": newDir, "account": "pri"}, true)
	resp, out = c.do("POST", "/api/accounts/remove", map[string]any{"name": "pri", "withFolders": true}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("remove with folders: %v", out)
	}
	_, out = c.do("GET", "/api/state", nil, false)
	if len(out["accounts"].([]any)) != 0 || len(out["folders"].([]any)) != 0 {
		t.Fatalf("account or folder left behind: %v", out)
	}
}

func TestTimingSettings(t *testing.T) {
	_, c, loginURL := setup(t)
	if resp, out := c.login(loginURL); resp.StatusCode != 200 {
		t.Fatalf("login: %v", out)
	}
	settings := func() map[string]any {
		_, out := c.do("GET", "/api/state", nil, false)
		return out["settings"].(map[string]any)
	}
	// Defaults are reported as real values, not 0.
	if s := settings(); s["settleSeconds"] != float64(5) || s["rescanMinutes"] != float64(15) {
		t.Fatalf("defaults: %v", s)
	}
	if resp, out := c.do("POST", "/api/settings", map[string]int{"settleSeconds": 30, "rescanMinutes": 60}, true); resp.StatusCode != 200 {
		t.Fatalf("save: %v", out)
	}
	if s := settings(); s["settleSeconds"] != float64(30) || s["rescanMinutes"] != float64(60) {
		t.Fatalf("after save: %v", s)
	}
	for _, bad := range []map[string]int{
		{"settleSeconds": 0, "rescanMinutes": 15},
		{"settleSeconds": 99999, "rescanMinutes": 15},
		{"settleSeconds": 30, "rescanMinutes": -1},
	} {
		if resp, _ := c.do("POST", "/api/settings", bad, true); resp.StatusCode != 400 {
			t.Fatalf("accepted %v", bad)
		}
	}
	if s := settings(); s["settleSeconds"] != float64(30) {
		t.Fatalf("a rejected save changed the settings: %v", s)
	}
}

// A server behind a proxy with basic auth: the headers entered on the page
// are used for probe and login, saved, and only their names come back.
func TestCustomHeadersFlow(t *testing.T) {
	const proxyAuth = "Basic cHJveHk6c2VjcmV0"
	im := fakeImmich()
	defer im.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != proxyAuth {
			http.Error(w, "401 Authorization Required", 401)
			return
		}
		r.Header.Del("Authorization")
		u, _ := url.Parse(im.URL)
		httputil.NewSingleHostReverseProxy(u).ServeHTTP(w, r)
	}))
	defer proxy.Close()
	s, c, loginURL := setup(t)
	c.login(loginURL)
	hdrs := []map[string]string{{"name": "Authorization", "value": proxyAuth}}

	resp, out := c.do("POST", "/api/probe", map[string]any{"server": proxy.URL}, true)
	if resp.StatusCode != 400 || !strings.Contains(out["error"].(string), "custom headers") {
		t.Fatalf("probe without headers: %d %v", resp.StatusCode, out)
	}
	if resp, out = c.do("POST", "/api/probe", map[string]any{"server": proxy.URL, "headers": hdrs}, true); resp.StatusCode != 200 {
		t.Fatalf("probe with headers: %d %v", resp.StatusCode, out)
	}
	// Reserved and malformed headers are refused.
	for _, bad := range []map[string]string{{"name": "x-api-key", "value": "k"}, {"name": "X-A", "value": "a\r\nb"}} {
		resp, _ = c.do("POST", "/api/probe", map[string]any{"server": proxy.URL, "headers": []map[string]string{bad}}, true)
		if resp.StatusCode != 400 {
			t.Fatalf("header %v accepted", bad)
		}
	}
	resp, out = c.do("POST", "/api/login/apikey", map[string]any{"name": "pri", "server": proxy.URL, "apiKey": "good-key", "headers": hdrs}, true)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d %v", resp.StatusCode, out)
	}

	// State lists the header name, never the value.
	req, _ := http.NewRequest("GET", c.base+"/api/state", nil)
	req.Header.Set(sessionHeader, c.session)
	sr, err := c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(sr.Body)
	sr.Body.Close()
	if strings.Contains(string(body), "cHJveHk") || !strings.Contains(string(body), `"headers":["Authorization"]`) {
		t.Fatalf("state: %s", body)
	}

	// "Log in again" sends the names with empty values: stored values are used.
	keep := []map[string]string{{"name": "Authorization", "value": ""}}
	if resp, out = c.do("POST", "/api/probe", map[string]any{"server": proxy.URL, "account": "pri", "headers": keep}, true); resp.StatusCode != 200 {
		t.Fatalf("re-login probe: %d %v", resp.StatusCode, out)
	}
	if resp, out = c.do("POST", "/api/login/apikey", map[string]any{"name": "pri", "server": proxy.URL, "apiKey": "good-key", "headers": keep}, true); resp.StatusCode != 200 {
		t.Fatalf("re-login: %d %v", resp.StatusCode, out)
	}

	// Changing them is checked against the server first.
	resp, _ = c.do("POST", "/api/accounts/headers", map[string]any{"name": "pri", "headers": []map[string]string{{"name": "Authorization", "value": "Basic d3Jvbmc="}}}, true)
	if resp.StatusCode != 400 {
		t.Fatal("headers the proxy refuses were saved")
	}
	a := config.Account{Name: "pri", Server: proxy.URL}
	if hs, _ := accounts.LoadHeaders(s.d.CfgPath, a); len(hs) != 1 || hs[0].Value != proxyAuth {
		t.Fatalf("stored headers after refused change: %v", hs)
	}
	// Removing them: the proxy now refuses the key, so that's refused too.
	if resp, _ = c.do("POST", "/api/accounts/headers", map[string]any{"name": "pri", "headers": []any{}}, true); resp.StatusCode != 400 {
		t.Fatal("removed headers the server needs")
	}
}
