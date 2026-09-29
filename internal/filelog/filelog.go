// Package filelog provides a minimal file-based debug logger.
//
// Hard rule: the logger must NEVER write prompts, responses, tool I/O,
// developer identity, secrets, or tokens. Log statements carry IDs and
// counts only.
package filelog

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

var (
	logger *log.Logger
	once   sync.Once
)

func getLogger() *log.Logger {
	once.Do(func() {
		var logDir string
		if dataDir := os.Getenv("CLAUDE_PLUGIN_DATA"); dataDir != "" {
			logDir = dataDir
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				home = os.Getenv("HOME")
			}
			logDir = filepath.Join(home, ".fiddler")
		}
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			// Fall back to stderr if we can't create the log dir.
			logger = log.New(os.Stderr, "fiddler-plugin: ", log.LstdFlags)
			return
		}
		f, err := os.OpenFile(
			filepath.Join(logDir, "plugin.log"),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY,
			0o644,
		)
		if err != nil {
			logger = log.New(os.Stderr, "fiddler-plugin: ", log.LstdFlags)
			return
		}
		logger = log.New(f, "", log.LstdFlags)
	})
	return logger
}

// Info logs an informational message. Arguments are formatted like fmt.Sprintf.
// NEVER pass content (prompts, responses, tool I/O, secrets) to this function.
func Info(format string, args ...any) {
	// Logging is best-effort; a write failure has no meaningful recovery path.
	_ = getLogger().Output(2, fmt.Sprintf("INFO "+format, args...))
}

// Warn logs a warning message.
func Warn(format string, args ...any) {
	_ = getLogger().Output(2, fmt.Sprintf("WARN "+format, args...))
}

// Error logs an error message.
func Error(format string, args ...any) {
	_ = getLogger().Output(2, fmt.Sprintf("ERROR "+format, args...))
}
