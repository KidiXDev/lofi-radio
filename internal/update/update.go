package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/kidixdev/lofi-radio/internal/version"
)

const (
	requestTimeout  = 30 * time.Second
	downloadTimeout = 15 * time.Minute
)

type Asset struct {
	Name string
	URL  string
}

type ReleaseInfo struct {
	TagName string
	HTMLURL string
	Asset   Asset
}

type githubRelease struct {
	TagName string        `json:"tag_name"`
	HTMLURL string        `json:"html_url"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

func DefaultInstallDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}

	if runtime.GOOS == "windows" {
		return filepath.Join(homeDir, "AppData", "Local", "lofi-radio", "bin"), nil
	}
	return filepath.Join(homeDir, ".local", "bin"), nil
}

func CheckLatest(currentVersion string) (ReleaseInfo, bool, error) {
	release, err := latestRelease()
	if err != nil {
		return ReleaseInfo{}, false, err
	}
	if !isNewerVersion(currentVersion, release.TagName) {
		return release, false, nil
	}
	return release, true, nil
}

func SelfUpdate(currentVersion, installDir string) (ReleaseInfo, bool, string, error) {
	release, isNewer, err := CheckLatest(currentVersion)
	if err != nil {
		return ReleaseInfo{}, false, "", err
	}
	if !isNewer {
		return release, false, "", nil
	}

	if strings.TrimSpace(installDir) == "" {
		installDir, err = DefaultInstallDir()
		if err != nil {
			return ReleaseInfo{}, false, "", err
		}
	}

	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return ReleaseInfo{}, true, "", fmt.Errorf("create install directory: %w", err)
	}

	executablePath, err := downloadAndInstall(release.Asset, installDir)
	if err != nil {
		return ReleaseInfo{}, true, "", err
	}

	return release, true, executablePath, nil
}

func latestRelease() (ReleaseInfo, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", version.Owner, version.Repo)
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return ReleaseInfo{}, fmt.Errorf("create release request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "lofi-radio-updater")

	client := &http.Client{Timeout: requestTimeout}
	response, err := client.Do(request)
	if err != nil {
		return ReleaseInfo{}, fmt.Errorf("request latest release: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		bodyText := strings.TrimSpace(string(body))
		if err := githubRateLimitError(response, bodyText); err != nil {
			return ReleaseInfo{}, err
		}
		return ReleaseInfo{}, fmt.Errorf("latest release request failed: %s (%s)", response.Status, bodyText)
	}

	var release githubRelease
	if err := json.NewDecoder(response.Body).Decode(&release); err != nil {
		return ReleaseInfo{}, fmt.Errorf("decode latest release response: %w", err)
	}

	asset, err := selectAsset(release.Assets)
	if err != nil {
		return ReleaseInfo{}, err
	}

	return ReleaseInfo{
		TagName: release.TagName,
		HTMLURL: release.HTMLURL,
		Asset:   asset,
	}, nil
}

func githubRateLimitError(response *http.Response, body string) error {
	if response == nil {
		return nil
	}

	if response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusTooManyRequests {
		return nil
	}

	remaining := strings.TrimSpace(response.Header.Get("X-RateLimit-Remaining"))
	retryAfter := strings.TrimSpace(response.Header.Get("Retry-After"))
	resetAt := strings.TrimSpace(response.Header.Get("X-RateLimit-Reset"))

	if remaining == "0" || response.StatusCode == http.StatusTooManyRequests || strings.Contains(strings.ToLower(body), "rate limit") {
		reason := "github api rate limit reached"
		if retryAfter != "" {
			return fmt.Errorf("%s, retry after %ss", reason, retryAfter)
		}
		if resetAt != "" {
			if unixSec, err := strconv.ParseInt(resetAt, 10, 64); err == nil {
				resetTime := time.Unix(unixSec, 0).Local().Format(time.RFC3339)
				return fmt.Errorf("%s, resets at %s", reason, resetTime)
			}
			return fmt.Errorf("%s, reset token=%s", reason, resetAt)
		}
		return fmt.Errorf("%s", reason)
	}

	return nil
}

func selectAsset(assets []githubAsset) (Asset, error) {
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}

	target := fmt.Sprintf("lofi-radio_%s_%s%s", osLabel(runtime.GOOS), archLabel(runtime.GOARCH), ext)

	for _, item := range assets {
		if item.Name == target {
			return Asset{Name: item.Name, URL: item.URL}, nil
		}
	}

	return Asset{}, fmt.Errorf("no release asset found for %s/%s", runtime.GOOS, runtime.GOARCH)
}

func downloadAndInstall(asset Asset, installDir string) (string, error) {
	request, err := http.NewRequest(http.MethodGet, asset.URL, nil)
	if err != nil {
		return "", fmt.Errorf("create asset request: %w", err)
	}
	request.Header.Set("User-Agent", "lofi-radio-updater")

	client := &http.Client{Timeout: requestTimeout}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("download release asset: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return "", fmt.Errorf("release asset download failed: %s (%s)", response.Status, strings.TrimSpace(string(body)))
	}

	tempArchive, err := os.CreateTemp("", "lofi-radio-release-*")
	if err != nil {
		return "", fmt.Errorf("create temp archive: %w", err)
	}
	tempArchivePath := tempArchive.Name()
	defer os.Remove(tempArchivePath)

	if _, err := io.Copy(tempArchive, response.Body); err != nil {
		tempArchive.Close()
		return "", fmt.Errorf("write temp archive: %w", err)
	}
	if err := tempArchive.Close(); err != nil {
		return "", fmt.Errorf("close temp archive: %w", err)
	}

	if strings.HasSuffix(asset.Name, ".zip") {
		return extractZipBinary(tempArchivePath, installDir)
	}
	return extractTarGzBinary(tempArchivePath, installDir)
}

func extractZipBinary(zipPath, installDir string) (string, error) {
	archive, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("open zip archive: %w", err)
	}
	defer archive.Close()

	targetBaseName := binaryName()
	for _, file := range archive.File {
		if filepath.Base(file.Name) != targetBaseName {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			return "", fmt.Errorf("open binary entry from zip: %w", err)
		}
		defer reader.Close()
		return writeExecutable(reader, installDir)
	}

	return "", fmt.Errorf("binary %q not found in zip archive", targetBaseName)
}

func extractTarGzBinary(tarGzPath, installDir string) (string, error) {
	file, err := os.Open(tarGzPath)
	if err != nil {
		return "", fmt.Errorf("open tar.gz archive: %w", err)
	}
	defer file.Close()

	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return "", fmt.Errorf("open gzip reader: %w", err)
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	targetBaseName := binaryName()

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read tar archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(header.Name) != targetBaseName {
			continue
		}
		return writeExecutable(tarReader, installDir)
	}

	return "", fmt.Errorf("binary %q not found in tar.gz archive", targetBaseName)
}

func writeExecutable(source io.Reader, installDir string) (string, error) {
	targetPath := filepath.Join(installDir, binaryName())
	tempPath := targetPath + ".tmp"

	targetFile, err := os.OpenFile(tempPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", fmt.Errorf("open temp binary for writing: %w", err)
	}
	if _, err := io.Copy(targetFile, source); err != nil {
		targetFile.Close()
		return "", fmt.Errorf("write binary: %w", err)
	}
	if err := targetFile.Close(); err != nil {
		return "", fmt.Errorf("close binary: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tempPath, 0o755); err != nil {
			return "", fmt.Errorf("chmod binary: %w", err)
		}
	}
	if err := os.Rename(tempPath, targetPath); err != nil {
		// Windows can't replace a running .exe. If the target is in use,
		// stage the new binary and schedule replacement after this process exits.
		if runtime.GOOS == "windows" && errors.Is(err, os.ErrPermission) {
			fallbackPath := targetPath + ".new"
			if fallbackErr := os.Rename(tempPath, fallbackPath); fallbackErr != nil {
				return "", fmt.Errorf("move staged binary into place: %w", fallbackErr)
			}
			if scheduleErr := scheduleWindowsReplaceAfterExit(targetPath, fallbackPath, os.Getpid()); scheduleErr != nil {
				return "", fmt.Errorf("schedule post-exit replacement: %w", scheduleErr)
			}
			return targetPath, nil
		}
		return "", fmt.Errorf("move binary into place: %w", err)
	}
	return targetPath, nil
}

func scheduleWindowsReplaceAfterExit(targetPath, stagedPath string, pid int) error {
	if runtime.GOOS != "windows" {
		return nil
	}

	scriptFile, err := os.CreateTemp(os.TempDir(), "lofi-update-replace-*.bat")
	if err != nil {
		return fmt.Errorf("create update script: %w", err)
	}
	scriptPath := scriptFile.Name()
	script := windowsReplaceScript(pid)
	if _, err := scriptFile.WriteString(script); err != nil {
		scriptFile.Close()
		_ = os.Remove(scriptPath)
		return fmt.Errorf("write update script: %w", err)
	}
	if err := scriptFile.Close(); err != nil {
		_ = os.Remove(scriptPath)
		return fmt.Errorf("close update script: %w", err)
	}

	cmd := exec.Command("cmd", "/C", scriptPath)
	cmd.Env = append(
		os.Environ(),
		"LOFI_REPLACE_SRC="+stagedPath,
		"LOFI_REPLACE_DST="+targetPath,
	)
	if err := cmd.Start(); err != nil {
		_ = os.Remove(scriptPath)
		return fmt.Errorf("start update script: %w", err)
	}
	return nil
}

func windowsReplaceScript(pid int) string {
	pidValue := strconv.Itoa(pid)
	return "@echo off\r\n" +
		"setlocal\r\n" +
		"set \"PID=" + pidValue + "\"\r\n" +
		"set \"SRC=%LOFI_REPLACE_SRC%\"\r\n" +
		"set \"DST=%LOFI_REPLACE_DST%\"\r\n" +
		"set \"COUNT=0\"\r\n" +
		":wait\r\n" +
		"tasklist /FI \"PID eq %PID%\" | find \"%PID%\" >nul\r\n" +
		"if not errorlevel 1 (\r\n" +
		"  timeout /t 1 /nobreak >nul\r\n" +
		"  goto wait\r\n" +
		")\r\n" +
		":retry\r\n" +
		"move /Y \"%SRC%\" \"%DST%\" >nul 2>nul\r\n" +
		"if not errorlevel 1 goto launch\r\n" +
		"set /a COUNT+=1\r\n" +
		"if %COUNT% GEQ 30 goto done\r\n" +
		"timeout /t 1 /nobreak >nul\r\n" +
		"goto retry\r\n" +
		":launch\r\n" +
		"if exist \"%DST%\" start \"\" \"%DST%\" >nul 2>nul\r\n" +
		":done\r\n" +
		"del \"%~f0\" >nul 2>nul\r\n" +
		"endlocal\r\n"
}

func StartUpdatedProcess(executablePath string) error {
	path := strings.TrimSpace(executablePath)
	if path == "" {
		return fmt.Errorf("empty executable path")
	}

	// On Windows, if a staged .new exists, replacement is deferred and the
	// replacement script will launch the new executable after swapping files.
	if runtime.GOOS == "windows" {
		if _, err := os.Stat(path + ".new"); err == nil {
			return nil
		}
	}

	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start updated process: %w", err)
	}
	return nil
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "lofi.exe"
	}
	return "lofi"
}

func osLabel(goos string) string {
	switch goos {
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	default:
		return strings.ToUpper(goos)
	}
}

func archLabel(arch string) string {
	switch arch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "arm64"
	default:
		return arch
	}
}

func isNewerVersion(current, latest string) bool {
	currentParts := normalizeVersion(current)
	latestParts := normalizeVersion(latest)

	if len(currentParts) == 0 || len(latestParts) == 0 {
		return current != latest
	}

	for i := range 3 {
		if latestParts[i] > currentParts[i] {
			return true
		}
		if latestParts[i] < currentParts[i] {
			return false
		}
	}
	return false
}

func normalizeVersion(value string) []int {
	trimmed := strings.TrimPrefix(strings.TrimSpace(value), "v")
	if trimmed == "" {
		return nil
	}

	prereleaseSplit := strings.SplitN(trimmed, "-", 2)
	core := prereleaseSplit[0]
	parts := strings.Split(core, ".")

	out := []int{0, 0, 0}
	for i := 0; i < len(parts) && i < 3; i++ {
		number, err := strconv.Atoi(parts[i])
		if err != nil {
			return nil
		}
		out[i] = number
	}
	return out
}
