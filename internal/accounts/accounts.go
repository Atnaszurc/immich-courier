// Package accounts adds, re-authenticates and removes Immich accounts. Both
// the CLI and the settings page use it, so every login path ends the same
// way: a verified API key in the OS keychain and the account in the config.
package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/zalando/go-keyring"

	"immich-courier/internal/auth"
	"immich-courier/internal/config"
	"immich-courier/internal/immich"
)

// KeyringService is the OS keychain service name; see KeyName for the user.
const KeyringService = "immich-courier"

// ServerInfo is what a server supports, for choosing a login method.
type ServerInfo struct {
	Server        string `json:"server"` // normalized URL
	Version       string `json:"version"`
	OAuth         bool   `json:"oauth"`
	PasswordLogin bool   `json:"passwordLogin"`
}

// NormalizeServer trims the URL, adds https:// if no scheme is given and
// drops a trailing slash or /api suffix.
func NormalizeServer(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("enter the address of your Immich server")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%q isn't a valid server address", s)
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/api")
	u.RawQuery, u.Fragment = "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,39}$`)

// ValidName checks an account name (it's also the keychain entry name).
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return errors.New("account names use letters, digits, space, dot, dash or underscore (max 40)")
	}
	return nil
}

// Probe checks that the server is an Immich server and reports how users can log in.
func Probe(ctx context.Context, server string) (ServerInfo, error) {
	server, err := NormalizeServer(server)
	if err != nil {
		return ServerInfo{}, err
	}
	c := immich.New(server)
	if err := c.Ping(ctx); err != nil {
		return ServerInfo{}, fmt.Errorf("can't reach an Immich server at %s: %w", server, err)
	}
	info := ServerInfo{Server: server}
	if v, err := c.Version(ctx); err == nil {
		info.Version = v.String()
	}
	f, err := c.Features(ctx)
	if err != nil {
		return ServerInfo{}, err
	}
	info.OAuth, info.PasswordLogin = f.OAuth, f.PasswordLogin
	return info, nil
}

// KeyFromSession creates the app's API key from a temporary login session
// and logs that session out. warn is set if only the logout failed.
func KeyFromSession(ctx context.Context, server string, s auth.Session) (secret string, warn, err error) {
	host, _ := os.Hostname()
	key, err := auth.MintAPIKey(ctx, server, s, "immich-courier ("+host+")")
	if err != nil && key.Secret == "" {
		return "", nil, err
	}
	return key.Secret, err, nil
}

// KeyName is the keychain entry for an account: its name plus a short hash
// of the config file and the server. Two config files that both have a
// "home" account, or an account that later points at another server, never
// share a key, so a key is only ever sent to the server it was made for.
func KeyName(cfgPath, name, server string) string {
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		abs = cfgPath
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		abs = strings.ToLower(abs) // same file, however it was typed
	}
	sum := sha256.Sum256([]byte(abs + "\n" + server))
	return name + "@" + hex.EncodeToString(sum[:6])
}

// LoadKey returns the stored API key for an account in this config.
func LoadKey(cfgPath string, a config.Account) (string, error) {
	kn := KeyName(cfgPath, a.Name, a.Server)
	k, err := keyring.Get(KeyringService, kn)
	if errors.Is(err, keyring.ErrNotFound) {
		// Early builds stored keys under the bare account name; move it.
		if old, lerr := keyring.Get(KeyringService, a.Name); lerr == nil {
			if keyring.Set(KeyringService, kn, old) == nil {
				_ = keyring.Delete(KeyringService, a.Name)
			}
			return old, nil
		}
	}
	return k, err
}

// Save verifies the API key, stores it in the keychain and adds the
// account to the config, or re-authenticates an existing one. An existing
// account keeps its server and Immich user: logging in to another server
// or as another user under the same name is refused, so its folders never
// silently start uploading somewhere else. It returns the key's user.
func Save(ctx context.Context, cfgPath, name, server, secret string) (immich.User, error) {
	if err := ValidName(name); err != nil {
		return immich.User{}, err
	}
	server, err := NormalizeServer(server)
	if err != nil {
		return immich.User{}, err
	}
	secret = strings.TrimSpace(secret)
	if secret == "" || strings.IndexFunc(secret, func(r rune) bool { return r <= ' ' || r > '~' }) >= 0 {
		return immich.User{}, errors.New("that doesn't look like an API key")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return immich.User{}, err
	}
	old, exists := cfg.AccountFold(name)
	if exists && old.Name != name {
		return immich.User{}, fmt.Errorf("there is already an account named %q; choose another name", old.Name)
	}
	if exists && old.Server != server {
		return immich.User{}, fmt.Errorf("account %q uploads to %s. To upload to %s, add it as a new account with another name", name, old.Server, server)
	}
	c := immich.New(server)
	c.APIKey = secret
	u, err := c.Me(ctx)
	if err != nil {
		var apiErr *immich.APIError
		if errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403) {
			return immich.User{}, fmt.Errorf("the API key was rejected; it needs at least these permissions: %s", strings.Join(auth.Permissions, ", "))
		}
		return immich.User{}, fmt.Errorf("couldn't check the API key: %w", err)
	}
	if exists && old.UserID != "" && old.UserID != u.ID {
		return immich.User{}, fmt.Errorf("that login is %s <%s>, but account %q uploads as another Immich user. Log in as that user, or add a new account", u.Name, u.Email, name)
	}
	// Keychain last, and undone if the config can't be saved, so a key
	// never sits next to a config that doesn't match it.
	kn := KeyName(cfgPath, name, server)
	prev, prevErr := keyring.Get(KeyringService, kn)
	if err := keyring.Set(KeyringService, kn, secret); err != nil {
		return immich.User{}, fmt.Errorf("couldn't store the key in the OS keychain: %w", err)
	}
	cfg.SetAccount(config.Account{Name: name, Server: server, UserID: u.ID})
	if err := cfg.Save(cfgPath); err != nil {
		if prevErr == nil {
			_ = keyring.Set(KeyringService, kn, prev)
		} else {
			_ = keyring.Delete(KeyringService, kn)
		}
		return immich.User{}, err
	}
	return u, nil
}

// HasKey reports whether the keychain holds a key for the account.
func HasKey(cfgPath string, a config.Account) bool {
	_, err := LoadKey(cfgPath, a)
	return err == nil
}

// DeleteKey removes an account's key from the keychain (also an early
// build's entry under the bare name). A missing key is not an error.
func DeleteKey(cfgPath string, a config.Account) error {
	err := keyring.Delete(KeyringService, KeyName(cfgPath, a.Name, a.Server))
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	_ = keyring.Delete(KeyringService, a.Name)
	return nil
}

// Remove deletes an account from the config and its key from the keychain.
// Folders that upload to the account are only removed with it if
// withFolders is set; otherwise it refuses while any exist. The API key
// itself stays in Immich (the app's key can't delete keys).
func Remove(cfgPath, name string, withFolders bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	kept := cfg.Folders[:0:0]
	for _, f := range cfg.Folders {
		if f.Account != name {
			kept = append(kept, f)
			continue
		}
		if !withFolders {
			return fmt.Errorf("folder %s still uploads to %q; remove that folder first", f.Path, name)
		}
	}
	cfg.Folders = kept
	var removed *config.Account
	for i, a := range cfg.Accounts {
		if a.Name == name {
			cfg.Accounts = append(cfg.Accounts[:i], cfg.Accounts[i+1:]...)
			removed = &a
			break
		}
	}
	if removed == nil {
		return fmt.Errorf("no account named %q", name)
	}
	if err := cfg.Save(cfgPath); err != nil {
		return err
	}
	return DeleteKey(cfgPath, *removed)
}
