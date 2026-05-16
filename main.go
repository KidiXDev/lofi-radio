package main

import (
	"flag"
	"fmt"
	"github.com/kidixdev/lofi-radio/internal/config"
	"github.com/kidixdev/lofi-radio/internal/radio"
	"github.com/kidixdev/lofi-radio/internal/tui"
	"os"
)

func main() {
	defaultChannel := config.DefaultChannel()
	channelID := flag.String("channel", defaultChannel.ID, "channel id to use")
	listChannels := flag.Bool("list-channels", false, "list available channel ids and exit")
	flag.Parse()

	if *listChannels {
		fmt.Println("Available channels:")
		for _, channel := range config.Channels() {
			fmt.Printf("- %s (%s)\n", channel.ID, channel.Name)
		}
		os.Exit(0)
	}

	radio.InitCrashLogging()

	defer func() {
		if recovered := recover(); recovered != nil {
			radio.Logf("main.panic recovered=%v", recovered)
			fmt.Println("Panic:", recovered)
			os.Exit(1)
		}
	}()

	selectedChannel, err := config.ResolveChannel(*channelID)
	if err != nil {
		fmt.Printf(
			"Error: %v. Run with -list-channels to see valid ids, or set %s.\n",
			err,
			config.ChannelEnvVar,
		)
		os.Exit(1)
	}

	if envValue := os.Getenv(config.ChannelEnvVar); envValue != "" && *channelID == defaultChannel.ID {
		selectedChannel, err = config.ResolveChannel(envValue)
		if err != nil {
			fmt.Printf(
				"Error: invalid %s value %q (%v). Run with -list-channels to see valid ids.\n",
				config.ChannelEnvVar,
				envValue,
				err,
			)
			os.Exit(1)
		}
	}

	if err := tui.Run(selectedChannel.Name, selectedChannel.PlaylistURL); err != nil {
		radio.Logf("main.error err=%v", err)
		fmt.Println("Error:", err)
		os.Exit(1)
	}
}
