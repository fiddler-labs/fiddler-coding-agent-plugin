// Package identity resolves the developer/account identity and workspace
// attributes the plugin can source locally, so plugin traces carry the same
// identity signal as Claude Code's native OpenTelemetry resource.
//
// Every value here is process-local — readable in any hook process without a
// live event — so the caller resolves it once and stamps it as OTLP resource
// attributes (see internal/otlp.BuildPayload). Resolution is best-effort and
// fails open: a missing file, unreadable home directory, absent env var, or
// missing git binary yields an empty field, never an error, consistent with the
// plugin's never-block-the-session posture.
//
// The developer identity (user.name / user.email) prefers the local git config
// — `git config user.name` / `user.email`, which git resolves across its
// system → global → local scopes, so a global identity is found even outside a
// working tree. The email falls back to the Claude OAuth email when git has
// none; the name has no such fallback (Claude Code exposes no user name).
//
// The account identity (and the OAuth email fallback) is read from
// ~/.claude.json, which is Claude Code's own internal state, NOT a public
// contract: its schema may change across Claude Code versions. Treat the field
// mapping as best-effort and keep the read tolerant of missing keys.
package identity

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Source values for Identity.Source, recording the origin of the resolved
// developer identity.
const (
	SourceGit    = "git"
	SourceClaude = "claude"
)

// Identity holds the locally-sourced identity/workspace attributes. Any field
// may be empty when its source is unavailable.
type Identity struct {
	// UserID is Claude Code's hashed user id (native attribute user.id),
	// a 64-char SHA-256 read verbatim from ~/.claude.json so it matches the
	// value native OTEL emits.
	UserID string
	// OrgID is the organization UUID (native attribute organization.id).
	OrgID string
	// AccountUUID is the Claude account UUID (native attribute user.account_uuid).
	AccountUUID string
	// Name is the developer's name for the attribute user.name, taken from
	// `git config user.name`. Empty when git has no name configured; there is no
	// account-level fallback (Claude Code exposes no user name).
	Name string
	// Email is the developer's email for the attribute user.email. It prefers
	// `git config user.email` and falls back to the Claude OAuth email; empty
	// when neither is available.
	Email string
	// Source records where the emitted identity came from — SourceGit or
	// SourceClaude — for the attribute user.identity.source. It reflects the
	// origin of Email, or of the git-only Name when no email resolved. Empty
	// when no name or email resolved. Not itself identifying.
	Source string
	// Entrypoint is how Claude Code was launched (native attribute
	// app.entrypoint), e.g. "cli", from CLAUDE_CODE_ENTRYPOINT.
	Entrypoint string
	// TerminalType names the terminal program (native attribute terminal.type),
	// from TERM_PROGRAM, falling back to TERM.
	TerminalType string
	// Cwd is the workspace directory the hook process runs in.
	Cwd string
}

// claudeConfig is the subset of ~/.claude.json this package reads. Unknown
// keys are ignored by encoding/json, so the struct stays tolerant of the
// file's larger, evolving schema.
type claudeConfig struct {
	UserID       string `json:"userID"`
	OAuthAccount struct {
		AccountUUID      string `json:"accountUuid"`
		OrganizationUUID string `json:"organizationUuid"`
		EmailAddress     string `json:"emailAddress"`
	} `json:"oauthAccount"`
}

// Resolve gathers the identity/workspace attributes from process-local
// sources. It never returns an error: each source degrades independently to
// an empty field.
func Resolve() Identity {
	id := Identity{
		Entrypoint:   os.Getenv("CLAUDE_CODE_ENTRYPOINT"),
		TerminalType: firstNonEmpty(os.Getenv("TERM_PROGRAM"), os.Getenv("TERM")),
	}

	if cwd, err := os.Getwd(); err == nil {
		id.Cwd = cwd
	}

	var claudeEmail string
	if cc, ok := readClaudeConfig(); ok {
		id.UserID = cc.UserID
		id.AccountUUID = cc.OAuthAccount.AccountUUID
		id.OrgID = cc.OAuthAccount.OrganizationUUID
		claudeEmail = cc.OAuthAccount.EmailAddress
	}

	id.Name, id.Email, id.Source = resolveUser(
		gitConfig("user.name"),
		gitConfig("user.email"),
		claudeEmail,
	)

	return id
}

// resolveUser applies the developer-identity precedence, separated from its I/O
// so the git→Claude fallback can be tested without a git binary. The email
// prefers the git value and falls back to the Claude OAuth email; the name is
// git-only. Source records the origin of the email, or of the git-only name
// when no email resolved.
func resolveUser(gitName, gitEmail, claudeEmail string) (name, email, source string) {
	name = strings.TrimSpace(gitName)
	gitEmail = strings.TrimSpace(gitEmail)
	claudeEmail = strings.TrimSpace(claudeEmail)

	switch {
	case gitEmail != "":
		email, source = gitEmail, SourceGit
	case claudeEmail != "":
		email, source = claudeEmail, SourceClaude
	}

	// No email resolved but a git name still originates from git.
	if source == "" && name != "" {
		source = SourceGit
	}

	return name, email, source
}

// gitConfigTimeout bounds each git invocation so a pathological git call can
// never stall the hook. `git config` is a fast local read; the timeout is a
// backstop that preserves the fail-open, never-block posture (mirroring the
// OTLP exporter's own short timeout).
const gitConfigTimeout = 2 * time.Second

// gitConfig returns the value of a git config key, or "" on any error (git
// missing, key unset, or timeout). git resolves the key across its system →
// global → local scopes, so a global identity is found even outside a working
// tree. Best-effort and fail-open, consistent with the rest of this package.
func gitConfig(key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitConfigTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "git", "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readClaudeConfig reads and parses ~/.claude.json best-effort. The second
// return is false when the file is absent, unreadable, or malformed.
func readClaudeConfig() (claudeConfig, bool) {
	path := claudeConfigPath()
	if path == "" {
		return claudeConfig{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return claudeConfig{}, false
	}
	var cc claudeConfig
	if err := json.Unmarshal(data, &cc); err != nil {
		return claudeConfig{}, false
	}
	return cc, true
}

// claudeConfigPath returns the path to ~/.claude.json, or "" if the home
// directory cannot be determined.
func claudeConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude.json")
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
