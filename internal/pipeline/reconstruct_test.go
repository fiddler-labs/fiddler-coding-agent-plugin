package pipeline

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/event"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/otlp"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/toolctx"
)

// toolByID returns the execute_tool span carrying the given gen_ai.tool.call.id.
func toolByID(c *collector, id string) (otlp.Span, bool) {
	for _, s := range c.all() {
		if s.Name == "execute_tool" && attrMap(s)["gen_ai.tool.call.id"] == id {
			return s, true
		}
	}
	return otlp.Span{}, false
}

// outcomes builds a ReadOutcomes closure returning a fixed map (ignores start).
func outcomes(m map[string]event.ToolOutcome) func(string) map[string]event.ToolOutcome {
	return func(string) map[string]event.ToolOutcome { return m }
}

// Given a turn with a user-denied call, an abandoned call, and a call that ran,
// when Stop classifies the leftover pending records, then the denial becomes a
// reject span, the abandoned call an unresolved span, the ran call is not
// re-emitted, and the pending records are swept.
func TestReconstruct_TurnEnd_AllBuckets(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)
	sess := "sess-recon"

	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	for _, id := range []string{"toolu_deny", "toolu_abandon", "toolu_ran"} {
		Process(event.Event{Kind: event.KindPreToolUse, SessionID: sess,
			PreTool: &event.PreToolCall{ToolUseID: id, Name: "Bash", Input: "x", PermissionMode: "default"}}, cfg)
	}
	// toolu_ran resolves live (deletes its pending, emits an accept span).
	Process(event.Event{Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{Name: "Bash", Input: "x", Output: "ok", DurationMs: 5, ToolUseID: "toolu_ran", PermissionMode: "default"}}, cfg)

	resultNano := fmt.Sprintf("%d", time.Now().UnixNano())
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{
		ResponseText: "done",
		ReadOutcomes: outcomes(map[string]event.ToolOutcome{
			"toolu_deny": {Kind: event.OutcomeDeniedUser, ResultNano: resultNano},
			"toolu_ran":  {Kind: event.OutcomeResolved},
		}),
	}}, cfg)

	deny, ok := toolByID(c, "toolu_deny")
	require.True(t, ok, "denied call is reconstructed as a reject span")
	assert.Equal(t, otlp.SpanStatusError, deny.Status.Code)
	dm := attrMap(deny)
	assert.Equal(t, "permission_denied", dm["error.type"])
	assert.Equal(t, "reject", dm["fiddler.coding_agent.permission.decision"])
	assert.Equal(t, "user-rejected", dm["fiddler.coding_agent.permission.denial_kind"])

	abandon, ok := toolByID(c, "toolu_abandon")
	require.True(t, ok, "abandoned call is reconstructed as an unresolved span")
	assert.Equal(t, otlp.SpanStatusUnset, abandon.Status.Code, "unresolved is UNSET, not ERROR")
	assert.Equal(t, "unresolved", attrMap(abandon)["fiddler.coding_agent.permission.decision"])
	assert.Empty(t, attrMap(abandon)["error.type"], "unresolved is not an error")

	ran, ok := toolByID(c, "toolu_ran")
	require.True(t, ok, "the ran call was emitted live")
	assert.Equal(t, "accept", attrMap(ran)["fiddler.coding_agent.permission.decision"])

	// Exactly one span per tool id (the ran call not duplicated).
	var toolSpans int
	for _, s := range c.all() {
		if s.Name == "execute_tool" {
			toolSpans++
		}
	}
	assert.Equal(t, 3, toolSpans, "one span each for ran/deny/abandon, no duplicates")

	// All reconstructed spans parent to the turn root.
	root, ok := c.byName("invoke_agent")
	require.True(t, ok)
	assert.Equal(t, root.SpanID, deny.ParentSpanID)
	assert.Equal(t, root.SpanID, abandon.ParentSpanID)

	assert.Empty(t, toolctx.List(sess), "pending records swept after turn end")
}

// Given a leftover pending record whose transcript outcome is Resolved (e.g. an
// auto-deny already emitted live whose pending delete failed), when Stop runs,
// then it is skipped, not re-emitted as an unresolved span.
func TestReconstruct_ResolvedLeftoverNotReemitted(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)
	sess := "sess-resolved-leftover"

	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	// A stray pending record whose call actually resolved (transcript says so).
	require.NoError(t, toolctx.Write(sess, "toolu_x", toolctx.Record{ToolName: "Bash", PreNano: "1"}))
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{
		ReadOutcomes: outcomes(map[string]event.ToolOutcome{"toolu_x": {Kind: event.OutcomeResolved}}),
	}}, cfg)

	_, ok := toolByID(c, "toolu_x")
	assert.False(t, ok, "a resolved leftover is not reconstructed")
}

// Given an auto-classifier denial already emitted live by handlePermissionDenied
// (which deletes its pending record), when Stop reconstructs leftovers and the
// same id appears in the transcript outcomes as Resolved (an auto-classifier
// denial never records toolDenialKind user-rejected/permission-rule), then it is
// not re-emitted: exactly one reject span exists for that id, from the live hook.
// This guards the second (no-pending-record) reconstruction pass against
// double-emitting a live-hooked denial.
func TestReconstruct_LiveAutoDenyNotReemitted(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)
	sess := "sess-live-autodeny"

	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{Kind: event.KindPreToolUse, SessionID: sess,
		PreTool: &event.PreToolCall{ToolUseID: "toolu_auto", Name: "Bash", Input: `{"command":"rm"}`, PermissionMode: "auto"}}, cfg)
	// Live auto-classifier denial: emits the reject span and deletes the pending.
	Process(event.Event{Kind: event.KindPermissionDenied, SessionID: sess,
		PermDenied: &event.PermissionDenied{ToolUseID: "toolu_auto", Name: "Bash", Input: `{"command":"rm"}`, Reason: "Blocked by classifier", PermissionMode: "auto"}}, cfg)
	require.False(t, func() bool { _, ok := toolctx.Read(sess, "toolu_auto"); return ok }(), "pending deleted by live denial")

	// At Stop the transcript classifies the auto-classifier denial as Resolved
	// (its tool_result carries no user-rejected/permission-rule kind).
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{
		ReadOutcomes: outcomes(map[string]event.ToolOutcome{"toolu_auto": {Kind: event.OutcomeResolved}}),
	}}, cfg)

	// Exactly one execute_tool span for the id, and it is the live auto-classifier
	// one — not re-emitted by the reconstruction pass.
	var spans []otlp.Span
	for _, s := range c.all() {
		if s.Name == "execute_tool" && attrMap(s)["gen_ai.tool.call.id"] == "toolu_auto" {
			spans = append(spans, s)
		}
	}
	require.Len(t, spans, 1, "live-hooked auto-deny is not re-emitted at Stop")
	assert.Equal(t, "auto-classifier", attrMap(spans[0])["fiddler.coding_agent.permission.denial_kind"])
}

// Given a denial that appears in the transcript with no pending record (a missed
// PreToolUse), when Stop runs, then it is still reconstructed as a reject span
// using the transcript's name and input.
func TestReconstruct_DenialWithoutPending(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)
	sess := "sess-denial-nopending"

	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{
		ReadOutcomes: outcomes(map[string]event.ToolOutcome{
			"toolu_orphan": {Kind: event.OutcomeDeniedRule, Name: "Bash", Input: `{"command":"rm"}`},
		}),
	}}, cfg)

	span, ok := toolByID(c, "toolu_orphan")
	require.True(t, ok, "a transcript-only denial is reconstructed")
	m := attrMap(span)
	assert.Equal(t, "reject", m["fiddler.coding_agent.permission.decision"])
	assert.Equal(t, "permission-rule", m["fiddler.coding_agent.permission.denial_kind"])
	assert.Equal(t, "Bash", m["gen_ai.tool.name"], "name recovered from transcript")
}

// Given an interrupted turn with a denied leftover, when the next UserPromptSubmit
// flushes it, then the recovered root and a reject span are emitted (parented to
// the stale root) and the pending records are swept.
func TestReconstruct_FlushAtNextTurnStart(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)
	sess := "sess-flush-turnstart"

	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "first"}}, cfg)
	Process(event.Event{Kind: event.KindPreToolUse, SessionID: sess,
		PreTool: &event.PreToolCall{ToolUseID: "toolu_deny", Name: "Edit", Input: "x", PermissionMode: "default"}}, cfg)
	// No Stop — the turn is interrupted. The next prompt flushes it, and its
	// ReadOutcomes classifies the stale turn's leftover as a user denial.
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{
		Prompt:       "second",
		ReadOutcomes: outcomes(map[string]event.ToolOutcome{"toolu_deny": {Kind: event.OutcomeDeniedUser}}),
	}}, cfg)

	deny, ok := toolByID(c, "toolu_deny")
	require.True(t, ok, "denied leftover reconstructed at the flush")
	assert.Equal(t, "reject", attrMap(deny)["fiddler.coding_agent.permission.decision"])

	// It parents to the stale (recovered) root, tagged fiddler.coding_agent.turn.recovered.
	var recoveredRoot otlp.Span
	for _, s := range c.all() {
		if s.Name == "invoke_agent" && boolAttr(t, s, "fiddler.coding_agent.turn.recovered") {
			recoveredRoot = s
		}
	}
	require.NotEmpty(t, recoveredRoot.SpanID, "recovered root emitted")
	assert.Equal(t, recoveredRoot.SpanID, deny.ParentSpanID)

	assert.Empty(t, toolctx.List(sess), "pending swept after flush")
}

// Given a turn flushed at the next UserPromptSubmit, when SessionEnd later fires,
// then the same turn is not flushed again (its context was overwritten), so no
// duplicate reject span is emitted.
func TestReconstruct_NoDoubleFlush(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)
	sess := "sess-nodouble"

	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "first"}}, cfg)
	Process(event.Event{Kind: event.KindPreToolUse, SessionID: sess,
		PreTool: &event.PreToolCall{ToolUseID: "toolu_deny", Name: "Edit", PermissionMode: "default"}}, cfg)
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{
		Prompt:       "second",
		ReadOutcomes: outcomes(map[string]event.ToolOutcome{"toolu_deny": {Kind: event.OutcomeDeniedUser}}),
	}}, cfg)
	// Session ends. The second turn has no tool calls of its own, and the real
	// classifier is time-windowed to that turn (the first turn's denial is
	// before the second turn's start), so SessionEnd sees no outcomes.
	Process(event.Event{Kind: event.KindSessionEnd, SessionID: sess, SessionEnd: &event.SessionEnd{
		ReadOutcomes: outcomes(map[string]event.ToolOutcome{}),
	}}, cfg)

	var denySpans int
	for _, s := range c.all() {
		if s.Name == "execute_tool" && attrMap(s)["gen_ai.tool.call.id"] == "toolu_deny" {
			denySpans++
		}
	}
	assert.Equal(t, 1, denySpans, "the denied call is reconstructed exactly once")
}

// Given an interrupted turn still open at session end, when SessionEnd flushes
// it, then its abandoned leftover is reconstructed as an unresolved span.
func TestReconstruct_FlushAtSessionEnd(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)
	sess := "sess-flush-sessionend"

	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{Kind: event.KindPreToolUse, SessionID: sess,
		PreTool: &event.PreToolCall{ToolUseID: "toolu_abandon", Name: "Bash", PermissionMode: "default"}}, cfg)
	Process(event.Event{Kind: event.KindSessionEnd, SessionID: sess, SessionEnd: &event.SessionEnd{
		ReadOutcomes: outcomes(map[string]event.ToolOutcome{}), // no result -> unresolved
	}}, cfg)

	span, ok := toolByID(c, "toolu_abandon")
	require.True(t, ok, "abandoned leftover reconstructed at SessionEnd")
	assert.Equal(t, otlp.SpanStatusUnset, span.Status.Code)
	assert.Equal(t, "unresolved", attrMap(span)["fiddler.coding_agent.permission.decision"])
	assert.Empty(t, toolctx.List(sess), "pending swept at SessionEnd")
}
