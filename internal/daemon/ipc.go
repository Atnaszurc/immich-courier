package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// The local API is plain HTTP over a Unix domain socket (supported on
// Windows 10 1803+ as well). The socket lives in the user's private config
// directory, which is what restricts access to it.
//
//	GET  /v1/status
//	POST /v1/pause | /v1/resume | /v1/rescan | /v1/reload | /v1/quit
//	POST /v1/settings  -> {"url": ...} settings page URL (with session token)

func (d *Daemon) serveIPC(ctx context.Context) error {
	path := SocketPath(d.CfgPath)
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	_ = os.Remove(path) // stale socket from a crash; Open already checked nobody answers
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	_ = os.Chmod(path, 0o600)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(d.Status())
	})
	action := func(f func() error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if err := f(); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}
	mux.HandleFunc("POST /v1/pause", action(func() error { d.SetPaused(true); return nil }))
	mux.HandleFunc("POST /v1/resume", action(func() error { d.SetPaused(false); return nil }))
	mux.HandleFunc("POST /v1/rescan", action(func() error { d.Rescan(); return nil }))
	mux.HandleFunc("POST /v1/reload", action(d.Reload))
	mux.HandleFunc("POST /v1/quit", action(func() error { d.Quit(); return nil }))
	mux.HandleFunc("POST /v1/settings", func(w http.ResponseWriter, r *http.Request) {
		if d.OpenSettings == nil {
			http.Error(w, "settings page not available", http.StatusNotFound)
			return
		}
		u, err := d.OpenSettings()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"url": u})
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ensurePrivateDir creates dir (0700) and refuses to use it if it's a
// symlink, belongs to another user or is accessible by others, e.g. when
// someone else created it first in a shared /tmp.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := checkSocketDir(dir); err != nil {
		return err
	}
	st, _ := os.Lstat(dir)
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("socket directory %s is accessible by other users (mode %v)", dir, st.Mode().Perm())
	}
	return nil
}

// checkSocketDir refuses a socket directory that someone else could have
// planted a socket in: a symlink, not owned by this user, or writable by
// others. Clients check it too, so they never talk to another user's server.
func checkSocketDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return fmt.Errorf("socket directory %s is not a plain directory", dir)
	}
	if !ownedByMe(st) {
		return fmt.Errorf("socket directory %s belongs to another user", dir)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("socket directory %s is writable by other users (mode %v)", dir, st.Mode().Perm())
	}
	return nil
}

// Client talks to a running daemon.
type Client struct{ hc *http.Client }

// ErrNotRunning means no daemon answered on the socket.
var ErrNotRunning = errors.New("immich-courier is not running")

// Dial returns a client for the daemon using cfgPath. It does not connect yet.
func Dial(cfgPath string) (*Client, error) {
	sock := SocketPath(cfgPath)
	if _, err := os.Stat(sock); err != nil {
		return nil, ErrNotRunning
	}
	if err := checkSocketDir(filepath.Dir(sock)); err != nil {
		return nil, fmt.Errorf("not connecting to immich-courier: %w", err)
	}
	return &Client{hc: &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dl net.Dialer
			return dl.DialContext(ctx, "unix", sock)
		}},
	}}, nil
}

func (c *Client) call(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://courier"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return ErrNotRunning
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s", string(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	err := c.call(ctx, http.MethodGet, "/v1/status", &s)
	return s, err
}

func (c *Client) Pause(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/v1/pause", nil)
}
func (c *Client) Resume(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/v1/resume", nil)
}
func (c *Client) Rescan(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/v1/rescan", nil)
}
func (c *Client) Reload(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/v1/reload", nil)
}
func (c *Client) Quit(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/v1/quit", nil)
}

// SettingsURL asks the running instance to start its settings page.
func (c *Client) SettingsURL(ctx context.Context) (string, error) {
	var r struct {
		URL string `json:"url"`
	}
	err := c.call(ctx, http.MethodPost, "/v1/settings", &r)
	return r.URL, err
}
