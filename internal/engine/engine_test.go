package engine

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"immich-courier/internal/config"
	"immich-courier/internal/immich"
	"immich-courier/internal/state"
)

// fakeImmich stores uploaded checksums and can be switched "offline", in
// which case every request gets a 502 like a reverse proxy in front of a
// stopped server.
type fakeImmich struct {
	mu       sync.Mutex
	assets   map[string]string // checksum -> filename
	uploads  atomic.Int32
	offline  atomic.Bool
	media    string // media-types response; empty means .jpg and .mp4
	failName string // uploads of this file name get a 500
	failures atomic.Int32
}

func (f *fakeImmich) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/server/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"res":"pong"}`))
	})
	mux.HandleFunc("GET /api/server/media-types", func(w http.ResponseWriter, r *http.Request) {
		if f.media != "" {
			w.Write([]byte(f.media))
			return
		}
		w.Write([]byte(`{"image":[".jpg"],"video":[".mp4"],"sidecar":[".xmp"]}`))
	})
	mux.HandleFunc("POST /api/assets/bulk-upload-check", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Assets []struct{ ID, Checksum string } `json:"assets"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		var out []map[string]string
		f.mu.Lock()
		for _, a := range in.Assets {
			if _, ok := f.assets[a.Checksum]; ok {
				out = append(out, map[string]string{"id": a.ID, "action": "reject", "reason": "duplicate", "assetId": "existing"})
			} else {
				out = append(out, map[string]string{"id": a.ID, "action": "accept"})
			}
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"results": out})
	})
	mux.HandleFunc("POST /api/assets", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" {
			http.Error(w, `{"message":"unauthorized"}`, 401)
			return
		}
		file, hdr, err := r.FormFile("assetData")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if f.failName != "" && hdr.Filename == f.failName {
			f.failures.Add(1)
			http.Error(w, `{"message":"boom"}`, 500)
			return
		}
		h := sha1.New()
		io.Copy(h, file)
		sum := hex.EncodeToString(h.Sum(nil))
		if sum != r.Header.Get("x-immich-checksum") {
			http.Error(w, `{"message":"checksum mismatch"}`, 400)
			return
		}
		if r.FormValue("fileCreatedAt") == "" {
			http.Error(w, `{"message":"missing fileCreatedAt"}`, 400)
			return
		}
		f.mu.Lock()
		f.assets[sum] = hdr.Filename
		f.mu.Unlock()
		f.uploads.Add(1)
		w.WriteHeader(201)
		w.Write([]byte(`{"id":"new","status":"created"}`))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.offline.Load() {
			http.Error(w, "Bad Gateway", 502)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (f *fakeImmich) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.assets {
		if n == name {
			return true
		}
	}
	return false
}

func fastOptions() Options {
	return Options{
		Settle:      300 * time.Millisecond,
		Rescan:      time.Hour,
		Tick:        50 * time.Millisecond,
		PingUp:      200 * time.Millisecond,
		PingDownMin: 100 * time.Millisecond,
		PingDownMax: 200 * time.Millisecond,
		RetryTick:   100 * time.Millisecond,
	}
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

type harness struct {
	fake  *fakeImmich
	dir   string
	store *state.Store
	stop  func()
}

func start(t *testing.T, fake *fakeImmich, srvURL, dir, dbPath string) *harness {
	t.Helper()
	return startWith(t, fake, srvURL, dbPath, dir)
}

// startWith runs an engine with one account "a" watching all dirs.
func startWith(t *testing.T, fake *fakeImmich, srvURL, dbPath string, dirs ...string) *harness {
	t.Helper()
	return startCreds(t, fake, srvURL, dbPath, Credentials{APIKey: "k"}, dirs...)
}

func startCreds(t *testing.T, fake *fakeImmich, srvURL, dbPath string, cr Credentials, dirs ...string) *harness {
	t.Helper()
	cfg := &config.Config{Accounts: []config.Account{{Name: "a", Server: srvURL}}}
	for _, d := range dirs {
		cfg.Folders = append(cfg.Folders, config.Folder{Path: d, Account: "a"})
	}
	dir := dirs[0]
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	e, err := New(cfg, map[string]Credentials{"a": cr}, store, log, fastOptions())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	return &harness{fake: fake, dir: dir, store: store, stop: func() {
		cancel()
		<-done
		store.Close()
	}}
}

func TestUploadDedupeAndOffline(t *testing.T) {
	fake := &fakeImmich{assets: map[string]string{}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	dir := t.TempDir()
	h := start(t, fake, srv.URL, dir, filepath.Join(t.TempDir(), "state.db"))
	defer h.stop()

	// A new photo is uploaded.
	os.WriteFile(filepath.Join(dir, "a.jpg"), []byte("photo-a"), 0o644)
	waitFor(t, "a.jpg uploaded", func() bool { return fake.has("a.jpg") })

	// Same content under another name: detected as duplicate, not uploaded again.
	os.WriteFile(filepath.Join(dir, "copy-of-a.jpg"), []byte("photo-a"), 0o644)
	waitFor(t, "copy marked duplicate", func() bool {
		r, ok, _ := h.store.Get(filepath.Join(dir, "copy-of-a.jpg"))
		return ok && r.Status == state.Duplicate
	})
	if n := fake.uploads.Load(); n != 1 {
		t.Fatalf("uploads = %d, want 1", n)
	}

	// Unsupported types are ignored.
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644)

	// New subfolder created after start is watched too.
	sub := filepath.Join(dir, "trip")
	os.Mkdir(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "b.mp4"), []byte("video-b"), 0o644)
	waitFor(t, "trip/b.mp4 uploaded", func() bool { return fake.has("b.mp4") })

	// Server goes away: new files are queued, not uploaded, then uploaded on return.
	fake.offline.Store(true)
	time.Sleep(300 * time.Millisecond)
	os.WriteFile(filepath.Join(dir, "c.jpg"), []byte("photo-c"), 0o644)
	waitFor(t, "c.jpg queued while offline", func() bool {
		r, ok, _ := h.store.Get(filepath.Join(dir, "c.jpg"))
		return ok && r.Status == state.Queued
	})
	time.Sleep(500 * time.Millisecond)
	if fake.has("c.jpg") {
		t.Fatal("c.jpg uploaded while server was offline")
	}
	fake.offline.Store(false)
	waitFor(t, "c.jpg uploaded after server returned", func() bool { return fake.has("c.jpg") })

	if _, ok, _ := h.store.Get(filepath.Join(dir, "notes.txt")); ok {
		t.Fatal("notes.txt should have been ignored")
	}
}

// The upload delay (Settle) gives the user time to take back a file: one
// deleted before the delay is over is never uploaded.
func TestFileDeletedDuringDelayIsNotUploaded(t *testing.T) {
	fake := &fakeImmich{assets: map[string]string{}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	dir := t.TempDir()
	h := start(t, fake, srv.URL, dir, filepath.Join(t.TempDir(), "state.db"))
	defer h.stop()

	oops := filepath.Join(dir, "oops.jpg")
	os.WriteFile(oops, []byte("wrong photo"), 0o644)
	time.Sleep(100 * time.Millisecond) // well inside the 300 ms delay
	os.Remove(oops)

	// A file that stays is still uploaded, which proves the engine was running.
	os.WriteFile(filepath.Join(dir, "keep.jpg"), []byte("right photo"), 0o644)
	waitFor(t, "keep.jpg uploaded", func() bool { return fake.has("keep.jpg") })
	if fake.has("oops.jpg") {
		t.Fatal("deleted file was uploaded")
	}
	if _, ok, _ := h.store.Get(oops); ok {
		t.Fatal("deleted file was recorded in the queue")
	}
}

func TestFileStillBeingWritten(t *testing.T) {
	fake := &fakeImmich{assets: map[string]string{}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	dir := t.TempDir()
	h := start(t, fake, srv.URL, dir, filepath.Join(t.TempDir(), "state.db"))
	defer h.stop()

	p := filepath.Join(dir, "big.mp4")
	f, _ := os.Create(p)
	for i := 0; i < 8; i++ { // keeps growing for ~800ms, longer than Settle
		f.Write([]byte("chunk"))
		f.Sync()
		time.Sleep(100 * time.Millisecond)
		if fake.has("big.mp4") {
			t.Fatal("uploaded before the file finished writing")
		}
	}
	f.Close()
	waitFor(t, "big.mp4 uploaded", func() bool { return fake.has("big.mp4") })
	r, _, _ := h.store.Get(p)
	if r.Size != 40 {
		t.Fatalf("uploaded size %d, want 40", r.Size)
	}
}

func TestRestartSkipsDoneAndUploadsQueued(t *testing.T) {
	fake := &fakeImmich{assets: map[string]string{}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	dir := t.TempDir()
	db := filepath.Join(t.TempDir(), "state.db")

	h := start(t, fake, srv.URL, dir, db)
	os.WriteFile(filepath.Join(dir, "a.jpg"), []byte("photo-a"), 0o644)
	waitFor(t, "a.jpg uploaded", func() bool { return fake.has("a.jpg") })
	// Queue a file while offline, then stop the app before it can upload.
	fake.offline.Store(true)
	time.Sleep(300 * time.Millisecond)
	os.WriteFile(filepath.Join(dir, "b.jpg"), []byte("photo-b"), 0o644)
	waitFor(t, "b.jpg queued", func() bool {
		r, ok, _ := h.store.Get(filepath.Join(dir, "b.jpg"))
		return ok && r.Status == state.Queued
	})
	h.stop()

	// Files added while the app wasn't running are found by the startup scan.
	os.WriteFile(filepath.Join(dir, "c.jpg"), []byte("photo-c"), 0o644)
	fake.offline.Store(false)
	h = start(t, fake, srv.URL, dir, db)
	defer h.stop()
	waitFor(t, "b.jpg and c.jpg uploaded after restart", func() bool { return fake.has("b.jpg") && fake.has("c.jpg") })
	if n := fake.uploads.Load(); n != 3 {
		t.Fatalf("uploads = %d, want 3 (a.jpg must not be re-uploaded)", n)
	}
}

// An account behind a proxy that wants its own header uploads with it.
func TestUploadWithCustomHeaders(t *testing.T) {
	fake := &fakeImmich{assets: map[string]string{}}
	inner := fake.handler()
	var refused atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Gate") != "open-sesame" {
			refused.Add(1)
			http.Error(w, "forbidden", 403)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	dir := t.TempDir()
	cr := Credentials{APIKey: "k", Headers: []immich.Header{{Name: "X-Gate", Value: "open-sesame"}}}
	h := startCreds(t, fake, srv.URL, filepath.Join(t.TempDir(), "state.db"), cr, dir)
	defer h.stop()
	os.WriteFile(filepath.Join(dir, "a.jpg"), []byte("photo-a"), 0o644)
	waitFor(t, "a.jpg uploaded", func() bool { return fake.has("a.jpg") })
	if n := refused.Load(); n != 0 {
		t.Fatalf("%d requests went without the custom header", n)
	}
}
