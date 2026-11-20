package daemon

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"immich-courier/internal/config"
	"immich-courier/internal/engine"
)

type fakeImmich struct {
	mu    sync.Mutex
	names map[string]bool
}

func (f *fakeImmich) has(n string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.names[n]
}

func (f *fakeImmich) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/server/ping", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"res":"pong"}`)) })
	mux.HandleFunc("GET /api/server/media-types", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"image":[".jpg"],"video":[],"sidecar":[]}`))
	})
	mux.HandleFunc("POST /api/assets/bulk-upload-check", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Assets []struct{ ID string } }
		json.NewDecoder(r.Body).Decode(&in)
		var out []map[string]string
		for _, a := range in.Assets {
			out = append(out, map[string]string{"id": a.ID, "action": "accept"})
		}
		json.NewEncoder(w).Encode(map[string]any{"results": out})
	})
	mux.HandleFunc("POST /api/assets", func(w http.ResponseWriter, r *http.Request) {
		file, hdr, err := r.FormFile("assetData")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		h := sha1.New()
		io.Copy(h, file)
		_ = hex.EncodeToString(h.Sum(nil))
		f.mu.Lock()
		f.names[hdr.Filename] = true
		f.mu.Unlock()
		w.WriteHeader(201)
		w.Write([]byte(`{"id":"x","status":"created"}`))
	})
	return mux
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func TestDaemonIPC(t *testing.T) {
	keyring.MockInit()
	keyring.Set(KeyringService, "a", "k")

	fake := &fakeImmich{names: map[string]bool{}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	// Short temp dir: Unix socket paths are limited to ~100 bytes.
	base, err := os.MkdirTemp("", "ic")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	cfgPath := filepath.Join(base, "config.toml")
	photos := filepath.Join(base, "photos")
	os.Mkdir(photos, 0o755)

	// Start with an account but no folders: the daemon runs and reports why it's idle.
	cfg := &config.Config{Accounts: []config.Account{{Name: "a", Server: srv.URL}}}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := Open(cfgPath, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	d.EngineOptions = &engine.Options{
		Settle: 200 * time.Millisecond, Tick: 50 * time.Millisecond, PingUp: 200 * time.Millisecond,
		PingDownMin: 100 * time.Millisecond, PingDownMax: 200 * time.Millisecond, RetryTick: 100 * time.Millisecond,
	}
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(context.Background()) }()

	ctx := context.Background()
	var c *Client
	waitFor(t, "IPC socket up", func() bool {
		c, err = Dial(cfgPath)
		if err != nil {
			return false
		}
		_, err = c.Status(ctx)
		return err == nil
	})
	st, _ := c.Status(ctx)
	if st.Problem == "" {
		t.Fatal("expected a problem report with no folders configured")
	}

	// A second instance must refuse to start.
	if _, err := Open(cfgPath, "test", log); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second instance: err = %v, want ErrAlreadyRunning", err)
	}

	// Add a folder and reload: uploads start without a restart.
	if err := cfg.AddFolder(config.Folder{Path: photos, Account: "a"}); err != nil {
		t.Fatal(err)
	}
	cfg.Save(cfgPath)
	if err := c.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(photos, "one.jpg"), []byte("1"), 0o644)
	waitFor(t, "one.jpg uploaded", func() bool { return fake.has("one.jpg") })

	// Paused: queued, not uploaded. Resumed: uploaded.
	if err := c.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(photos, "two.jpg"), []byte("2"), 0o644)
	waitFor(t, "two.jpg queued", func() bool {
		s, _ := c.Status(ctx)
		return len(s.Accounts) == 1 && s.Accounts[0].Queued == 1 && s.Paused
	})
	time.Sleep(400 * time.Millisecond)
	if fake.has("two.jpg") {
		t.Fatal("uploaded while paused")
	}
	c.Resume(ctx)
	waitFor(t, "two.jpg uploaded after resume", func() bool { return fake.has("two.jpg") })
	waitFor(t, "status shows 2 uploaded, 0 queued", func() bool {
		s, _ := c.Status(ctx)
		a := s.Accounts[0]
		return a.Online && !s.Paused && a.Uploaded == 2 && a.Queued == 0
	})

	if err := c.Quit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon didn't quit")
	}
	if _, err := os.Stat(SocketPath(cfgPath)); err == nil {
		t.Fatal("socket left behind after quit")
	}
}

func TestLongConfigPathGetsShortSocket(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("deep-folder-name/", 8), "config.toml")
	p := SocketPath(long)
	if len(p) > maxSocketPath {
		t.Fatalf("socket path too long (%d): %s", len(p), p)
	}
	if p != SocketPath(long) || p == SocketPath(long+"x") {
		t.Fatal("socket path must be stable per config and differ between configs")
	}
	dir := t.TempDir()
	short := filepath.Join(dir, "c.toml")
	if len(filepath.Join(dir, "c.toml.sock")) <= maxSocketPath && SocketPath(short) != filepath.Join(dir, "c.toml.sock") {
		t.Fatal("short config paths should keep the socket next to the config")
	}
}

// Two config files in one folder must not share a queue, socket or log;
// the default config keeps the original names.
func TestDerivedPathsPerConfig(t *testing.T) {
	dir := t.TempDir()
	def, work := filepath.Join(dir, "config.toml"), filepath.Join(dir, "work.toml")
	if StatePath(def) != filepath.Join(dir, "state.db") || LogPath(def) != filepath.Join(dir, "immich-courier.log") {
		t.Fatalf("default config paths changed: %s %s", StatePath(def), LogPath(def))
	}
	for _, f := range []func(string) string{StatePath, LogPath, SocketPath} {
		if f(def) == f(work) {
			t.Fatalf("configs share %s", f(def))
		}
	}
}
