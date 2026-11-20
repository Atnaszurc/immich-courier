package main

import (
	"strings"
	"testing"
)

// File names and server messages must not reach the terminal raw.
func TestSafeEscapesControlCharacters(t *testing.T) {
	in := "a\x1b]0;pwned\x07b\nAccount \"x\"  [online]\r.jpg"
	out := safe(in)
	if strings.IndexFunc(out, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		t.Fatalf("control characters left in %q", out)
	}
	if safe(`C:\Users\me\Pictures\ok.jpg`) != `C:\Users\me\Pictures\ok.jpg` || safe("café ✓.jpg") != "café ✓.jpg" {
		t.Fatal("ordinary names must print unchanged")
	}
}
