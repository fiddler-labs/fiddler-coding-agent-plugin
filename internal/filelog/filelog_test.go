package filelog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Given CLAUDE_PLUGIN_DATA points at a directory, when lines are logged, then
// each is appended to plugin.log there with its level, and the log follows a
// change of CLAUDE_PLUGIN_DATA (the file is reopened per write, not held open,
// so on Windows the directory can still be removed afterwards).
func TestWrite_AppendsToPluginDataLog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_PLUGIN_DATA", dir)

	Info("first id=%s", "abc")
	Warn("second n=%d", 2)
	Error("third")

	data, err := os.ReadFile(filepath.Join(dir, "plugin.log"))
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 3)
	assert.Contains(t, lines[0], "INFO first id=abc")
	assert.Contains(t, lines[1], "WARN second n=2")
	assert.Contains(t, lines[2], "ERROR third")

	other := t.TempDir()
	t.Setenv("CLAUDE_PLUGIN_DATA", other)
	Info("moved")
	data, err = os.ReadFile(filepath.Join(other, "plugin.log"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "INFO moved")
}
