package engine

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"immich-courier/internal/state"
)

// Regression tests for the 2026-11 security audit (run-1).

// A server's media-types list can narrow what's uploaded, never widen it to
// documents, keys or extensionless files, also not after a restart from the
// cached list.
func TestServerCantWidenMediaTypes(t *testing.T) {
	fake := &fakeImmich{assets: map[string]string{}, media: `{"image":[".jpg",".txt",""],"video":[".kdbx"]}`}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	dir := t.TempDir()
	db := filepath.Join(t.TempDir(), "state.db")

	for round, ok := range []string{"ok1.jpg", "ok2.jpg"} {
		h := start(t, fake, srv.URL, dir, db)
		os.WriteFile(filepath.Join(dir, "first-"+ok), []byte("photo"+ok), 0o644)
		waitFor(t, "server online", func() bool { return fake.has("first-" + ok) })
		for _, n := range []string{"notes.txt", "vault.kdbx", "noext"} {
			os.WriteFile(filepath.Join(dir, n), []byte("secret "+n), 0o644)
		}
		os.WriteFile(filepath.Join(dir, ok), []byte("photo-"+ok), 0o644)
		waitFor(t, ok+" uploaded", func() bool { return fake.has(ok) })
		time.Sleep(500 * time.Millisecond)
		for _, n := range []string{"notes.txt", "vault.kdbx", "noext"} {
			if _, seen, _ := h.store.Get(filepath.Join(dir, n)); seen || fake.has(n) {
				t.Fatalf("round %d: %s was queued or uploaded", round, n)
			}
		}
		h.stop()
	}
}

// One file the server keeps failing (500) must not hold up the rest of the
// account's queue forever.
func TestFailingFileDoesNotBlockAccount(t *testing.T) {
	fake := &fakeImmich{assets: map[string]string{}, failName: "a-bad.jpg"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	dir := t.TempDir()
	h := start(t, fake, srv.URL, dir, filepath.Join(t.TempDir(), "state.db"))
	defer h.stop()

	os.WriteFile(filepath.Join(dir, "a-bad.jpg"), []byte("poison"), 0o644) // sorts first in the queue
	waitFor(t, "bad file tried", func() bool { return fake.failures.Load() > 0 })
	os.WriteFile(filepath.Join(dir, "b-good.jpg"), []byte("fine"), 0o644)
	waitFor(t, "good file uploaded", func() bool { return fake.has("b-good.jpg") })

	r, _, _ := h.store.Get(filepath.Join(dir, "a-bad.jpg"))
	if r.Status != state.Failed || r.Attempts < 1 || r.NextTry <= time.Now().Unix() {
		t.Fatalf("bad file not charged for its failure: %+v", r)
	}
	if n := fake.failures.Load(); n != 1 {
		t.Fatalf("bad file retried %d times right away, want once", n)
	}
}

// Files queued for a folder that is then removed ("Stop watching") are not
// uploaded afterwards.
func TestRemovedFolderDropsQueuedFiles(t *testing.T) {
	fake := &fakeImmich{assets: map[string]string{}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	removed, kept := t.TempDir(), t.TempDir()
	db := filepath.Join(t.TempDir(), "state.db")

	h := startWith(t, fake, srv.URL, db, removed, kept)
	fake.offline.Store(true)
	time.Sleep(300 * time.Millisecond)
	withdrawn := filepath.Join(removed, "private.jpg")
	os.WriteFile(withdrawn, []byte("private"), 0o644)
	waitFor(t, "private.jpg queued", func() bool {
		r, ok, _ := h.store.Get(withdrawn)
		return ok && r.Status == state.Queued
	})
	h.stop()

	fake.offline.Store(false)
	h = startWith(t, fake, srv.URL, db, kept)
	defer h.stop()
	os.WriteFile(filepath.Join(kept, "ok.jpg"), []byte("ok"), 0o644)
	waitFor(t, "ok.jpg uploaded", func() bool { return fake.has("ok.jpg") })
	waitFor(t, "private.jpg dropped", func() bool {
		r, _, _ := h.store.Get(withdrawn)
		return r.Status == state.Rejected
	})
	if fake.has("private.jpg") {
		t.Fatal("file from a removed folder was uploaded")
	}
}

// A symlink named like a photo must not upload the file it points to.
func TestSymlinkIsNotFollowed(t *testing.T) {
	fake := &fakeImmich{assets: map[string]string{}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	dir, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret.jpg")
	os.WriteFile(secret, []byte("not yours"), 0o644)
	if err := os.Symlink(secret, filepath.Join(dir, "innocent.jpg")); err != nil {
		t.Skip("can't create symlinks here:", err)
	}
	h := start(t, fake, srv.URL, dir, filepath.Join(t.TempDir(), "state.db"))
	defer h.stop()

	os.WriteFile(filepath.Join(dir, "real.jpg"), []byte("real"), 0o644)
	waitFor(t, "real.jpg uploaded", func() bool { return fake.has("real.jpg") })
	os.Symlink(secret, filepath.Join(dir, "later.jpg")) // via the watcher, not the startup scan
	time.Sleep(800 * time.Millisecond)
	if fake.has("innocent.jpg") || fake.has("later.jpg") || fake.uploads.Load() != 1 {
		t.Fatal("a symlink's target was uploaded")
	}
}

// stopWithin fails the test if stopping the engine hangs.
func stopWithin(t *testing.T, h *harness, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { h.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("engine didn't stop: a worker is blocked")
	}
}
