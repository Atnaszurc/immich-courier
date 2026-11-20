// Package auth turns any login method Immich supports into a long-lived,
// narrowly scoped API key. Whatever the method, the flow ends the same way:
// create an API key, log the temporary session out, return the key. Passwords
// and session tokens are never kept.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode"

	"immich-courier/internal/immich"
)

// Permissions the app's API key gets. asset.upload covers uploading and the
// bulk-upload-check; user.read lets the UI show who a folder uploads as.
var Permissions = []string{"asset.upload", "user.read"}

// DefaultCallbackPort is fixed so identity providers with exact-match
// redirect URIs can be configured once: http://127.0.0.1:47823/callback
const DefaultCallbackPort = 47823

// Session is a temporary Immich session obtained by logging in.
type Session struct {
	Token string
	Email string
	Name  string
}

// PasswordLogin logs in with Immich's built-in email/password login.
func PasswordLogin(ctx context.Context, c *immich.Client, email, password string) (Session, error) {
	r, err := c.PasswordLogin(ctx, email, password)
	if err != nil {
		return Session{}, err
	}
	return Session{Token: r.AccessToken, Email: r.UserEmail, Name: r.Name}, nil
}

// OIDCOptions controls the browser-based login.
type OIDCOptions struct {
	Port        int                    // loopback port; 0 means DefaultCallbackPort
	OpenBrowser func(url string) error // nil means the OS default browser
	Timeout     time.Duration          // 0 means 5 minutes
	OnURL       func(url string)       // called with the login URL (e.g. to print it)
	// ReturnURL, if set, is where the browser goes after the provider
	// redirects back (e.g. the settings page). Must be an http(s) URL.
	ReturnURL string
}

// OIDCLogin runs Immich's OAuth login through the system browser with a
// loopback redirect (RFC 8252). The redirect URI must be registered with the
// identity provider behind Immich.
func OIDCLogin(ctx context.Context, c *immich.Client, opt OIDCOptions) (Session, error) {
	if opt.Port == 0 {
		opt.Port = DefaultCallbackPort
	}
	if opt.Timeout == 0 {
		opt.Timeout = 5 * time.Minute
	}
	if opt.OpenBrowser == nil {
		opt.OpenBrowser = OpenBrowser
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", opt.Port)

	state := randomString(32)
	verifier := randomString(48)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", opt.Port))
	if err != nil {
		return Session{}, fmt.Errorf("listen on %s (is another login running?): %w", redirectURI, err)
	}

	type result struct {
		url string
		err error
	}
	done := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			// Not the answer to this login (anything on this machine can
			// reach the port): ignore it and keep waiting.
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		var res result
		switch {
		case q.Get("error") != "":
			// Only the error code: the description is free text chosen by
			// whoever sent the browser here.
			res.err = fmt.Errorf("the identity provider refused the login (%s)", printable(q.Get("error"), 64))
		case q.Get("code") == "":
			res.err = errors.New("no code in callback")
		default:
			res.url = redirectURI + "?" + r.URL.RawQuery
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		switch {
		case opt.ReturnURL != "":
			// The settings page shows the outcome; go straight back to it.
			http.Redirect(w, r, opt.ReturnURL, http.StatusSeeOther)
		case res.err != nil:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, page("Login failed", "Go back to immich-courier for details."))
		default:
			fmt.Fprint(w, page("Logged in", "You can close this window and return to immich-courier."))
		}
		select {
		case done <- res:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	authURL, err := c.OAuthAuthorize(ctx, redirectURI, state, challenge)
	if err != nil {
		return Session{}, fmt.Errorf("start OAuth login: %w", err)
	}
	if err := checkWebURL(authURL); err != nil {
		return Session{}, fmt.Errorf("the server's login address: %w", err)
	}
	if opt.OnURL != nil {
		opt.OnURL(authURL)
	}
	if err := opt.OpenBrowser(authURL); err != nil && opt.OnURL == nil {
		return Session{}, fmt.Errorf("open browser: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()
	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		return Session{}, fmt.Errorf("waiting for browser login: %w", ctx.Err())
	}
	if res.err != nil {
		return Session{}, res.err
	}

	r, err := c.OAuthCallback(ctx, res.url, state, verifier)
	if err != nil {
		return Session{}, fmt.Errorf("finish OAuth login: %w", err)
	}
	return Session{Token: r.AccessToken, Email: r.UserEmail, Name: r.Name}, nil
}

// MintAPIKey uses a session to create the app's API key, then logs the
// session out. The session is logged out even if key creation fails.
func MintAPIKey(ctx context.Context, baseURL string, s Session, keyName string) (immich.APIKey, error) {
	c := immich.New(baseURL)
	c.AccessToken = s.Token
	key, err := c.CreateAPIKey(ctx, keyName, Permissions)
	logoutErr := c.Logout(ctx)
	if err != nil {
		return immich.APIKey{}, fmt.Errorf("create API key: %w", err)
	}
	if logoutErr != nil {
		// The key works; a session left behind expires on its own.
		return key, fmt.Errorf("API key created, but logging out the temporary session failed: %w", logoutErr)
	}
	return key, nil
}

// checkWebURL accepts only absolute http(s) URLs. The OS openers below run
// whatever handler a URL names (file:, ms-settings:, a local program path),
// and some URLs come from the server, so nothing else is passed to them.
func checkWebURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q isn't an http(s) address", printable(s, 200))
	}
	return nil
}

// printable truncates s and drops control characters, for error messages.
func printable(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// OpenBrowser opens an http(s) URL in the user's default browser.
func OpenBrowser(url string) error {
	if err := checkWebURL(url); err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func page(title, msg string) string {
	return `<!doctype html><meta charset="utf-8"><title>immich-courier</title>` +
		`<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem">` +
		`<h1>` + title + `</h1><p>` + msg + `</p></body>`
}
