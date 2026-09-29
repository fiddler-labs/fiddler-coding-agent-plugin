package transcript

// This file reconstructs the per-LLM-call view of a turn from the session
// transcript: one entry per assistant message.id (each message.id is exactly
// one LLM API call), carrying that call's own token usage/model/stop reason,
// its rendered output message, and a snapshot of the full input message
// history that preceded it.
//
// This yields one span per API call, with the whole growing messages array as
// input and that call's response as output. The plugin is a hook observer, not
// a proxy, so it reconstructs these per-call spans from the transcript after
// the fact.
//
// Message shape: the transcript writes one line per content block, and every
// content-block line of one assistant message repeats the same message.id (and
// usage). Consecutive same-role lines are coalesced into one logical message;
// an assistant message.id boundary marks a new LLM call.
//
// Fails open: returns nil / partial on any error (like ReadTurnUsage).

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/filelog"
)

// LLMCall is one reconstructed LLM API call within a turn (one assistant
// message.id). InputMessages is the full ordered conversation history that
// preceded this call, and Output is this call's assistant message — both
// pre-rendered as JSON message objects ({"role","parts":[...]}) so the neutral
// layers never need transcript-format knowledge. Usage is this call's own token
// counts (deduped by message.id).
type LLMCall struct {
	MessageID           string
	Model               string
	StopReason          string
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	// InputMessages holds the rendered JSON message objects preceding this call,
	// oldest first. It is a snapshot (three-index sub-slice) of the running
	// history: safe to share read-only because entries are immutable strings and
	// the history is only ever appended to.
	InputMessages []string
	// Output is the rendered JSON message object for this call's assistant
	// response (all its content blocks: thinking / text / tool_use).
	Output string
	// OutputText is the plain concatenated text of this call's response (its
	// "text" parts only, excluding thinking and tool_use). It backs the
	// gen_ai.llm.output scalar, alongside the structured Output above. Empty for
	// a call that produced only a tool_use / thinking and no visible text.
	OutputText string
	// InputText is the plain text of the newest user-role message preceding this
	// call — the input delta that triggered it: the user's prompt for the first
	// call of a turn, or the tool result(s) for a tool-continuation call. It
	// backs the gen_ai.llm.input.user scalar and so differs call-to-call,
	// matching native per-request spans. The full prior history stays in
	// InputMessages.
	InputText string
	// StartNano/EndNano bound the call: EndNano is this assistant message's
	// timestamp; StartNano is the previous call's end (or the turn start for the
	// first call), so consecutive call spans abut.
	StartNano string
	EndNano   string
	// Synthesized marks a call that was not read from the transcript but rebuilt
	// from the hook payload's final answer because the transcript write had not
	// landed in time (see SettleTurnCalls). It carries no model or token usage.
	Synthesized bool
}

// turnScan is the full result of one forward pass over the transcript: the
// in-turn calls plus the trailing conversation state after the last line, which
// SettleTurnCalls needs to synthesize a final call that has not been written.
type turnScan struct {
	calls []LLMCall
	// history is every rendered message in file order (the whole session).
	history []string
	// lastUserText is the plain text of the newest user-role message in the
	// file (e.g. the tool result the unwritten final call received).
	lastUserText string
}

// ReadTurnCalls reconstructs the ordered list of LLM calls for the turn
// (assistant messages with timestamp >= startUnixNano), each with the full
// input message history that preceded it. Returns nil on any error or when no
// in-turn assistant message is found (fail open).
//
// The scan is a single forward pass over the whole file: the input history of
// even the first in-turn call includes every earlier message in the session
// (that is what a real API call receives), so unlike the outcome/usage readers
// the prefix cannot be skipped. This is the same full-file read ReadTurnUsage
// already performs at turn end.
func ReadTurnCalls(transcriptPath, startUnixNano string) []LLMCall {
	return scanTurn(transcriptPath, startUnixNano).calls
}

// scanTurn is the single forward pass behind ReadTurnCalls. It returns the zero
// turnScan on any error (fail open).
func scanTurn(transcriptPath, startUnixNano string) turnScan {
	if transcriptPath == "" {
		return turnScan{}
	}

	// A missing/unparseable start would make every assistant message qualify,
	// pulling prior turns' calls into this turn. Callers always supply a real
	// start; an empty one signals abnormal state, so degrade to nothing.
	turnStart, err := strconv.ParseInt(startUnixNano, 10, 64)
	if err != nil || turnStart <= 0 {
		return turnScan{}
	}

	f, err := os.Open(transcriptPath)
	if err != nil {
		if os.IsNotExist(err) {
			filelog.Warn("transcript not found: %s", transcriptPath)
		} else {
			filelog.Warn("transcript calls read failed: %v", err)
		}
		return turnScan{}
	}
	defer func() { _ = f.Close() }()

	var (
		calls   []LLMCall
		history []string // rendered messages in file order (whole session)
		lastEnd = startUnixNano

		// lastUserParts holds the parts of the most recent user-role message, so
		// an assistant call can record its input delta (the newest user message
		// that preceded it) as InputText.
		lastUserParts []callPart

		// Accumulator for the message currently being coalesced across lines.
		curRole  string
		curMsgID string
		curTS    string
		curParts []callPart
		curUsage messageUsage
		curModel string
		curStop  string
		curOpen  bool
	)

	flush := func() {
		if !curOpen {
			return
		}
		rendered := renderMessage(curRole, curParts)
		// An assistant message within the turn is one LLM call. Snapshot the
		// history *before* appending this message: that is the call's input.
		if curRole == roleAssistant && inTurn(curTS, turnStart) {
			calls = append(calls, LLMCall{
				MessageID:           curMsgID,
				Model:               curModel,
				StopReason:          curStop,
				InputTokens:         curUsage.InputTokens,
				OutputTokens:        curUsage.OutputTokens,
				CacheReadTokens:     curUsage.CacheReadInputTokens,
				CacheCreationTokens: curUsage.CacheCreationInputTokens,
				InputMessages:       history[:len(history):len(history)],
				Output:              rendered,
				OutputText:          textOfParts(curParts),
				InputText:           userTextOfParts(lastUserParts),
				StartNano:           lastEnd,
				EndNano:             nanoString(curTS),
			})
			lastEnd = nanoString(curTS)
		}
		// Remember this user message so the next assistant call can record it as
		// its input delta.
		if curRole == roleUser {
			lastUserParts = curParts
		}
		history = append(history, rendered)
		curOpen = false
		curParts = nil
		curUsage = messageUsage{}
		curModel, curStop, curMsgID, curTS, curRole = "", "", "", "", ""
	}

	// processLine folds one parsed transcript line into the current message,
	// flushing at a message boundary. A closure (like flush) so it shares the
	// coalescing state rather than threading it through parameters.
	processLine := func(line string) {
		var e callEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return
		}
		role := entryRole(e.Type)
		if role == "" {
			return // not a conversation message (mode, attachment, ...)
		}

		// A new logical message begins on a role change or an assistant
		// message.id change; consecutive same-role lines (an assistant's blocks,
		// or parallel tool_result lines) coalesce into one message.
		if curOpen && (role != curRole || (role == roleAssistant && e.Message.ID != curMsgID)) {
			flush()
		}
		if !curOpen {
			curOpen = true
			curRole = role
			curMsgID = e.Message.ID
			curTS = e.Timestamp
		}
		curParts = append(curParts, blocksToParts(e.Message.Content)...)
		if role == roleAssistant {
			if e.Message.Model != "" {
				curModel = e.Message.Model
			}
			if e.Message.StopReason != "" {
				curStop = e.Message.StopReason
			}
			// First-wins usage: every block line of a message repeats identical
			// usage, so keep the first non-empty one.
			if !curUsage.hasData() && e.Message.Usage.hasData() {
				curUsage = e.Message.Usage
			}
		}
	}

	scanTranscriptLines(f, "transcript calls", processLine)
	flush()

	filelog.Info("turn calls reconstructed: calls=%d history_msgs=%d", len(calls), len(history))
	return turnScan{
		calls:        calls,
		history:      history,
		lastUserText: userTextOfParts(lastUserParts),
	}
}

// readLine reads the next newline-terminated line from r, without the trailing
// newline, capped at maxLen bytes. A line longer than maxLen is drained to its
// end and returned as ("", true, ...) so the caller can skip just that line and
// keep reading the rest of the input. err is io.EOF (alongside the final,
// possibly unterminated, line) once the input is exhausted.
func readLine(r *bufio.Reader, maxLen int) (string, bool, error) {
	var buf []byte
	tooLong := false
	for {
		frag, err := r.ReadSlice('\n')
		if len(buf)+len(frag) > maxLen {
			tooLong = true // stop accumulating; keep draining to the newline
		}
		if !tooLong {
			buf = append(buf, frag...) // frag aliases r's buffer; append copies it out
		}
		if err == bufio.ErrBufferFull {
			continue // line longer than the reader buffer; more fragments follow
		}
		if tooLong {
			return "", true, err
		}
		// Drop the trailing newline (and a preceding CR) so callers get the line
		// content only; the final unterminated line has neither.
		line := buf
		if n := len(line); n > 0 && line[n-1] == '\n' {
			line = line[:n-1]
			if n := len(line); n > 0 && line[n-1] == '\r' {
				line = line[:n-1]
			}
		}
		return string(line), false, err
	}
}

// scanTranscriptLines reads f line by line through readLine, calling fn for each
// non-blank line. A line longer than maxTranscriptLineBytes is drained and
// skipped rather than aborting the scan, so the reader keeps going past an
// over-long line instead of dropping every line after it (fail open). label
// prefixes the warnings so the source reader is identifiable in the logs.
func scanTranscriptLines(f io.Reader, label string, fn func(line string)) {
	reader := bufio.NewReaderSize(f, 256*1024)
	for {
		raw, tooLong, readErr := readLine(reader, maxTranscriptLineBytes)
		if tooLong {
			filelog.Warn("%s: skipped a line larger than %d bytes", label, maxTranscriptLineBytes)
		} else if line := strings.TrimSpace(raw); line != "" {
			fn(line)
		}
		if readErr != nil {
			if readErr != io.EOF {
				filelog.Warn("%s read error: %v", label, readErr)
			}
			break
		}
	}
}

// maxTranscriptLineBytes bounds a single transcript line the readers will parse.
// Beyond this the line is skipped (fail open) rather than aborting the scan.
const maxTranscriptLineBytes = 16 * 1024 * 1024

// Conversation roles, matching the transcript entry "type".
const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// entryRole maps a transcript entry type to a conversation role, or "" for
// entries that are not conversation messages (mode, permission-mode,
// attachment, file-history-snapshot, last-prompt, ...).
func entryRole(entryType string) string {
	switch entryType {
	case roleUser:
		return roleUser
	case roleAssistant:
		return roleAssistant
	default:
		return ""
	}
}

// inTurn reports whether an entry timestamp is at or after the turn start.
func inTurn(ts string, turnStart int64) bool {
	n := isoToUnixNano(ts)
	return n != 0 && n >= turnStart
}

// nanoString converts an ISO timestamp to an epoch-nanoseconds string, or ""
// when unparseable.
func nanoString(ts string) string {
	n := isoToUnixNano(ts)
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

// textOfParts concatenates the "text" parts of an assistant message, skipping
// thinking and tool_use blocks. It backs the gen_ai.llm.output scalar (the
// visible model response), distinct from Output which carries every block as
// structured JSON. Multiple text parts are joined with a blank line; the result
// is "" for a call that emitted only thinking / tool_use.
func textOfParts(parts []callPart) string {
	var texts []string
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

// userTextOfParts renders a user-role message to plain text for the
// gen_ai.llm.input.user scalar: text parts verbatim, and tool_result parts as
// their result content (the input a tool-continuation call actually received).
// Multiple parts (e.g. parallel tool results) are joined with a blank line; the
// result is "" for a message with no readable content.
func userTextOfParts(parts []callPart) string {
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		case "tool_result":
			if s := contentText(p.Content); s != "" {
				texts = append(texts, s)
			}
		}
	}
	return strings.Join(texts, "\n\n")
}

// contentText extracts readable text from a tool_result's content, which the
// transcript may encode as a JSON string, an array of content blocks, or some
// other shape. A string is unquoted; an array yields its text blocks joined;
// anything else falls back to the raw JSON so nothing is silently dropped.
func contentText(raw json.RawMessage) string {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return ""
	}
	if t[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	if t[0] == '[' {
		var blocks []callBlock
		if json.Unmarshal(raw, &blocks) == nil {
			var bt []string
			for _, b := range blocks {
				if b.Type == "text" && b.Text != "" {
					bt = append(bt, b.Text)
				}
			}
			if len(bt) > 0 {
				return strings.Join(bt, "\n\n")
			}
		}
	}
	return t
}

// --- Rendering (transcript block shape -> {role, parts:[...]} JSON) ---

// renderMessage serializes a coalesced message to a compact JSON object
// {"role":...,"parts":[...]}. This is the message shape the
// gen_ai.input.messages / gen_ai.output.messages attributes carry.
func renderMessage(role string, parts []callPart) string {
	if parts == nil {
		parts = []callPart{}
	}
	b, err := json.Marshal(renderedMessage{Role: role, Parts: parts})
	if err != nil {
		// Marshal of these plain structs effectively never fails; degrade to a
		// minimal valid object rather than emitting invalid JSON.
		return `{"role":"` + role + `","parts":[]}`
	}
	return string(b)
}

// blocksToParts decodes a transcript entry's message.content (a JSON string for
// a plain user prompt, or an array of content blocks) into rendered parts.
func blocksToParts(content json.RawMessage) []callPart {
	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	// A plain string content is a single text part.
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(content, &s) == nil {
			return []callPart{{Type: "text", Text: s}}
		}
		return nil
	}
	var blocks []callBlock
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	parts := make([]callPart, 0, len(blocks))
	for _, blk := range blocks {
		parts = append(parts, blk.toPart())
	}
	return parts
}

// renderedMessage / callPart are the output shape; callBlock is the transcript
// input shape. They are kept distinct so the emitted schema does not leak
// transcript-only fields.
type renderedMessage struct {
	Role  string     `json:"role"`
	Parts []callPart `json:"parts"`
}

type callPart struct {
	Type string `json:"type"`
	// text / thinking
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type callBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// toPart maps one transcript content block to its rendered part, normalizing
// the known block types and preserving the raw type for anything unknown.
func (b callBlock) toPart() callPart {
	switch b.Type {
	case "text":
		return callPart{Type: "text", Text: b.Text}
	case "thinking":
		return callPart{Type: "thinking", Text: b.Thinking}
	case "tool_use":
		return callPart{Type: "tool_use", ID: b.ID, Name: b.Name, Input: b.Input}
	case "tool_result":
		return callPart{Type: "tool_result", ToolUseID: b.ToolUseID, Content: b.Content, IsError: b.IsError}
	default:
		return callPart{Type: b.Type}
	}
}

// callEntry is the transcript line shape the calls reader needs. It is kept
// separate from the usage/outcome entry shapes so a string-valued content never
// breaks their array decoding (and vice versa).
type callEntry struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		ID         string          `json:"id"`
		Model      string          `json:"model"`
		StopReason string          `json:"stop_reason"`
		Usage      messageUsage    `json:"usage"`
		Content    json.RawMessage `json:"content"`
	} `json:"message"`
}
