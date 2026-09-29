package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Given both CLAUDE_PLUGIN_OPTION_* and OTEL_* env vars are set, when config is
// loaded, then the plugin options win over the OTel fallback and the endpoint's
// trailing slash is trimmed.
func TestLoad_PluginOptionsWin(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_OPTION_OTLP_URL", "https://plugin.example.com/")
	t.Setenv("CLAUDE_PLUGIN_OPTION_AUTH_TOKEN", "plugin-tok")
	t.Setenv("CLAUDE_PLUGIN_OPTION_APP_ID", "plugin-app")
	// OTel env vars present but should be overridden by plugin options.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://otel.example.com")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer otel-tok,fiddler-application-id=otel-app")

	cfg := Load()
	assert.Equal(t, "https://plugin.example.com", cfg.Endpoint, "trailing slash trimmed")
	assert.Equal(t, "plugin-tok", cfg.APIKey)
	assert.Equal(t, "plugin-app", cfg.AppID)
	assert.True(t, cfg.Valid())
}

// Given no plugin options but the standard OTEL_* env vars set, when config is
// loaded, then the values come from the OTel vars and the "Bearer " prefix is
// stripped from the token.
func TestLoad_FallsBackToOTelEnv(t *testing.T) {
	// No plugin options set; OTel env vars provide everything.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://otel.example.com")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer otel-tok,fiddler-application-id=otel-app")

	cfg := Load()
	assert.Equal(t, "https://otel.example.com", cfg.Endpoint)
	assert.Equal(t, "otel-tok", cfg.APIKey, "Bearer prefix stripped")
	assert.Equal(t, "otel-app", cfg.AppID)
}

// Given the app id is absent from the OTLP headers but present in
// OTEL_RESOURCE_ATTRIBUTES, when config is loaded, then the app id falls back to
// the resource attribute.
func TestLoad_AppIDFromResourceAttrs(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://otel.example.com")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer otel-tok")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "application.id=resource-app")

	cfg := Load()
	assert.Equal(t, "resource-app", cfg.AppID, "app id falls back to resource attribute")
}

// Given only the required plugin options are set, when config is loaded, then
// OmitUserInfo defaults to false.
func TestLoad_Defaults(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_OPTION_OTLP_URL", "https://plugin.example.com")
	t.Setenv("CLAUDE_PLUGIN_OPTION_AUTH_TOKEN", "tok")
	t.Setenv("CLAUDE_PLUGIN_OPTION_APP_ID", "app")

	cfg := Load()
	assert.False(t, cfg.OmitUserInfo, "OMIT_USER_INFO defaults to false")
}

// writePluginManifest creates a temp CLAUDE_PLUGIN_ROOT containing a
// .claude-plugin/plugin.json with the given version and returns the root.
func writePluginManifest(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".claude-plugin")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	manifest := `{"name":"fiddler","version":"` + version + `"}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644))
	return root
}

// Given a plugin manifest on disk carrying a version, when config is loaded,
// then PluginVersion is read from that manifest.
func TestLoad_PluginVersionFromManifest(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_ROOT", writePluginManifest(t, "1.2.3"))
	assert.Equal(t, "1.2.3", Load().PluginVersion)
}

// Given the plugin root is unset, has no manifest, or has a malformed manifest,
// when the plugin version is resolved, then each case yields "" (fail open)
// rather than erroring.
func TestPluginVersion_MissingSourcesYieldEmpty(t *testing.T) {
	t.Run("unset CLAUDE_PLUGIN_ROOT", func(t *testing.T) {
		t.Setenv("CLAUDE_PLUGIN_ROOT", "")
		assert.Equal(t, "", pluginVersion())
	})
	t.Run("manifest missing", func(t *testing.T) {
		t.Setenv("CLAUDE_PLUGIN_ROOT", t.TempDir())
		assert.Equal(t, "", pluginVersion(), "no plugin.json in root")
	})
	t.Run("manifest malformed", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, ".claude-plugin")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "plugin.json"), []byte("not json"), 0o644))
		t.Setenv("CLAUDE_PLUGIN_ROOT", root)
		assert.Equal(t, "", pluginVersion(), "parse error yields empty")
	})
}

// Given configs with various required fields present or missing, when Valid is
// checked, then it is true only when endpoint, token, and app id are all set.
func TestValid(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"complete", Config{Endpoint: "e", APIKey: "k", AppID: "a"}, true},
		{"no endpoint", Config{APIKey: "k", AppID: "a"}, false},
		{"no token", Config{Endpoint: "e", AppID: "a"}, false},
		{"no app id", Config{Endpoint: "e", APIKey: "k"}, false},
		{"empty", Config{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.cfg.Valid())
		})
	}
}

// Given an env var set to various truthy, falsy, and unrecognized values, when
// envBool reads it, then recognized values parse accordingly and an
// unrecognized value falls back to the supplied default.
func TestEnvBool(t *testing.T) {
	t.Setenv("FIDDLER_TEST_BOOL", "true")
	assert.True(t, envBool("FIDDLER_TEST_BOOL", false))
	t.Setenv("FIDDLER_TEST_BOOL", "1")
	assert.True(t, envBool("FIDDLER_TEST_BOOL", false))
	t.Setenv("FIDDLER_TEST_BOOL", "no")
	assert.False(t, envBool("FIDDLER_TEST_BOOL", true))
	t.Setenv("FIDDLER_TEST_BOOL", "garbage")
	assert.True(t, envBool("FIDDLER_TEST_BOOL", true), "unrecognized falls back to default")
}
