// Package transcript parses the Claude Code session transcript JSONL
// to extract per-turn token usage, model, and stop reason.
//
// The transcript is written asynchronously by Claude Code at the path
// provided in the hook payload's transcript_path field. Each line is a
// JSON object representing one content-block entry. Assistant entries
// carry message.usage, message.model, and message.stop_reason.
//
// Critical: the transcript repeats the same message.usage on every
// content-block entry of one assistant message (thinking, text, each
// tool_use). Token totals MUST dedupe by message.id; naive summing
// overcounts by the number of content blocks. Dedup by message.id is
// covered by the package tests.
//
// Fails open: returns zero/empty on any error. The transcript may lag
// (written asynchronously), so the final message's usage can be missing.
// Response text is sourced from the Stop payload, not here.
package transcript

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/filelog"
)

// TurnUsage holds the aggregated token usage, model, and stop reason
// for a single turn, deduped by message.id.
type TurnUsage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	Model               string
	StopReason          string
}

// ReadTurnUsage parses the transcript JSONL at the given path and
// returns aggregated usage for the current turn (entries with
// timestamp >= startUnixNano).
//
// Returns a zero-value TurnUsage on any error (fail open).
func ReadTurnUsage(transcriptPath string, startUnixNano string) TurnUsage {
	if transcriptPath == "" {
		return TurnUsage{}
	}

	turnStart, err := strconv.ParseInt(startUnixNano, 10, 64)
	if err != nil {
		turnStart = 0
	}

	f, err := os.Open(transcriptPath)
	if err != nil {
		if os.IsNotExist(err) {
			filelog.Warn("transcript not found: %s", transcriptPath)
		} else {
			filelog.Warn("transcript read failed: %v", err)
		}
		return TurnUsage{}
	}
	defer func() { _ = f.Close() }()

	// usageByMsg deduplicates usage by message.id. Only the first
	// occurrence of each message.id is kept — subsequent content-block
	// entries for the same message carry identical usage.
	usageByMsg := make(map[string]messageUsage)
	var lastModel, lastStopReason string

	// processLine folds one transcript line into the deduped usage totals. A
	// closure so it shares the accumulator state rather than threading it
	// through parameters.
	processLine := func(line string) {
		var entry transcriptEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return
		}

		// Only assistant entries carry usage.
		if entry.Type != "assistant" {
			return
		}

		// Filter to current turn by timestamp.
		entryNano := isoToUnixNano(entry.Timestamp)
		if entryNano < turnStart {
			return
		}

		msg := entry.Message
		if msg.ID != "" && msg.Usage.hasData() {
			if prev, seen := usageByMsg[msg.ID]; !seen {
				usageByMsg[msg.ID] = msg.Usage
			} else if prev != msg.Usage {
				// Dedup assumes every content-block entry of a message
				// repeats identical usage. If a later entry disagrees,
				// the transcript format may have changed to report
				// incremental usage — warn so first-wins totals don't go
				// silently wrong. Log ids/counts only, never content.
				filelog.Warn("transcript: usage disagreement for message.id=%s "+
					"(kept in=%d out=%d; saw in=%d out=%d)",
					msg.ID, prev.InputTokens, prev.OutputTokens,
					msg.Usage.InputTokens, msg.Usage.OutputTokens)
			}
		}

		if msg.Model != "" {
			lastModel = msg.Model
		}
		if msg.StopReason != "" {
			lastStopReason = msg.StopReason
		}
	}

	scanTranscriptLines(f, "transcript usage", processLine)

	if len(usageByMsg) == 0 {
		filelog.Warn("no assistant usage found for turn (transcript may lag)")
	}

	var result TurnUsage
	for _, u := range usageByMsg {
		result.InputTokens += u.InputTokens
		result.OutputTokens += u.OutputTokens
		result.CacheReadTokens += u.CacheReadInputTokens
		result.CacheCreationTokens += u.CacheCreationInputTokens
	}
	result.Model = lastModel
	result.StopReason = lastStopReason

	filelog.Info("turn usage: msgs=%d input=%d output=%d cache_read=%d cache_creation=%d model=%s stop_reason=%s",
		len(usageByMsg),
		result.InputTokens, result.OutputTokens,
		result.CacheReadTokens, result.CacheCreationTokens,
		result.Model, result.StopReason)

	return result
}

// FirstEntryNano returns the earliest entry timestamp in the transcript as an
// epoch-nanoseconds string, or "" on any error or when no entry has a parseable
// timestamp (fail open). Transcript files are written in chronological order, so
// the first timestamped line is the earliest; the scan stops there.
//
// It is used to bound a sub-agent's reconstruction: a sub-agent has its own
// dedicated transcript but no turn-start signal, so its start time is taken from
// its transcript's first entry (see event.SubagentEnd.ReadStart).
func FirstEntryNano(transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		if os.IsNotExist(err) {
			filelog.Warn("transcript not found: %s", transcriptPath)
		} else {
			filelog.Warn("transcript first-entry read failed: %v", err)
		}
		return ""
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry transcriptEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if n := isoToUnixNano(entry.Timestamp); n != 0 {
			return strconv.FormatInt(n, 10)
		}
	}
	return ""
}

// ReadVersion returns the Claude Code version that wrote the transcript, read
// from the top-level "version" field that Claude Code stamps on each entry. The
// hook payload does not carry the version, so the transcript is the only source.
//
// It returns the first non-empty version found and stops scanning (the version
// is effectively constant for a session). Returns "" on any error or when no
// entry carries a version (fail open), e.g. an empty path or a transcript that
// only holds a leading summary entry.
func ReadVersion(transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		if os.IsNotExist(err) {
			filelog.Warn("transcript not found: %s", transcriptPath)
		} else {
			filelog.Warn("transcript version read failed: %v", err)
		}
		return ""
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry transcriptEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.Version != "" {
			return entry.Version
		}
	}
	return ""
}

// --- Internal types matching the transcript JSONL shape ---

type transcriptEntry struct {
	Type      string            `json:"type"`
	Timestamp string            `json:"timestamp"`
	Version   string            `json:"version"`
	Message   transcriptMessage `json:"message"`
}

type transcriptMessage struct {
	ID         string       `json:"id"`
	Model      string       `json:"model"`
	StopReason string       `json:"stop_reason"`
	Usage      messageUsage `json:"usage"`
}

type messageUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// hasData returns true if any token count is non-zero.
func (u messageUsage) hasData() bool {
	return u.InputTokens > 0 || u.OutputTokens > 0 ||
		u.CacheReadInputTokens > 0 || u.CacheCreationInputTokens > 0
}

// isoToUnixNano parses an ISO8601 timestamp (e.g. "2026-07-08T03:30:56.642Z")
// to epoch nanoseconds. Returns 0 on any parse failure so callers treat it
// as "before the turn start".
func isoToUnixNano(ts string) int64 {
	if ts == "" {
		return 0
	}
	// Go's time.RFC3339Nano handles "2026-07-08T03:30:56.642Z" directly.
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		// Try without fractional seconds.
		t, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return 0
		}
	}
	return t.UnixNano()
}
