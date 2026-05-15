package main

import (
	"fmt"
	"github.com/kidixdev/lofi-radio/internal/config"
	"github.com/kidixdev/lofi-radio/internal/tui"
	"os"
)

func main() {
	if err := tui.Run(config.PlaylistURL); err != nil {
		fmt.Println("Error:", err)
		os.Exit(1)
	}
}
