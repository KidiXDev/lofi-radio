package config

import (
	"fmt"
	"strings"
)

const ChannelEnvVar = "LOFI_CHANNEL"

type ChannelType string

const (
	ChannelTypeLive  ChannelType = "live"
	ChannelTypeVideo ChannelType = "video"
)

type Channel struct {
	ID          string
	Name        string
	Type        ChannelType
	PlaylistURL *string
	VideoURL    *string
	ChannelURL  *string
}

var channels = []Channel{
	{
		ID:          "lofi-girl",
		Name:        "Lofi Girl",
		Type:        ChannelTypeLive,
		PlaylistURL: stringPtr("https://youtube.com/playlist?list=PL6NdkXsPL07Il2hEQGcLI4dg_LTg7xA2L&si=nKgC5KsqxFZjtEv3"),
	},
	{
		ID:          "chillhop",
		Name:        "Chillhop Music",
		Type:        ChannelTypeLive,
		PlaylistURL: stringPtr("https://youtube.com/playlist?list=PLt7bG0K25iXjjrfjMxkI6ClvebydMpT4b&si=ZzYGA0G96p4zDHmO"),
	},
	{
		ID:          "bootleg-boy",
		Name:        "The Bootleg Boy",
		Type:        ChannelTypeLive,
		PlaylistURL: stringPtr("https://youtube.com/playlist?list=PLOzDu-MXXLlgdiaISfz-Vf9mkVsNW1Bw_&si=AeBUwg7WSsBYZLyt"),
	},
	{
		ID:          "steezyasfck",
		Name:        "STEEZYASFUCK",
		Type:        ChannelTypeLive,
		PlaylistURL: stringPtr("https://youtube.com/playlist?list=PLqeSJS3N5tzhr13DqMJVXPPSio5UYS9sb&si=Ho1-4iCiBNRU2e-C"),
	},
	{
		ID:       "claude-fm",
		Name:     "Claude FM",
		Type:     ChannelTypeLive,
		VideoURL: stringPtr("https://www.youtube.com/live/YmQ7jRgf4f0?si=SDCDv_vrOeQgoqk9"),
	},
	{
		ID:         "homework-radio",
		Name:       "Homework Radio",
		Type:       ChannelTypeLive,
		ChannelURL: stringPtr("https://www.youtube.com/@HomeworkRadio"),
	},
	{
		ID:          "intimate-vibes",
		Name:        "Intimate Vibes",
		Type:        ChannelTypeVideo,
		PlaylistURL: stringPtr("https://www.youtube.com/playlist?list=PLqyaDLGn3DXcMn6i18yr4fBoNz4tvMi6l"),
	},
}

func stringPtr(v string) *string {
	return &v
}

func (c Channel) ActiveURL() string {
	if c.PlaylistURL != nil {
		return *c.PlaylistURL
	}
	if c.VideoURL != nil {
		return *c.VideoURL
	}
	if c.ChannelURL != nil {
		return *c.ChannelURL
	}
	return ""
}

func Channels() []Channel {
	items := make([]Channel, len(channels))
	copy(items, channels)
	return items
}

func DefaultChannel() Channel {
	return channels[0]
}

func ResolveChannel(channelID string) (Channel, error) {
	if strings.TrimSpace(channelID) == "" {
		channel := DefaultChannel()
		if err := channel.Validate(); err != nil {
			return Channel{}, err
		}
		return channel, nil
	}

	selected := strings.ToLower(strings.TrimSpace(channelID))
	for _, channel := range channels {
		if channel.ID == selected {
			if err := channel.Validate(); err != nil {
				return Channel{}, err
			}
			return channel, nil
		}
	}

	return Channel{}, fmt.Errorf("unknown channel %q", channelID)
}

func (c Channel) Validate() error {
	if c.Type != ChannelTypeLive && c.Type != ChannelTypeVideo {
		return fmt.Errorf("channel %q must set a valid Type (%q or %q)", c.ID, ChannelTypeLive, ChannelTypeVideo)
	}

	count := 0
	if c.PlaylistURL != nil {
		count++
	}
	if c.VideoURL != nil {
		count++
	}
	if c.ChannelURL != nil {
		count++
	}
	if count != 1 {
		return fmt.Errorf("channel %q must set exactly one of PlaylistURL, VideoURL, ChannelURL", c.ID)
	}
	return nil
}
