package turnctx

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Given a turn context created for a session, when it is loaded back, then the
// persisted trace id, root span id, start time, prompt, and cwd all round-trip.
func TestNewAndLoad_RoundTrip(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())

	created, err := New("sess1", "fix the bug", "/repo")
	require.NoError(t, err)
	require.NotEmpty(t, created.TraceID)
	require.NotEmpty(t, created.RootSpanID)
	require.NotEmpty(t, created.StartNano)

	loaded, err := Load("sess1")
	require.NoError(t, err)
	assert.Equal(t, created.TraceID, loaded.TraceID)
	assert.Equal(t, created.RootSpanID, loaded.RootSpanID)
	assert.Equal(t, created.StartNano, loaded.StartNano)
	assert.Equal(t, "fix the bug", loaded.UserPrompt)
	assert.Equal(t, "/repo", loaded.Cwd)
}

// Given no context file for a session, when Load is called, then it lazily
// creates a fresh context with a new trace id rather than failing.
func TestLoad_LazyCreatesWhenMissing(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())

	loaded, err := Load("never-started")
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.NotEmpty(t, loaded.TraceID, "lazy-created context has a trace id")
	assert.NotEmpty(t, loaded.RootSpanID)
}

// Given a persisted context, when it is cleared and then loaded again, then a
// new context with a different trace id is lazily created — the cleared one is
// gone.
func TestClear(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())

	_, err := New("sess1", "p", "/repo")
	require.NoError(t, err)

	Clear("sess1")
	// After clear, Load lazily creates a fresh context with a new trace id.
	before, _ := New("sess2", "p", "/repo")
	Clear("sess2")
	reloaded, err := Load("sess2")
	require.NoError(t, err)
	assert.NotEqual(t, before.TraceID, reloaded.TraceID, "cleared context is gone; a new one is created")
}

// Given no context for a session, when Clear is called, then it is a no-op and
// does not panic.
func TestClear_IdempotentWhenAbsent(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	assert.NotPanics(t, func() { Clear("does-not-exist") })
}

// Peek returns the persisted context for a live turn without creating one.
func TestPeek_ValidFile(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())

	created, err := New("sess1", "fix the bug", "/repo")
	require.NoError(t, err)

	peeked, ok := Peek("sess1")
	require.True(t, ok, "existing context is found")
	assert.Equal(t, created.TraceID, peeked.TraceID)
	assert.Equal(t, created.RootSpanID, peeked.RootSpanID)
	assert.Equal(t, created.StartNano, peeked.StartNano)
	assert.Equal(t, "fix the bug", peeked.UserPrompt)
}

// Unlike Load, Peek does not lazily create a context when none exists: a
// missing file reports absence rather than fabricating a fresh turn.
func TestPeek_MissingFile(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())

	ctx, ok := Peek("never-started")
	assert.False(t, ok, "missing file reports absence")
	assert.Nil(t, ctx)
}

// A torn or otherwise unparseable file is treated as absent (nil, false),
// never surfaced as a partial context — the fail-open contract this package
// documents for the write race.
func TestPeek_CorruptFile(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())

	require.NoError(t, os.WriteFile(ctxPath("sess-corrupt"), []byte("{not valid json"), 0o644))

	ctx, ok := Peek("sess-corrupt")
	assert.False(t, ok, "corrupt file reports absence")
	assert.Nil(t, ctx)
}
