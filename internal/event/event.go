// Package event defines the runtime-agnostic event model that the shared
// pipeline consumes.
//
// Each coding agent runtime has its own hook payload shape. A per-runtime
// adapter (see internal/source/<runtime>)
// parses that payload into these neutral types, so the pipeline and span
// builders never depend on a specific runtime's field names.
package event

// Kind identifies the lifecycle event a hook represents, normalized across
// runtimes.
type Kind int

const (
	// KindUnknown is an event the adapter does not map to a capture action.
	KindUnknown Kind = iota
	// KindTurnStart begins a turn (Claude Code: UserPromptSubmit).
	KindTurnStart
	// KindPreToolUse marks a tool call about to run (Claude Code: PreToolUse).
	// It records a pending record so the call's permission lifecycle can be
	// stitched at resolution; it emits no span of its own.
	KindPreToolUse
	// KindToolCall is a completed tool invocation (Claude Code: PostToolUse).
	KindToolCall
	// KindPermissionDenied is an auto-mode permission denial (Claude Code:
	// PermissionDenied). It carries the tool_use_id and a reason and produces
	// a rejected execute_tool span.
	KindPermissionDenied
	// KindTurnEnd ends a turn (Claude Code: Stop).
	KindTurnEnd
	// KindSubagentEnd marks a sub-agent finishing (Claude Code: SubagentStop).
	// It carries the sub-agent's agent_id and a reader over its dedicated
	// transcript, so the pipeline can emit the sub-agent's span subtree nested
	// under the Agent tool call that launched it.
	KindSubagentEnd
	// KindSessionEnd ends a session (Claude Code: SessionEnd). It carries no
	// payload beyond SessionID; the pipeline uses it to flush any turn whose
	// TurnEnd never arrived (e.g. the session ended mid-turn).
	KindSessionEnd
)

// Event is a normalized hook event. Exactly one of the payload pointers is
// populated, matching Kind.
type Event struct {
	Kind      Kind
	SessionID string
	Cwd       string

	TurnStart   *TurnStart
	PreTool     *PreToolCall
	ToolCall    *ToolCall
	PermDenied  *PermissionDenied
	TurnEnd     *TurnEnd
	SubagentEnd *SubagentEnd
	SessionEnd  *SessionEnd
}

// TurnStart carries the data available when a turn begins.
//
// ReadOutcomes, when non-nil, classifies the tool calls of the *previous*
// (possibly interrupted) turn from the transcript, so its leftover pending
// records can be reconstructed when the new turn's start flushes it. It is a
// closure supplied by the runtime adapter (which owns the transcript format).
//
// ReadCalls reconstructs the *previous* turn's LLM calls from the transcript,
// so the interrupted-turn flush can emit their chat spans (it runs on the same
// stale-context path as ReadOutcomes). Nil when no transcript path.
type TurnStart struct {
	Prompt       string
	ReadOutcomes func(startUnixNano string) map[string]ToolOutcome
	ReadCalls    func(startUnixNano string) []LLMCall
}

// SessionEnd carries the data available when a session ends. ReadOutcomes
// classifies a turn still in progress at session end (its Stop never arrived),
// so its leftover pending records can be reconstructed during the final flush.
// ReadCalls reconstructs that turn's LLM calls for the same final flush.
type SessionEnd struct {
	ReadOutcomes func(startUnixNano string) map[string]ToolOutcome
	ReadCalls    func(startUnixNano string) []LLMCall
}

// LLMCall is one reconstructed LLM API call within a turn (one assistant
// message). InputMessages is the full ordered conversation history that
// preceded the call and Output is the call's assistant message, both
// pre-rendered as JSON message objects by the runtime adapter (which owns the
// transcript format) so the pipeline and span builders stay runtime-agnostic.
// Usage is this call's own token usage/model/stop reason.
type LLMCall struct {
	MessageID string
	// StartNano/EndNano bound the call span; StartNano abuts the previous call's
	// end (or the turn start for the first call).
	StartNano string
	EndNano   string
	// InputMessages holds the rendered JSON message objects preceding this call,
	// oldest first; Output is the rendered assistant response message.
	InputMessages []string
	Output        string
	// OutputText is the plain text of this call's response (its "text" parts
	// only, excluding thinking/tool_use). It backs the gen_ai.llm.output scalar,
	// alongside the structured Output.
	OutputText string
	// InputText is the plain text of the newest user-role message preceding this
	// call (the user's prompt for the first call, the tool result(s) for a
	// tool-continuation call). It backs the gen_ai.llm.input.user scalar and so
	// differs call-to-call; the full prior history stays in InputMessages.
	InputText string
	Usage     Usage
	// Synthesized marks a call rebuilt from the hook payload's final answer
	// because its transcript entry had not been written in time. It carries no
	// model or token usage, and its span is tagged so it stays distinguishable.
	Synthesized bool
}

// ToolOutcomeKind is how a tool call resolved, as read from the transcript.
type ToolOutcomeKind int

const (
	// OutcomeResolved means the tool produced an id-keyed result (success or an
	// execution error) and was emitted live; reconstruction skips it.
	OutcomeResolved ToolOutcomeKind = iota
	// OutcomeDeniedUser is a manual permission-dialog denial.
	OutcomeDeniedUser
	// OutcomeDeniedRule is a deny-rule match.
	OutcomeDeniedRule
)

// ToolOutcome is a tool call's transcript-derived outcome plus the tool name and
// input recovered from the transcript (a fallback used when the pending record
// is missing).
type ToolOutcome struct {
	Kind ToolOutcomeKind
	Name string
	// ResultNano is the epoch-nanoseconds timestamp of the tool_result entry
	// (empty when unknown), used to estimate a reconstructed denial's wait.
	ResultNano string
	Input      string
}

// PreToolCall carries the data available before a tool runs (Claude Code:
// PreToolUse). It anchors the permission lifecycle: the pipeline records it so
// resolution events can recover the pre-execution time and permission mode.
type PreToolCall struct {
	ToolUseID      string
	Name           string
	Input          string
	PermissionMode string
}

// ToolCall carries the data for one completed tool invocation.
type ToolCall struct {
	Name       string
	Input      string
	Output     string
	DurationMs float64

	// ToolUseID is the runtime's per-invocation id for the tool call. It
	// correlates the tool span with native OTel events that key on the same id,
	// and is emitted as gen_ai.tool.call.id. May be empty when the payload omits
	// it.
	ToolUseID string

	// PermissionMode is the active permission mode reported on the resolution
	// payload (default/plan/acceptEdits/auto/dontAsk/bypassPermissions). Stored
	// verbatim; may be empty.
	PermissionMode string

	// AgentID is the sub-agent this tool call ran *inside*, when the payload
	// carries a top-level agent_id (a tool a sub-agent executed). Empty for a
	// main-agent tool call. The pipeline uses it to parent the tool span under
	// that sub-agent's span rather than the turn root.
	AgentID string

	// LaunchedAgentID is the sub-agent this tool call *launched*, when the call
	// is an Agent tool whose response carries an agentId (tool_response.agentId).
	// Empty for any non-launching tool call. The pipeline uses it to give the
	// Agent launch span the derived id the sub-agent's subtree parents onto.
	LaunchedAgentID string

	// Failed marks a tool invocation that errored (Claude Code:
	// PostToolUseFailure). When set, ErrorType is a low-cardinality class
	// and Error is the failure detail; Output is typically empty because a
	// failed tool returns no result.
	Failed    bool
	ErrorType string
	Error     string
}

// PermissionDenied carries an auto-mode permission denial (Claude Code:
// PermissionDenied). The tool never ran, so there is no output; Reason is the
// classifier/safety message.
type PermissionDenied struct {
	ToolUseID      string
	Name           string
	Input          string
	Reason         string
	PermissionMode string

	// AgentID is the sub-agent this denied tool ran *inside*, when the payload
	// carries a top-level agent_id (present only when the hook fires inside a
	// sub-agent call). Empty for a main-agent denial. The pipeline uses it to
	// parent the denied span under that sub-agent's span rather than the turn
	// root, mirroring ToolCall.AgentID.
	AgentID string
}

// TurnEnd carries the data available when a turn ends.
//
// ReadUsage, when non-nil, returns transcript-sourced token usage for the
// turn. It is a closure supplied by the runtime adapter (which owns the
// transcript format) and called by the pipeline with the turn's start
// timestamp, so the pipeline never depends on a specific runtime's
// transcript parsing.
type TurnEnd struct {
	ResponseText string
	ReadUsage    func(startUnixNano string) Usage

	// ReadCalls reconstructs this turn's per-LLM-call spans (input history +
	// per-call output/usage) from the transcript. The pipeline emits one chat
	// span per call, falling back to a single aggregated span (from ResponseText
	// + ReadUsage) when this is nil or returns nothing. Supplied by the runtime
	// adapter; nil when no transcript path.
	ReadCalls func(startUnixNano string) []LLMCall

	// ReadOutcomes classifies this turn's tool calls from the transcript, so
	// leftover pending records (denied / abandoned) can be reconstructed at
	// turn end. Supplied by the runtime adapter; nil when no transcript path.
	ReadOutcomes func(startUnixNano string) map[string]ToolOutcome

	// Failed marks a turn that ended in an API error (Claude Code:
	// StopFailure). When set, ErrorType is a low-cardinality class (e.g.
	// rate_limit) and Error is the detail. A refusal is detected separately
	// by the pipeline from the turn's stop reason, so Failed stays false for
	// it here.
	Failed    bool
	ErrorType string
	Error     string
}

// SubagentEnd carries the data available when a sub-agent finishes (Claude
// Code: SubagentStop). A sub-agent's conversation lives in its own transcript
// file (not the main one), so its LLM/chat spans and tokens are reconstructed
// from there and nested under the Agent tool call that launched it.
//
// AgentID identifies the sub-agent; the pipeline derives its span ids from it
// (there is no parent_agent_id in the payload). AgentType is the sub-agent's
// type ("Explore", or "" for wrapper agents), stamped as the agent name.
// ResponseText is the sub-agent's final answer (last_assistant_message).
//
// ReadStart returns the sub-agent's start time — the earliest entry timestamp
// in its transcript — as an epoch-nanoseconds string ("" when unknown). It
// bounds the sub-agent's invoke_agent span and seeds ReadCalls/ReadUsage, which
// (like their TurnEnd counterparts) filter to entries at/after that start.
// ReadCalls reconstructs the sub-agent's per-call chat spans; ReadUsage its
// aggregate token usage. All three are closures supplied by the runtime adapter
// (which owns the transcript format); each is nil when no transcript path.
type SubagentEnd struct {
	AgentID      string
	AgentType    string
	ResponseText string
	ReadStart    func() string
	ReadCalls    func(startUnixNano string) []LLMCall
	ReadUsage    func(startUnixNano string) Usage
}

// Usage holds per-turn token usage and model metadata, deduplicated across
// the turn's model calls.
type Usage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	Model               string
	StopReason          string
}
