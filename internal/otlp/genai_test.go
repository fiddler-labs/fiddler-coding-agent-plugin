package otlp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/event"
)

// intAttr reads an int-typed attribute (OTLP encodes ints as strings).
func intAttr(s Span, key string) (string, bool) {
	for _, a := range s.Attributes {
		if a.Key == key && a.Value.IntValue != nil {
			return *a.Value.IntValue, true
		}
	}
	return "", false
}

// attrMap flattens a span's string attributes into a map for assertions.
func attrMap(s Span) map[string]string {
	m := make(map[string]string, len(s.Attributes))
	for _, a := range s.Attributes {
		if a.Value.StringValue != nil {
			m[a.Key] = *a.Value.StringValue
		}
	}
	return m
}

// testBuilder is a SpanBuilder with arbitrary (non-Claude) identity, to
// confirm the builder stamps whatever the runtime supplies.
func testBuilder() SpanBuilder {
	return SpanBuilder{
		TraceID:   "trace1",
		SessionID: "sess1",
		AgentName: "test-agent",
		Provider:  "test-provider",
	}
}

func assertBaseAttrs(t *testing.T, m map[string]string) {
	t.Helper()
	assert.Equal(t, "sess1", m["gen_ai.conversation.id"])
	assert.Equal(t, "test-agent", m["gen_ai.agent.name"])
	assert.Equal(t, "test-provider", m["gen_ai.provider.name"])
}

// Given a prompt and a final answer, When InvokeAgent builds the root span, Then
// it is the parentless invoke_agent span carrying the turn's input/output pair.
func TestSpanBuilder_InvokeAgent(t *testing.T) {
	s := testBuilder().InvokeAgent("root1", "100", "200", "fix the bug", "Fixed it.")

	assert.Equal(t, "invoke_agent", s.Name)
	assert.Equal(t, "trace1", s.TraceID)
	assert.Equal(t, "root1", s.SpanID)
	assert.Empty(t, s.ParentSpanID, "the invoke_agent span is the root span")

	m := attrMap(s)
	assertBaseAttrs(t, m)
	assert.Equal(t, "invoke_agent", m["gen_ai.operation.name"])
	assert.Equal(t, "fix the bug", m["gen_ai.llm.input.user"])
	assert.Equal(t, "Fixed it.", m["gen_ai.llm.output"], "the final answer is stamped on the root span")
}

// Given an empty prompt and empty answer, When InvokeAgent builds the root span,
// Then both the input and output scalars are still stamped (as "") so they
// read as captured-but-empty rather than dropped.
func TestSpanBuilder_InvokeAgent_EmitsEmptyPromptAndOutput(t *testing.T) {
	s := testBuilder().InvokeAgent("root1", "100", "200", "", "")
	m := attrMap(s)

	in, hasIn := m["gen_ai.llm.input.user"]
	assert.True(t, hasIn, "input attribute is always set, even when empty")
	assert.Equal(t, "", in)

	out, hasOut := m["gen_ai.llm.output"]
	assert.True(t, hasOut, "output attribute is always set, even when empty")
	assert.Equal(t, "", out)
}

// Given a tool call, When the Tool span is built, Then it is an execute_tool
// span parented to the root carrying the tool name, call id, arguments and result.
func TestSpanBuilder_Tool(t *testing.T) {
	s := testBuilder().Tool("span1", "root1", "100", "200", "Bash", "toolu_01ABC", `{"command":"ls"}`, "file1.txt")

	assert.Equal(t, "execute_tool", s.Name)
	assert.Equal(t, "root1", s.ParentSpanID)

	m := attrMap(s)
	assertBaseAttrs(t, m)
	assert.Equal(t, "execute_tool", m["gen_ai.operation.name"])
	assert.Equal(t, "Bash", m["gen_ai.tool.name"])
	assert.Equal(t, "toolu_01ABC", m["gen_ai.tool.call.id"])
	assert.Equal(t, `{"command":"ls"}`, m["gen_ai.tool.call.arguments"])
	assert.Equal(t, "file1.txt", m["gen_ai.tool.call.result"])
}

// Given a tool call with an empty tool_use_id, When the Tool span is built, Then
// the gen_ai.tool.call.id attribute is omitted rather than set to "".
func TestSpanBuilder_Tool_OmitsEmptyCallID(t *testing.T) {
	s := testBuilder().Tool("span1", "root1", "100", "200", "Bash", "", `{"command":"ls"}`, "file1.txt")
	_, ok := attrMap(s)["gen_ai.tool.call.id"]
	assert.False(t, ok, "empty tool call id should not set the attribute")
}

// Given an accept PermissionInfo with a wait, when stamped on a span, then the
// decision, mode, wait.source and wait_ms attributes are set and denial-only
// fields are absent.
func TestSpan_SetPermission_Accept(t *testing.T) {
	s := testBuilder().Tool("s", "root", "100", "200", "Bash", "toolu_1", "in", "out")
	s.SetPermission(PermissionInfo{
		Decision:   PermissionAccept,
		Mode:       "default",
		WaitMs:     1200,
		HasWait:    true,
		WaitSource: WaitEstimated,
	})

	m := attrMap(s)
	assert.Equal(t, "accept", m["fiddler.coding_agent.permission.decision"])
	assert.Equal(t, "default", m["fiddler.coding_agent.permission.mode"])
	assert.Equal(t, "estimated", m["fiddler.coding_agent.permission.wait.source"])
	ms, ok := intAttr(s, "fiddler.coding_agent.permission.wait_ms")
	require.True(t, ok)
	assert.Equal(t, "1200", ms)
	// Denial-only fields are omitted on an accept.
	_, hasKind := m["fiddler.coding_agent.permission.denial_kind"]
	assert.False(t, hasKind)
}

// Given a reject PermissionInfo with a denial kind and reason, when stamped on a
// span, then the decision, denial_kind, denial_reason and mode attributes are set.
func TestSpan_SetPermission_Reject(t *testing.T) {
	s := testBuilder().Tool("s", "root", "100", "200", "Bash", "toolu_1", "in", "")
	s.SetPermission(PermissionInfo{
		Decision:     PermissionReject,
		DenialKind:   DenialAutoClassifier,
		DenialReason: "Blocked by classifier",
		Mode:         "auto",
	})

	m := attrMap(s)
	assert.Equal(t, "reject", m["fiddler.coding_agent.permission.decision"])
	assert.Equal(t, "auto-classifier", m["fiddler.coding_agent.permission.denial_kind"])
	assert.Equal(t, "Blocked by classifier", m["fiddler.coding_agent.permission.denial_reason"])
	assert.Equal(t, "auto", m["fiddler.coding_agent.permission.mode"])
}

// Given a PermissionInfo with HasWait=false, when stamped on a span, then
// wait_ms and wait.source are omitted (unknown, not zero).
func TestSpan_SetPermission_OmitsWaitWhenAbsent(t *testing.T) {
	s := testBuilder().Tool("s", "root", "100", "200", "Bash", "toolu_1", "in", "out")
	s.SetPermission(PermissionInfo{Decision: PermissionAccept, Mode: "default"})

	_, ok := intAttr(s, "fiddler.coding_agent.permission.wait_ms")
	assert.False(t, ok, "wait omitted when HasWait is false (unknown, not zero)")
	_, hasSrc := attrMap(s)["fiddler.coding_agent.permission.wait.source"]
	assert.False(t, hasSrc)
}

// Given an LLM call with full token usage, When the LLM span is built, Then it is
// a chat span parented to the root carrying input/output, model, finish reason
// and every token count.
func TestSpanBuilder_LLM_WithUsage(t *testing.T) {
	usage := event.Usage{
		InputTokens:         3498,
		OutputTokens:        1562,
		CacheReadTokens:     600,
		CacheCreationTokens: 150,
		Model:               "claude-sonnet-4",
		StopReason:          "end_turn",
	}
	s := testBuilder().LLM("span1", "root1", "100", "200", "fix the bug", "Fixed it.", usage)

	assert.Equal(t, "chat", s.Name)
	assert.Equal(t, "root1", s.ParentSpanID)

	m := attrMap(s)
	assertBaseAttrs(t, m)
	assert.Equal(t, "chat", m["gen_ai.operation.name"])
	assert.Equal(t, "fix the bug", m["gen_ai.llm.input.user"], "chat span carries the input alongside the output")
	assert.Equal(t, "Fixed it.", m["gen_ai.llm.output"])
	assert.Equal(t, "claude-sonnet-4", m["gen_ai.request.model"])
	assert.Equal(t, "end_turn", m["gen_ai.response.finish_reasons"])

	in, ok := intAttr(s, "gen_ai.usage.input_tokens")
	require.True(t, ok)
	assert.Equal(t, "3498", in)
	out, _ := intAttr(s, "gen_ai.usage.output_tokens")
	assert.Equal(t, "1562", out)
	cr, _ := intAttr(s, "gen_ai.usage.cache_read.input_tokens")
	assert.Equal(t, "600", cr)
	cc, _ := intAttr(s, "gen_ai.usage.cache_creation.input_tokens")
	assert.Equal(t, "150", cc)
}

// Given an LLM call with an empty prompt and zero usage, When the LLM span is
// built, Then the input scalar and token counts are still stamped (as ""/0) while
// the empty model and finish reason are omitted.
func TestSpanBuilder_LLM_ZeroUsage(t *testing.T) {
	s := testBuilder().LLM("span1", "root1", "100", "200", "", "Fixed it.", event.Usage{})

	m := attrMap(s)
	// An empty prompt still stamps the input attribute (as ""), mirroring InvokeAgent.
	inUser, hasInput := m["gen_ai.llm.input.user"]
	assert.True(t, hasInput, "input attribute is always set, even when empty")
	assert.Equal(t, "", inUser)
	// Token counts are always emitted (as 0); model and finish reason are
	// omitted when empty.
	in, ok := intAttr(s, "gen_ai.usage.input_tokens")
	require.True(t, ok)
	assert.Equal(t, "0", in)
	_, hasModel := m["gen_ai.request.model"]
	assert.False(t, hasModel, "empty model omitted")
	_, hasFinish := m["gen_ai.response.finish_reasons"]
	assert.False(t, hasFinish, "empty finish reason omitted")
}

// msgObj is a minimal decode target for a rendered {role, parts} message.
type msgObj struct {
	Role  string `json:"role"`
	Parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"parts"`
}

// decodeArray parses a gen_ai.*.messages attribute into its message objects.
func decodeArray(t *testing.T, s string) []msgObj {
	t.Helper()
	var arr []msgObj
	require.NoError(t, json.Unmarshal([]byte(s), &arr), "messages attribute must be a valid JSON array: %s", s)
	return arr
}

// Given an LLM call with input history and an output message, When the LLMCall
// span is built, Then it carries the scalar input/output, the full history as
// input.messages, its response as a single-element output.messages, plus model,
// finish reason and usage.
func TestSpanBuilder_LLMCall_StampsMessagesAndUsage(t *testing.T) {
	in := []string{
		`{"role":"user","parts":[{"type":"text","text":"add logging"}]}`,
		`{"role":"assistant","parts":[{"type":"text","text":"on it"}]}`,
	}
	out := `{"role":"assistant","parts":[{"type":"text","text":"done"}]}`
	usage := event.Usage{
		InputTokens:  798,
		OutputTokens: 262,
		Model:        "claude-sonnet-4",
		StopReason:   "end_turn",
	}
	s := testBuilder().LLMCall("span1", "root1", "100", "200", "add logging", in, out, "done", usage)

	assert.Equal(t, "chat", s.Name)
	assert.Equal(t, "root1", s.ParentSpanID)

	m := attrMap(s)
	assertBaseAttrs(t, m)
	assert.Equal(t, "chat", m["gen_ai.operation.name"])

	// The scalar input/output attributes, alongside the arrays: input.user is
	// this call's input delta, output is its response text.
	assert.Equal(t, "add logging", m["gen_ai.llm.input.user"])
	assert.Equal(t, "done", m["gen_ai.llm.output"])

	// input.messages is the whole history as one JSON array.
	inArr := decodeArray(t, m["gen_ai.input.messages"])
	require.Len(t, inArr, 2)
	assert.Equal(t, "user", inArr[0].Role)
	assert.Equal(t, "assistant", inArr[1].Role)

	// output.messages is a single-element array holding this call's response.
	outArr := decodeArray(t, m["gen_ai.output.messages"])
	require.Len(t, outArr, 1)
	assert.Equal(t, "done", outArr[0].Parts[0].Text)

	assert.Equal(t, "claude-sonnet-4", m["gen_ai.request.model"])
	assert.Equal(t, "end_turn", m["gen_ai.response.finish_reasons"])
	inTok, ok := intAttr(s, "gen_ai.usage.input_tokens")
	require.True(t, ok)
	assert.Equal(t, "798", inTok)
}

// Given an LLM call with no prompt, history or output, When the LLMCall span is
// built, Then the structural message arrays are omitted while the scalar
// input/output are stamped (as "") and token counts stamp 0.
func TestSpanBuilder_LLMCall_EmptyMessages(t *testing.T) {
	// No prompt, no input history, and no output: the structural message arrays
	// are omitted (empty != absent has meaning there), but the scalar
	// gen_ai.llm.input.user / gen_ai.llm.output attributes are always stamped
	// (as ""). Token counts stamp 0.
	s := testBuilder().LLMCall("span1", "root1", "100", "200", "", nil, "", "", event.Usage{})
	m := attrMap(s)
	_, hasIn := m["gen_ai.input.messages"]
	assert.False(t, hasIn, "empty history omits the input.messages array")
	_, hasOut := m["gen_ai.output.messages"]
	assert.False(t, hasOut, "empty output omits the output.messages array")
	scalarIn, hasScalarIn := m["gen_ai.llm.input.user"]
	assert.True(t, hasScalarIn, "input.user scalar is always set, even when empty")
	assert.Equal(t, "", scalarIn)
	scalarOut, hasScalarOut := m["gen_ai.llm.output"]
	assert.True(t, hasScalarOut, "output scalar is always set, even when empty")
	assert.Equal(t, "", scalarOut)
	in, ok := intAttr(s, "gen_ai.usage.input_tokens")
	require.True(t, ok)
	assert.Equal(t, "0", in)
}

// Given messages that fit within the budget, When the array JSON is rendered,
// Then the whole array is emitted verbatim.
func TestMessageArrayJSON_FitsWhole(t *testing.T) {
	msgs := []string{
		`{"role":"user","parts":[{"type":"text","text":"a"}]}`,
		`{"role":"assistant","parts":[{"type":"text","text":"b"}]}`,
	}
	got := messageArrayJSON(msgs, MaxMessagesAttrLen)
	assert.Equal(t, "["+strings.Join(msgs, ",")+"]", got)
	assert.Len(t, decodeArray(t, got), 2)
}

// Given no messages, When the array JSON is rendered, Then it is "[]".
func TestMessageArrayJSON_Empty(t *testing.T) {
	assert.Equal(t, "[]", messageArrayJSON(nil, MaxMessagesAttrLen))
}

// Given more messages than fit the budget, When the array JSON is rendered, Then
// the oldest are dropped, a leading marker records the dropped count, and the
// newest messages are kept — still valid JSON within the budget.
func TestMessageArrayJSON_KeepNewestTruncation(t *testing.T) {
	const n = 20
	msgs := make([]string, n)
	for i := range msgs {
		msgs[i] = fmt.Sprintf(`{"role":"user","parts":[{"type":"text","text":"msg-%02d"}]}`, i)
	}
	const maxLen = 220
	got := messageArrayJSON(msgs, maxLen)

	assert.LessOrEqual(t, len(got), maxLen, "result must fit the budget")
	arr := decodeArray(t, got)
	require.GreaterOrEqual(t, len(arr), 2, "marker + at least one kept message")

	// The first element is the truncation marker recording the dropped count.
	marker := arr[0]
	assert.Equal(t, "system", marker.Role)
	require.Len(t, marker.Parts, 1)
	dropped := n - (len(arr) - 1) // total minus kept (excluding the marker)
	assert.Contains(t, marker.Parts[0].Text, fmt.Sprintf("truncated %d earlier message(s)", dropped))

	// Keep-newest: the last kept element is the newest input message, and the
	// dropped ones are the oldest.
	assert.Equal(t, "msg-19", arr[len(arr)-1].Parts[0].Text, "newest message retained")
}

// Given a single message larger than the whole budget, When messageArrayJSON
// renders it, Then rather than byte-truncating the array into malformed JSON, the
// text inside the message is shortened so the JSON stays closed, and a marker
// records that the shown message was shortened.
func TestMessageArrayJSON_SingleMessageTooLarge(t *testing.T) {
	big := `{"role":"user","parts":[{"type":"text","text":"` + strings.Repeat("x", 500) + `"}]}`
	const maxLen = 200
	got := messageArrayJSON([]string{big}, maxLen)
	assert.LessOrEqual(t, len(got), maxLen, "within the cap")

	arr := decodeArray(t, got) // must parse as valid JSON
	require.Len(t, arr, 2, "marker + the shortened message")

	assert.Equal(t, "system", arr[0].Role)
	require.Len(t, arr[0].Parts, 1)
	assert.Contains(t, arr[0].Parts[0].Text, "shortened")

	assert.Equal(t, "user", arr[1].Role)
	require.Len(t, arr[1].Parts, 1)
	assert.Contains(t, arr[1].Parts[0].Text, "x", "the message's own text is kept, just shorter")
	assert.Less(t, len(arr[1].Parts[0].Text), 500, "the text was shortened")
}

// Given a tool_result whose huge text leaf is nested inside a content array, When
// truncateMessageToFit runs, Then it reaches into the nested structure, shortens
// the text leaf, and leaves valid JSON with the surrounding fields (role, type,
// tool_use_id) intact.
func TestTruncateMessageToFit_NestedContent(t *testing.T) {
	msg := `{"role":"user","parts":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"` +
		strings.Repeat("y", 2000) + `"}]}]}`
	const maxLen = 300
	got := truncateMessageToFit(msg, maxLen)

	assert.LessOrEqual(t, len(got), maxLen, "within the budget")
	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(got), &decoded), "must be valid JSON: %s", got)
	assert.Equal(t, "user", decoded["role"])
	assert.Contains(t, got, "toolu_1", "identifying fields are preserved")
	assert.Contains(t, got, "tool_result", "part type is preserved")
	assert.Less(t, strings.Count(got, "y"), 2000, "the nested text leaf was shortened")
}

// Given a message far larger than the budget, When truncateMessageToFit runs,
// Then it fits within the budget and uses nearly all of it — the proportional
// cap step keeps most of the allowed content.
func TestTruncateMessageToFit_KeepsNearlyAllTheBudget(t *testing.T) {
	msg := `{"role":"user","parts":[{"type":"text","text":"` + strings.Repeat("x", 1_200_000) + `"}]}`
	const maxLen = 1_000_000
	got := truncateMessageToFit(msg, maxLen)

	assert.LessOrEqual(t, len(got), maxLen, "within the budget")
	require.NoError(t, json.Unmarshal([]byte(got), new(map[string]interface{})), "must be valid JSON")
	assert.Greater(t, len(got), maxLen*9/10, "keeps >90%% of the budget")
}

// Given a message with nested string values, a number, and a bool, When
// capStrings caps at 3, Then every string value (however deeply nested) is
// shortened to the cap while object keys, numbers, and bools are left untouched.
func TestCapStrings_TruncatesOnlyStringLeaves(t *testing.T) {
	src := `{"role":"user","n":12345,"ok":true,"parts":[{"type":"text","text":"HELLOWORLD"}]}`
	dec := json.NewDecoder(strings.NewReader(src))
	dec.UseNumber() // mirror truncateMessageToFit: numbers stay json.Number
	var v interface{}
	require.NoError(t, dec.Decode(&v))

	capStrings(v, 3)

	m := v.(map[string]interface{})
	assert.Equal(t, "use", m["role"], "string value truncated to the cap")
	assert.Equal(t, json.Number("12345"), m["n"], "numbers are left untouched (not truncated, not reformatted)")
	assert.Equal(t, true, m["ok"], "bools are left untouched")

	parts := m["parts"].([]interface{})
	p0 := parts[0].(map[string]interface{})
	// Keys ("type", "text") are never truncated; only the values are.
	assert.Equal(t, "tex", p0["type"], "nested string value truncated")
	assert.Equal(t, "HEL", p0["text"], "nested string value truncated")
}

// Given two string leaves, one within the cap and one over it, When capStrings
// caps at 4, Then the leaf already within the cap is unchanged and only the
// over-cap leaf is shortened.
func TestCapStrings_ShortLeavesUntouched(t *testing.T) {
	src := `{"a":"hi","b":"a much longer value here"}`
	var v interface{}
	require.NoError(t, json.Unmarshal([]byte(src), &v))

	capStrings(v, 4)

	m := v.(map[string]interface{})
	assert.Equal(t, "hi", m["a"], "leaf already within the cap is unchanged")
	assert.Equal(t, "a mu", m["b"], "leaf over the cap is shortened to the cap")
}

// Given a budget smaller than the message's JSON wrapper itself, When
// truncateMessageToFit runs, Then it returns "" because no string-shortening can
// make it fit (keys and braces are never cut).
func TestTruncateMessageToFit_ReturnsEmptyWhenStructureAloneTooBig(t *testing.T) {
	msg := `{"role":"user","parts":[{"type":"text","text":"anything"}]}`
	assert.Equal(t, "", truncateMessageToFit(msg, 5))
}

// Given a budget too small to fit even a shortened message beside the marker,
// When messageArrayJSON renders it, Then everything is dropped and only the
// marker remains (still valid JSON).
func TestMessageArrayJSON_SingleMessageTooLarge_MarkerOnlyFallback(t *testing.T) {
	big := `{"role":"user","parts":[{"type":"text","text":"` + strings.Repeat("x", 500) + `"}]}`
	const maxLen = 100
	got := messageArrayJSON([]string{big}, maxLen)
	assert.LessOrEqual(t, len(got), maxLen, "within the cap")

	arr := decodeArray(t, got) // must parse as valid JSON
	require.Len(t, arr, 1, "just the truncation marker")
	assert.Equal(t, "system", arr[0].Role)
	require.Len(t, arr[0].Parts, 1)
	assert.Contains(t, arr[0].Parts[0].Text, "truncated 1 earlier message(s)")
}

// Given a freshly built span, When its status and kind are inspected, Then they
// default to OK status and Internal kind.
func TestSpanBuilder_StatusAndKind(t *testing.T) {
	s := testBuilder().InvokeAgent("s", "1", "2", "p", "r")
	require.Equal(t, SpanStatusOK, s.Status.Code)
	assert.Equal(t, SpanKindInternal, s.Kind)
}

// Given a span, When MarkError is called with an error type and message, Then the
// span status becomes ERROR with that message and an error.type attribute.
func TestSpan_MarkError(t *testing.T) {
	s := testBuilder().Tool("s", "p", "1", "2", "Bash", "", "in", "")
	s.MarkError("tool_error", "Exit code 1")

	assert.Equal(t, SpanStatusError, s.Status.Code)
	assert.Equal(t, "Exit code 1", s.Status.Message)
	assert.Equal(t, "tool_error", attrMap(s)["error.type"])
}

// Given a span, When MarkError is called with a blank type and message, Then the
// status is still ERROR but the message stays empty and error.type is omitted.
func TestSpan_MarkError_OmitsBlankFields(t *testing.T) {
	s := testBuilder().LLM("s", "p", "1", "2", "", "", event.Usage{})
	s.MarkError("", "")

	assert.Equal(t, SpanStatusError, s.Status.Code, "error code survives with no detail")
	assert.Empty(t, s.Status.Message)
	_, hasErrType := attrMap(s)["error.type"]
	assert.False(t, hasErrType, "blank error type omitted")
}

// The mixed shape the adapter emits when a StopFailure carries a detail but no
// error code: the detail becomes the status message while error.type is
// omitted. Complements the both-set and both-blank cases above.
func TestSpan_MarkError_DetailWithoutType(t *testing.T) {
	s := testBuilder().LLM("s", "p", "1", "2", "", "", event.Usage{})
	s.MarkError("", "connection reset")

	assert.Equal(t, SpanStatusError, s.Status.Code)
	assert.Equal(t, "connection reset", s.Status.Message, "detail recorded without a type")
	_, hasErrType := attrMap(s)["error.type"]
	assert.False(t, hasErrType, "blank error type omitted even when detail is present")
}
