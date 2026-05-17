package radio

import "testing"

func TestIsUnavailableEntry(t *testing.T) {
	tests := []struct {
		name       string
		title      string
		liveStatus string
		expected   bool
	}{
		{
			name:       "valid active stream",
			title:      "Lofi Girl - beats to relax/study to",
			liveStatus: "is_live",
			expected:   false,
		},
		{
			name:       "valid standard upload",
			title:      "Cozy Rain Ambience",
			liveStatus: "not_live",
			expected:   false,
		},
		{
			name:       "valid empty/NA status",
			title:      "Chill Lofi Beats",
			liveStatus: "NA",
			expected:   false,
		},
		{
			name:       "upcoming stream via status",
			title:      "Cozy Lofi Chill - Upcoming Stream",
			liveStatus: "is_upcoming",
			expected:   true,
		},
		{
			name:       "private video via title check",
			title:      "[Private video]",
			liveStatus: "NA",
			expected:   true,
		},
		{
			name:       "deleted video via title check",
			title:      "[Deleted video]",
			liveStatus: "NA",
			expected:   true,
		},
		{
			name:       "unavailable video via title check",
			title:      "[Unavailable video]",
			liveStatus: "NA",
			expected:   true,
		},
		{
			name:       "unavailable live stream recording via title check",
			title:      "This live stream recording is not available.",
			liveStatus: "NA",
			expected:   true,
		},
		{
			name:       "upcoming video via title",
			title:      "[upcoming video]",
			liveStatus: "NA",
			expected:   true,
		},
		{
			name:       "upcoming live stream via title",
			title:      "Upcoming Live Stream",
			liveStatus: "NA",
			expected:   true,
		},
		{
			name:       "ended live stream via status",
			title:      "Ended Lofi Stream",
			liveStatus: "was_live",
			expected:   true,
		},
		{
			name:       "post-processing live stream via status",
			title:      "Processing Lofi Stream",
			liveStatus: "post_live",
			expected:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := isUnavailableEntry(tc.title, tc.liveStatus)
			if actual != tc.expected {
				t.Errorf("Expected isUnavailableEntry(%q, %q) to be %v, got %v", tc.title, tc.liveStatus, tc.expected, actual)
			}
		})
	}
}
