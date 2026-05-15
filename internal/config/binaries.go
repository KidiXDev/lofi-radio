package config

import (
	"path/filepath"
	"runtime"
)

func YtDlpPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join("bin", "ytdlp", "yt-dlp.exe")
	}

	return filepath.Join("bin", "ytdlp", "yt-dlp_linux")
}

func FFplayPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join("bin", "ffmpeg", "win", "ffplay.exe")
	}

	return filepath.Join("bin", "ffmpeg", "linux", "ffplay")
}
