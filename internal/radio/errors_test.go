package radio

import (
	"errors"
	"testing"
)

func TestCleanYtDlpError(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "standard youtube live stream not available",
			input:    "ERROR: [youtube] HBPtQVzRZUY: This live stream recording is not available.",
			expected: "This live stream recording is not available.",
		},
		{
			name:     "age confirmation required",
			input:    "ERROR: [youtube] Sign in to confirm your age",
			expected: "Sign in to confirm your age",
		},
		{
			name:     "generic error with no extractor",
			input:    "ERROR: Something went terribly wrong",
			expected: "Something went terribly wrong",
		},
		{
			name:     "extractor only no ID",
			input:    "ERROR: [youtube] Private video. Sign in if you have been granted access to this video",
			expected: "Private video. Sign in if you have been granted access to this video",
		},
		{
			name:     "playlist private",
			input:    "ERROR: [youtube:playlist] PLxxxxx: playlist is private",
			expected: "playlist is private",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := cleanYtDlpError(tc.input)
			if actual != tc.expected {
				t.Errorf("Expected: %q, got: %q", tc.expected, actual)
			}
		})
	}
}

func TestMapYtDlpMessage(t *testing.T) {
	tests := []struct {
		name     string
		stderr   string
		fallback string
		expected string
	}{
		{
			name:     "warning and actual error",
			stderr:   "WARNING: [youtube] No supported JavaScript runtime could be found. Only deno is enabled by default; to use another runtime add  --js-runtimes RUNTIME[:PATH]  to your command/config. YouTube extraction without a JS runtime has been deprecated, and some formats may be missing. See  https://github.com/yt-dlp/yt-dlp/wiki/EJS  for details on installing one\nERROR: [youtube] HBPtQVzRZUY: This live stream recording is not available.",
			fallback: "Failed to resolve stream.",
			expected: "This live stream recording is not available.",
		},
		{
			name:     "warning and age confirmation",
			stderr:   "WARNING: [youtube] No supported JavaScript runtime could be found.\nERROR: [youtube] Sign in to confirm your age",
			fallback: "Failed to resolve stream.",
			expected: "This video requires account access and cannot be played here.",
		},
		{
			name:     "only warning about JS runtime",
			stderr:   "WARNING: [youtube] No supported JavaScript runtime could be found. Only deno is enabled by default; to use another runtime add  --js-runtimes RUNTIME[:PATH]  to your command/config.",
			fallback: "Failed to resolve stream.",
			expected: "Cannot extract stream metadata. Please update yt-dlp or install a JS runtime (Node.js or Deno).",
		},
		{
			name:     "no errors or warnings - fallback",
			stderr:   "Just some standard output\nNo issues here",
			fallback: "Default fallback message",
			expected: "Default fallback message",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := mapYtDlpMessage(tc.stderr, tc.fallback)
			if actual != tc.expected {
				t.Errorf("Expected: %q, got: %q", tc.expected, actual)
			}
		})
	}
}

func TestUserMessage(t *testing.T) {
	err := &FriendlyError{
		Op:      "test",
		Message: "User-friendly message",
		Detail:  "Some low level details",
		Err:     errors.New("raw error"),
	}

	msg := UserMessage(err)
	if msg != "User-friendly message" {
		t.Errorf("Expected 'User-friendly message', got %q", msg)
	}

	rawErr := errors.New("raw error only")
	msgRaw := UserMessage(rawErr)
	if msgRaw != "Operation failed. Please try another category or channel." {
		t.Errorf("Expected fallback string, got %q", msgRaw)
	}
}
