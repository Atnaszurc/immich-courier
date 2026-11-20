//go:build unix

package daemon

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A socket in a directory others can write to (e.g. one someone else
// created first in a shared /tmp) may be theirs: clients must not use it,
// and the daemon must not serve there.
func TestSocketDirOthersCanWriteIsRefused(t *testing.T) {
	base, err := os.MkdirTemp("", "icd") // short: sun_path limit
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	os.Chmod(base, 0o777) // like a pre-created squatted directory
	cfg := filepath.Join(base, "config.toml")
	ln, err := net.Listen("unix", SocketPath(cfg)) // the "attacker's" server
	if err != nil {
		t.Skip("unix socket:", err)
	}
	defer ln.Close()

	if _, err := Dial(cfg); err == nil || !strings.Contains(err.Error(), "writable by other users") {
		t.Fatalf("Dial into a world-writable socket dir: %v", err)
	}
	if err := ensurePrivateDir(base); err == nil {
		t.Fatal("daemon accepted a world-writable socket dir")
	}
	os.Chmod(base, 0o700)
	if _, err := Dial(cfg); err != nil {
		t.Fatalf("private dir refused: %v", err)
	}
}
