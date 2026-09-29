package transcript

import (
	"bufio"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodeMsg unmarshals a rendered {"role","parts":[...]} message string.
func decodeMsg(t *testing.T, s string) renderedMessage {
	t.Helper()
	var m renderedMessage
	require.NoError(t, json.Unmarshal([]byte(s), &m), "rendered message must be valid JSON: %s", s)
	return m
}

// Given a transcript with two assistant messages and a turn start before both,
// when calls are reconstructed, then one call per message id is produced — each
// with its per-call input history, coalesced output, token usage, and time
// bounds that abut (call 2 starts where call 1 ended).
func TestReadTurnCalls_ReconstructsPerCall(t *testing.T) {
	// Start before every entry: both assistant messages become calls.
	start := nanosAt(t, "2026-07-08T03:30:00Z")
	calls := ReadTurnCalls(filepath.Join("testdata", "turn_calls.jsonl"), start)

	require.Len(t, calls, 2, "one call per assistant message.id")

	// --- Call 1 (msg_A): the first LLM call ---
	c0 := calls[0]
	assert.Equal(t, "msg_A", c0.MessageID)
	assert.Equal(t, "claude-sonnet-4", c0.Model)
	assert.Equal(t, "tool_use", c0.StopReason)
	assert.Equal(t, int64(1200), c0.InputTokens)
	assert.Equal(t, int64(500), c0.OutputTokens)
	assert.Equal(t, int64(300), c0.CacheReadTokens)
	assert.Equal(t, int64(100), c0.CacheCreationTokens)

	// Its input is only the user prompt that preceded it.
	require.Len(t, c0.InputMessages, 1, "history before the first call")
	in0 := decodeMsg(t, c0.InputMessages[0])
	assert.Equal(t, roleUser, in0.Role)
	require.Len(t, in0.Parts, 1)
	assert.Equal(t, "text", in0.Parts[0].Type)
	assert.Equal(t, "add logging", in0.Parts[0].Text)

	// Its output coalesces all three content blocks of msg_A.
	out0 := decodeMsg(t, c0.Output)
	assert.Equal(t, roleAssistant, out0.Role)
	require.Len(t, out0.Parts, 3)
	assert.Equal(t, "thinking", out0.Parts[0].Type)
	assert.Equal(t, "consider where", out0.Parts[0].Text)
	assert.Equal(t, "text", out0.Parts[1].Type)
	assert.Equal(t, "I'll add it", out0.Parts[1].Text)
	assert.Equal(t, "tool_use", out0.Parts[2].Type)
	assert.Equal(t, "Edit", out0.Parts[2].Name)

	// OutputText is the visible text only: thinking and tool_use are excluded.
	assert.Equal(t, "I'll add it", c0.OutputText)
	// InputText is the newest user message preceding the call — here the prompt.
	assert.Equal(t, "add logging", c0.InputText)

	// Bounds: starts at the turn start, ends at msg_A's timestamp.
	assert.Equal(t, start, c0.StartNano, "first call abuts the turn start")
	assert.Equal(t, nanosAt(t, "2026-07-08T03:30:56.000Z"), c0.EndNano)

	// --- Call 2 (msg_B): the second LLM call ---
	c1 := calls[1]
	assert.Equal(t, "msg_B", c1.MessageID)
	assert.Equal(t, "end_turn", c1.StopReason)
	assert.Equal(t, int64(798), c1.InputTokens)

	// Its input is the whole conversation so far: prompt, msg_A, tool_result.
	require.Len(t, c1.InputMessages, 3, "prompt + msg_A + tool_result")
	assert.Equal(t, roleUser, decodeMsg(t, c1.InputMessages[0]).Role)
	assert.Equal(t, roleAssistant, decodeMsg(t, c1.InputMessages[1]).Role)
	toolResultMsg := decodeMsg(t, c1.InputMessages[2])
	assert.Equal(t, roleUser, toolResultMsg.Role)
	require.Len(t, toolResultMsg.Parts, 1)
	assert.Equal(t, "tool_result", toolResultMsg.Parts[0].Type)
	assert.Equal(t, "toolu_1", toolResultMsg.Parts[0].ToolUseID)

	out1 := decodeMsg(t, c1.Output)
	require.Len(t, out1.Parts, 1)
	assert.Equal(t, "done", out1.Parts[0].Text)
	assert.Equal(t, "done", c1.OutputText)
	// The second call's input delta is the tool_result it received, not the
	// original prompt — so InputText differs from the first call's.
	assert.Equal(t, "ok", c1.InputText)

	// Bounds abut: call 2 starts where call 1 ended.
	assert.Equal(t, c0.EndNano, c1.StartNano, "consecutive call spans abut")
	assert.Equal(t, nanosAt(t, "2026-07-08T03:31:05.000Z"), c1.EndNano)
}

// Given a turn start after the first message, when calls are reconstructed, then
// only the later message qualifies as a call but its input still includes the
// full prior history (the prefix is never skipped).
func TestReadTurnCalls_FullHistoryPrefixSurvivesFilter(t *testing.T) {
	// Start after msg_A: only msg_B qualifies as an in-turn call, but its input
	// history must still include every earlier message — a real API call receives
	// the whole conversation, so the prefix is never skipped.
	start := nanosAt(t, "2026-07-08T03:31:02Z")
	calls := ReadTurnCalls(filepath.Join("testdata", "turn_calls.jsonl"), start)

	require.Len(t, calls, 1, "only msg_B is at/after the start")
	assert.Equal(t, "msg_B", calls[0].MessageID)
	assert.Len(t, calls[0].InputMessages, 3, "full prior history retained despite the filter")
}

// Given an empty path, when calls are read, then nil is returned.
func TestReadTurnCalls_EmptyPath(t *testing.T) {
	assert.Nil(t, ReadTurnCalls("", "1"))
}

// Given an empty or zero turn start, when calls are read, then nil is returned
// rather than over-reporting prior turns' calls (fail closed).
func TestReadTurnCalls_EmptyStartFailsClosed(t *testing.T) {
	// An empty/zero start would make every assistant message qualify and pull in
	// prior turns' calls, so it degrades to nothing rather than over-reporting.
	assert.Nil(t, ReadTurnCalls(filepath.Join("testdata", "turn_calls.jsonl"), ""))
	assert.Nil(t, ReadTurnCalls(filepath.Join("testdata", "turn_calls.jsonl"), "0"))
}

// Given a path to a missing file, when calls are read, then nil is returned
// (fail open).
func TestReadTurnCalls_MissingFileFailsOpen(t *testing.T) {
	assert.Nil(t, ReadTurnCalls(filepath.Join("testdata", "does-not-exist.jsonl"), "1"))
}

// Given a short line, an over-long line, then another short line, When read with
// a small cap and a small buffer (forcing fragmented reads), Then the over-long
// line is drained and flagged (returning no content) and the line after it is
// still read.
func TestReadLine_SkipsOverLongLineAndContinues(t *testing.T) {
	input := "short1\n" + strings.Repeat("x", 100) + "\nshort2\n"
	r := bufio.NewReaderSize(strings.NewReader(input), 16)

	l1, tooLong1, err1 := readLine(r, 20)
	assert.Equal(t, "short1", l1)
	assert.False(t, tooLong1)
	assert.NoError(t, err1)

	l2, tooLong2, err2 := readLine(r, 20)
	assert.True(t, tooLong2, "the 100-char line exceeds the 20-byte cap")
	assert.Equal(t, "", l2, "an over-long line returns no content")
	assert.NoError(t, err2)

	l3, tooLong3, err3 := readLine(r, 20)
	assert.Equal(t, "short2", l3, "the line after the over-long one is still read")
	assert.False(t, tooLong3)
	assert.NoError(t, err3)
}

// Given input whose last line has no trailing newline, When read, Then the line
// is returned together with io.EOF so the caller processes it before stopping.
func TestReadLine_FinalLineWithoutNewline(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("only line"), 16)
	line, tooLong, err := readLine(r, 100)
	assert.Equal(t, "only line", line)
	assert.False(t, tooLong)
	assert.Equal(t, io.EOF, err)
}

// Given empty input, When read, Then it returns empty content with io.EOF.
func TestReadLine_EmptyInput(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader(""), 16)
	line, tooLong, err := readLine(r, 100)
	assert.Equal(t, "", line)
	assert.False(t, tooLong)
	assert.Equal(t, io.EOF, err)
}
