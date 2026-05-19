package uninstall

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type Result struct {
	ExecutablePath string
	CacheDir       string
	Deferred       bool
	PathUpdated    bool
}

func Run() (Result, error) {
	executablePath, err := os.Executable()
	if err != nil {
		return Result{}, fmt.Errorf("resolve executable path: %w", err)
	}

	executableDir := filepath.Dir(executablePath)
	cacheDir := filepath.Join(executableDir, ".cache")
	pathUpdated := removePathReference(executableDir)

	if runtime.GOOS == "windows" {
		if err := scheduleWindowsUninstall(executablePath, cacheDir, os.Getpid()); err != nil {
			return Result{}, err
		}
		return Result{
			ExecutablePath: executablePath,
			CacheDir:       cacheDir,
			Deferred:       true,
			PathUpdated:    pathUpdated,
		}, nil
	}

	if err := os.Remove(executablePath); err != nil && !os.IsNotExist(err) {
		return Result{}, fmt.Errorf("remove executable %q: %w", executablePath, err)
	}
	if err := os.RemoveAll(cacheDir); err != nil {
		return Result{}, fmt.Errorf("remove cache directory %q: %w", cacheDir, err)
	}

	return Result{
		ExecutablePath: executablePath,
		CacheDir:       cacheDir,
		Deferred:       false,
		PathUpdated:    pathUpdated,
	}, nil
}

func scheduleWindowsUninstall(executablePath, cacheDir string, pid int) error {
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
		"LOFI_UNINSTALL_CACHE="+cacheDir,
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
	needle := normalizePathForCompare(executableDir)

	for _, line := range lines {
		normalizedLine := normalizePathForCompare(line)
		if shouldStripPathLine(normalizedLine, needle) {
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

func normalizePathForCompare(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")))
}

func shouldStripPathLine(normalizedLine, needle string) bool {
	trimmed := strings.TrimSpace(normalizedLine)
	if strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, needle) {
		return false
	}

	return strings.HasPrefix(trimmed, "export path=") ||
		strings.HasPrefix(trimmed, "path=") ||
		strings.HasPrefix(trimmed, "set path=")
}

func windowsUninstallScript(pid int) string {
	pidValue := strconv.Itoa(pid)
	return "@echo off\r\n" +
		"setlocal\r\n" +
		"set \"PID=" + pidValue + "\"\r\n" +
		"set \"EXE=%LOFI_UNINSTALL_EXE%\"\r\n" +
		"set \"CACHE=%LOFI_UNINSTALL_CACHE%\"\r\n" +
		"set \"COUNT=0\"\r\n" +
		":wait\r\n" +
		"tasklist /FI \"PID eq %PID%\" | find \"%PID%\" >nul\r\n" +
		"if not errorlevel 1 (\r\n" +
		"  timeout /t 1 /nobreak >nul\r\n" +
		"  goto wait\r\n" +
		")\r\n" +
		":retryexe\r\n" +
		"del /F /Q \"%EXE%\" >nul 2>nul\r\n" +
		"if not exist \"%EXE%\" goto delcache\r\n" +
		"set /a COUNT+=1\r\n" +
		"if %COUNT% GEQ 30 goto fail\r\n" +
		"timeout /t 1 /nobreak >nul\r\n" +
		"goto retryexe\r\n" +
		":delcache\r\n" +
		"if exist \"%CACHE%\" rmdir /S /Q \"%CACHE%\" >nul 2>nul\r\n" +
		":fail\r\n" +
		"del \"%~f0\" >nul 2>nul\r\n" +
		"endlocal\r\n"
}
