package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A tool-using turn in the race state the Stop hook can see: the
// prompt, a tool_use call, and its tool_result are on disk, but the final
// answer call is not yet.
const (
	racePrompt     = `{"type":"user","timestamp":"2026-07-08T03:30:50.000Z","message":{"content":"how many entries?"}}`
	raceToolUse    = `{"type":"assistant","timestamp":"2026-07-08T03:30:56.000Z","message":{"id":"msg_A","model":"claude-haiku-4-5","stop_reason":"tool_use","usage":{"input_tokens":1200,"output_tokens":50},"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls -a"}}]}}`
	raceToolResult = `{"type":"user","timestamp":"2026-07-08T03:31:00.000Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"a\nb","is_error":false}]}}`
	raceFinal      = `{"type":"assistant","timestamp":"2026-07-08T03:31:05.000Z","message":{"id":"msg_B","model":"claude-haiku-4-5","stop_reason":"end_turn","usage":{"input_tokens":1300,"output_tokens":20},"content":[{"type":"text","text":"There are 22 entries."}]}}`
	raceAnswer     = "There are 22 entries."
)

// shortSettle shortens the settle bounds for the duration of a test.
func shortSettle(t *testing.T) {
	t.Helper()
	oldTimeout, oldPoll := settleTimeout, settlePoll
	settleTimeout, settlePoll = 200*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { settleTimeout, settlePoll = oldTimeout, oldPoll })
}

// raceTranscript writes the race-state transcript and returns its path.
func raceTranscript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	lines := strings.Join([]string{racePrompt, raceToolUse, raceToolResult}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(lines), 0o600))
	return path
}

// appendAfter appends line to path after delay, and returns a channel closed
// once it has been written.
func appendAfter(t *testing.T, path, line string, delay time.Duration) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(delay)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Errorf("open: %v", err)
			return
		}
		_, _ = f.WriteString(line + "\n")
		_ = f.Close()
	}()
	return done
}

// Given a Stop payload in the race state, When the pipeline's first transcript
// read is ReadUsage (as in handleTurnEnd) and the final line lands during the
// wait, Then usage already includes the final call, and ReadCalls returns both
// real calls without a second wait.
func TestAdapt_Stop_SettlesBeforeFirstRead(t *testing.T) {
	shortSettle(t)
	path := raceTranscript(t)
	ev, ok := Adapt("Stop", map[string]any{
		"session_id":             "s",
		"transcript_path":        path,
		"last_assistant_message": raceAnswer,
	})
	require.True(t, ok)

	landed := appendAfter(t, path, raceFinal, 40*time.Millisecond)
	usage := ev.TurnEnd.ReadUsage("1")
	<-landed
	assert.Equal(t, int64(2500), usage.InputTokens, "usage includes the final call after the settle")
	assert.Equal(t, "end_turn", usage.StopReason, "stop reason comes from the final call")

	began := time.Now()
	calls := ev.TurnEnd.ReadCalls("1")
	assert.Less(t, time.Since(began), 100*time.Millisecond, "calls reuse the settled result")
	require.Len(t, calls, 2)
	assert.Equal(t, "msg_B", calls[1].MessageID)
	assert.False(t, calls[1].Synthesized)
	assert.Equal(t, "claude-haiku-4-5", calls[1].Usage.Model)
}

// Given a Stop payload in the race state whose final line never lands, When
// ReadCalls runs, Then the final call is synthesized from last_assistant_message
// with no model or token usage, and marked Synthesized.
func TestAdapt_Stop_SynthesizesOnTimeout(t *testing.T) {
	shortSettle(t)
	path := raceTranscript(t)
	ev, ok := Adapt("Stop", map[string]any{
		"session_id":             "s",
		"transcript_path":        path,
		"last_assistant_message": raceAnswer,
	})
	require.True(t, ok)

	calls := ev.TurnEnd.ReadCalls("1")
	require.Len(t, calls, 2)
	syn := calls[1]
	assert.True(t, syn.Synthesized)
	assert.Equal(t, raceAnswer, syn.OutputText)
	assert.Empty(t, syn.Usage.Model)
	assert.Zero(t, syn.Usage.InputTokens)
	assert.Equal(t, "a\nb", syn.InputText)
}

// Given a StopFailure payload (last_assistant_message is the API error text,
// which never appears as an assistant entry), When ReadCalls runs, Then there
// is no wait and nothing is synthesized.
func TestAdapt_StopFailure_NoSettle(t *testing.T) {
	shortSettle(t)
	path := raceTranscript(t)
	ev, ok := Adapt("StopFailure", map[string]any{
		"session_id":             "s",
		"transcript_path":        path,
		"error":                  "server_error",
		"last_assistant_message": "API Error: 500",
	})
	require.True(t, ok)

	began := time.Now()
	calls := ev.TurnEnd.ReadCalls("1")
	assert.Less(t, time.Since(began), settleTimeout/2, "StopFailure never waits")
	require.Len(t, calls, 1)
	assert.False(t, calls[0].Synthesized)
}

// Given a SubagentStop payload whose sub-agent transcript is in the race state,
// When ReadCalls runs and the final line lands during the wait, Then both real
// calls are returned.
func TestAdapt_SubagentStop_Settles(t *testing.T) {
	shortSettle(t)
	path := raceTranscript(t)
	ev, ok := Adapt("SubagentStop", map[string]any{
		"session_id":             "s",
		"agent_id":               "agent-1",
		"agent_transcript_path":  path,
		"last_assistant_message": raceAnswer,
	})
	require.True(t, ok)

	landed := appendAfter(t, path, raceFinal, 40*time.Millisecond)
	calls := ev.SubagentEnd.ReadCalls(ev.SubagentEnd.ReadStart())
	<-landed
	require.Len(t, calls, 2)
	assert.Equal(t, "msg_B", calls[1].MessageID)
	assert.False(t, calls[1].Synthesized)
}

// Given a SubagentStop payload in the race state whose final line never lands,
// When ReadCalls runs, Then the final call is synthesized.
func TestAdapt_SubagentStop_SynthesizesOnTimeout(t *testing.T) {
	shortSettle(t)
	path := raceTranscript(t)
	ev, ok := Adapt("SubagentStop", map[string]any{
		"session_id":             "s",
		"agent_id":               "agent-1",
		"agent_transcript_path":  path,
		"last_assistant_message": raceAnswer,
	})
	require.True(t, ok)

	calls := ev.SubagentEnd.ReadCalls(ev.SubagentEnd.ReadStart())
	require.Len(t, calls, 2)
	assert.True(t, calls[1].Synthesized)
}

// Given a Stop payload whose final message has its thinking line on disk but
// not its text line, and the text line never lands, When ReadCalls runs, Then
// the payload's answer is filled into that call, which keeps its real model
// and token usage and is not tagged synthesized.
func TestAdapt_Stop_FillsMissingFinalText(t *testing.T) {
	shortSettle(t)
	path := raceTranscript(t)
	thinking := `{"type":"assistant","timestamp":"2026-07-08T03:31:04.000Z","message":{"id":"msg_B","model":"claude-haiku-4-5","stop_reason":"end_turn","usage":{"input_tokens":1300,"output_tokens":20},"content":[{"type":"thinking","thinking":"","signature":"sig"}]}}`
	<-appendAfter(t, path, thinking, 0)

	ev, ok := Adapt("Stop", map[string]any{
		"session_id":             "s",
		"transcript_path":        path,
		"last_assistant_message": raceAnswer,
	})
	require.True(t, ok)

	calls := ev.TurnEnd.ReadCalls("1")
	require.Len(t, calls, 2)
	final := calls[1]
	assert.Equal(t, "msg_B", final.MessageID)
	assert.False(t, final.Synthesized)
	assert.Equal(t, raceAnswer, final.OutputText)
	assert.Equal(t, "claude-haiku-4-5", final.Usage.Model)
	assert.Equal(t, int64(1300), final.Usage.InputTokens)
}
