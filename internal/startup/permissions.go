package startup

import (
	"fmt"
	"os"
	"path/filepath"
)

func CheckExecutableCachePermissions() error {
	executablePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	executableDir := filepath.Dir(executablePath)
	cacheCheckDir := filepath.Join(executableDir, ".cache", "permission-check")
	if err := os.MkdirAll(cacheCheckDir, 0o755); err != nil {
		return fmt.Errorf("create cache check directory %q: %w", cacheCheckDir, err)
	}

	probeFile := filepath.Join(cacheCheckDir, "write-test.tmp")
	if err := os.WriteFile(probeFile, []byte("ok"), 0o644); err != nil {
		return fmt.Errorf("write cache probe file %q: %w", probeFile, err)
	}

	if err := os.Remove(probeFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cleanup cache probe file %q: %w", probeFile, err)
	}
	if err := os.Remove(cacheCheckDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cleanup cache check directory %q: %w", cacheCheckDir, err)
	}

	return nil
}
