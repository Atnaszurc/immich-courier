// Package webui is the settings page: a small web app served on 127.0.0.1
// only while it's in use, opened in the user's browser from the tray or CLI.
//
// Security model (any website in the same browser could try to talk to a
// local port):
//   - Listens on 127.0.0.1 with a random port; shut down after 30 idle minutes.
//   - Access needs a random session token. The tray/CLI opens /?t=TOKEN, which
//     sets an HttpOnly, SameSite=Lax cookie and redirects to a clean URL. (Lax,
//     not Strict: returning from the SSO provider is a cross-site redirect
//     chain, and Strict cookies aren't sent on those.)
//   - The Host header must be exactly 127.0.0.1:PORT (blocks DNS rebinding).
//   - State-changing requests need a custom header (a cross-site page can't
//     send one without a CORS preflight, which we never allow) and, if the
//     browser sends Origin, it must be this server.
//   - Strict CSP; the page loads nothing from the internet.
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"immich-courier/internal/daemon"
)

//go:embed static
var staticFS embed.FS

const (
	csrfHeader  = "X-Courier-Request"
	idleTimeout = 30 * time.Minute
)

// Server is the settings page. Create it once; URL starts it on demand.
type Server struct {
	d   *daemon.Daemon
	log *slog.Logger

	mu      sync.Mutex
	srv     *http.Server
	base    string // http://127.0.0.1:PORT
	host    string // 127.0.0.1:PORT
	token   string
	cookie  string // per port: browsers share cookies across ports
	lastUse time.Time

	login loginState
}

func New(d *daemon.Daemon) *Server {
	return &Server{d: d, log: d.Log.With("component", "settings")}
}

// URL starts the server if it isn't running and returns a URL that logs the
// browser in to it.
func (s *Server) URL() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		if err := s.start(); err != nil {
			return "", err
		}
	}
	s.lastUse = time.Now()
	return s.base + "/?t=" + s.token, nil
}

// Close stops the server if it's running.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

func (s *Server) stopLocked() {
	if s.srv != nil {
		s.srv.Close()
		s.srv = nil
		s.log.Info("settings page closed")
	}
}

func (s *Server) start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	b := make([]byte, 32)
	rand.Read(b)
	s.token = base64.RawURLEncoding.EncodeToString(b)
	s.host = ln.Addr().String()
	s.base = "http://" + s.host
	_, port, _ := net.SplitHostPort(s.host)
	s.cookie = "ic_session_" + port

	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	s.srv = srv
	go srv.Serve(ln)
	go s.idleWatch(srv)
	s.log.Info("settings page started", "address", s.base)
	return nil
}

func (s *Server) idleWatch(srv *http.Server) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		if s.srv != srv {
			s.mu.Unlock()
			return
		}
		if time.Since(s.lastUse) > idleTimeout && !s.login.busy() {
			s.stopLocked()
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
	}
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(static)
	mux.HandleFunc("GET /{$}", s.index(files))
	mux.Handle("GET /app.js", files)
	mux.Handle("GET /app.css", files)
	mux.Handle("GET /icon.svg", files)
	s.routes(mux)
	return s.guard(mux)
}

// index logs the browser in when it carries the token, then serves the page.
func (s *Server) index(files http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if t := r.URL.Query().Get("t"); t != "" {
			if !s.tokenOK(t) {
				http.Error(w, "This settings link has expired. Open Settings again from immich-courier.", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name: s.cookieName(), Value: t, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		files.ServeHTTP(w, r)
	}
}

func (s *Server) cookieName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cookie
}

func (s *Server) tokenOK(t string) bool {
	s.mu.Lock()
	tok := s.token
	s.mu.Unlock()
	return tok != "" && subtle.ConstantTimeCompare([]byte(t), []byte(tok)) == 1
}

// guard applies the checks described in the package comment to every request.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		host, base := s.host, s.base
		s.lastUse = time.Now()
		s.mu.Unlock()

		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")

		if r.Host != host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		isLogin := r.Method == http.MethodGet && r.URL.Path == "/" && r.URL.Query().Get("t") != ""
		if !isLogin {
			c, err := r.Cookie(s.cookieName())
			if err != nil || !s.tokenOK(c.Value) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>immich-courier</title><p>Open <b>Settings</b> from the immich-courier tray icon to manage it.</p>`)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get(csrfHeader) != "1" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != base {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		}
		next.ServeHTTP(w, r)
	})
}

// --- JSON helpers ---

type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, apiError{Error: err.Error()})
}

func readJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return errors.New("bad request")
	}
	return nil
}

// reload applies config changes to the running engine. A config with no
// folders yet isn't an error from the page's point of view.
func (s *Server) reload() {
	if err := s.d.Reload(); err != nil {
		s.log.Info("reloaded; not uploading yet", "reason", err)
	}
}

func bg() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}
