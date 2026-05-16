package radio

import (
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"
)

var logMu sync.Mutex
var crashLogFile *os.File

func writeLog(format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()

	f, err := os.OpenFile("logger.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
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

	f, err := os.OpenFile("logger.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}

	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		_ = f.Close()
		return
	}

	crashLogFile = f
}
