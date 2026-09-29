package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// turnStartNano is the fixture's turn boundary: entries at or after it belong to
// the turn, the one before it (00:00:05) must be excluded.
func turnStartNano() string {
	return fmt.Sprintf("%d", time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC).UnixNano())
}

// Given a transcript with an accepted call, a user-rejected denial, a deny-rule
// denial, an abandoned call, an interruption text block, and a pre-turn entry,
// when the turn is classified, then denials map to their kinds, the accepted
// call is Resolved, and the abandoned/pre-turn/interruption calls are absent.
func TestReadTurnOutcomes_Classifies(t *testing.T) {
	got := ReadTurnOutcomes(filepath.Join("testdata", "turn_outcomes.jsonl"), turnStartNano())

	require.Contains(t, got, "toolu_ok")
	assert.Equal(t, OutcomeResolved, got["toolu_ok"].Kind, "normal result -> resolved")
	assert.Equal(t, "Bash", got["toolu_ok"].Name)
	assert.JSONEq(t, `{"command":"ls"}`, got["toolu_ok"].Input)

	require.Contains(t, got, "toolu_userdeny")
	assert.Equal(t, OutcomeDeniedUser, got["toolu_userdeny"].Kind, "toolDenialKind=user-rejected -> denied-user")
	assert.Equal(t, "Edit", got["toolu_userdeny"].Name)

	require.Contains(t, got, "toolu_ruledeny")
	assert.Equal(t, OutcomeDeniedRule, got["toolu_ruledeny"].Kind, "toolDenialKind=permission-rule -> denied-rule")

	// An is_error result with no toolDenialKind (an ordinary execution error, and
	// the shape an auto-classifier denial's tool_result takes) is Resolved, not a
	// denial. This backs the reconstructLeftovers invariant that a live-hooked
	// auto-classifier denial is never re-emitted from the transcript.
	require.Contains(t, got, "toolu_execerr")
	assert.Equal(t, OutcomeResolved, got["toolu_execerr"].Kind, "is_error without toolDenialKind -> resolved, not a denial")

	assert.NotContains(t, got, "toolu_abandon", "a tool_use with no result is not an outcome (unresolved downstream)")
	assert.NotContains(t, got, "toolu_old", "an entry before the turn start is excluded by the reverse-scan stop")
}

// Given an empty or non-positive start, when outcomes are read, then an empty map
// is returned rather than scanning the entire session transcript (which would
// reconstruct denials from prior turns). Guards the flushInterruptedTurn path.
func TestReadTurnOutcomes_EmptyStart(t *testing.T) {
	assert.Empty(t, ReadTurnOutcomes(filepath.Join("testdata", "turn_outcomes.jsonl"), ""), "empty start -> empty map")
	assert.Empty(t, ReadTurnOutcomes(filepath.Join("testdata", "turn_outcomes.jsonl"), "0"), "zero start -> empty map")
}

// Given no transcript path, when outcomes are read, then an empty map is
// returned (fail open).
func TestReadTurnOutcomes_NoPath(t *testing.T) {
	assert.Empty(t, ReadTurnOutcomes("", turnStartNano()))
}

// Given a missing transcript file, when outcomes are read, then an empty map is
// returned (fail open).
func TestReadTurnOutcomes_MissingFile(t *testing.T) {
	assert.Empty(t, ReadTurnOutcomes(filepath.Join("testdata", "does-not-exist.jsonl"), turnStartNano()))
}

// Given a start time after every entry, when outcomes are read, then the scan
// stops immediately and nothing is classified (time-window lower bound).
func TestReadTurnOutcomes_StartAfterAll(t *testing.T) {
	future := fmt.Sprintf("%d", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	assert.Empty(t, ReadTurnOutcomes(filepath.Join("testdata", "turn_outcomes.jsonl"), future))
}

// Given a plain-text (string) message.content, when it is scanned, then it is
// tolerated and skipped rather than breaking the classifier. Exercised by the
// interruption text block in the fixture, which yields no outcome.
func TestReadTurnOutcomes_ContentAsStringTolerated(t *testing.T) {
	got := ReadTurnOutcomes(filepath.Join("testdata", "turn_outcomes.jsonl"), turnStartNano())
	// The interruption marker carried no tool_use_id, so it contributes nothing;
	// the surrounding array-content entries still classify correctly.
	assert.Len(t, got, 4, "only the id-keyed results are classified (ok, userdeny, ruledeny, execerr)")
}

// Given a transcript larger than one read chunk, when it is scanned in reverse,
// then lines split across chunk boundaries are reassembled correctly (a
// denial near the top is still found).
func TestReadTurnOutcomes_ReverseReaderChunkBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jsonl")
	f, err := os.Create(path)
	require.NoError(t, err)

	// A denial as the very first line, then enough padding assistant lines to
	// exceed several 64KiB read chunks, so the denial sits many chunks deep.
	_, err = f.WriteString(`{"type":"user","timestamp":"2026-01-01T00:00:11Z","toolDenialKind":"user-rejected","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_deep","is_error":true}]}}` + "\n")
	require.NoError(t, err)
	pad := `{"type":"assistant","timestamp":"2026-01-01T00:00:12Z","message":{"content":[{"type":"text","text":"` + string(make([]byte, 1000)) + `"}]}}`
	for i := 0; i < 300; i++ { // ~300 * ~1KB = ~300KB > several chunks
		_, err = f.WriteString(pad + "\n")
		require.NoError(t, err)
	}
	require.NoError(t, f.Close())

	got := ReadTurnOutcomes(path, turnStartNano())
	require.Contains(t, got, "toolu_deep", "a denial many chunks deep is still found by the reverse reader")
	assert.Equal(t, OutcomeDeniedUser, got["toolu_deep"].Kind)
}
