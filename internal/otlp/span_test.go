package otlp

import (
	"testing"
	"unicode/utf8"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Given strings of varying lengths and byte widths, when truncated to a max, the
// result stays within the byte limit and never splits a multi-byte UTF-8 rune.
func TestTruncate(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		maxLen int
		want   string
	}{
		{"shorter than max", "hello", 10, "hello"},
		{"exactly max", "hello", 5, "hello"},
		{"ascii cut", "hello", 3, "hel"},
		{"empty", "", 5, ""},
		{"multibyte not split (é is 2 bytes)", "héllo", 2, "h"},
		{"multibyte kept when it fits", "héllo", 3, "hé"},
		{"emoji not split (😀 is 4 bytes)", "a😀b", 2, "a"},
		{"emoji kept when it fits", "a😀b", 5, "a😀"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Truncate(tc.in, tc.maxLen)
			assert.Equal(t, tc.want, got)
			assert.True(t, utf8.ValidString(got), "result must be valid UTF-8")
			assert.LessOrEqual(t, len(got), tc.maxLen)
		})
	}
}

// resAttrLookup returns a getter over a payload's resource attributes.
func resAttrLookup(t *testing.T, p ExportPayload) func(string) (string, bool) {
	t.Helper()
	require.Len(t, p.ResourceSpans, 1)
	attrs := p.ResourceSpans[0].Resource.Attributes
	return func(key string) (string, bool) {
		for _, a := range attrs {
			if a.Key == key && a.Value.StringValue != nil {
				return *a.Value.StringValue, true
			}
		}
		return "", false
	}
}

// Given a minimal config, when the payload is built, the required resource
// attributes (service.name, application.id), the plugin's own instrumentation
// scope, and the spans are all present.
func TestBuildPayload(t *testing.T) {
	span := NewSpan("n", "trace1", "span1", "", "1", "2", nil)
	cfg := &config.Config{AppID: "app-123", ServiceName: "fiddler-coding-agent-plugin"}
	p := BuildPayload(cfg, []Span{span})

	require.Len(t, p.ResourceSpans, 1)
	rs := p.ResourceSpans[0]
	resAttr := resAttrLookup(t, p)

	name, ok := resAttr("service.name")
	require.True(t, ok, "service.name resource attribute present")
	assert.Equal(t, "fiddler-coding-agent-plugin", name,
		"service.name names the plugin (producer), not the observed runtime")
	appID, ok := resAttr("application.id")
	require.True(t, ok)
	assert.Equal(t, "app-123", appID)

	require.Len(t, rs.ScopeSpans, 1)
	assert.Equal(t, ScopeName, rs.ScopeSpans[0].Scope.Name)
	assert.Equal(t, "ai.fiddler.coding_agent_plugin", rs.ScopeSpans[0].Scope.Name,
		"plugin uses its own instrumentation scope, not Anthropic's native one")
	require.Len(t, rs.ScopeSpans[0].Spans, 1)
	assert.Equal(t, "span1", rs.ScopeSpans[0].Spans[0].SpanID)
}

// Given a config carrying the plugin version, when the payload is built, it is
// stamped as the service.version resource attribute; when unset, the attribute
// is omitted rather than emitted empty.
func TestBuildPayload_ServiceVersion(t *testing.T) {
	withVersion := &config.Config{AppID: "app-123", ServiceName: "svc", PluginVersion: "0.4.0"}
	resAttr := resAttrLookup(t, BuildPayload(withVersion, nil))
	got, ok := resAttr("service.version")
	require.True(t, ok, "service.version present when plugin version is set")
	assert.Equal(t, "0.4.0", got)

	noVersion := &config.Config{AppID: "app-123", ServiceName: "svc"}
	resAttr = resAttrLookup(t, BuildPayload(noVersion, nil))
	_, ok = resAttr("service.version")
	assert.False(t, ok, "service.version omitted when plugin version is empty")
}

// Given a config carrying resolved identity/workspace attributes, when the
// payload is built, each is stamped as a resource attribute with its value.
func TestBuildPayload_IdentityResourceAttrs(t *testing.T) {
	cfg := &config.Config{
		AppID:          "app-123",
		ServiceName:    "svc",
		UserID:         "hasheduser",
		OrgID:          "org-uuid",
		AccountUUID:    "acct-uuid",
		UserName:       "Jane Dev",
		UserEmail:      "dev@example.com",
		IdentitySource: "git",
		AppVersion:     "2.1.0",
		AppEntrypoint:  "cli",
		TerminalType:   "kitty",
		Cwd:            "/work/repo",
	}
	resAttr := resAttrLookup(t, BuildPayload(cfg, nil))

	for key, want := range map[string]string{
		"user.id":              "hasheduser",
		"organization.id":      "org-uuid",
		"user.account_uuid":    "acct-uuid",
		"user.name":            "Jane Dev",
		"user.email":           "dev@example.com",
		"user.identity.source": "git",
		"app.version":          "2.1.0",
		"app.entrypoint":       "cli",
		"terminal.type":        "kitty",
		"cwd":                  "/work/repo",
	} {
		got, ok := resAttr(key)
		assert.True(t, ok, "%s present", key)
		assert.Equal(t, want, got, key)
	}
}

// Given a config with no identity/workspace attributes set, when the payload is
// built, those resource attributes are omitted rather than emitted empty.
func TestBuildPayload_OmitsEmptyIdentityAttrs(t *testing.T) {
	cfg := &config.Config{AppID: "app-123", ServiceName: "svc"}
	resAttr := resAttrLookup(t, BuildPayload(cfg, nil))

	for _, key := range []string{
		"user.id", "organization.id", "user.account_uuid",
		"user.name", "user.email", "user.identity.source",
		"app.version", "app.entrypoint", "terminal.type", "cwd",
	} {
		_, ok := resAttr(key)
		assert.False(t, ok, "%s omitted when unset", key)
	}
}

// Given OmitUserInfo is set, when the payload is built, the personally-
// identifying attributes (user.email, user.account_uuid) are dropped while the
// already-hashed user.id and the org-level organization.id are retained.
func TestBuildPayload_OmitUserInfoDropsPII(t *testing.T) {
	cfg := &config.Config{
		AppID:          "app-123",
		ServiceName:    "svc",
		OmitUserInfo:   true,
		UserID:         "hasheduser",
		OrgID:          "org-uuid",
		AccountUUID:    "acct-uuid",
		UserName:       "Jane Dev",
		UserEmail:      "dev@example.com",
		IdentitySource: "git",
	}
	resAttr := resAttrLookup(t, BuildPayload(cfg, nil))

	_, hasName := resAttr("user.name")
	assert.False(t, hasName, "user.name dropped under OmitUserInfo")
	_, hasEmail := resAttr("user.email")
	assert.False(t, hasEmail, "user.email dropped under OmitUserInfo")
	_, hasSource := resAttr("user.identity.source")
	assert.False(t, hasSource, "user.identity.source dropped under OmitUserInfo")
	_, hasAcct := resAttr("user.account_uuid")
	assert.False(t, hasAcct, "user.account_uuid dropped under OmitUserInfo")

	// Hashed and org-level identifiers are retained.
	uid, ok := resAttr("user.id")
	assert.True(t, ok)
	assert.Equal(t, "hasheduser", uid, "hashed user.id retained under OmitUserInfo")
	org, ok := resAttr("organization.id")
	assert.True(t, ok)
	assert.Equal(t, "org-uuid", org, "organization.id retained under OmitUserInfo")
}

// Given repeated calls, when a trace id is generated, it is 32 hex chars (16
// bytes) and differs from the next one.
func TestNewTraceID(t *testing.T) {
	id := NewTraceID()
	require.Len(t, id, 32, "trace id is 16 bytes = 32 hex chars")
	assertHex(t, id)
	assert.NotEqual(t, id, NewTraceID(), "trace ids should differ")
}

// Given repeated calls, when a span id is generated, it is 16 hex chars (8
// bytes) and differs from the next one.
func TestNewSpanID(t *testing.T) {
	id := NewSpanID()
	require.Len(t, id, 16, "span id is 8 bytes = 16 hex chars")
	assertHex(t, id)
	assert.NotEqual(t, id, NewSpanID(), "span ids should differ")
}

// Given a trace id and key, when a span id is derived, it is 16 hex chars (8
// bytes), non-zero, and identical every time — so separate processes agree.
func TestSpanIDFrom_Deterministic(t *testing.T) {
	trace := NewTraceID()
	id := SpanIDFrom(trace, "subagent:agent-42")
	require.Len(t, id, 16, "derived span id is 8 bytes = 16 hex chars")
	assertHex(t, id)
	assert.NotEqual(t, "0000000000000000", id, "derived span id is never the zero id")
	assert.Equal(t, id, SpanIDFrom(trace, "subagent:agent-42"), "same inputs -> same id")
}

// Given the same key under different trace ids, or different keys under the same
// trace id, when span ids are derived, they differ — so ids don't collide
// across traces or across a trace's sub-agents.
func TestSpanIDFrom_DistinctInputs(t *testing.T) {
	trace := NewTraceID()
	other := NewTraceID()
	assert.NotEqual(t, SpanIDFrom(trace, "subagent:a"), SpanIDFrom(trace, "subagent:b"),
		"distinct keys -> distinct ids")
	assert.NotEqual(t, SpanIDFrom(trace, "subagent:a"), SpanIDFrom(other, "subagent:a"),
		"distinct traces -> distinct ids")
	assert.NotEqual(t, SpanIDFrom(trace, "agent-tool:a"), SpanIDFrom(trace, "subagent:a"),
		"the two prefixes for one agent_id -> distinct ids")
}

func assertHex(t *testing.T, s string) {
	t.Helper()
	for _, r := range s {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		assert.True(t, isHex, "non-hex char %q in %q", r, s)
	}
}
