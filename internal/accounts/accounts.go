// Package accounts adds, re-authenticates and removes Immich accounts. Both
// the CLI and the settings page use it, so every login path ends the same
// way: a verified API key in the OS keychain and the account in the config.
package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// Client returns a client for server that sends the given custom headers
// (already resolved and validated, see ResolveHeaders).
func Client(server string, headers []immich.Header) *immich.Client {
	c := immich.New(server)
	c.Headers = headers
	return c
}

// Probe checks that the server is an Immich server and reports how users
// can log in. headers are the custom headers a proxy in front of it needs.
func Probe(ctx context.Context, server string, headers []immich.Header) (ServerInfo, error) {
	server, err := NormalizeServer(server)
	if err != nil {
		return ServerInfo{}, err
	}
	c := Client(server, headers)
	if err := c.Ping(ctx); err != nil {
		var apiErr *immich.APIError
		if errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403 || apiErr.Status == 407) {
			// Immich answers ping without credentials, so this came from
			// something in front of it.
			if len(headers) > 0 {
				return ServerInfo{}, fmt.Errorf("%s refused the request (HTTP %d) before it reached Immich; check the custom headers", server, apiErr.Status)
			}
			return ServerInfo{}, fmt.Errorf("%s asks for a login (HTTP %d) before it reaches Immich. If it is behind a reverse proxy with its own login (for example basic auth), add the headers it needs as custom headers", server, apiErr.Status)
		}
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
func KeyFromSession(ctx context.Context, c *immich.Client, s auth.Session) (secret string, warn, err error) {
	host, _ := os.Hostname()
	key, err := auth.MintAPIKey(ctx, c, s, "immich-courier ("+host+")")
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

// headersName is the keychain entry holding an account's custom headers.
// "/" can't occur in an account name, so it never collides with a key.
func headersName(cfgPath, name, server string) string {
	return KeyName(cfgPath, name, server) + "/headers"
}

// LoadHeaders returns an account's custom headers (nil if it has none).
func LoadHeaders(cfgPath string, a config.Account) ([]immich.Header, error) {
	v, err := keyring.Get(KeyringService, headersName(cfgPath, a.Name, a.Server))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("couldn't read the custom headers from the OS keychain: %w", err)
	}
	var hs []immich.Header
	if err := json.Unmarshal([]byte(v), &hs); err != nil || immich.ValidateHeaders(hs) != nil {
		return nil, errors.New("the custom headers stored in the OS keychain are damaged; set them again")
	}
	return hs, nil
}

// storeHeaders writes (or, for an empty set, deletes) an account's custom
// headers and returns a function that puts the previous entry back.
func storeHeaders(cfgPath, name, server string, hs []immich.Header) (undo func(), err error) {
	kn := headersName(cfgPath, name, server)
	prev, prevErr := keyring.Get(KeyringService, kn)
	undo = func() {
		if prevErr == nil {
			_ = keyring.Set(KeyringService, kn, prev)
		} else {
			_ = keyring.Delete(KeyringService, kn)
		}
	}
	if len(hs) == 0 {
		if err := keyring.Delete(KeyringService, kn); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return nil, fmt.Errorf("couldn't remove the custom headers from the OS keychain: %w", err)
		}
		return undo, nil
	}
	b, err := json.Marshal(hs)
	if err != nil {
		return nil, err
	}
	if err := keyring.Set(KeyringService, kn, string(b)); err != nil {
		return nil, fmt.Errorf("couldn't store the custom headers in the OS keychain: %w", err)
	}
	return undo, nil
}

// ResolveHeaders turns custom headers entered by the user into the set to
// use for account name on server. nil means "unchanged": the account's
// stored headers (none for a new account). Otherwise in replaces them, and
// an entry with an empty value keeps that header's stored value, so a form
// can show the names without ever sending the secrets back to the page.
// Stored values are only reused for the same account on the same server.
func ResolveHeaders(cfgPath, name, server string, in []immich.Header) ([]immich.Header, error) {
	server, err := NormalizeServer(server)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	var stored []immich.Header
	var loadErr error
	if a, ok := cfg.Account(name); ok && a.Server == server {
		stored, loadErr = LoadHeaders(cfgPath, a)
	}
	if in == nil {
		return stored, loadErr
	}
	out := make([]immich.Header, 0, len(in))
	for _, h := range in {
		if strings.TrimSpace(h.Name) == "" && strings.TrimSpace(h.Value) == "" {
			continue // an empty row in a form
		}
		if strings.TrimSpace(h.Value) == "" {
			for _, s := range stored {
				if strings.EqualFold(s.Name, strings.TrimSpace(h.Name)) {
					h.Value = s.Value
				}
			}
		}
		out = append(out, h)
	}
	if err := immich.ValidateHeaders(out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetHeaders replaces an existing account's custom headers (see
// ResolveHeaders for in; nil removes them all). The stored API key must
// still work with the new headers, as the same Immich user, before
// anything is saved.
func SetHeaders(ctx context.Context, cfgPath, name string, in []immich.Header) ([]immich.Header, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	a, ok := cfg.Account(name)
	if !ok {
		return nil, fmt.Errorf("no account named %q", name)
	}
	if in == nil {
		in = []immich.Header{}
	}
	hs, err := ResolveHeaders(cfgPath, name, a.Server, in)
	if err != nil {
		return nil, err
	}
	key, err := LoadKey(cfgPath, a)
	if err != nil {
		return nil, fmt.Errorf("account %q isn't logged in; log in again and enter the headers there", name)
	}
	c := Client(a.Server, hs)
	c.APIKey = key
	u, err := c.Me(ctx)
	if err != nil {
		return nil, fmt.Errorf("the server didn't accept the account's key with these headers, so nothing was changed: %w", err)
	}
	if a.UserID != "" && u.ID != a.UserID {
		return nil, errors.New("with these headers the key belongs to another Immich user, so nothing was changed")
	}
	if _, err := storeHeaders(cfgPath, name, a.Server, hs); err != nil {
		return nil, err
	}
	return hs, nil
}

// Save verifies the API key, stores it in the keychain and adds the
// account to the config, or re-authenticates an existing one. An existing
// account keeps its server and Immich user: logging in to another server
// or as another user under the same name is refused, so its folders never
// silently start uploading somewhere else. headers is the account's full
// set of custom headers from ResolveHeaders (empty removes them). It
// returns the key's user.
func Save(ctx context.Context, cfgPath, name, server, secret string, headers []immich.Header) (immich.User, error) {
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
	if err := immich.ValidateHeaders(headers); err != nil {
		return immich.User{}, err
	}
	c := Client(server, headers)
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
	undoKey := func() {
		if prevErr == nil {
			_ = keyring.Set(KeyringService, kn, prev)
		} else {
			_ = keyring.Delete(KeyringService, kn)
		}
	}
	undoHeaders, err := storeHeaders(cfgPath, name, server, headers)
	if err != nil {
		undoKey()
		return immich.User{}, err
	}
	cfg.SetAccount(config.Account{Name: name, Server: server, UserID: u.ID})
	if err := cfg.Save(cfgPath); err != nil {
		undoHeaders()
		undoKey()
		return immich.User{}, err
	}
	return u, nil
}

// HasKey reports whether the keychain holds a key for the account.
func HasKey(cfgPath string, a config.Account) bool {
	_, err := LoadKey(cfgPath, a)
	return err == nil
}

// DeleteKey removes an account's key and custom headers from the keychain
// (also an early build's key under the bare name). A missing entry is not
// an error.
func DeleteKey(cfgPath string, a config.Account) error {
	for _, kn := range []string{KeyName(cfgPath, a.Name, a.Server), headersName(cfgPath, a.Name, a.Server)} {
		if err := keyring.Delete(KeyringService, kn); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return err
		}
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
