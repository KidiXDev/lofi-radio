package radio

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/kidixdev/lofi-radio/internal/config"
)

const maxLogBytes = 5 << 20

var logMu sync.Mutex
var crashLogFile *os.File
var logPath = filepath.Join(config.AppDir(), "logger.log")

func writeLog(format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	line := fmt.Sprintf(format, args...)
	ts := time.Now().Format(time.RFC3339Nano)
	_, _ = fmt.Fprintf(f, "%s %s\n", ts, line)
}

func Logf(format string, args ...any) {
	writeLog(format, args...)
}

func InitCrashLogging() {
	logMu.Lock()
	defer logMu.Unlock()

	if crashLogFile != nil {
		return
	}

	flags := os.O_APPEND | os.O_CREATE | os.O_WRONLY
	if info, err := os.Stat(logPath); err == nil && info.Size() > maxLogBytes {
		flags |= os.O_TRUNC // ponytail: truncate, not rotate; keep a .old copy if crash history matters
	}
	f, err := os.OpenFile(logPath, flags, 0o644)
	if err != nil {
		return
	}

	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		_ = f.Close()
		return
	}

	crashLogFile = f
}
