package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestPlanMP4(t *testing.T) {
	cases := []struct {
		name         string
		ext, v, a    string
		compat       bool
		wantSkip     bool
		wantVideoEnc bool
		wantAudioEnc bool
	}{
		{"already compliant mp4", ".mp4", "h264", "aac", false, true, false, false},
		{"mkv h264+aac only needs a remux", ".mkv", "h264", "aac", false, false, false, false},
		// The regression that shipped .mkv files: h264 video, Opus audio.
		{"opus audio must be re-encoded", ".mkv", "h264", "opus", false, false, false, true},
		{"vp9 is copied when the user picked it", ".webm", "vp9", "aac", false, false, false, false},
		{"vp9 is transcoded in compat mode", ".webm", "vp9", "aac", true, false, true, false},
		{"exotic codec is always transcoded", ".ogv", "theora", "vorbis", false, false, true, true},
		{"mp4 with opus still needs audio work", ".mp4", "h264", "opus", false, false, false, true},
	}
	for _, c := range cases {
		got := planMP4(c.ext, c.v, c.a, c.compat)
		if got.Skip != c.wantSkip || got.EncodeVideo != c.wantVideoEnc || got.EncodeAudio != c.wantAudioEnc {
			t.Errorf("%s: planMP4(%q,%q,%q,compat=%v) = %+v", c.name, c.ext, c.v, c.a, c.compat, got)
		}
	}
}

func TestCodecLabel(t *testing.T) {
	cases := map[string]string{
		"avc1.640028": "H.264",
		"av01.0.08M":  "AV1",
		"vp09.00.10":  "VP9",
		"hev1.1.6":    "H.265",
		"weird":       "",
	}
	for in, want := range cases {
		if got := codecLabel(in); got != want {
			t.Errorf("codecLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPickThumbnail(t *testing.T) {
	type cand = struct {
		URL        string `json:"url"`
		Width      int    `json:"width"`
		Height     int    `json:"height"`
		Preference int    `json:"preference"`
	}
	list := []cand{
		{URL: "small.jpg", Width: 120},
		{URL: "big.jpg", Width: 1280},
		{URL: "huge.jpg", Width: 4096}, // skipped: wasteful to proxy
	}

	if got := pickThumbnail("primary.jpg", list); got != "primary.jpg" {
		t.Errorf("yt-dlp's own pick should win, got %q", got)
	}
	if got := pickThumbnail("", list); got != "big.jpg" {
		t.Errorf("expected widest sane candidate, got %q", got)
	}
	if got := pickThumbnail("", nil); got != "" {
		t.Errorf("expected empty result for no candidates, got %q", got)
	}
	// Everything oversized: fall back rather than showing nothing.
	if got := pickThumbnail("", []cand{{URL: "only.jpg", Width: 5000}}); got != "only.jpg" {
		t.Errorf("expected fallback to last candidate, got %q", got)
	}
}

func TestThumbnailStore(t *testing.T) {
	id := rememberThumbnail("https://example.com/t.jpg")
	if id == "" {
		t.Fatal("expected an id")
	}
	if got, ok := lookupThumbnail(id); !ok || got != "https://example.com/t.jpg" {
		t.Errorf("lookup failed: %q %v", got, ok)
	}
	if _, ok := lookupThumbnail("nope"); ok {
		t.Error("unknown id must not resolve")
	}
	// Only the server may choose what gets fetched — no scheme, no entry.
	for _, bad := range []string{"file:///etc/passwd", "ftp://host/x", "", "//evil.com/x"} {
		if rememberThumbnail(bad) != "" {
			t.Errorf("scheme %q should have been rejected", bad)
		}
	}

	thumbMu.Lock()
	thumbStore[id] = thumbEntry{url: "https://example.com/t.jpg", expires: time.Now().Add(-time.Second)}
	thumbMu.Unlock()
	if _, ok := lookupThumbnail(id); ok {
		t.Error("expired entry must not resolve")
	}
	pruneThumbnails()
	thumbMu.Lock()
	_, present := thumbStore[id]
	thumbMu.Unlock()
	if present {
		t.Error("expired entry should have been pruned")
	}
}

func TestAlbumArchiveName(t *testing.T) {
	tr := func(artist, album, date string) map[string]string {
		return map[string]string{"artist": artist, "album": album, "date": date}
	}
	cases := []struct {
		name   string
		tracks []map[string]string
		want   string
	}{
		{"album", []map[string]string{tr("Faramarz Aslani", "Roozhaye Taraneho Andooh", "1999"), tr("Faramarz Aslani", "Roozhaye Taraneho Andooh", "1999")}, "Faramarz Aslani - 1999 - Roozhaye Taraneho Andooh"},
		{"full date", []map[string]string{tr("A", "B", "1999-04-01")}, "A - 1999 - B"},
		{"no year", []map[string]string{tr("A", "B", "")}, "A - B"},
		{"compilation", []map[string]string{tr("A", "Hits", "2001"), tr("C", "Hits", "2001")}, "Various Artists - 2001 - Hits"},
		{"playlist of albums", []map[string]string{tr("A", "X", "2001"), tr("A", "Y", "2003")}, "My Mix"},
		{"no tags", nil, "My Mix"},
	}
	for _, c := range cases {
		if got := albumArchiveName(c.tracks, "My Mix"); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSanitizeFilenameKeepsWholeRunes(t *testing.T) {
	got := sanitizeFilename(strings.Repeat("ف", 50)) // 100 bytes
	if !utf8.ValidString(got) || len(got) > 80 {
		t.Errorf("truncated badly: %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
}
