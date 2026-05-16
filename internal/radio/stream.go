package radio

import (
	"fmt"
	"github.com/kidixdev/lofi-radio/internal/utils"
	"net/url"
	"strings"
)

func GetDirectAudioURL(videoURL string) (string, error) {
	writeLog("stream.resolve.start video_url=%q", videoURL)

	// Prefer true audio-only formats first (YouTube live commonly exposes
	// m4a/opus audio tracks separately; this avoids video-capable HLS master
	// manifests that can break analyzer decoding and waste bandwidth).
	audioOnlySelectors := []string{
		"bestaudio[protocol!=m3u8]/bestaudio",
		"140/251/250/249/bestaudio",
		"bestaudio/best",
	}

	var (
		stdout string
		stderr string
		err    error
	)
	for _, sel := range audioOnlySelectors {
		stdout, stderr, err = runYtDlp(
			"--no-playlist",
			"--encoding", "utf-8",
			"-f", sel,
			"-g",
			videoURL,
		)
		if err == nil && strings.TrimSpace(stdout) != "" {
			writeLog("stream.resolve.selector_ok selector=%q", sel)
			break
		}
		writeLog("stream.resolve.selector_failed selector=%q err=%v stderr=%q", sel, err, stderr)
	}
	if err != nil {
		writeLog("stream.resolve.error err=%v stderr=%q", err, stderr)
		return "", fmt.Errorf("yt-dlp stream error: %w\n%s", err, stderr)
	}

	streamURL := strings.TrimSpace(stdout)
	if streamURL == "" {
		return "", fmt.Errorf("empty stream URL")
	}

	lines := strings.Split(streamURL, "\n")
	for _, line := range lines {
		candidate := strings.TrimSpace(line)
		if utils.IsHTTPURL(candidate) {
			if u, err := url.Parse(candidate); err == nil {
				writeLog("stream.resolve.ok host=%q path=%q", u.Host, u.Path)
			} else {
				writeLog("stream.resolve.ok raw_url=%q", candidate)
			}
			return candidate, nil
		}
	}

	writeLog("stream.resolve.error no-playable-url raw=%q", streamURL)
	return "", fmt.Errorf("no playable stream URL found")
}
