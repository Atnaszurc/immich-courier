package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The log must stay capped while the app runs, not only at startup.
func TestLogRotatesWhileRunning(t *testing.T) {
	p := filepath.Join(t.TempDir(), "immich-courier.log")
	r := &rotatingLog{path: p, max: 1000}
	if err := r.open(); err != nil {
		t.Fatal(err)
	}
	line := []byte(strings.Repeat("x", 99) + "\n") // 100 bytes, like one slog record
	for i := 0; i < 55; i++ {                      // 5,500 bytes: several rotations
		if _, err := r.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	r.Close()

	cur, _ := os.ReadFile(p)
	old, err := os.ReadFile(p + ".1")
	if err != nil {
		t.Fatal("no rotated copy:", err)
	}
	for name, b := range map[string][]byte{"log": cur, "log.1": old} {
		if len(b) > 1000 {
			t.Errorf("%s is %d bytes, cap is 1000", name, len(b))
		}
		if len(b)%len(line) != 0 || (len(b) > 0 && !bytes.HasSuffix(b, []byte("\n"))) {
			t.Errorf("%s has a split line (%d bytes)", name, len(b))
		}
	}
	if len(cur) != 500 { // 55 lines = 5 full files of 10, then 5 lines
		t.Errorf("current log has %d bytes, want 500", len(cur))
	}
}

// An oversized log left by an earlier run is moved aside at startup.
func TestLogRotatesAtOpen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "immich-courier.log")
	os.WriteFile(p, bytes.Repeat([]byte("y"), 2000), 0o600)
	r := &rotatingLog{path: p, max: 1000}
	if err := r.open(); err != nil {
		t.Fatal(err)
	}
	r.Write([]byte("new\n"))
	r.Close()
	if b, _ := os.ReadFile(p); string(b) != "new\n" {
		t.Fatalf("log after open = %q", b)
	}
	if st, err := os.Stat(p + ".1"); err != nil || st.Size() != 2000 {
		t.Fatal("old log not kept as .1")
	}
}
