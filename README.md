# 🎧 Lofi Radio CLI

A sleek, lightweight command-line interface for streaming high-quality Lofi music directly from YouTube playlists. Built with Go, powered by `yt-dlp` and `ffplay`.

![Go Version](https://img.shields.io/badge/Go-1.25.1-00ADD8?style=flat-square&logo=go)
![License](https://img.shields.io/badge/License-MIT-green?style=flat-square)
![Platform](https://img.shields.io/badge/Platform-Windows%20%7C%20Linux-orange?style=flat-square)

## ✨ Features

- **Instant Streaming**: Play curated lofi beats without opening a browser.
- **Auto-Discovery**: Fetches the latest live streams and videos from a central playlist.
- **Zero Configuration**: Bundled with all necessary binaries (`ffmpeg`, `yt-dlp`) for Windows and Linux.
- **Lightweight**: Minimal CPU and memory footprint compared to web browsers.
- **Terminal UI**: Simple and intuitive command-line interaction.

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

## 🛠️ How it Works

The application uses a modular architecture:
- **`internal/radio`**: Handles playlist parsing, station selection, and audio stream extraction.
- **`internal/utils`**: Core execution utilities for handling external processes.

### Dependencies
- [yt-dlp](https://github.com/yt-dlp/yt-dlp): For extracting audio streams from YouTube.
- [FFmpeg/FFplay](https://ffmpeg.org/): For high-performance audio playback.

## 📁 Project Structure

```text
.
├── bin/            # Bundled binaries (ffmpeg, yt-dlp)
│   ├── linux/      # Linux-specific binaries
│   └── win/        # Windows-specific binaries (.exe)
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
