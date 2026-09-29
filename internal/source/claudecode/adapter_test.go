package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/event"
)

// loadPayload reads a fixture payload from testdata/payloads.
func loadPayload(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "payloads", name))
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

// Given a payload whose transcript_path points at a transcript carrying Claude
// Code's top-level "version" field, when ReadAgentVersion reads it, then it
// returns that version; when the path is absent it fails open to "".
func TestReadAgentVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	line := `{"type":"assistant","version":"2.1.0","message":{"id":"m"}}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(line), 0o600))

	assert.Equal(t, "2.1.0", ReadAgentVersion(map[string]any{"transcript_path": path}))
	assert.Equal(t, "", ReadAgentVersion(map[string]any{}), "no transcript_path -> empty")
}

// Given a UserPromptSubmit hook payload, when this is adapted to the internal
// event payload, then the event is KindTurnStart carrying the prompt and cwd,
// with the stale-turn flush readers (ReadOutcomes, ReadCalls) wired from
// transcript_path.
func TestAdapt_UserPromptSubmit(t *testing.T) {
	ev, ok := Adapt("UserPromptSubmit", loadPayload(t, "user_prompt_submit.json"))
	require.True(t, ok)
	assert.Equal(t, event.KindTurnStart, ev.Kind)
	assert.Equal(t, "abc123", ev.SessionID)
	assert.Equal(t, "/home/user/repo", ev.Cwd)
	require.NotNil(t, ev.TurnStart)
	assert.Equal(t, "fix the failing test", ev.TurnStart.Prompt)
	assert.NotNil(t, ev.TurnStart.ReadOutcomes, "transcript_path present -> ReadOutcomes set for stale-turn flush")
	assert.NotNil(t, ev.TurnStart.ReadCalls, "transcript_path present -> ReadCalls set for stale-turn flush")
}

// Given a PreToolUse hook payload, when this is adapted to the internal event
// payload, then the event is KindPreToolUse carrying the tool_use_id, name,
// input, and permission_mode, with no ToolCall.
func TestAdapt_PreToolUse(t *testing.T) {
	ev, ok := Adapt("PreToolUse", loadPayload(t, "pre_tool_use.json"))
	require.True(t, ok)
	assert.Equal(t, event.KindPreToolUse, ev.Kind)
	assert.Equal(t, "abc123", ev.SessionID)
	require.NotNil(t, ev.PreTool)
	assert.Equal(t, "toolu_01ABC", ev.PreTool.ToolUseID)
	assert.Equal(t, "Bash", ev.PreTool.Name)
	assert.JSONEq(t, `{"command":"ls -la"}`, ev.PreTool.Input)
	assert.Equal(t, "default", ev.PreTool.PermissionMode)
	assert.Nil(t, ev.ToolCall)
}

// Given a PostToolUse hook payload, when this is adapted to the internal event
// payload, then the event is KindToolCall carrying the tool name, input,
// extracted output, duration, tool_use_id, and permission_mode.
func TestAdapt_PostToolUse(t *testing.T) {
	ev, ok := Adapt("PostToolUse", loadPayload(t, "post_tool_use.json"))
	require.True(t, ok)
	assert.Equal(t, event.KindToolCall, ev.Kind)
	require.NotNil(t, ev.ToolCall)
	assert.Equal(t, "Bash", ev.ToolCall.Name)
	assert.JSONEq(t, `{"command":"ls -la"}`, ev.ToolCall.Input)
	assert.Equal(t, "file1.txt\nfile2.txt", ev.ToolCall.Output, "stdout extracted from map response")
	assert.Equal(t, float64(1500), ev.ToolCall.DurationMs)
	assert.Equal(t, "toolu_01ABC", ev.ToolCall.ToolUseID, "tool_use_id extracted")
	assert.Equal(t, "default", ev.ToolCall.PermissionMode, "permission_mode extracted")
}

// Given a PermissionDenied hook payload, when this is adapted to the internal
// event payload, then the event is KindPermissionDenied carrying the tool_use_id,
// name, input, reason, and permission_mode; a main-agent denial (no top-level
// agent_id) leaves AgentID empty.
func TestAdapt_PermissionDenied(t *testing.T) {
	ev, ok := Adapt("PermissionDenied", loadPayload(t, "permission_denied.json"))
	require.True(t, ok)
	assert.Equal(t, event.KindPermissionDenied, ev.Kind)
	assert.Equal(t, "abc123", ev.SessionID)
	require.NotNil(t, ev.PermDenied)
	assert.Equal(t, "toolu_01XYZ", ev.PermDenied.ToolUseID)
	assert.Equal(t, "Bash", ev.PermDenied.Name)
	assert.JSONEq(t, `{"command":"rm -rf /tmp/x"}`, ev.PermDenied.Input)
	assert.Equal(t, "Blocked by classifier", ev.PermDenied.Reason)
	assert.Equal(t, "auto", ev.PermDenied.PermissionMode)
	assert.Empty(t, ev.PermDenied.AgentID, "no top-level agent_id -> empty")
	assert.Nil(t, ev.ToolCall)
}

// Given a PermissionDenied hook payload with a top-level agent_id (a tool denied
// inside a sub-agent), when this is adapted to the internal event payload, then
// that id is mapped to PermDenied.AgentID so the pipeline can nest the denied
// span under that sub-agent.
func TestAdapt_PermissionDenied_SubAgentAgentID(t *testing.T) {
	ev, ok := Adapt("PermissionDenied", map[string]any{
		"session_id":  "s",
		"tool_name":   "Bash",
		"tool_input":  map[string]any{"command": "rm -rf /"},
		"tool_use_id": "toolu_9",
		"reason":      "Blocked by classifier",
		"agent_id":    "agent-77",
	})
	require.True(t, ok)
	require.NotNil(t, ev.PermDenied)
	assert.Equal(t, "agent-77", ev.PermDenied.AgentID, "in-sub-agent denial -> AgentID set")
}

// Given a PostToolUseFailure hook payload, when this is adapted to the internal
// event payload, then the event is a failed KindToolCall carrying the error
// string and a tool_error type, with an empty output (no tool_response on
// failure).
func TestAdapt_PostToolUseFailure(t *testing.T) {
	ev, ok := Adapt("PostToolUseFailure", loadPayload(t, "post_tool_use_failure.json"))
	require.True(t, ok)
	assert.Equal(t, event.KindToolCall, ev.Kind)
	require.NotNil(t, ev.ToolCall)
	assert.Equal(t, "Bash", ev.ToolCall.Name)
	assert.JSONEq(t, `{"command":"npm test","description":"Run test suite"}`, ev.ToolCall.Input)
	assert.Empty(t, ev.ToolCall.Output, "failed tool has no tool_response")
	assert.Equal(t, float64(4187), ev.ToolCall.DurationMs)
	assert.True(t, ev.ToolCall.Failed)
	assert.Equal(t, "tool_error", ev.ToolCall.ErrorType)
	assert.Equal(t, "Exit code 1\nError: Cannot find module 'express'", ev.ToolCall.Error)
	assert.Equal(t, "toolu_01ABC", ev.ToolCall.ToolUseID, "tool_use_id extracted on failure too")
}

// Given a PostToolUseFailure hook payload marked is_interrupt, when this is
// adapted to the internal event payload, then the failure's error type is
// "interrupted" rather than "tool_error".
func TestAdapt_PostToolUseFailure_Interrupt(t *testing.T) {
	ev, ok := Adapt("PostToolUseFailure", map[string]any{
		"session_id":   "s",
		"tool_name":    "Bash",
		"tool_input":   map[string]any{"command": "sleep 100"},
		"error":        "Tool aborted",
		"is_interrupt": true,
	})
	require.True(t, ok)
	require.NotNil(t, ev.ToolCall)
	assert.True(t, ev.ToolCall.Failed)
	assert.Equal(t, "interrupted", ev.ToolCall.ErrorType, "is_interrupt -> interrupted")
}

// Given a PostToolUseFailure hook payload with a top-level agent_id (a tool that
// failed inside a sub-agent), when this is adapted to the internal event payload,
// then that id is mapped to ToolCall.AgentID so the failed span nests under the
// sub-agent rather than the turn root.
func TestAdapt_PostToolUseFailure_SubAgentAgentID(t *testing.T) {
	ev, ok := Adapt("PostToolUseFailure", map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]any{"command": "false"},
		"error":      "Exit code 1",
		"agent_id":   "agent-77",
	})
	require.True(t, ok)
	assert.True(t, ev.ToolCall.Failed)
	assert.Equal(t, "agent-77", ev.ToolCall.AgentID, "failed in-sub-agent tool -> AgentID set")
	assert.Empty(t, ev.ToolCall.LaunchedAgentID, "a failure carries no tool_response -> no launched agent id")
}

// Given a main-agent PostToolUseFailure hook payload, when this is adapted to the
// internal event payload, then it carries neither agent field.
func TestAdapt_PostToolUseFailure_MainAgentNoAgentID(t *testing.T) {
	ev, ok := Adapt("PostToolUseFailure", loadPayload(t, "post_tool_use_failure.json"))
	require.True(t, ok)
	assert.Empty(t, ev.ToolCall.AgentID, "no top-level agent_id -> empty")
	assert.Empty(t, ev.ToolCall.LaunchedAgentID, "failure has no launch response -> empty")
}

// Given a StopFailure hook payload, when this is adapted to the internal event
// payload, then the event is a failed KindTurnEnd carrying the error code as
// ErrorType, error_details as the detail, the rendered API error as ResponseText,
// and the transcript readers wired.
func TestAdapt_StopFailure(t *testing.T) {
	ev, ok := Adapt("StopFailure", loadPayload(t, "stop_failure.json"))
	require.True(t, ok)
	assert.Equal(t, event.KindTurnEnd, ev.Kind)
	require.NotNil(t, ev.TurnEnd)
	assert.True(t, ev.TurnEnd.Failed)
	assert.Equal(t, "rate_limit", ev.TurnEnd.ErrorType, "error code -> ErrorType")
	assert.Equal(t, "429 Too Many Requests", ev.TurnEnd.Error, "error_details preferred as detail")
	assert.Equal(t, "API Error: Rate limit reached", ev.TurnEnd.ResponseText)
	assert.NotNil(t, ev.TurnEnd.ReadUsage, "transcript_path present -> ReadUsage set")
	assert.NotNil(t, ev.TurnEnd.ReadCalls, "transcript_path present -> ReadCalls set")
}

// Given a StopFailure hook payload with no error_details, when this is adapted to
// the internal event payload, then the detail falls back to
// last_assistant_message.
func TestAdapt_StopFailure_DetailFallback(t *testing.T) {
	ev, ok := Adapt("StopFailure", map[string]any{
		"session_id":             "s",
		"error":                  "server_error",
		"last_assistant_message": "API Error: Internal server error",
	})
	require.True(t, ok)
	require.NotNil(t, ev.TurnEnd)
	assert.Equal(t, "API Error: Internal server error", ev.TurnEnd.Error,
		"no error_details -> falls back to last_assistant_message")
}

// Given a StopFailure hook payload whose error code is missing (documented as
// always present, but contract drift is possible), when this is adapted to the
// internal event payload, then the turn is still marked failed (graceful
// degradation) and only the error.type classification is dropped, not the failure
// itself.
func TestAdapt_StopFailure_MissingError(t *testing.T) {
	ev, ok := Adapt("StopFailure", map[string]any{
		"session_id":    "s",
		"error_details": "connection reset",
	})
	require.True(t, ok)
	require.NotNil(t, ev.TurnEnd)
	assert.True(t, ev.TurnEnd.Failed, "turn stays failed even without an error code")
	assert.Empty(t, ev.TurnEnd.ErrorType, "missing error -> no classification")
	assert.Equal(t, "connection reset", ev.TurnEnd.Error, "detail still captured")
}

// Given a StopFailure hook payload with neither error_details nor
// last_assistant_message, when this is adapted to the internal event payload,
// then the detail double-fallback lands on "" and ResponseText is empty too.
func TestAdapt_StopFailure_NoDetail(t *testing.T) {
	ev, ok := Adapt("StopFailure", map[string]any{
		"session_id": "s",
		"error":      "server_error",
	})
	require.True(t, ok)
	require.NotNil(t, ev.TurnEnd)
	assert.True(t, ev.TurnEnd.Failed)
	assert.Equal(t, "server_error", ev.TurnEnd.ErrorType)
	assert.Empty(t, ev.TurnEnd.Error, "no error_details or last_assistant_message -> empty detail")
	assert.Empty(t, ev.TurnEnd.ResponseText)
}

// Given a Stop hook payload, when this is adapted to the internal event payload,
// then the event is KindTurnEnd carrying the response text, with ReadUsage,
// ReadOutcomes, and ReadCalls wired from transcript_path.
func TestAdapt_Stop(t *testing.T) {
	ev, ok := Adapt("Stop", loadPayload(t, "stop.json"))
	require.True(t, ok)
	assert.Equal(t, event.KindTurnEnd, ev.Kind)
	require.NotNil(t, ev.TurnEnd)
	assert.Equal(t, "I fixed the failing test.", ev.TurnEnd.ResponseText)
	assert.NotNil(t, ev.TurnEnd.ReadUsage, "transcript_path present -> ReadUsage set")
	assert.NotNil(t, ev.TurnEnd.ReadOutcomes, "transcript_path present -> ReadOutcomes set")
	assert.NotNil(t, ev.TurnEnd.ReadCalls, "transcript_path present -> ReadCalls set")
}

// Given a Stop hook payload with no transcript_path, when this is adapted to the
// internal event payload, then the ReadUsage, ReadOutcomes, and ReadCalls
// closures are left nil.
func TestAdapt_Stop_NoTranscriptPath(t *testing.T) {
	ev, ok := Adapt("Stop", map[string]any{
		"session_id":             "s",
		"last_assistant_message": "done",
	})
	require.True(t, ok)
	require.NotNil(t, ev.TurnEnd)
	assert.Nil(t, ev.TurnEnd.ReadUsage, "no transcript_path -> ReadUsage nil")
	assert.Nil(t, ev.TurnEnd.ReadOutcomes, "no transcript_path -> ReadOutcomes nil")
	assert.Nil(t, ev.TurnEnd.ReadCalls, "no transcript_path -> ReadCalls nil")
}

// Given a SessionEnd hook payload, when this is adapted to the internal event
// payload, then the event is KindSessionEnd carrying session id and cwd, with
// ReadOutcomes and ReadCalls wired from transcript_path and no turn/tool
// payloads.
func TestAdapt_SessionEnd(t *testing.T) {
	ev, ok := Adapt("SessionEnd", map[string]any{
		"session_id": "abc123", "cwd": "/repo",
		"transcript_path": "/home/user/.claude/projects/x/transcript.jsonl",
	})
	require.True(t, ok)
	assert.Equal(t, event.KindSessionEnd, ev.Kind)
	assert.Equal(t, "abc123", ev.SessionID)
	assert.Equal(t, "/repo", ev.Cwd)
	assert.Nil(t, ev.TurnStart)
	assert.Nil(t, ev.TurnEnd)
	assert.Nil(t, ev.ToolCall)
	require.NotNil(t, ev.SessionEnd)
	assert.NotNil(t, ev.SessionEnd.ReadOutcomes, "transcript_path present -> ReadOutcomes set")
	assert.NotNil(t, ev.SessionEnd.ReadCalls, "transcript_path present -> ReadCalls set")
}

// Given a Stop hook payload whose transcript holds one assistant message, when
// the callsReader closure wired onto its adapted event runs, then that call is
// mapped into an internal event.LLMCall, carrying the rendered messages and
// per-call usage verbatim.
func TestAdapt_Stop_ReadCallsMapsTranscript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	lines := "" +
		`{"type":"user","timestamp":"2026-07-08T03:30:50.000Z","message":{"content":"add logging"}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-07-08T03:30:56.000Z","message":{"id":"msg_A","model":"claude-sonnet-4","stop_reason":"end_turn","usage":{"input_tokens":1200,"output_tokens":500,"cache_read_input_tokens":300,"cache_creation_input_tokens":100},"content":[{"type":"text","text":"done"}]}}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(lines), 0o600))

	ev, ok := Adapt("Stop", map[string]any{
		"session_id":             "s",
		"transcript_path":        path,
		"last_assistant_message": "done",
	})
	require.True(t, ok)
	require.NotNil(t, ev.TurnEnd.ReadCalls)

	// A start before every entry: the single assistant message becomes one call.
	calls := ev.TurnEnd.ReadCalls("1")
	require.Len(t, calls, 1)
	c := calls[0]
	assert.Equal(t, "msg_A", c.MessageID)
	assert.Equal(t, int64(1200), c.Usage.InputTokens)
	assert.Equal(t, int64(500), c.Usage.OutputTokens)
	assert.Equal(t, int64(300), c.Usage.CacheReadTokens)
	assert.Equal(t, int64(100), c.Usage.CacheCreationTokens)
	assert.Equal(t, "claude-sonnet-4", c.Usage.Model)
	assert.Equal(t, "end_turn", c.Usage.StopReason)
	require.Len(t, c.InputMessages, 1)
	assert.Contains(t, c.InputMessages[0], "add logging")
	assert.Contains(t, c.Output, "done")
	assert.Equal(t, "done", c.OutputText, "visible response text carried through")
	assert.Equal(t, "add logging", c.InputText, "newest user message carried through")
}

// Given a Stop hook payload whose transcript reconstructs no calls (empty/
// lagging), when the callsReader closure on its adapted event runs, then it
// returns a nil slice, so the pipeline takes its aggregated fallback.
func TestAdapt_Stop_ReadCallsEmptyIsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(""), 0o600))

	ev, ok := Adapt("Stop", map[string]any{"session_id": "s", "transcript_path": path})
	require.True(t, ok)
	require.NotNil(t, ev.TurnEnd.ReadCalls)
	assert.Nil(t, ev.TurnEnd.ReadCalls("1"), "no reconstructable calls -> nil, not empty slice")
}

// Given a main-agent PostToolUse hook payload, when this is adapted to the
// internal event payload, then the event carries neither agent field.
func TestAdapt_PostToolUse_MainAgentNoAgentID(t *testing.T) {
	ev, ok := Adapt("PostToolUse", loadPayload(t, "post_tool_use.json"))
	require.True(t, ok)
	assert.Empty(t, ev.ToolCall.AgentID, "no top-level agent_id -> empty")
	assert.Empty(t, ev.ToolCall.LaunchedAgentID, "non-launch response -> empty")
}

// Given a PostToolUse hook payload with a top-level agent_id (a tool run inside a
// sub-agent), when this is adapted to the internal event payload, then that id is
// mapped to ToolCall.AgentID.
func TestAdapt_PostToolUse_SubAgentAgentID(t *testing.T) {
	ev, ok := Adapt("PostToolUse", map[string]any{
		"tool_name":  "Read",
		"tool_input": map[string]any{"file_path": "/x"},
		"agent_id":   "agent-77",
	})
	require.True(t, ok)
	assert.Equal(t, "agent-77", ev.ToolCall.AgentID, "in-sub-agent tool -> AgentID set")
	assert.Empty(t, ev.ToolCall.LaunchedAgentID)
}

// Given an Agent launch's PostToolUse hook payload, when this is adapted to the
// internal event payload, then its tool_response.agentId is mapped to
// ToolCall.LaunchedAgentID, identifying the sub-agent the call spawned.
func TestAdapt_PostToolUse_AgentLaunch(t *testing.T) {
	ev, ok := Adapt("PostToolUse", map[string]any{
		"tool_name":     "Agent",
		"tool_input":    map[string]any{"subagent_type": "Explore", "prompt": "find X"},
		"tool_response": map[string]any{"agentId": "agent-99", "status": "async_launched"},
	})
	require.True(t, ok)
	assert.Equal(t, "Agent", ev.ToolCall.Name)
	assert.Equal(t, "agent-99", ev.ToolCall.LaunchedAgentID, "launch response agentId -> LaunchedAgentID")
	assert.Empty(t, ev.ToolCall.AgentID, "the launcher is the main agent -> no top-level agent_id")
}

// Given a SubagentStop hook payload, when this is adapted to the internal event
// payload, then it maps to KindSubagentEnd carrying the sub-agent's identity, its
// final answer, and closures over its dedicated transcript; and when those reader
// closures run, then they read the file wholesale — including its isSidechain
// rows.
func TestAdapt_SubagentStop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-abc.jsonl")
	lines := "" +
		`{"type":"user","timestamp":"2026-07-08T03:31:00.000Z","isSidechain":true,"message":{"content":"find the bug"}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-07-08T03:31:05.000Z","isSidechain":true,"message":{"id":"msg_S","model":"claude-sonnet-4","stop_reason":"end_turn","usage":{"input_tokens":800,"output_tokens":200},"content":[{"type":"text","text":"found it"}]}}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(lines), 0o600))

	ev, ok := Adapt("SubagentStop", map[string]any{
		"session_id":             "s",
		"cwd":                    "/repo",
		"agent_id":               "agent-abc",
		"agent_type":             "Explore",
		"agent_transcript_path":  path,
		"last_assistant_message": "found it",
	})
	require.True(t, ok)
	assert.Equal(t, event.KindSubagentEnd, ev.Kind)
	assert.Nil(t, ev.ToolCall)
	assert.Nil(t, ev.TurnEnd)
	require.NotNil(t, ev.SubagentEnd)
	se := ev.SubagentEnd
	assert.Equal(t, "agent-abc", se.AgentID)
	assert.Equal(t, "Explore", se.AgentType)
	assert.Equal(t, "found it", se.ResponseText)
	require.NotNil(t, se.ReadStart)
	require.NotNil(t, se.ReadCalls)
	require.NotNil(t, se.ReadUsage)

	start := se.ReadStart()
	assert.NotEmpty(t, start, "ReadStart returns the sub-agent's first-entry timestamp")

	calls := se.ReadCalls(start)
	require.Len(t, calls, 1, "the sub-agent's assistant message is reconstructed from its dedicated file")
	assert.Equal(t, "msg_S", calls[0].MessageID)
	assert.Equal(t, int64(800), calls[0].Usage.InputTokens)

	usage := se.ReadUsage(start)
	assert.Equal(t, int64(800), usage.InputTokens)
	assert.Equal(t, int64(200), usage.OutputTokens)
}

// Given a SubagentStop hook payload without a transcript path, when this is
// adapted to the internal event payload, then the reader closures are left nil,
// so the pipeline degrades to last_assistant_message alone.
func TestAdapt_SubagentStop_NoTranscriptPath(t *testing.T) {
	ev, ok := Adapt("SubagentStop", map[string]any{"session_id": "s", "agent_id": "agent-x"})
	require.True(t, ok)
	require.NotNil(t, ev.SubagentEnd)
	assert.Nil(t, ev.SubagentEnd.ReadStart)
	assert.Nil(t, ev.SubagentEnd.ReadCalls)
	assert.Nil(t, ev.SubagentEnd.ReadUsage)
}

// Given a hook event the plugin does not capture, when it is passed to the
// adapter, then it returns ok=false and no event (nothing is adapted).
func TestAdapt_UnknownEvent(t *testing.T) {
	_, ok := Adapt("SessionStart", map[string]any{"session_id": "abc123"})
	assert.False(t, ok, "uncaptured events return ok=false")
}

// Given PostToolUse hook payloads whose tool responses are of different shapes (a
// plain string, or a map with stdout/stderr), when each is adapted to the
// internal event payload, then the scannable output text is extracted
// accordingly.
func TestAdapt_ToolResponseVariants(t *testing.T) {
	t.Run("plain string response", func(t *testing.T) {
		ev, ok := Adapt("PostToolUse", map[string]any{
			"session_id":    "s",
			"tool_name":     "Read",
			"tool_input":    map[string]any{"file": "a.go"},
			"tool_response": "line1\nline2",
		})
		require.True(t, ok)
		assert.Equal(t, "line1\nline2", ev.ToolCall.Output)
	})

	t.Run("stderr included", func(t *testing.T) {
		ev, ok := Adapt("PostToolUse", map[string]any{
			"session_id":    "s",
			"tool_name":     "Bash",
			"tool_input":    "cmd",
			"tool_response": map[string]any{"stdout": "out", "stderr": "err"},
		})
		require.True(t, ok)
		assert.Equal(t, "out\nerr", ev.ToolCall.Output)
	})
}

// Given a hook payload missing its expected fields, when this is adapted to the
// internal event payload, then it still returns ok=true with the corresponding
// event fields left empty.
func TestAdapt_MissingFields(t *testing.T) {
	ev, ok := Adapt("UserPromptSubmit", map[string]any{})
	require.True(t, ok)
	assert.Empty(t, ev.SessionID)
	assert.Empty(t, ev.TurnStart.Prompt)
}
