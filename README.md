# 🎧 Lofi Radio

A sleek, lightweight command-line interface for streaming high-quality Lofi music. Built with Go, powered by `yt-dlp`, `ffmpeg`, `oto`, and `go-tui`.

![Go Version](https://img.shields.io/badge/Go-1.25.1-00ADD8?style=flat-square&logo=go)
![License](https://img.shields.io/badge/License-GPLv3-green?style=flat-square)
![Platform](https://img.shields.io/badge/Platform-Windows%20%7C%20Linux-orange?style=flat-square)

## ✨ Features

- **Interactive Full-Screen TUI**: Built on [`go-tui`](https://github.com/grindlemire/go-tui) with clean category picker and now-playing screen.
- **Auto-Discovery**: Fetches currently available categories from the selected channel playlist.
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
- [FFmpeg](https://ffmpeg.org/): For audio decoding.
- [oto](https://github.com/hajimehoshi/oto): For low-level audio playback.
- [go-tui](https://github.com/grindlemire/go-tui): For the terminal UI framework.

## 📦 Installation

### Download from Releases

1. Open the [Releases page](https://github.com/KidiXDev/lofi-radio/releases/latest).
2. Download the asset for your platform:
   - `lofi-radio_Windows_x86_64.zip` or `lofi-radio_Windows_arm64.zip`
   - `lofi-radio_Linux_x86_64.tar.gz` or `lofi-radio_Linux_arm64.tar.gz`
3. Extract the binary:
   - Windows: `lofi.exe`
   - Linux: `lofi`
4. Put it in a directory available in your `PATH`.

### One-line installer

- Windows (PowerShell):
  ```powershell
  irm https://raw.githubusercontent.com/KidiXDev/lofi-radio/main/scripts/install.ps1 | iex
  ```
- Linux:
  ```bash
  curl -fsSL https://raw.githubusercontent.com/KidiXDev/lofi-radio/main/scripts/install.sh | bash
  ```

Installer behavior:
- Downloads the latest release for your OS/architecture.
- Installs into a user-safe directory:
  - Windows: `%LOCALAPPDATA%\lofi-radio\bin`
  - Linux: `~/.local/bin/lofi-radio`
- Adds the install directory to `PATH` for future shells.
- Then you can run: `lofi`

## 🔄 Updates

- Check for updates:
  ```bash
  lofi -check-update
  ```
- Install latest update from GitHub Releases:
  ```bash
  lofi -update
  ```
- Optional custom install location during update:
  ```bash
  lofi -update -update-install-dir "/custom/path"
  ```

## 🗺️ Roadmap

We are continuously working to make **Lofi Radio** the best terminal-based lofi player. Here is what we have planned:

- [x] **TUI Category Picker**: Add a screen to switch between different lofi providers seamlessly.
- [x] **Volume Control**: Native volume adjustment within the TUI.
- [x] **Visualizers**: Add an ASCII-based audio visualizer for that extra retro feel.
- [ ] **Favorites**: Bookmark specific tracks or streams.
- [ ] **Sleep Timer**: Automatically stop playback after a set duration.

and more to come !

## 📜 License

Distributed under the [GNU General Public License v3.0](LICENSE).

