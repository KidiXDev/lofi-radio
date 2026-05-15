package main

import (
	"fmt"
	"github.com/kidixdev/lofi-radio/internal/bootstrap"
	"github.com/kidixdev/lofi-radio/internal/config"
	"github.com/kidixdev/lofi-radio/internal/radio"
	"os"
)

func main() {
	fmt.Println("Lofi Radio Player")
	fmt.Println("=================")
	fmt.Println("Checking dependencies...")

	_, err := bootstrap.EnsureDependencies()
	if err != nil {
		fmt.Println("Dependency setup failed:", err)
		os.Exit(1)
	}

	fmt.Println("Loading currently available radios...")

	stations, err := radio.FetchStationsFromPlaylist(config.PlaylistURL)
	if err != nil {
		fmt.Println("Failed to load radios:", err)
		os.Exit(1)
	}

	if len(stations) == 0 {
		fmt.Println("No available radios found in playlist.")
		os.Exit(1)
	}

	station := radio.ChooseStation(stations)

	fmt.Println()
	fmt.Println("Selected:", station.Title)
	fmt.Println("Getting direct audio stream...")

	streamURL, err := radio.GetDirectAudioURL(station.VideoURL)
	if err != nil {
		fmt.Println("Failed to get stream URL:", err)
		return
	}

	fmt.Println("Playing:", station.Title)
	fmt.Println("Press Ctrl+C to stop.")
	fmt.Println()

	if err := radio.PlayStream(streamURL); err != nil {
		fmt.Println("Playback error:", err)
	}
}
