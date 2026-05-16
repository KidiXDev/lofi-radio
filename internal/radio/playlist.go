package radio

import (
	"github.com/kidixdev/lofi-radio/internal/utils"
	"strings"
)

func FetchCategoriesFromPlaylist(playlistURL string) ([]Category, error) {
	stdout, stderr, err := runYtDlp(
		"--flat-playlist",
		"--encoding", "utf-8",
		"--print", "%(title)s\t%(id)s",
		playlistURL,
	)
	if err != nil {
		return nil, newYtDlpFriendlyError(
			"playlist.fetch",
			err,
			stderr,
			"Failed to load categories for this channel.",
		)
	}

	lines := strings.Split(stdout, "\n")
	categories := make([]Category, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}

		title := strings.TrimSpace(parts[0])
		videoID := strings.TrimSpace(parts[1])
		if title == "" || videoID == "" || isUnavailableEntry(title) {
			continue
		}

		videoURL := normalizeVideoURL(videoID)
		if _, exists := seen[videoURL]; exists {
			continue
		}

		categories = append(categories, Category{
			Title:    title,
			VideoURL: videoURL,
		})
		seen[videoURL] = struct{}{}
	}

	if len(categories) == 0 {
		return nil, &FriendlyError{
			Op:      "playlist.fetch",
			Message: "This channel has no categories available right now.",
		}
	}

	return categories, nil
}

func normalizeVideoURL(value string) string {
	if utils.IsHTTPURL(value) {
		return value
	}

	return "https://www.youtube.com/watch?v=" + value
}

func isUnavailableEntry(title string) bool {
	normalized := strings.ToLower(strings.TrimSpace(title))

	return strings.Contains(normalized, "private video") ||
		strings.Contains(normalized, "deleted video") ||
		normalized == "[unavailable video]"
}
