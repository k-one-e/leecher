package main

import (
	htmlpkg "html"
	"strings"
)

// appVersion holds the raw `git describe` output, injected at build time via
// -ldflags "-X main.appVersion=...".
var appVersion = "dev"

// displayVersion strips git's build metadata so the UI shows a release number
// rather than "v1.1.0-450-g9aed054-dirty". Anything that is not exactly a clean
// tagged commit is marked -dev, because it isn't a release.
func displayVersion() string {
	v := strings.TrimSpace(appVersion)
	if v == "" {
		return "dev"
	}

	dev := strings.HasSuffix(v, "-dirty")
	v = strings.TrimSuffix(v, "-dirty")

	// git describe appends "-<commits>-g<sha>" once the tag is behind HEAD.
	if i := strings.LastIndex(v, "-g"); i > 0 {
		if j := strings.LastIndex(v[:i], "-"); j > 0 {
			v = v[:j]
			dev = true
		}
	}
	if dev {
		v += "-dev"
	}
	return v
}

// versionHTML renders the footer version, keeping the exact build string in the
// tooltip so a bug report can still identify the commit.
func versionHTML() string {
	return `<span title="` + htmlpkg.EscapeString(appVersion) + `">` +
		htmlpkg.EscapeString(displayVersion()) + `</span>`
}
