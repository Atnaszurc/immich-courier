// Package config holds the user's accounts and watched folders. Secrets are
// never stored here; API keys live in the OS keychain, keyed by account name.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Settings struct {
	// Seconds a file's size and mtime must stay unchanged before it's queued.
	SettleSeconds int `toml:"settle_seconds"`
	// Minutes between full rescans of watched folders (safety net for missed events).
	RescanMinutes int `toml:"rescan_minutes"`
}

type Account struct {
	Name   string `toml:"name"`
	Server string `toml:"server"`
}

type Folder struct {
	Path    string `toml:"path"`
	Account string `toml:"account"`
}

type Config struct {
	Settings Settings  `toml:"settings"`
	Accounts []Account `toml:"account"`
	Folders  []Folder  `toml:"folder"`
}

func (s Settings) Settle() time.Duration {
	if s.SettleSeconds <= 0 {
		return 5 * time.Second
	}
	return time.Duration(s.SettleSeconds) * time.Second
}

func (s Settings) Rescan() time.Duration {
	if s.RescanMinutes <= 0 {
		return 15 * time.Minute
	}
	return time.Duration(s.RescanMinutes) * time.Minute
}

// Dir is the per-user directory for config and state.
func Dir() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "immich-courier"), nil
}

// DefaultPath is <Dir>/config.toml.
func DefaultPath() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.toml"), nil
}

// Load reads the config. A missing file is an empty config, not an error.
func Load(path string) (*Config, error) {
	c := &Config{}
	_, err := toml.DecodeFile(path, c)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	for i := range c.Folders {
		p, err := ExpandPath(c.Folders[i].Path)
		if err != nil {
			return nil, err
		}
		c.Folders[i].Path = p
	}
	return c, c.Validate()
}

// Save writes the config atomically.
func (c *Config) Save(path string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *Config) Account(name string) (Account, bool) {
	for _, a := range c.Accounts {
		if a.Name == name {
			return a, true
		}
	}
	return Account{}, false
}

// SetAccount adds or updates an account.
func (c *Config) SetAccount(a Account) {
	for i := range c.Accounts {
		if c.Accounts[i].Name == a.Name {
			c.Accounts[i] = a
			return
		}
	}
	c.Accounts = append(c.Accounts, a)
}

// AddFolder adds a watched folder after validating it.
func (c *Config) AddFolder(f Folder) error {
	p, err := ExpandPath(f.Path)
	if err != nil {
		return err
	}
	f.Path = p
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", p)
	}
	next := *c
	next.Folders = append(append([]Folder{}, c.Folders...), f)
	if err := next.Validate(); err != nil {
		return err
	}
	c.Folders = next.Folders
	return nil
}

// RemoveFolder removes a watched folder; it reports whether it was found.
func (c *Config) RemoveFolder(path string) (bool, error) {
	p, err := ExpandPath(path)
	if err != nil {
		return false, err
	}
	for i, f := range c.Folders {
		if samePath(f.Path, p) {
			c.Folders = append(c.Folders[:i], c.Folders[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func (c *Config) Validate() error {
	names := map[string]bool{}
	for _, a := range c.Accounts {
		if a.Name == "" || a.Server == "" {
			return errors.New("every account needs a name and a server")
		}
		if names[a.Name] {
			return fmt.Errorf("account %q is defined twice", a.Name)
		}
		names[a.Name] = true
	}
	for i, f := range c.Folders {
		if !names[f.Account] {
			return fmt.Errorf("folder %s uses unknown account %q (run login first)", f.Path, f.Account)
		}
		if !filepath.IsAbs(f.Path) {
			return fmt.Errorf("folder path %q must be absolute", f.Path)
		}
		for _, g := range c.Folders[i+1:] {
			if Within(f.Path, g.Path) || Within(g.Path, f.Path) {
				return fmt.Errorf("folders %s and %s overlap; watched folders can't be nested", f.Path, g.Path)
			}
		}
	}
	return nil
}

// ExpandPath expands a leading ~ and returns a clean absolute path.
func ExpandPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[1:])
	}
	return filepath.Abs(p)
}

// Within reports whether path is dir or inside dir.
func Within(path, dir string) bool {
	if caseInsensitiveFS() {
		path, dir = strings.ToLower(path), strings.ToLower(dir)
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func samePath(a, b string) bool {
	if caseInsensitiveFS() {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func caseInsensitiveFS() bool { return runtime.GOOS == "windows" || runtime.GOOS == "darwin" }
