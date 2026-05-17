package radio

import (
	"errors"
	"fmt"
	"strings"
)

type FriendlyError struct {
	Op      string
	Message string
	Detail  string
	Err     error
}

func (e *FriendlyError) Error() string {
	if e == nil {
		return ""
	}

	parts := make([]string, 0, 4)
	if e.Op != "" {
		parts = append(parts, e.Op)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if e.Err != nil {
		parts = append(parts, e.Err.Error())
	}
	if e.Detail != "" {
		parts = append(parts, e.Detail)
	}

	return strings.Join(parts, ": ")
}

func (e *FriendlyError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func UserMessage(err error) string {
	if err == nil {
		return ""
	}

	var friendly *FriendlyError
	if errors.As(err, &friendly) {
		if strings.TrimSpace(friendly.Message) != "" {
			return friendly.Message
		}
	}

	return "Operation failed. Please try another category or channel."
}

func newYtDlpFriendlyError(op string, runErr error, stderr, genericMessage string) error {
	userMessage := mapYtDlpMessage(stderr, genericMessage)
	return &FriendlyError{
		Op:      op,
		Message: userMessage,
		Detail:  strings.TrimSpace(stderr),
		Err:     runErr,
	}
}

func cleanYtDlpError(errLine string) string {
	errLine = strings.TrimSpace(errLine)

	// Strip "ERROR:" prefix (case-insensitive)
	if strings.HasPrefix(strings.ToLower(errLine), "error:") {
		errLine = errLine[len("error:"):]
		errLine = strings.TrimSpace(errLine)
	}

	// Strip video extractor, e.g. "[youtube]"
	if strings.HasPrefix(errLine, "[") {
		idx := strings.Index(errLine, "]")
		if idx != -1 {
			errLine = errLine[idx+1:]
			errLine = strings.TrimSpace(errLine)
		}
	}

	// Strip video ID, e.g. "HBPtQVzRZUY: " if present before the colon
	if idx := strings.Index(errLine, ": "); idx != -1 {
		prefix := errLine[:idx]
		if !strings.Contains(prefix, " ") && len(prefix) < 30 {
			errLine = errLine[idx+2:]
			errLine = strings.TrimSpace(errLine)
		}
	}

	return errLine
}

func mapYtDlpMessage(stderr, fallback string) string {
	// Split by newline to find the actual ERROR lines
	var errorLines []string
	for _, line := range strings.Split(stderr, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Match exact/sub "ERROR:" prefix
		if strings.Contains(strings.ToUpper(trimmed), "ERROR:") {
			errorLines = append(errorLines, trimmed)
		}
	}

	// Determine the main text for matching and processing
	var targetText string
	var isFromErrorLine bool
	if len(errorLines) > 0 {
		targetText = errorLines[0]
		isFromErrorLine = true
	} else {
		targetText = stderr
	}

	low := strings.ToLower(targetText)

	switch {
	case strings.Contains(low, "this live event will begin in"):
		return "This category is scheduled and not live yet. Choose another category."
	case strings.Contains(low, "video unavailable"),
		strings.Contains(low, "private video"),
		strings.Contains(low, "deleted video"),
		strings.Contains(low, "[unavailable video]"):
		return "This category is currently unavailable. Choose another category."
	case strings.Contains(low, "sign in to confirm your age"),
		strings.Contains(low, "private"):
		return "This video requires account access and cannot be played here."
	case strings.Contains(low, "429"),
		strings.Contains(low, "too many requests"):
		return "Rate limited by YouTube. Please wait a moment, then try again."
	case strings.Contains(low, "timed out"),
		strings.Contains(low, "connection refused"),
		strings.Contains(low, "temporary failure"),
		strings.Contains(low, "name resolution"),
		strings.Contains(low, "network is unreachable"):
		return "Network error while contacting YouTube. Check your connection and try again."
	case strings.Contains(low, "no supported javascript runtime could be found"):
		// Only return the JS runtime missing message if we didn't have a more specific ERROR line!
		return "Cannot extract stream metadata. Please update yt-dlp or install a JS runtime (Node.js or Deno)."
	}

	// If we got a specific error line but it didn't match any standard friendly categories,
	// clean it up and show the raw/original YouTube error directly!
	if isFromErrorLine {
		cleaned := cleanYtDlpError(targetText)
		if cleaned != "" {
			return cleaned
		}
	}

	if strings.TrimSpace(fallback) != "" {
		return fallback
	}
	return "Failed to fetch stream data from YouTube."
}

func WrapFriendly(op, message string, err error) error {
	if err == nil {
		return nil
	}

	return &FriendlyError{
		Op:      op,
		Message: message,
		Err:     err,
	}
}

func LogErrorDetails(err error) {
	if err == nil {
		return
	}

	var friendly *FriendlyError
	if errors.As(err, &friendly) {
		if friendly.Detail != "" {
			writeLog("error.detail op=%q detail=%q", friendly.Op, friendly.Detail)
		}
		return
	}

	writeLog("error.raw err=%q", fmt.Sprintf("%v", err))
}
