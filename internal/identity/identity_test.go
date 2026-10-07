package identity

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setHome points the home directory at dir on every OS: os.UserHomeDir reads
// HOME on Unix but USERPROFILE on Windows.
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// writeClaudeConfig points the home directory at a temp dir and writes
// ~/.claude.json there.
func writeClaudeConfig(t *testing.T, contents string) {
	t.Helper()
	home := t.TempDir()
	setHome(t, home)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude.json"), []byte(contents), 0o600))
}

// suppressGitConfig points git at empty config files so no host git identity
// leaks into a test. Combined with this repo carrying no local user.* config,
// gitConfig() then resolves to "".
//
// An empty temp file rather than os.DevNull: os.DevNull is "NUL" on Windows,
// which is not a dependable config path for Git for Windows.
func suppressGitConfig(t *testing.T) {
	t.Helper()
	empty := emptyGitConfig(t)
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_SYSTEM", empty)
}

// emptyGitConfig creates an empty git config file and returns its path.
func emptyGitConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "empty-gitconfig")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	return path
}

// setGitConfig writes a temp global git config carrying the given user identity
// and points git at it (system config suppressed). An empty name or email is
// omitted from the file so the corresponding key stays unset.
func setGitConfig(t *testing.T, name, email string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gitconfig")
	body := "[user]\n"
	if name != "" {
		body += fmt.Sprintf("\tname = %s\n", name)
	}
	if email != "" {
		body += fmt.Sprintf("\temail = %s\n", email)
	}
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", path)
	t.Setenv("GIT_CONFIG_SYSTEM", emptyGitConfig(t))
}

// Given a well-formed ~/.claude.json and no git identity, when identity is
// resolved, the hashed user id and the oauthAccount fields (account uuid, org
// uuid, email) are read through, unrecognized keys are ignored, and user.email
// falls back to the OAuth email tagged as claude-sourced.
func TestResolve_ReadsAccountIdentity(t *testing.T) {
	suppressGitConfig(t)
	writeClaudeConfig(t, `{
		"userID": "hashed-user-id",
		"oauthAccount": {
			"accountUuid": "acct-uuid",
			"organizationUuid": "org-uuid",
			"emailAddress": "dev@example.com"
		},
		"someOtherKey": {"ignored": true}
	}`)

	id := Resolve()
	assert.Equal(t, "hashed-user-id", id.UserID)
	assert.Equal(t, "acct-uuid", id.AccountUUID)
	assert.Equal(t, "org-uuid", id.OrgID)
	assert.Empty(t, id.Name, "no git name configured")
	assert.Equal(t, "dev@example.com", id.Email, "falls back to OAuth email")
	assert.Equal(t, SourceClaude, id.Source)
}

// Given no ~/.claude.json on disk and no git identity, when identity is
// resolved, the identity fields fail open to empty rather than the call
// erroring.
func TestResolve_MissingFileFailsOpen(t *testing.T) {
	suppressGitConfig(t)
	setHome(t, t.TempDir()) // no .claude.json written
	id := Resolve()
	assert.Empty(t, id.UserID)
	assert.Empty(t, id.AccountUUID)
	assert.Empty(t, id.OrgID)
	assert.Empty(t, id.Name)
	assert.Empty(t, id.Email)
	assert.Empty(t, id.Source)
}

// Given a ~/.claude.json that is not valid JSON, when identity is resolved, the
// account fields fail open to empty.
func TestResolve_MalformedFileFailsOpen(t *testing.T) {
	suppressGitConfig(t)
	writeClaudeConfig(t, `{ not valid json `)
	id := Resolve()
	assert.Empty(t, id.UserID)
	assert.Empty(t, id.Email)
	assert.Empty(t, id.Source)
}

// Given a git identity, when identity is resolved, user.name/user.email come
// from git and take precedence over the Claude OAuth email, tagged git-sourced.
func TestResolve_PrefersGitIdentity(t *testing.T) {
	setGitConfig(t, "Jane Dev", "jane@example.com")
	writeClaudeConfig(t, `{"oauthAccount": {"emailAddress": "oauth@example.com"}}`)

	id := Resolve()
	assert.Equal(t, "Jane Dev", id.Name)
	assert.Equal(t, "jane@example.com", id.Email, "git email preferred over OAuth")
	assert.Equal(t, SourceGit, id.Source)
}

// Given git configures a name but no email, when identity is resolved, the name
// is the git name while the email falls back to the OAuth email; the source
// reflects the email's (claude) origin.
func TestResolve_GitNameOAuthEmailFallback(t *testing.T) {
	setGitConfig(t, "Jane Dev", "")
	writeClaudeConfig(t, `{"oauthAccount": {"emailAddress": "oauth@example.com"}}`)

	id := Resolve()
	assert.Equal(t, "Jane Dev", id.Name)
	assert.Equal(t, "oauth@example.com", id.Email, "no git email, falls back to OAuth")
	assert.Equal(t, SourceClaude, id.Source, "source tracks the email origin")
}

// resolveUser holds the identity precedence; exercise every branch directly
// (no git binary or files) so the fallback logic stays pinned.
func TestResolveUser(t *testing.T) {
	tests := []struct {
		name                          string
		gitName, gitEmail, claudEmail string
		wantName, wantEmail, wantSrc  string
	}{
		{"git name and email", "Jane", "jane@git", "oauth@cc", "Jane", "jane@git", SourceGit},
		{"git email preferred", "", "jane@git", "oauth@cc", "", "jane@git", SourceGit},
		{"oauth email fallback", "", "", "oauth@cc", "", "oauth@cc", SourceClaude},
		{"git name only", "Jane", "", "", "Jane", "", SourceGit},
		{"git name, oauth email", "Jane", "", "oauth@cc", "Jane", "oauth@cc", SourceClaude},
		{"nothing", "", "", "", "", "", ""},
		{"whitespace trimmed", "  Jane  ", "  jane@git  ", "", "Jane", "jane@git", SourceGit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, email, src := resolveUser(tt.gitName, tt.gitEmail, tt.claudEmail)
			assert.Equal(t, tt.wantName, name)
			assert.Equal(t, tt.wantEmail, email)
			assert.Equal(t, tt.wantSrc, src)
		})
	}
}

// Given the hook environment sets CLAUDE_CODE_ENTRYPOINT and TERM_PROGRAM, when
// identity is resolved, entrypoint and terminal type are populated from them and
// cwd is the process working directory.
func TestResolve_EnvironmentAttributes(t *testing.T) {
	setHome(t, t.TempDir())
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("TERM_PROGRAM", "kitty")

	id := Resolve()
	assert.Equal(t, "cli", id.Entrypoint)
	assert.Equal(t, "kitty", id.TerminalType, "TERM_PROGRAM preferred")

	cwd, err := os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, cwd, id.Cwd)
}

// Given TERM_PROGRAM is unset but TERM is set, when identity is resolved, the
// terminal type falls back to TERM.
func TestResolve_TerminalFallsBackToTERM(t *testing.T) {
	setHome(t, t.TempDir())
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("TERM", "xterm-256color")

	id := Resolve()
	assert.Equal(t, "xterm-256color", id.TerminalType)
}
