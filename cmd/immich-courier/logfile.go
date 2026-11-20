package main

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// maxLogSize caps the tray's log file. When a write would take it past
// this, the file is moved to <log>.1 (replacing the previous one) and a new
// one is started, so the two together stay around 2 × maxLogSize no matter
// how long the app runs.
const maxLogSize = 5 << 20

// rotatingLog is an append-only log file that rotates while running, not
// only at startup: with Start at login the app can run for weeks.
type rotatingLog struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

// openLog opens the log at p. If it can't, logging goes to stderr.
func openLog(p string) io.WriteCloser {
	r := &rotatingLog{path: p, max: maxLogSize}
	if err := r.open(); err != nil {
		fmt.Fprintln(os.Stderr, "can't open log:", err)
		return nopCloser{os.Stderr}
	}
	return r
}

func (r *rotatingLog) open() error {
	if st, err := os.Stat(r.path); err == nil && st.Size() >= r.max {
		r.moveAside()
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

// moveAside renames the log to <log>.1. If that fails (e.g. another
// program holds the file on Windows), it empties the log instead, so the
// size cap holds either way.
func (r *rotatingLog) moveAside() {
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		_ = os.Truncate(r.path, 0)
	}
}

// Write appends p. slog writes one whole record per call, so a rotation
// never splits a line.
func (r *rotatingLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil && r.size > 0 && r.size+int64(len(p)) > r.max {
		r.f.Close()
		r.f = nil
		r.moveAside()
		if err := r.open(); err != nil {
			fmt.Fprintln(os.Stderr, "can't reopen log:", err)
		}
	}
	if r.f == nil {
		return os.Stderr.Write(p)
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotatingLog) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
