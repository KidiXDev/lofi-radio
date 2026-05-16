package config

import (
	"path/filepath"
	"runtime"
)

var (
	ytDlpPath   = defaultYtDlpPath()
	ffplayPath  = defaultFFplayPath()
	ffmpegPath  = defaultFFmpegPath()
)

func SetBinaryPaths(ytDlp, ffplay string) {
	if ytDlp != "" {
		ytDlpPath = ytDlp
	}

	if ffplay != "" {
		ffplayPath = ffplay
		// Derive ffmpeg sibling from ffplay path.
		dir := filepath.Dir(ffplay)
		base := "ffmpeg"
		if runtime.GOOS == "windows" {
			base = "ffmpeg.exe"
		}
		ffmpegPath = filepath.Join(dir, base)
	}
}

func YtDlpPath() string {
	return ytDlpPath
}

func FFplayPath() string {
	return ffplayPath
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

func defaultFFplayPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join("bin", "ffmpeg", "win", "ffplay.exe")
	}

	return filepath.Join("bin", "ffmpeg", "linux", "ffplay")
}
