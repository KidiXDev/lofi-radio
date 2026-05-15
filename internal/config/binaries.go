package config

import (
	"path/filepath"
	"runtime"
)

var (
	ytDlpPath  = defaultYtDlpPath()
	ffplayPath = defaultFFplayPath()
)

func SetBinaryPaths(ytDlp, ffplay string) {
	if ytDlp != "" {
		ytDlpPath = ytDlp
	}

	if ffplay != "" {
		ffplayPath = ffplay
	}
}

func YtDlpPath() string {
	return ytDlpPath
}

func FFplayPath() string {
	return ffplayPath
}

func defaultYtDlpPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join("bin", "ytdlp", "yt-dlp.exe")
	}

	return filepath.Join("bin", "ytdlp", "yt-dlp_linux")
}

func defaultFFplayPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join("bin", "ffmpeg", "win", "ffplay.exe")
	}

	return filepath.Join("bin", "ffmpeg", "linux", "ffplay")
}
