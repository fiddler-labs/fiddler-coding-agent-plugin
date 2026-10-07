// Package toolctx holds the per-tool-call context that stitches a tool's
// permission lifecycle across isolated hook processes. It is the per-tool-call
// sibling of turnctx (which holds the per-turn trace context).
//
// Each hook fires as a separate, short-lived process with no shared memory.
// PreToolUse writes pending/<session_id>/<tool_use_id>.json once; the call's
// resolution (PostToolUse / PostToolUseFailure / PermissionDenied) reads it to
// recover the pre-execution timestamp and permission mode, then deletes it.
// Any record still present at turn end is a call that never resolved live and
// is classified against the transcript.
//
// One file per tool_use_id is deliberate: Claude Code issues tool calls in
// parallel, so N concurrent hook processes each touch a distinct file. There
// is no cross-process read-modify-write on a shared file and therefore no
// lock — a shared per-session file would need one, and its contention could
// stall the developer's session.
//
// The on-disk directory is named "pending" because it holds records for calls
// whose outcome is still pending; the Go package is named for its role (the
// per-tool-call context), mirroring how turnctx stores under "context".
//
// All operations fail open: a missing or corrupt record degrades one span's
// permission detail but never blocks the session.
package toolctx

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/filelog"
)

// StaleCutoff bounds how long an unresolved pending record is kept before
// SweepStale reclaims it. It must be far larger than any plausible
// interrupt-then-resume gap (a user interrupting a turn, walking away, then
// returning to type) so the age-based sweep only reaps records from sessions
// that crashed or otherwise never came back.
const StaleCutoff = 24 * time.Hour

// Record is the per-call state captured at PreToolUse. Only the fields the
// resolution needs are stored; identity/decision/reason come from the
// id-carrying resolution events themselves.
type Record struct {
	ToolName       string `json:"tool_name"`
	ToolInput      string `json:"tool_input"`
	PermissionMode string `json:"permission_mode"`
	PreNano        string `json:"pre_unix_nano"`
}

// getDir returns the directory holding a session's pending records, creating
// it if necessary. Mirrors turnctx: prefers CLAUDE_PLUGIN_DATA, falls back to
// ~/.fiddler when it is unset (e.g. local testing).
func getDir(sessionID string) string {
	var base string
	if dataDir := os.Getenv("CLAUDE_PLUGIN_DATA"); dataDir != "" {
		base = dataDir
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.Getenv("HOME")
		}
		base = filepath.Join(home, ".fiddler")
	}
	dir := filepath.Join(base, "pending", safe(sessionID))
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// pathSegmentReplacer maps characters that are path separators or otherwise
// invalid in a file name on some OS to "_". "/" separates paths everywhere;
// "\\" is also a separator on Windows, and ":" is invalid in Windows file names
// (it would name an alternate data stream or a drive).
var pathSegmentReplacer = strings.NewReplacer("/", "_", "\\", "_", ":", "_")

// safe makes an id usable as a path segment on every OS.
func safe(id string) string {
	return pathSegmentReplacer.Replace(id)
}

// recPath returns the file path for one tool call's record.
func recPath(sessionID, toolUseID string) string {
	return filepath.Join(getDir(sessionID), safe(toolUseID)+".json")
}

// Write persists the record for a tool call. Called once at PreToolUse.
// Plain os.WriteFile, no lock: the file is unique to this tool_use_id, so no
// other process writes it concurrently.
func Write(sessionID, toolUseID string, r Record) error {
	if toolUseID == "" {
		return fmt.Errorf("pending: empty tool_use_id")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal pending record: %w", err)
	}
	if err := os.WriteFile(recPath(sessionID, toolUseID), data, 0o644); err != nil {
		return fmt.Errorf("write pending record: %w", err)
	}
	return nil
}

// Read returns the record for a tool call. The second return value is false
// when the record is absent or corrupt (fail open).
func Read(sessionID, toolUseID string) (Record, bool) {
	if toolUseID == "" {
		return Record{}, false
	}
	data, err := os.ReadFile(recPath(sessionID, toolUseID))
	if err != nil {
		return Record{}, false
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, false
	}
	return r, true
}

// Delete removes one tool call's record. Called at resolution. Fails open if
// the record is already gone.
func Delete(sessionID, toolUseID string) {
	if toolUseID == "" {
		return
	}
	if err := os.Remove(recPath(sessionID, toolUseID)); err != nil && !os.IsNotExist(err) {
		filelog.Warn("pending: could not remove record: %v", err)
	}
}

// List returns all pending records for a session keyed by tool_use_id. Used at
// turn end / interrupted-turn flush to classify calls that never resolved
// live. Corrupt entries are skipped (fail open).
func List(sessionID string) map[string]Record {
	out := make(map[string]Record)
	entries, err := os.ReadDir(getDir(sessionID))
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if r, ok := Read(sessionID, id); ok {
			out[id] = r
		}
	}
	return out
}

// SweepSession removes all pending records for a session (its whole
// directory). Called after the turn's leftover records have been classified and
// emitted (turn end / interrupted-turn flush), and at SessionEnd. Fails open.
func SweepSession(sessionID string) {
	if err := os.RemoveAll(getDir(sessionID)); err != nil {
		filelog.Warn("pending: could not sweep session: %v", err)
	}
}

// SweepStale removes pending records older than StaleCutoff across all
// sessions, reclaiming files left by sessions that crashed or never resumed.
// It is age-based so it never reaps a record an in-progress interrupted turn
// still needs. Fails open.
func SweepStale() {
	root := getDir("") // .../pending
	sessions, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-StaleCutoff)
	for _, s := range sessions {
		if !s.IsDir() {
			continue
		}
		sessionDir := filepath.Join(root, s.Name())
		files, err := os.ReadDir(sessionDir)
		if err != nil {
			continue
		}
		remaining := 0
		for _, f := range files {
			info, err := f.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				if err := os.Remove(filepath.Join(sessionDir, f.Name())); err != nil {
					filelog.Warn("pending: could not remove stale record: %v", err)
					remaining++
				}
			} else {
				remaining++
			}
		}
		if remaining == 0 {
			_ = os.Remove(sessionDir)
		}
	}
}
