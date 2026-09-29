package otlp

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/config"
)

// Resource attribute keys. service.name and application.id are always emitted;
// most of the rest mirror Claude Code's native OpenTelemetry
// resource so plugin traces carry a comparable identity/environment signal.
// user.name and user.identity.source extend it with the git-sourced developer
// identity (see internal/identity): user.email prefers the git email over the
// Claude OAuth email, and user.identity.source records which was used. All are
// resolved from process-local sources and omitted when their source is
// unavailable.
const (
	resAttrServiceName    = "service.name"
	resAttrServiceVersion = "service.version"
	resAttrAppID          = "application.id"
	resAttrUserID         = "user.id"
	resAttrOrgID          = "organization.id"
	resAttrAccountUUID    = "user.account_uuid"
	resAttrUserName       = "user.name"
	resAttrUserEmail      = "user.email"
	resAttrIdentitySource = "user.identity.source"
	resAttrAppVersion     = "app.version"
	resAttrEntrypoint     = "app.entrypoint"
	resAttrTerminal       = "terminal.type"
	resAttrCwd            = "cwd"
)

// Instrumentation scope identifying the tracing source. This is the
// plugin's own scope, deliberately distinct from Anthropic's native
// claude_code tracing scope: the plugin is a separate telemetry producer,
// and a plugin-specific scope keeps the two producers distinguishable
// downstream.
const (
	ScopeName    = "ai.fiddler.coding_agent_plugin"
	ScopeVersion = "1.0.0"
)

// OTLP span status codes.
const (
	// SpanStatusUnset is the OTLP status code for UNSET — neither success nor
	// failure. Used for outcomes that are honestly unknown (e.g. a tool call
	// requested but never resolved), so they don't count as errors.
	SpanStatusUnset = 0
	// SpanStatusOK is the OTLP status code for OK.
	SpanStatusOK = 1
	// SpanStatusError is the OTLP status code for ERROR.
	SpanStatusError = 2
)

// SpanKindInternal is OTLP SPAN_KIND_INTERNAL.
const SpanKindInternal = 1

// KeyValue represents an OTLP attribute key-value pair.
type KeyValue struct {
	Key   string   `json:"key"`
	Value AnyValue `json:"value"`
}

// AnyValue represents an OTLP AnyValue.
type AnyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

// StringAttr creates a string-typed KeyValue.
func StringAttr(key, value string) KeyValue {
	return KeyValue{Key: key, Value: AnyValue{StringValue: &value}}
}

// IntAttr creates an int-typed KeyValue. OTLP encodes ints as strings
// in the JSON representation.
func IntAttr(key string, value int64) KeyValue {
	s := fmt.Sprintf("%d", value)
	return KeyValue{Key: key, Value: AnyValue{IntValue: &s}}
}

// BoolAttr creates a bool-typed KeyValue.
func BoolAttr(key string, value bool) KeyValue {
	return KeyValue{Key: key, Value: AnyValue{BoolValue: &value}}
}

// Span represents a single OTLP span in JSON form. It is built by hand
// rather than via the OpenTelemetry Go SDK — see the package doc in
// ids.go for why (spans are produced by separate hook processes with
// pre-chosen, file-shared IDs; child spans ship before their parent
// exists).
type Span struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	ParentSpanID      string     `json:"parentSpanId,omitempty"`
	Name              string     `json:"name"`
	Kind              int        `json:"kind"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []KeyValue `json:"attributes"`
	Status            SpanStatus `json:"status"`
}

// SpanStatus represents an OTLP span status. Message carries the error
// description and is set only for ERROR status.
type SpanStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

// NewSpan creates a new Span with the given parameters.
func NewSpan(name, traceID, spanID, parentSpanID, startNano, endNano string, attrs []KeyValue) Span {
	return Span{
		TraceID:           traceID,
		SpanID:            spanID,
		ParentSpanID:      parentSpanID,
		Name:              name,
		Kind:              SpanKindInternal,
		StartTimeUnixNano: startNano,
		EndTimeUnixNano:   endNano,
		Attributes:        attrs,
		Status:            SpanStatus{Code: SpanStatusOK},
	}
}

// ScopeSpans groups spans under an instrumentation scope.
type ScopeSpans struct {
	Scope InstrumentationScope `json:"scope"`
	Spans []Span               `json:"spans"`
}

// InstrumentationScope identifies the instrumentation library.
type InstrumentationScope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ResourceSpans is the top-level OTLP trace export structure.
type ResourceSpans struct {
	Resource   Resource     `json:"resource"`
	ScopeSpans []ScopeSpans `json:"scopeSpans"`
}

// Resource holds OTLP resource attributes.
type Resource struct {
	Attributes []KeyValue `json:"attributes"`
}

// ExportPayload is the envelope POSTed to /v1/traces.
type ExportPayload struct {
	ResourceSpans []ResourceSpans `json:"resourceSpans"`
}

// BuildPayload assembles a complete OTLP/JSON export payload from spans. The
// resource attributes carry service.name and application.id (always present)
// plus the locally-resolved identity/workspace attributes
// from cfg, which mirror Claude Code's native OpenTelemetry resource. Empty
// attributes are omitted, and OmitUserInfo drops the personally-identifying
// ones (see resourceAttrs).
func BuildPayload(cfg *config.Config, spans []Span) ExportPayload {
	return ExportPayload{
		ResourceSpans: []ResourceSpans{
			{
				Resource: Resource{
					Attributes: resourceAttrs(cfg),
				},
				ScopeSpans: []ScopeSpans{
					{
						Scope: InstrumentationScope{
							Name:    ScopeName,
							Version: ScopeVersion,
						},
						Spans: spans,
					},
				},
			},
		},
	}
}

// resourceAttrs builds the OTLP resource attributes from cfg. service.name and
// application.id are always present; the remaining attributes (service.version,
// app.version, and the identity/workspace attributes) are appended only when
// non-empty. When cfg.OmitUserInfo is set, the personally-identifying
// attributes (user.name, user.email, user.account_uuid, and the
// user.identity.source describing them) are dropped, while the already-hashed
// user.id and org-level organization.id are retained.
func resourceAttrs(cfg *config.Config) []KeyValue {
	attrs := []KeyValue{
		StringAttr(resAttrServiceName, cfg.ServiceName),
		StringAttr(resAttrAppID, cfg.AppID),
	}
	appendAttr := func(key, val string) {
		if val != "" {
			attrs = append(attrs, StringAttr(key, val))
		}
	}

	appendAttr(resAttrServiceVersion, cfg.PluginVersion)
	appendAttr(resAttrUserID, cfg.UserID)
	appendAttr(resAttrOrgID, cfg.OrgID)
	if !cfg.OmitUserInfo {
		appendAttr(resAttrAccountUUID, cfg.AccountUUID)
		appendAttr(resAttrUserName, cfg.UserName)
		appendAttr(resAttrUserEmail, cfg.UserEmail)
		appendAttr(resAttrIdentitySource, cfg.IdentitySource)
	}
	appendAttr(resAttrAppVersion, cfg.AppVersion)
	appendAttr(resAttrEntrypoint, cfg.AppEntrypoint)
	appendAttr(resAttrTerminal, cfg.TerminalType)
	appendAttr(resAttrCwd, cfg.Cwd)

	return attrs
}

// MarshalPayload serializes the export payload to JSON bytes.
func MarshalPayload(p ExportPayload) ([]byte, error) {
	return json.Marshal(p)
}

// Truncate truncates a string to at most maxLen bytes, backing off to a
// rune boundary so a multi-byte UTF-8 sequence is never split.
func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	cut := maxLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// MaxAttrLen is the default max length (bytes) for content attributes. It
// matches Claude Code native OTel's 60 KB per-content-attribute cap (chosen for
// backends that limit attribute values at 64 KB); note CC counts UTF-16 code
// units while this counts bytes, so they are close in intent, not identical.
const MaxAttrLen = 64_000

// MaxMessagesAttrLen is the max length (bytes) for the message-array attributes
// (gen_ai.input.messages / gen_ai.output.messages). These carry the whole
// per-call message history in one attribute, so they get a larger per-attribute
// budget than ordinary content (MaxAttrLen). Oversized histories are keep-newest
// truncated (see messageArrayJSON), not dropped.
//
// This is a per-attribute cap only; it does not bound the total turn-end batch,
// which carries one such attribute per LLM call and can reach several
// MB. That payload is bounded by time, not size: the turn-end export runs under
// turnEndExportTimeout (see pipeline.go) rather than being size-capped here.
const MaxMessagesAttrLen = 1_000_000
