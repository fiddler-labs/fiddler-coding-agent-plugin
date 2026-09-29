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

// Transcript lines for a tool-using turn: prompt, a tool_use call (msg_A), its
// tool_result, then the final answer call (msg_B). raceLines is the state the
// Stop hook sees when it wins the race: everything but msg_B.
const (
	lineSummary    = `{"type":"summary","summary":"prior session","timestamp":"2026-07-08T03:29:00.000Z"}`
	linePrompt     = `{"type":"user","timestamp":"2026-07-08T03:30:50.000Z","message":{"content":"how many entries?"}}`
	lineToolUse    = `{"type":"assistant","timestamp":"2026-07-08T03:30:56.000Z","message":{"id":"msg_A","model":"claude-haiku-4-5","stop_reason":"tool_use","usage":{"input_tokens":1200,"output_tokens":50},"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls -a"}}]}}`
	lineToolResult = `{"type":"user","timestamp":"2026-07-08T03:31:00.000Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"a\nb","is_error":false}]}}`
	lineFinal      = `{"type":"assistant","timestamp":"2026-07-08T03:31:05.000Z","message":{"id":"msg_B","model":"claude-haiku-4-5","stop_reason":"end_turn","usage":{"input_tokens":1300,"output_tokens":20},"content":[{"type":"text","text":"There are 22 entries."}]}}`

	finalAnswer = "There are 22 entries."
)

var raceLines = []string{lineSummary, linePrompt, lineToolUse, lineToolResult}

// Short bounds so the tests run fast; the logic is the same as the defaults.
const (
	testTimeout = 300 * time.Millisecond
	testPoll    = 5 * time.Millisecond
)

// writeTranscript writes lines (newline-terminated) to a temp transcript file.
func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return path
}

// appendLine appends one newline-terminated line to path.
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(line + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// Given a transcript that already holds the final answer, When settled, Then
// the calls come back unchanged with no wait.
func TestSettleTurnCalls_CompleteTranscriptNoWait(t *testing.T) {
	path := writeTranscript(t, append(raceLines, lineFinal)...)
	start := nanosAt(t, "2026-07-08T03:30:00Z")

	began := time.Now()
	calls, res := SettleTurnCalls(path, start, finalAnswer, testTimeout, testPoll)

	assert.Less(t, time.Since(began), testTimeout/2, "no wait when the final call is on disk")
	assert.False(t, res.Pending)
	require.Len(t, calls, 2)
	assert.Equal(t, "msg_B", calls[1].MessageID)
	assert.False(t, calls[1].Synthesized)
}

// Given the race state (final answer not yet written), When the final line is
// appended partway through the wait, Then the wait picks it up and returns the
// real call with its model and token usage, not a synthesized one.
func TestSettleTurnCalls_FinalLineLandsDuringWait(t *testing.T) {
	path := writeTranscript(t, raceLines...)
	start := nanosAt(t, "2026-07-08T03:30:00Z")

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(50 * time.Millisecond)
		appendLine(t, path, lineFinal)
	}()

	calls, res := SettleTurnCalls(path, start, finalAnswer, testTimeout, testPoll)
	<-done

	assert.True(t, res.Pending, "the missing final call was detected")
	assert.True(t, res.Resolved, "the final call appeared during the wait")
	assert.False(t, res.Synthesized)
	assert.GreaterOrEqual(t, res.Waited, 40*time.Millisecond)
	assert.Less(t, res.Waited, testTimeout, "the wait stops as soon as the call lands")

	require.Len(t, calls, 2)
	final := calls[1]
	assert.Equal(t, "msg_B", final.MessageID)
	assert.False(t, final.Synthesized)
	assert.Equal(t, "claude-haiku-4-5", final.Model)
	assert.Equal(t, int64(1300), final.InputTokens)
	assert.Equal(t, finalAnswer, final.OutputText)
}

// Given the race state and a final line that never lands, When the wait times
// out, Then a synthesized final call is appended: input is the full history on
// disk (ending with the tool result), output is the payload's answer, and model
// and token usage are left empty.
func TestSettleTurnCalls_TimeoutSynthesizesFinalCall(t *testing.T) {
	path := writeTranscript(t, raceLines...)
	start := nanosAt(t, "2026-07-08T03:30:00Z")

	calls, res := SettleTurnCalls(path, start, finalAnswer, testTimeout, testPoll)

	assert.True(t, res.Pending)
	assert.False(t, res.Resolved)
	assert.True(t, res.Synthesized)
	assert.GreaterOrEqual(t, res.Waited, testTimeout, "waits the full timeout before giving up")

	require.Len(t, calls, 2, "the real tool_use call plus the synthesized final call")
	assert.False(t, calls[0].Synthesized)

	syn := calls[1]
	assert.True(t, syn.Synthesized)
	assert.Empty(t, syn.MessageID)
	assert.Empty(t, syn.Model, "model is left empty rather than guessed")
	assert.Empty(t, syn.StopReason)
	assert.Zero(t, syn.InputTokens)
	assert.Zero(t, syn.OutputTokens)
	assert.Equal(t, finalAnswer, syn.OutputText)
	assert.Equal(t, finalAnswer, decodeMsg(t, syn.Output).Parts[0].Text)
	assert.Equal(t, roleAssistant, decodeMsg(t, syn.Output).Role)

	// Input is prompt + tool_use call + tool_result: the whole history on disk.
	require.Len(t, syn.InputMessages, 3)
	last := decodeMsg(t, syn.InputMessages[2])
	assert.Equal(t, "tool_result", last.Parts[0].Type)
	assert.Equal(t, "a\nb", syn.InputText, "input delta is the tool result it received")

	// The span abuts the previous call and ends at or after it.
	assert.Equal(t, calls[0].EndNano, syn.StartNano)
	assert.NotEmpty(t, syn.EndNano)
}

// Given the race state and a final line that never lands, When the wait times
// out, Then the synthesized call ends when the wait began, so its duration does
// not include the wait.
func TestSettleTurnCalls_SynthesizedEndExcludesWait(t *testing.T) {
	path := writeTranscript(t, raceLines...)
	start := nanosAt(t, "2026-07-08T03:30:00Z")

	before := time.Now().UnixNano()
	calls, res := SettleTurnCalls(path, start, finalAnswer, testTimeout, testPoll)
	after := time.Now().UnixNano()
	require.True(t, res.Synthesized)

	end := mustParseNano(t, calls[1].EndNano)
	assert.GreaterOrEqual(t, end, before)
	assert.Less(t, end, after-int64(testTimeout)/2, "end is taken before the wait, not after it")
}

// A final answer that used extended thinking: a thinking line (text empty) then
// a text line, both with the same message id and stop reason end_turn.
const (
	lineFinalThinking = `{"type":"assistant","timestamp":"2026-07-08T03:31:04.000Z","message":{"id":"msg_B","model":"claude-haiku-4-5","stop_reason":"end_turn","usage":{"input_tokens":1300,"output_tokens":20},"content":[{"type":"thinking","thinking":"","signature":"sig"}]}}`
	lineFinalText     = `{"type":"assistant","timestamp":"2026-07-08T03:31:05.000Z","message":{"id":"msg_B","model":"claude-haiku-4-5","stop_reason":"end_turn","usage":{"input_tokens":1300,"output_tokens":20},"content":[{"type":"text","text":"There are 22 entries."}]}}`
)

// Given the final message's thinking line is on disk but its text line is not,
// When the text line lands partway through the wait, Then the wait picks it up
// and the final call carries the real text, with no extra call.
func TestSettleTurnCalls_TextLineLandsDuringWait(t *testing.T) {
	path := writeTranscript(t, append(raceLines, lineFinalThinking)...)
	start := nanosAt(t, "2026-07-08T03:30:00Z")

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(50 * time.Millisecond)
		appendLine(t, path, lineFinalText)
	}()

	calls, res := SettleTurnCalls(path, start, finalAnswer, testTimeout, testPoll)
	<-done

	assert.True(t, res.Pending)
	assert.True(t, res.Resolved)
	assert.False(t, res.Filled)
	assert.False(t, res.Synthesized)
	assert.Less(t, res.Waited, testTimeout)
	require.Len(t, calls, 2, "no extra call")
	assert.Equal(t, "msg_B", calls[1].MessageID)
	assert.Equal(t, finalAnswer, calls[1].OutputText)
}

// Given the final message's thinking line is on disk but its text line never
// lands, When the wait times out, Then the payload's answer is filled into that
// call, which keeps its real model and token usage and is not tagged; no extra
// call is added.
func TestSettleTurnCalls_TimeoutFillsFinalText(t *testing.T) {
	path := writeTranscript(t, append(raceLines, lineFinalThinking)...)
	start := nanosAt(t, "2026-07-08T03:30:00Z")

	calls, res := SettleTurnCalls(path, start, finalAnswer, testTimeout, testPoll)

	assert.True(t, res.Pending)
	assert.False(t, res.Resolved)
	assert.True(t, res.Filled)
	assert.False(t, res.Synthesized)
	require.Len(t, calls, 2, "the existing call is filled, no extra call")

	final := calls[1]
	assert.Equal(t, "msg_B", final.MessageID)
	assert.False(t, final.Synthesized, "a filled call is not tagged")
	assert.Equal(t, "claude-haiku-4-5", final.Model, "real model kept")
	assert.Equal(t, int64(1300), final.InputTokens, "real token usage kept")
	assert.Equal(t, finalAnswer, final.OutputText)

	out := decodeMsg(t, final.Output)
	assert.Equal(t, roleAssistant, out.Role)
	require.Len(t, out.Parts, 2, "thinking part kept, text part appended")
	assert.Equal(t, "thinking", out.Parts[0].Type)
	assert.Equal(t, "text", out.Parts[1].Type)
	assert.Equal(t, finalAnswer, out.Parts[1].Text)
}

// Given a complete final message whose text differs from the payload only in
// formatting, When settled, Then there is no wait: the text-missing check only
// looks for empty text, never an exact match.
func TestSettleTurnCalls_CompleteFinalFormattingDiffNoWait(t *testing.T) {
	path := writeTranscript(t, append(raceLines, lineFinalThinking, lineFinalText)...)
	start := nanosAt(t, "2026-07-08T03:30:00Z")

	began := time.Now()
	calls, res := SettleTurnCalls(path, start, "**There are 22 entries.**", testTimeout, testPoll)
	assert.Less(t, time.Since(began), testTimeout/2)
	assert.False(t, res.Pending)
	require.Len(t, calls, 2)
	assert.Equal(t, finalAnswer, calls[1].OutputText, "transcript text is kept")
}

// Given a tool_use message whose thinking line is on disk but whose other lines
// are not, When settled and nothing more lands, Then it is treated as the
// whole final message missing (a reply must follow a tool call) and a final
// call is synthesized rather than filled into the tool_use call.
func TestSettleTurnCalls_ThinkingOnlyToolUseWaitsForFinalCall(t *testing.T) {
	thinkingToolUse := `{"type":"assistant","timestamp":"2026-07-08T03:30:56.000Z","message":{"id":"msg_A","model":"claude-haiku-4-5","stop_reason":"tool_use","content":[{"type":"thinking","thinking":"","signature":"sig"}]}}`
	path := writeTranscript(t, linePrompt, thinkingToolUse)

	calls, res := SettleTurnCalls(path, nanosAt(t, "2026-07-08T03:30:00Z"), finalAnswer, testTimeout, testPoll)
	assert.True(t, res.Pending)
	assert.True(t, res.Synthesized)
	assert.False(t, res.Filled)
	require.Len(t, calls, 2)
	assert.Empty(t, calls[0].OutputText, "the tool_use call is not filled")
	assert.True(t, calls[1].Synthesized)
}

func mustParseNano(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	require.NoError(t, err)
	return n
}

// Given a turn that genuinely ends on a tool call (e.g. a manual reject), When
// settled, Then there is no wait, because last_assistant_message is either
// empty or that tool_use message's own text.
func TestSettleTurnCalls_TurnEndingOnToolUseNoWait(t *testing.T) {
	withText := `{"type":"assistant","timestamp":"2026-07-08T03:30:56.000Z","message":{"id":"msg_A","stop_reason":"tool_use","content":[{"type":"text","text":"Let me list the files."},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]}}`
	path := writeTranscript(t, linePrompt, withText)
	start := nanosAt(t, "2026-07-08T03:30:00Z")

	for name, final := range map[string]string{
		"empty final answer":           "",
		"final answer is its own text": "Let me list the files.",
		"same text, other whitespace":  "  Let me\nlist the   files. ",
	} {
		t.Run(name, func(t *testing.T) {
			began := time.Now()
			calls, res := SettleTurnCalls(path, start, final, testTimeout, testPoll)
			assert.Less(t, time.Since(began), testTimeout/2)
			assert.False(t, res.Pending)
			require.Len(t, calls, 1)
			assert.False(t, calls[0].Synthesized)
		})
	}
}

// Given a last call whose stop reason is null (unknown), When settled, Then
// there is no wait: only a tool_use stop reason proves a reply must follow.
func TestSettleTurnCalls_NullStopReasonNoWait(t *testing.T) {
	nullStop := `{"type":"assistant","timestamp":"2026-07-08T03:30:56.000Z","message":{"id":"msg_A","stop_reason":null,"content":[{"type":"text","text":"partial"}]}}`
	path := writeTranscript(t, linePrompt, nullStop)

	calls, res := SettleTurnCalls(path, nanosAt(t, "2026-07-08T03:30:00Z"), finalAnswer, testTimeout, testPoll)
	assert.False(t, res.Pending)
	require.Len(t, calls, 1)
}

// Given no in-turn calls (empty transcript, or a start after every entry),
// When settled, Then there is no wait and no synthesized call, so the pipeline
// keeps its aggregated fallback.
func TestSettleTurnCalls_ZeroCallsNoWait(t *testing.T) {
	empty := writeTranscript(t)
	calls, res := SettleTurnCalls(empty, "1", finalAnswer, testTimeout, testPoll)
	assert.False(t, res.Pending)
	assert.Empty(t, calls)

	path := writeTranscript(t, raceLines...)
	calls, res = SettleTurnCalls(path, nanosAt(t, "2026-07-08T04:00:00Z"), finalAnswer, testTimeout, testPoll)
	assert.False(t, res.Pending)
	assert.Empty(t, calls)
}

// Given a missing transcript file, When settled, Then it fails open with no
// wait.
func TestSettleTurnCalls_MissingFileNoWait(t *testing.T) {
	calls, res := SettleTurnCalls(filepath.Join(t.TempDir(), "nope.jsonl"), "1", finalAnswer, testTimeout, testPoll)
	assert.False(t, res.Pending)
	assert.Nil(t, calls)
}

// Given the file grows during the wait but not with the final call (e.g. an
// unrelated metadata line), When settled, Then it keeps waiting and still
// synthesizes on timeout.
func TestSettleTurnCalls_UnrelatedGrowthKeepsWaiting(t *testing.T) {
	path := writeTranscript(t, raceLines...)
	start := nanosAt(t, "2026-07-08T03:30:00Z")

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(30 * time.Millisecond)
		appendLine(t, path, `{"type":"last-prompt","timestamp":"2026-07-08T03:31:01.000Z"}`)
	}()

	calls, res := SettleTurnCalls(path, start, finalAnswer, testTimeout, testPoll)
	<-done

	assert.True(t, res.Synthesized)
	require.Len(t, calls, 2)
	assert.True(t, calls[1].Synthesized)
}
