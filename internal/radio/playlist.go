package radio

import (
	"github.com/kidixdev/lofi-radio/internal/utils"
	"net/url"
	"path"
	"regexp"
	"strings"
)

var trailingDateTimeSuffix = regexp.MustCompile(`\s+\d{4}-\d{2}-\d{2}\s+\d{2}:\d{2}(:\d{2})?\s*$`)

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

		title := cleanCategoryTitle(parts[0])
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

func FetchCategoryFromVideo(videoURL string) ([]Category, error) {
	stdout, stderr, err := runYtDlp(
		"--no-playlist",
		"--encoding", "utf-8",
		"--print", "%(title)s\t%(id)s",
		videoURL,
	)
	if err != nil {
		return nil, newYtDlpFriendlyError(
			"video.fetch",
			err,
			stderr,
			"Failed to load category for this video.",
		)
	}

	for _, rawLine := range strings.Split(stdout, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}

		title := cleanCategoryTitle(parts[0])
		videoID := strings.TrimSpace(parts[1])
		if title == "" || videoID == "" {
			continue
		}

		return []Category{
			{
				Title:    title,
				VideoURL: normalizeVideoURL(videoID),
			},
		}, nil
	}

	return nil, &FriendlyError{
		Op:      "video.fetch",
		Message: "This video has no playable category right now.",
	}
}

func FetchLiveCategoriesFromChannel(channelURL string) ([]Category, error) {
	streamsURL := normalizeChannelStreamsURL(channelURL)
	stdout, stderr, err := runYtDlp(
		"--flat-playlist",
		"--encoding", "utf-8",
		"--print", "%(title)s\t%(id)s\t%(live_status)s",
		streamsURL,
	)
	if err != nil {
		return nil, newYtDlpFriendlyError(
			"channel.fetch",
			err,
			stderr,
			"Failed to load live categories for this channel.",
		)
	}

	categories := parseChannelCategories(stdout)

	// Fallback: some channel URLs return incomplete live_status in flat mode.
	// Ask yt-dlp to resolve the streams tab explicitly from the base URL.
	if len(categories) == 0 {
		fallbackStdout, _, fallbackErr := runYtDlp(
			"--flat-playlist",
			"--encoding", "utf-8",
			"--extractor-args", "youtube:tab=streams",
			"--print", "%(title)s\t%(id)s\t%(live_status)s",
			strings.TrimSpace(channelURL),
		)
		if fallbackErr == nil {
			categories = parseChannelCategories(fallbackStdout)
		}
	}
	if len(categories) == 0 {
		return nil, &FriendlyError{
			Op:      "channel.fetch",
			Message: "This channel has no live categories available right now.",
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

func normalizeChannelStreamsURL(channelURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(channelURL))
	if err != nil || parsed == nil {
		return channelURL
	}

	if strings.HasSuffix(parsed.Path, "/streams") {
		return parsed.String()
	}

	parsed.Path = path.Clean(parsed.Path + "/streams")
	return parsed.String()
}

func cleanCategoryTitle(value string) string {
	title := strings.TrimSpace(value)
	title = trailingDateTimeSuffix.ReplaceAllString(title, "")
	return strings.TrimSpace(title)
}

func parseChannelCategories(stdout string) []Category {
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

		title := cleanCategoryTitle(parts[0])
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

	return categories
}
