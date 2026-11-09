package main

import "testing"

func TestDisplayVersion(t *testing.T) {
	cases := []struct{ raw, want string }{
		// Clean release build straight off a tag.
		{"v1.1.0", "v1.1.0"},
		// Commits after the tag, or uncommitted work: not a release.
		{"v1.1.0-3-g9aed054", "v1.1.0-dev"},
		{"v1.1.0-450-g9aed054-dirty", "v1.1.0-dev"},
		{"v1.1.0-dirty", "v1.1.0-dev"},
		// Prerelease tags keep their suffix.
		{"v1.2.0-rc1", "v1.2.0-rc1"},
		{"v1.2.0-rc1-2-gabc1234", "v1.2.0-rc1-dev"},
		// Fallbacks.
		{"dev", "dev"},
		{"", "dev"},
		{"20260911", "20260911"},
	}
	for _, c := range cases {
		appVersion = c.raw
		if got := displayVersion(); got != c.want {
			t.Errorf("displayVersion(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
	appVersion = "dev"
}

func TestVersionHTMLKeepsFullBuildStringInTooltip(t *testing.T) {
	appVersion = "v1.1.0-450-g9aed054-dirty"
	defer func() { appVersion = "dev" }()

	got := versionHTML()
	want := `<span title="v1.1.0-450-g9aed054-dirty">v1.1.0-dev</span>`
	if got != want {
		t.Errorf("versionHTML() = %q, want %q", got, want)
	}
}
