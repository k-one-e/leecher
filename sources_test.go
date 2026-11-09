package main

import "testing"

func TestVimeoPlayerURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"https://vimeo.com/799630185", "https://player.vimeo.com/video/799630185", true},
		{"https://www.vimeo.com/799630185/", "https://player.vimeo.com/video/799630185", true},
		{"https://vimeo.com/799630185?share=copy", "https://player.vimeo.com/video/799630185", true},
		{"https://vimeo.com/channels/staffpicks/799630185", "https://player.vimeo.com/video/799630185", true},
		// Unlisted videos carry a hash, as a path segment or as ?h=.
		{"https://vimeo.com/799630185/a1b2c3d4e5", "https://player.vimeo.com/video/799630185?h=a1b2c3d4e5", true},
		{"https://vimeo.com/799630185?h=a1b2c3d4e5", "https://player.vimeo.com/video/799630185?h=a1b2c3d4e5", true},
		// Not rewritable.
		{"https://player.vimeo.com/video/799630185", "", false},
		{"https://vimeo.com/user12345/videos", "", false},
		{"https://youtu.be/799630185", "", false},
		{"https://vimeo.com/", "", false},
		{"not a url at all", "", false},
	}
	for _, c := range cases {
		got, ok := vimeoPlayerURL(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("vimeoPlayerURL(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// The rewritten URL must still pass the source allow-list, otherwise the
// fallback would be rejected before yt-dlp ever runs.
func TestVimeoPlayerURLPassesValidation(t *testing.T) {
	out, ok := vimeoPlayerURL("https://vimeo.com/799630185")
	if !ok {
		t.Fatal("expected a rewrite")
	}
	if err := validateSourceURL(out); err != nil {
		t.Errorf("rewritten URL rejected by validateSourceURL: %v", err)
	}
}
