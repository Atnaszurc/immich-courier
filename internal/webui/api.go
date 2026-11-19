package webui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"immich-courier/internal/accounts"
	"immich-courier/internal/auth"
	"immich-courier/internal/config"
	"immich-courier/internal/engine"
	"immich-courier/internal/immich"
)

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/state", s.apiState)
	mux.HandleFunc("POST /api/pause", s.apiPause)
	mux.HandleFunc("POST /api/rescan", func(w http.ResponseWriter, r *http.Request) {
		s.d.Rescan()
		writeJSON(w, 200, struct{}{})
	})
	mux.HandleFunc("POST /api/probe", s.apiProbe)
	mux.HandleFunc("POST /api/login/password", s.apiLoginPassword)
	mux.HandleFunc("POST /api/login/apikey", s.apiLoginAPIKey)
	mux.HandleFunc("POST /api/login/oidc", s.apiLoginOIDC)
	mux.HandleFunc("GET /api/login/oidc", s.apiLoginOIDCStatus)
	mux.HandleFunc("POST /api/accounts/remove", s.apiRemoveAccount)
	mux.HandleFunc("POST /api/folders/add", s.apiAddFolder)
	mux.HandleFunc("POST /api/folders/remove", s.apiRemoveFolder)
	mux.HandleFunc("GET /api/dirs", s.apiDirs)
	mux.HandleFunc("POST /api/dirs/create", s.apiMkdir)
}

// --- state ---

type accountView struct {
	Name    string                `json:"name"`
	Server  string                `json:"server"`
	HasKey  bool                  `json:"hasKey"`
	InUse   bool                  `json:"inUse"` // a folder uploads to it
	Status  *engine.AccountStatus `json:"status,omitempty"`
	Folders int                   `json:"folders"`
}

type stateView struct {
	Version  string          `json:"version"`
	Paused   bool            `json:"paused"`
	Problem  string          `json:"problem,omitempty"`
	Accounts []accountView   `json:"accounts"`
	Folders  []config.Folder `json:"folders"`
	Config   string          `json:"config"`
	Home     string          `json:"home"`
}

func (s *Server) apiState(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load(s.d.CfgPath)
	if err != nil {
		fail(w, 500, err)
		return
	}
	st := s.d.Status()
	live := map[string]engine.AccountStatus{}
	for _, a := range st.Accounts {
		live[a.Name] = a
	}
	v := stateView{Version: st.Version, Paused: st.Paused, Problem: st.Problem, Config: s.d.CfgPath, Folders: cfg.Folders, Accounts: []accountView{}}
	if v.Folders == nil {
		v.Folders = []config.Folder{}
	}
	v.Home, _ = os.UserHomeDir()
	for _, a := range cfg.Accounts {
		av := accountView{Name: a.Name, Server: a.Server, HasKey: accounts.HasKey(a.Name)}
		for _, f := range cfg.Folders {
			if f.Account == a.Name {
				av.Folders++
			}
		}
		av.InUse = av.Folders > 0
		if l, ok := live[a.Name]; ok {
			av.Status = &l
		}
		v.Accounts = append(v.Accounts, av)
	}
	writeJSON(w, 200, v)
}

func (s *Server) apiPause(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Paused bool `json:"paused"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	s.d.SetPaused(in.Paused)
	writeJSON(w, 200, struct{}{})
}

// --- accounts ---

func (s *Server) apiProbe(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Server string `json:"server"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	ctx, cancel := bg()
	defer cancel()
	info, err := accounts.Probe(ctx, in.Server)
	if err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, 200, info)
}

type loginReq struct {
	Name     string `json:"name"`
	Server   string `json:"server"`
	Email    string `json:"email"`
	Password string `json:"password"`
	APIKey   string `json:"apiKey"`
}

func (in *loginReq) validate() error {
	if err := accounts.ValidName(in.Name); err != nil {
		return err
	}
	srv, err := accounts.NormalizeServer(in.Server)
	in.Server = srv
	return err
}

func (s *Server) finishLogin(w http.ResponseWriter, ctx context.Context, in loginReq, secret string) {
	u, err := accounts.Save(ctx, s.d.CfgPath, in.Name, in.Server, secret)
	if err != nil {
		fail(w, 400, err)
		return
	}
	s.reload()
	writeJSON(w, 200, map[string]string{"name": u.Name, "email": u.Email})
}

func (s *Server) apiLoginPassword(w http.ResponseWriter, r *http.Request) {
	var in loginReq
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	if err := in.validate(); err != nil {
		fail(w, 400, err)
		return
	}
	ctx, cancel := bg()
	defer cancel()
	sess, err := auth.PasswordLogin(ctx, immich.New(in.Server), strings.TrimSpace(in.Email), in.Password)
	if err != nil {
		var apiErr *immich.APIError
		if errors.As(err, &apiErr) && (apiErr.Status == 400 || apiErr.Status == 401) {
			err = errors.New("wrong email or password")
		}
		fail(w, 400, err)
		return
	}
	secret, warn, err := accounts.KeyFromSession(ctx, in.Server, sess)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if warn != nil {
		s.log.Warn("login", "warning", warn)
	}
	s.finishLogin(w, ctx, in, secret)
}

func (s *Server) apiLoginAPIKey(w http.ResponseWriter, r *http.Request) {
	var in loginReq
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	if err := in.validate(); err != nil {
		fail(w, 400, err)
		return
	}
	ctx, cancel := bg()
	defer cancel()
	s.finishLogin(w, ctx, in, in.APIKey)
}

// --- OIDC: the page navigates to the provider; the loopback callback
// sends the browser back to the page, which polls for the outcome. ---

type loginState struct {
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
	result  map[string]string // state: pending|done|error, plus message/user
}

func (l *loginState) busy() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running
}

func (l *loginState) set(m map[string]string, running bool) {
	l.mu.Lock()
	l.result, l.running = m, running
	l.mu.Unlock()
}

func (s *Server) apiLoginOIDC(w http.ResponseWriter, r *http.Request) {
	var in loginReq
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	if err := in.validate(); err != nil {
		fail(w, 400, err)
		return
	}
	// A new attempt replaces an abandoned one (e.g. the user left the
	// provider's page), which would otherwise hold the callback port.
	s.login.mu.Lock()
	if s.login.running {
		cancelOld, doneOld := s.login.cancel, s.login.done
		s.login.mu.Unlock()
		cancelOld()
		select {
		case <-doneOld:
		case <-time.After(3 * time.Second):
			fail(w, 409, errors.New("a previous login is still finishing; try again in a moment"))
			return
		}
		s.login.mu.Lock()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	done := make(chan struct{})
	s.login.running, s.login.cancel, s.login.done = true, cancel, done
	s.login.result = map[string]string{"state": "pending"}
	s.login.mu.Unlock()

	s.mu.Lock()
	returnURL := s.base + "/#login"
	s.mu.Unlock()

	urlCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		defer close(done)
		defer cancel()
		c := immich.New(in.Server)
		sess, err := auth.OIDCLogin(ctx, c, auth.OIDCOptions{
			OpenBrowser: func(string) error { return nil }, // the page navigates itself
			OnURL:       func(u string) { urlCh <- u },
			ReturnURL:   returnURL,
		})
		if err != nil {
			errCh <- err
			s.login.set(map[string]string{"state": "error", "error": oidcHint(err)}, false)
			return
		}
		secret, warn, err := accounts.KeyFromSession(ctx, in.Server, sess)
		if err == nil {
			if warn != nil {
				s.log.Warn("login", "warning", warn)
			}
			var u immich.User
			u, err = accounts.Save(ctx, s.d.CfgPath, in.Name, in.Server, secret)
			if err == nil {
				s.reload()
				s.login.set(map[string]string{"state": "done", "account": in.Name, "name": u.Name, "email": u.Email}, false)
				return
			}
		}
		s.login.set(map[string]string{"state": "error", "error": err.Error()}, false)
	}()

	select {
	case u := <-urlCh:
		writeJSON(w, 200, map[string]string{"url": u})
	case err := <-errCh:
		fail(w, 400, errors.New(oidcHint(err)))
	case <-time.After(30 * time.Second):
		fail(w, 504, errors.New("the server didn't respond"))
	}
}

func oidcHint(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "listen on") {
		return "Another login is already waiting in a browser tab. Finish or close it, then try again."
	}
	return msg
}

func (s *Server) apiLoginOIDCStatus(w http.ResponseWriter, r *http.Request) {
	s.login.mu.Lock()
	res := s.login.result
	s.login.mu.Unlock()
	if res == nil {
		res = map[string]string{"state": "none"}
	}
	writeJSON(w, 200, res)
}

func (s *Server) apiRemoveAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string `json:"name"`
		WithFolders bool   `json:"withFolders"` // also stop watching its folders
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	if err := accounts.Remove(s.d.CfgPath, in.Name, in.WithFolders); err != nil {
		fail(w, 400, err)
		return
	}
	s.reload()
	writeJSON(w, 200, struct{}{})
}

// --- folders ---

func (s *Server) apiAddFolder(w http.ResponseWriter, r *http.Request) {
	var in config.Folder
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	cfg, err := config.Load(s.d.CfgPath)
	if err != nil {
		fail(w, 500, err)
		return
	}
	if err := cfg.AddFolder(in); err != nil {
		fail(w, 400, err)
		return
	}
	if err := cfg.Save(s.d.CfgPath); err != nil {
		fail(w, 500, err)
		return
	}
	s.reload()
	writeJSON(w, 200, struct{}{})
}

func (s *Server) apiRemoveFolder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	cfg, err := config.Load(s.d.CfgPath)
	if err != nil {
		fail(w, 500, err)
		return
	}
	ok, err := cfg.RemoveFolder(in.Path)
	if err == nil && !ok {
		err = errors.New("that folder isn't being watched")
	}
	if err != nil {
		fail(w, 400, err)
		return
	}
	if err := cfg.Save(s.d.CfgPath); err != nil {
		fail(w, 500, err)
		return
	}
	s.reload()
	writeJSON(w, 200, struct{}{})
}

// --- directory browser (browsers can't hand a real folder path to a page) ---

type dirsView struct {
	Path   string   `json:"path"`
	Parent string   `json:"parent"`          // "" at a root
	Dirs   []string `json:"dirs"`            // names
	Roots  []string `json:"roots,omitempty"` // drives on Windows
	Error  string   `json:"error,omitempty"`
}

const maxDirs = 2000

func (s *Server) apiDirs(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		p, _ = os.UserHomeDir()
	}
	p, err := config.ExpandPath(p)
	if err != nil {
		fail(w, 400, err)
		return
	}
	v := dirsView{Path: p, Dirs: []string{}, Roots: roots()}
	if parent := filepath.Dir(p); parent != p {
		v.Parent = parent
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		v.Error = "Can't open this folder: " + err.Error()
		writeJSON(w, 200, v)
		return
	}
	for _, e := range entries {
		if len(v.Dirs) >= maxDirs {
			break
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") {
			continue
		}
		isDir := e.IsDir()
		if !isDir && e.Type()&os.ModeSymlink != 0 {
			if st, err := os.Stat(filepath.Join(p, name)); err == nil && st.IsDir() {
				isDir = true
			}
		}
		if isDir {
			v.Dirs = append(v.Dirs, name)
		}
	}
	sort.Slice(v.Dirs, func(i, j int) bool { return strings.ToLower(v.Dirs[i]) < strings.ToLower(v.Dirs[j]) })
	writeJSON(w, 200, v)
}

func roots() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	var out []string
	for c := 'A'; c <= 'Z'; c++ {
		d := string(c) + `:\`
		if _, err := os.Stat(d); err == nil {
			out = append(out, d)
		}
	}
	return out
}

func (s *Server) apiMkdir(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Parent string `json:"parent"`
		Name   string `json:"name"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\:*?"<>|`) {
		fail(w, 400, fmt.Errorf("%q isn't a valid folder name", name))
		return
	}
	parent, err := config.ExpandPath(in.Parent)
	if err != nil {
		fail(w, 400, err)
		return
	}
	p := filepath.Join(parent, name)
	if err := os.Mkdir(p, 0o755); err != nil {
		fail(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]string{"path": p})
}
