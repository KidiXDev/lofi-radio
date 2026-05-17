package radio

import (
	"github.com/kidixdev/lofi-radio/internal/utils"
	"strings"
)

func FetchCategoriesFromPlaylist(playlistURL string) ([]Category, error) {
	stdout, stderr, err := runYtDlp(
		"--flat-playlist",
		"--encoding", "utf-8",
		"--print", "%(title)s\t%(id)s\t%(live_status)s",
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

		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}

		title := strings.TrimSpace(parts[0])
		videoID := strings.TrimSpace(parts[1])
		liveStatus := ""
		if len(parts) >= 3 {
			liveStatus = strings.TrimSpace(parts[2])
		}

		if title == "" || videoID == "" || isUnavailableEntry(title, liveStatus) {
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

func isUnavailableEntry(title, liveStatus string) bool {
	normalizedTitle := strings.ToLower(strings.TrimSpace(title))
	normalizedStatus := strings.ToLower(strings.TrimSpace(liveStatus))

	if normalizedStatus == "is_upcoming" || normalizedStatus == "was_live" || normalizedStatus == "post_live" {
		return true
	}

	return strings.Contains(normalizedTitle, "private video") ||
		strings.Contains(normalizedTitle, "deleted video") ||
		strings.Contains(normalizedTitle, "unavailable") ||
		strings.Contains(normalizedTitle, "not available") ||
		strings.Contains(normalizedTitle, "upcoming video") ||
		strings.Contains(normalizedTitle, "upcoming live stream")
}
