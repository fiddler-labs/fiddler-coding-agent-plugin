// Package pipeline consumes normalized events and orchestrates the
// per-turn trace lifecycle: it manages the per-session context that
// stitches spans across hook processes and emits gen_ai spans.
//
// It is runtime-agnostic: it operates on internal/event values produced
// by a per-runtime adapter (internal/source/<runtime>), never on a
// specific agent's payload shape.
package pipeline

import (
	"fmt"
	"strconv"
	"time"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/agentctx"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/config"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/event"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/filelog"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/otlp"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/toolctx"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/turnctx"
)

// Process dispatches a normalized event to the appropriate handler.
// Errors are logged but never surfaced to the caller (fail open).
func Process(ev event.Event, cfg *config.Config) {
	if ev.SessionID == "" {
		filelog.Warn("event has no session id; skipping")
		return
	}

	switch ev.Kind {
	case event.KindTurnStart:
		handleTurnStart(cfg, ev)
	case event.KindPreToolUse:
		handlePreToolUse(ev)
	case event.KindToolCall:
		handleToolCall(cfg, ev)
	case event.KindPermissionDenied:
		handlePermissionDenied(cfg, ev)
	case event.KindTurnEnd:
		handleTurnEnd(cfg, ev)
	case event.KindSubagentEnd:
		handleSubagentEnd(cfg, ev)
	case event.KindSessionEnd:
		handleSessionEnd(cfg, ev)
	default:
		filelog.Info("unhandled event kind: %d", ev.Kind)
	}
}

// handleTurnStart allocates and persists trace context for the new turn.
// Emits nothing for the new turn — its spans are built later by tool calls
// and turn end.
//
// A context file already present at this point belongs to a previous turn
// whose TurnEnd never arrived (e.g. the user interrupted it): its tool spans
// were emitted but its root span was not. Flush that root now so those spans
// reparent instead of being orphaned, before New overwrites the file.
func handleTurnStart(cfg *config.Config, ev event.Event) {
	if stale, ok := turnctx.Peek(ev.SessionID); ok {
		// The next prompt runs under a generous hook budget, so the flush uses
		// the default export timeout.
		flushInterruptedTurn(cfg, ev.SessionID, stale, ev.TurnStart.ReadOutcomes, ev.TurnStart.ReadCalls, otlp.DefaultTimeout)
	}

	// Age-based GC of pending records left by sessions that crashed or never
	// resumed. It runs after any interrupted-turn flush so it never reaps a
	// record that flush still needs, and its cutoff is far larger than any
	// interrupt-then-resume gap, so a record from an in-progress interrupted
	// turn is never mistaken for an abandoned one.
	toolctx.SweepStale()

	// Reclaim sub-agent trace records left by sessions that crashed or whose
	// SubagentStop never arrived. Age-based like toolctx.SweepStale, and swept
	// here (not at TurnEnd) because an async sub-agent's record must outlive the
	// turn that launched it.
	agentctx.SweepStale()

	_, err := turnctx.New(ev.SessionID, ev.TurnStart.Prompt, ev.Cwd)
	if err != nil {
		filelog.Error("TurnStart: create context: %v", err)
	}
}

// handlePreToolUse records the pending state for a tool call about to run, so
// its permission lifecycle can be stitched at resolution. It emits no span.
func handlePreToolUse(ev event.Event) {
	pt := ev.PreTool
	if pt.ToolUseID == "" {
		filelog.Warn("PreToolUse without tool_use_id; skipping pending record")
		return
	}
	rec := toolctx.Record{
		ToolName:       pt.Name,
		ToolInput:      pt.Input,
		PermissionMode: pt.PermissionMode,
		PreNano:        fmt.Sprintf("%d", time.Now().UnixNano()),
	}
	if err := toolctx.Write(ev.SessionID, pt.ToolUseID, rec); err != nil {
		filelog.Error("PreToolUse: write pending: %v", err)
	}
}

// handleSessionEnd flushes the root span of a turn still in progress when the
// session ends (its TurnEnd never arrived), then clears the context. Without
// this, a session that ends mid-turn leaves that turn's tool spans orphaned.
func handleSessionEnd(cfg *config.Config, ev event.Event) {
	if stale, ok := turnctx.Peek(ev.SessionID); ok {
		// SessionEnd hooks run under a tight (~1.5s) budget, so the flush export
		// uses a sub-budget deadline to fail open cleanly rather than be killed.
		var readOutcomes func(string) map[string]event.ToolOutcome
		var readCalls func(string) []event.LLMCall
		if ev.SessionEnd != nil {
			readOutcomes = ev.SessionEnd.ReadOutcomes
			readCalls = ev.SessionEnd.ReadCalls
		}
		flushInterruptedTurn(cfg, ev.SessionID, stale, readOutcomes, readCalls, sessionEndExportTimeout)
	}
	turnctx.Clear(ev.SessionID)
	// Sub-agent trace records are session-scoped and are never swept at TurnEnd
	// (they must outlive the launching turn for async sub-agents), so the session
	// is where they are reclaimed. Any still present had no SubagentStop.
	agentctx.SweepSession(ev.SessionID)
}

// flushInterruptedTurn emits the root invoke_agent span for a turn whose
// TurnEnd never arrived, so its already-emitted tool spans reparent onto it
// instead of showing as orphans, and reconstructs that turn's leftover pending
// tool calls (denied / abandoned) from the transcript.
//
// The root's turn-level LLM span needs token usage and the response text, which
// are available only on a real TurnEnd, so it is intentionally skipped here.
// The true end time is unknown, so the moment the interruption is detected is
// used as an upper bound, and the span is tagged fiddler.coding_agent.turn.recovered
// so incomplete turns stay distinguishable.
//
// The root is marked ERROR with error.type="interrupted". The turn never
// confirmed success (it never reached Stop), so neither OK nor Unset is an
// honest status; ERROR is the only one that does not misrepresent an unknown
// outcome as a clean completion. "interrupted" mirrors the tool-level
// is_interrupt classification, keeping benign interrupts distinguishable from
// real failures downstream.
//
// readOutcomes classifies the interrupted turn's tool calls from the transcript
// (may be nil). readCalls reconstructs the interrupted turn's completed LLM calls
// (may be nil); they are emitted as OK chat spans — they finished before the
// interruption, which is carried by the root's ERROR status, not by these calls.
// exportTimeout bounds the batched export (shorter at SessionEnd). The
// reconstructed spans are sent with the root in one batch, then the session's
// pending records are swept.
func flushInterruptedTurn(cfg *config.Config, sessionID string, ctx *turnctx.Context,
	readOutcomes func(startUnixNano string) map[string]event.ToolOutcome,
	readCalls func(startUnixNano string) []event.LLMCall, exportTimeout time.Duration) {
	now := fmt.Sprintf("%d", time.Now().UnixNano())
	start := ctx.StartNano
	if start == "" {
		start = now
	}

	sb := spanBuilder(cfg, sessionID, ctx.TraceID)
	// An interrupted turn never reached Stop, so there is no final answer to stamp.
	root := sb.InvokeAgent(ctx.RootSpanID, start, now, ctx.UserPrompt, "")
	root.Attributes = append(root.Attributes, otlp.BoolAttr(otlp.AttrTurnRecovered, true))
	root.MarkError("interrupted", "turn ended without Stop")

	batch := []otlp.Span{root}
	// Completed LLM calls of the interrupted turn become OK chat spans (see the
	// readCalls note above).
	if readCalls != nil {
		batch = append(batch, llmCallSpans(sb, ctx.RootSpanID, readCalls(start), start, now)...)
	}
	var outcomes map[string]event.ToolOutcome
	if readOutcomes != nil {
		// Use the start fallback, not the raw ctx.StartNano: an empty StartNano
		// would make the classifier scan the entire session transcript (see
		// ReadTurnOutcomes) and reconstruct denials from prior turns. This
		// mirrors handleTurnEnd, which also passes the fallback-corrected start.
		outcomes = readOutcomes(start)
	}
	batch = append(batch, reconstructLeftovers(sb, ctx.RootSpanID, sessionID, outcomes)...)

	if err := otlp.SendWithTimeout(cfg, batch, exportTimeout); err != nil {
		filelog.Error("flushInterruptedTurn: send: %v", err)
	}
	toolctx.SweepSession(sessionID)

	filelog.Info("interrupted turn recovered: session=%s trace_id=%s root_span_id=%s spans=%d",
		sessionID, ctx.TraceID, ctx.RootSpanID, len(batch))
}

// handleToolCall emits a tool span, nested according to whether the call is a
// main-agent tool, a tool a sub-agent ran, or an Agent tool that launched a
// sub-agent (see resolveToolSpan).
func handleToolCall(cfg *config.Config, ev event.Event) {
	traceID, spanID, parentSpanID, ok := resolveToolSpan(ev.SessionID, ev.ToolCall)
	if !ok {
		return // resolveToolSpan logged the reason
	}

	// Real tool duration: the adapter supplies DurationMs (tool execution
	// time). The span ends now and starts DurationMs earlier. When it is
	// absent (or non-positive), emit a zero-duration span rather than a
	// fabricated one — nesting is by parentSpanId, not by time.
	now := time.Now().UnixNano()
	start := now
	if ev.ToolCall.DurationMs > 0 {
		start = now - int64(ev.ToolCall.DurationMs*float64(time.Millisecond))
	}

	sb := spanBuilder(cfg, ev.SessionID, traceID)
	span := sb.Tool(
		spanID,
		parentSpanID,
		fmt.Sprintf("%d", start),
		fmt.Sprintf("%d", now),
		ev.ToolCall.Name,
		ev.ToolCall.ToolUseID,
		ev.ToolCall.Input,
		ev.ToolCall.Output,
	)

	// A failed tool (PostToolUseFailure) is marked ERROR so it is
	// distinguishable from a successful call in the trace. The permission
	// decision is still "accept" — the tool ran; only its execution failed.
	if ev.ToolCall.Failed {
		span.MarkError(ev.ToolCall.ErrorType, ev.ToolCall.Error)
	}

	// The tool ran, so permission was granted. Stamp the permission decision,
	// mode, and (from the pending record) the estimated blocked-on-user wait,
	// then clear the pending record. Missing pending degrades gracefully:
	// decision + mode still land, only the wait is omitted.
	//
	// Mode comes from the resolution payload first (authoritative for this
	// call) and falls back to the pending record only if the payload omits it.
	// The wait is an estimate that includes hook/IPC overhead — see
	// otlp.PermissionInfo — so an auto-allowed call reports a near-zero wait.
	pi := otlp.PermissionInfo{Decision: otlp.PermissionAccept, Mode: ev.ToolCall.PermissionMode}
	if rec, ok := toolctx.Read(ev.SessionID, ev.ToolCall.ToolUseID); ok {
		if pi.Mode == "" {
			pi.Mode = rec.PermissionMode
		}
		if ms, ok := waitMs(rec.PreNano, now, ev.ToolCall.DurationMs); ok {
			pi.WaitMs = ms
			pi.HasWait = true
			pi.WaitSource = otlp.WaitEstimated
		}
		toolctx.Delete(ev.SessionID, ev.ToolCall.ToolUseID)
	}
	span.SetPermission(pi)

	if err := otlp.Send(cfg, []otlp.Span{span}); err != nil {
		filelog.Error("ToolCall: send: %v", err)
	}

	// Identifiers only, never content (filelog rule). The agent ids carry the
	// sub-agent routing (agent_id: a tool a sub-agent ran; launched_agent_id: a
	// sub-agent this call spawned).
	filelog.Info("tool span emitted: session=%s tool=%s agent_id=%q launched_agent_id=%q trace_id=%s span_id=%s parent_span_id=%s",
		ev.SessionID, ev.ToolCall.Name, ev.ToolCall.AgentID, ev.ToolCall.LaunchedAgentID,
		traceID, spanID, parentSpanID)
}

// resolveToolSpan resolves the trace id, span id, and parent span id for a tool
// call, by the two sub-agent ids it may carry:
//
//   - An Agent launch (LaunchedAgentID set) gets the derived id A(launched) that
//     the sub-agent subtree parents onto, and nests under its launcher: the
//     sub-agent root S(AgentID) for a launch made inside another sub-agent, or
//     the turn root for a main-agent launch. The launcher's trace is recorded
//     under the launched agent_id (agentctx) so the sub-agent reuses it.
//   - A tool a sub-agent ran (AgentID set, no LaunchedAgentID) reuses that
//     sub-agent's trace from agentctx and parents under its root S(AgentID).
//   - A main-agent tool (neither id set) parents under the turn root in the
//     turn's trace.
//
// A sub-agent call takes its trace from agentctx; when that record is not found
// it resolves against the turn only while the turn is open (see turnRootIfOpen).
// ok is false when there is nothing to emit: the turn context cannot be loaded
// for a main-agent call, or a sub-agent call arrives after its turn has ended.
func resolveToolSpan(sessionID string, tc *event.ToolCall) (traceID, spanID, parentSpanID string, ok bool) {
	switch {
	case tc.LaunchedAgentID != "":
		if tc.AgentID != "" {
			// A launch made inside another sub-agent: nest under that sub-agent's
			// root S(AgentID) in its trace.
			if rec, found := agentctx.Read(sessionID, tc.AgentID); found && rec.TraceID != "" {
				trace := rec.TraceID
				writeAgentTrace(sessionID, tc.LaunchedAgentID, trace)
				return trace, agentToolSpanID(trace, tc.LaunchedAgentID), subagentSpanID(trace, tc.AgentID), true
			}
			// The launching sub-agent's trace is not on record: use the turn root
			// while the turn is open, and skip otherwise.
			trace, root, ok := turnRootIfOpen(sessionID)
			if !ok {
				return "", "", "", false
			}
			writeAgentTrace(sessionID, tc.LaunchedAgentID, trace)
			return trace, agentToolSpanID(trace, tc.LaunchedAgentID), root, true
		}
		// A main-agent launch: nest under the turn root in the turn's trace.
		ctx, err := turnctx.Load(sessionID)
		if err != nil {
			filelog.Error("ToolCall: load context: %v", err)
			return "", "", "", false
		}
		writeAgentTrace(sessionID, tc.LaunchedAgentID, ctx.TraceID)
		return ctx.TraceID, agentToolSpanID(ctx.TraceID, tc.LaunchedAgentID), ctx.RootSpanID, true

	case tc.AgentID != "":
		// A tool a sub-agent ran: reuse the sub-agent's trace from agentctx and
		// parent under its root S(AgentID). When the record is not found, use the
		// turn root while the turn is open, and skip otherwise.
		trace, parent, ok := resolveAgentOrTurn(sessionID, tc.AgentID, "ToolCall")
		if !ok {
			return "", "", "", false
		}
		return trace, otlp.NewSpanID(), parent, true

	default:
		// A main-agent tool: turn root in the turn's trace.
		ctx, err := turnctx.Load(sessionID)
		if err != nil {
			filelog.Error("ToolCall: load context: %v", err)
			return "", "", "", false
		}
		return ctx.TraceID, otlp.NewSpanID(), ctx.RootSpanID, true
	}
}

// turnRootIfOpen returns the current turn's trace and root span, or ok=false when
// no turn is open. It never creates a turn context.
func turnRootIfOpen(sessionID string) (traceID, rootSpanID string, ok bool) {
	ctx, found := turnctx.Peek(sessionID)
	if !found {
		return "", "", false
	}
	return ctx.TraceID, ctx.RootSpanID, true
}

// writeAgentTrace persists the launching turn's trace under a sub-agent's
// agent_id so the sub-agent's own tool hooks and its SubagentStop resolve the
// same trace, even after the launching turn's Stop cleared turnctx. Fails open.
func writeAgentTrace(sessionID, agentID, traceID string) {
	if err := agentctx.Write(sessionID, agentID, agentctx.Record{TraceID: traceID}); err != nil {
		filelog.Error("ToolCall: write agentctx: %v", err)
	}
}

// agentToolSpanID and subagentSpanID derive the two span ids that link a
// sub-agent's subtree to the Agent tool call that launched it, from the
// launching turn's trace: A(agent_id) is the Agent-launch execute_tool span,
// S(agent_id) is the sub-agent's invoke_agent root. Both are pure functions of
// (trace, agent_id), so isolated hook processes agree with no shared map.
func agentToolSpanID(traceID, agentID string) string {
	return otlp.SpanIDFrom(traceID, "agent-tool:"+agentID)
}

func subagentSpanID(traceID, agentID string) string {
	return otlp.SpanIDFrom(traceID, "subagent:"+agentID)
}

// resolveAgentOrTurn returns the trace and parent span for a call that may
// belong to a sub-agent. With an agent_id whose launch trace is on record
// (agentctx), it nests under the sub-agent root S(agentID) in the launching
// turn's trace. When the agent_id has no record, it uses the turn root while the
// turn is open and returns ok=false otherwise. A call with no agent_id (the main
// agent) resolves against the turn's trace. ok is false when there is nothing to
// emit: a sub-agent call whose turn has ended, or a main-agent call whose turn
// context cannot be loaded.
//
// This is the resolution shared by resolveToolSpan and handlePermissionDenied,
// so a sub-agent tool and a denial of that same tool land in the same trace
// under the same sub-agent root. The caller assigns the leaf span's own id.
// caller labels the failure log so it identifies which handler could not load
// the turn context.
func resolveAgentOrTurn(sessionID, agentID, caller string) (trace, parent string, ok bool) {
	if agentID != "" {
		if rec, found := agentctx.Read(sessionID, agentID); found && rec.TraceID != "" {
			return rec.TraceID, subagentSpanID(rec.TraceID, agentID), true
		}
		return turnRootIfOpen(sessionID)
	}
	ctx, err := turnctx.Load(sessionID)
	if err != nil {
		filelog.Error("%s: load context: %v", caller, err)
		return "", "", false
	}
	return ctx.TraceID, ctx.RootSpanID, true
}

// handlePermissionDenied emits a rejected execute_tool span for an auto-mode
// denial. The tool never ran, so the span has no result and is marked ERROR
// with error.type=permission_denied. The pending record supplies the pre time
// (for the wait) and mode; it is then cleared.
//
// The wait here is time-to-auto-decision (pre -> denial), not time blocked on a
// human — auto denials are not prompted — and is labelled estimated like the
// accept path.
//
// Trace resolution mirrors resolveToolSpan: a denial carrying a top-level
// agent_id (a sub-agent's tool) nests under that sub-agent's root S(agent_id) in
// the launching turn's trace (agentctx), so it lands in the same trace as the
// sub-agent's successful tool spans. This matters for an async sub-agent whose
// denial fires after the launching turn's Stop: a bare turnctx.Load would find
// no context and lazily mint a stray trace, scattering the span. A main-agent
// denial (no agent_id) resolves to the turn root as before.
func handlePermissionDenied(cfg *config.Config, ev event.Event) {
	pd := ev.PermDenied
	trace, parent, ok := resolveAgentOrTurn(ev.SessionID, pd.AgentID, "PermissionDenied")
	if !ok {
		return // resolveAgentOrTurn logged the reason
	}

	now := time.Now().UnixNano()
	start := now

	pi := otlp.PermissionInfo{
		Decision:     otlp.PermissionReject,
		DenialKind:   otlp.DenialAutoClassifier,
		DenialReason: pd.Reason,
		Mode:         pd.PermissionMode,
	}
	if rec, ok := toolctx.Read(ev.SessionID, pd.ToolUseID); ok {
		if pi.Mode == "" {
			pi.Mode = rec.PermissionMode
		}
		if pre, perr := strconv.ParseInt(rec.PreNano, 10, 64); perr == nil {
			start = pre
			pi.WaitMs = (now - pre) / int64(time.Millisecond)
			pi.HasWait = true
			pi.WaitSource = otlp.WaitEstimated
		}
		toolctx.Delete(ev.SessionID, pd.ToolUseID)
	}

	sb := spanBuilder(cfg, ev.SessionID, trace)
	span := sb.Tool(
		otlp.NewSpanID(),
		parent,
		fmt.Sprintf("%d", start),
		fmt.Sprintf("%d", now),
		pd.Name,
		pd.ToolUseID,
		pd.Input,
		"",
	)
	span.MarkError(otlp.ErrTypePermissionDenied, pd.Reason)
	span.SetPermission(pi)

	if err := otlp.Send(cfg, []otlp.Span{span}); err != nil {
		filelog.Error("PermissionDenied: send: %v", err)
	}

	filelog.Info("permission-denied span emitted: session=%s tool=%s agent_id=%q parent=%s",
		ev.SessionID, pd.Name, pd.AgentID, parent)
}

// waitMs estimates the blocked-on-user wait for an accepted call from the
// pending pre-execution time: the interval pre->now spans both the wait and
// the execution, and duration_ms (which excludes permission-prompt time) is
// the execution part, so wait = (now - pre) - duration_ms. Returns false when
// pre is unparseable. Negative results clamp to zero.
//
// This is an estimate, not a measurement: it includes hook/IPC overhead (so an
// auto-allowed call, never prompted, reports a small non-user value), and if
// duration_ms is absent it also includes execution time. Callers label it
// WaitSource=estimated.
func waitMs(preNano string, now int64, durationMs float64) (int64, bool) {
	pre, err := strconv.ParseInt(preNano, 10, 64)
	if err != nil {
		return 0, false
	}
	ms := (now-pre)/int64(time.Millisecond) - int64(durationMs)
	if ms < 0 {
		ms = 0
	}
	return ms, true
}

// sessionEndExportTimeout bounds the SessionEnd flush export. It sits below the
// ~1.5s SessionEnd hook budget (leaving headroom for process spawn, config
// load, and the transcript read) so a slow endpoint makes the export fail open
// cleanly rather than being SIGKILLed mid-POST.
const sessionEndExportTimeout = 1200 * time.Millisecond

// turnEndExportTimeout bounds the Stop (TurnEnd) batch export. That batch is far
// heavier than a live tool span: it carries the root invoke_agent span
// plus one chat span per LLM call, each with a growing gen_ai.input.messages
// snapshot, so a whole turn's payload can reach several MB. The 2s DefaultTimeout
// used for live single-span sends is too tight for it — a healthy but non-trivial
// batch could exceed 2s and be dropped whole, orphaning the turn's tool spans and
// losing the root. This deadline gives the batch room to land while
// staying well under Claude Code's 60s hook ceiling. It applies only to this
// once-per-turn send; live tool sends keep the tighter DefaultTimeout so per-tool
// responsiveness against a slow endpoint is unchanged.
const turnEndExportTimeout = 10 * time.Second

// reconstructLeftovers builds spans for tool calls that got a PreToolUse
// pending record but never resolved via a live hook, classifying each against
// the transcript outcomes:
//   - denied (user / rule) -> a rejected execute_tool span;
//   - resolved (ran)       -> skipped (already emitted live at PostToolUse);
//   - absent from outcomes -> an unresolved execute_tool span.
//
// It also emits a rejected span for a denial that appears in the transcript but
// has no pending record (a missed PreToolUse), using the transcript's name and
// input. It does not delete records; the caller sweeps the session afterward.
func reconstructLeftovers(sb otlp.SpanBuilder, rootSpanID, sessionID string,
	outcomes map[string]event.ToolOutcome) []otlp.Span {
	records := toolctx.List(sessionID)
	if len(records) == 0 && len(outcomes) == 0 {
		return nil
	}
	var spans []otlp.Span
	handled := make(map[string]bool, len(records))
	for id, rec := range records {
		handled[id] = true
		o, ok := outcomes[id]
		switch {
		case ok && o.Kind == event.OutcomeResolved:
			// Ran and produced a result; already emitted live at PostToolUse.
		case ok && o.Kind == event.OutcomeDeniedUser:
			spans = append(spans, denialSpan(sb, rootSpanID, id, rec.ToolName, rec.ToolInput, rec.PermissionMode, otlp.DenialUserRejected, rec.PreNano, o.ResultNano))
		case ok && o.Kind == event.OutcomeDeniedRule:
			spans = append(spans, denialSpan(sb, rootSpanID, id, rec.ToolName, rec.ToolInput, rec.PermissionMode, otlp.DenialPermissionRule, rec.PreNano, o.ResultNano))
		default:
			// No transcript result for this id: requested but never resolved.
			spans = append(spans, unresolvedSpan(sb, rootSpanID, id, rec.ToolName, rec.ToolInput, rec.PermissionMode, rec.PreNano))
		}
	}
	// Denials in the transcript with no pending record (PreToolUse was missed):
	// reconstruct from the transcript alone, without mode or wait.
	//
	// This cannot double-emit an auto-classifier denial already emitted live by
	// handlePermissionDenied (which deletes its pending record, so its id also
	// reaches this loop): the live PermissionDenied hook fires only for
	// auto-classifier denials, and their transcript tool_result never carries
	// toolDenialKind in {user-rejected, permission-rule} — ReadTurnOutcomes maps
	// only those two to a denial and everything else to OutcomeResolved, which is
	// skipped below. Manual ("user-rejected") and deny-rule ("permission-rule")
	// denials fire no live hook, so they exist only here. This rests on Claude
	// Code's observed behavior that an auto-classifier denial never records those
	// two kinds; TestReconstruct_LiveAutoDenyNotReemitted guards it.
	for id, o := range outcomes {
		if handled[id] {
			continue
		}
		switch o.Kind {
		case event.OutcomeDeniedUser:
			spans = append(spans, denialSpan(sb, rootSpanID, id, o.Name, o.Input, "", otlp.DenialUserRejected, "", o.ResultNano))
		case event.OutcomeDeniedRule:
			spans = append(spans, denialSpan(sb, rootSpanID, id, o.Name, o.Input, "", otlp.DenialPermissionRule, "", o.ResultNano))
		}
	}
	return spans
}

// denialSpan builds a rejected execute_tool span for a denied call: decision =
// reject with the denial kind, marked ERROR with error.type=permission_denied,
// no result (the tool never ran). The span covers pre->result when both are
// known.
func denialSpan(sb otlp.SpanBuilder, rootSpanID, id, name, input, mode, denialKind, preNano, resultNano string) otlp.Span {
	start, end := spanBounds(preNano, resultNano)
	span := sb.Tool(otlp.NewSpanID(), rootSpanID, start, end, name, id, input, "")
	span.MarkError(otlp.ErrTypePermissionDenied, "")
	pi := otlp.PermissionInfo{Decision: otlp.PermissionReject, DenialKind: denialKind, Mode: mode}
	if ms, ok := waitBetween(preNano, resultNano); ok {
		pi.WaitMs = ms
		pi.HasWait = true
		pi.WaitSource = otlp.WaitEstimated
	}
	span.SetPermission(pi)
	return span
}

// unresolvedSpan builds a span for a tool call that was requested but never
// resolved (e.g. the user abandoned the permission prompt). Its outcome is
// unknown, so it has no result, no wait, and status UNSET — not ERROR, which
// would misrepresent an abandoned call as a failure. It is a point-in-time
// marker anchored at the recorded pre-execution time.
func unresolvedSpan(sb otlp.SpanBuilder, rootSpanID, id, name, input, mode, preNano string) otlp.Span {
	at := preNano
	if at == "" {
		at = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	span := sb.Tool(otlp.NewSpanID(), rootSpanID, at, at, name, id, input, "")
	span.Status = otlp.SpanStatus{Code: otlp.SpanStatusUnset}
	span.SetPermission(otlp.PermissionInfo{Decision: otlp.PermissionUnresolved, Mode: mode})
	return span
}

// spanBounds returns start,end span timestamps, preferring pre->result and
// falling back to whichever is present (or "now" if neither). When both bounds
// are present it clamps end >= start: preNano is a hook's time.Now() while
// resultNano is derived from a transcript ISO timestamp (different clocks), so
// under clock skew the result could otherwise precede the pre and yield a span
// that ends before it starts. Symmetric with waitBetween's >= 0 clamp.
func spanBounds(preNano, resultNano string) (string, string) {
	switch {
	case preNano != "" && resultNano != "":
		if pre, err1 := strconv.ParseInt(preNano, 10, 64); err1 == nil {
			if res, err2 := strconv.ParseInt(resultNano, 10, 64); err2 == nil && res < pre {
				return preNano, preNano
			}
		}
		return preNano, resultNano
	case resultNano != "":
		return resultNano, resultNano
	case preNano != "":
		return preNano, preNano
	default:
		now := fmt.Sprintf("%d", time.Now().UnixNano())
		return now, now
	}
}

// waitBetween estimates the blocked-on-user wait for a reconstructed denial as
// resultNano - preNano (ms), clamped to >= 0. Returns false when either bound
// is missing or unparseable.
func waitBetween(preNano, resultNano string) (int64, bool) {
	pre, err1 := strconv.ParseInt(preNano, 10, 64)
	res, err2 := strconv.ParseInt(resultNano, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	ms := (res - pre) / int64(time.Millisecond)
	if ms < 0 {
		ms = 0
	}
	return ms, true
}

// llmCallSpans builds one chat span per reconstructed LLM call (one assistant
// message.id), each a child of the turn's root and carrying that call's full
// input message history plus its output. Returns nil when there are no calls,
// so callers can fall back to the aggregated LLM span. fallbackStart/End back
// up any call whose own timestamp was unparseable, so no span is emitted with
// an empty time bound.
//
// Each span's gen_ai.llm.input.user scalar is that call's own input delta — the
// newest user message preceding it — so it differs call-to-call rather than
// repeating the turn prompt.
func llmCallSpans(sb otlp.SpanBuilder, rootSpanID string, calls []event.LLMCall, fallbackStart, fallbackEnd string) []otlp.Span {
	if len(calls) == 0 {
		return nil
	}
	spans := make([]otlp.Span, 0, len(calls))
	for _, c := range calls {
		start := c.StartNano
		if start == "" {
			start = fallbackStart
		}
		end := c.EndNano
		if end == "" {
			end = fallbackEnd
		}
		span := sb.LLMCall(
			otlp.NewSpanID(),
			rootSpanID,
			start,
			end,
			c.InputText,
			c.InputMessages,
			c.Output,
			c.OutputText,
			c.Usage,
		)
		if c.Synthesized {
			span.Attributes = append(span.Attributes, otlp.BoolAttr(otlp.AttrLLMCallSynthesized, true))
		}
		spans = append(spans, span)
	}
	return spans
}

// handleTurnEnd emits the root invoke_agent span (backfilled with the persisted
// start time) plus the turn's LLM spans, then clears the context. The LLM spans
// are one reconstructed chat span per API call; when that
// reconstruction is unavailable it falls back to a single aggregated LLM span.
func handleTurnEnd(cfg *config.Config, ev event.Event) {
	ctx, err := turnctx.Load(ev.SessionID)
	if err != nil {
		filelog.Error("TurnEnd: load context: %v", err)
		return
	}

	now := fmt.Sprintf("%d", time.Now().UnixNano())
	start := ctx.StartNano
	if start == "" {
		start = now
	}

	sb := spanBuilder(cfg, ev.SessionID, ctx.TraceID)

	// Root invoke_agent span: span id == persisted root so tool spans
	// parent to it.
	agentSpan := sb.InvokeAgent(
		ctx.RootSpanID,
		start,
		now,
		ctx.UserPrompt,
		ev.TurnEnd.ResponseText,
	)

	// Transcript-sourced token usage/model for the turn. The adapter
	// supplies ReadUsage (it owns the transcript format); the pipeline
	// provides the turn start so usage is filtered to this turn.
	var usage event.Usage
	if ev.TurnEnd.ReadUsage != nil {
		usage = ev.TurnEnd.ReadUsage(start)
	}

	// Per-LLM-call chat spans reconstructed from the transcript: one span per
	// API call (assistant message.id), each carrying the full input message
	// history that preceded it and that call's output. This replaces
	// the single aggregated LLM span so each API call is observed individually.
	// The adapter supplies ReadCalls (it owns the transcript format); the
	// pipeline passes the turn start so the reconstruction is scoped to this turn.
	var calls []event.LLMCall
	if ev.TurnEnd.ReadCalls != nil {
		calls = ev.TurnEnd.ReadCalls(start)
	}
	callSpans := llmCallSpans(sb, ctx.RootSpanID, calls, start, now)

	// A failed turn (StopFailure) or a refusal marks the root ERROR: a
	// StopFailure is an API error, and a refusal is a completed call the model
	// declined (surfaced only via the transcript stop reason, so it arrives on a
	// normal Stop). Either way the turn did not produce a real response.
	failed, errType, detail := turnError(ev.TurnEnd, usage)
	if failed {
		agentSpan.MarkError(errType, detail)
	}

	batch := []otlp.Span{agentSpan}
	if len(callSpans) > 0 {
		// A refusal is a completed call the model declined: the refused assistant
		// message is the last reconstructed call, so that span carries the error.
		// A StopFailure is an API error that produced no assistant message to
		// reconstruct, so it is left to the root's ERROR status above — the
		// per-call spans here are all genuinely-successful calls and must not be
		// mislabeled. (A StopFailure on the very first call reconstructs nothing
		// and takes the aggregated-fallback branch below instead.)
		if failed && !ev.TurnEnd.Failed {
			callSpans[len(callSpans)-1].MarkError(errType, detail)
		}
		batch = append(batch, callSpans...)
	} else {
		// Fallback: a single aggregated LLM span carrying the prompt and response
		// together, so a turn with no reconstructable calls still gets a
		// self-contained chat span.
		llmSpan := sb.LLM(otlp.NewSpanID(), ctx.RootSpanID, start, now, ctx.UserPrompt, ev.TurnEnd.ResponseText, usage)
		if failed {
			llmSpan.MarkError(errType, detail)
		}
		batch = append(batch, llmSpan)
	}

	// Reconstruct any tool call that got a PreToolUse record but no live
	// resolution: a denial (reject) or an abandoned call (unresolved). This
	// uses the already-loaded ctx (parented to its root) and runs before Clear;
	// re-Loading after Clear would lazily create a fresh trace and orphan these
	// spans. The pending records are swept after the batch is emitted.
	var outcomes map[string]event.ToolOutcome
	if ev.TurnEnd.ReadOutcomes != nil {
		outcomes = ev.TurnEnd.ReadOutcomes(start)
	}
	batch = append(batch, reconstructLeftovers(sb, ctx.RootSpanID, ev.SessionID, outcomes)...)

	if err := otlp.SendWithTimeout(cfg, batch, turnEndExportTimeout); err != nil {
		filelog.Error("TurnEnd: send: %v", err)
	}
	// Sweep the whole session's pending dir (not per-record deletes): every
	// leftover was just classified and emitted above, so this reclaims them all,
	// including any unmatched stragglers. Safe because turns are sequential
	// within a session (the same non-concurrency assumption the write-once
	// context file relies on).
	toolctx.SweepSession(ev.SessionID)

	filelog.Info("turn-end spans emitted: session=%s trace_id=%s root_span_id=%s spans=%d",
		ev.SessionID, ctx.TraceID, ctx.RootSpanID, len(batch))

	turnctx.Clear(ev.SessionID)
}

// handleSubagentEnd completes a sub-agent's subtree. A sub-agent's conversation
// lives in a dedicated transcript (agent_transcript_path), not the main one, so
// on SubagentStop its chat spans and tokens are reconstructed from there and
// emitted as a sub-agent invoke_agent root (id S(agent_id)) parented to the
// Agent tool call that launched it (A(agent_id)).
//
// The trace comes only from agentctx (written at the Agent launch), never from
// turnctx. For an async sub-agent turnctx was already cleared at the launching
// turn's Stop, so a turnctx.Load here would lazily mint a fresh trace and emit a
// second, bogus root in a separate trace. On an agentctx miss (no launch
// record, a duplicate SubagentStop after Delete, or a record reclaimed by
// SweepStale) it therefore skips silently rather than fall back.
//
// The sub-agent's own tool spans already nested live under S(agent_id) via
// resolveToolSpan (a sub-agent's internal tools fire PostToolUse in the parent
// session carrying a top-level agent_id), so this handler reconstructs the chat
// spans only; it never re-emits tool spans.
func handleSubagentEnd(cfg *config.Config, ev event.Event) {
	se := ev.SubagentEnd
	if se == nil || se.AgentID == "" {
		filelog.Warn("SubagentEnd without agent_id; skipping")
		return
	}

	rec, ok := agentctx.Read(ev.SessionID, se.AgentID)
	if !ok || rec.TraceID == "" {
		// No launch record for this sub-agent: the Agent launch was never observed,
		// this is a duplicate SubagentStop (its record was already Deleted), or the
		// record was reclaimed by SweepStale. There is nothing to nest onto and
		// minting a trace here would split the sub-agent into a separate trace,
		// so skip.
		filelog.Info("subagent skipped (no agentctx record): session=%s agent_id=%s",
			ev.SessionID, se.AgentID)
		return
	}
	trace := rec.TraceID

	now := fmt.Sprintf("%d", time.Now().UnixNano())
	// The readers keep transcript entries at or after readFrom. This is the
	// sub-agent's own dedicated transcript — only its entries, no neighbouring turn
	// to exclude — so when its start is unknown ("") the whole file is the correct
	// range, and an empty floor reads it all. spanStart instead bounds the emitted
	// spans, so unlike the reader floor it needs a real timestamp and falls back to now.
	readFrom := ""
	if se.ReadStart != nil {
		readFrom = se.ReadStart()
	}
	spanStart := readFrom
	if spanStart == "" {
		spanStart = now
	}

	sb := spanBuilder(cfg, ev.SessionID, trace)
	// Name the sub-agent by its type ("Explore", ...); wrapper agents report "" and
	// keep the configured agent name. This also names the sub-agent's chat spans.
	if se.AgentType != "" {
		sb.AgentName = se.AgentType
	}

	// Per-call chat spans reconstructed from the sub-agent's dedicated transcript.
	// The first call's input delta is the sub-agent's first user message — its task
	// prompt — so it doubles as the invoke_agent root's input.
	var calls []event.LLMCall
	if se.ReadCalls != nil {
		calls = se.ReadCalls(readFrom)
	}
	taskPrompt := ""
	if len(calls) > 0 {
		taskPrompt = calls[0].InputText
	}

	subSpanID := subagentSpanID(trace, se.AgentID)
	// InvokeAgent hardcodes an empty parent (it is the turn root for a main turn);
	// a sub-agent root instead parents to the Agent-launch span A(agent_id).
	sub := sb.InvokeAgent(subSpanID, spanStart, now, taskPrompt, se.ResponseText)
	sub.ParentSpanID = agentToolSpanID(trace, se.AgentID)

	batch := []otlp.Span{sub}
	callSpans := llmCallSpans(sb, subSpanID, calls, spanStart, now)
	if len(callSpans) > 0 {
		batch = append(batch, callSpans...)
	} else {
		// Fallback (mirrors handleTurnEnd): a single aggregated chat span carrying
		// the sub-agent's prompt/response together, so a sub-agent with no
		// reconstructable per-call spans still gets one self-contained chat span.
		var usage event.Usage
		if se.ReadUsage != nil {
			usage = se.ReadUsage(readFrom)
		}
		batch = append(batch, sb.LLM(otlp.NewSpanID(), subSpanID, spanStart, now, taskPrompt, se.ResponseText, usage))
	}

	if err := otlp.SendWithTimeout(cfg, batch, turnEndExportTimeout); err != nil {
		filelog.Error("SubagentEnd: send: %v", err)
	}

	// One record per sub-agent, Deleted at its SubagentStop: a duplicate stop then
	// hits the skip-on-miss above, and SessionEnd's sweep has less to reclaim.
	agentctx.Delete(ev.SessionID, se.AgentID)

	filelog.Info("subagent spans emitted: session=%s agent_id=%s trace_id=%s span_id=%s parent_span_id=%s spans=%d",
		ev.SessionID, se.AgentID, trace, subSpanID, sub.ParentSpanID, len(batch))
}

// turnError decides whether a turn ended in an error and, if so, its class and
// detail. A StopFailure sets TurnEnd.Failed explicitly; a refusal is inferred
// from the transcript stop reason on an otherwise-normal Stop. The explicit
// failure takes precedence when both are present.
func turnError(te *event.TurnEnd, usage event.Usage) (failed bool, errType, detail string) {
	if te.Failed {
		return true, te.ErrorType, te.Error
	}
	if usage.StopReason == "refusal" {
		return true, "refusal", "model refused to respond"
	}
	return false, "", ""
}

// spanBuilder assembles the per-turn span builder from the turn's trace id
// and the runtime identity carried on the config.
func spanBuilder(cfg *config.Config, sessionID, traceID string) otlp.SpanBuilder {
	return otlp.SpanBuilder{
		TraceID:   traceID,
		SessionID: sessionID,
		AgentName: cfg.AgentName,
		Provider:  cfg.Provider,
	}
}
