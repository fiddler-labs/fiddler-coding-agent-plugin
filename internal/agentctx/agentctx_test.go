package agentctx

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/toolctx"
)

// isolate points agentctx storage at a temp dir for the test.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_PLUGIN_DATA", dir)
	return dir
}

// Given a record written for an agent_id, when it is read back, then the
// returned record equals what was written.
func TestWriteRead_RoundTrip(t *testing.T) {
	isolate(t)
	rec := Record{TraceID: "0123456789abcdef0123456789abcdef"}
	require.NoError(t, Write("sess", "agent-1", rec))

	got, ok := Read("sess", "agent-1")
	require.True(t, ok)
	assert.Equal(t, rec, got)
}

// Given no record for an id, when it is read, then ok is false and no error is
// raised (fail open).
func TestRead_MissingFailsOpen(t *testing.T) {
	isolate(t)
	_, ok := Read("sess", "nope")
	assert.False(t, ok, "absent record returns ok=false")
}

// Given a record file containing invalid JSON, when it is read, then ok is
// false — the record is treated as absent (fail open).
func TestRead_CorruptFailsOpen(t *testing.T) {
	dir := isolate(t)
	sessDir := filepath.Join(dir, "agents", "sess")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sessDir, "agent-1.json"), []byte("{not json"), 0o644))

	_, ok := Read("sess", "agent-1")
	assert.False(t, ok, "corrupt record returns ok=false")
}

// Given an empty agent_id, when Write is called, then it returns an error,
// because the id is the file key and cannot be empty.
func TestWrite_EmptyIDErrors(t *testing.T) {
	isolate(t)
	assert.Error(t, Write("sess", "", Record{}))
}

// Given an empty agent_id, when Read is called, then ok is false without
// touching the filesystem (empty id is never a valid key).
func TestRead_EmptyIDFailsOpen(t *testing.T) {
	isolate(t)
	_, ok := Read("sess", "")
	assert.False(t, ok)
}

// Given a written record, when Delete is called, then the record is gone, and
// when Delete is called again for the same id, then it is a no-op (idempotent).
func TestDelete(t *testing.T) {
	isolate(t)
	require.NoError(t, Write("sess", "agent-1", Record{TraceID: "t"}))
	Delete("sess", "agent-1")
	_, ok := Read("sess", "agent-1")
	assert.False(t, ok)
	// idempotent: deleting an absent record does not panic or error.
	Delete("sess", "agent-1")
}

// Given records in two sessions, when SweepSession is called for one, then that
// session's records are removed and the other session is left untouched.
func TestSweepSession(t *testing.T) {
	isolate(t)
	require.NoError(t, Write("sess", "agent-1", Record{TraceID: "t1"}))
	require.NoError(t, Write("other", "agent-2", Record{TraceID: "t2"}))

	SweepSession("sess")
	_, ok := Read("sess", "agent-1")
	assert.False(t, ok, "swept session record is gone")
	_, ok = Read("other", "agent-2")
	assert.True(t, ok, "other session untouched")
}

// Given one fresh record and one older than the cutoff, when SweepStale runs,
// then the stale record is removed and the fresh one remains.
func TestSweepStale(t *testing.T) {
	dir := isolate(t)
	require.NoError(t, Write("sess", "fresh", Record{TraceID: "t1"}))
	require.NoError(t, Write("sess", "old", Record{TraceID: "t2"}))

	// Backdate the "old" record well beyond the cutoff.
	oldPath := filepath.Join(dir, "agents", "sess", "old.json")
	past := time.Now().Add(-StaleCutoff - time.Hour)
	require.NoError(t, os.Chtimes(oldPath, past, past))

	SweepStale()

	_, ok := Read("sess", "fresh")
	assert.True(t, ok, "recent record survives")
	_, ok = Read("sess", "old")
	assert.False(t, ok, "stale record reclaimed")
}

// Given a session whose only record is stale, when SweepStale runs, then the
// record is removed and the now-empty session directory is removed too.
func TestSweepStale_RemovesEmptySessionDir(t *testing.T) {
	dir := isolate(t)
	require.NoError(t, Write("sess", "old", Record{TraceID: "t"}))
	oldPath := filepath.Join(dir, "agents", "sess", "old.json")
	past := time.Now().Add(-StaleCutoff - time.Hour)
	require.NoError(t, os.Chtimes(oldPath, past, past))

	SweepStale()

	_, err := os.Stat(filepath.Join(dir, "agents", "sess"))
	assert.True(t, os.IsNotExist(err), "emptied session dir is removed")
}

// Given an agentctx record and a toolctx record under one session, when
// toolctx.SweepSession runs for that session (as it does at every TurnEnd),
// then the agentctx record survives. This guards the load-bearing design
// choice that agentctx lives in a sibling directory of toolctx's "pending",
// so async sub-agent records outlive the turn that launched them.
func TestSweepSession_ToolctxDoesNotRemoveAgentctx(t *testing.T) {
	isolate(t)
	require.NoError(t, Write("sess", "agent-1", Record{TraceID: "t"}))
	require.NoError(t, toolctx.Write("sess", "toolu_1", toolctx.Record{PreNano: "1"}))

	// toolctx.SweepSession fires at every TurnEnd; it must not touch agentctx.
	toolctx.SweepSession("sess")

	_, ok := Read("sess", "agent-1")
	assert.True(t, ok, "agentctx record survives toolctx.SweepSession (sibling dir)")
}
