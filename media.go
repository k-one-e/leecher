package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Codecs an mp4 container can carry with a stream copy and that mainstream
// players actually decode.
var mp4VideoCodecs = map[string]bool{"h264": true, "hevc": true, "av1": true, "vp9": true, "mpeg4": true}
var mp4AudioCodecs = map[string]bool{"aac": true, "mp3": true, "ac3": true, "eac3": true, "alac": true}

// codecLabel maps an ffmpeg/yt-dlp codec string to a short display name.
func codecLabel(vcodec string) string {
	c := strings.ToLower(vcodec)
	switch {
	case strings.HasPrefix(c, "avc") || strings.HasPrefix(c, "h264"):
		return "H.264"
	case strings.HasPrefix(c, "av01") || strings.HasPrefix(c, "av1"):
		return "AV1"
	case strings.HasPrefix(c, "vp9") || strings.HasPrefix(c, "vp09"):
		return "VP9"
	case strings.HasPrefix(c, "vp8"):
		return "VP8"
	case strings.HasPrefix(c, "hev") || strings.HasPrefix(c, "hvc") || strings.HasPrefix(c, "h265"):
		return "H.265"
	}
	return ""
}

// probeCodec returns the codec name of the first stream of the given type
// ("v" or "a"), or "" if there is no such stream.
func probeCodec(ctx context.Context, ffprobe, src, streamType string) (string, error) {
	out, err := exec.CommandContext(ctx, ffprobe,
		"-v", "error",
		"-select_streams", streamType+":0",
		"-show_entries", "stream=codec_name",
		"-of", "default=noprint_wrappers=1:nokey=1",
		src,
	).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// readTrackTags returns the artist/album/date tags of each track. Tracks that
// cannot be probed are skipped; no ffprobe means no tags.
func readTrackTags(ctx context.Context, dir string, tracks []os.DirEntry) []map[string]string {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return nil
	}
	var all []map[string]string
	for _, t := range tracks {
		out, err := exec.CommandContext(ctx, ffprobe,
			"-v", "error",
			"-show_entries", "format_tags=artist,album,date",
			"-of", "default=noprint_wrappers=1",
			filepath.Join(dir, t.Name()),
		).Output()
		if err != nil {
			continue
		}
		tags := map[string]string{}
		for _, line := range strings.Split(string(out), "\n") {
			k, v, ok := strings.Cut(strings.TrimPrefix(line, "TAG:"), "=")
			if ok {
				tags[strings.ToLower(k)] = strings.TrimSpace(v)
			}
		}
		all = append(all, tags)
	}
	return all
}

// albumArchiveName names an album download "Artist - Year - Album". Tracks
// from different albums mean a playlist, which keeps its own title.
func albumArchiveName(tracks []map[string]string, playlistTitle string) string {
	var album, year string
	artists := map[string]bool{}
	for _, t := range tracks {
		if t["album"] == "" {
			continue
		}
		if album == "" {
			album = t["album"]
		} else if t["album"] != album {
			return playlistTitle
		}
		if year == "" && len(t["date"]) >= 4 {
			year = t["date"][:4]
		}
		if t["artist"] != "" {
			artists[t["artist"]] = true
		}
	}
	if album == "" {
		return playlistTitle
	}
	var parts []string
	switch len(artists) {
	case 0:
	case 1:
		for a := range artists {
			parts = append(parts, a)
		}
	default:
		parts = append(parts, "Various Artists")
	}
	if year != "" {
		parts = append(parts, year)
	}
	return strings.Join(append(parts, album), " - ")
}

// mp4Plan describes what ffmpeg has to do to land a file in an mp4 container.
type mp4Plan struct {
	EncodeVideo bool
	EncodeAudio bool
	Skip        bool // already a compliant .mp4
}

// planMP4 decides the cheapest route to an mp4: no work, a stream copy, an
// audio-only re-encode, or a full transcode.
func planMP4(ext, vcodec, acodec string, preferCompat bool) mp4Plan {
	plan := mp4Plan{
		EncodeVideo: !mp4VideoCodecs[vcodec] || (preferCompat && vcodec != "h264"),
		EncodeAudio: acodec != "" && !mp4AudioCodecs[acodec],
	}
	if strings.EqualFold(ext, ".mp4") && !plan.EncodeVideo && !plan.EncodeAudio {
		plan.Skip = true
	}
	return plan
}

// ensureMP4 normalises a downloaded video into an .mp4 container.
// Incompatible streams are re-encoded, everything else is stream-copied, so a
// file that already fits mp4 only pays for a remux. When preferCompat is set,
// non-H.264 video is transcoded for iPhone/QuickTime playback.
// Returns the (possibly new) file path.
func ensureMP4(ctx context.Context, job *Job, src string, preferCompat bool) string {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		logger.Warn("ffprobe not found, skipping container check", "job", job.ID)
		return src
	}

	vcodec, err := probeCodec(ctx, ffprobe, src, "v")
	if err != nil {
		logger.Warn("ffprobe failed, keeping original file", "job", job.ID, "error", err)
		return src
	}
	if vcodec == "" {
		return src // audio-only result, nothing to remux
	}
	acodec, err := probeCodec(ctx, ffprobe, src, "a")
	if err != nil {
		logger.Warn("ffprobe failed, keeping original file", "job", job.ID, "error", err)
		return src
	}

	plan := planMP4(filepath.Ext(src), vcodec, acodec, preferCompat)
	if plan.Skip {
		return src
	}

	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		logger.Warn("ffmpeg not found, keeping original file", "job", job.ID)
		return src
	}

	msg := "Converting to MP4..."
	if plan.EncodeVideo {
		msg = "Re-encoding to H.264..."
	}
	logger.Info("Normalising to MP4", "job", job.ID, "vcodec", vcodec, "acodec", acodec,
		"video_encode", plan.EncodeVideo, "audio_encode", plan.EncodeAudio)

	job.mu.Lock()
	job.Message = msg
	job.Progress = 99
	job.notifySubscribers()
	job.mu.Unlock()

	base := strings.TrimSuffix(src, filepath.Ext(src))
	tmp := base + ".converted.mp4"
	dst := base + ".mp4"

	args := []string{"-i", src, "-map", "0:v:0", "-map", "0:a?", "-sn", "-dn"}
	if plan.EncodeVideo {
		args = append(args, "-c:v", "libx264", "-preset", "fast", "-crf", "23")
	} else {
		args = append(args, "-c:v", "copy")
	}
	if plan.EncodeAudio {
		args = append(args, "-c:a", "aac", "-b:a", "192k")
	} else {
		args = append(args, "-c:a", "copy")
	}
	args = append(args, "-movflags", "+faststart", "-y", tmp)

	cmd := lowPriorityCmd(ctx, ffmpegPath, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		logger.Warn("MP4 conversion failed, keeping original", "job", job.ID, "error", err, "output", string(output))
		os.Remove(tmp)
		return src
	}

	os.Remove(src)
	if err := os.Rename(tmp, dst); err != nil {
		logger.Warn("Failed to rename converted file", "job", job.ID, "error", err)
		return tmp
	}
	logger.Info("MP4 conversion complete", "job", job.ID, "file", dst)
	return dst
}
