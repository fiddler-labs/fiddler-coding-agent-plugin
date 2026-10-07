// Package filelog provides a minimal file-based debug logger.
//
// Hard rule: the logger must NEVER write prompts, responses, tool I/O,
// developer identity, secrets, or tokens. Log statements carry IDs and
// counts only.
//
// Each call opens plugin.log, appends one line, and closes it again. A hook
// process logs only a handful of lines, so the extra open is negligible, and
// not holding the file open matters on Windows: an open file cannot be deleted
// there, which would block cleanup of the plugin data directory (and of test
// temp directories). O_APPEND keeps concurrent hook processes from
// overwriting each other's lines.
package filelog

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// mu serializes writes from goroutines in one process.
var mu sync.Mutex

// logDir returns the directory for plugin.log: CLAUDE_PLUGIN_DATA when set
// (Claude Code's per-plugin data directory), else ~/.fiddler.
func logDir() string {
	if dataDir := os.Getenv("CLAUDE_PLUGIN_DATA"); dataDir != "" {
		return dataDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".fiddler")
}

// write appends one formatted line to plugin.log, falling back to stderr when
// the log file can't be opened. Best effort: a write failure has no
// meaningful recovery path.
func write(level, format string, args ...any) {
	msg := fmt.Sprintf(level+" "+format, args...)

	mu.Lock()
	defer mu.Unlock()

	dir := logDir()
	if err := os.MkdirAll(dir, 0o755); err == nil {
		f, err := os.OpenFile(
			filepath.Join(dir, "plugin.log"),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY,
			0o644,
		)
		if err == nil {
			_ = log.New(f, "", log.LstdFlags).Output(3, msg)
			_ = f.Close()
			return
		}
	}
	_ = log.New(os.Stderr, "fiddler-plugin: ", log.LstdFlags).Output(3, msg)
}

// Info logs an informational message. Arguments are formatted like fmt.Sprintf.
// NEVER pass content (prompts, responses, tool I/O, secrets) to this function.
func Info(format string, args ...any) {
	write("INFO", format, args...)
}

// Warn logs a warning message.
func Warn(format string, args ...any) {
	write("WARN", format, args...)
}

// Error logs an error message.
func Error(format string, args ...any) {
	write("ERROR", format, args...)
}
