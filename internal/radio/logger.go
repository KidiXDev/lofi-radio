package radio

import (
	"fmt"
	"os"
	"sync"
	"time"
)

var logMu sync.Mutex

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

