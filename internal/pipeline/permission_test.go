package pipeline

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/agentctx"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/event"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/otlp"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/toolctx"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/turnctx"
)

// Given a PreToolUse event, when it is processed, then no span is emitted and a
// pending record is written carrying the tool name, permission mode, and a
// pre-execution timestamp.
func TestPreToolUse_WritesPendingNoSpan(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)
	sess := "sess-pre"

	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindPreToolUse, SessionID: sess,
		PreTool: &event.PreToolCall{ToolUseID: "toolu_1", Name: "Bash", Input: `{"command":"ls"}`, PermissionMode: "default"},
	}, cfg)

	// No span from PreToolUse (only the nothing-emitted TurnStart precedes it).
	_, ok := c.byName("execute_tool")
	assert.False(t, ok, "PreToolUse emits no span")

	rec, ok := toolctx.Read(sess, "toolu_1")
	require.True(t, ok, "pending record written")
	assert.Equal(t, "Bash", rec.ToolName)
	assert.Equal(t, "default", rec.PermissionMode)
	assert.NotEmpty(t, rec.PreNano)
}

// Given a PreToolUse event with no tool_use_id, when it is processed, then no
// record is written and no panic occurs (there is nothing to key on).
func TestPreToolUse_NoIDSkips(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	cfg := testConfig("http://127.0.0.1:1")
	assert.NotPanics(t, func() {
		Process(event.Event{
			Kind: event.KindPreToolUse, SessionID: "s",
			PreTool: &event.PreToolCall{ToolUseID: "", Name: "Bash"},
		}, cfg)
	})
	assert.Empty(t, toolctx.List("s"), "no record written without a tool_use_id")
}

// Given a PreToolUse followed by a PostToolUse for the same id, when the call is
// processed, then the execute_tool span carries decision=accept, the permission
// mode, and an estimated wait, and the pending record is deleted.
func TestPermission_AcceptStampsAttributes(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)
	sess := "sess-accept"

	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindPreToolUse, SessionID: sess,
		PreTool: &event.PreToolCall{ToolUseID: "toolu_1", Name: "Bash", Input: `{"command":"ls"}`, PermissionMode: "default"},
	}, cfg)
	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{Name: "Bash", Input: `{"command":"ls"}`, Output: "file1.txt", DurationMs: 10, ToolUseID: "toolu_1", PermissionMode: "default"},
	}, cfg)

	tool, ok := c.byName("execute_tool")
	require.True(t, ok)
	m := attrMap(tool)
	assert.Equal(t, "accept", m["fiddler.coding_agent.permission.decision"])
	assert.Equal(t, "default", m["fiddler.coding_agent.permission.mode"])
	assert.Equal(t, "estimated", m["fiddler.coding_agent.permission.wait.source"])
	_, hasWait := intAttrOK(tool, "fiddler.coding_agent.permission.wait_ms")
	assert.True(t, hasWait, "wait_ms present when pending record exists")

	_, stillPending := toolctx.Read(sess, "toolu_1")
	assert.False(t, stillPending, "pending record deleted after resolution")
}

// Given a pending record whose pre-time is 5s ago and a PostToolUse with
// duration_ms=1000, when it is processed, then the estimated wait_ms is
// approximately elapsed - duration ≈ 4000.
func TestPermission_AcceptWaitEstimate(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)
	sess := "sess-wait"

	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)

	// Control the pre time: 5s ago. With duration_ms=1000, the estimated wait
	// is (now - pre) - duration ≈ 5000 - 1000 = 4000ms.
	preNano := fmt.Sprintf("%d", time.Now().Add(-5*time.Second).UnixNano())
	require.NoError(t, toolctx.Write(sess, "toolu_1", toolctx.Record{ToolName: "Bash", PreNano: preNano, PermissionMode: "default"}))

	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{Name: "Bash", Input: "x", Output: "y", DurationMs: 1000, ToolUseID: "toolu_1"},
	}, cfg)

	tool, ok := c.byName("execute_tool")
	require.True(t, ok)
	raw, ok := intAttrOK(tool, "fiddler.coding_agent.permission.wait_ms")
	require.True(t, ok)
	ms, err := strconv.ParseInt(raw, 10, 64)
	require.NoError(t, err)
	assert.InDelta(t, 4000, ms, 500, "wait ≈ (now-pre) - duration_ms")
}

// Given a PostToolUse with no preceding PreToolUse, when it is processed, then
// the execute_tool span still emits with decision=accept and the payload's mode,
// and wait_ms is omitted.
func TestPermission_MissingPendingDegrades(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)
	sess := "sess-nopending"

	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	// No PreToolUse -> no pending record.
	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{Name: "Bash", Input: "x", Output: "y", DurationMs: 10, ToolUseID: "toolu_1", PermissionMode: "acceptEdits"},
	}, cfg)

	tool, ok := c.byName("execute_tool")
	require.True(t, ok, "tool span still emitted without a pending record")
	m := attrMap(tool)
	assert.Equal(t, "accept", m["fiddler.coding_agent.permission.decision"])
	assert.Equal(t, "acceptEdits", m["fiddler.coding_agent.permission.mode"], "mode from payload")
	_, hasWait := intAttrOK(tool, "fiddler.coding_agent.permission.wait_ms")
	assert.False(t, hasWait, "wait omitted without a pending record")
}

// Given a PreToolUse then a PostToolUseFailure for the same id, when it is
// processed, then the span is ERROR with error.type=tool_error and still carries
// decision=accept, because permission was granted and only execution failed.
func TestPermission_PostToolUseFailureStillAccept(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)
	sess := "sess-failaccept"

	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindPreToolUse, SessionID: sess,
		PreTool: &event.PreToolCall{ToolUseID: "toolu_1", Name: "Bash", PermissionMode: "default"},
	}, cfg)
	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{Name: "Bash", Input: "x", DurationMs: 10, ToolUseID: "toolu_1", PermissionMode: "default",
			Failed: true, ErrorType: "tool_error", Error: "Exit code 1"},
	}, cfg)

	tool, ok := c.byName("execute_tool")
	require.True(t, ok)
	assert.Equal(t, otlp.SpanStatusError, tool.Status.Code, "execution failure marks ERROR")
	m := attrMap(tool)
	assert.Equal(t, "tool_error", m["error.type"])
	assert.Equal(t, "accept", m["fiddler.coding_agent.permission.decision"], "permission was granted; only execution failed")
}

// Given a PreToolUse then a PermissionDenied for the same id, when it is
// processed, then an execute_tool span is emitted with ERROR/permission_denied,
// decision=reject, denial_kind=auto-classifier, the reason, and no result, and
// the pending record is deleted.
func TestPermission_AutoDenyEmitsRejectSpan(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)
	sess := "sess-autodeny"

	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindPreToolUse, SessionID: sess,
		PreTool: &event.PreToolCall{ToolUseID: "toolu_1", Name: "Bash", Input: `{"command":"rm -rf /tmp/x"}`, PermissionMode: "auto"},
	}, cfg)
	Process(event.Event{
		Kind: event.KindPermissionDenied, SessionID: sess,
		PermDenied: &event.PermissionDenied{ToolUseID: "toolu_1", Name: "Bash", Input: `{"command":"rm -rf /tmp/x"}`, Reason: "Blocked by classifier", PermissionMode: "auto"},
	}, cfg)

	tool, ok := c.byName("execute_tool")
	require.True(t, ok, "auto-deny emits a rejected execute_tool span")
	assert.Equal(t, otlp.SpanStatusError, tool.Status.Code)
	m := attrMap(tool)
	assert.Equal(t, "permission_denied", m["error.type"])
	assert.Equal(t, "reject", m["fiddler.coding_agent.permission.decision"])
	assert.Equal(t, "auto-classifier", m["fiddler.coding_agent.permission.denial_kind"])
	assert.Equal(t, "Blocked by classifier", m["fiddler.coding_agent.permission.denial_reason"])
	assert.Equal(t, "auto", m["fiddler.coding_agent.permission.mode"])
	assert.Equal(t, "toolu_1", m["gen_ai.tool.call.id"])
	assert.Empty(t, m["gen_ai.tool.call.result"], "denied tool produced no result")

	_, stillPending := toolctx.Read(sess, "toolu_1")
	assert.False(t, stillPending, "pending record deleted after denial")
}

// Given an async sub-agent whose auto-denied tool arrives after the launching
// turn's Stop cleared turnctx, when the PermissionDenied is processed, then the
// denied execute_tool span reuses the launching turn's trace (from agentctx, NOT
// a lazily-minted one) and parents to the sub-agent root S(agent_id) — the same
// nesting the sub-agent's successful tool spans get. No stray turn context is
// minted, so an async sub-agent's denial stays in the launching trace.
func TestPermission_AutoDeny_SubAgentNestsUnderLaunchTrace(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-autodeny-subagent"
	// Turn 1: launch the sub-agent, then Stop (clears turnctx).
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{Name: "Agent", LaunchedAgentID: "agent-1"},
	}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "done"}}, cfg)

	root, ok := c.byName("invoke_agent")
	require.True(t, ok)
	launchTrace := root.TraceID

	// Async: the sub-agent's tool is auto-denied AFTER turn 1's Stop. turnctx is
	// cleared, so a bare turnctx.Load would lazily mint a different trace.
	Process(event.Event{
		Kind: event.KindPermissionDenied, SessionID: sess,
		PermDenied: &event.PermissionDenied{ToolUseID: "toolu_1", Name: "Bash", Input: `{"command":"rm -rf /"}`, Reason: "Blocked by classifier", PermissionMode: "auto", AgentID: "agent-1"},
	}, cfg)

	denied, ok := execToolByToolName(c, "Bash", "")
	require.True(t, ok, "sub-agent denial emits a rejected execute_tool span")
	assert.Equal(t, otlp.SpanStatusError, denied.Status.Code)
	assert.Equal(t, "permission_denied", attrMap(denied)["error.type"])
	assert.Equal(t, launchTrace, denied.TraceID,
		"denied span reuses the launching turn's trace, not a lazily-minted one")
	assert.Equal(t, otlp.SpanIDFrom(launchTrace, "subagent:agent-1"), denied.ParentSpanID,
		"denied span parents to the sub-agent root S(agent_id)")

	_, minted := turnctx.Peek(sess)
	assert.False(t, minted, "no stray turn context minted for the async denial")
}

// Given a denial tagged with an agent_id for which no agentctx record exists (a
// missed launch), when it is processed, then the denied span degrades to the
// turn root in the turn's trace rather than deriving against a wrong trace —
// still fail-open.
func TestPermission_AutoDeny_AgentMissDegradesToRoot(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-autodeny-miss"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindPermissionDenied, SessionID: sess,
		PermDenied: &event.PermissionDenied{ToolUseID: "toolu_1", Name: "Bash", Reason: "Blocked", PermissionMode: "auto", AgentID: "unknown-agent"},
	}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "x"}}, cfg)

	root, ok := c.byName("invoke_agent")
	require.True(t, ok)
	denied, ok := execToolByToolName(c, "Bash", "")
	require.True(t, ok)
	// No agentctx record for "unknown-agent" was ever written.
	_, hasRec := agentctx.Read(sess, "unknown-agent")
	require.False(t, hasRec, "precondition: no launch record")
	assert.Equal(t, root.TraceID, denied.TraceID, "degrades into the turn's trace")
	assert.Equal(t, root.SpanID, denied.ParentSpanID, "degrades to the turn root, not a derived sub-agent parent")
}

// Given a completed turn and an auto-denial tagged with an unknown sub-agent that
// arrives after that turn has ended (the async case), when it
// is processed, then no span is emitted and — crucially — no stray turn context
// is minted. A bare turnctx.Load would lazily mint and persist one here, seeding
// a phantom recovered root; turnRootIfOpen (Peek-only) prevents that.
func TestPermission_AutoDeny_AsyncMiss_SkipsNoMint(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-autodeny-async-miss"
	// A turn runs to completion; no sub-agent is ever launched.
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "done"}}, cfg)

	_, hasCtx := turnctx.Peek(sess)
	require.False(t, hasCtx, "precondition: the turn has ended")
	_, hasRec := agentctx.Read(sess, "ghost")
	require.False(t, hasRec, "precondition: no launch record")

	// The auto-denial, tagged with an unknown sub-agent, arrives after the turn ended.
	Process(event.Event{
		Kind: event.KindPermissionDenied, SessionID: sess,
		PermDenied: &event.PermissionDenied{ToolUseID: "toolu_1", Name: "Bash", Reason: "Blocked", PermissionMode: "auto", AgentID: "ghost"},
	}, cfg)

	_, emitted := execToolByToolName(c, "Bash", "")
	assert.False(t, emitted, "no span for a sub-agent denial that arrives after its turn ended")
	_, minted := turnctx.Peek(sess)
	assert.False(t, minted, "no stray turn context minted for the async denial")
}

// Given a main-agent auto-denial (no agent_id), when it is processed, then the
// denied span nests under the turn root in the turn's trace, unchanged by the
// agentctx routing (regression guard for sub-agent routing).
func TestPermission_AutoDeny_MainAgentParentsToTurnRoot(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-autodeny-main"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindPermissionDenied, SessionID: sess,
		PermDenied: &event.PermissionDenied{ToolUseID: "toolu_1", Name: "Bash", Reason: "Blocked", PermissionMode: "auto"},
	}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "x"}}, cfg)

	root, ok := c.byName("invoke_agent")
	require.True(t, ok)
	denied, ok := execToolByToolName(c, "Bash", "")
	require.True(t, ok)
	assert.Equal(t, root.TraceID, denied.TraceID, "main-agent denial stays in the turn's trace")
	assert.Equal(t, root.SpanID, denied.ParentSpanID, "main-agent denial parents to the turn root")
}

// intAttrOK reads an int-typed attribute value without requiring *testing.T,
// returning ok=false when absent.
func intAttrOK(s otlp.Span, key string) (string, bool) {
	for _, a := range s.Attributes {
		if a.Key == key && a.Value.IntValue != nil {
			return *a.Value.IntValue, true
		}
	}
	return "", false
}
