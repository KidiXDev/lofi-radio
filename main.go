package main

import (
	"fmt"
	"github.com/kidixdev/lofi-radio/internal/config"
	"github.com/kidixdev/lofi-radio/internal/radio"
	"github.com/kidixdev/lofi-radio/internal/tui"
	"os"
)

func main() {
	radio.InitCrashLogging()

	defer func() {
		if recovered := recover(); recovered != nil {
			radio.Logf("main.panic recovered=%v", recovered)
			fmt.Println("Panic:", recovered)
			os.Exit(1)
		}
	}()

	if err := tui.Run(config.PlaylistURL); err != nil {
		radio.Logf("main.error err=%v", err)
		fmt.Println("Error:", err)
		os.Exit(1)
	}
}
