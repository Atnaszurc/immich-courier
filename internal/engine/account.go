package engine

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"immich-courier/internal/config"
	"immich-courier/internal/fsafe"
	"immich-courier/internal/immich"
	"immich-courier/internal/state"
)

// fallbackExtensions is used until a server has told us what it accepts
// (GET /api/server/media-types), e.g. when starting while offline.
var fallbackExtensions = strings.Fields(`.3gp .3gpp .avi .flv .insv .m2t .m2ts .m4v .mkv .mov .mp4 .mpe .mpeg .mpg .mts .mxf .ts .vob .webm .wmv
.avif .bmp .gif .jfif .jpeg .jpg .png .webp .3fr .ari .arw .cap .cin .cr2 .cr3 .crw .dcr .dng .erf .fff .iiq .k25 .kdc .mrw .nef .nrw .orf .ori .pef .psd .raf .raw .rw2 .rwl .sr2 .srf .srw .x3f .heic .heif .hif .insp .jp2 .jpe .jxl .mpo .svg .tif .tiff`)

// knownMedia is every extension the app will ever upload. A server's
// media-types list can only narrow it: a malicious or intercepted server
// must not be able to widen uploads to documents or keys in watched folders.
// When Immich adds a format, add it to fallbackExtensions.
var knownMedia = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range fallbackExtensions {
		m[e] = true
	}
	return m
}()

// mediaOnly keeps the known media extensions from a server's list.
func mediaOnly(m immich.MediaTypes) []string {
	var out []string
	for _, e := range append(append([]string{}, m.Image...), m.Video...) {
		if e = strings.ToLower(e); knownMedia[e] {
			out = append(out, e)
		}
	}
	return out
}

// account owns one Immich account: it tracks whether the server is
// reachable and uploads that account's queue while it is.
type account struct {
	cfg     config.Account
	client  *immich.Client
	store   *state.Store
	log     *slog.Logger
	opt     Options
	folders []string // watched folders bound to this account in the running config

	online     atomic.Bool
	authFailed atomic.Bool
	failStreak atomic.Int32 // consecutive uploads that failed with network/5xx errors
	paused     atomic.Bool

	statMu     sync.Mutex
	uploading  string
	uploaded   int
	lastUpload time.Time
	lastError  string
	wake       chan struct{} // something new is queued
	recheck    chan struct{} // an upload hit a network error

	extMu sync.RWMutex
	exts  map[string]bool
}

func newAccount(cfg config.Account, c *immich.Client, store *state.Store, log *slog.Logger, opt Options) *account {
	a := &account{
		cfg: cfg, client: c, store: store, opt: opt,
		log:     log.With("account", cfg.Name),
		wake:    make(chan struct{}, 1),
		recheck: make(chan struct{}, 1),
	}
	exts := fallbackExtensions
	var cached immich.MediaTypes
	if ok, _ := store.GetMeta("media-types:"+cfg.Server, &cached); ok {
		if m := mediaOnly(cached); len(m) > 0 {
			exts = m
		}
	}
	a.setExts(exts)
	return a
}

func (a *account) setExts(exts []string) {
	m := make(map[string]bool, len(exts))
	for _, e := range exts {
		m[strings.ToLower(e)] = true
	}
	a.extMu.Lock()
	a.exts = m
	a.extMu.Unlock()
}

func (a *account) accepts(path string) bool {
	a.extMu.RLock()
	defer a.extMu.RUnlock()
	return a.exts[strings.ToLower(filepath.Ext(path))]
}

func (a *account) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *account) run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.connectivity(ctx) }()
	go func() { defer wg.Done(); a.uploader(ctx) }()
	wg.Wait()
}

// connectivity pings the server: every PingUp while reachable, with
// exponential backoff while not. It is the only reachability check.
func (a *account) connectivity(ctx context.Context) {
	backoff := a.opt.PingDownMin
	first := true
	for {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := a.client.Ping(pctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		var wait time.Duration
		if err == nil {
			if !a.online.Load() {
				a.log.Info("server reachable, uploads running", "server", a.cfg.Server)
				a.refreshMediaTypes(ctx)
				a.online.Store(true)
				a.poke()
			}
			backoff = a.opt.PingDownMin
			wait = a.opt.PingUp
		} else {
			if a.online.Load() || first {
				a.log.Warn("server unreachable, uploads paused (files are still queued)", "server", a.cfg.Server, "err", err)
			}
			a.online.Store(false)
			wait = backoff
			backoff = min(backoff*2, a.opt.PingDownMax)
		}
		first = false
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-a.recheck:
			// An upload hit a network or server error. Wait before probing,
			// growing with consecutive failures, so a server that answers
			// ping but fails uploads isn't retried in a tight loop.
			streak := int(a.failStreak.Load())
			d := min(a.opt.PingDownMin<<min(streak-1, 10), a.opt.PingDownMax)
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
		}
	}
}

func (a *account) refreshMediaTypes(ctx context.Context) {
	m, err := a.client.MediaTypes(ctx)
	if err != nil {
		return
	}
	exts := mediaOnly(m)
	if len(exts) == 0 {
		return
	}
	a.setExts(exts)
	_ = a.store.SetMeta("media-types:"+a.cfg.Server, immich.MediaTypes{Image: exts})
}

// goOffline pauses uploads after a network or server error and asks the
// connectivity loop to start probing.
func (a *account) goOffline(err error) {
	a.failStreak.Add(1)
	if a.online.Swap(false) {
		a.log.Warn("server unreachable, uploads paused (files are still queued)", "server", a.cfg.Server, "err", err)
	}
	select {
	case a.recheck <- struct{}{}:
	default:
	}
}

func (a *account) uploader(ctx context.Context) {
	t := time.NewTicker(a.opt.RetryTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
		case <-t.C:
		}
		if a.online.Load() && !a.authFailed.Load() && !a.paused.Load() {
			a.drain(ctx)
		}
	}
}

const batchSize = 50

// drain uploads due files until the queue is empty or the server goes away.
func (a *account) drain(ctx context.Context) {
	for ctx.Err() == nil && a.online.Load() && !a.paused.Load() {
		due, err := a.store.Due(a.cfg.Name, time.Now(), batchSize)
		if err != nil {
			a.log.Error("state database read failed", "err", err)
			return
		}
		if len(due) == 0 {
			return
		}
		// A folder removed or bound to another account since these were
		// queued: drop them here. If the folder is watched again, the
		// file is re-hashed for whichever account it then belongs to.
		kept := due[:0]
		for _, d := range due {
			if a.owns(d.Path) {
				kept = append(kept, d)
				continue
			}
			d.Status, d.LastError = state.Rejected, "folder no longer uploads to this account"
			d.Size = -1
			a.put(d)
		}
		if due = kept; len(due) == 0 {
			continue
		}
		items := make([]immich.CheckItem, len(due))
		for i, d := range due {
			items[i] = immich.CheckItem{ID: strconv.Itoa(i), Checksum: d.SHA1}
		}
		results, err := a.client.BulkUploadCheck(ctx, items)
		if err != nil {
			a.handleErr(err, nil)
			return
		}
		byID := map[string]immich.CheckResult{}
		for _, r := range results {
			byID[r.ID] = r
		}
		for i, d := range due {
			if ctx.Err() != nil || !a.online.Load() || a.paused.Load() {
				return
			}
			r := byID[strconv.Itoa(i)]
			switch {
			case r.Action == "reject" && r.Reason == "duplicate":
				d.Status, d.AssetID = state.Duplicate, r.AssetID
				a.log.Info("already in Immich, skipped", "path", d.Path)
				a.put(d)
			case r.Action == "reject":
				d.Status, d.LastError = state.Rejected, "server rejected: "+r.Reason
				a.log.Warn("server won't accept file", "path", d.Path, "reason", r.Reason)
				a.put(d)
			default:
				if !a.uploadOne(ctx, d) {
					return
				}
			}
		}
	}
}

// owns reports whether path is in a folder bound to this account.
func (a *account) owns(path string) bool {
	for _, f := range a.folders {
		if config.Within(path, f) {
			return true
		}
	}
	return false
}

// uploadOne uploads one file; it returns false if draining should stop.
func (a *account) uploadOne(ctx context.Context, d state.Entry) bool {
	// Open once, check that what was opened is the file that was hashed,
	// and upload from that handle: a symlink or FIFO swapped in after
	// hashing is never read.
	f, st, err := fsafe.OpenRegular(d.Path)
	if err == nil && (st.Size() != d.Size || st.ModTime().UnixNano() != d.ModTime) {
		f.Close()
		err = fsafe.ErrNotRegular // treat as changed
	}
	if errors.Is(err, fsafe.ErrNotRegular) || errors.Is(err, fs.ErrNotExist) {
		// Gone or changed since hashing. Drop it from the queue; if it still
		// exists, the watcher or next rescan re-hashes it.
		d.Status, d.LastError = state.Rejected, "file changed or removed before upload"
		d.Size = -1 // forces a re-hash if the file is seen again
		a.put(d)
		return true
	}
	if err != nil {
		return a.handleErr(err, &d) // e.g. locked by another program: retry this file later
	}
	defer f.Close()
	start := time.Now()
	a.setUploading(d.Path)
	res, err := a.client.Upload(ctx, f, filepath.Base(d.Path), d.SHA1, st.ModTime())
	a.setUploading("")
	if err != nil {
		return a.handleErr(err, &d)
	}
	a.failStreak.Store(0)
	d.AssetID, d.Attempts, d.NextTry, d.LastError = res.ID, 0, 0, ""
	if res.Status == "duplicate" {
		d.Status = state.Duplicate
	} else {
		d.Status = state.Uploaded
	}
	a.put(d)
	a.statMu.Lock() // after put, so status never shows a file as both uploaded and queued
	a.uploaded++
	a.lastUpload = time.Now()
	a.lastError = ""
	a.statMu.Unlock()
	a.log.Info("uploaded", "path", d.Path, "status", res.Status, "bytes", d.Size, "took", time.Since(start).Round(time.Millisecond))
	return true
}

// handleErr classifies an error. 401 stops the account until the user logs
// in again. Any other failure while uploading a file is charged to that
// file (it is retried later, with backoff), so one bad file can't stay at
// the head of the queue. On top of that, network errors and 5xx pause the
// account; a file that can't be read locally doesn't. It returns whether
// draining can continue.
func (a *account) handleErr(err error, d *state.Entry) bool {
	if errors.Is(err, context.Canceled) {
		return false // shutting down or reloading; the file stays queued
	}
	a.statMu.Lock()
	a.lastError = err.Error()
	a.statMu.Unlock()
	var apiErr *immich.APIError
	isAPI := errors.As(err, &apiErr)
	if isAPI && apiErr.Status == http.StatusUnauthorized {
		a.authFailed.Store(true)
		a.log.Error("the API key was rejected; uploads stopped for this account. Log in again: immich-courier login --account "+a.cfg.Name+" --server "+a.cfg.Server, "err", err)
		return false
	}
	if d != nil {
		d.Status = state.Failed
		d.Attempts++
		d.LastError = err.Error()
		d.NextTry = time.Now().Add(retryDelay(d.Attempts)).Unix()
		a.put(*d)
	}
	var pathErr *fs.PathError
	switch {
	case d != nil && errors.As(err, &pathErr):
		// The file couldn't be read (locked, permissions); not a server problem.
	case !isAPI || apiErr.Status >= 500:
		a.goOffline(err)
		return false
	case d == nil:
		a.log.Error("upload check failed", "err", err)
		return false
	}
	a.log.Warn("upload failed, will retry", "path", d.Path, "attempt", d.Attempts, "err", err)
	return true
}

func retryDelay(attempts int) time.Duration {
	d := time.Minute << min(attempts-1, 9) // 1m, 2m, 4m ... capped
	return min(d, 6*time.Hour)
}

func (a *account) put(d state.Entry) {
	if err := a.store.Put(d.Path, d.Record); err != nil {
		a.log.Error("state database write failed", "err", err)
	}
}

func (a *account) setUploading(path string) {
	a.statMu.Lock()
	a.uploading = path
	a.statMu.Unlock()
}

func (a *account) status() AccountStatus {
	q, _ := a.store.QueueLen(a.cfg.Name)
	a.statMu.Lock()
	defer a.statMu.Unlock()
	return AccountStatus{
		Name: a.cfg.Name, Server: a.cfg.Server,
		Online: a.online.Load(), Paused: a.paused.Load(), AuthFailed: a.authFailed.Load(),
		Queued: q, Uploading: a.uploading, Uploaded: a.uploaded,
		LastUpload: a.lastUpload, LastError: a.lastError,
	}
}
