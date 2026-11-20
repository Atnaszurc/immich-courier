//go:build unix

package engine

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"immich-courier/internal/state"
)

// A FIFO named like a photo must not stall the shared hash worker (and with
// it every folder and account) or block shutdown.
func TestFIFODoesNotStallHasher(t *testing.T) {
	fake := &fakeImmich{assets: map[string]string{}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	dir := t.TempDir()
	h := start(t, fake, srv.URL, dir, filepath.Join(t.TempDir(), "state.db"))

	// A regular file swapped for a FIFO while it settles: it reaches the
	// hasher, which must not open it blocking.
	p := filepath.Join(dir, "pipe.jpg")
	os.WriteFile(p, nil, 0o644)
	time.Sleep(100 * time.Millisecond)
	st, _ := os.Stat(p)
	os.Remove(p)
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		h.stop()
		t.Skip("mkfifo:", err)
	}
	os.Chtimes(p, st.ModTime(), st.ModTime())

	time.Sleep(500 * time.Millisecond)
	os.WriteFile(filepath.Join(dir, "after.jpg"), []byte("after"), 0o644)
	waitFor(t, "after.jpg uploaded", func() bool { return fake.has("after.jpg") })
	stopWithin(t, h, 3*time.Second)
}

// A queued file swapped for a FIFO must not block the account's uploader.
func TestFIFOSwappedIntoQueueDoesNotBlockUploader(t *testing.T) {
	fake := &fakeImmich{assets: map[string]string{}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	dir := t.TempDir()
	h := start(t, fake, srv.URL, dir, filepath.Join(t.TempDir(), "state.db"))

	fake.offline.Store(true)
	time.Sleep(300 * time.Millisecond)
	p := filepath.Join(dir, "a.jpg")
	os.WriteFile(p, nil, 0o644) // zero bytes, like a FIFO
	waitFor(t, "a.jpg queued", func() bool {
		r, ok, _ := h.store.Get(p)
		return ok && r.Status == state.Queued
	})
	st, _ := os.Stat(p)
	os.Remove(p)
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		h.stop()
		t.Skip("mkfifo:", err)
	}
	os.Chtimes(p, st.ModTime(), st.ModTime())
	fake.offline.Store(false)

	os.WriteFile(filepath.Join(dir, "b.jpg"), []byte("b"), 0o644)
	waitFor(t, "b.jpg uploaded", func() bool { return fake.has("b.jpg") })
	if r, _, _ := h.store.Get(p); r.Status != state.Rejected {
		t.Fatalf("FIFO entry status %v, want rejected", r.Status)
	}
	stopWithin(t, h, 3*time.Second)
}
