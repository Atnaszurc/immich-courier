package immich

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Header is a custom HTTP header sent with every request to a server, for
// a reverse proxy in front of Immich that wants its own credentials (basic
// auth, an access-gateway token). Values are secrets: they live in the OS
// keychain next to the API key and are never logged or shown again.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Limits keep the whole set inside one keychain entry (Windows Credential
// Manager caps an entry at 2560 bytes).
const (
	MaxHeaders     = 10
	MaxHeaderBytes = 2000 // all names and values together
)

// reservedHeaders are set by the app or by net/http itself. Letting a custom
// header replace them would break requests (framing, decoding) or swap in
// different Immich credentials than the account's own key.
var reservedHeaders = map[string]bool{
	"accept": true, "accept-encoding": true, "connection": true,
	"content-length": true, "content-type": true, "expect": true,
	"host": true, "keep-alive": true, "proxy-connection": true, "te": true,
	"trailer": true, "transfer-encoding": true, "upgrade": true,
	"x-api-key": true, "x-immich-user-token": true, "x-immich-session-token": true,
	"x-immich-share-key": true, "x-immich-checksum": true,
}

// ValidateHeaders checks names (HTTP tokens, no duplicates, nothing the app
// sets itself) and values (printable ASCII, no line breaks). Values must
// not be empty. It trims spaces around names and values in place.
func ValidateHeaders(hs []Header) error {
	if len(hs) > MaxHeaders {
		return fmt.Errorf("at most %d custom headers", MaxHeaders)
	}
	seen, size := map[string]bool{}, 0
	for i := range hs {
		h := &hs[i]
		h.Name, h.Value = strings.TrimSpace(h.Name), strings.TrimSpace(h.Value)
		if h.Name == "" {
			return errors.New("a custom header has no name")
		}
		if len(h.Name) > 64 || strings.IndexFunc(h.Name, func(r rune) bool { return !isTokenChar(r) }) >= 0 {
			return fmt.Errorf("%q isn't a valid header name (letters, digits and - only, for example X-Proxy-Token)", printable(h.Name))
		}
		lower := strings.ToLower(h.Name)
		if reservedHeaders[lower] {
			return fmt.Errorf("the %s header is set by the app and can't be overridden", h.Name)
		}
		if seen[lower] {
			return fmt.Errorf("the %s header is listed twice", h.Name)
		}
		seen[lower] = true
		if h.Value == "" {
			return fmt.Errorf("enter a value for the %s header", h.Name)
		}
		// Printable ASCII only: no CR/LF (header injection), no other
		// control characters, nothing proxies might re-encode.
		if strings.IndexFunc(h.Value, func(r rune) bool { return r != '\t' && (r < ' ' || r > '~') }) >= 0 {
			return fmt.Errorf("the value of the %s header has characters that can't be sent in a header", h.Name)
		}
		size += len(h.Name) + len(h.Value)
	}
	if size > MaxHeaderBytes {
		return fmt.Errorf("the custom headers are too long (%d bytes, at most %d)", size, MaxHeaderBytes)
	}
	return nil
}

// HeaderNames returns just the names, safe to show or log.
func HeaderNames(hs []Header) []string {
	names := make([]string, len(hs))
	for i, h := range hs {
		names[i] = h.Name
	}
	return names
}

func isTokenChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", r)
}

func printable(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 64 {
		s = s[:64] + "…"
	}
	return s
}

// authorize sets the custom headers, then the account's Immich credentials.
// Custom headers go first so nothing can replace the app's own (reserved
// names are refused by ValidateHeaders anyway). A session token normally
// travels as "Authorization: Bearer"; if a custom header already uses
// Authorization (proxy basic auth), Immich's own x-immich-user-token header
// carries it instead.
func (c *Client) authorize(req *http.Request) {
	customAuth := false
	for _, h := range c.Headers {
		req.Header.Set(h.Name, h.Value)
		if strings.EqualFold(h.Name, "Authorization") {
			customAuth = true
		}
	}
	switch {
	case c.APIKey != "":
		req.Header.Set("x-api-key", c.APIKey)
	case c.AccessToken != "" && customAuth:
		req.Header.Set("x-immich-user-token", c.AccessToken)
	case c.AccessToken != "":
		req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	}
}
