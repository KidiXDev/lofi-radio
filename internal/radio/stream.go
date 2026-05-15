package radio

import (
	"fmt"
	"github.com/kidixdev/lofi-radio/internal/utils"
	"strings"
)

func GetDirectAudioURL(videoURL string) (string, error) {
	stdout, stderr, err := runYtDlp(
		"--no-playlist",
		"--encoding", "utf-8",
		"-f", "bestaudio/best",
		"-g",
		videoURL,
	)
	if err != nil {
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
			return candidate, nil
		}
	}

	return "", fmt.Errorf("no playable stream URL found")
}
