package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsNotWritten(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	c := &Config{Accounts: []Account{{Name: "a", Server: "https://x"}}}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "settle_seconds") || strings.Contains(string(b), "rescan_minutes") {
		t.Fatalf("default settings written out:\n%s", b)
	}
}

func TestFoldersCantNest(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	sibling := filepath.Join(root, "subway") // shares a prefix but isn't inside sub
	for _, d := range []string{sub, sibling} {
		os.Mkdir(d, 0o755)
	}
	c := &Config{Accounts: []Account{{Name: "a", Server: "https://x"}, {Name: "b", Server: "https://y"}}}
	if err := c.AddFolder(Folder{Path: sub, Account: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddFolder(Folder{Path: sibling, Account: "b"}); err != nil {
		t.Fatalf("sibling with shared prefix rejected: %v", err)
	}
	if err := c.AddFolder(Folder{Path: root, Account: "b"}); err == nil {
		t.Fatal("parent of a watched folder accepted")
	}
	if err := c.AddFolder(Folder{Path: filepath.Join(sub), Account: "b"}); err == nil {
		t.Fatal("same folder accepted twice")
	}
	if len(c.Folders) != 2 {
		t.Fatalf("folders = %d, want 2", len(c.Folders))
	}
}

func TestUnknownAccountRejected(t *testing.T) {
	c := &Config{}
	if err := c.AddFolder(Folder{Path: t.TempDir(), Account: "nope"}); err == nil {
		t.Fatal("folder with unknown account accepted")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cfg", "config.toml")
	c := &Config{Accounts: []Account{{Name: "a", Server: "https://x"}}}
	if err := c.AddFolder(Folder{Path: t.TempDir(), Account: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Folders) != 1 || got.Folders[0] != c.Folders[0] {
		t.Fatalf("round trip mismatch: %+v vs %+v", got.Folders, c.Folders)
	}
}
