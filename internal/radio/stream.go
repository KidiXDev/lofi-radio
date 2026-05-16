package radio

import (
	"fmt"
	"github.com/kidixdev/lofi-radio/internal/utils"
	"net/url"
	"strings"
)

func GetDirectAudioURL(videoURL string) (string, error) {
	writeLog("stream.resolve.start video_url=%q", videoURL)

	// Prefer a resilient selector first. Newer yt-dlp + YouTube frequently
	// requires JS runtime to expose exact audio-only itags; `bestaudio/best`
	// still resolves quickly and avoids multi-pass failures on first connect.
	audioOnlySelectors := []string{
		"bestaudio/best",
		"bestaudio[protocol!=m3u8]/bestaudio",
		"140/251/250/249/bestaudio",
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
