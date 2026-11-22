// Package daemon runs the engine for a config file, restarts it when the
// config changes, and exposes a small local API (HTTP over a Unix domain
// socket next to the config) for the CLI and tray.
package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"immich-courier/internal/accounts"
	"immich-courier/internal/config"
	"immich-courier/internal/engine"
	"immich-courier/internal/state"
)

const KeyringService = accounts.KeyringService

// derived names a file that belongs to one config file, next to it. The
// default config.toml keeps the plain names; any other config gets its own
// (work.toml → work.toml.state.db), so two configs in one folder never
// share a queue, a socket or a log.
func derived(cfgPath, plain, suffix string) string {
	dir, base := filepath.Split(cfgPath)
	if base == "config.toml" {
		return filepath.Join(dir, plain)
	}
	return filepath.Join(dir, base+suffix)
}

// StatePath is the state database for a config file.
func StatePath(cfgPath string) string { return derived(cfgPath, "state.db", ".state.db") }

// LogPath is the tray's log file for a config file.
func LogPath(cfgPath string) string {
	return derived(cfgPath, "immich-courier.log", ".log")
}

// maxSocketPath stays under the smallest sun_path limit (104 bytes on macOS).
const maxSocketPath = 100

// SocketPath is courier.sock next to the config (see derived). If that path
// is too long for a Unix socket, it is a short name derived from the config
// path inside a private per-user directory instead.
func SocketPath(cfgPath string) string {
	p := derived(cfgPath, "courier.sock", ".sock")
	if len(p) <= maxSocketPath {
		return p
	}
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		abs = cfgPath
	}
	sum := sha256.Sum256([]byte(abs))
	return filepath.Join(shortSocketDir(), "ic-"+hex.EncodeToString(sum[:6])+".sock")
}

// shortSocketDir is a short, per-user, private directory for sockets.
func shortSocketDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" && runtime.GOOS == "linux" {
		return d // already private to the user
	}
	name := "immich-courier"
	if u, err := user.Current(); err == nil {
		name += "-" + filepath.Base(u.Username) // /tmp is shared on Linux/macOS
	}
	return filepath.Join(os.TempDir(), name)
}

// Status is what the daemon reports over IPC and to the tray.
type Status struct {
	Version  string                 `json:"version"`
	Config   string                 `json:"config"`
	Started  time.Time              `json:"started"`
	Paused   bool                   `json:"paused"`
	Problem  string                 `json:"problem,omitempty"` // why nothing is running, e.g. no folders
	Accounts []engine.AccountStatus `json:"accounts"`
	Folders  []config.Folder        `json:"folders"`
}

type Daemon struct {
	CfgPath string
	Version string
	Log     *slog.Logger
	// EngineOptions overrides timings (tests); settle/rescan come from config otherwise.
	EngineOptions *engine.Options
	// OpenSettings starts the settings page if needed and returns its URL.
	// Set by the program; nil means settings aren't available.
	OpenSettings func() (string, error)

	store   *state.Store
	started time.Time

	mu      sync.Mutex
	eng     *engine.Engine
	stopEng context.CancelFunc
	engDone chan struct{}
	problem string
	paused  bool
	quit    chan struct{}
}

// Open prepares a daemon: it claims the IPC socket (failing if another
// instance is running) and opens the state database.
func Open(cfgPath, version string, log *slog.Logger) (*Daemon, error) {
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return nil, err
	}
	if c, err := Dial(cfgPath); err == nil {
		if _, err := c.Status(context.Background()); err == nil {
			return nil, ErrAlreadyRunning
		}
	}
	store, err := state.Open(StatePath(cfgPath))
	if err != nil {
		return nil, err
	}
	d := &Daemon{CfgPath: cfgPath, Version: version, Log: log, store: store, started: time.Now(), quit: make(chan struct{})}
	var paused bool
	if ok, _ := store.GetMeta("paused", &paused); ok {
		d.paused = paused
	}
	return d, nil
}

var ErrAlreadyRunning = errors.New("immich-courier is already running")

// Run starts the engine and the IPC server and blocks until ctx is done or
// Quit is called.
func (d *Daemon) Run(ctx context.Context) error {
	defer d.store.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	srvErr := make(chan error, 1)
	go func() { srvErr <- d.serveIPC(ctx) }()

	if err := d.Reload(); err != nil {
		d.Log.Warn("not uploading yet", "reason", err)
	}
	var err error
	select {
	case <-ctx.Done():
	case <-d.quit:
	case err = <-srvErr:
		srvErr = nil
	}
	d.stopEngine()
	cancel()
	if srvErr != nil {
		err = <-srvErr // wait for the socket to be removed
	}
	if err != nil {
		return fmt.Errorf("local API: %w", err)
	}
	return nil
}

// Quit stops Run.
func (d *Daemon) Quit() {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.quit:
	default:
		close(d.quit)
	}
}

// Reload re-reads the config and keychain and restarts the engine. Queued
// files are kept in the state database, so nothing is lost.
func (d *Daemon) Reload() error {
	d.stopEngine()
	d.mu.Lock()
	defer d.mu.Unlock()

	eng, err := d.buildEngine()
	if err != nil {
		d.problem = err.Error()
		return err
	}
	d.problem = ""
	eng.SetPaused(d.paused)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := eng.Run(ctx); err != nil {
			d.Log.Error("engine stopped", "err", err)
			d.mu.Lock()
			d.problem = err.Error()
			d.mu.Unlock()
		}
	}()
	d.eng, d.stopEng, d.engDone = eng, cancel, done
	return nil
}

func (d *Daemon) buildEngine() (*engine.Engine, error) {
	cfg, err := config.Load(d.CfgPath)
	if err != nil {
		return nil, err
	}
	if len(cfg.Folders) == 0 {
		return nil, errors.New("no folders to watch yet")
	}
	keys := map[string]engine.Credentials{}
	for _, f := range cfg.Folders {
		if _, ok := keys[f.Account]; ok {
			continue
		}
		a, ok := cfg.Account(f.Account)
		if !ok {
			return nil, fmt.Errorf("folder %s uses unknown account %q", f.Path, f.Account)
		}
		k, err := accounts.LoadKey(d.CfgPath, a)
		if err != nil {
			return nil, fmt.Errorf("no API key for account %q; log in again", f.Account)
		}
		if strings.IndexFunc(k, func(r rune) bool { return r <= ' ' || r > '~' }) >= 0 {
			return nil, fmt.Errorf("the API key stored for account %q is malformed; log in again", f.Account)
		}
		hs, err := accounts.LoadHeaders(d.CfgPath, a)
		if err != nil {
			return nil, fmt.Errorf("account %q: %w", f.Account, err)
		}
		keys[f.Account] = engine.Credentials{APIKey: k, Headers: hs}
	}
	opt := engine.Options{Settle: cfg.Settings.Settle(), Rescan: cfg.Settings.Rescan()}
	if d.EngineOptions != nil {
		opt = *d.EngineOptions
	}
	return engine.New(cfg, keys, d.store, d.Log, opt)
}

func (d *Daemon) stopEngine() {
	d.mu.Lock()
	stop, done := d.stopEng, d.engDone
	d.eng, d.stopEng, d.engDone = nil, nil, nil
	d.mu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
}

func (d *Daemon) Status() Status {
	d.mu.Lock()
	eng, s := d.eng, Status{Version: d.Version, Config: d.CfgPath, Started: d.started, Paused: d.paused, Problem: d.problem}
	d.mu.Unlock()
	if eng != nil {
		s.Accounts = eng.Status()
		s.Folders = eng.Folders()
	}
	if s.Accounts == nil {
		s.Accounts = []engine.AccountStatus{}
	}
	return s
}

// SetPaused pauses or resumes uploads; it survives restarts.
func (d *Daemon) SetPaused(p bool) {
	d.mu.Lock()
	d.paused = p
	eng := d.eng
	d.mu.Unlock()
	if eng != nil {
		eng.SetPaused(p)
	}
	_ = d.store.SetMeta("paused", p)
	d.Log.Info("uploads " + map[bool]string{true: "paused", false: "resumed"}[p])
}

func (d *Daemon) Rescan() {
	d.mu.Lock()
	eng := d.eng
	d.mu.Unlock()
	if eng != nil {
		eng.Rescan()
	}
}
