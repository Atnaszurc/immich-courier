package accounts

import "testing"

func TestNormalizeServer(t *testing.T) {
	cases := map[string]string{
		"photos.example.com":                 "https://photos.example.com",
		" https://photos.example.com/ ":      "https://photos.example.com",
		"https://photos.example.com/api":     "https://photos.example.com",
		"http://192.168.1.5:2283/api/":       "http://192.168.1.5:2283",
		"https://example.com/immich/":        "https://example.com/immich",
		"https://example.com/immich?x=1#top": "https://example.com/immich",
	}
	for in, want := range cases {
		got, err := NormalizeServer(in)
		if err != nil || got != want {
			t.Errorf("NormalizeServer(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ftp://x", "https://"} {
		if _, err := NormalizeServer(bad); err == nil {
			t.Errorf("NormalizeServer(%q) accepted", bad)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"personal", "Priscila IG", "work-2", "a.b_c"} {
		if ValidName(ok) != nil {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", " lead", "../x", "a/b", "x\x00", "this-name-is-way-too-long-for-an-account-label"} {
		if ValidName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
