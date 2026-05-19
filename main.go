package main

import (
	"bufio"
	"flag"
	"fmt"
	"github.com/kidixdev/lofi-radio/internal/config"
	"github.com/kidixdev/lofi-radio/internal/radio"
	"github.com/kidixdev/lofi-radio/internal/startup"
	"github.com/kidixdev/lofi-radio/internal/tui"
	"github.com/kidixdev/lofi-radio/internal/uninstall"
	"github.com/kidixdev/lofi-radio/internal/update"
	"github.com/kidixdev/lofi-radio/internal/version"
	"os"
	"strings"
)

func main() {
	defaultChannel := config.DefaultChannel()
	channelID := flag.String("channel", defaultChannel.ID, "channel id to use")
	listChannels := flag.Bool("list-channels", false, "list available channel ids and exit")
	showVersion := flag.Bool("version", false, "print app version and exit")
	runUpdate := flag.Bool("update", false, "download and install latest release")
	updateInstallDir := flag.String("update-install-dir", "", "install directory for -update")
	runUninstall := flag.Bool("uninstall", false, "uninstall this app")
	assumeYes := flag.Bool("y", false, "skip confirmation for destructive operations")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
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

	if *runUninstall {
		if !*assumeYes {
			fmt.Print("This will uninstall lofi from the current executable location. Continue? [y/N]: ")
			reader := bufio.NewReader(os.Stdin)
			answer, _ := reader.ReadString('\n')
			answer = strings.ToLower(strings.TrimSpace(answer))
			if answer != "y" && answer != "yes" {
				fmt.Println("Uninstall canceled.")
				os.Exit(0)
			}
		}

		result, err := uninstall.Run()
		if err != nil {
			fmt.Printf("Error: uninstall failed: %v\n", err)
			os.Exit(1)
		}

		if result.Deferred {
			fmt.Printf("Uninstall scheduled.\n")
			fmt.Printf("Executable will be removed after this process exits: %s\n", result.ExecutablePath)
			fmt.Printf("Cache directory cleanup scheduled: %s\n", result.CacheDir)
		} else {
			fmt.Printf("Uninstall complete.\n")
			fmt.Printf("Removed executable: %s\n", result.ExecutablePath)
			fmt.Printf("Removed cache directory: %s\n", result.CacheDir)
		}
		if result.PathUpdated {
			fmt.Printf("PATH references updated.\n")
		}
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

	if err := startup.CheckExecutableCachePermissions(); err != nil {
		radio.Logf("main.startup.permission_check.error err=%v", err)
		fmt.Printf("Error: cannot write cache in executable directory.\n")
		fmt.Printf("Reason: %v\n", err)
		fmt.Printf("Please move the app to a writable folder (for example inside your user directory) and run it again.\n")
		os.Exit(1)
	}

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

	configManager, err := config.NewDefaultManager()
	if err != nil {
		fmt.Printf("Error: failed to initialize config manager: %v\n", err)
		os.Exit(1)
	}

	settings, err := configManager.Load()
	if err != nil {
		fmt.Printf("Error: failed to load persisted config: %v\n", err)
		os.Exit(1)
	}

	if err := tui.Run(selectedChannel, settings, configManager); err != nil {
		radio.Logf("main.error err=%v", err)
		fmt.Println("Error:", err)
		os.Exit(1)
	}
}
