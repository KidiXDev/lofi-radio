package uninstall

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/kidixdev/lofi-radio/internal/config"
)

type Result struct {
	ExecutablePath string
	AppDir         string
	Deferred       bool
	PathUpdated    bool
}

func Run() (Result, error) {
	executablePath, err := os.Executable()
	if err != nil {
		return Result{}, fmt.Errorf("resolve executable path: %w", err)
	}

	executableDir := filepath.Dir(executablePath)
	pathUpdated := removePathReference(executableDir)
	if err := removeAppData(executableDir); err != nil {
		return Result{}, err
	}

	if runtime.GOOS == "windows" {
		if err := scheduleWindowsUninstall(executablePath, executableDir, os.Getpid()); err != nil {
			return Result{}, err
		}
		return Result{
			ExecutablePath: executablePath,
			AppDir:         executableDir,
			Deferred:       true,
			PathUpdated:    pathUpdated,
		}, nil
	}

	if err := os.Remove(executablePath); err != nil && !os.IsNotExist(err) {
		return Result{}, fmt.Errorf("remove executable %q: %w", executablePath, err)
	}
	_ = os.Remove(executableDir) // only succeeds when it was a dedicated, now empty, install dir

	return Result{
		ExecutablePath: executablePath,
		AppDir:         executableDir,
		Deferred:       false,
		PathUpdated:    pathUpdated,
	}, nil
}

// removeAppData deletes only what the app creates next to its binary. The binary
// may live in $HOME or a shared bin dir, so whole directories are never wiped.
func removeAppData(dir string) error {
	owned := []string{
		filepath.Join(dir, ".cache", "categories"),
		filepath.Join(dir, ".cache", "permission-check"),
		config.YtDlpPath(),
		config.YtDlpPath() + ".download",
		filepath.Dir(config.FFmpegPath()),
		filepath.Join(dir, "config.yaml"),
		filepath.Join(dir, "config.yaml.tmp"),
		filepath.Join(dir, "logger.log"),
	}
	for _, path := range owned {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %q: %w", path, err)
		}
	}
	// Drop parents only if that left them empty; os.Remove refuses non-empty dirs.
	for _, path := range []string{
		filepath.Dir(config.YtDlpPath()),
		filepath.Dir(filepath.Dir(config.FFmpegPath())),
		filepath.Join(dir, "bin"),
		filepath.Join(dir, ".cache"),
	} {
		_ = os.Remove(path)
	}
	return nil
}

func scheduleWindowsUninstall(executablePath, executableDir string, pid int) error {
	batFile, err := os.CreateTemp(os.TempDir(), "lofi-uninstall-*.bat")
	if err != nil {
		return fmt.Errorf("create uninstall script: %w", err)
	}
	batPath := batFile.Name()

	script := windowsUninstallScript(pid)
	if _, err := batFile.WriteString(script); err != nil {
		batFile.Close()
		_ = os.Remove(batPath)
		return fmt.Errorf("write uninstall script: %w", err)
	}
	if err := batFile.Close(); err != nil {
		_ = os.Remove(batPath)
		return fmt.Errorf("close uninstall script: %w", err)
	}

	cmd := exec.Command("cmd", "/C", batPath)
	cmd.Env = append(
		os.Environ(),
		"LOFI_UNINSTALL_EXE="+executablePath,
		"LOFI_UNINSTALL_DIR="+executableDir,
	)
	if err := cmd.Start(); err != nil {
		_ = os.Remove(batPath)
		return fmt.Errorf("schedule windows uninstall: %w", err)
	}
	return nil
}

func removePathReference(executableDir string) bool {
	if runtime.GOOS == "windows" {
		return removeWindowsUserPathEntry(executableDir)
	}
	return removeUnixPathExports(executableDir)
}

func removeWindowsUserPathEntry(executableDir string) bool {
	// Only undo what install.ps1 did; a shared dir like C:\tools stays on PATH.
	installerDir := filepath.Join(os.Getenv("LOCALAPPDATA"), "lofi-radio", "bin")
	if !strings.EqualFold(filepath.Clean(executableDir), installerDir) {
		return false
	}
	// Update user PATH persistently with exact-segment removal.
	ps := `$target = $env:TARGET_DIR; ` +
		`$path = [Environment]::GetEnvironmentVariable("Path","User"); ` +
		`if ($null -eq $path) { exit 0 }; ` +
		`$parts = $path -split ';' | Where-Object { $_ -ne "" }; ` +
		`$normalizedTarget = ([IO.Path]::GetFullPath($target)).TrimEnd('\'); ` +
		`$filtered = @(); ` +
		`foreach ($p in $parts) { ` +
		`  try { $n = ([IO.Path]::GetFullPath($p)).TrimEnd('\') } catch { $n = $p.TrimEnd('\') }; ` +
		`  if (-not $n.Equals($normalizedTarget, [System.StringComparison]::OrdinalIgnoreCase)) { $filtered += $p } ` +
		`}; ` +
		`$newPath = ($filtered -join ';'); ` +
		`if ($newPath -ne $path) { [Environment]::SetEnvironmentVariable("Path", $newPath, "User") }`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
	cmd.Env = append(os.Environ(), "TARGET_DIR="+executableDir)
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

func removeUnixPathExports(executableDir string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}

	profiles := []string{
		filepath.Join(home, ".profile"),
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".config", "fish", "config.fish"),
	}

	updatedAny := false
	for _, profile := range profiles {
		updated, err := stripPathLines(profile, executableDir)
		if err == nil && updated {
			updatedAny = true
		}
	}
	return updatedAny
}

func stripPathLines(profilePath, executableDir string) (bool, error) {
	content, err := os.ReadFile(profilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	lines := strings.Split(string(content), "\n")
	changed := false
	filtered := make([]string, 0, len(lines))

	for _, line := range lines {
		if shouldStripPathLine(line, executableDir) {
			changed = true
			continue
		}
		filtered = append(filtered, line)
	}

	if !changed {
		return false, nil
	}

	output := strings.Join(filtered, "\n")
	tempFile, err := os.CreateTemp(filepath.Dir(profilePath), ".profile-tmp-*")
	if err != nil {
		return false, err
	}
	tempPath := tempFile.Name()
	if _, err := tempFile.WriteString(output); err != nil {
		tempFile.Close()
		_ = os.Remove(tempPath)
		return false, err
	}
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempPath)
		return false, err
	}
	if err := os.Chmod(tempPath, 0o644); err != nil {
		_ = os.Remove(tempPath)
		return false, err
	}
	if err := os.Rename(tempPath, profilePath); err != nil {
		_ = os.Remove(tempPath)
		return false, err
	}
	return true, nil
}

// shouldStripPathLine matches only the exact lines install.sh writes, so a user's
// own PATH line that merely mentions the dir (with other entries) is left alone.
func shouldStripPathLine(line, dir string) bool {
	switch strings.TrimSpace(line) {
	case `export PATH="` + dir + `:$PATH"`,
		`export PATH="$PATH:` + dir + `"`,
		`set -gx PATH "` + dir + `" $PATH`:
		return true
	}
	return false
}

func windowsUninstallScript(pid int) string {
	pidValue := strconv.Itoa(pid)
	return "@echo off\r\n" +
		"setlocal\r\n" +
		"set \"PID=" + pidValue + "\"\r\n" +
		"set \"EXE=%LOFI_UNINSTALL_EXE%\"\r\n" +
		"set \"DIR=%LOFI_UNINSTALL_DIR%\"\r\n" +
		"cd /d \"%TEMP%\"\r\n" +
		"set \"COUNT=0\"\r\n" +
		":wait\r\n" +
		"tasklist /FI \"PID eq %PID%\" | find \"%PID%\" >nul\r\n" +
		"if not errorlevel 1 (\r\n" +
		"  timeout /t 1 /nobreak >nul\r\n" +
		"  goto wait\r\n" +
		")\r\n" +
		":retryexe\r\n" +
		"del /F /Q \"%EXE%\" >nul 2>nul\r\n" +
		"if not exist \"%EXE%\" goto deldir\r\n" +
		"set /a COUNT+=1\r\n" +
		"if %COUNT% GEQ 30 goto fail\r\n" +
		"timeout /t 1 /nobreak >nul\r\n" +
		"goto retryexe\r\n" +
		":deldir\r\n" +
		"rmdir \"%DIR%\" >nul 2>nul\r\n" +
		":fail\r\n" +
		"del \"%~f0\" >nul 2>nul\r\n" +
		"endlocal\r\n"
}
