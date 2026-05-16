#!/usr/bin/env bash
set -euo pipefail

OWNER="KidiXDev"
REPO="lofi-radio"
API_URL="https://api.github.com/repos/${OWNER}/${REPO}/releases/latest"

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) ARCH_LABEL="x86_64" ;;
  aarch64|arm64) ARCH_LABEL="arm64" ;;
  *)
    echo "Unsupported architecture: $arch"
    exit 1
    ;;
esac

os="$(uname -s)"
case "$os" in
  Linux) OS_LABEL="Linux" ;;
  *)
    echo "Unsupported OS: $os"
    exit 1
    ;;
esac

asset="lofi-radio_${OS_LABEL}_${ARCH_LABEL}.tar.gz"

download_url="$(curl -fsSL "$API_URL" | grep '"browser_download_url"' | cut -d '"' -f 4 | grep "$asset" | head -n 1)"
if [ -z "$download_url" ]; then
  echo "Failed to find asset: $asset"
  exit 1
fi

install_dir="${HOME}/.local/bin"
mkdir -p "$install_dir"

tmp_archive="$(mktemp)"
trap 'rm -f "$tmp_archive"' EXIT
curl -fL "$download_url" -o "$tmp_archive"

tar -xzf "$tmp_archive" -C "$install_dir" lofi
chmod +x "$install_dir/lofi"

case ":${PATH}:" in
  *":${install_dir}:"*)
    ;;
  *)
    shell_rc="${HOME}/.bashrc"
    if [ -n "${ZSH_VERSION:-}" ]; then
      shell_rc="${HOME}/.zshrc"
    fi
    printf '\nexport PATH="%s:$PATH"\n' "$install_dir" >> "$shell_rc"
    echo "Added $install_dir to PATH in $shell_rc"
    ;;
esac

echo "Installed: $install_dir/lofi"
echo "Run: lofi"
