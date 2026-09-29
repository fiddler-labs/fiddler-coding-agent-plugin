package otlp

// This file builds spans using OpenTelemetry GenAI semantic conventions.
// Spans are classified by gen_ai.operation.name.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/event"
)

// GenAI attribute keys. Most follow the OpenTelemetry GenAI semantic
// conventions. gen_ai.llm.input.user and gen_ai.llm.output are not part of
// those conventions: they are plugin-emitted plain-text summaries of a span's
// input and output, kept alongside the standard structured message attributes.
const (
	attrConversationID = "gen_ai.conversation.id"
	attrAgentName      = "gen_ai.agent.name"
	attrProviderName   = "gen_ai.provider.name"
	attrOperationName  = "gen_ai.operation.name"
	attrToolName       = "gen_ai.tool.name"
	attrToolCallID     = "gen_ai.tool.call.id"
	attrToolArguments  = "gen_ai.tool.call.arguments"
	attrToolResult     = "gen_ai.tool.call.result"
	attrLLMInputUser   = "gen_ai.llm.input.user"
	attrLLMOutput      = "gen_ai.llm.output"
	// attrInputMessages / attrOutputMessages carry the whole per-call message
	// history and the call's response as a single JSON array each ({role,
	// parts:[...]}). Unlike the other content attributes they get the larger
	// MaxMessagesAttrLen budget (see messageArrayJSON).
	attrInputMessages  = "gen_ai.input.messages"
	attrOutputMessages = "gen_ai.output.messages"
	attrInputTokens    = "gen_ai.usage.input_tokens"
	attrOutputTokens   = "gen_ai.usage.output_tokens"
	attrCacheReadTok   = "gen_ai.usage.cache_read.input_tokens"
	attrCacheCreateTok = "gen_ai.usage.cache_creation.input_tokens"
	attrRequestModel   = "gen_ai.request.model"
	attrFinishReasons  = "gen_ai.response.finish_reasons"
	// attrErrorType is the OTel semantic-convention attribute naming the
	// class of error that made the operation fail. It is low-cardinality
	// (e.g. rate_limit, tool_error) and pairs with the span's ERROR status.
	attrErrorType = "error.type"
)

// Vendor attributes (fiddler.coding_agent.*). These are plugin-owned
// signals with no OpenTelemetry GenAI equivalent, namespaced under the
// plugin's own prefix and kept runtime-agnostic.
const (
	// AttrTurnRecovered tags a root invoke_agent span reconstructed for a turn
	// whose Stop never arrived. Exported because the pipeline stamps it.
	AttrTurnRecovered = "fiddler.coding_agent.turn.recovered"

	// AttrLLMCallSynthesized tags a chat span rebuilt from the hook payload's
	// final answer because the call's transcript entry had not been written by
	// the time the turn ended. Such a span has no model or token usage.
	// Exported because the pipeline stamps it.
	AttrLLMCallSynthesized = "fiddler.coding_agent.llm_call.synthesized"

	attrPermDecision     = "fiddler.coding_agent.permission.decision"
	attrPermMode         = "fiddler.coding_agent.permission.mode"
	attrPermDenialKind   = "fiddler.coding_agent.permission.denial_kind"
	attrPermDenialReason = "fiddler.coding_agent.permission.denial_reason"
	attrPermWaitMs       = "fiddler.coding_agent.permission.wait_ms"
	attrPermWaitSource   = "fiddler.coding_agent.permission.wait.source"
)

// Permission attribute values (fiddler.coding_agent.permission.*). Exported so
// the pipeline sets them without duplicating string literals.
const (
	PermissionAccept     = "accept"
	PermissionReject     = "reject"
	PermissionUnresolved = "unresolved" // requested but never resolved (abandoned)

	DenialAutoClassifier = "auto-classifier" // auto-mode classifier/safety block
	DenialUserRejected   = "user-rejected"   // manual permission-dialog "no"
	DenialPermissionRule = "permission-rule" // a deny rule matched

	// WaitEstimated marks a wait derived by subtraction rather than measured
	// directly. Every wait emitted is estimated.
	WaitEstimated = "estimated"

	// ErrTypePermissionDenied is the error.type stamped on a denied
	// execute_tool span. The key is standard OTel; this value is custom.
	ErrTypePermissionDenied = "permission_denied"
)

// gen_ai.operation.name values, per the OpenTelemetry GenAI semantic
// conventions.
const (
	opInvokeAgent = "invoke_agent"
	opChat        = "chat"
	opExecuteTool = "execute_tool"
)

// Span names. The operation is carried by gen_ai.operation.name, not the name.
const (
	nameInvokeAgent = "invoke_agent"
	nameLLM         = "chat"
	nameTool        = "execute_tool"
)

// SpanBuilder builds the per-turn gen_ai spans. It holds the invariants
// shared by every span in a turn — the trace id, session id, and the
// runtime's identity (agent name + provider) — so the runtime, not this
// package, decides those values.
type SpanBuilder struct {
	TraceID   string
	SessionID string
	AgentName string
	Provider  string
}

// baseAttrs returns the attributes stamped on every span.
func (b SpanBuilder) baseAttrs() []KeyValue {
	return []KeyValue{
		StringAttr(attrConversationID, b.SessionID),
		StringAttr(attrAgentName, b.AgentName),
		StringAttr(attrProviderName, b.Provider),
	}
}

// InvokeAgent builds the root agent span for a turn. It carries the turn's
// input/output pair — the user's prompt and the agent's final answer — because
// the invoke_agent span models the whole turn (user asked X, agent answered Y).
// The response is stamped here from the reliable hook payload rather than the
// transcript, so the final answer is always captured on this span even when
// transcript reconstruction of the per-call chat spans lags or comes up empty.
// Both scalars are always stamped (as "" when empty) so an empty value is
// explicit rather than absent.
func (b SpanBuilder) InvokeAgent(spanID, startNano, endNano, prompt, response string) Span {
	attrs := b.baseAttrs()
	attrs = append(attrs, StringAttr(attrOperationName, opInvokeAgent))
	attrs = append(attrs, StringAttr(attrLLMInputUser, Truncate(prompt, MaxAttrLen)))
	attrs = append(attrs, StringAttr(attrLLMOutput, Truncate(response, MaxAttrLen)))
	return NewSpan(nameInvokeAgent, b.TraceID, spanID, "", startNano, endNano, attrs)
}

// LLM builds the turn-level LLM span, child of the invoke_agent span.
// Token usage, model, and finish reason are stamped when present in usage.
//
// Both the user prompt and the response are stamped here so the chat span is
// self-contained: each chat span carries its own input and output. The prompt
// is also carried on the invoke_agent root; that duplication is intentional.
func (b SpanBuilder) LLM(spanID, parentSpanID, startNano, endNano, prompt, response string, usage event.Usage) Span {
	attrs := b.baseAttrs()
	attrs = append(attrs, StringAttr(attrOperationName, opChat))
	attrs = append(attrs, StringAttr(attrLLMInputUser, Truncate(prompt, MaxAttrLen)))
	attrs = append(attrs, StringAttr(attrLLMOutput, Truncate(response, MaxAttrLen)))
	attrs = append(attrs,
		IntAttr(attrInputTokens, usage.InputTokens),
		IntAttr(attrOutputTokens, usage.OutputTokens),
		IntAttr(attrCacheReadTok, usage.CacheReadTokens),
		IntAttr(attrCacheCreateTok, usage.CacheCreationTokens),
	)
	if usage.Model != "" {
		attrs = append(attrs, StringAttr(attrRequestModel, usage.Model))
	}
	if usage.StopReason != "" {
		attrs = append(attrs, StringAttr(attrFinishReasons, usage.StopReason))
	}
	return NewSpan(nameLLM, b.TraceID, spanID, parentSpanID, startNano, endNano, attrs)
}

// LLMCall builds one reconstructed chat span for a single LLM API call (one
// assistant message.id), child of the invoke_agent span. Unlike LLM (the
// aggregated turn-level span), it carries the full per-call message history as
// gen_ai.input.messages and the call's response as gen_ai.output.messages —
// JSON arrays of {role, parts:[...]}, so a reconstructed turn lines up
// call-for-call with the individual API calls. These two attributes get the
// larger MaxMessagesAttrLen budget (keep-newest truncation); usage/model/finish
// reason are stamped as on LLM.
//
// Alongside the arrays it also stamps the scalar gen_ai.llm.input.user and
// gen_ai.llm.output attributes.
// inputUser is this call's input delta — the newest user-role message that
// preceded it (the user's prompt for the first call, the tool result for a
// tool-continuation call), so it differs call-to-call, matching native
// per-request spans; the full prior history lives in gen_ai.input.messages.
// outputText is this call's visible response text (text parts only). Both
// scalars are always stamped (as "" when empty) so an empty value is explicit
// rather than absent.
func (b SpanBuilder) LLMCall(spanID, parentSpanID, startNano, endNano, inputUser string, inputMessages []string, output, outputText string, usage event.Usage) Span {
	attrs := b.baseAttrs()
	attrs = append(attrs, StringAttr(attrOperationName, opChat))
	attrs = append(attrs, StringAttr(attrLLMInputUser, Truncate(inputUser, MaxAttrLen)))
	attrs = append(attrs, StringAttr(attrLLMOutput, Truncate(outputText, MaxAttrLen)))
	if len(inputMessages) > 0 {
		attrs = append(attrs, StringAttr(attrInputMessages, messageArrayJSON(inputMessages, MaxMessagesAttrLen)))
	}
	if output != "" {
		attrs = append(attrs, StringAttr(attrOutputMessages, messageArrayJSON([]string{output}, MaxMessagesAttrLen)))
	}
	attrs = append(attrs,
		IntAttr(attrInputTokens, usage.InputTokens),
		IntAttr(attrOutputTokens, usage.OutputTokens),
		IntAttr(attrCacheReadTok, usage.CacheReadTokens),
		IntAttr(attrCacheCreateTok, usage.CacheCreationTokens),
	)
	if usage.Model != "" {
		attrs = append(attrs, StringAttr(attrRequestModel, usage.Model))
	}
	if usage.StopReason != "" {
		attrs = append(attrs, StringAttr(attrFinishReasons, usage.StopReason))
	}
	return NewSpan(nameLLM, b.TraceID, spanID, parentSpanID, startNano, endNano, attrs)
}

// messageArrayJSON renders pre-serialized message objects (each already a JSON
// {"role","parts":[...]} object, oldest first) as a single JSON array string,
// keep-newest truncated to fit maxLen bytes. When older messages are dropped a
// leading marker message records how many, so the drop is visible downstream.
//
// The result is always valid JSON. When even the newest message alone exceeds
// the budget it is kept but its content is shortened in place (leaf strings are
// truncated, the JSON wrapper is never cut), and the marker records that the
// shown message was shortened too. Only if the shortened message still cannot
// fit beside the marker is everything dropped (marker alone, then "[]").
func messageArrayJSON(messages []string, maxLen int) string {
	if len(messages) == 0 {
		return "[]"
	}
	full := "[" + strings.Join(messages, ",") + "]"
	if len(full) <= maxLen {
		return full
	}
	// Keep-newest: walk newest -> oldest, accumulating while the kept messages
	// plus a marker recording the dropped count still fit. Size is tracked
	// incrementally rather than re-joining each step.
	keptBytes := 0 // sum of kept message lengths, separators excluded
	start := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		newKept := len(messages) - i
		// "[" + marker + "," + join(kept, ",") + "]"
		trial := 1 + len(truncationMarker(i, false)) + 1 + (keptBytes + len(messages[i]) + (newKept - 1)) + 1
		if trial > maxLen {
			break
		}
		keptBytes += len(messages[i])
		start = i
	}
	if start == len(messages) {
		// Not even the newest message fits whole beside a marker. Keep it but
		// shorten the text inside it so the array stays valid JSON, and record both
		// the older messages dropped and that the shown message was shortened.
		dropped := len(messages) - 1
		marker := truncationMarker(dropped, true)
		budget := maxLen - len(marker) - len("[,]") // "[" + marker + "," + msg + "]"
		if budget > 0 {
			shortened := truncateMessageToFit(messages[len(messages)-1], budget)
			if shortened != "" && len("["+marker+","+shortened+"]") <= maxLen {
				return "[" + marker + "," + shortened + "]"
			}
		}
		// The shortened message still won't fit beside the marker: drop everything
		// and emit the marker alone (all messages dropped), still valid JSON. Fall
		// back to an empty array only if even that overflows.
		markerOnly := "[" + truncationMarker(len(messages), false) + "]"
		if len(markerOnly) <= maxLen {
			return markerOnly
		}
		return "[]"
	}
	// The loop sized the kept set to fit maxLen, so this is already within budget.
	return "[" + truncationMarker(start, false) + "," + strings.Join(messages[start:], ",") + "]"
}

// truncateMessageToFit shortens a single pre-serialized message object so its
// JSON encoding is at most maxLen bytes, without ever breaking the JSON
// structure. It caps every string leaf in the message to a shared length and
// lowers that cap by the measured overflow each pass until the re-encoded
// message fits — mirroring how OTLP SDKs truncate: only leaf strings shrink, the
// {role,parts:[...]} wrapper and any numbers are preserved, so the result is
// always valid JSON. Returns "" if the message cannot fit even with every string
// emptied (its structure alone exceeds maxLen).
func truncateMessageToFit(message string, maxLen int) string {
	if len(message) <= maxLen {
		return message
	}
	dec := json.NewDecoder(strings.NewReader(message))
	dec.UseNumber() // keep numbers exact rather than reformatting them
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return ""
	}
	// The cap only ever decreases, so capping in place across passes is safe. No
	// leaf needs to exceed the whole budget, so start there.
	for cap := maxLen; ; {
		capStrings(v, cap)
		encoded, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		if len(encoded) <= maxLen {
			return string(encoded)
		}
		if cap == 0 {
			return "" // even with every string emptied the structure is too big
		}
		// We know exactly how far over budget we are, so lower the cap by that
		// overflow: trimming a leaf by N source bytes removes at least N encoded
		// bytes, so this lands within budget in a pass or two and keeps nearly all
		// the allowed content. Clamp at 0 so the loop still makes a final
		// all-strings-emptied pass and terminates.
		cap -= len(encoded) - maxLen
		if cap < 0 {
			cap = 0
		}
	}
}

// capStrings truncates every string leaf reachable in v (a decoded JSON value)
// to at most capBytes bytes, in place. Objects and arrays are walked
// recursively; object keys, numbers, bools, and null are left untouched.
func capStrings(v interface{}, capBytes int) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			if s, ok := child.(string); ok {
				t[k] = Truncate(s, capBytes)
				continue
			}
			capStrings(child, capBytes)
		}
	case []interface{}:
		for i, child := range t {
			if s, ok := child.(string); ok {
				t[i] = Truncate(s, capBytes)
				continue
			}
			capStrings(child, capBytes)
		}
	}
}

// truncationMarker builds the leading synthetic message that records how many
// older messages keep-newest truncation dropped, and — when shortened is set —
// that the message shown alongside it was itself shortened to fit. It is a valid
// {role,parts} object so the array parses cleanly; the text is JSON-escaped via
// Marshal.
func truncationMarker(dropped int, shortened bool) string {
	var msg string
	switch {
	case dropped > 0 && shortened:
		msg = fmt.Sprintf("[truncated %d earlier message(s); the message shown was also shortened]", dropped)
	case shortened:
		msg = "[the message shown was shortened to fit]"
	default:
		msg = fmt.Sprintf("[truncated %d earlier message(s)]", dropped)
	}
	text, err := json.Marshal(msg)
	if err != nil {
		return `{"role":"system","parts":[]}`
	}
	return `{"role":"system","parts":[{"type":"text","text":` + string(text) + `}]}`
}

// MarkError flips a span to ERROR status. errType is a low-cardinality class
// stamped as the error.type attribute (OTel convention); detail is the
// human-readable message recorded on the span status (truncated). A blank
// errType or detail is omitted, so the ERROR code always survives even when
// the runtime supplies no detail.
func (s *Span) MarkError(errType, detail string) {
	s.Status = SpanStatus{Code: SpanStatusError}
	if detail != "" {
		s.Status.Message = Truncate(detail, MaxAttrLen)
	}
	if errType != "" {
		s.Attributes = append(s.Attributes, StringAttr(attrErrorType, errType))
	}
}

// PermissionInfo carries the permission-lifecycle facts stamped on an
// execute_tool span as fiddler.coding_agent.permission.* attributes. Empty
// fields are omitted, so callers set only what they know for a given outcome.
//
// WaitMs is an estimate of time blocked on the user, not an exact measurement,
// and callers should pair it with WaitSource=estimated. Because it is derived
// by subtraction it includes non-user latency, so read small values loosely:
//   - auto-allowed (no prompt): the value is really hook/IPC overhead, ~0;
//   - user-prompted accept: ≈ the human wait, minus the tiny pre->prompt gap;
//   - auto-deny: it is time-to-auto-decision, not time blocked on a human;
//   - when duration_ms is absent it may also include execution time.
type PermissionInfo struct {
	Decision     string // accept | reject | unresolved
	Mode         string // permission mode, verbatim from the payload
	DenialKind   string // set on denials: auto-classifier | user-rejected | permission-rule
	DenialReason string // set on auto-mode denials
	WaitMs       int64  // estimated blocked-on-user duration; emitted only when HasWait (see type doc)
	WaitSource   string // "estimated" (see WaitEstimated)
	HasWait      bool
}

// SetPermission appends the permission attributes to a span, omitting empties.
// The wait is emitted only when HasWait, so a genuinely-unknown wait is absent
// rather than reported as zero.
func (s *Span) SetPermission(p PermissionInfo) {
	if p.Decision != "" {
		s.Attributes = append(s.Attributes, StringAttr(attrPermDecision, p.Decision))
	}
	if p.Mode != "" {
		s.Attributes = append(s.Attributes, StringAttr(attrPermMode, p.Mode))
	}
	if p.DenialKind != "" {
		s.Attributes = append(s.Attributes, StringAttr(attrPermDenialKind, p.DenialKind))
	}
	if p.DenialReason != "" {
		s.Attributes = append(s.Attributes, StringAttr(attrPermDenialReason, Truncate(p.DenialReason, MaxAttrLen)))
	}
	if p.HasWait {
		s.Attributes = append(s.Attributes, IntAttr(attrPermWaitMs, p.WaitMs))
		if p.WaitSource != "" {
			s.Attributes = append(s.Attributes, StringAttr(attrPermWaitSource, p.WaitSource))
		}
	}
}

// Tool builds a tool span, child of the invoke_agent span.
func (b SpanBuilder) Tool(spanID, parentSpanID, startNano, endNano, toolName, toolCallID, toolInput, toolOutput string) Span {
	attrs := b.baseAttrs()
	attrs = append(attrs,
		StringAttr(attrOperationName, opExecuteTool),
		StringAttr(attrToolName, toolName),
		StringAttr(attrToolArguments, Truncate(toolInput, MaxAttrLen)),
		StringAttr(attrToolResult, Truncate(toolOutput, MaxAttrLen)),
	)
	if toolCallID != "" {
		attrs = append(attrs, StringAttr(attrToolCallID, toolCallID))
	}
	return NewSpan(nameTool, b.TraceID, spanID, parentSpanID, startNano, endNano, attrs)
}
