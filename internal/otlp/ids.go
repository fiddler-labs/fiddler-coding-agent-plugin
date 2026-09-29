// Package otlp provides OTLP/JSON span construction, ID generation, and
// HTTP export for the Fiddler coding-agent plugin.
//
// The OTLP payload is hand-built as JSON rather than using the
// OpenTelemetry Go SDK. Spans for one trace are produced by separate,
// short-lived hook processes: a child span (e.g. a tool call) is exported
// before its parent (the turn's root span) exists, using trace and span
// IDs chosen up front and shared across processes via a context file. The
// SDK's in-process, auto-generated ID and context-propagation model can't
// express that; hand-building the payload lets us set traceId, spanId, and
// parentSpanId directly.
package otlp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// NewTraceID generates a random 16-byte (32 hex char) trace ID.
func NewTraceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// NewSpanID generates a random 8-byte (16 hex char) span ID.
func NewSpanID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// SpanIDFrom deterministically derives a span ID from a trace ID and a
// caller-chosen key: the first 8 bytes of sha256(traceID || 0x00 || key),
// hex-encoded (16 chars) — the same shape as NewSpanID.
//
// This lets separate, short-lived hook processes agree on a span's ID with no
// shared map. Every process in a turn holds the same trace_id (via turnctx), so
// SpanIDFrom(traceID, key) yields the same ID everywhere for the same key. A
// child span produced by one process can then be parented to another span whose
// ID is derived the same way, even though the parent is produced by a different
// process and may be exported after the child — OTLP backends accept
// child-before-parent (see the package doc). The 0x00 separator keeps traceID
// and key unambiguous when concatenated.
//
// For any real (non-empty) trace ID the result is a valid, non-zero span ID: a
// sha256 prefix colliding with the all-zero span ID is infeasible.
func SpanIDFrom(traceID, key string) string {
	sum := sha256.Sum256([]byte(traceID + "\x00" + key))
	return hex.EncodeToString(sum[:8])
}
