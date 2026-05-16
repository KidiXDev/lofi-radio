package config

import (
	"fmt"
	"strings"
)

const ChannelEnvVar = "LOFI_CHANNEL"

type Channel struct {
	ID          string
	Name        string
	PlaylistURL string
}

var channels = []Channel{
	{
		ID:          "lofi-girl",
		Name:        "Lofi Girl",
		PlaylistURL: "https://youtube.com/playlist?list=PL6NdkXsPL07Il2hEQGcLI4dg_LTg7xA2L&si=nKgC5KsqxFZjtEv3",
	},
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
		return DefaultChannel(), nil
	}

	selected := strings.ToLower(strings.TrimSpace(channelID))
	for _, channel := range channels {
		if channel.ID == selected {
			return channel, nil
		}
	}

	return Channel{}, fmt.Errorf("unknown channel %q", channelID)
}
