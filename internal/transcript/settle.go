package transcript

// This file handles the race between Claude Code's transcript writer and the
// Stop / SubagentStop hook.
//
// Claude Code does not append transcript entries to disk synchronously: it
// queues them and drains the queue on a short timer (about 100 ms in current
// versions). The Stop hook is started as soon as the turn ends, with the final
// answer taken from memory (last_assistant_message), so the hook can read the
// transcript before the turn's final assistant message has been written. The per-call
// reconstruction then comes up one call short: typically the post-tool summary
// call is missing.
//
// Claude Code also writes one line per content block, so a final answer that
// used extended thinking is a thinking line (text empty) then a text line, and
// the hook can read between them: the final call is on disk without its text.
//
// SettleTurnCalls detects both states and waits, bounded, for the write to land.
// If it never does, it uses the payload's answer so it is not silently lost.

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/filelog"
)

// Default bounds for SettleTurnCalls. The wait only happens when the final
// call is detected as missing; a turn whose transcript is already complete
// pays nothing.
const (
	// DefaultSettleTimeout caps how long a hook waits for the final assistant
	// message to reach the transcript: about 10x Claude Code's observed write
	// interval, so a real (token-bearing) span is preferred over a synthesized one even on
	// a loaded machine, while staying far below the 60 s hook timeout.
	DefaultSettleTimeout = 1 * time.Second
	// DefaultSettlePoll is how often the transcript size is checked while
	// waiting. A size check is a single stat; the file is re-parsed only when
	// it has grown.
	DefaultSettlePoll = 20 * time.Millisecond
)

// stopReasonToolUse is the stop reason of an assistant message that requested a
// tool call. A finished turn cannot end on one: a tool call is always followed
// by a tool result and another model call (unless the turn was cut short, in
// which case last_assistant_message is that same message or empty).
const stopReasonToolUse = "tool_use"

// SettleResult reports what SettleTurnCalls did, for logging (counts only).
type SettleResult struct {
	// Pending is true when the final answer was detected as not yet written.
	Pending bool
	// Resolved is true when the final answer appeared during the wait.
	Resolved bool
	// Synthesized is true when the wait timed out with the whole final message
	// missing, and the final call was rebuilt from the payload's final answer.
	Synthesized bool
	// Filled is true when the wait timed out with the final message on disk
	// but its text missing, and the payload's final answer was filled in.
	Filled bool
	// Waited is how long the hook waited (zero when nothing was pending).
	Waited time.Duration
}

// SettleTurnCalls reconstructs the turn's LLM calls like ReadTurnCalls, but
// first makes sure the turn's final assistant message has been written.
//
// finalText is the final answer from the hook payload (last_assistant_message).
// Two states mean the final answer has not fully reached the transcript yet
// (see finalCallPending): the whole final message is missing, or its thinking
// line is on disk but its text line is not. In either state the transcript size
// is checked every poll interval, and the file is re-read whenever it grows,
// until the final answer appears or timeout elapses. On timeout:
//   - whole message missing: a synthesized final call is appended; its input is
//     the full history on disk, its output is finalText, and it has no model or
//     token usage;
//   - text line missing: finalText is filled into the call already on disk,
//     which keeps its real model and token usage.
//
// Any other state (no transcript, no in-turn calls, empty finalText, a complete
// final message) returns ReadTurnCalls' result unchanged with no wait, so this
// is safe to use on every turn end.
func SettleTurnCalls(transcriptPath, startUnixNano, finalText string, timeout, poll time.Duration) ([]LLMCall, SettleResult) {
	scan := scanTurn(transcriptPath, startUnixNano)
	// A failed or empty scan has zero calls, so nothing is pending: no wait.
	if finalCallPending(scan.calls, finalText) == pendingNone {
		return scan.calls, SettleResult{}
	}

	res := SettleResult{Pending: true}
	began := time.Now()
	deadline := began.Add(timeout)
	size := fileSize(transcriptPath)
	for time.Now().Before(deadline) {
		time.Sleep(poll)
		s := fileSize(transcriptPath)
		if s == size {
			continue
		}
		size = s
		scan = scanTurn(transcriptPath, startUnixNano)
		if finalCallPending(scan.calls, finalText) == pendingNone {
			res.Resolved = true
			break
		}
	}
	res.Waited = time.Since(began)

	calls := scan.calls
	if !res.Resolved {
		switch finalCallPending(calls, finalText) {
		case pendingCall:
			calls = append(calls, synthesizeFinalCall(scan, finalText, began))
			res.Synthesized = true
		case pendingText:
			calls[len(calls)-1] = fillFinalText(calls[len(calls)-1], finalText)
			res.Filled = true
		}
	}

	// Counts and timings only, never content (filelog rule).
	filelog.Info("final call settle: waited_ms=%d resolved=%t synthesized=%t filled=%t calls=%d",
		res.Waited.Milliseconds(), res.Resolved, res.Synthesized, res.Filled, len(calls))
	return calls, res
}

// pendingState is what (if anything) is missing from the turn's final answer.
type pendingState int

const (
	// pendingNone: the final answer is on disk (or there is nothing to check).
	pendingNone pendingState = iota
	// pendingCall: the whole final message is missing. The last in-turn call
	// requested a tool, so a reply must follow it.
	pendingCall
	// pendingText: the final message is on disk but without its text. Claude
	// Code writes one line per content block, so a final answer that used
	// extended thinking is a thinking line (with empty text) followed by a text
	// line, and the hook can read between the two.
	pendingText
)

// finalCallPending reports what is missing from the turn's final answer, given
// the payload's final answer finalText (nothing is pending when it is empty).
//
// pendingCall: the last call ended in tool_use and finalText is not that call's
// own text. Text is compared with whitespace collapsed, because Claude Code
// joins a message's text blocks differently from OutputText. If the tool_use
// call's own text happens to equal the final answer, the wait is skipped; the
// only consequence is a missing final span (the pre-fix behavior), never wrong
// data.
//
// pendingText: the last call did not end in tool_use and has no text at all.
// This checks only for empty text, not an exact match, so a formatting
// difference between the transcript and the payload can never trigger a wait.
func finalCallPending(calls []LLMCall, finalText string) pendingState {
	if collapseSpace(finalText) == "" || len(calls) == 0 {
		return pendingNone
	}
	last := calls[len(calls)-1]
	got := collapseSpace(last.OutputText)
	if last.StopReason == stopReasonToolUse {
		if got != collapseSpace(finalText) {
			return pendingCall
		}
		return pendingNone
	}
	if got == "" {
		return pendingText
	}
	return pendingNone
}

// synthesizeFinalCall builds the final call that never reached the transcript.
// Its input is the whole history on disk (which ends with the tool result it
// received), and its output is the payload's final answer. Model and usage are
// left empty rather than guessed; Synthesized tags the span so it stays
// distinguishable from a transcript-read call.
//
// The span ends at waitBegan (when the hook started waiting), so its duration
// does not include the settle wait. It is clamped to start no later than it
// ends, since the start comes from a transcript timestamp (a different clock).
func synthesizeFinalCall(scan turnScan, finalText string, waitBegan time.Time) LLMCall {
	end := strconv.FormatInt(waitBegan.UnixNano(), 10)
	start := end
	if n := len(scan.calls); n > 0 && scan.calls[n-1].EndNano != "" {
		start = scan.calls[n-1].EndNano
		if s, err := strconv.ParseInt(start, 10, 64); err == nil && s > waitBegan.UnixNano() {
			end = start
		}
	}
	parts := []callPart{{Type: "text", Text: finalText}}
	return LLMCall{
		// Full slice expression caps capacity so a later append never writes
		// into scan.history's backing array.
		InputMessages: scan.history[:len(scan.history):len(scan.history)],
		Output:        renderMessage(roleAssistant, parts),
		OutputText:    finalText,
		InputText:     scan.lastUserText,
		StartNano:     start,
		EndNano:       end,
		Synthesized:   true,
	}
}

// fillFinalText completes a final call whose text line never reached the
// transcript: finalText becomes its OutputText and is appended to its rendered
// Output as a text part. Everything else (model, token usage, timing, input) is
// the real transcript data and is kept as is. The call is not tagged: apart
// from where the text came from, it is identical to a fully read call.
func fillFinalText(c LLMCall, finalText string) LLMCall {
	c.OutputText = finalText
	var msg renderedMessage
	if err := json.Unmarshal([]byte(c.Output), &msg); err != nil || msg.Role == "" {
		msg = renderedMessage{Role: roleAssistant}
	}
	c.Output = renderMessage(msg.Role, append(msg.Parts, callPart{Type: "text", Text: finalText}))
	return c
}

// fileSize returns the size of path, or -1 when it cannot be stat'd.
func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return fi.Size()
}

// collapseSpace trims s and collapses every run of whitespace to one space.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
