package immich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The API key must never follow a redirect to another host (or scheme).
func TestRedirectOffOriginIsRefused(t *testing.T) {
	var leaked atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("x-api-key"))
		w.Write([]byte(`{"id":"x"}`))
	}))
	defer other.Close()
	for _, code := range []int{301, 302, 307, 308} {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+r.URL.Path, code)
		}))
		c := New(origin.URL)
		c.APIKey = "secret-key"
		if _, err := c.Me(context.Background()); err == nil || !strings.Contains(err.Error(), "redirected") {
			t.Errorf("%d: err = %v, want a refused redirect", code, err)
		}
		_, err := c.Upload(context.Background(), strings.NewReader("data"), "a.jpg", "abc", time.Now())
		if err == nil {
			t.Errorf("%d: upload followed the redirect", code)
		}
		origin.Close()
	}
	if k, _ := leaked.Load().(string); k != "" {
		t.Fatalf("API key reached the other host: %q", k)
	}
}

// Redirects within the same server (e.g. a trailing-slash fix) still work.
func TestRedirectSameOriginIsFollowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/users/me" {
			http.Redirect(w, r, "/api/users/me/", http.StatusFound)
			return
		}
		w.Write([]byte(`{"id":"u1"}`))
	}))
	defer srv.Close()
	c := New(srv.URL)
	c.APIKey = "k"
	if u, err := c.Me(context.Background()); err != nil || u.ID != "u1" {
		t.Fatalf("same-origin redirect: %v %v", u, err)
	}
}
