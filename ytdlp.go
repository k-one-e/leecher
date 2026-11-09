package main

import (
	"fmt"
	"strconv"
	"strings"
)

// Every yt-dlp invocation in the service is built here. Keeping probe,
// download and playlist argv construction in one place is what stops the
// three call sites from drifting apart (a mismatch between them is how
// downloads ended up as .mkv while the UI promised .mp4).

// progressPrefix marks the machine-readable progress lines we ask yt-dlp to
// emit. Parsing those instead of the human "[download]  42.1% of ..." output
// means an upstream wording change cannot silently break the progress bar.
const progressPrefix = "LEECHER_PROGRESS"

// progressTemplateArgs renders one `LEECHER_PROGRESS <pct> <status>` line per update.
func progressTemplateArgs() []string {
	return []string{
		"--progress-template",
		"download:" + progressPrefix + " %(progress._percent_str)s %(progress.status)s",
	}
}

// audioTagArgs writes title/artist/album/year and a square cover into extracted audio.
func audioTagArgs() []string {
	return []string{
		"--embed-metadata",
		// Without this the date tag is the upload year, not the album's release year.
		// release_date ("Released on:") comes first: release_year is scraped from the
		// "℗ YEAR Label" line, which misreads a numeric label ("℗ 815455 Records") as 8154.
		"--parse-metadata", "%(release_date>%Y,release_year|)s:%(meta_date)s",
		"--embed-thumbnail",
		"--convert-thumbnails", "jpg",
		// YouTube covers are 16:9 frames with the art centred; keep the centre square.
		"--ppa", `ThumbnailsConvertor+ffmpeg_o:-c:v mjpeg -vf crop="'if(gt(ih,iw),iw,ih)':'if(gt(iw,ih),ih,iw)'"`,
	}
}

type ytdlpMode int

const (
	modeProbe ytdlpMode = iota
	modeDownload
	modePlaylist
)

type ytdlpRequest struct {
	Mode     ytdlpMode
	URL      string
	FormatID string
	// OutputTemplate is the -o value; required for download and playlist modes.
	OutputTemplate string
	// AudioOnly extracts audio to MP3 instead of keeping a video container.
	AudioOnly bool
	// PreferH264 biases the format sort toward H.264 for maximum device
	// compatibility. Ignored when AudioOnly is set.
	PreferH264 bool
	// AudioFormat overrides the playlist extraction format (default mp3).
	AudioFormat string
	// SkipPlayerClients drops the YouTube player-client override; used by the
	// probe fallback strategy.
	SkipPlayerClients bool
	// MaxFileSizeMB of 0 means unlimited.
	MaxFileSizeMB int
	// PlaylistMax caps the number of playlist entries fetched.
	PlaylistMax int
	// PlaylistItems ("4-6") selects a slice of the playlist; it replaces the cap.
	PlaylistItems string
}

// buildYtdlpArgs renders the full argv (excluding the binary itself).
func buildYtdlpArgs(req ytdlpRequest) []string {
	var args []string

	switch req.Mode {
	case modeProbe:
		args = []string{
			"--no-playlist",
			// Playlist entries are only counted, so skip resolving each one (32s -> 2s for 9 tracks).
			"--flat-playlist",
			"--dump-json",
			"--no-download",
			"--no-check-formats",
			"--no-warnings",
			"--quiet",
			"--socket-timeout", "15",
		}

	case modeDownload:
		args = []string{
			"--no-playlist",
			"--restrict-filenames",
			"--newline",
			"--progress",
			"--no-mtime",
			"--buffer-size", "16K",
			"--http-chunk-size", "10M",
			"-f", req.FormatID,
		}
		args = append(args, progressTemplateArgs()...)

	case modePlaylist:
		max := req.PlaylistMax
		if max < 1 {
			max = playlistMaxEntries
		}
		args = []string{
			"--yes-playlist",
			"--playlist-end", strconv.Itoa(max),
			// Keeps spaces, apostrophes and non-Latin titles; only strips what Windows rejects.
			"--windows-filenames",
			"--newline",
			"--progress",
			"--no-mtime",
			"--ignore-errors", // one bad track shouldn't kill the whole album
			"--buffer-size", "16K",
			"--http-chunk-size", "10M",
			"-f", "ba/b",
		}
		if req.PlaylistItems != "" {
			args[1], args[2] = "--playlist-items", req.PlaylistItems
		}
		args = append(args, progressTemplateArgs()...)
	}

	if req.MaxFileSizeMB > 0 {
		args = append(args, "--max-filesize", fmt.Sprintf("%dM", req.MaxFileSizeMB))
	}

	args = append(args, cacheArgs()...)
	args = append(args, cookieArgs()...)
	args = append(args, jsRuntimeArgs()...)
	if !req.SkipPlayerClients {
		args = append(args, youtubeClientArgs()...)
	}

	switch {
	case req.Mode == modePlaylist:
		format := req.AudioFormat
		if format == "" {
			format = "mp3"
		}
		args = append(args, "--extract-audio", "--audio-format", format)
		args = append(args, audioTagArgs()...)
		// Album position becomes the track number so players keep the album order.
		args = append(args, "--parse-metadata", "%(playlist_index|)s:%(track_number)s")

	case req.Mode == modeDownload && req.AudioOnly:
		args = append(args, "--extract-audio", "--audio-format", "mp3")
		args = append(args, audioTagArgs()...)

	case req.Mode == modeDownload:
		if req.PreferH264 {
			args = append(args, "-S", "vcodec:h264")
		}
		// yt-dlp falls back to mkv by itself when the picked codecs cannot be
		// muxed into mp4; ensureMP4 repairs those cases after the download.
		args = append(args, "--merge-output-format", "mp4")
	}

	if req.OutputTemplate != "" {
		args = append(args, "-o", req.OutputTemplate)
	}
	return append(args, req.URL)
}

// parseProgressPercent extracts the completion percentage from a yt-dlp output
// line. It understands both our progress template and the legacy human
// "[download]  42.1% of ..." format, so an older yt-dlp still drives the bar.
func parseProgressPercent(line string) (float64, bool) {
	if rest, ok := strings.CutPrefix(line, progressPrefix); ok {
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}
		pct, err := strconv.ParseFloat(strings.TrimSuffix(fields[0], "%"), 64)
		if err != nil {
			return 0, false
		}
		return pct, true
	}

	if !strings.Contains(line, "[download]") || !strings.Contains(line, "%") {
		return 0, false
	}
	m := progressRe.FindStringSubmatch(line)
	if len(m) < 2 {
		return 0, false
	}
	pct, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	return pct, true
}
