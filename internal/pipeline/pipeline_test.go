package pipeline

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/agentctx"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/config"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/event"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/otlp"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/turnctx"
)

// collector is an httptest server that captures every span POSTed to it.
type collector struct {
	srv   *httptest.Server
	mu    sync.Mutex
	spans []otlp.Span
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	c := &collector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The exporter gzips every body (Content-Encoding: gzip); mirror a real
		// OTLP endpoint by decompressing, which also asserts the header is set.
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			require.NoError(t, err)
			defer func() { _ = zr.Close() }()
			body = zr
		} else {
			t.Errorf("expected Content-Encoding: gzip, got %q", r.Header.Get("Content-Encoding"))
		}
		var payload otlp.ExportPayload
		require.NoError(t, json.NewDecoder(body).Decode(&payload))
		c.mu.Lock()
		for _, rs := range payload.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				c.spans = append(c.spans, ss.Spans...)
			}
		}
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *collector) byName(name string) (otlp.Span, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.spans {
		if s.Name == name {
			return s, true
		}
	}
	return otlp.Span{}, false
}

func (c *collector) all() []otlp.Span {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]otlp.Span(nil), c.spans...)
}

func attrMap(s otlp.Span) map[string]string {
	m := make(map[string]string, len(s.Attributes))
	for _, a := range s.Attributes {
		if a.Value.StringValue != nil {
			m[a.Key] = *a.Value.StringValue
		}
	}
	return m
}

func testConfig(endpoint string) *config.Config {
	return &config.Config{
		Endpoint:  endpoint,
		APIKey:    "tok",
		AppID:     "app",
		AgentName: "test-agent",
		Provider:  "test-provider",
	}
}

// The pipeline is runtime-agnostic: these tests drive it with neutral
// event.Event values, not a specific runtime's payloads.

// Given a full turn (TurnStart, one tool call, TurnEnd), when processed, then
// three correctly-nested spans are emitted in one trace with gen_ai attributes
// and runtime identity, the tool duration reflects DurationMs, and the context
// file exists mid-turn and is cleared after TurnEnd.
func TestFullTurn(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CLAUDE_PLUGIN_DATA", dataDir)
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-full"
	Process(event.Event{
		Kind: event.KindTurnStart, SessionID: sess, Cwd: "/repo",
		TurnStart: &event.TurnStart{Prompt: "fix the bug"},
	}, cfg)

	// Context file exists mid-turn.
	ctxPath := filepath.Join(dataDir, "context", sess+".json")
	_, err := os.Stat(ctxPath)
	require.NoError(t, err, "context file should exist after TurnStart")

	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess, Cwd: "/repo",
		ToolCall: &event.ToolCall{Name: "Bash", Input: `{"command":"ls"}`, Output: "file1.txt", DurationMs: 1500, ToolUseID: "toolu_01ABC"},
	}, cfg)
	Process(event.Event{
		Kind: event.KindTurnEnd, SessionID: sess, Cwd: "/repo",
		TurnEnd: &event.TurnEnd{ResponseText: "Fixed it."},
	}, cfg)

	require.Len(t, c.all(), 3)

	root, ok := c.byName("invoke_agent")
	require.True(t, ok)
	tool, ok := c.byName("execute_tool")
	require.True(t, ok)
	llm, ok := c.byName("chat")
	require.True(t, ok)

	// Stitching: one trace, correct nesting.
	assert.Equal(t, root.TraceID, tool.TraceID)
	assert.Equal(t, root.TraceID, llm.TraceID)
	assert.Empty(t, root.ParentSpanID, "the invoke_agent span is the root")
	assert.Equal(t, root.SpanID, tool.ParentSpanID, "tool parented to root")
	assert.Equal(t, root.SpanID, llm.ParentSpanID, "llm parented to root")

	// gen_ai attributes, including runtime identity from config.
	rootAttrs := attrMap(root)
	assert.Equal(t, "invoke_agent", rootAttrs["gen_ai.operation.name"])
	assert.Equal(t, "fix the bug", rootAttrs["gen_ai.llm.input.user"])
	assert.Equal(t, "test-agent", rootAttrs["gen_ai.agent.name"])
	assert.Equal(t, "test-provider", rootAttrs["gen_ai.provider.name"])
	assert.Equal(t, "execute_tool", attrMap(tool)["gen_ai.operation.name"])
	assert.Equal(t, "Bash", attrMap(tool)["gen_ai.tool.name"])
	assert.Equal(t, "toolu_01ABC", attrMap(tool)["gen_ai.tool.call.id"], "tool_use_id flows to the span")
	assert.Equal(t, "chat", attrMap(llm)["gen_ai.operation.name"])
	assert.Equal(t, "fix the bug", attrMap(llm)["gen_ai.llm.input.user"], "chat span carries input alongside output")
	assert.Equal(t, "Fixed it.", attrMap(llm)["gen_ai.llm.output"])

	// Tool duration reflects DurationMs (1500ms = 1.5e9 ns).
	toolStart := mustAtoi(t, tool.StartTimeUnixNano)
	toolEnd := mustAtoi(t, tool.EndTimeUnixNano)
	assert.Equal(t, int64(1500*1_000_000), toolEnd-toolStart)

	// Context file cleared after TurnEnd.
	_, err = os.Stat(ctxPath)
	assert.True(t, os.IsNotExist(err), "context file should be removed after TurnEnd")
}

// Given a turn with two tool calls, when processed, then both execute_tool spans
// share the same trace and the same root parent.
func TestParallelToolCalls(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-parallel"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{Kind: event.KindToolCall, SessionID: sess, ToolCall: &event.ToolCall{Name: "Read", Input: "a"}}, cfg)
	Process(event.Event{Kind: event.KindToolCall, SessionID: sess, ToolCall: &event.ToolCall{Name: "Edit", Input: "b"}}, cfg)

	var tools []otlp.Span
	for _, s := range c.all() {
		if s.Name == "execute_tool" {
			tools = append(tools, s)
		}
	}
	require.Len(t, tools, 2)
	assert.Equal(t, tools[0].TraceID, tools[1].TraceID, "same turn trace")
	assert.Equal(t, tools[0].ParentSpanID, tools[1].ParentSpanID, "same root parent")
}

// Given a tool call with no prior TurnStart, when processed, then the context is
// lazily created and the tool span is still emitted with a trace id (fail open).
func TestFailOpen_MissingTurnStart(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	// ToolCall with no prior TurnStart: context is lazily created.
	Process(event.Event{Kind: event.KindToolCall, SessionID: "orphan", ToolCall: &event.ToolCall{Name: "Bash", Input: "ls"}}, cfg)

	tool, ok := c.byName("execute_tool")
	require.True(t, ok, "tool span still emitted via lazy-created context")
	assert.NotEmpty(t, tool.TraceID)
}

// Given an unreachable endpoint, when a turn is processed, then Process does not
// panic (fail open).
func TestFailOpen_UnreachableEndpoint(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	// Port 1 refuses connections; Process must not panic.
	cfg := testConfig("http://127.0.0.1:1")

	assert.NotPanics(t, func() {
		Process(event.Event{Kind: event.KindTurnStart, SessionID: "s", TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
		Process(event.Event{Kind: event.KindTurnEnd, SessionID: "s", TurnEnd: &event.TurnEnd{ResponseText: "x"}}, cfg)
	})
}

// Given a TurnEnd whose ReadUsage returns token usage, when processed, then the
// chat span is stamped with those token counts, model and finish reason.
func TestTurnEnd_StampsUsageFromReadUsage(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-usage"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindTurnEnd, SessionID: sess,
		TurnEnd: &event.TurnEnd{
			ResponseText: "done",
			ReadUsage: func(startUnixNano string) event.Usage {
				return event.Usage{InputTokens: 3498, OutputTokens: 1562, Model: "claude-sonnet-4", StopReason: "end_turn"}
			},
		},
	}, cfg)

	llm, ok := c.byName("chat")
	require.True(t, ok)
	assert.Equal(t, "3498", intAttr(t, llm, "gen_ai.usage.input_tokens"))
	assert.Equal(t, "1562", intAttr(t, llm, "gen_ai.usage.output_tokens"))
	assert.Equal(t, "claude-sonnet-4", attrMap(llm)["gen_ai.request.model"])
	assert.Equal(t, "end_turn", attrMap(llm)["gen_ai.response.finish_reasons"])
}

// A failed tool call (PostToolUseFailure) produces an execute_tool span with
// ERROR status and an error.type attribute.
func TestToolCallFailure_MarksErrorStatus(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-tool-fail"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{
			Name: "Bash", Input: `{"command":"npm test"}`,
			Failed: true, ErrorType: "tool_error", Error: "Exit code 1",
		},
	}, cfg)

	tool, ok := c.byName("execute_tool")
	require.True(t, ok)
	assert.Equal(t, otlp.SpanStatusError, tool.Status.Code)
	assert.Equal(t, "Exit code 1", tool.Status.Message)
	assert.Equal(t, "tool_error", attrMap(tool)["error.type"])
}

// A failed turn (StopFailure) marks both the chat and invoke_agent spans ERROR.
func TestTurnEndFailure_MarksErrorStatus(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-turn-fail"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindTurnEnd, SessionID: sess,
		TurnEnd: &event.TurnEnd{
			ResponseText: "API Error: Rate limit reached",
			Failed:       true, ErrorType: "rate_limit", Error: "429 Too Many Requests",
		},
	}, cfg)

	llm, ok := c.byName("chat")
	require.True(t, ok)
	root, ok := c.byName("invoke_agent")
	require.True(t, ok)

	assert.Equal(t, otlp.SpanStatusError, llm.Status.Code)
	assert.Equal(t, "rate_limit", attrMap(llm)["error.type"])
	assert.Equal(t, "429 Too Many Requests", llm.Status.Message)
	assert.Equal(t, otlp.SpanStatusError, root.Status.Code, "root turn span also marked error")
	assert.Equal(t, "rate_limit", attrMap(root)["error.type"])
}

// A refusal arrives on a normal Stop but is surfaced as ERROR via the
// transcript stop reason.
func TestTurnEndRefusal_MarksErrorStatus(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-refusal"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindTurnEnd, SessionID: sess,
		TurnEnd: &event.TurnEnd{
			ResponseText: "",
			ReadUsage: func(string) event.Usage {
				return event.Usage{StopReason: "refusal"}
			},
		},
	}, cfg)

	llm, ok := c.byName("chat")
	require.True(t, ok)
	assert.Equal(t, otlp.SpanStatusError, llm.Status.Code)
	assert.Equal(t, "refusal", attrMap(llm)["error.type"])
}

// When a turn is both explicitly failed (StopFailure) and carries a refusal
// stop reason, the explicit failure wins. This proves turnError's early-return
// precedence, which the isolated failure/refusal tests above cannot show.
func TestTurnEndFailureAndRefusal_ExplicitFailureWins(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-both"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindTurnEnd, SessionID: sess,
		TurnEnd: &event.TurnEnd{
			ResponseText: "API Error: Rate limit reached",
			Failed:       true, ErrorType: "rate_limit", Error: "429 Too Many Requests",
			ReadUsage: func(string) event.Usage {
				return event.Usage{StopReason: "refusal"}
			},
		},
	}, cfg)

	llm, ok := c.byName("chat")
	require.True(t, ok)
	assert.Equal(t, otlp.SpanStatusError, llm.Status.Code)
	assert.Equal(t, "rate_limit", attrMap(llm)["error.type"],
		"explicit failure classification wins over the refusal stop reason")
	assert.Equal(t, "429 Too Many Requests", llm.Status.Message)
}

// The shape the adapter emits when StopFailure lacks an error code but carries
// a detail (ErrorType empty, Error set): the turn is still ERROR with the
// detail as the status message and no error.type attribute. This exercises the
// mixed MarkError case through the full pipeline path.
func TestTurnEndFailure_DetailWithoutType(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-fail-notype"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindTurnEnd, SessionID: sess,
		TurnEnd: &event.TurnEnd{Failed: true, ErrorType: "", Error: "connection reset"},
	}, cfg)

	for _, name := range []string{"chat", "invoke_agent"} {
		s, ok := c.byName(name)
		require.True(t, ok)
		assert.Equal(t, otlp.SpanStatusError, s.Status.Code, "%s marked error", name)
		assert.Equal(t, "connection reset", s.Status.Message, "%s carries the detail", name)
		_, hasErrType := attrMap(s)["error.type"]
		assert.False(t, hasErrType, "%s omits blank error.type", name)
	}
}

// A successful turn keeps OK status on every span (no regression).
func TestTurnEndSuccess_KeepsOKStatus(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-ok"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "done"}}, cfg)

	for _, s := range c.all() {
		assert.Equal(t, otlp.SpanStatusOK, s.Status.Code, "span %q should be OK", s.Name)
		assert.Empty(t, s.Status.Message)
	}
}

// Given an event of unknown kind, when processed, then nothing is emitted.
func TestSkipsUnknownKind(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)
	Process(event.Event{Kind: event.KindUnknown, SessionID: "s"}, cfg)
	assert.Empty(t, c.all(), "unknown kind emits nothing")
}

// An interrupted turn (tool calls but no TurnEnd) has its root span flushed
// by the next TurnStart, so its tool spans reparent instead of orphaning.
func TestInterruptedTurn_RecoveredOnNextTurnStart(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-interrupted"
	// Turn 1: starts, runs a tool, then is interrupted (no TurnEnd).
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "first"}}, cfg)
	Process(event.Event{Kind: event.KindToolCall, SessionID: sess, ToolCall: &event.ToolCall{Name: "Bash", Input: "ls"}}, cfg)

	tool, ok := c.byName("execute_tool")
	require.True(t, ok)

	// No root emitted yet — the tool span is orphaned at this point.
	_, ok = c.byName("invoke_agent")
	require.False(t, ok, "no root before the interrupted turn is flushed")

	// Capture the persisted start time before turn 2 overwrites the context,
	// so we can prove the recovered root keeps it (see the assertion below).
	stale, ok := turnctx.Peek(sess)
	require.True(t, ok)
	require.NotEmpty(t, stale.StartNano)

	// Turn 2 starts: this must flush turn 1's root before overwriting context.
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "second"}}, cfg)

	root, ok := c.byName("invoke_agent")
	require.True(t, ok, "interrupted turn's root is flushed on next TurnStart")
	assert.Equal(t, tool.ParentSpanID, root.SpanID, "tool reparents onto the recovered root")
	assert.Equal(t, tool.TraceID, root.TraceID, "recovered root shares the interrupted turn's trace")
	assert.True(t, boolAttr(t, root, "fiddler.coding_agent.turn.recovered"), "recovered root is tagged")
	assert.Equal(t, "first", attrMap(root)["gen_ai.llm.input.user"], "recovered root keeps the turn's prompt")
	assert.Empty(t, root.ParentSpanID, "recovered root is a root span")

	// The recovered root keeps the turn's real start time (persisted at
	// TurnStart), not the now-fallback — otherwise every recovered turn would
	// silently collapse to zero duration.
	assert.Equal(t, stale.StartNano, root.StartTimeUnixNano,
		"recovered root preserves the persisted turn start, not the now fallback")
	assert.NotEqual(t, root.StartTimeUnixNano, root.EndTimeUnixNano,
		"recovered root spans a real, non-zero duration")

	// The turn never confirmed success (no Stop), so the recovered root is
	// ERROR with error.type=interrupted, not a misleading clean OK.
	assert.Equal(t, otlp.SpanStatusError, root.Status.Code, "recovered root is ERROR")
	assert.Equal(t, "interrupted", attrMap(root)["error.type"])
}

// An interrupted turn whose last tool call failed (ERROR execute_tool span)
// still has that child reparent correctly onto the recovered root. The child's
// ERROR status is independent of the root's: the tool error stands on its own,
// and the root carries its own interrupted status.
func TestInterruptedTurn_ErrorChildReparents(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-interrupted-err"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "run it"}}, cfg)
	// The tool call aborts (e.g. user interrupt): an ERROR execute_tool span.
	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{
			Name: "Bash", Input: `{"command":"sleep 100"}`,
			Failed: true, ErrorType: "interrupted", Error: "Tool aborted",
		},
	}, cfg)

	tool, ok := c.byName("execute_tool")
	require.True(t, ok)
	assert.Equal(t, otlp.SpanStatusError, tool.Status.Code, "aborted tool span is ERROR")
	assert.Equal(t, "interrupted", attrMap(tool)["error.type"])

	// The turn is then interrupted; SessionEnd flushes its root.
	Process(event.Event{Kind: event.KindSessionEnd, SessionID: sess}, cfg)

	root, ok := c.byName("invoke_agent")
	require.True(t, ok, "interrupted turn's root is flushed")
	assert.Equal(t, tool.ParentSpanID, root.SpanID, "ERROR tool span reparents onto the recovered root")
	assert.Equal(t, tool.TraceID, root.TraceID)
	assert.True(t, boolAttr(t, root, "fiddler.coding_agent.turn.recovered"))
	assert.Equal(t, otlp.SpanStatusError, root.Status.Code, "recovered root is ERROR (interrupted)")
	assert.Equal(t, "interrupted", attrMap(root)["error.type"])
}

// A cleanly-ended turn (TurnEnd fired, context cleared) is not re-flushed by
// the following TurnStart — no duplicate root.
func TestCleanTurn_NotRecoveredOnNextTurnStart(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-clean"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{Kind: event.KindToolCall, SessionID: sess, ToolCall: &event.ToolCall{Name: "Bash", Input: "ls"}}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "done"}}, cfg)

	// A second turn starts; the first turn was cleared, so nothing to flush.
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p2"}}, cfg)

	var roots []otlp.Span
	for _, s := range c.all() {
		if s.Name == "invoke_agent" {
			roots = append(roots, s)
		}
	}
	require.Len(t, roots, 1, "exactly one root from the clean turn; no recovery duplicate")
	assert.False(t, boolAttr(t, roots[0], "fiddler.coding_agent.turn.recovered"), "clean turn root is not tagged recovered")
}

// A session that ends mid-turn has the in-progress turn's root flushed.
func TestSessionEnd_FlushesInterruptedTurn(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("CLAUDE_PLUGIN_DATA", dataDir)
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-end"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{Kind: event.KindToolCall, SessionID: sess, ToolCall: &event.ToolCall{Name: "Bash", Input: "ls"}}, cfg)
	Process(event.Event{Kind: event.KindSessionEnd, SessionID: sess}, cfg)

	tool, ok := c.byName("execute_tool")
	require.True(t, ok)
	root, ok := c.byName("invoke_agent")
	require.True(t, ok, "SessionEnd flushes the in-progress turn's root")
	assert.Equal(t, tool.ParentSpanID, root.SpanID)
	assert.True(t, boolAttr(t, root, "fiddler.coding_agent.turn.recovered"))

	_, err := os.Stat(filepath.Join(dataDir, "context", sess+".json"))
	assert.True(t, os.IsNotExist(err), "context cleared after SessionEnd")
}

// SessionEnd with no live turn emits nothing and does not panic.
func TestSessionEnd_NoContext_Noop(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	assert.NotPanics(t, func() {
		Process(event.Event{Kind: event.KindSessionEnd, SessionID: "never-seen"}, cfg)
	})
	assert.Empty(t, c.all(), "SessionEnd with no context emits nothing")
}

// chatSpans returns every chat span in arrival order (batch order is preserved
// by the collector), so per-call ordering assertions hold.
func (c *collector) chatSpans() []otlp.Span {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []otlp.Span
	for _, s := range c.spans {
		if s.Name == "chat" {
			out = append(out, s)
		}
	}
	return out
}

// twoCalls is a ReadCalls stub returning two reconstructed LLM calls: a first
// tool-using call and a final end_turn call, each with its own rendered
// input/output messages.
func twoCalls(startUnixNano string) []event.LLMCall {
	return []event.LLMCall{
		{
			MessageID:     "msg_A",
			StartNano:     "100",
			EndNano:       "200",
			InputMessages: []string{`{"role":"user","parts":[{"type":"text","text":"add logging"}]}`},
			Output:        `{"role":"assistant","parts":[{"type":"text","text":"call-one"}]}`,
			OutputText:    "call-one",
			InputText:     "add logging",
			Usage:         event.Usage{InputTokens: 1200, OutputTokens: 500, Model: "claude-sonnet-4", StopReason: "tool_use"},
		},
		{
			MessageID: "msg_B",
			StartNano: "200",
			EndNano:   "300",
			InputMessages: []string{
				`{"role":"user","parts":[{"type":"text","text":"add logging"}]}`,
				`{"role":"assistant","parts":[{"type":"text","text":"call-one"}]}`,
				`{"role":"user","parts":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}`,
			},
			Output:     `{"role":"assistant","parts":[{"type":"text","text":"call-two"}]}`,
			OutputText: "call-two",
			InputText:  "ok",
			Usage:      event.Usage{InputTokens: 798, OutputTokens: 262, Model: "claude-sonnet-4", StopReason: "end_turn"},
		},
	}
}

// chatOutputText returns the single text part of a chat span's
// gen_ai.output.messages array, to identify which call a span belongs to.
func chatOutputText(t *testing.T, s otlp.Span) string {
	t.Helper()
	raw := attrMap(s)["gen_ai.output.messages"]
	var arr []struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &arr))
	require.NotEmpty(t, arr)
	require.NotEmpty(t, arr[0].Parts)
	return arr[0].Parts[0].Text
}

// A turn with reconstructable calls emits one chat span per LLM call, each
// parented to the root and carrying its own input/output messages — and no
// aggregated fallback span.
func TestTurnEnd_PerCallChatSpans(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-percall"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "add logging"}}, cfg)
	Process(event.Event{
		Kind: event.KindTurnEnd, SessionID: sess,
		TurnEnd: &event.TurnEnd{ResponseText: "call-two", ReadCalls: twoCalls},
	}, cfg)

	root, ok := c.byName("invoke_agent")
	require.True(t, ok)

	// The root carries the turn's input/output pair from the reliable hook payload
	// (prompt + ResponseText), so the final answer is captured even in the per-call
	// path where the per-call output otherwise comes only from the (lag-prone)
	// transcript.
	rootAttrs := attrMap(root)
	assert.Equal(t, "add logging", rootAttrs["gen_ai.llm.input.user"], "root carries the prompt")
	assert.Equal(t, "call-two", rootAttrs["gen_ai.llm.output"], "root carries the final answer from ResponseText")

	chats := c.chatSpans()
	require.Len(t, chats, 2, "one chat span per reconstructed call, no aggregated fallback")

	for _, s := range chats {
		assert.Equal(t, root.SpanID, s.ParentSpanID, "chat parented to root")
		assert.Equal(t, root.TraceID, s.TraceID)
		m := attrMap(s)
		assert.Contains(t, m, "gen_ai.input.messages", "per-call span carries the input history")
		assert.Contains(t, m, "gen_ai.output.messages", "per-call span carries its output")
		// Per-call spans carry the input/output scalars alongside the message
		// arrays.
		assert.Contains(t, m, "gen_ai.llm.input.user", "per-call span carries its input scalar")
		assert.Contains(t, m, "gen_ai.llm.output", "per-call span carries its output text scalar")
	}

	// The second call's input history is the full conversation so far.
	assert.Equal(t, "call-one", chatOutputText(t, chats[0]))
	assert.Equal(t, "call-two", chatOutputText(t, chats[1]))
	// Both scalars are per-call: the input delta differs (the prompt for the
	// first call, the tool result for the second), matching native per-request
	// spans, and so does the output text.
	assert.Equal(t, "add logging", attrMap(chats[0])["gen_ai.llm.input.user"])
	assert.Equal(t, "ok", attrMap(chats[1])["gen_ai.llm.input.user"])
	assert.Equal(t, "call-one", attrMap(chats[0])["gen_ai.llm.output"])
	assert.Equal(t, "call-two", attrMap(chats[1])["gen_ai.llm.output"])
	in1 := attrMap(chats[1])["gen_ai.input.messages"]
	var arr []json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(in1), &arr))
	assert.Len(t, arr, 3, "second call sees prompt + first response + tool_result")

	// Per-call token usage, not an aggregate.
	assert.Equal(t, "1200", intAttr(t, chats[0], "gen_ai.usage.input_tokens"))
	assert.Equal(t, "798", intAttr(t, chats[1], "gen_ai.usage.input_tokens"))
}

// Given a turn whose final call was synthesized because its transcript write
// had not landed, When the turn ends, Then that chat span is tagged
// fiddler.coding_agent.llm_call.synthesized=true, carries no model, and the
// transcript-read call is left untagged.
func TestTurnEnd_SynthesizedCallTagged(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-synth"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "how many?"}}, cfg)
	Process(event.Event{
		Kind: event.KindTurnEnd, SessionID: sess,
		TurnEnd: &event.TurnEnd{
			ResponseText: "22",
			ReadCalls: func(start string) []event.LLMCall {
				calls := twoCalls(start)
				calls[1].Synthesized = true
				calls[1].Usage = event.Usage{}
				return calls
			},
		},
	}, cfg)

	chats := c.chatSpans()
	require.Len(t, chats, 2)
	assert.False(t, boolAttr(t, chats[0], otlp.AttrLLMCallSynthesized), "transcript-read call is not tagged")
	assert.True(t, boolAttr(t, chats[1], otlp.AttrLLMCallSynthesized), "synthesized call is tagged")
	assert.NotContains(t, attrMap(chats[1]), "gen_ai.request.model", "synthesized call carries no model")
}

// When ReadCalls yields nothing (nil reader or empty result), the turn falls
// back to the single aggregated chat span carrying prompt + response together.
func TestTurnEnd_EmptyCalls_FallsBackToAggregated(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-fallback"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindTurnEnd, SessionID: sess,
		TurnEnd: &event.TurnEnd{
			ResponseText: "done",
			ReadCalls:    func(string) []event.LLMCall { return nil },
		},
	}, cfg)

	chats := c.chatSpans()
	require.Len(t, chats, 1, "aggregated fallback span")
	m := attrMap(chats[0])
	assert.Equal(t, "p", m["gen_ai.llm.input.user"], "fallback uses the aggregated schema")
	assert.Equal(t, "done", m["gen_ai.llm.output"])
}

// A refusal reconstructs as completed calls: the model declined on the last
// call, so only that per-call span is marked ERROR — the earlier successful
// calls stay OK — and the root is ERROR.
func TestTurnEndRefusal_PerCall_MarksLastCallOnly(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-refusal-percall"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindTurnEnd, SessionID: sess,
		TurnEnd: &event.TurnEnd{
			ReadCalls: twoCalls,
			ReadUsage: func(string) event.Usage { return event.Usage{StopReason: "refusal"} },
		},
	}, cfg)

	chats := c.chatSpans()
	require.Len(t, chats, 2)
	assert.Equal(t, otlp.SpanStatusOK, chats[0].Status.Code, "earlier call stays OK")
	assert.Equal(t, otlp.SpanStatusError, chats[1].Status.Code, "the refused (last) call is ERROR")
	assert.Equal(t, "refusal", attrMap(chats[1])["error.type"])

	root, ok := c.byName("invoke_agent")
	require.True(t, ok)
	assert.Equal(t, otlp.SpanStatusError, root.Status.Code, "root is ERROR on refusal")
	assert.Equal(t, "refusal", attrMap(root)["error.type"])
}

// A StopFailure after ≥1 successful call: the erroring API call produced no
// assistant message to reconstruct, so the failure is carried by the root's
// ERROR status and the reconstructed per-call spans stay OK (never mislabeled).
func TestTurnEndFailure_PerCall_RootErrorCallsStayOK(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-fail-percall"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindTurnEnd, SessionID: sess,
		TurnEnd: &event.TurnEnd{
			ResponseText: "API Error: Rate limit reached",
			Failed:       true, ErrorType: "rate_limit", Error: "429 Too Many Requests",
			ReadCalls: twoCalls,
		},
	}, cfg)

	chats := c.chatSpans()
	require.Len(t, chats, 2)
	for _, s := range chats {
		assert.Equal(t, otlp.SpanStatusOK, s.Status.Code, "reconstructed call spans stay OK on StopFailure")
	}
	root, ok := c.byName("invoke_agent")
	require.True(t, ok)
	assert.Equal(t, otlp.SpanStatusError, root.Status.Code, "root carries the StopFailure")
	assert.Equal(t, "rate_limit", attrMap(root)["error.type"])
}

// An interrupted turn's completed LLM calls are emitted as OK chat spans when
// the next TurnStart flushes it — the interruption is on the root, not the
// calls that finished before it.
func TestInterruptedTurn_EmitsCompletedCallSpans(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-interrupted-calls"
	Process(event.Event{
		Kind: event.KindTurnStart, SessionID: sess,
		TurnStart: &event.TurnStart{Prompt: "first"},
	}, cfg)
	// Interrupted: next TurnStart carries the reconstruction of the prior turn.
	Process(event.Event{
		Kind: event.KindTurnStart, SessionID: sess,
		TurnStart: &event.TurnStart{Prompt: "second", ReadCalls: twoCalls},
	}, cfg)

	root, ok := c.byName("invoke_agent")
	require.True(t, ok, "interrupted turn's root is flushed")
	assert.Equal(t, otlp.SpanStatusError, root.Status.Code, "root is ERROR (interrupted)")

	chats := c.chatSpans()
	require.Len(t, chats, 2, "both completed calls emitted")
	for _, s := range chats {
		assert.Equal(t, root.SpanID, s.ParentSpanID, "call spans reparent onto the recovered root")
		assert.Equal(t, otlp.SpanStatusOK, s.Status.Code, "completed calls are OK; the interruption is on the root")
	}
}

// execToolByToolName returns the first execute_tool span whose gen_ai.tool.name
// matches, and (when spanID is non-empty) whose SpanID matches too — needed when
// a turn has several execute_tool spans for the same tool (e.g. two Agent
// launches). Order is arrival order.
func execToolByToolName(c *collector, toolName, spanID string) (otlp.Span, bool) {
	for _, s := range c.all() {
		if s.Name != "execute_tool" || attrMap(s)["gen_ai.tool.name"] != toolName {
			continue
		}
		if spanID != "" && s.SpanID != spanID {
			continue
		}
		return s, true
	}
	return otlp.Span{}, false
}

// Given a main-agent Agent tool that launches a sub-agent, when its PostToolUse
// is processed, then its execute_tool span gets the derived id A(agent_id) and
// nests under the turn root, and the launching turn's trace is recorded in
// agentctx under the launched agent_id (so the async sub-agent can reuse it).
func TestAgentLaunch_NestsUnderRootAndRecordsTrace(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-agent-launch"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{Name: "Agent", Input: `{"subagent_type":"Explore"}`, LaunchedAgentID: "agent-1"},
	}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "done"}}, cfg)

	root, ok := c.byName("invoke_agent")
	require.True(t, ok)
	launch, ok := execToolByToolName(c, "Agent", "")
	require.True(t, ok, "Agent launch emits an execute_tool span")

	assert.Equal(t, root.TraceID, launch.TraceID, "launch span shares the turn's trace")
	assert.Equal(t, root.SpanID, launch.ParentSpanID, "a main-agent launch nests under the turn root")
	assert.Equal(t, otlp.SpanIDFrom(root.TraceID, "agent-tool:agent-1"), launch.SpanID,
		"launch span gets the derived id A(agent_id) the sub-agent subtree parents onto")

	// The launching trace is persisted under the launched agent_id and survives
	// TurnEnd (agentctx is not swept there) so the async sub-agent reuses it.
	rec, ok := agentctx.Read(sess, "agent-1")
	require.True(t, ok, "agentctx record written at launch and survives TurnEnd")
	assert.Equal(t, root.TraceID, rec.TraceID, "recorded trace is the launching turn's")
}

// Given a sub-agent that runs a tool asynchronously — its PostToolUse arrives
// after the launching turn's Stop cleared turnctx — when it is processed, then
// its span reuses the launching turn's trace (from agentctx, NOT a freshly
// minted one) and parents to the sub-agent root S(agent_id), so an async
// sub-agent's spans stay in the launching trace.
func TestSubAgentTool_Async_ReusesLaunchTrace(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-async-subtool"
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

	// Async: the sub-agent runs a Read AFTER turn 1's Stop. turnctx is cleared,
	// so a turnctx.Load here would lazily mint a different trace.
	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{Name: "Read", Input: `{"file_path":"/x"}`, AgentID: "agent-1"},
	}, cfg)

	sub, ok := execToolByToolName(c, "Read", "")
	require.True(t, ok, "sub-agent tool span emitted")
	assert.Equal(t, launchTrace, sub.TraceID,
		"sub-agent tool reuses the launching turn's trace, not a lazily-minted one")
	assert.Equal(t, otlp.SpanIDFrom(launchTrace, "subagent:agent-1"), sub.ParentSpanID,
		"sub-agent tool parents to the sub-agent root S(agent_id)")
}

// Given a tool tagged with an agent_id for which no agentctx record exists (a
// missed launch), when it is processed, then it degrades to the turn root in the
// turn's trace rather than deriving against a wrong or empty trace.
func TestSubAgentTool_MissDegradesToRoot(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-subtool-miss"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{Name: "Read", Input: "a", AgentID: "unknown-agent"},
	}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "x"}}, cfg)

	root, ok := c.byName("invoke_agent")
	require.True(t, ok)
	sub, ok := execToolByToolName(c, "Read", "")
	require.True(t, ok)
	assert.Equal(t, root.TraceID, sub.TraceID, "degrades into the turn's trace")
	assert.Equal(t, root.SpanID, sub.ParentSpanID, "degrades to the turn root, not a derived sub-agent parent")
}

// Given a completed turn and a tool call tagged with an unknown sub-agent that
// arrives after that turn has ended, when it is processed, then no span is
// emitted and no turn context is created.
func TestSubAgentTool_AsyncMiss_SkipsNoMint(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-async-miss"
	// A turn runs to completion; no sub-agent is ever launched.
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "done"}}, cfg)

	_, hasCtx := turnctx.Peek(sess)
	require.False(t, hasCtx, "precondition: the turn has ended")

	// The tool, tagged with an unknown sub-agent, arrives after the turn ended.
	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{Name: "Read", Input: `{"file_path":"/x"}`, AgentID: "ghost"},
	}, cfg)

	_, emitted := execToolByToolName(c, "Read", "")
	assert.False(t, emitted, "no span for a sub-agent tool that arrives after its turn ended")
	_, created := turnctx.Peek(sess)
	assert.False(t, created, "no turn context created")
}

// Given a completed turn and a nested launch, made by an unknown sub-agent, that
// arrives after that turn has ended, when it is processed, then no span is
// emitted and no trace is recorded for the launched sub-agent.
func TestNestedAgentLaunch_AsyncMiss_SkipsNoRecord(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-nested-async-miss"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "done"}}, cfg)

	// An unknown sub-agent launches another, after the turn ended.
	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{Name: "Agent", AgentID: "ghost-outer", LaunchedAgentID: "inner"},
	}, cfg)

	_, emitted := execToolByToolName(c, "Agent", "")
	assert.False(t, emitted, "no span for a nested launch that arrives after its turn ended")
	_, hasRec := agentctx.Read(sess, "inner")
	assert.False(t, hasRec, "no trace recorded for the launched sub-agent")
	_, created := turnctx.Peek(sess)
	assert.False(t, created, "no turn context created")
}

// Given an Agent launch that itself runs inside a sub-agent (a nested launch:
// both agent_id and the launched agentId are present), when it is processed,
// then its span gets id A(inner) and nests under the outer sub-agent's root
// S(outer), all in the outer sub-agent's trace, and the inner agent_id is
// recorded against that same trace.
func TestNestedAgentLaunch_NestsUnderOuterSubAgent(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-nested-launch"
	// Turn 1 launches the outer sub-agent; Stop clears turnctx.
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{Name: "Agent", LaunchedAgentID: "outer"},
	}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "done"}}, cfg)

	root, ok := c.byName("invoke_agent")
	require.True(t, ok)
	trace := root.TraceID

	// The outer sub-agent asynchronously launches an inner sub-agent.
	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{Name: "Agent", AgentID: "outer", LaunchedAgentID: "inner"},
	}, cfg)

	wantID := otlp.SpanIDFrom(trace, "agent-tool:inner")
	inner, ok := execToolByToolName(c, "Agent", wantID)
	require.True(t, ok, "nested launch emits an execute_tool span with id A(inner)")
	assert.Equal(t, trace, inner.TraceID, "nested launch stays in the outer sub-agent's trace")
	assert.Equal(t, otlp.SpanIDFrom(trace, "subagent:outer"), inner.ParentSpanID,
		"nested launch nests under the outer sub-agent's root S(outer)")

	rec, ok := agentctx.Read(sess, "inner")
	require.True(t, ok, "inner agent_id recorded")
	assert.Equal(t, trace, rec.TraceID, "inner sub-agent inherits the same trace")
}

// subRootByID returns the invoke_agent span with the given span id (the derived
// sub-agent root S(agent_id)), distinguishing it from the turn root.
func subRootByID(c *collector, spanID string) (otlp.Span, bool) {
	for _, s := range c.all() {
		if s.Name == "invoke_agent" && s.SpanID == spanID {
			return s, true
		}
	}
	return otlp.Span{}, false
}

// chatsWithParent returns the chat spans parented to the given span id, in
// arrival order.
func (c *collector) chatsWithParent(parentSpanID string) []otlp.Span {
	var out []otlp.Span
	for _, s := range c.chatSpans() {
		if s.ParentSpanID == parentSpanID {
			out = append(out, s)
		}
	}
	return out
}

// Given an Agent launch recorded in agentctx, when the sub-agent's SubagentStop
// is processed (after the launching turn's Stop — the async case), then a
// sub-agent invoke_agent root (id S(agent_id), parent A(agent_id)) and its
// per-call chat spans (parent S) are emitted in the launching turn's trace, the
// root is named by the sub-agent's type, and the agentctx record is deleted.
func TestSubagentEnd_ReconstructsSubtree(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-subagent-end"
	// Turn 1 launches the sub-agent (writes agentctx), then Stop (clears turnctx).
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{Name: "Agent", LaunchedAgentID: "agent-1"},
	}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "done"}}, cfg)

	root, ok := c.byName("invoke_agent")
	require.True(t, ok)
	trace := root.TraceID
	launch, ok := execToolByToolName(c, "Agent", "")
	require.True(t, ok)

	// SubagentStop arrives after turn 1's Stop (async): the trace comes only from
	// agentctx, so the subtree still lands in the launching trace.
	Process(event.Event{
		Kind: event.KindSubagentEnd, SessionID: sess,
		SubagentEnd: &event.SubagentEnd{
			AgentID:      "agent-1",
			AgentType:    "Explore",
			ResponseText: "sub done",
			ReadStart:    func() string { return "100" },
			ReadCalls:    twoCalls,
		},
	}, cfg)

	subID := otlp.SpanIDFrom(trace, "subagent:agent-1")
	subRoot, ok := subRootByID(c, subID)
	require.True(t, ok, "sub-agent invoke_agent emitted with id S(agent_id)")
	assert.Equal(t, trace, subRoot.TraceID, "sub-agent root in the launching trace")
	assert.Equal(t, launch.SpanID, subRoot.ParentSpanID, "sub-agent root parents to the Agent launch span")
	assert.Equal(t, otlp.SpanIDFrom(trace, "agent-tool:agent-1"), subRoot.ParentSpanID,
		"the launch span id is A(agent_id)")
	assert.Equal(t, "Explore", attrMap(subRoot)["gen_ai.agent.name"], "sub-agent named by its type")
	assert.Equal(t, "sub done", attrMap(subRoot)["gen_ai.llm.output"], "sub-agent root carries its final answer")

	subChats := c.chatsWithParent(subID)
	require.Len(t, subChats, 2, "one chat span per reconstructed sub-agent call, parented to S")
	for _, s := range subChats {
		assert.Equal(t, trace, s.TraceID, "sub-agent chat spans share the launching trace")
	}
	// Per-call token usage, not an aggregate.
	assert.Equal(t, "1200", intAttr(t, subChats[0], "gen_ai.usage.input_tokens"))
	assert.Equal(t, "798", intAttr(t, subChats[1], "gen_ai.usage.input_tokens"))

	_, found := agentctx.Read(sess, "agent-1")
	assert.False(t, found, "agentctx record deleted after SubagentStop")
}

// Given a SubagentStop whose agent_id has no agentctx record (a launch that was
// never observed, or a stop arriving after the record was swept), when it is
// processed, then nothing is emitted and no trace is lazily minted — the
// skip-on-miss that keeps a stray stop from splitting the sub-agent into a
// separate trace.
func TestSubagentEnd_SkipOnMiss(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	Process(event.Event{
		Kind: event.KindSubagentEnd, SessionID: "sess-subagent-miss",
		SubagentEnd: &event.SubagentEnd{AgentID: "ghost", ReadCalls: twoCalls, ReadStart: func() string { return "100" }},
	}, cfg)
	assert.Empty(t, c.all(), "SubagentStop with no launch record emits nothing")
}

// Given a SubagentStop carrying an empty agent_id (no key to derive the nesting
// spans from), when it is processed, then it is skipped and nothing is emitted.
func TestSubagentEnd_EmptyAgentID_Skipped(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	Process(event.Event{
		Kind: event.KindSubagentEnd, SessionID: "sess-subagent-noid",
		SubagentEnd: &event.SubagentEnd{AgentID: "", ReadCalls: twoCalls},
	}, cfg)
	assert.Empty(t, c.all(), "SubagentStop without an agent_id emits nothing")
}

// Given two SubagentStop events for the same agent_id (the first Deletes the
// agentctx record), when both are processed, then only the first emits a
// sub-agent root and the second is skipped-on-miss — exactly one root exists,
// never a second bogus one.
func TestSubagentEnd_DuplicateStop_NoSecondRoot(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-subagent-dup"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{Kind: event.KindToolCall, SessionID: sess, ToolCall: &event.ToolCall{Name: "Agent", LaunchedAgentID: "agent-1"}}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "done"}}, cfg)

	stop := event.Event{
		Kind: event.KindSubagentEnd, SessionID: sess,
		SubagentEnd: &event.SubagentEnd{AgentID: "agent-1", AgentType: "Explore", ReadCalls: twoCalls, ReadStart: func() string { return "100" }},
	}
	Process(stop, cfg)
	Process(stop, cfg) // duplicate: agentctx record is already gone

	root, ok := c.byName("invoke_agent")
	require.True(t, ok)
	subID := otlp.SpanIDFrom(root.TraceID, "subagent:agent-1")
	count := 0
	for _, s := range c.all() {
		if s.Name == "invoke_agent" && s.SpanID == subID {
			count++
		}
	}
	assert.Equal(t, 1, count, "exactly one sub-agent root; the duplicate stop is skipped")
}

// Given a sub-agent whose transcript yields no per-call spans (ReadCalls empty)
// but does report aggregate token usage, when its SubagentStop is processed, then
// a single aggregated chat span (parent S(agent_id)) is emitted carrying the
// sub-agent's response and usage — mirroring the turn-end fallback.
func TestSubagentEnd_EmptyCalls_FallsBackToAggregated(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-subagent-fallback"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{Kind: event.KindToolCall, SessionID: sess, ToolCall: &event.ToolCall{Name: "Agent", LaunchedAgentID: "agent-1"}}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "done"}}, cfg)

	root, ok := c.byName("invoke_agent")
	require.True(t, ok)
	trace := root.TraceID

	Process(event.Event{
		Kind: event.KindSubagentEnd, SessionID: sess,
		SubagentEnd: &event.SubagentEnd{
			AgentID:      "agent-1",
			AgentType:    "Explore",
			ResponseText: "sub answer",
			ReadStart:    func() string { return "100" },
			ReadCalls:    func(string) []event.LLMCall { return nil },
			ReadUsage: func(string) event.Usage {
				return event.Usage{InputTokens: 42, OutputTokens: 7, Model: "claude-sonnet-4"}
			},
		},
	}, cfg)

	subID := otlp.SpanIDFrom(trace, "subagent:agent-1")
	subChats := c.chatsWithParent(subID)
	require.Len(t, subChats, 1, "aggregated fallback chat span for the sub-agent")
	m := attrMap(subChats[0])
	assert.Equal(t, "sub answer", m["gen_ai.llm.output"], "fallback carries the sub-agent response")
	assert.Equal(t, "42", intAttr(t, subChats[0], "gen_ai.usage.input_tokens"))
	assert.Equal(t, "7", intAttr(t, subChats[0], "gen_ai.usage.output_tokens"))
}

// Given a sub-agent whose start timestamp is unknown (ReadStart returns ""), when
// its SubagentStop is processed, then the readers receive an empty floor so the
// whole of the sub-agent's dedicated transcript is read and its per-call chat spans
// are still reconstructed, rather than filtered away.
func TestSubagentEnd_UnknownStart_ReadsWholeTranscript(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-subagent-unknown-start"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{Kind: event.KindToolCall, SessionID: sess, ToolCall: &event.ToolCall{Name: "Agent", LaunchedAgentID: "agent-1"}}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "done"}}, cfg)

	root, ok := c.byName("invoke_agent")
	require.True(t, ok)
	trace := root.TraceID

	gotFloor, floorSeen := "", false
	Process(event.Event{
		Kind: event.KindSubagentEnd, SessionID: sess,
		SubagentEnd: &event.SubagentEnd{
			AgentID:      "agent-1",
			AgentType:    "Explore",
			ResponseText: "sub answer",
			ReadStart:    func() string { return "" },
			ReadCalls: func(floor string) []event.LLMCall {
				gotFloor, floorSeen = floor, true
				return twoCalls(floor)
			},
		},
	}, cfg)

	require.True(t, floorSeen, "the calls reader was consulted")
	assert.Empty(t, gotFloor, "unknown start -> empty floor (read whole transcript), not now")

	subID := otlp.SpanIDFrom(trace, "subagent:agent-1")
	assert.NotEmpty(t, c.chatsWithParent(subID), "per-call chat spans reconstructed despite unknown start")
}

// Given a wrapper sub-agent whose agent_type is empty, when its SubagentStop is
// processed, then it still nests (the nesting key is agent_id, not the type) and
// its root's gen_ai.agent.name falls back to the configured agent name.
func TestSubagentEnd_WrapperAgent_UsesConfigName(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-subagent-wrapper"
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{Kind: event.KindToolCall, SessionID: sess, ToolCall: &event.ToolCall{Name: "Agent", LaunchedAgentID: "agent-1"}}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "done"}}, cfg)

	root, ok := c.byName("invoke_agent")
	require.True(t, ok)
	trace := root.TraceID

	Process(event.Event{
		Kind: event.KindSubagentEnd, SessionID: sess,
		SubagentEnd: &event.SubagentEnd{
			AgentID:   "agent-1",
			AgentType: "", // wrapper agent
			ReadStart: func() string { return "100" },
			ReadCalls: twoCalls,
		},
	}, cfg)

	subID := otlp.SpanIDFrom(trace, "subagent:agent-1")
	subRoot, ok := subRootByID(c, subID)
	require.True(t, ok, "wrapper sub-agent still nests")
	assert.Equal(t, "test-agent", attrMap(subRoot)["gen_ai.agent.name"],
		"wrapper agent falls back to the configured agent name")
}

// Given the full async lifecycle in order — turn 1 launches the sub-agent, the
// sub-agent runs a tool (tagged with a top-level agent_id), turn 1's Stop
// clears turnctx, then SubagentStop arrives afterward — when they are all
// processed, then the whole subtree lands in the one launching trace: the Agent
// launch A(id) under the turn root, the sub-agent root S(id) under A(id), and
// the sub-agent's tool and chat spans under S(id).
func TestSubagentEnd_EndToEnd_AsyncTreeInOneTrace(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	c := newCollector(t)
	cfg := testConfig(c.srv.URL)

	sess := "sess-subagent-e2e"
	// Turn 1: prompt, Agent launch, then a tool the sub-agent runs.
	Process(event.Event{Kind: event.KindTurnStart, SessionID: sess, TurnStart: &event.TurnStart{Prompt: "p"}}, cfg)
	Process(event.Event{Kind: event.KindToolCall, SessionID: sess, ToolCall: &event.ToolCall{Name: "Agent", LaunchedAgentID: "agent-1"}}, cfg)
	Process(event.Event{
		Kind: event.KindToolCall, SessionID: sess,
		ToolCall: &event.ToolCall{Name: "Read", Input: `{"file_path":"/x"}`, AgentID: "agent-1"},
	}, cfg)
	Process(event.Event{Kind: event.KindTurnEnd, SessionID: sess, TurnEnd: &event.TurnEnd{ResponseText: "done"}}, cfg)

	// SubagentStop after Stop.
	Process(event.Event{
		Kind: event.KindSubagentEnd, SessionID: sess,
		SubagentEnd: &event.SubagentEnd{
			AgentID: "agent-1", AgentType: "Explore", ResponseText: "sub done",
			ReadStart: func() string { return "100" }, ReadCalls: twoCalls,
		},
	}, cfg)

	root, ok := c.byName("invoke_agent")
	require.True(t, ok)
	trace := root.TraceID
	aID := otlp.SpanIDFrom(trace, "agent-tool:agent-1")
	sID := otlp.SpanIDFrom(trace, "subagent:agent-1")

	// The Agent launch A(id) nests under the turn root.
	launch, ok := execToolByToolName(c, "Agent", aID)
	require.True(t, ok, "Agent launch has id A(agent_id)")
	assert.Equal(t, root.SpanID, launch.ParentSpanID, "launch nests under the turn root")

	// The sub-agent's own tool nests under S(id).
	subTool, ok := execToolByToolName(c, "Read", "")
	require.True(t, ok)
	assert.Equal(t, sID, subTool.ParentSpanID, "sub-agent tool parents to S(agent_id)")

	// The sub-agent root S(id) nests under the Agent launch A(id).
	subRoot, ok := subRootByID(c, sID)
	require.True(t, ok)
	assert.Equal(t, aID, subRoot.ParentSpanID, "sub-agent root parents to A(agent_id)")

	// Its chat spans nest under S(id).
	require.Len(t, c.chatsWithParent(sID), 2, "sub-agent chat spans parent to S")

	// Everything is in the one launching trace — no scatter.
	for _, s := range c.all() {
		assert.Equal(t, trace, s.TraceID, "span %q is in the single launching trace", s.Name)
	}
}

// intAttr returns an int-typed attribute value (OTLP encodes ints as strings).
func intAttr(t *testing.T, s otlp.Span, key string) string {
	t.Helper()
	for _, a := range s.Attributes {
		if a.Key == key && a.Value.IntValue != nil {
			return *a.Value.IntValue
		}
	}
	t.Fatalf("int attribute %q not found", key)
	return ""
}

// boolAttr returns a bool-typed attribute value, or false if the key is
// absent (so it doubles as an "is this turn tagged recovered?" check).
func boolAttr(t *testing.T, s otlp.Span, key string) bool {
	t.Helper()
	for _, a := range s.Attributes {
		if a.Key == key && a.Value.BoolValue != nil {
			return *a.Value.BoolValue
		}
	}
	return false
}

func mustAtoi(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	require.NoError(t, err)
	return n
}
