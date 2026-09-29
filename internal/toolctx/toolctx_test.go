package toolctx

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolate points pending storage at a temp dir for the test.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_PLUGIN_DATA", dir)
	return dir
}

// Given a record written for a tool_use_id, when it is read back, then the
// returned record equals what was written.
func TestWriteRead_RoundTrip(t *testing.T) {
	isolate(t)
	rec := Record{ToolName: "Bash", ToolInput: `{"command":"ls"}`, PermissionMode: "default", PreNano: "123"}
	require.NoError(t, Write("sess", "toolu_1", rec))

	got, ok := Read("sess", "toolu_1")
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
	sessDir := filepath.Join(dir, "pending", "sess")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sessDir, "toolu_1.json"), []byte("{not json"), 0o644))

	_, ok := Read("sess", "toolu_1")
	assert.False(t, ok, "corrupt record returns ok=false")
}

// Given an empty tool_use_id, when Write is called, then it returns an error,
// because the id is the file key and cannot be empty.
func TestWrite_EmptyIDErrors(t *testing.T) {
	isolate(t)
	assert.Error(t, Write("sess", "", Record{}))
}

// Given a written record, when Delete is called, then the record is gone, and
// when Delete is called again for the same id, then it is a no-op (idempotent).
func TestDelete(t *testing.T) {
	isolate(t)
	require.NoError(t, Write("sess", "toolu_1", Record{PreNano: "1"}))
	Delete("sess", "toolu_1")
	_, ok := Read("sess", "toolu_1")
	assert.False(t, ok)
	// idempotent: deleting an absent record does not panic or error.
	Delete("sess", "toolu_1")
}

// Given two records written under one session, when List is called for that
// session, then both are returned keyed by tool_use_id.
func TestList(t *testing.T) {
	isolate(t)
	require.NoError(t, Write("sess", "toolu_1", Record{ToolName: "Bash", PreNano: "1"}))
	require.NoError(t, Write("sess", "toolu_2", Record{ToolName: "Edit", PreNano: "2"}))

	got := List("sess")
	require.Len(t, got, 2)
	assert.Equal(t, "Bash", got["toolu_1"].ToolName)
	assert.Equal(t, "Edit", got["toolu_2"].ToolName)
}

// Given a session with no records, when List is called, then it returns an
// empty map.
func TestList_EmptyWhenNoSession(t *testing.T) {
	isolate(t)
	assert.Empty(t, List("sess"))
}

// Given records in two sessions, when SweepSession is called for one, then that
// session's records are removed and the other session is left untouched.
func TestSweepSession(t *testing.T) {
	isolate(t)
	require.NoError(t, Write("sess", "toolu_1", Record{PreNano: "1"}))
	require.NoError(t, Write("other", "toolu_2", Record{PreNano: "2"}))

	SweepSession("sess")
	assert.Empty(t, List("sess"), "swept session is empty")
	assert.Len(t, List("other"), 1, "other session untouched")
}

// Given one fresh record and one older than the cutoff, when SweepStale runs,
// then the stale record is removed and the fresh one remains.
func TestSweepStale(t *testing.T) {
	dir := isolate(t)
	require.NoError(t, Write("sess", "fresh", Record{PreNano: "1"}))
	require.NoError(t, Write("sess", "old", Record{PreNano: "2"}))

	// Backdate the "old" record well beyond the cutoff.
	oldPath := filepath.Join(dir, "pending", "sess", "old.json")
	past := time.Now().Add(-StaleCutoff - time.Hour)
	require.NoError(t, os.Chtimes(oldPath, past, past))

	SweepStale()

	got := List("sess")
	assert.Contains(t, got, "fresh", "recent record survives")
	assert.NotContains(t, got, "old", "stale record reclaimed")
}

// Given a session whose only record is stale, when SweepStale runs, then the
// record is removed and the now-empty session directory is removed too.
func TestSweepStale_RemovesEmptySessionDir(t *testing.T) {
	dir := isolate(t)
	require.NoError(t, Write("sess", "old", Record{PreNano: "1"}))
	oldPath := filepath.Join(dir, "pending", "sess", "old.json")
	past := time.Now().Add(-StaleCutoff - time.Hour)
	require.NoError(t, os.Chtimes(oldPath, past, past))

	SweepStale()

	_, err := os.Stat(filepath.Join(dir, "pending", "sess"))
	assert.True(t, os.IsNotExist(err), "emptied session dir is removed")
}
