# 🎧 Lofi Radio

A sleek, lightweight command-line interface for streaming high-quality Lofi music directly from YouTube playlists. Built with Go, powered by `yt-dlp`, `ffmpeg`, and `go-tui`.

![Go Version](https://img.shields.io/badge/Go-1.25.1-00ADD8?style=flat-square&logo=go)
![License](https://img.shields.io/badge/License-GPLv3-green?style=flat-square)
![Platform](https://img.shields.io/badge/Platform-Windows%20%7C%20Linux-orange?style=flat-square)

## ✨ Features

- **Interactive Full-Screen TUI**: Built on [`go-tui`](https://github.com/grindlemire/go-tui) with clean station picker and now-playing screen.
- **Auto-Discovery**: Fetches currently available stations from the selected channel playlist.
- **Scalable Channel Config**: Channels are defined in a centralized config catalog and selected by id.
- **Lightweight**: Minimal CPU and memory footprint compared to browser-based playback.

## 📻 Supported Channels
Planned channel roadmap:
- [x] **Lofi Girl**
- [ ] **Chillhop Music**
- [ ] **the bootleg boy**
- [ ] **STEEZYASFUCK**
- [ ] **The Jazz Hop Café**
- [ ] **Homework Radio**
- [ ] **Ambition**
- [ ] **Dreamy**
- [ ] **Lofi Geek**
- [ ] **Chill with Taiki**

## 🚀 Getting Started

### Prerequisites

- [Go](https://go.dev/doc/install) (1.25.1 or later)
- Internet connection

### Dependencies
- [yt-dlp](https://github.com/yt-dlp/yt-dlp): For extracting audio streams from YouTube.
- [FFmpeg/FFplay](https://ffmpeg.org/): For high-performance audio playback.
- [go-tui](https://github.com/grindlemire/go-tui): For the terminal UI framework.

## 🗺️ Roadmap

We are continuously working to make **Lofi Radio** the best terminal-based lofi player. Here is what we have planned:

- [x] **TUI Station Picker**: Add a screen to switch between different lofi providers seamlessly.
- [x] **Volume Control**: Native volume adjustment within the TUI.
- [x] **Visualizers**: Add an ASCII-based audio visualizer for that extra retro feel.
- [ ] **Favorites**: Bookmark specific tracks or streams.
- [ ] **Sleep Timer**: Automatically stop playback after a set duration.

and more to come !

## 📜 License

Distributed under the [GNU General Public License v3.0](LICENSE).
