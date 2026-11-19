// Package engine watches folders, decides which files are ready, and hands
// them to one uploader per account.
//
// Pipeline: watch/scan → pending (waits until size+mtime settle) → hash →
// state DB (queued) → account uploader (only while the server answers ping).
package engine

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"immich-courier/internal/config"
	"immich-courier/internal/immich"
	"immich-courier/internal/state"
)

// Options tunes timings; zero values get sensible defaults. Tests shorten them.
type Options struct {
	Settle      time.Duration // how long size+mtime must be stable
	Rescan      time.Duration // full rescan interval
	Tick        time.Duration // how often pending files are checked
	PingUp      time.Duration // ping interval while reachable
	PingDownMin time.Duration // first retry after the server goes away
	PingDownMax time.Duration // cap for the retry backoff
	RetryTick   time.Duration // how often the uploader looks for due retries
}

func (o *Options) defaults() {
	set := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	set(&o.Settle, 5*time.Second)
	set(&o.Rescan, 15*time.Minute)
	set(&o.Tick, time.Second)
	set(&o.PingUp, time.Minute)
	set(&o.PingDownMin, 10*time.Second)
	set(&o.PingDownMax, 5*time.Minute)
	set(&o.RetryTick, time.Minute)
}

type Engine struct {
	folders  []config.Folder
	accounts map[string]*account
	store    *state.Store
	log      *slog.Logger
	opt      Options

	mu        sync.Mutex
	pending   map[string]*candidate
	hashQ     chan job
	watcher   *fsnotify.Watcher
	rescanReq chan struct{}
}

type candidate struct {
	folder  config.Folder
	size    int64
	modTime time.Time
	stable  time.Time // when size+mtime were last seen changing
}

type job struct {
	path    string
	folder  config.Folder
	size    int64
	modTime time.Time
}

// New builds an engine. apiKeys maps account name to its API key.
func New(cfg *config.Config, apiKeys map[string]string, store *state.Store, log *slog.Logger, opt Options) (*Engine, error) {
	opt.defaults()
	e := &Engine{
		folders:   cfg.Folders,
		accounts:  map[string]*account{},
		store:     store,
		log:       log,
		opt:       opt,
		pending:   map[string]*candidate{},
		hashQ:     make(chan job, 64),
		rescanReq: make(chan struct{}, 1),
	}
	for _, f := range cfg.Folders {
		if _, ok := e.accounts[f.Account]; ok {
			continue
		}
		a, ok := cfg.Account(f.Account)
		if !ok {
			return nil, fmt.Errorf("folder %s: unknown account %q", f.Path, f.Account)
		}
		key, ok := apiKeys[a.Name]
		if !ok || key == "" {
			return nil, fmt.Errorf("account %q has no API key; run: immich-courier login --account %s --server %s", a.Name, a.Name, a.Server)
		}
		c := immich.New(a.Server)
		c.APIKey = key
		e.accounts[a.Name] = newAccount(a, c, store, log, opt)
	}
	return e, nil
}

// Run blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	if len(e.folders) == 0 {
		return errors.New("no folders to watch; add one with: immich-courier add-folder --path DIR --account NAME")
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	e.watcher = w

	var wg sync.WaitGroup
	for _, a := range e.accounts {
		wg.Add(1)
		go func() { defer wg.Done(); a.run(ctx) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); e.hashWorker(ctx) }()

	for _, f := range e.folders {
		e.log.Info("watching folder", "path", f.Path, "account", f.Account)
		e.addTree(f, f.Path)
	}

	tick := time.NewTicker(e.opt.Tick)
	defer tick.Stop()
	rescan := time.NewTicker(e.opt.Rescan)
	defer rescan.Stop()

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case ev, ok := <-w.Events:
			if !ok {
				break loop
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Chmod) != 0 {
				e.observe(ev.Name, ev.Op&fsnotify.Create != 0)
			}
		case err, ok := <-w.Errors:
			if !ok {
				break loop
			}
			// Typically an overflow: events were lost, so rescan.
			e.log.Warn("watcher error, rescanning", "err", err)
			e.scanAll()
		case <-tick.C:
			e.checkPending(ctx)
		case <-rescan.C:
			e.scanAll()
		case <-e.rescanReq:
			e.log.Info("rescan requested")
			e.scanAll()
			for _, a := range e.accounts {
				a.poke()
			}
		}
	}
	wg.Wait()
	return nil
}

// Rescan asks the running engine to rescan all folders and retry due uploads.
func (e *Engine) Rescan() {
	select {
	case e.rescanReq <- struct{}{}:
	default:
	}
}

// SetPaused pauses or resumes uploads for all accounts. Watching, hashing
// and queuing continue while paused.
func (e *Engine) SetPaused(p bool) {
	for _, a := range e.accounts {
		a.paused.Store(p)
		if !p {
			a.poke()
		}
	}
}

// Folders returns the watched folders.
func (e *Engine) Folders() []config.Folder { return e.folders }

// AccountStatus is a snapshot of one account, for the tray and IPC.
type AccountStatus struct {
	Name       string    `json:"name"`
	Server     string    `json:"server"`
	Online     bool      `json:"online"`
	Paused     bool      `json:"paused"`
	AuthFailed bool      `json:"authFailed"`
	Queued     int       `json:"queued"`
	Uploading  string    `json:"uploading,omitempty"`
	Uploaded   int       `json:"uploadedThisSession"`
	LastUpload time.Time `json:"lastUpload,omitzero"`
	LastError  string    `json:"lastError,omitempty"`
}

// Status returns a snapshot per account, sorted by name. Pending files that
// are still settling or being hashed count as queued.
func (e *Engine) Status() []AccountStatus {
	pending := map[string]int{}
	e.mu.Lock()
	for _, c := range e.pending {
		pending[c.folder.Account]++
	}
	e.mu.Unlock()
	out := make([]AccountStatus, 0, len(e.accounts))
	for _, a := range e.accounts {
		s := a.status()
		s.Queued += pending[a.cfg.Name]
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (e *Engine) folderFor(path string) (config.Folder, bool) {
	for _, f := range e.folders {
		if config.Within(path, f.Path) {
			return f, true
		}
	}
	return config.Folder{}, false
}

// addTree watches dir and its subdirectories and scans the files in them.
func (e *Engine) addTree(f config.Folder, dir string) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			e.log.Warn("can't read", "path", p, "err", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != dir && skipName(d.Name()) {
				return fs.SkipDir
			}
			if err := e.watcher.Add(p); err != nil {
				e.log.Warn("can't watch", "path", p, "err", err)
			}
			return nil
		}
		e.consider(f, p)
		return nil
	})
}

func (e *Engine) scanAll() {
	for _, f := range e.folders {
		e.addTree(f, f.Path)
	}
}

// observe handles a watcher event for path.
func (e *Engine) observe(path string, created bool) {
	f, ok := e.folderFor(path)
	if !ok {
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		return // deleted or moved away; upload-only, so nothing to do
	}
	if st.IsDir() {
		if created && !skipName(filepath.Base(path)) {
			e.addTree(f, path) // files may have landed before the watch was added
		}
		return
	}
	e.consider(f, path)
}

// consider adds a file to pending unless the state DB says it's already handled.
func (e *Engine) consider(f config.Folder, path string) {
	if skipName(filepath.Base(path)) || !e.accounts[f.Account].accepts(path) {
		return
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return
	}
	if r, ok, _ := e.store.Get(path); ok && r.Account == f.Account &&
		r.Size == st.Size() && r.ModTime == st.ModTime().UnixNano() {
		return // unchanged since we last hashed it (queued or done)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.pending[path]
	if !ok {
		e.pending[path] = &candidate{folder: f, size: st.Size(), modTime: st.ModTime(), stable: time.Now()}
		return
	}
	if c.size != st.Size() || !c.modTime.Equal(st.ModTime()) {
		c.size, c.modTime, c.stable = st.Size(), st.ModTime(), time.Now()
	}
}

// checkPending moves files whose size and mtime have settled to the hasher.
func (e *Engine) checkPending(ctx context.Context) {
	now := time.Now()
	var ready []job
	e.mu.Lock()
	for p, c := range e.pending {
		st, err := os.Stat(p)
		if err != nil {
			delete(e.pending, p)
			continue
		}
		if st.Size() != c.size || !st.ModTime().Equal(c.modTime) {
			c.size, c.modTime, c.stable = st.Size(), st.ModTime(), now
			continue
		}
		if now.Sub(c.stable) >= e.opt.Settle {
			ready = append(ready, job{path: p, folder: c.folder, size: c.size, modTime: c.modTime})
		}
	}
	e.mu.Unlock()

	for _, j := range ready {
		select {
		case e.hashQ <- j:
			e.mu.Lock()
			delete(e.pending, j.path)
			e.mu.Unlock()
		case <-ctx.Done():
			return
		default:
			return // hasher is busy; the rest stay pending until the next tick
		}
	}
}

// hashWorker hashes settled files and queues them. Hashing happens even while
// the server is unreachable, so files are ready the moment it comes back.
func (e *Engine) hashWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-e.hashQ:
			e.hashOne(j)
		}
	}
}

func (e *Engine) hashOne(j job) {
	sum, err := hashFile(j.path)
	if err != nil {
		e.log.Warn("can't read file", "path", j.path, "err", err)
		return
	}
	// If it changed while we read it, let the watcher pick it up again.
	if st, err := os.Stat(j.path); err != nil || st.Size() != j.size || !st.ModTime().Equal(j.modTime) {
		e.consider(j.folder, j.path)
		return
	}
	rec := state.Record{Account: j.folder.Account, Size: j.size, ModTime: j.modTime.UnixNano(), SHA1: sum, Status: state.Queued}
	if old, ok, _ := e.store.Get(j.path); ok && old.Account == rec.Account && old.SHA1 == sum &&
		(old.Status == state.Uploaded || old.Status == state.Duplicate) {
		// Only the mtime changed (e.g. touched); content is already handled.
		rec.Status, rec.AssetID = old.Status, old.AssetID
	}
	if err := e.store.Put(j.path, rec); err != nil {
		e.log.Error("state database write failed", "err", err)
		return
	}
	if rec.Status == state.Queued {
		e.log.Info("queued", "path", j.path, "account", rec.Account)
		e.accounts[rec.Account].poke()
	}
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// skipName filters hidden files, OS clutter and in-progress downloads.
func skipName(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~$") {
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".tmp", ".part", ".crdownload", ".download", ".partial":
		return true
	}
	return false
}
