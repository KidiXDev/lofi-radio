package bootstrap

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kidixdev/lofi-radio/internal/config"
)

const (
	ytDlpWinURL      = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe"
	ytDlpWinArm64URL = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_arm64.exe"
	ytDlpLinuxURL    = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_linux"
	ytDlpLinuxArmURL = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_linux_aarch64"

	ffmpegWinURL      = "https://github.com/yt-dlp/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl.zip"
	ffmpegWinArm64URL = "https://github.com/yt-dlp/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-winarm64-gpl.zip"
	ffmpegLinuxURL    = "https://github.com/yt-dlp/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-linux64-gpl.tar.xz"
	ffmpegLinuxArmURL = "https://github.com/yt-dlp/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-linuxarm64-gpl.tar.xz"
)

var downloadHTTPClient = &http.Client{
	Timeout: 10 * time.Minute,
}

var errBinDirFound = errors.New("bin-dir-found")

type BinaryPaths struct {
	YtDlp  string
	FFplay string
}

func EnsureDependencies() (BinaryPaths, error) {
	return EnsureDependenciesWithProgress(nil)
}

func EnsureDependenciesWithProgress(reporter ProgressReporter) (BinaryPaths, error) {
	ytDlpPath, err := resolveYtDlpPath(reporter)
	if err != nil {
		return BinaryPaths{}, err
	}

	ffplayPath, err := resolveFFplayPath(reporter)
	if err != nil {
		return BinaryPaths{}, err
	}

	config.SetBinaryPaths(ytDlpPath, ffplayPath)
	emitStatus(reporter, "dependencies", "Ready")

	return BinaryPaths{
		YtDlp:  ytDlpPath,
		FFplay: ffplayPath,
	}, nil
}

func resolveYtDlpPath(reporter ProgressReporter) (string, error) {
	if hostPath := findExecutableInPath("yt-dlp"); hostPath != "" {
		emitStatus(reporter, "yt-dlp", "Using system binary")
		return hostPath, nil
	}

	localPath := config.YtDlpPath()
	if ensureRunnableFile(localPath) {
		emitStatus(reporter, "yt-dlp", "Using local cached binary")
		return localPath, nil
	}

	downloadURL, err := ytDlpDownloadURL()
	if err != nil {
		return "", err
	}

	emitStatus(reporter, "yt-dlp", "Downloading binary")
	if err := downloadBinary(downloadURL, localPath, "yt-dlp", reporter); err != nil {
		return "", fmt.Errorf("ensure yt-dlp binary: %w", err)
	}

	emitStatus(reporter, "yt-dlp", "Binary downloaded")
	return localPath, nil
}

func resolveFFplayPath(reporter ProgressReporter) (string, error) {
	if hostFFplay := findExecutableInPath("ffplay"); hostFFplay != "" {
		emitStatus(reporter, "ffplay", "Using system binary")
		return hostFFplay, nil
	}

	hostFFmpeg := findExecutableInPath("ffmpeg")
	if hostFFmpeg != "" {
		hostFFplayFromFFmpeg := filepath.Join(filepath.Dir(hostFFmpeg), ffplayFileName())
		if isRunnableFile(hostFFplayFromFFmpeg) {
			emitStatus(reporter, "ffplay", "Using ffplay from system ffmpeg")
			return hostFFplayFromFFmpeg, nil
		}
	}

	localFFplayPath := config.FFplayPath()
	if ensureRunnableFile(localFFplayPath) {
		emitStatus(reporter, "ffplay", "Using local cached binary")
		return localFFplayPath, nil
	}

	emitStatus(reporter, "ffplay", "Downloading ffmpeg bundle")
	if err := ensurePortableFFmpeg(filepath.Dir(localFFplayPath), reporter); err != nil {
		return "", fmt.Errorf("ensure ffmpeg/ffplay binaries: %w", err)
	}

	if !isRunnableFile(localFFplayPath) {
		return "", fmt.Errorf("ffplay binary not found after download: %s", localFFplayPath)
	}

	emitStatus(reporter, "ffplay", "Binary downloaded")
	return localFFplayPath, nil
}

func ytDlpDownloadURL() (string, error) {
	switch runtime.GOOS {
	case "windows":
		switch runtime.GOARCH {
		case "amd64":
			return ytDlpWinURL, nil
		case "arm64":
			return ytDlpWinArm64URL, nil
		default:
			return "", fmt.Errorf("unsupported windows architecture for yt-dlp: %s", runtime.GOARCH)
		}
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			return ytDlpLinuxURL, nil
		case "arm64":
			return ytDlpLinuxArmURL, nil
		default:
			return "", fmt.Errorf("unsupported linux architecture for yt-dlp: %s", runtime.GOARCH)
		}
	default:
		return "", fmt.Errorf("unsupported OS for yt-dlp bootstrap: %s", runtime.GOOS)
	}
}

func ffmpegDownloadURL() (string, error) {
	switch runtime.GOOS {
	case "windows":
		switch runtime.GOARCH {
		case "amd64":
			return ffmpegWinURL, nil
		case "arm64":
			return ffmpegWinArm64URL, nil
		default:
			return "", fmt.Errorf("unsupported windows architecture for ffmpeg: %s", runtime.GOARCH)
		}
	case "linux":
		switch runtime.GOARCH {
		case "amd64":
			return ffmpegLinuxURL, nil
		case "arm64":
			return ffmpegLinuxArmURL, nil
		default:
			return "", fmt.Errorf("unsupported linux architecture for ffmpeg: %s", runtime.GOARCH)
		}
	default:
		return "", fmt.Errorf("unsupported OS for ffmpeg bootstrap: %s", runtime.GOOS)
	}
}

func ensurePortableFFmpeg(destinationDir string, reporter ProgressReporter) error {
	if err := os.MkdirAll(destinationDir, 0o755); err != nil {
		return fmt.Errorf("create ffmpeg directory: %w", err)
	}

	downloadURL, err := ffmpegDownloadURL()
	if err != nil {
		return err
	}

	tempArchive, err := os.CreateTemp("", ffmpegArchivePattern())
	if err != nil {
		return fmt.Errorf("create ffmpeg temp file: %w", err)
	}
	tempArchivePath := tempArchive.Name()
	if err := tempArchive.Close(); err != nil {
		return fmt.Errorf("close ffmpeg temp file: %w", err)
	}
	if err := os.Remove(tempArchivePath); err != nil {
		return fmt.Errorf("remove ffmpeg placeholder temp file: %w", err)
	}
	defer os.Remove(tempArchivePath)

	if err := downloadBinary(downloadURL, tempArchivePath, "ffmpeg", reporter); err != nil {
		return fmt.Errorf("download ffmpeg archive: %w", err)
	}

	if runtime.GOOS == "windows" {
		return extractFFmpegFromZIP(tempArchivePath, destinationDir)
	}

	return extractFFmpegFromTarXZ(tempArchivePath, destinationDir)
}

func ffmpegArchivePattern() string {
	if runtime.GOOS == "windows" {
		return "ffmpeg-*.zip"
	}

	return "ffmpeg-*.tar.xz"
}

func extractFFmpegFromZIP(archivePath, destinationDir string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open ffmpeg zip archive: %w", err)
	}
	defer reader.Close()

	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}

		relativeBinPath, ok := filePathFromBinDir(file.Name)
		if !ok {
			continue
		}

		if err := extractZipFile(file, destinationDir, relativeBinPath); err != nil {
			return err
		}
	}

	return nil
}

func extractZipFile(file *zip.File, destinationDir, relativePath string) error {
	source, err := file.Open()
	if err != nil {
		return fmt.Errorf("open zip file entry %q: %w", file.Name, err)
	}
	defer source.Close()

	targetPath, err := safeJoinPath(destinationDir, relativePath)
	if err != nil {
		return fmt.Errorf("invalid zip file path %q: %w", file.Name, err)
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", targetPath, err)
	}

	target, err := os.Create(targetPath)
	if err != nil {
		return fmt.Errorf("create target file %s: %w", targetPath, err)
	}

	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return fmt.Errorf("write target file %s: %w", targetPath, err)
	}

	if err := target.Close(); err != nil {
		return fmt.Errorf("close target file %s: %w", targetPath, err)
	}

	if runtime.GOOS != "windows" {
		_ = os.Chmod(targetPath, 0o755)
	}

	return nil
}

func extractFFmpegFromTarXZ(archivePath, destinationDir string) error {
	tempExtractDir, err := os.MkdirTemp("", "ffmpeg-extract-*")
	if err != nil {
		return fmt.Errorf("create temporary extraction directory: %w", err)
	}
	defer os.RemoveAll(tempExtractDir)

	cmd := exec.Command("tar", "-xJf", archivePath, "-C", tempExtractDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		var missingToolErr *exec.Error
		if errors.As(err, &missingToolErr) && errors.Is(missingToolErr.Err, exec.ErrNotFound) {
			return fmt.Errorf("extract ffmpeg tar.xz archive: tar command not found on host")
		}

		return fmt.Errorf("extract ffmpeg tar.xz archive: %w (%s)", err, strings.TrimSpace(string(output)))
	}

	return copyBinFolderContents(tempExtractDir, destinationDir)
}

func copyBinFolderContents(extractedRootDir, destinationDir string) error {
	binDir, err := findExtractedBinDir(extractedRootDir)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(binDir)
	if err != nil {
		return fmt.Errorf("read extracted bin directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		sourcePath := filepath.Join(binDir, entry.Name())
		targetPath := filepath.Join(destinationDir, entry.Name())

		if err := copyFile(sourcePath, targetPath); err != nil {
			return err
		}

		if runtime.GOOS != "windows" {
			_ = os.Chmod(targetPath, 0o755)
		}
	}

	return nil
}

func findExtractedBinDir(rootDir string) (string, error) {
	var binDir string

	err := filepath.WalkDir(rootDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if !entry.IsDir() {
			return nil
		}

		if entry.Name() != "bin" {
			return nil
		}

		binDir = path
		return errBinDirFound
	})
	if err != nil && !errors.Is(err, errBinDirFound) {
		return "", fmt.Errorf("search extracted ffmpeg directory: %w", err)
	}

	if binDir == "" {
		return "", fmt.Errorf("bin directory not found in extracted ffmpeg archive")
	}

	return binDir, nil
}

func filePathFromBinDir(archivePath string) (string, bool) {
	normalized := filepath.ToSlash(archivePath)
	const marker = "/bin/"
	markerIndex := strings.LastIndex(normalized, marker)
	if markerIndex < 0 {
		return "", false
	}

	relative := normalized[markerIndex+len(marker):]
	relative = strings.TrimSpace(relative)
	if relative == "" {
		return "", false
	}

	return filepath.FromSlash(relative), true
}

func safeJoinPath(baseDir, relativePath string) (string, error) {
	base := filepath.Clean(baseDir)
	target := filepath.Clean(filepath.Join(base, relativePath))
	if target == base {
		return target, nil
	}

	if !strings.HasPrefix(target, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("target path escapes destination: %s", target)
	}

	return target, nil
}

func downloadBinary(url, destination, component string, reporter ProgressReporter) error {
	response, err := downloadHTTPClient.Get(url)
	if err != nil {
		return fmt.Errorf("download request failed: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download request returned status %s", response.Status)
	}

	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create destination directory: %w", err)
	}

	tempDestination := destination + ".download"
	target, err := os.Create(tempDestination)
	if err != nil {
		return fmt.Errorf("create temporary destination file: %w", err)
	}

	startedAt := time.Now()
	lastReportAt := startedAt
	totalBytes := response.ContentLength
	var writtenBytes int64
	buffer := make([]byte, 64*1024)

	reportDownload := func(done bool) {
		elapsed := time.Since(startedAt)
		elapsedSeconds := elapsed.Seconds()
		if elapsedSeconds < 0.001 {
			elapsedSeconds = 0.001
		}

		speed := float64(writtenBytes) / elapsedSeconds
		var eta time.Duration
		if totalBytes > 0 && writtenBytes < totalBytes && speed > 0 {
			remaining := float64(totalBytes-writtenBytes) / speed
			eta = time.Duration(remaining * float64(time.Second))
		}

		emitDownload(reporter, component, DownloadProgress{
			BytesReceived: writtenBytes,
			TotalBytes:    totalBytes,
			SpeedPerSec:   speed,
			ETA:           eta,
			Done:          done,
		})
	}

	for {
		readCount, readErr := response.Body.Read(buffer)
		if readCount > 0 {
			writeCount, writeErr := target.Write(buffer[:readCount])
			if writeErr != nil {
				_ = target.Close()
				return fmt.Errorf("write downloaded binary: %w", writeErr)
			}
			if writeCount != readCount {
				_ = target.Close()
				return fmt.Errorf("write downloaded binary: short write")
			}

			writtenBytes += int64(writeCount)
			if time.Since(lastReportAt) >= 120*time.Millisecond {
				reportDownload(false)
				lastReportAt = time.Now()
			}
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = target.Close()
			return fmt.Errorf("read download stream: %w", readErr)
		}
	}

	if err := target.Close(); err != nil {
		return fmt.Errorf("close downloaded binary: %w", err)
	}

	reportDownload(true)

	if runtime.GOOS != "windows" {
		if err := os.Chmod(tempDestination, 0o755); err != nil {
			return fmt.Errorf("chmod downloaded binary: %w", err)
		}
	}

	if _, err := os.Stat(destination); err == nil {
		if err := os.Remove(destination); err != nil {
			_ = os.Remove(tempDestination)
			return fmt.Errorf("remove existing destination file: %w", err)
		}
	} else if !os.IsNotExist(err) {
		_ = os.Remove(tempDestination)
		return fmt.Errorf("stat destination file: %w", err)
	}

	if err := os.Rename(tempDestination, destination); err != nil {
		_ = os.Remove(tempDestination)
		return fmt.Errorf("move downloaded binary into place: %w", err)
	}

	return nil
}

func copyFile(sourcePath, targetPath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open source file %s: %w", sourcePath, err)
	}
	defer source.Close()

	target, err := os.Create(targetPath)
	if err != nil {
		return fmt.Errorf("create target file %s: %w", targetPath, err)
	}

	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return fmt.Errorf("copy %s to %s: %w", sourcePath, targetPath, err)
	}

	if err := target.Close(); err != nil {
		return fmt.Errorf("close target file %s: %w", targetPath, err)
	}

	return nil
}

func findExecutableInPath(name string) string {
	resolvedPath, err := exec.LookPath(name)
	if err != nil {
		return ""
	}

	return resolvedPath
}

func isRunnableFile(path string) bool {
	fileInfo, err := os.Stat(path)
	if err != nil || fileInfo.IsDir() {
		return false
	}

	if runtime.GOOS == "windows" {
		return true
	}

	return fileInfo.Mode()&0o111 != 0
}

func ensureRunnableFile(path string) bool {
	fileInfo, err := os.Stat(path)
	if err != nil || fileInfo.IsDir() {
		return false
	}

	if runtime.GOOS == "windows" {
		return true
	}

	if fileInfo.Mode()&0o111 != 0 {
		return true
	}

	if err := os.Chmod(path, 0o755); err != nil {
		return false
	}

	refreshedInfo, err := os.Stat(path)
	if err != nil {
		return false
	}

	return refreshedInfo.Mode()&0o111 != 0
}

func emitStatus(reporter ProgressReporter, component, message string) {
	if reporter == nil {
		return
	}

	reporter.ReportProgress(ProgressEvent{
		Type:      ProgressEventStatus,
		Component: component,
		Message:   message,
	})
}

func emitDownload(reporter ProgressReporter, component string, progress DownloadProgress) {
	if reporter == nil {
		return
	}

	reporter.ReportProgress(ProgressEvent{
		Type:      ProgressEventDownload,
		Component: component,
		Download:  progress,
	})
}

func ffplayFileName() string {
	if runtime.GOOS == "windows" {
		return "ffplay.exe"
	}

	return "ffplay"
}
