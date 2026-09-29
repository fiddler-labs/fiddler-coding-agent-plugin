// Package claudecode adapts Claude Code hook payloads into the neutral
// event model consumed by the shared pipeline.
//
// All Claude Code-specific knowledge (hook event names, payload field
// names, response shapes) lives here. Adding another coding agent means
// adding a sibling package under internal/source, not changing the
// pipeline or span builders.
package claudecode

import (
	"encoding/json"
	"fmt"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/event"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/filelog"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/transcript"
)

// error.type values assigned to a failed tool call (PostToolUseFailure). Kept
// deliberately coarse: Claude Code documents the failure's error string as an
// unstable, tool-specific format ("treat the rest as display text, not a stable
// format"), so the plugin classifies only by the stable is_interrupt signal
// rather than parsing that string.
const (
	toolErrTypeError       = "tool_error"
	toolErrTypeInterrupted = "interrupted"
)

// Runtime identity for Claude Code sessions. AgentName and Provider are
// stamped on emitted spans as gen_ai.agent.name and gen_ai.provider.name —
// they identify the coding-agent runtime being observed. Each runtime
// adapter declares its own identity.
const (
	AgentName = "claude-code"
	Provider  = "anthropic"
)

// ServiceName is the service.name resource attribute stamped on emitted
// spans. Per OTel semantics, service.name identifies the service that
// *produces* the telemetry — that is the plugin itself, not the observed
// agent runtime (which is carried by gen_ai.agent.name). Naming it per
// runtime lets each adapter declare a distinct producer identity.
const ServiceName = "fiddler-claude-code-plugin"

// Adapt converts a Claude Code hook event name and payload into a neutral
// event.Event. The second return value is false when the event is not one
// the plugin captures.
func Adapt(eventName string, payload map[string]any) (event.Event, bool) {
	sessionID := getString(payload, "session_id")
	cwd := getString(payload, "cwd")

	switch eventName {
	case "UserPromptSubmit":
		return event.Event{
			Kind:      event.KindTurnStart,
			SessionID: sessionID,
			Cwd:       cwd,
			TurnStart: &event.TurnStart{
				Prompt: getString(payload, "prompt"),
				// The transcript is one file for the whole session, so at the
				// next prompt it still holds the previous (possibly
				// interrupted) turn's entries, letting the flush reconstruct
				// that turn's leftover pending calls and LLM-call spans.
				ReadOutcomes: outcomesReader(getString(payload, "transcript_path")),
				ReadCalls:    callsReader(getString(payload, "transcript_path")),
			},
		}, true

	case "PreToolUse":
		// Fires before every tool call, whether or not it needs permission.
		// Records the pending state that anchors the call's permission
		// lifecycle; no span is emitted here.
		return event.Event{
			Kind:      event.KindPreToolUse,
			SessionID: sessionID,
			Cwd:       cwd,
			PreTool: &event.PreToolCall{
				ToolUseID:      getString(payload, "tool_use_id"),
				Name:           getString(payload, "tool_name"),
				Input:          toJSON(payload["tool_input"]),
				PermissionMode: getString(payload, "permission_mode"),
			},
		}, true

	case "PostToolUse":
		return event.Event{
			Kind:      event.KindToolCall,
			SessionID: sessionID,
			Cwd:       cwd,
			ToolCall: &event.ToolCall{
				Name:           getString(payload, "tool_name"),
				Input:          toJSON(payload["tool_input"]),
				Output:         toolResponseText(payload["tool_response"]),
				DurationMs:     getFloat(payload, "duration_ms"),
				ToolUseID:      getString(payload, "tool_use_id"),
				PermissionMode: getString(payload, "permission_mode"),
				// A top-level agent_id marks a tool a sub-agent ran; the Agent
				// launch's tool_response.agentId marks the sub-agent it spawned.
				AgentID:         getString(payload, "agent_id"),
				LaunchedAgentID: launchedAgentID(payload["tool_response"]),
			},
		}, true

	case "PostToolUseFailure":
		// A failed tool: same tool_name/tool_input as PostToolUse, but the
		// result is replaced by a top-level error string and there is no
		// tool_response, so Output stays empty.
		return event.Event{
			Kind:      event.KindToolCall,
			SessionID: sessionID,
			Cwd:       cwd,
			ToolCall: &event.ToolCall{
				Name:           getString(payload, "tool_name"),
				Input:          toJSON(payload["tool_input"]),
				DurationMs:     getFloat(payload, "duration_ms"),
				ToolUseID:      getString(payload, "tool_use_id"),
				PermissionMode: getString(payload, "permission_mode"),
				AgentID:        getString(payload, "agent_id"),
				Failed:         true,
				ErrorType:      toolErrorType(payload),
				Error:          getString(payload, "error"),
			},
		}, true

	case "PermissionDenied":
		// Auto-mode denial (classifier/safety). Carries the tool_use_id and a
		// reason; the tool never ran, so there is no output.
		return event.Event{
			Kind:      event.KindPermissionDenied,
			SessionID: sessionID,
			Cwd:       cwd,
			PermDenied: &event.PermissionDenied{
				ToolUseID:      getString(payload, "tool_use_id"),
				Name:           getString(payload, "tool_name"),
				Input:          toJSON(payload["tool_input"]),
				Reason:         getString(payload, "reason"),
				PermissionMode: getString(payload, "permission_mode"),
				// A top-level agent_id marks a tool a sub-agent ran (present only
				// when the denial fires inside a sub-agent call); the pipeline
				// nests the denied span under that sub-agent rather than the turn.
				AgentID: getString(payload, "agent_id"),
			},
		}, true

	case "SubagentStop":
		// A sub-agent finished. Its conversation is in a dedicated transcript
		// (agent_transcript_path), not the main one, so its chat spans, start
		// time, and token usage are reconstructed from there. There is no
		// parent_agent_id in the payload; the pipeline derives the sub-agent's
		// parent span from agent_id.
		//
		// The final answer may not have reached the sub-agent transcript yet,
		// so the calls/usage readers settle it first.
		agentTranscript := getString(payload, "agent_transcript_path")
		finalText := getString(payload, "last_assistant_message")
		st := newSettler(agentTranscript, finalText)
		return event.Event{
			Kind:      event.KindSubagentEnd,
			SessionID: sessionID,
			Cwd:       cwd,
			SubagentEnd: &event.SubagentEnd{
				AgentID:      getString(payload, "agent_id"),
				AgentType:    getString(payload, "agent_type"),
				ResponseText: finalText,
				ReadStart:    startReader(agentTranscript),
				ReadCalls:    st.callsReader(),
				ReadUsage:    st.wrapUsage(usageReader(agentTranscript)),
			},
		}, true

	case "Stop":
		// The hook can start before Claude Code has written the turn's final
		// assistant message to the transcript. Every transcript
		// reader settles that first, so calls, usage, and refusal detection all
		// see the final message.
		transcriptPath := getString(payload, "transcript_path")
		finalText := getString(payload, "last_assistant_message")
		st := newSettler(transcriptPath, finalText)
		return event.Event{
			Kind:      event.KindTurnEnd,
			SessionID: sessionID,
			Cwd:       cwd,
			TurnEnd: &event.TurnEnd{
				ResponseText: finalText,
				ReadUsage:    st.wrapUsage(usageReader(transcriptPath)),
				ReadCalls:    st.callsReader(),
				ReadOutcomes: st.wrapOutcomes(outcomesReader(transcriptPath)),
			},
		}, true

	case "StopFailure":
		// A failed turn: the API errored. error is a low-cardinality code
		// (rate_limit, server_error, ...) that identifies the error type;
		// error_details carries the detail; last_assistant_message here is the
		// rendered API error string, not Claude's output, so it doubles as the
		// turn's response text.
		transcriptPath := getString(payload, "transcript_path")
		errType := getString(payload, "error")
		if errType == "" {
			// error is documented as always present on StopFailure and is the
			// turn's failure classifier. An empty value means the payload
			// contract drifted: still mark the turn failed (Failed stays true),
			// but surface the anomaly rather than silently dropping error.type.
			filelog.Warn("StopFailure payload missing 'error' field: session=%s", sessionID)
		}
		return event.Event{
			Kind:      event.KindTurnEnd,
			SessionID: sessionID,
			Cwd:       cwd,
			TurnEnd: &event.TurnEnd{
				ResponseText: getString(payload, "last_assistant_message"),
				ReadUsage:    usageReader(transcriptPath),
				ReadCalls:    callsReader(transcriptPath),
				ReadOutcomes: outcomesReader(transcriptPath),
				Failed:       true,
				ErrorType:    errType,
				Error:        stopFailureDetail(payload),
			},
		}, true

	case "SessionEnd":
		return event.Event{
			Kind:      event.KindSessionEnd,
			SessionID: sessionID,
			Cwd:       cwd,
			SessionEnd: &event.SessionEnd{
				ReadOutcomes: outcomesReader(getString(payload, "transcript_path")),
				ReadCalls:    callsReader(getString(payload, "transcript_path")),
			},
		}, true

	default:
		return event.Event{}, false
	}
}

// ReadAgentVersion returns the Claude Code version for the session, read from
// the transcript referenced by the hook payload's transcript_path (the payload
// itself carries no version field). It is stamped as the app.version resource
// attribute to mirror Claude Code's native OpenTelemetry resource. Returns ""
// when the transcript path is absent or the version cannot be read (fail open).
//
// This performs a small read of the transcript (first versioned line), so the
// entrypoint calls it once per process, alongside identity resolution.
func ReadAgentVersion(payload map[string]any) string {
	return transcript.ReadVersion(getString(payload, "transcript_path"))
}

// usageReader returns a closure that reads per-turn token usage from the
// Claude Code transcript at transcriptPath, filtered to the turn that
// started at startUnixNano. Returns nil when no transcript path is given.
// The closure maps the Claude-specific transcript.TurnUsage into the
// neutral event.Usage, keeping transcript-format knowledge in this package.
func usageReader(transcriptPath string) func(startUnixNano string) event.Usage {
	if transcriptPath == "" {
		return nil
	}
	return func(startUnixNano string) event.Usage {
		u := transcript.ReadTurnUsage(transcriptPath, startUnixNano)
		return event.Usage{
			InputTokens:         u.InputTokens,
			OutputTokens:        u.OutputTokens,
			CacheReadTokens:     u.CacheReadTokens,
			CacheCreationTokens: u.CacheCreationTokens,
			Model:               u.Model,
			StopReason:          u.StopReason,
		}
	}
}

// callsReader returns a closure that reconstructs the turn's per-LLM-call spans
// from the Claude Code transcript, mapping the transcript-specific LLMCall into
// the neutral event model. Returns nil when no transcript path is given,
// keeping transcript-format knowledge in this package. The rendered input/output
// message JSON is carried through verbatim (the transcript package owns the
// message shape).
func callsReader(transcriptPath string) func(startUnixNano string) []event.LLMCall {
	if transcriptPath == "" {
		return nil
	}
	return func(startUnixNano string) []event.LLMCall {
		return toEventCalls(transcript.ReadTurnCalls(transcriptPath, startUnixNano))
	}
}

// toEventCalls maps transcript-specific LLM calls into the neutral event model.
// Returns nil for no calls.
func toEventCalls(raw []transcript.LLMCall) []event.LLMCall {
	if len(raw) == 0 {
		return nil
	}
	out := make([]event.LLMCall, len(raw))
	for i, c := range raw {
		out[i] = event.LLMCall{
			MessageID:     c.MessageID,
			StartNano:     c.StartNano,
			EndNano:       c.EndNano,
			InputMessages: c.InputMessages,
			Output:        c.Output,
			OutputText:    c.OutputText,
			InputText:     c.InputText,
			Synthesized:   c.Synthesized,
			Usage: event.Usage{
				InputTokens:         c.InputTokens,
				OutputTokens:        c.OutputTokens,
				CacheReadTokens:     c.CacheReadTokens,
				CacheCreationTokens: c.CacheCreationTokens,
				Model:               c.Model,
				StopReason:          c.StopReason,
			},
		}
	}
	return out
}

// Settle bounds used at Stop / SubagentStop. Variables (not constants) so
// tests can shorten them.
var (
	settleTimeout = transcript.DefaultSettleTimeout
	settlePoll    = transcript.DefaultSettlePoll
)

// settler makes sure a turn's final assistant message is on disk before any
// transcript reader runs. The pipeline calls the usage, calls, and
// outcomes readers in its own order, so whichever runs first triggers the
// settle, and the settled calls are reused by the calls reader. The pipeline
// passes every reader the same start, and each hook is a single short-lived
// process, so one settle per event is enough; a reader invoked with a different
// start falls back to a plain read.
type settler struct {
	path      string
	finalText string

	done    bool
	start   string
	settled []transcript.LLMCall
}

// newSettler returns a settler for transcriptPath, or nil when there is no
// transcript (every wrapper then degrades to the plain reader, which is nil too).
func newSettler(transcriptPath, finalText string) *settler {
	if transcriptPath == "" {
		return nil
	}
	return &settler{path: transcriptPath, finalText: finalText}
}

// settle runs SettleTurnCalls once for start and caches the result.
func (s *settler) settle(start string) {
	if s.done {
		return
	}
	s.done = true
	s.start = start
	s.settled, _ = transcript.SettleTurnCalls(s.path, start, s.finalText, settleTimeout, settlePoll)
}

// callsReader returns the settled calls (including a synthesized final call
// when the write never landed). Nil when there is no transcript.
func (s *settler) callsReader() func(startUnixNano string) []event.LLMCall {
	if s == nil {
		return nil
	}
	return func(start string) []event.LLMCall {
		s.settle(start)
		if start != s.start {
			return toEventCalls(transcript.ReadTurnCalls(s.path, start))
		}
		return toEventCalls(s.settled)
	}
}

// wrapUsage settles before reading usage, so the final call's tokens and stop
// reason (refusal detection) are counted when its write lands in time.
func (s *settler) wrapUsage(read func(string) event.Usage) func(string) event.Usage {
	if s == nil || read == nil {
		return read
	}
	return func(start string) event.Usage {
		s.settle(start)
		return read(start)
	}
}

// wrapOutcomes settles before classifying tool outcomes.
func (s *settler) wrapOutcomes(read func(string) map[string]event.ToolOutcome) func(string) map[string]event.ToolOutcome {
	if s == nil || read == nil {
		return read
	}
	return func(start string) map[string]event.ToolOutcome {
		s.settle(start)
		return read(start)
	}
}

// startReader returns a closure that reads a sub-agent's start time — the
// earliest entry timestamp in its dedicated transcript — as an epoch-nanoseconds
// string. Returns nil when no transcript path is given, keeping transcript-format
// knowledge in this package.
func startReader(transcriptPath string) func() string {
	if transcriptPath == "" {
		return nil
	}
	return func() string {
		return transcript.FirstEntryNano(transcriptPath)
	}
}

// launchedAgentID extracts the sub-agent id an Agent-launch tool response
// carries (tool_response.agentId). Returns "" for any response shape without it
// (i.e. every non-launching tool call), so it is a safe probe on all tool
// responses.
func launchedAgentID(resp any) string {
	m, ok := resp.(map[string]any)
	if !ok {
		return ""
	}
	id, _ := m["agentId"].(string)
	return id
}

// outcomesReader returns a closure that classifies a turn's tool calls from the
// Claude Code transcript, mapping the transcript-specific outcome enum into the
// neutral event model. Returns nil when no transcript path is given, keeping
// transcript-format knowledge in this package.
func outcomesReader(transcriptPath string) func(startUnixNano string) map[string]event.ToolOutcome {
	if transcriptPath == "" {
		return nil
	}
	return func(startUnixNano string) map[string]event.ToolOutcome {
		raw := transcript.ReadTurnOutcomes(transcriptPath, startUnixNano)
		out := make(map[string]event.ToolOutcome, len(raw))
		for id, o := range raw {
			out[id] = event.ToolOutcome{
				Kind:       mapOutcomeKind(o.Kind),
				Name:       o.Name,
				ResultNano: o.ResultNano,
				Input:      o.Input,
			}
		}
		return out
	}
}

// mapOutcomeKind translates the transcript outcome enum to the neutral one.
func mapOutcomeKind(k transcript.OutcomeKind) event.ToolOutcomeKind {
	switch k {
	case transcript.OutcomeDeniedUser:
		return event.OutcomeDeniedUser
	case transcript.OutcomeDeniedRule:
		return event.OutcomeDeniedRule
	default:
		return event.OutcomeResolved
	}
}

// toolErrorType classifies a PostToolUseFailure. Claude Code sets is_interrupt
// when the failure reached it as an abort rather than an error the tool itself
// reported; otherwise it is an ordinary tool error.
func toolErrorType(payload map[string]any) string {
	if b, _ := payload["is_interrupt"].(bool); b {
		return toolErrTypeInterrupted
	}
	return toolErrTypeError
}

// stopFailureDetail returns the human-readable detail for a StopFailure,
// preferring the richer error_details and falling back to the rendered error
// message shown in the conversation.
func stopFailureDetail(payload map[string]any) string {
	if d := getString(payload, "error_details"); d != "" {
		return d
	}
	return getString(payload, "last_assistant_message")
}

// --- Payload helpers (Claude Code payload shapes) ---

// getString returns the string value for key, or "" if absent or not a string.
func getString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// getFloat returns the float64 value for key, or 0 if absent or not a number.
// JSON numbers unmarshal to float64.
func getFloat(m map[string]any, key string) float64 {
	v, _ := m[key].(float64)
	return v
}

// toJSON serializes a value to a JSON string, returning strings as-is.
func toJSON(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// toolResponseText extracts scannable text from a tool response. Bash
// responses are maps with stdout/stderr; other tools may send a plain
// string or an arbitrary structure.
func toolResponseText(resp any) string {
	if resp == nil {
		return ""
	}
	if s, ok := resp.(string); ok {
		return s
	}
	if m, ok := resp.(map[string]any); ok {
		var parts []string
		for _, key := range []string{"stdout", "stderr"} {
			if s, ok := m[key].(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			result := parts[0]
			for _, p := range parts[1:] {
				result += "\n" + p
			}
			return result
		}
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return fmt.Sprintf("%v", resp)
	}
	return string(b)
}
