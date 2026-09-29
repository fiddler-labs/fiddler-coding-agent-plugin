package transcript

// This file classifies each tool call in a turn by looking up its outcome in
// the session transcript, keyed by tool_use_id. It is the resolution oracle for
// the pipeline's leftover-pending sweep: a pending record (written at
// PreToolUse) that never resolved via a live hook is classified here.
//
// The transcript is read from the end and the scan stops at the turn start, so
// the parse cost is bounded to the current turn's entries rather than the whole
// (potentially multi-MB) single-file session transcript. This matters most at
// SessionEnd, which runs under a tight hook budget.

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/filelog"
)

// OutcomeKind is how a tool call resolved, as read from the transcript.
type OutcomeKind int

const (
	// OutcomeResolved means the tool produced an id-keyed result (success or an
	// execution error). Such calls are emitted live at PostToolUse(Failure), so
	// the pipeline skips them during reconstruction.
	OutcomeResolved OutcomeKind = iota
	// OutcomeDeniedUser is a manual permission-dialog denial
	// (toolDenialKind="user-rejected").
	OutcomeDeniedUser
	// OutcomeDeniedRule is a deny-rule match (toolDenialKind="permission-rule").
	OutcomeDeniedRule
)

// ToolOutcome carries a tool call's transcript-derived outcome plus the
// tool name and input recovered from the assistant tool_use block. The name and
// input are a fallback for reconstruction when the PreToolUse pending record is
// missing (the resolution hook was dropped); when the record exists it is
// authoritative.
type ToolOutcome struct {
	Kind OutcomeKind
	Name string
	// ResultNano is the epoch-nanoseconds timestamp of the tool_result entry
	// (empty when unparseable). Used to estimate the blocked-on-user wait for a
	// reconstructed denial. Set only on entries that produced a result.
	ResultNano string
	Input      string
}

// ReadTurnOutcomes classifies every tool call in the turn (entries with
// timestamp >= startUnixNano) by tool_use_id. Only ids that produced an
// id-keyed tool_result appear in the map; an id absent from the result is a
// call that never resolved (the pipeline treats it as unresolved).
//
// Fails open: returns an empty map on any error.
func ReadTurnOutcomes(transcriptPath, startUnixNano string) map[string]ToolOutcome {
	out := map[string]ToolOutcome{}
	if transcriptPath == "" {
		return out
	}

	// A missing or unparseable start is treated as "no turn boundary known" and
	// returns an empty map rather than defaulting turnStart to 0, which would
	// disable the reverse-scan stop condition and classify the entire session
	// transcript (reconstructing denials from prior turns). Callers always have
	// a real start; an empty one signals abnormal state, so degrade to nothing.
	turnStart, err := strconv.ParseInt(startUnixNano, 10, 64)
	if err != nil || turnStart <= 0 {
		return out
	}

	f, err := os.Open(transcriptPath)
	if err != nil {
		if !os.IsNotExist(err) {
			filelog.Warn("transcript outcomes read failed: %v", err)
		}
		return out
	}
	defer func() { _ = f.Close() }()

	// acc collects name/input (from tool_use blocks) and the classified kind
	// (from tool_result blocks) per id; resultSeen marks ids that actually
	// produced a result, so tool_use-only ids (requested, never resulted) are
	// excluded and fall through to "unresolved" downstream.
	acc := map[string]*ToolOutcome{}
	resultSeen := map[string]bool{}

	err = forEachLineReverse(f, func(line []byte) bool {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			return false
		}
		var e outcomeEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return false
		}
		// Entries are chronological; once we pass an entry older than the turn
		// start, everything earlier is older too, so stop. This early stop
		// assumes monotonically non-decreasing timestamps in append order: a
		// transcript that wrote a slightly out-of-order timestamp near the turn
		// boundary could cause in-turn entries earlier in the file to be missed.
		// Real append-ordered session logs satisfy this; the trade-off is bounded
		// parse cost (only the current turn's suffix is read), which matters most
		// under the tight SessionEnd budget.
		if e.Timestamp != "" {
			if ts := isoToUnixNano(e.Timestamp); ts != 0 && ts < turnStart {
				return true
			}
		}
		// message.content is an array of blocks for tool_use / tool_result
		// entries, but a plain string for text entries; decode tolerantly.
		var blocks []contentBlock
		if len(e.Message.Content) == 0 || json.Unmarshal(e.Message.Content, &blocks) != nil {
			return false
		}
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				if b.ID == "" {
					continue
				}
				o := acc[b.ID]
				if o == nil {
					o = &ToolOutcome{}
					acc[b.ID] = o
				}
				o.Name = b.Name
				o.Input = strings.TrimSpace(string(b.Input))
			case "tool_result":
				if b.ToolUseID == "" {
					continue
				}
				o := acc[b.ToolUseID]
				if o == nil {
					o = &ToolOutcome{}
					acc[b.ToolUseID] = o
				}
				resultSeen[b.ToolUseID] = true
				if ts := isoToUnixNano(e.Timestamp); ts != 0 {
					o.ResultNano = strconv.FormatInt(ts, 10)
				}
				switch e.ToolDenialKind {
				case "user-rejected":
					o.Kind = OutcomeDeniedUser
				case "permission-rule":
					o.Kind = OutcomeDeniedRule
				default:
					o.Kind = OutcomeResolved
				}
			}
		}
		return false
	})
	if err != nil {
		filelog.Warn("transcript outcomes scan error: %v", err)
	}

	for id, o := range acc {
		if resultSeen[id] {
			out[id] = *o
		}
	}
	return out
}

// --- Internal shapes for the classifier (kept separate from the usage
// structs so a string-valued message.content never fails usage parsing). ---

type outcomeEntry struct {
	Type           string `json:"type"`
	Timestamp      string `json:"timestamp"`
	ToolDenialKind string `json:"toolDenialKind"`
	Message        struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	// tool_use fields:
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result fields:
	ToolUseID string `json:"tool_use_id"`
}

// forEachLineReverse invokes fn for each newline-delimited line of f, from the
// last line to the first, stopping early when fn returns true. Lines are passed
// without the trailing newline. Reading from the end lets callers bound work to
// a recent suffix of a large file.
func forEachLineReverse(f *os.File, fn func(line []byte) (stop bool)) error {
	const chunkSize = 64 * 1024
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	pos := stat.Size()
	var carry []byte // partial leading line, belongs after the next (earlier) chunk
	chunk := make([]byte, chunkSize)

	for pos > 0 {
		readN := int64(chunkSize)
		if pos < readN {
			readN = pos
		}
		pos -= readN
		if _, err := f.ReadAt(chunk[:readN], pos); err != nil && err != io.EOF {
			return err
		}
		// File order within data: this chunk's bytes, then the carried partial.
		data := make([]byte, 0, int(readN)+len(carry))
		data = append(data, chunk[:readN]...)
		data = append(data, carry...)

		lines := bytes.Split(data, []byte{'\n'})
		// lines[0] is a partial line (its start is in an earlier chunk) unless
		// we are at the file's beginning; process the complete lines newest
		// (last) to oldest, and carry the partial to the next iteration.
		for i := len(lines) - 1; i >= 1; i-- {
			if fn(lines[i]) {
				return nil
			}
		}
		carry = lines[0]
	}
	if len(carry) > 0 {
		fn(carry)
	}
	return nil
}
