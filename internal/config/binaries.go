package config

import (
	"os"
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

// AppDir is the executable's directory. Config, logs, cache and downloaded
// binaries live there so they don't depend on the shell's working directory.
func AppDir() string {
	executablePath, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(executablePath)
}

func defaultFFmpegPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(AppDir(), "bin", "ffmpeg", "win", "ffmpeg.exe")
	}
	return filepath.Join(AppDir(), "bin", "ffmpeg", "linux", "ffmpeg")
}

func defaultYtDlpPath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(AppDir(), "bin", "ytdlp", "yt-dlp.exe")
	}

	return filepath.Join(AppDir(), "bin", "ytdlp", "yt-dlp_linux")
}
