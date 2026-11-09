package main

import (
	"fmt"
	htmlpkg "html"
	urlpkg "net/url"
	"regexp"
	"strings"
)

// platforms is the single source of truth for what the UI advertises: it feeds
// both the footer line and the "unsupported URL" error. Keep in sync with
// allowedRe. Featured entries are the ones named in the footer; the rest are
// summarised as a count and listed in its tooltip.
var platforms = []struct {
	Name     string
	Featured bool
}{
	{"YouTube", true},
	{"Twitter / X", true},
	{"Instagram", true},
	{"Facebook", false},
	{"TikTok", true},
	{"Vimeo", false},
	{"Dailymotion", false},
	{"Reddit", false},
	{"Twitch", false},
	{"SoundCloud", false},
	{"Bilibili", false},
	{"NicoVideo", false},
	{"Threads", false},
	{"Pornhub", false},
}

// videoHosts are the domains handed to yt-dlp. allowedRe is built from this
// list and the UI uses it to label a pasted link before it is fetched.
var videoHosts = []string{
	"youtube.com", "youtu.be", "twitter.com", "x.com", "instagram.com", "facebook.com", "fb.watch",
	"tiktok.com", "vimeo.com", "dailymotion.com", "reddit.com", "twitch.tv", "soundcloud.com",
	"bilibili.com", "nicovideo.jp", "threads.net", "pornhub.com",
}

func videoHostPattern() string {
	parts := make([]string, len(videoHosts))
	for i, h := range videoHosts {
		parts[i] = regexp.QuoteMeta(h)
	}
	return strings.Join(parts, "|")
}

func platformNames() string {
	names := make([]string, 0, len(platforms))
	for _, p := range platforms {
		names = append(names, p.Name)
	}
	return strings.Join(names, ", ")
}

// platformSummaryHTML renders the footer line. A wall of badges answered the
// "what can I paste?" question far too loudly for something read once; the full
// list still shows up in the error when a URL is actually rejected.
func platformSummaryHTML() string {
	var featured []string
	for _, p := range platforms {
		if p.Featured {
			featured = append(featured, p.Name)
		}
	}
	line := "Works with " + strings.Join(featured, ", ")
	if rest := len(platforms) - len(featured); rest > 0 {
		line += fmt.Sprintf(" \u00b7 and %d more", rest)
	}
	return `<span title="` + htmlpkg.EscapeString(platformNames()) + `">` +
		htmlpkg.EscapeString(line) + `</span>`
}

// vimeoPathRe pulls the numeric video ID (and an optional unlisted hash) out of
// a vimeo.com page path, including channel/group forms like
// /channels/staffpicks/799630185.
var vimeoPathRe = regexp.MustCompile(`^/(?:[\w-]+/)*?(\d+)(?:/([0-9a-fA-F]+))?/?$`)

// vimeoPlayerURL rewrites a vimeo.com page link to its embed-player equivalent.
// Vimeo's web API refuses anonymous requests ("the web client only works when
// logged-in") while the player endpoint still serves public and unlisted videos
// without credentials, so this is the fallback when a plain link fails.
func vimeoPlayerURL(raw string) (string, bool) {
	u, err := urlpkg.Parse(raw)
	if err != nil {
		return "", false
	}
	switch strings.ToLower(u.Hostname()) {
	case "vimeo.com", "www.vimeo.com":
	default:
		return "", false
	}

	m := vimeoPathRe.FindStringSubmatch(u.Path)
	if m == nil {
		return "", false
	}

	out := &urlpkg.URL{Scheme: "https", Host: "player.vimeo.com", Path: "/video/" + m[1]}
	hash := m[2]
	if hash == "" {
		hash = u.Query().Get("h")
	}
	if hash != "" {
		out.RawQuery = urlpkg.Values{"h": {hash}}.Encode()
	}
	return out.String(), true
}
