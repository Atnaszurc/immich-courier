package tray

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"

	"immich-courier/internal/daemon"
	"immich-courier/internal/engine"
)

func TestSummarize(t *testing.T) {
	acct := func(name string, mod func(*engine.AccountStatus)) engine.AccountStatus {
		a := engine.AccountStatus{Name: name, Online: true}
		if mod != nil {
			mod(&a)
		}
		return a
	}
	cases := []struct {
		name string
		s    daemon.Status
		icon iconState
		text string
	}{
		{"idle", daemon.Status{Accounts: []engine.AccountStatus{acct("a", nil)}}, iconIdle, "Up to date"},
		{"no folders", daemon.Status{Problem: "no folders to watch yet"}, iconProblem, "Not uploading: no folders to watch yet"},
		{"uploading", daemon.Status{Accounts: []engine.AccountStatus{acct("a", func(a *engine.AccountStatus) {
			a.Uploading, a.Queued = "/x/IMG_1.jpg", 3
		})}}, iconBusy, "Uploading IMG_1.jpg (3 left)"},
		{"offline", daemon.Status{Accounts: []engine.AccountStatus{acct("a", func(a *engine.AccountStatus) {
			a.Online, a.Queued = false, 5
		})}}, iconOffline, "Server unreachable, 5 waiting"},
		{"one of two offline, idle", daemon.Status{Accounts: []engine.AccountStatus{
			acct("a", nil), acct("b", func(a *engine.AccountStatus) { a.Online = false }),
		}}, iconOffline, "Some servers unreachable"},
		{"paused beats offline", daemon.Status{Paused: true, Accounts: []engine.AccountStatus{acct("a", func(a *engine.AccountStatus) {
			a.Online, a.Queued = false, 2
		})}}, iconPaused, "Paused, 2 waiting"},
		{"auth failed beats everything but problem", daemon.Status{Paused: true, Accounts: []engine.AccountStatus{acct("a", func(a *engine.AccountStatus) {
			a.AuthFailed = true
		})}}, iconProblem, "Log in again: a"},
	}
	for _, c := range cases {
		got := summarize(c.s)
		if got.icon != c.icon || got.text != c.text {
			t.Errorf("%s: got (%d, %q), want (%d, %q)", c.name, got.icon, got.text, c.icon, c.text)
		}
	}
}

func TestIconsEncode(t *testing.T) {
	for s := iconIdle; s <= iconProblem; s++ {
		if _, err := png.Decode(bytes.NewReader(pngBytes(render(s, 32)))); err != nil {
			t.Fatalf("state %d: %v", s, err)
		}
		b := ico(s, 16, 32)
		var hdr [3]uint16
		binary.Read(bytes.NewReader(b), binary.LittleEndian, &hdr)
		if hdr != [3]uint16{0, 1, 2} {
			t.Fatalf("bad ico header %v", hdr)
		}
	}
}
