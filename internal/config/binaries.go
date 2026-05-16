package config

import (
	"path/filepath"
	"runtime"
)

var (
	ytDlpPath  = defaultYtDlpPath()
	ffmpegPath = defaultFFmpegPath()
)

func SetBinaryPaths(ytDlp, ffmpeg string) {
	if ytDlp != "" {
		ytDlpPath = ytDlp
	}

	if ffmpeg != "" {
		ffmpegPath = ffmpeg
	}
}

func YtDlpPath() string {
	return ytDlpPath
}

func FFmpegPath() string {
	return ffmpegPath
}

func defaultFFmpegPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join("bin", "ffmpeg", "win", "ffmpeg.exe")
	}
	return filepath.Join("bin", "ffmpeg", "linux", "ffmpeg")
}

func defaultYtDlpPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join("bin", "ytdlp", "yt-dlp.exe")
	}

	return filepath.Join("bin", "ytdlp", "yt-dlp_linux")
}
