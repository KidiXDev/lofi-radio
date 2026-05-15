# 🎧 Lofi Radio CLI

A sleek, lightweight command-line interface for streaming high-quality Lofi music directly from YouTube playlists. Built with Go, powered by `yt-dlp`, `ffplay`, and `go-tui`.

![Go Version](https://img.shields.io/badge/Go-1.25.1-00ADD8?style=flat-square&logo=go)
![License](https://img.shields.io/badge/License-MIT-green?style=flat-square)
![Platform](https://img.shields.io/badge/Platform-Windows%20%7C%20Linux-orange?style=flat-square)

## ✨ Features

- **Interactive Full-Screen TUI**: Built on [`go-tui`](https://github.com/grindlemire/go-tui) with clean station picker and now-playing screen.
- **Bootstrap Progress View**: Shows live dependency download progress, transfer speed, and ETA when binaries need to be fetched.
- **Music-Player Controls**: Pause/resume, volume up/down, station switching, and playback elapsed time in one lightweight interface.
- **Auto-Discovery**: Fetches currently available stations from the configured YouTube playlist.
- **Smart Dependency Bootstrap**: Uses host-installed `ffplay`/`yt-dlp` first, then falls back to local portable binaries in `bin/`.
- **Lightweight**: Minimal CPU and memory footprint compared to browser-based playback.

## 🚀 Getting Started

### Prerequisites

- [Go](https://go.dev/doc/install) (1.25.1 or later)
- Internet connection

### Installation

1. Clone the repository:
   ```bash
   git clone https://github.com/kidixdev/lofi-radio.git
   cd lofi-radio
   ```

2. Build and run:
   ```bash
   go run main.go
   ```

   *Or build the binary:*
   ```bash
   go build -o lofi-radio main.go
   ./lofi-radio
   ```

## ⌨ Controls

- `↑/↓` or `k/j`: Move through station list
- `Enter`: Play selected station
- `Space` or `p`: Pause/resume playback
- `+` / `-`: Volume up/down
- `s`: Switch station while playing
- `q` or `Esc`: Quit (or cancel while stream URL is resolving)

## 🛠️ How it Works

The application uses a modular architecture:
- **`internal/radio`**: Handles playlist parsing, station selection, and audio stream extraction.
- **`internal/utils`**: Core execution utilities for handling external processes.
- **`internal/bootstrap`**: Resolves and bootstraps external binaries (`yt-dlp`, `ffmpeg/ffplay`).

### Dependencies
- [yt-dlp](https://github.com/yt-dlp/yt-dlp): For extracting audio streams from YouTube.
- [FFmpeg/FFplay](https://ffmpeg.org/): For high-performance audio playback.
- [go-tui](https://github.com/grindlemire/go-tui): For the terminal UI framework.

## 📁 Project Structure

```text
.
├── bin/            # Auto-created portable binaries (if host tools are missing)
│   ├── ffmpeg/     # ffmpeg/ffplay binaries
│   └── ytdlp/      # yt-dlp binary
├── internal/       # Core application logic
│   ├── config/     # Configuration & paths
│   ├── radio/      # Streaming & Playlist logic
│   └── utils/      # Shell commands & URL helpers
├── main.go         # Entry point
└── go.mod          # Module definition
```

## 📜 License

Distributed under the MIT License. See `LICENSE` for more information.

---

<p align="center">
  Built with ❤️ for the lofi community.
</p>
