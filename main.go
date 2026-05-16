package main

import (
	"flag"
	"fmt"
	"github.com/kidixdev/lofi-radio/internal/config"
	"github.com/kidixdev/lofi-radio/internal/radio"
	"github.com/kidixdev/lofi-radio/internal/tui"
	"github.com/kidixdev/lofi-radio/internal/update"
	"github.com/kidixdev/lofi-radio/internal/version"
	"os"
)

func main() {
	defaultChannel := config.DefaultChannel()
	channelID := flag.String("channel", defaultChannel.ID, "channel id to use")
	listChannels := flag.Bool("list-channels", false, "list available channel ids and exit")
	showVersion := flag.Bool("version", false, "print app version and exit")
	checkUpdate := flag.Bool("check-update", false, "check latest release version and exit")
	runUpdate := flag.Bool("update", false, "download and install latest release")
	updateInstallDir := flag.String("update-install-dir", "", "install directory for -update")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		os.Exit(0)
	}

	if *checkUpdate {
		release, hasUpdate, err := update.CheckLatest(version.Version)
		if err != nil {
			fmt.Printf("Error: check update failed: %v\n", err)
			os.Exit(1)
		}
		if !hasUpdate {
			fmt.Printf("You are up to date (%s).\n", version.Version)
			os.Exit(0)
		}
		fmt.Printf("Update available: %s -> %s\n", version.Version, release.TagName)
		fmt.Printf("Release page: %s\n", release.HTMLURL)
		os.Exit(0)
	}

	if *runUpdate {
		release, updated, executablePath, err := update.SelfUpdate(version.Version, *updateInstallDir)
		if err != nil {
			fmt.Printf("Error: update failed: %v\n", err)
			os.Exit(1)
		}
		if !updated {
			fmt.Printf("You are up to date (%s).\n", version.Version)
			os.Exit(0)
		}
		fmt.Printf("Updated to %s\n", release.TagName)
		fmt.Printf("Installed binary: %s\n", executablePath)
		fmt.Printf("Release page: %s\n", release.HTMLURL)
		os.Exit(0)
	}

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
