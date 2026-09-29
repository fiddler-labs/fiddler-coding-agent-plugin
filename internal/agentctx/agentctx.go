// Package agentctx holds the per-sub-agent context that lets a sub-agent's
// spans reuse the trace of the turn that launched it, across isolated hook
// processes. It is the per-sub-agent sibling of turnctx (per-turn trace
// context) and toolctx (per-tool-call permission context).
//
// The problem it solves is asynchronous sub-agents. Claude Code launches a
// sub-agent's Agent tool and acks it immediately (tool_response.status ==
// "async_launched"); the sub-agent then runs in the background, and its own
// tool PostToolUse hooks and its SubagentStop fire *after* the launching turn's
// Stop. By then turnctx was cleared, so turnctx.Load would lazily mint a *new*
// trace — and because span IDs are derived from the trace ID (otlp.SpanIDFrom),
// the sub-agent subtree would land in a different trace than the Agent launch
// span, breaking nesting (OTLP parent refs are within-trace only).
//
// agentctx bridges that gap: at the Agent launch the pipeline records
// agent_id -> launching turn's trace_id; the (possibly async) sub-agent tool
// hooks and SubagentStop read it to resolve the same trace the launch used.
// Only the trace_id is stored — the Agent-launch span id, the sub-agent root
// span id, and the turn root are all derivable from it via otlp.SpanIDFrom.
//
// One file per agent_id is deliberate (parallel sub-agents touch distinct
// files), so there is no cross-process read-modify-write and no lock, matching
// toolctx. Unlike toolctx, records are swept only at TurnStart (age-based) and
// SessionEnd, never at TurnEnd: an async sub-agent's record must outlive the
// turn that launched it. Each record is Deleted individually at its SubagentStop.
//
// All operations fail open: a missing or corrupt record degrades one
// sub-agent's nesting but never blocks the session.
package agentctx

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/filelog"
)

// StaleCutoff bounds how long a sub-agent record is kept before SweepStale
// reclaims it. It must be far larger than any plausible sub-agent lifetime so
// the age-based sweep only reaps records from sessions that crashed or whose
// SubagentStop never arrived. Matches toolctx.StaleCutoff.
const StaleCutoff = 24 * time.Hour

// Record maps a sub-agent to the trace of the turn that launched it. Only the
// trace id is needed: the Agent-launch span id A(agent_id), the sub-agent root
// span id S(agent_id), and the turn root are all derivable from it.
type Record struct {
	TraceID string `json:"trace_id"`
}

// getDir returns the directory holding a session's sub-agent records, creating
// it if necessary. It is a sibling of toolctx's "pending" dir, NOT under it:
// toolctx.SweepSession runs at TurnEnd and must not remove these records, which
// have to outlive the launching turn for async sub-agents. Prefers
// CLAUDE_PLUGIN_DATA, falls back to ~/.fiddler when it is unset (e.g. local
// testing), mirroring turnctx/toolctx.
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
	dir := filepath.Join(base, "agents", safe(sessionID))
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// safe makes an id usable as a path segment.
func safe(id string) string {
	return strings.ReplaceAll(id, "/", "_")
}

// recPath returns the file path for one sub-agent's record.
func recPath(sessionID, agentID string) string {
	return filepath.Join(getDir(sessionID), safe(agentID)+".json")
}

// Write persists the trace record for a sub-agent. Called at the Agent launch's
// PostToolUse. Plain os.WriteFile, no lock: the file is unique to this agent_id,
// so no other process writes it concurrently.
func Write(sessionID, agentID string, r Record) error {
	if agentID == "" {
		return fmt.Errorf("agentctx: empty agent_id")
	}
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal agent record: %w", err)
	}
	if err := os.WriteFile(recPath(sessionID, agentID), data, 0o644); err != nil {
		return fmt.Errorf("write agent record: %w", err)
	}
	return nil
}

// Read returns the trace record for a sub-agent. The second return value is
// false when the record is absent or corrupt (fail open).
func Read(sessionID, agentID string) (Record, bool) {
	if agentID == "" {
		return Record{}, false
	}
	data, err := os.ReadFile(recPath(sessionID, agentID))
	if err != nil {
		return Record{}, false
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return Record{}, false
	}
	return r, true
}

// Delete removes one sub-agent's record. Called at its SubagentStop. Fails open
// if the record is already gone.
func Delete(sessionID, agentID string) {
	if agentID == "" {
		return
	}
	if err := os.Remove(recPath(sessionID, agentID)); err != nil && !os.IsNotExist(err) {
		filelog.Warn("agentctx: could not remove record: %v", err)
	}
}

// SweepSession removes all sub-agent records for a session (its whole
// directory). Called at SessionEnd. Fails open.
func SweepSession(sessionID string) {
	if err := os.RemoveAll(getDir(sessionID)); err != nil {
		filelog.Warn("agentctx: could not sweep session: %v", err)
	}
}

// SweepStale removes sub-agent records older than StaleCutoff across all
// sessions, reclaiming files left by sessions that crashed or whose
// SubagentStop never arrived. It is age-based so it never reaps a record an
// in-flight async sub-agent still needs. Fails open. Mirrors
// toolctx.SweepStale.
func SweepStale() {
	root := getDir("") // .../agents
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
					filelog.Warn("agentctx: could not remove stale record: %v", err)
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
