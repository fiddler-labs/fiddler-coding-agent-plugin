// Package config resolves plugin configuration.
//
// The core pipeline accepts a resolved [Config] struct and never reads
// the environment directly. Config resolution lives here so the pipeline
// stays runtime-agnostic.
//
// Resolution order (first non-empty wins):
//  1. Claude Code plugin userConfig (CLAUDE_PLUGIN_OPTION_*)
//  2. Standard OTel environment variables (OTEL_EXPORTER_OTLP_*)
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the resolved plugin configuration.
type Config struct {
	// Endpoint is the OTLP/HTTP base URL.
	Endpoint string

	// APIKey is the ingestion token (Bearer token value).
	APIKey string

	// AppID is the application.id resource attribute. It is required for spans
	// to be accepted by the endpoint.
	AppID string

	// AgentName and Provider identify the coding-agent runtime being
	// observed (e.g. "claude-code"/"anthropic"). They are set by the
	// entrypoint from the active runtime adapter, not resolved from the
	// environment, and are stamped as gen_ai.agent.name / gen_ai.provider.name.
	AgentName string
	Provider  string

	// ServiceName is the service.name resource attribute — the service that
	// produces the telemetry (the plugin itself), distinct from the observed
	// AgentName. Set by the entrypoint from the active runtime adapter.
	ServiceName string

	// PluginVersion is the plugin's own version, read from
	// .claude-plugin/plugin.json and stamped as the service.version resource
	// attribute. Empty when the manifest cannot be read (attribute omitted
	// downstream), e.g. when CLAUDE_PLUGIN_ROOT is unset or the file is missing.
	PluginVersion string

	// OmitUserInfo controls whether personally-identifying account attributes
	// are emitted. When true, otlp.BuildPayload omits the user.email and
	// user.account_uuid resource attributes; the hashed user.id and the
	// org-level organization.id are still emitted (user.id is already a hash,
	// and organization.id is not personal). Defaults to false.
	OmitUserInfo bool

	// Identity/workspace attributes, resolved from process-local sources (local
	// git config, the coding agent's local account state, and the hook
	// environment) by the entrypoint — not from OTel env vars — and stamped as
	// OTLP resource attributes to mirror the coding agent's native
	// OpenTelemetry resource. See internal/identity. Any of these may be empty
	// when its source is unavailable, in which case the corresponding attribute
	// is omitted.
	UserID         string // user.id (hashed, 64-char SHA-256)
	OrgID          string // organization.id
	AccountUUID    string // user.account_uuid
	UserName       string // user.name (git config user.name)
	UserEmail      string // user.email (git config user.email, else account OAuth email)
	IdentitySource string // user.identity.source (identity origin, e.g. "git")
	AppEntrypoint  string // app.entrypoint (e.g. "cli")
	TerminalType   string // terminal.type (terminal program)
	Cwd            string // cwd (workspace directory)

	// AppVersion is the observed coding agent's version (Claude Code's app
	// version), read from the session transcript by the entrypoint and stamped
	// as the app.version resource attribute — the key Claude Code's own
	// OpenTelemetry uses for its version. Distinct from the plugin's own
	// PluginVersion (service.version). Empty when the transcript is unavailable,
	// in which case the attribute is omitted.
	AppVersion string
}

// Valid returns true if the minimum required fields are set.
func (c *Config) Valid() bool {
	return c.Endpoint != "" && c.APIKey != "" && c.AppID != ""
}

// Load resolves configuration from available sources.
//
// Priority: Claude Code plugin userConfig (CLAUDE_PLUGIN_OPTION_*) takes
// precedence over the standard OTel environment variables. This lets the
// plugin work out of the box when installed via Claude Code (userConfig
// prompts on first load) while remaining compatible with
// organization-managed OTel env vars.
func Load() *Config {
	// --- Source 1: Claude Code plugin userConfig ---
	// Claude Code exposes userConfig values as CLAUDE_PLUGIN_OPTION_<KEY>.
	pluginEndpoint := strings.TrimRight(os.Getenv("CLAUDE_PLUGIN_OPTION_OTLP_URL"), "/")
	pluginToken := os.Getenv("CLAUDE_PLUGIN_OPTION_AUTH_TOKEN")
	pluginAppID := os.Getenv("CLAUDE_PLUGIN_OPTION_APP_ID")

	// --- Source 2: Standard OTel env vars (fallback) ---
	otelEndpoint := strings.TrimRight(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), "/")
	headers := parseKV(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"))
	resourceAttrs := parseKV(os.Getenv("OTEL_RESOURCE_ATTRIBUTES"))

	otelAppID := headers["fiddler-application-id"]
	if otelAppID == "" {
		otelAppID = resourceAttrs["application.id"]
	}

	otelToken := ""
	if auth := headers["Authorization"]; auth != "" {
		otelToken = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer"))
	}

	// --- Resolve: plugin option wins, then OTel env var ---
	return &Config{
		Endpoint: firstNonEmpty(pluginEndpoint, otelEndpoint),
		APIKey:   firstNonEmpty(pluginToken, otelToken),
		AppID:    firstNonEmpty(pluginAppID, otelAppID),
		// Defaults to false (user identity attributes are included); set
		// FIDDLER_OMIT_USER_INFO=true to drop them.
		OmitUserInfo:  envBool("FIDDLER_OMIT_USER_INFO", false),
		PluginVersion: pluginVersion(),
	}
}

// pluginVersion reads the plugin's version from .claude-plugin/plugin.json,
// located via the CLAUDE_PLUGIN_ROOT environment variable that Claude Code sets
// for hook processes. plugin.json is the single source of truth for the version
// (see AGENTS.md). Best-effort: returns "" on any error (unset env, missing
// file, or parse failure) so a missing manifest simply omits the attribute.
func pluginVersion() string {
	root := os.Getenv("CLAUDE_PLUGIN_ROOT")
	if root == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(root, ".claude-plugin", "plugin.json"))
	if err != nil {
		return ""
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	return m.Version
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseKV parses comma-separated key=value pairs (the format used by
// OTEL_EXPORTER_OTLP_HEADERS and OTEL_RESOURCE_ATTRIBUTES).
func parseKV(raw string) map[string]string {
	result := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if k, v, ok := strings.Cut(pair, "="); ok {
			result[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return result
}

// envBool reads a boolean environment variable with a default.
// Recognizes "true", "1", "yes" (case-insensitive) as true;
// "false", "0", "no" as false; anything else returns the default.
func envBool(key string, defaultVal bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	default:
		return defaultVal
	}
}
