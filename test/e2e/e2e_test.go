// Package e2e exercises the Claude Code wiring end to end: real hook
// payloads run through the claudecode adapter and the shared pipeline,
// asserting the OTLP spans that reach the (faked) Fiddler endpoint.
//
// The component tests cover each package in isolation; this test covers
// the seam between the runtime adapter and the runtime-agnostic pipeline.
package e2e

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/config"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/otlp"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/pipeline"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/source/claudecode"
)

// loadPayload reads a Claude Code hook-payload fixture from testdata/payloads
// into the generic map the runtime dispatcher passes to the adapter (matching
// how cmd/on-event decodes stdin, so JSON numbers arrive as float64).
func loadPayload(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "payloads", name))
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

// decodePayload reads an OTLP export request body, transparently decompressing
// the gzip the exporter always applies (Content-Encoding: gzip), and asserts
// that header is present — mirroring a real Fiddler OTLP endpoint.
func decodePayload(t *testing.T, r *http.Request) otlp.ExportPayload {
	t.Helper()
	require.Equal(t, "gzip", r.Header.Get("Content-Encoding"), "exporter must gzip the body")
	zr, err := gzip.NewReader(r.Body)
	require.NoError(t, err)
	defer func() { _ = zr.Close() }()
	var payload otlp.ExportPayload
	require.NoError(t, json.NewDecoder(zr).Decode(&payload))
	return payload
}

// Given real Claude Code hook payloads for a full turn (UserPromptSubmit,
// PostToolUse, Stop), when driven through the adapter and pipeline, then three
// correctly-nested OTLP spans reach the endpoint carrying Claude Code identity,
// session grouping, and the plugin's own service.name and instrumentation scope.
func TestClaudeCodeFullTurn(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())

	var mu sync.Mutex
	var spans []otlp.Span
	var resources []otlp.Resource
	var scopes []otlp.InstrumentationScope
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := decodePayload(t, r)
		mu.Lock()
		for _, rs := range payload.ResourceSpans {
			resources = append(resources, rs.Resource)
			for _, ss := range rs.ScopeSpans {
				scopes = append(scopes, ss.Scope)
				spans = append(spans, ss.Spans...)
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Endpoint:    srv.URL,
		APIKey:      "tok",
		AppID:       "app",
		AgentName:   claudecode.AgentName,
		Provider:    claudecode.Provider,
		ServiceName: claudecode.ServiceName,
	}

	// Drive real Claude Code hook events through adapter -> pipeline.
	dispatch := func(name string, payload map[string]any) {
		ev, ok := claudecode.Adapt(name, payload)
		require.True(t, ok, "event %s should adapt", name)
		pipeline.Process(ev, cfg)
	}

	sess := "e2e-sess"
	dispatch("UserPromptSubmit", map[string]any{
		"session_id": sess, "cwd": "/repo", "prompt": "fix the bug",
	})
	dispatch("PostToolUse", map[string]any{
		"session_id": sess, "cwd": "/repo",
		"tool_name":     "Bash",
		"tool_input":    map[string]any{"command": "ls"},
		"tool_response": map[string]any{"stdout": "file1.txt"},
		"tool_use_id":   "toolu_01ABC",
		"duration_ms":   float64(1500),
	})
	dispatch("Stop", map[string]any{
		"session_id": sess, "cwd": "/repo",
		"last_assistant_message": "Fixed it.",
	})

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, spans, 3)

	byName := func(n string) otlp.Span {
		for _, s := range spans {
			if s.Name == n {
				return s
			}
		}
		t.Fatalf("span %q not found", n)
		return otlp.Span{}
	}
	root := byName("invoke_agent")
	tool := byName("execute_tool")
	llm := byName("chat")

	// One correctly-nested trace.
	assert.Equal(t, root.TraceID, tool.TraceID)
	assert.Equal(t, root.TraceID, llm.TraceID)
	assert.Empty(t, root.ParentSpanID)
	assert.Equal(t, root.SpanID, tool.ParentSpanID)
	assert.Equal(t, root.SpanID, llm.ParentSpanID)

	// Claude Code identity flows through to the spans.
	attr := func(s otlp.Span, key string) string {
		for _, a := range s.Attributes {
			if a.Key == key && a.Value.StringValue != nil {
				return *a.Value.StringValue
			}
		}
		return ""
	}
	assert.Equal(t, "claude-code", attr(root, "gen_ai.agent.name"))
	assert.Equal(t, "anthropic", attr(root, "gen_ai.provider.name"))
	assert.Equal(t, "fix the bug", attr(root, "gen_ai.llm.input.user"))
	assert.Equal(t, "Bash", attr(tool, "gen_ai.tool.name"))
	assert.Equal(t, "toolu_01ABC", attr(tool, "gen_ai.tool.call.id"))
	assert.Equal(t, "file1.txt", attr(tool, "gen_ai.tool.call.result"))
	assert.Equal(t, "Fixed it.", attr(llm, "gen_ai.llm.output"))

	// Session grouping: gen_ai.conversation.id carries the session id on
	// every span.
	assert.Equal(t, sess, attr(root, "gen_ai.conversation.id"))
	assert.Equal(t, sess, attr(tool, "gen_ai.conversation.id"))
	assert.Equal(t, sess, attr(llm, "gen_ai.conversation.id"))

	// The producer identity: service.name names the plugin, not the observed
	// runtime.
	resAttr := func(res otlp.Resource, key string) string {
		for _, a := range res.Attributes {
			if a.Key == key && a.Value.StringValue != nil {
				return *a.Value.StringValue
			}
		}
		return ""
	}
	require.NotEmpty(t, resources)
	assert.Equal(t, "fiddler-coding-agent-plugin", resAttr(resources[0], "service.name"))
	assert.Equal(t, "app", resAttr(resources[0], "application.id"))

	// The instrumentation scope is the plugin's own, not Anthropic's native
	// claude_code scope.
	require.NotEmpty(t, scopes)
	assert.Equal(t, "ai.fiddler.coding_agent_plugin", scopes[0].Name)
}

// The permission-capture path, driven from real hook-payload fixtures through
// the real adapter -> pipeline -> OTLP seam: PreToolUse anchors each call, an
// accepted call (PostToolUse) carries permission attributes, and an auto-mode
// denial (PermissionDenied) produces a rejected execute_tool span that would
// otherwise be invisible.
//
// Given a session that accepts one tool call and has another auto-denied, when
// the hook payloads are replayed, then two execute_tool spans are emitted — one
// accept (OK) and one reject (ERROR/permission_denied) — with the permission
// attributes set on each.
func TestClaudeCodePermissionCapture(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())

	var mu sync.Mutex
	var spans []otlp.Span
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := decodePayload(t, r)
		mu.Lock()
		for _, rs := range payload.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				spans = append(spans, ss.Spans...)
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Endpoint:    srv.URL,
		APIKey:      "tok",
		AppID:       "app",
		AgentName:   claudecode.AgentName,
		Provider:    claudecode.Provider,
		ServiceName: claudecode.ServiceName,
	}

	// Replay a session's hook payloads from fixtures, in order. Each
	// fixture carries its own hook_event_name; the fixture file name maps to
	// the payload, the event name drives the adapter.
	replay := []struct{ event, fixture string }{
		{"UserPromptSubmit", "user_prompt_submit.json"},
		{"PreToolUse", "pre_tool_use_accept.json"},
		{"PostToolUse", "post_tool_use.json"},
		{"PreToolUse", "pre_tool_use_denied.json"},
		{"PermissionDenied", "permission_denied.json"},
		{"Stop", "stop.json"},
	}
	for _, step := range replay {
		ev, ok := claudecode.Adapt(step.event, loadPayload(t, step.fixture))
		require.True(t, ok, "event %s should adapt", step.event)
		pipeline.Process(ev, cfg)
	}

	mu.Lock()
	defer mu.Unlock()

	attr := func(s otlp.Span, key string) string {
		for _, a := range s.Attributes {
			if a.Key == key && a.Value.StringValue != nil {
				return *a.Value.StringValue
			}
		}
		return ""
	}

	var accept, reject otlp.Span
	var tools int
	for _, s := range spans {
		if s.Name != "execute_tool" {
			continue
		}
		tools++
		switch attr(s, "gen_ai.tool.call.id") {
		case "toolu_A":
			accept = s
		case "toolu_B":
			reject = s
		}
	}
	require.Equal(t, 2, tools, "both the accepted and the denied call produce a span")

	// Accepted call: permission attributes, OK status.
	assert.Equal(t, otlp.SpanStatusOK, accept.Status.Code)
	assert.Equal(t, "accept", attr(accept, "fiddler.coding_agent.permission.decision"))
	assert.Equal(t, "default", attr(accept, "fiddler.coding_agent.permission.mode"))

	// Denied call: a rejected span.
	assert.Equal(t, otlp.SpanStatusError, reject.Status.Code)
	assert.Equal(t, "permission_denied", attr(reject, "error.type"))
	assert.Equal(t, "reject", attr(reject, "fiddler.coding_agent.permission.decision"))
	assert.Equal(t, "auto-classifier", attr(reject, "fiddler.coding_agent.permission.denial_kind"))
	assert.Equal(t, "Blocked by classifier", attr(reject, "fiddler.coding_agent.permission.denial_reason"))
}

// The transcript-reconstruction path end to end: a tool the user rejected at the
// prompt (no id-carrying hook fires) is recovered from the session transcript at
// Stop as a reject span, and a tool that was requested but never resolved is
// recovered as an unresolved span.
//
// Given a turn whose transcript records one user-rejected denial and leaves one
// call with no result, when Stop runs, then a reject span and an unresolved span
// are emitted through the real adapter -> pipeline -> OTLP seam.
func TestClaudeCodeDenialReconstruction(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())

	var mu sync.Mutex
	var spans []otlp.Span
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := decodePayload(t, r)
		mu.Lock()
		for _, rs := range payload.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				spans = append(spans, ss.Spans...)
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Endpoint:    srv.URL,
		APIKey:      "tok",
		AppID:       "app",
		AgentName:   claudecode.AgentName,
		Provider:    claudecode.Provider,
		ServiceName: claudecode.ServiceName,
	}
	dispatch := func(name string, payload map[string]any) {
		ev, ok := claudecode.Adapt(name, payload)
		require.True(t, ok, "event %s should adapt", name)
		pipeline.Process(ev, cfg)
	}

	sess := "e2e-denial"
	transcriptPath := filepath.Join(t.TempDir(), "transcript.jsonl")

	dispatch("UserPromptSubmit", map[string]any{"session_id": sess, "cwd": "/repo", "prompt": "do it"})
	dispatch("PreToolUse", map[string]any{
		"session_id": sess, "tool_name": "Edit",
		"tool_input": map[string]any{"file": "a.go"}, "tool_use_id": "toolu_deny", "permission_mode": "default",
	})
	dispatch("PreToolUse", map[string]any{
		"session_id": sess, "tool_name": "Bash",
		"tool_input": map[string]any{"command": "sleep 1"}, "tool_use_id": "toolu_abandon", "permission_mode": "default",
	})

	// The transcript is written during the turn: a user-rejected denial for
	// toolu_deny, and nothing for toolu_abandon. Timestamp it "now" so it falls
	// after the turn start the pipeline recorded at UserPromptSubmit.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	transcriptLine := func(obj map[string]any) string {
		b, err := json.Marshal(obj)
		require.NoError(t, err)
		return string(b) + "\n"
	}
	content := transcriptLine(map[string]any{
		"type": "assistant", "timestamp": now,
		"message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_deny", "name": "Edit", "input": map[string]any{"file": "a.go"}},
		}},
	}) + transcriptLine(map[string]any{
		"type": "user", "timestamp": now, "toolDenialKind": "user-rejected",
		"message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_deny", "is_error": true,
				"content": "The user doesn't want to proceed with this tool use."},
		}},
	})
	require.NoError(t, os.WriteFile(transcriptPath, []byte(content), 0o644))

	dispatch("Stop", map[string]any{
		"session_id": sess, "cwd": "/repo",
		"last_assistant_message": "ok", "transcript_path": transcriptPath,
	})

	mu.Lock()
	defer mu.Unlock()

	attr := func(s otlp.Span, key string) string {
		for _, a := range s.Attributes {
			if a.Key == key && a.Value.StringValue != nil {
				return *a.Value.StringValue
			}
		}
		return ""
	}
	find := func(id string) (otlp.Span, bool) {
		for _, s := range spans {
			if s.Name == "execute_tool" && attr(s, "gen_ai.tool.call.id") == id {
				return s, true
			}
		}
		return otlp.Span{}, false
	}

	deny, ok := find("toolu_deny")
	require.True(t, ok, "user-rejected denial reconstructed from the transcript")
	assert.Equal(t, otlp.SpanStatusError, deny.Status.Code)
	assert.Equal(t, "permission_denied", attr(deny, "error.type"))
	assert.Equal(t, "reject", attr(deny, "fiddler.coding_agent.permission.decision"))
	assert.Equal(t, "user-rejected", attr(deny, "fiddler.coding_agent.permission.denial_kind"))

	abandon, ok := find("toolu_abandon")
	require.True(t, ok, "abandoned call reconstructed as unresolved")
	assert.Equal(t, otlp.SpanStatusUnset, abandon.Status.Code)
	assert.Equal(t, "unresolved", attr(abandon, "fiddler.coding_agent.permission.decision"))
}

// A failed tool and a failed turn drive real PostToolUseFailure / StopFailure
// payloads through the adapter -> pipeline seam and reach the endpoint as
// ERROR spans, distinguishable from the success path above.
func TestClaudeCodeFailurePath(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())

	var mu sync.Mutex
	var spans []otlp.Span
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := decodePayload(t, r)
		mu.Lock()
		for _, rs := range payload.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				spans = append(spans, ss.Spans...)
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Endpoint:    srv.URL,
		APIKey:      "tok",
		AppID:       "app",
		AgentName:   claudecode.AgentName,
		Provider:    claudecode.Provider,
		ServiceName: claudecode.ServiceName,
	}

	dispatch := func(name string, payload map[string]any) {
		ev, ok := claudecode.Adapt(name, payload)
		require.True(t, ok, "event %s should adapt", name)
		pipeline.Process(ev, cfg)
	}

	sess := "e2e-fail"
	dispatch("UserPromptSubmit", map[string]any{
		"session_id": sess, "cwd": "/repo", "prompt": "run the tests",
	})
	dispatch("PostToolUseFailure", map[string]any{
		"session_id": sess, "cwd": "/repo",
		"tool_name":   "Bash",
		"tool_input":  map[string]any{"command": "npm test"},
		"error":       "Exit code 1",
		"duration_ms": float64(4187),
	})
	dispatch("StopFailure", map[string]any{
		"session_id": sess, "cwd": "/repo",
		"error":                  "rate_limit",
		"error_details":          "429 Too Many Requests",
		"last_assistant_message": "API Error: Rate limit reached",
	})

	mu.Lock()
	defer mu.Unlock()

	byName := func(n string) otlp.Span {
		for _, s := range spans {
			if s.Name == n {
				return s
			}
		}
		t.Fatalf("span %q not found", n)
		return otlp.Span{}
	}
	attr := func(s otlp.Span, key string) string {
		for _, a := range s.Attributes {
			if a.Key == key && a.Value.StringValue != nil {
				return *a.Value.StringValue
			}
		}
		return ""
	}

	tool := byName("execute_tool")
	assert.Equal(t, otlp.SpanStatusError, tool.Status.Code, "failed tool span is ERROR")
	assert.Equal(t, "tool_error", attr(tool, "error.type"))
	assert.Equal(t, "Exit code 1", tool.Status.Message)

	llm := byName("chat")
	root := byName("invoke_agent")
	assert.Equal(t, otlp.SpanStatusError, llm.Status.Code, "failed turn chat span is ERROR")
	assert.Equal(t, otlp.SpanStatusError, root.Status.Code, "failed turn root span is ERROR")
	assert.Equal(t, "rate_limit", attr(llm, "error.type"))
	assert.Equal(t, "429 Too Many Requests", llm.Status.Message)
}

// A turn interrupted mid-tool (its tool call aborts, then the session ends
// with no Stop) drives real PostToolUseFailure / SessionEnd payloads through
// the adapter -> pipeline seam. This is the seam-level version of the
// interrupted-turn recovery: the aborted tool span would otherwise be
// orphaned, but SessionEnd flushes a recovered root it reparents onto — and
// because the turn never confirmed success, both spans land ERROR.
func TestClaudeCodeInterruptedTurn(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())

	var mu sync.Mutex
	var spans []otlp.Span
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := decodePayload(t, r)
		mu.Lock()
		for _, rs := range payload.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				spans = append(spans, ss.Spans...)
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Endpoint:    srv.URL,
		APIKey:      "tok",
		AppID:       "app",
		AgentName:   claudecode.AgentName,
		Provider:    claudecode.Provider,
		ServiceName: claudecode.ServiceName,
	}

	dispatch := func(name string, payload map[string]any) {
		ev, ok := claudecode.Adapt(name, payload)
		require.True(t, ok, "event %s should adapt", name)
		pipeline.Process(ev, cfg)
	}

	sess := "e2e-interrupted"
	dispatch("UserPromptSubmit", map[string]any{
		"session_id": sess, "cwd": "/repo", "prompt": "run a long job",
	})
	// The tool call is aborted (user interrupt): an ERROR execute_tool span,
	// emitted eagerly while its root span id only lives in the context file.
	dispatch("PostToolUseFailure", map[string]any{
		"session_id": sess, "cwd": "/repo",
		"tool_name":    "Bash",
		"tool_input":   map[string]any{"command": "sleep 100"},
		"error":        "Tool aborted",
		"is_interrupt": true,
		"duration_ms":  float64(1200),
	})
	// No Stop ever arrives — the session ends mid-turn.
	dispatch("SessionEnd", map[string]any{
		"session_id": sess, "cwd": "/repo",
	})

	mu.Lock()
	defer mu.Unlock()

	// Exactly the tool span and the recovered root: no chat span, since the
	// turn-level LLM span needs a real Stop (token usage / response text).
	require.Len(t, spans, 2)

	byName := func(n string) otlp.Span {
		for _, s := range spans {
			if s.Name == n {
				return s
			}
		}
		t.Fatalf("span %q not found", n)
		return otlp.Span{}
	}
	attr := func(s otlp.Span, key string) string {
		for _, a := range s.Attributes {
			if a.Key == key && a.Value.StringValue != nil {
				return *a.Value.StringValue
			}
		}
		return ""
	}
	boolAttr := func(s otlp.Span, key string) bool {
		for _, a := range s.Attributes {
			if a.Key == key && a.Value.BoolValue != nil {
				return *a.Value.BoolValue
			}
		}
		return false
	}

	for _, s := range spans {
		assert.NotEqual(t, "chat", s.Name, "no chat span without a real Stop")
	}

	tool := byName("execute_tool")
	root := byName("invoke_agent")

	// The aborted tool span is ERROR/interrupted and reparents onto the
	// recovered root instead of orphaning.
	assert.Equal(t, otlp.SpanStatusError, tool.Status.Code, "aborted tool span is ERROR")
	assert.Equal(t, "interrupted", attr(tool, "error.type"))
	assert.Equal(t, root.SpanID, tool.ParentSpanID, "tool reparents onto the recovered root")
	assert.Equal(t, root.TraceID, tool.TraceID, "one trace across the interrupted turn")

	// The recovered root: a root span, tagged recovered, ERROR because the
	// turn never confirmed success.
	assert.Empty(t, root.ParentSpanID, "recovered root is a root span")
	assert.True(t, boolAttr(root, "fiddler.coding_agent.turn.recovered"), "root tagged recovered")
	assert.Equal(t, otlp.SpanStatusError, root.Status.Code, "recovered root is ERROR")
	assert.Equal(t, "interrupted", attr(root, "error.type"))
	assert.Equal(t, "run a long job", attr(root, "gen_ai.llm.input.user"), "root keeps the turn's prompt")
}
