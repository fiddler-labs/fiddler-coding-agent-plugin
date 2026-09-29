// Package turnctx manages the per-session context file that stitches
// spans across isolated hook processes into one trace per turn.
//
// Each hook fires as a separate, short-lived process with no shared
// memory. The context file at ~/.fiddler/context/<session_id>.json
// bridges the gap: UserPromptSubmit writes it once (trace_id, root
// span_id, start time, prompt); PostToolUse and Stop read it to parent
// their spans; Stop deletes it. The file is write-once, read-many, then
// deleted per turn.
//
// All operations fail open: a missing or corrupt file degrades data
// (dropped tool spans for that turn) but never blocks the session.
package turnctx

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/filelog"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/otlp"
)

// getCtxDir returns the directory where per-session context files are stored,
// creating it if necessary.
//
// Prefers CLAUDE_PLUGIN_DATA (set by Claude Code for each plugin) so
// storage lives in the plugin's managed data directory. Falls back to
// ~/.fiddler/context when it is unset (e.g. local testing).
func getCtxDir() string {
	var dir string
	if dataDir := os.Getenv("CLAUDE_PLUGIN_DATA"); dataDir != "" {
		dir = filepath.Join(dataDir, "context")
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.Getenv("HOME")
		}
		dir = filepath.Join(home, ".fiddler", "context")
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// Context holds the per-turn state shared across hook processes.
type Context struct {
	TraceID    string `json:"trace_id"`
	RootSpanID string `json:"root_span_id"`
	StartNano  string `json:"start_unix_nano"`
	UserPrompt string `json:"user_prompt"`
	Cwd        string `json:"cwd"`
}

// ctxPath returns the file path for a session's context file.
func ctxPath(sessionID string) string {
	safe := strings.ReplaceAll(sessionID, "/", "_")
	return filepath.Join(getCtxDir(), safe+".json")
}

// New allocates a fresh trace context for a new turn and writes it to
// disk. Called at UserPromptSubmit.
func New(sessionID, userPrompt, cwd string) (*Context, error) {
	ctx := &Context{
		TraceID:    otlp.NewTraceID(),
		RootSpanID: otlp.NewSpanID(),
		StartNano:  fmt.Sprintf("%d", time.Now().UnixNano()),
		UserPrompt: userPrompt,
		Cwd:        cwd,
	}

	if err := writeCtx(sessionID, ctx); err != nil {
		return ctx, err
	}

	filelog.Info("context created: session=%s trace_id=%s root_span_id=%s",
		sessionID, ctx.TraceID, ctx.RootSpanID)
	return ctx, nil
}

// Load reads the turn's trace context from disk. If the file is missing
// or corrupt, it lazily creates a fresh context (so tool spans for a
// turn whose UserPromptSubmit was missed still get a trace).
func Load(sessionID string) (*Context, error) {
	path := ctxPath(sessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			filelog.Warn("no context for session=%s; creating lazily", sessionID)
			return New(sessionID, "", "")
		}
		return nil, fmt.Errorf("read context file: %w", err)
	}

	var ctx Context
	if err := json.Unmarshal(data, &ctx); err != nil {
		filelog.Warn("corrupt context for session=%s; creating lazily", sessionID)
		return New(sessionID, "", "")
	}
	return &ctx, nil
}

// Peek returns the turn context for a session if one exists on disk.
// Unlike Load, it never lazily creates a context: the second return value
// is false when the file is absent or corrupt. It is used to detect a
// leftover context from a turn whose TurnEnd never arrived (an interrupted
// turn), so its root span can be flushed before the file is overwritten.
//
// Peek is a plain read with no locking, consistent with the rest of this
// package (hooks run as separate short-lived processes, so an in-process
// mutex would not serialize them). The only race it admits is a turn's Stop
// and the next turn's UserPromptSubmit both flushing the same context; both
// emit the root span with the same trace id and span id, so the result is an
// idempotent duplicate (a span re-sent with identical identity), not corruption.
func Peek(sessionID string) (*Context, bool) {
	data, err := os.ReadFile(ctxPath(sessionID))
	if err != nil {
		return nil, false
	}
	var ctx Context
	if err := json.Unmarshal(data, &ctx); err != nil {
		return nil, false
	}
	return &ctx, true
}

// Clear deletes the context file for a session. Called at Stop after
// the turn's spans are emitted, and at SessionEnd for cleanup.
// Fails open if the file is already gone.
func Clear(sessionID string) {
	path := ctxPath(sessionID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		filelog.Warn("could not remove context file: %v", err)
	}
}

// writeCtx serializes and writes the context to disk.
//
// Plain os.WriteFile is used deliberately, with no file locking or
// atomic-rename: the context file is written once per turn at
// UserPromptSubmit, which Claude Code runs to completion before any tool
// calls, so the write finishes before PostToolUse/Stop read it. There is
// no cross-process read-modify-write on this file.
// The only residual risk is a torn read in a rare abnormal ordering (for
// example a missed UserPromptSubmit causing a concurrent lazy-create in
// Load), and that path is already fail-open: an unmarshal error drops one
// tool span for one turn without affecting the session.
func writeCtx(sessionID string, ctx *Context) error {
	data, err := json.Marshal(ctx)
	if err != nil {
		return fmt.Errorf("marshal context: %w", err)
	}
	path := ctxPath(sessionID)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write context file: %w", err)
	}
	return nil
}
