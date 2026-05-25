package config

import "testing"

func TestResolveChannelIntimateVibesIsVideo(t *testing.T) {
	t.Parallel()

	channel, err := ResolveChannel("intimate-vibes")
	if err != nil {
		t.Fatalf("ResolveChannel() error = %v", err)
	}

	if channel.Type != ChannelTypeVideo {
		t.Fatalf("expected type %q, got %q", ChannelTypeVideo, channel.Type)
	}
	if channel.PlaylistURL == nil {
		t.Fatal("expected intimate-vibes to use PlaylistURL")
	}
}

func TestChannelValidateRejectsInvalidType(t *testing.T) {
	t.Parallel()

	channel := Channel{
		ID:       "broken-channel",
		Name:     "Broken Channel",
		Type:     ChannelType("invalid"),
		VideoURL: stringPtr("https://www.youtube.com/watch?v=dQw4w9WgXcQ"),
	}

	if err := channel.Validate(); err == nil {
		t.Fatal("expected Validate() to fail for invalid channel type")
	}
}
