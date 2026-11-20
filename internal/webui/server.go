// Package webui is the settings page: a small web app served on 127.0.0.1
// only while it's in use, opened in the user's browser from the tray or CLI.
//
// Security model (any website in the same browser, and any program on this
// machine, can try to talk to a local port):
//   - Listens on 127.0.0.1 with a random port; shut down after 30 idle minutes.
//   - No cookies. Browsers send cookies to every port on a host, so a cookie
//     would reach any other program listening on 127.0.0.1 (confirmed in
//     Chrome, Edge and Firefox).
//   - The tray/CLI opens /#t=TICKET. The ticket is random, works once and
//     expires after two minutes, so one read from a process list or browser
//     history is useless. It's in the fragment, so it is never sent in a
//     request line, a Referer or a log.
//   - The page trades the ticket for a session secret (POST /api/session),
//     keeps it in sessionStorage (this tab only; survives the SSO round trip)
//     and sends it in a header on every API call. A cross-site page can't
//     send that header without a CORS preflight, which we never allow.
//   - The Host header must be exactly 127.0.0.1:PORT (blocks DNS rebinding).
//   - Changes need a matching Origin when the browser sends one.
//   - Strict CSP; the page loads nothing from the internet. The page's own
//     files (HTML, JS, CSS) are public and need no session.
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"immich-courier/internal/daemon"
)

//go:embed static
var staticFS embed.FS

const (
	csrfHeader    = "X-Courier-Request"
	sessionHeader = "X-Courier-Session"
	idleTimeout   = 30 * time.Minute
	ticketTTL     = 2 * time.Minute
	maxSessions   = 32 // tabs; the oldest is dropped beyond this
)

// Server is the settings page. Create it once; URL starts it on demand.
type Server struct {
	d   *daemon.Daemon
	log *slog.Logger

	mu       sync.Mutex
	srv      *http.Server
	base     string               // http://127.0.0.1:PORT
	host     string               // 127.0.0.1:PORT
	tickets  map[string]time.Time // one-time opening tickets -> expiry
	sessions []string             // session secrets, oldest first
	lastUse  time.Time

	login loginState
}

func New(d *daemon.Daemon) *Server {
	return &Server{d: d, log: d.Log.With("component", "settings")}
}

// URL starts the server if it isn't running and returns a link that logs
// one browser tab in to it. Each call makes a new one-time link.
func (s *Server) URL() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		if err := s.start(); err != nil {
			return "", err
		}
	}
	now := time.Now()
	s.lastUse = now
	for t, exp := range s.tickets {
		if now.After(exp) {
			delete(s.tickets, t)
		}
	}
	t := randomToken()
	s.tickets[t] = now.Add(ticketTTL)
	return s.base + "/#t=" + t, nil
}

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
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
		s.tickets, s.sessions = nil, nil
		s.log.Info("settings page closed")
	}
}

func (s *Server) start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.host = ln.Addr().String()
	s.base = "http://" + s.host
	s.tickets, s.sessions = map[string]time.Time{}, nil

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
	mux.Handle("GET /{$}", files)
	mux.Handle("GET /app.js", files)
	mux.Handle("GET /app.css", files)
	mux.Handle("GET /icon.svg", files)
	mux.HandleFunc("POST /api/session", s.apiSession)
	s.routes(mux)
	return s.guard(mux)
}

// apiSession trades a one-time ticket for a session secret.
func (s *Server) apiSession(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ticket string `json:"ticket"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	s.mu.Lock()
	exp, ok := s.tickets[in.Ticket]
	delete(s.tickets, in.Ticket) // one use, valid or not
	var sess string
	if ok && in.Ticket != "" && time.Now().Before(exp) {
		sess = randomToken()
		s.sessions = append(s.sessions, sess)
		if len(s.sessions) > maxSessions {
			s.sessions = s.sessions[1:]
		}
	}
	s.mu.Unlock()
	if sess == "" {
		fail(w, http.StatusUnauthorized, errors.New("this settings link has expired or was already used; open Settings again from immich-courier"))
		return
	}
	writeJSON(w, 200, map[string]string{"session": sess})
}

func (s *Server) sessionOK(v string) bool {
	if v == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ok := 0
	for _, sess := range s.sessions {
		ok |= subtle.ConstantTimeCompare([]byte(v), []byte(sess))
	}
	return ok == 1
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
		isAPI := strings.HasPrefix(r.URL.Path, "/api/")
		if isAPI && r.URL.Path != "/api/session" && !s.sessionOK(r.Header.Get(sessionHeader)) {
			fail(w, http.StatusUnauthorized, errors.New("not logged in"))
			return
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
