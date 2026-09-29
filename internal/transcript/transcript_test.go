package transcript

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nanosAt parses an ISO timestamp into a unix-nano string, matching the
// form ReadTurnUsage expects for the turn start.
func nanosAt(t *testing.T, iso string) string {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		t.Fatalf("parse %q: %v", iso, err)
	}
	return strconv.FormatInt(ts.UnixNano(), 10)
}

// Given a transcript with three message ids each repeated across content-block
// entries, when usage is read from before all of them, then tokens are summed
// once per message id (deduped) and model/stop reason come from the last entry.
func TestReadTurnUsage_DedupesByMessageID(t *testing.T) {
	// Start before all entries: all three message ids counted, deduped
	// across their repeated content-block entries.
	start := nanosAt(t, "2026-07-08T03:00:00Z")
	u := ReadTurnUsage(filepath.Join("testdata", "three_message_ids.jsonl"), start)

	assert.Equal(t, int64(3498), u.InputTokens, "1200+1500+798")
	assert.Equal(t, int64(1562), u.OutputTokens, "500+800+262")
	assert.Equal(t, int64(600), u.CacheReadTokens, "300+200+100")
	assert.Equal(t, int64(150), u.CacheCreationTokens, "100+50+0")
	assert.Equal(t, "claude-sonnet-4", u.Model)
	assert.Equal(t, "end_turn", u.StopReason, "from the last assistant entry")
}

// Given a turn start after the first two messages, when usage is read, then only
// the entry at/after the start is counted.
func TestReadTurnUsage_FiltersByTurnStart(t *testing.T) {
	// Start after the first two messages: only msg_01GHI (03:31:08) counts.
	start := nanosAt(t, "2026-07-08T03:31:05Z")
	u := ReadTurnUsage(filepath.Join("testdata", "three_message_ids.jsonl"), start)

	assert.Equal(t, int64(798), u.InputTokens)
	assert.Equal(t, int64(262), u.OutputTokens)
	assert.Equal(t, "end_turn", u.StopReason)
}

// Given a path to a missing file, when usage is read, then it returns zero usage
// with no error (fail open).
func TestReadTurnUsage_MissingFileFailsOpen(t *testing.T) {
	u := ReadTurnUsage(filepath.Join("testdata", "does-not-exist.jsonl"), "0")
	assert.Equal(t, TurnUsage{}, u, "missing file returns zero usage, no error")
}

// Given an empty path, when usage is read, then it returns zero usage.
func TestReadTurnUsage_EmptyPath(t *testing.T) {
	assert.Equal(t, TurnUsage{}, ReadTurnUsage("", "0"))
}

// Given a transcript with a single line larger than the cap between two valid
// assistant entries, When ReadTurnUsage reads it, Then the over-long line is
// skipped and the usage on the entry after it is still counted.
func TestReadTurnUsage_SkipsOverLongLineAndKeepsCounting(t *testing.T) {
	before := `{"type":"assistant","timestamp":"2026-07-08T03:30:56.000Z","message":{"id":"msg_A","model":"claude-sonnet-4","stop_reason":"tool_use","usage":{"input_tokens":100,"output_tokens":10}}}`
	// One line beyond the cap; its content is irrelevant since it is drained.
	oversized := strings.Repeat("x", maxTranscriptLineBytes+10)
	after := `{"type":"assistant","timestamp":"2026-07-08T03:31:05.000Z","message":{"id":"msg_B","model":"claude-sonnet-4","stop_reason":"end_turn","usage":{"input_tokens":798,"output_tokens":20}}}`

	path := filepath.Join(t.TempDir(), "oversized.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(before+"\n"+oversized+"\n"+after+"\n"), 0o600))

	start := nanosAt(t, "2026-07-08T03:00:00Z")
	u := ReadTurnUsage(path, start)

	// Both surrounding entries are counted; only the oversized line is dropped.
	assert.Equal(t, int64(898), u.InputTokens, "100 (before) + 798 (after) — the entry past the oversized line survives")
	assert.Equal(t, int64(30), u.OutputTokens, "10 + 20")
	assert.Equal(t, "end_turn", u.StopReason, "the last entry after the skipped line is still read")
}

// Given two entries for one message id whose usage disagrees, when usage is
// read, then the first-seen usage is kept (and a warning is logged).
func TestReadTurnUsage_DisagreeingUsageFirstWins(t *testing.T) {
	// The two entries for msg_X disagree; first-seen usage is kept and a
	// warning is logged (not asserted here, but the totals must be stable).
	start := nanosAt(t, "2026-07-08T03:00:00Z")
	u := ReadTurnUsage(filepath.Join("testdata", "disagreeing_usage.jsonl"), start)
	assert.Equal(t, int64(100), u.InputTokens, "first-seen usage wins")
	assert.Equal(t, int64(50), u.OutputTokens)
}

// Given a transcript whose leading line has no timestamp followed by timestamped
// entries, when FirstEntryNano reads it, then it returns the earliest (first
// chronological) timestamped entry, skipping the non-timestamped leading line.
func TestFirstEntryNano(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.jsonl")
	lines := "" +
		`{"type":"file-history-snapshot"}` + "\n" + // no timestamp: skipped
		`{"type":"user","timestamp":"2026-07-08T03:31:00.000Z","message":{"content":"go"}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-07-08T03:31:05.000Z","message":{"id":"m"}}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(lines), 0o600))

	assert.Equal(t, nanosAt(t, "2026-07-08T03:31:00Z"), FirstEntryNano(path),
		"first timestamped entry is the start")
}

// Given an empty path, a missing file, or a file with no parseable timestamps,
// when FirstEntryNano reads it, then it fails open to "".
func TestFirstEntryNano_FailsOpen(t *testing.T) {
	assert.Equal(t, "", FirstEntryNano(""), "empty path -> empty")
	assert.Equal(t, "", FirstEntryNano(filepath.Join(t.TempDir(), "nope.jsonl")), "missing file -> empty")

	dir := t.TempDir()
	path := filepath.Join(dir, "no_ts.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"summary"}`+"\n"), 0o600))
	assert.Equal(t, "", FirstEntryNano(path), "no timestamped entry -> empty")
}

// Given a transcript whose entries carry Claude Code's top-level "version"
// field, when ReadVersion reads it, then it returns the first version found,
// skipping a leading entry that has none.
func TestReadVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.jsonl")
	lines := "" +
		`{"type":"summary"}` + "\n" + // no version: skipped
		`{"type":"user","version":"2.1.0","message":{"content":"go"}}` + "\n" +
		`{"type":"assistant","version":"2.1.0","message":{"id":"m"}}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(lines), 0o600))

	assert.Equal(t, "2.1.0", ReadVersion(path))
}

// Given an empty path, a missing file, or a transcript with no version field,
// when ReadVersion reads it, then it fails open to "".
func TestReadVersion_FailsOpen(t *testing.T) {
	assert.Equal(t, "", ReadVersion(""), "empty path -> empty")
	assert.Equal(t, "", ReadVersion(filepath.Join(t.TempDir(), "nope.jsonl")), "missing file -> empty")

	dir := t.TempDir()
	path := filepath.Join(dir, "no_version.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"user","message":{"content":"hi"}}`+"\n"), 0o600))
	assert.Equal(t, "", ReadVersion(path), "no version field -> empty")
}
