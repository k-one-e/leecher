package main

import (
	"strings"
	"testing"
)

func argsString(args []string) string { return strings.Join(args, " ") }

func hasFlagValue(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func TestBuildYtdlpArgs_ProbeOmitsDownloadFlags(t *testing.T) {
	args := buildYtdlpArgs(ytdlpRequest{Mode: modeProbe, URL: "https://youtu.be/x"})

	for _, flag := range []string{"--dump-json", "--no-download", "--no-playlist"} {
		if !hasFlag(args, flag) {
			t.Errorf("probe args missing %s: %s", flag, argsString(args))
		}
	}
	if hasFlag(args, "--merge-output-format") {
		t.Errorf("probe must not request a merge container: %s", argsString(args))
	}
	if args[len(args)-1] != "https://youtu.be/x" {
		t.Errorf("URL must be the final argument: %s", argsString(args))
	}
}

func TestBuildYtdlpArgs_ProbeCanSkipPlayerClients(t *testing.T) {
	with := buildYtdlpArgs(ytdlpRequest{Mode: modeProbe, URL: "u"})
	without := buildYtdlpArgs(ytdlpRequest{Mode: modeProbe, URL: "u", SkipPlayerClients: true})

	if !hasFlag(with, "--extractor-args") {
		t.Errorf("primary probe should pass player clients: %s", argsString(with))
	}
	if hasFlag(without, "--extractor-args") {
		t.Errorf("fallback probe should drop player clients: %s", argsString(without))
	}
}

// The MKV regression: a video download must always target the mp4 container,
// and must only bias the codec sort when compatibility is requested.
func TestBuildYtdlpArgs_DownloadAlwaysTargetsMP4(t *testing.T) {
	specific := buildYtdlpArgs(ytdlpRequest{
		Mode:           modeDownload,
		URL:            "u",
		FormatID:       "137+bestaudio[ext=m4a]/137",
		OutputTemplate: "/tmp/%(title)s.%(ext)s",
	})
	if !hasFlagValue(specific, "--merge-output-format", "mp4") {
		t.Errorf("expected mp4 container, got: %s", argsString(specific))
	}
	if hasFlag(specific, "-S") {
		t.Errorf("a specific user pick must not be re-sorted by codec: %s", argsString(specific))
	}
	if !hasFlagValue(specific, "-f", "137+bestaudio[ext=m4a]/137") {
		t.Errorf("format selector not passed through: %s", argsString(specific))
	}

	compat := buildYtdlpArgs(ytdlpRequest{
		Mode:           modeDownload,
		URL:            "u",
		FormatID:       "bv*+ba/b",
		OutputTemplate: "/tmp/%(title)s.%(ext)s",
		PreferH264:     true,
	})
	if !hasFlagValue(compat, "-S", "vcodec:h264") {
		t.Errorf("compat mode should prefer h264: %s", argsString(compat))
	}
	if !hasFlagValue(compat, "--merge-output-format", "mp4") {
		t.Errorf("expected mp4 container, got: %s", argsString(compat))
	}
}

func TestBuildYtdlpArgs_AudioOnlyExtractsMP3(t *testing.T) {
	args := buildYtdlpArgs(ytdlpRequest{
		Mode:           modeDownload,
		URL:            "u",
		FormatID:       "ba",
		OutputTemplate: "/tmp/out",
		AudioOnly:      true,
		PreferH264:     true, // must be ignored for audio
	})
	if !hasFlagValue(args, "--audio-format", "mp3") || !hasFlag(args, "--extract-audio") {
		t.Errorf("expected mp3 extraction: %s", argsString(args))
	}
	if hasFlag(args, "--merge-output-format") || hasFlag(args, "-S") {
		t.Errorf("audio-only must not carry video flags: %s", argsString(args))
	}
}

func TestBuildYtdlpArgs_PlaylistIsCapped(t *testing.T) {
	args := buildYtdlpArgs(ytdlpRequest{
		Mode:           modePlaylist,
		URL:            "u",
		OutputTemplate: "/tmp/out",
		PlaylistMax:    7,
	})
	if !hasFlagValue(args, "--playlist-end", "7") {
		t.Errorf("expected playlist cap: %s", argsString(args))
	}
	if !hasFlag(args, "--yes-playlist") || !hasFlag(args, "--ignore-errors") {
		t.Errorf("playlist flags missing: %s", argsString(args))
	}
	if !hasFlagValue(args, "--audio-format", "mp3") {
		t.Errorf("playlist should default to mp3: %s", argsString(args))
	}

	def := buildYtdlpArgs(ytdlpRequest{Mode: modePlaylist, URL: "u"})
	if !hasFlagValue(def, "--playlist-end", "100") {
		t.Errorf("expected default playlist cap of %d: %s", playlistMaxEntries, argsString(def))
	}

	slice := buildYtdlpArgs(ytdlpRequest{Mode: modePlaylist, URL: "u", PlaylistItems: "4-6"})
	if !hasFlagValue(slice, "--playlist-items", "4-6") || hasFlag(slice, "--playlist-end") {
		t.Errorf("a worker slice replaces the cap: %s", argsString(slice))
	}
	if !hasFlag(buildYtdlpArgs(ytdlpRequest{Mode: modeProbe, URL: "u"}), "--flat-playlist") {
		t.Error("probe must list playlists flat")
	}
}

func TestPlaylistRanges(t *testing.T) {
	cases := []struct {
		n, w int
		want string
	}{
		{9, 3, "1-3 4-6 7-9"},
		{10, 3, "1-4 5-7 8-10"},
		{2, 3, "1-1 2-2"},
		{1, 3, "1-1"},
		{0, 3, ""},
		{250, 3, "1-34 35-67 68-100"},
	}
	for _, c := range cases {
		if got := strings.Join(playlistRanges(c.n, c.w), " "); got != c.want {
			t.Errorf("playlistRanges(%d, %d) = %q, want %q", c.n, c.w, got, c.want)
		}
	}
}

func TestBuildYtdlpArgs_AudioIsTagged(t *testing.T) {
	playlist := buildYtdlpArgs(ytdlpRequest{Mode: modePlaylist, URL: "u"})
	single := buildYtdlpArgs(ytdlpRequest{Mode: modeDownload, URL: "u", FormatID: "b", AudioOnly: true})
	video := buildYtdlpArgs(ytdlpRequest{Mode: modeDownload, URL: "u", FormatID: "b"})

	for name, args := range map[string][]string{"playlist": playlist, "single": single} {
		if !hasFlag(args, "--embed-metadata") || !hasFlag(args, "--embed-thumbnail") {
			t.Errorf("%s audio must embed tags and cover: %s", name, argsString(args))
		}
		if !hasFlagValue(args, "--parse-metadata", "%(release_date>%Y,release_year|)s:%(meta_date)s") {
			t.Errorf("%s audio must date by release year: %s", name, argsString(args))
		}
	}
	if !hasFlagValue(playlist, "--parse-metadata", "%(playlist_index|)s:%(track_number)s") {
		t.Errorf("playlist must number tracks by position: %s", argsString(playlist))
	}
	if hasFlagValue(single, "--parse-metadata", "%(playlist_index|)s:%(track_number)s") {
		t.Errorf("single track has no album position: %s", argsString(single))
	}
	if hasFlag(video, "--embed-thumbnail") {
		t.Errorf("video downloads are left untouched: %s", argsString(video))
	}
}

func TestBuildYtdlpArgs_MaxFileSize(t *testing.T) {
	args := buildYtdlpArgs(ytdlpRequest{Mode: modeDownload, URL: "u", FormatID: "b", MaxFileSizeMB: 500})
	if !hasFlagValue(args, "--max-filesize", "500M") {
		t.Errorf("expected size cap: %s", argsString(args))
	}

	none := buildYtdlpArgs(ytdlpRequest{Mode: modeDownload, URL: "u", FormatID: "b"})
	if hasFlag(none, "--max-filesize") {
		t.Errorf("no cap expected when MaxFileSizeMB is 0: %s", argsString(none))
	}
}

func TestBuildYtdlpArgs_CookiesArePassedToEveryMode(t *testing.T) {
	withConfig(t, func(c *Config) { c.CookiesFile = "/tmp/cookies.txt" })

	for _, mode := range []ytdlpMode{modeProbe, modeDownload, modePlaylist} {
		args := buildYtdlpArgs(ytdlpRequest{Mode: mode, URL: "u", FormatID: "b", OutputTemplate: "/tmp/o"})
		if !hasFlagValue(args, "--cookies", "/tmp/cookies.txt") {
			t.Errorf("mode %d dropped cookies: %s", mode, argsString(args))
		}
	}
}

func TestParseProgressPercent(t *testing.T) {
	cases := []struct {
		line string
		want float64
		ok   bool
	}{
		{"LEECHER_PROGRESS  42.1% downloading", 42.1, true},
		{"LEECHER_PROGRESS 100.0% finished", 100, true},
		{"LEECHER_PROGRESS N/A% downloading", 0, false},
		// Legacy human output must still drive the bar on older yt-dlp.
		{"[download]  42.1% of 96.11MiB at 5.00MiB/s ETA 00:11", 42.1, true},
		{"[download] Destination: /tmp/video.mp4", 0, false},
		{"[Merger] Merging formats into \"/tmp/video.mp4\"", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := parseProgressPercent(c.line)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseProgressPercent(%q) = (%v, %v), want (%v, %v)", c.line, got, ok, c.want, c.ok)
		}
	}
}

func TestBuildYtdlpArgs_RequestsMachineReadableProgress(t *testing.T) {
	for _, mode := range []ytdlpMode{modeDownload, modePlaylist} {
		args := buildYtdlpArgs(ytdlpRequest{Mode: mode, URL: "u", FormatID: "b", OutputTemplate: "/tmp/o"})
		found := false
		for i, a := range args {
			if a == "--progress-template" && i+1 < len(args) && strings.Contains(args[i+1], progressPrefix) {
				found = true
			}
		}
		if !found {
			t.Errorf("mode %d should request the machine-readable progress template: %s", mode, argsString(args))
		}
	}
	probe := buildYtdlpArgs(ytdlpRequest{Mode: modeProbe, URL: "u"})
	if hasFlag(probe, "--progress-template") {
		t.Errorf("probe does not download, so it needs no progress template: %s", argsString(probe))
	}
}
