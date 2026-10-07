// Package shim tests scripts/on-event.sh, the bash shim every hook runs, by
// executing the real script the way Claude Code does: through bash (Git Bash on
// Windows), with CLAUDE_PLUGIN_ROOT / CLAUDE_PLUGIN_DATA / CLAUDE_PLUGIN_OPTION_*
// in the environment and the hook payload on stdin.
//
// The e2e package covers the Go pipeline from an in-process call. This package
// covers what only shows up on a real OS: binary naming and the .exe suffix,
// line endings, paths with spaces, and that every exit path stays 0.
//
// Release-mode download against a published GitHub release is opt-in: set
// FIDDLER_SHIM_RELEASE_TAG (for example v0.8.0-rc.1). See
// .github/workflows/release-smoke.yml.
package shim

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// exeSuffix is the suffix Windows binaries need; scripts/on-event.sh uses the
// same rule.
var exeSuffix = func() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}()

// repoRoot returns the repository root (this file lives in test/shim).
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// findBash returns the bash that Claude Code would use, or skips the test.
// On Windows that is Git Bash. A plain "bash" on PATH can be WSL's
// C:\Windows\System32\bash.exe, which runs a Linux userland and would test the
// wrong thing, so it is never used there.
func findBash(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("FIDDLER_TEST_BASH"); p != "" {
		return p
	}
	if runtime.GOOS == "windows" {
		for _, p := range []string{
			`C:\Program Files\Git\bin\bash.exe`,
			`C:\Program Files\Git\usr\bin\bash.exe`,
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		t.Skip("Git Bash not found; set FIDDLER_TEST_BASH")
	}
	p, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	return p
}

// shellPath converts a native path to the form Claude Code passes to hooks on
// Windows ("C:/Users/..."); a no-op elsewhere.
func shellPath(p string) string { return filepath.ToSlash(p) }

// stagePlugin copies the files the shim needs into a fresh plugin root whose
// path contains a space (Windows profile paths often do), and returns it.
func stagePlugin(t *testing.T) string {
	t.Helper()
	src := repoRoot(t)
	root := filepath.Join(t.TempDir(), "plugin root")
	for _, rel := range []string{
		filepath.Join(".claude-plugin", "plugin.json"),
		filepath.Join("scripts", "on-event.sh"),
	} {
		data, err := os.ReadFile(filepath.Join(src, rel))
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, rel), data, 0o755))
	}
	return root
}

// pluginVersion reads the version from the staged plugin.json.
func pluginVersion(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".claude-plugin", "plugin.json"))
	require.NoError(t, err)
	m := regexp.MustCompile(`"version"\s*:\s*"([^"]+)"`).FindSubmatch(data)
	require.NotNil(t, m, "plugin.json has a version")
	return string(m[1])
}

// setPluginVersion rewrites the version in the staged plugin.json.
func setPluginVersion(t *testing.T, root, version string) {
	t.Helper()
	path := filepath.Join(root, ".claude-plugin", "plugin.json")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	out := regexp.MustCompile(`("version"\s*:\s*")[^"]+(")`).ReplaceAll(data, []byte("${1}"+version+"${2}"))
	require.NoError(t, os.WriteFile(path, out, 0o644))
}

// buildBinary compiles cmd/on-event to dest.
func buildBinary(t *testing.T, dest string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(dest), 0o755))
	cmd := exec.Command("go", "build", "-o", dest, "./cmd/on-event")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "go build: %s", out)
}

// receiver is a fake Fiddler OTLP endpoint that records requests.
type receiver struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []string // "<path> <Authorization>"
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		r.mu.Lock()
		r.reqs = append(r.reqs, req.URL.Path+" "+req.Header.Get("Authorization"))
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.reqs...)
}

// shimEnv builds a hook-like environment: the caller's environment minus any
// plugin/Fiddler variables that could leak in, plus the given ones. HOME and
// USERPROFILE point at a temp dir so no real ~/.claude.json is read.
func shimEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "CLAUDE_PLUGIN_"), strings.HasPrefix(k, "FIDDLER_"),
			strings.EqualFold(k, "HOME"), strings.EqualFold(k, "USERPROFILE"):
			continue
		}
		env = append(env, kv)
	}
	home := t.TempDir()
	env = append(env, "HOME="+shellPath(home), "USERPROFILE="+home)
	return append(env, extra...)
}

// runShim runs scripts/on-event.sh for one hook event and returns its exit
// code and stderr.
func runShim(t *testing.T, bash, root string, env []string, event, payloadFile string) (int, string) {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(repoRoot(t), "test", "e2e", "testdata", "payloads", payloadFile))
	require.NoError(t, err)

	cmd := exec.Command(bash, shellPath(filepath.Join(root, "scripts", "on-event.sh")), event)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(string(payload))
	var stderr strings.Builder
	cmd.Stderr = &stderr

	done := make(chan error, 1)
	require.NoError(t, cmd.Start())
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-time.After(90 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("%s: shim did not finish within 90s (a hook must never hang)", event)
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), stderr.String()
		}
		require.NoError(t, err)
	}
	return 0, stderr.String()
}

func optionEnv(r *receiver) []string {
	return []string{
		"CLAUDE_PLUGIN_OPTION_OTLP_URL=" + r.URL + "/",
		"CLAUDE_PLUGIN_OPTION_AUTH_TOKEN=test-token",
		"CLAUDE_PLUGIN_OPTION_APP_ID=test-app",
	}
}

// Given a local build in the plugin's bin/ and userConfig options pointing at a
// fake endpoint, when a prompt and a tool call go through the shim, then a
// trace reaches the endpoint with the configured token and every hook exits 0.
func TestShim_LocalModeExportsTrace(t *testing.T) {
	bash := findBash(t)
	root := stagePlugin(t)
	buildBinary(t, filepath.Join(root, "bin", "on-event"+exeSuffix))
	rcv := newReceiver(t)

	env := shimEnv(t, append(optionEnv(rcv),
		"CLAUDE_PLUGIN_ROOT="+shellPath(root),
		"CLAUDE_PLUGIN_DATA="+shellPath(filepath.Join(t.TempDir(), "data")),
		"FIDDLER_BINARY_SOURCE=local",
	)...)

	code, stderr := runShim(t, bash, root, env, "UserPromptSubmit", "user_prompt_submit.json")
	require.Equal(t, 0, code, "stderr: %s", stderr)
	code, stderr = runShim(t, bash, root, env, "PostToolUse", "post_tool_use.json")
	require.Equal(t, 0, code, "stderr: %s", stderr)

	assert.Contains(t, rcv.requests(), "/v1/traces Bearer test-token")
}

// Given no userConfig options at all, when a hook runs, then the shim and binary
// do nothing and still exit 0.
func TestShim_NoConfigFailsOpen(t *testing.T) {
	bash := findBash(t)
	root := stagePlugin(t)
	buildBinary(t, filepath.Join(root, "bin", "on-event"+exeSuffix))

	env := shimEnv(t,
		"CLAUDE_PLUGIN_ROOT="+shellPath(root),
		"CLAUDE_PLUGIN_DATA="+shellPath(filepath.Join(t.TempDir(), "data")),
		"FIDDLER_BINARY_SOURCE=local",
	)
	code, stderr := runShim(t, bash, root, env, "PostToolUse", "post_tool_use.json")
	assert.Equal(t, 0, code, "stderr: %s", stderr)
}

// Given a cached binary that exists and is executable but can't be started (a
// wrong-architecture file, or one blocked by policy), when a hook runs, then
// the shim reports it and exits 0 instead of bash's 126.
func TestShim_UnstartableBinaryFailsOpen(t *testing.T) {
	bash := findBash(t)
	root := stagePlugin(t)
	bad := filepath.Join(root, "bin", "on-event"+exeSuffix)
	require.NoError(t, os.MkdirAll(filepath.Dir(bad), 0o755))
	require.NoError(t, os.WriteFile(bad, []byte("\x00\x01\x02 not a real binary \x00"), 0o755))

	env := shimEnv(t,
		"CLAUDE_PLUGIN_ROOT="+shellPath(root),
		"CLAUDE_PLUGIN_DATA="+shellPath(filepath.Join(t.TempDir(), "data")),
		"FIDDLER_BINARY_SOURCE=local",
	)
	code, stderr := runShim(t, bash, root, env, "PostToolUse", "post_tool_use.json")
	assert.Equal(t, 0, code, "stderr: %s", stderr)
	assert.Contains(t, stderr, "could not execute")
}

// Given local builds where only the binary for this machine's OS/arch can run
// (the other architecture's name holds a file that can't start), when hooks
// run, then a trace arrives, so the shim picked this machine's binary from the
// real uname. On the windows-11-arm runner this exercises the "-ARM64"
// detection with Git Bash's own uname, which reports x86_64 for -m there.
func TestShim_PicksBinaryForThisMachine(t *testing.T) {
	other := map[string]string{"amd64": "arm64", "arm64": "amd64"}[runtime.GOARCH]
	if other == "" {
		t.Skipf("no alternate architecture for %s", runtime.GOARCH)
	}
	bash := findBash(t)
	root := stagePlugin(t)
	binPath := func(arch string) string {
		return filepath.Join(root, "bin", "on-event-"+runtime.GOOS+"-"+arch+exeSuffix)
	}
	buildBinary(t, binPath(runtime.GOARCH))
	require.NoError(t, os.WriteFile(binPath(other), []byte("\x00 not a binary \x00"), 0o755))
	rcv := newReceiver(t)

	env := shimEnv(t, append(optionEnv(rcv),
		"CLAUDE_PLUGIN_ROOT="+shellPath(root),
		"CLAUDE_PLUGIN_DATA="+shellPath(filepath.Join(t.TempDir(), "data")),
		"FIDDLER_BINARY_SOURCE=local",
	)...)
	for _, step := range []struct{ event, payload string }{
		{"UserPromptSubmit", "user_prompt_submit.json"},
		{"PostToolUse", "post_tool_use.json"},
	} {
		code, stderr := runShim(t, bash, root, env, step.event, step.payload)
		require.Equal(t, 0, code, "stderr: %s", stderr)
		require.NotContains(t, stderr, "could not execute",
			"%s: shim picked the %s binary instead of %s", step.event, other, runtime.GOARCH)
	}
	assert.Contains(t, rcv.requests(), "/v1/traces Bearer test-token")
}

// fakeUname puts a uname on PATH that prints sysname for -s and machine for -m,
// imitating Git Bash on Windows, and returns a PATH value with it first.
func fakeUname(t *testing.T, sysname, machine string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n  -m) echo " + machine + " ;;\n  *) echo " + sysname + " ;;\nesac\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "uname"), []byte(script), 0o755))
	return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// markerStub writes an executable script at path that creates marker when run.
func markerStub(t *testing.T, path, marker string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	stub := "#!/bin/sh\ncat >/dev/null\ntouch '" + marker + "'\n"
	require.NoError(t, os.WriteFile(path, []byte(stub), 0o755))
}

// Given uname reports Git Bash on Windows, when the shim resolves the binary,
// then it uses the Windows names for the release cache
// (on-event-windows-<arch>-<version>.exe) and for local builds
// (on-event-windows-<arch>.exe). On Windows ARM64, Git Bash's uname -m reports
// x86_64 (its tools run under x64 emulation) and only the "-ARM64" suffix on
// uname -s identifies the host, so that case must still resolve to arm64.
// Runs on macOS/Linux only; on Windows, Git Bash's own uname wins over PATH and
// the local/release tests exercise the real names.
func TestShim_WindowsNaming(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a fake uname; covered by the real-name tests on Windows")
	}
	bash := findBash(t)

	for _, tc := range []struct {
		name, sysname, machine, arch string
	}{
		{"x64", "MINGW64_NT-10.0-26100", "x86_64", "amd64"},
		{"arm64 under x64 emulation", "MINGW64_NT-10.0-26100-ARM64", "x86_64", "arm64"},
		{"arm64 msys sysname", "MSYS_NT-10.0-26100-ARM64", "x86_64", "arm64"},
		{"native arm64 runtime", "MINGW64_NT-10.0-26100", "aarch64", "arm64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pathEnv := fakeUname(t, tc.sysname, tc.machine)

			t.Run("release cache", func(t *testing.T) {
				root := stagePlugin(t)
				data := filepath.Join(t.TempDir(), "data")
				marker := filepath.Join(t.TempDir(), "ran")
				markerStub(t, filepath.Join(data, "bin", "on-event-windows-"+tc.arch+"-"+pluginVersion(t, root)+".exe"), marker)

				env := shimEnv(t, "CLAUDE_PLUGIN_ROOT="+root, "CLAUDE_PLUGIN_DATA="+data, pathEnv)
				code, stderr := runShim(t, bash, root, env, "PostToolUse", "post_tool_use.json")
				require.Equal(t, 0, code, "stderr: %s", stderr)
				assert.FileExists(t, marker, "cached on-event-windows-%s binary was run; stderr: %s", tc.arch, stderr)
			})

			t.Run("local build", func(t *testing.T) {
				root := stagePlugin(t)
				marker := filepath.Join(t.TempDir(), "ran")
				markerStub(t, filepath.Join(root, "bin", "on-event-windows-"+tc.arch+".exe"), marker)

				env := shimEnv(t, "CLAUDE_PLUGIN_ROOT="+root, "CLAUDE_PLUGIN_DATA="+t.TempDir(),
					"FIDDLER_BINARY_SOURCE=local", pathEnv)
				code, stderr := runShim(t, bash, root, env, "PostToolUse", "post_tool_use.json")
				require.Equal(t, 0, code, "stderr: %s", stderr)
				assert.FileExists(t, marker, "local on-event-windows-%s binary was run; stderr: %s", tc.arch, stderr)
			})
		})
	}
}

// Given FIDDLER_SHIM_RELEASE_TAG names a published release, when hooks run in
// the default release mode, then the shim downloads that release's binary for
// this OS/arch, verifies its checksum, caches it under the versioned name,
// prunes an older cached version, and the binary exports a trace. Opt-in: it
// needs network access and an existing release.
func TestShim_ReleaseDownload(t *testing.T) {
	tag := os.Getenv("FIDDLER_SHIM_RELEASE_TAG")
	if tag == "" {
		t.Skip("set FIDDLER_SHIM_RELEASE_TAG (e.g. v0.8.0-rc.1) to test a published release")
	}
	// The tag becomes part of a download URL and a cache path; accept only
	// v<semver> (the same shape release.yml's version guard enforces).
	require.Regexp(t, `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`, tag, "FIDDLER_SHIM_RELEASE_TAG must be v<semver>")
	bash := findBash(t)
	root := stagePlugin(t)
	version := strings.TrimPrefix(tag, "v")
	setPluginVersion(t, root, version)

	osName := runtime.GOOS
	arch := runtime.GOARCH
	data := filepath.Join(t.TempDir(), "data")
	stale := filepath.Join(data, "bin", "on-event-"+osName+"-"+arch+"-0.0.1"+exeSuffix)
	require.NoError(t, os.MkdirAll(filepath.Dir(stale), 0o755))
	require.NoError(t, os.WriteFile(stale, []byte("old"), 0o755))

	rcv := newReceiver(t)
	env := shimEnv(t, append(optionEnv(rcv),
		"CLAUDE_PLUGIN_ROOT="+shellPath(root),
		"CLAUDE_PLUGIN_DATA="+shellPath(data),
	)...)

	code, stderr := runShim(t, bash, root, env, "UserPromptSubmit", "user_prompt_submit.json")
	require.Equal(t, 0, code, "stderr: %s", stderr)
	code, stderr = runShim(t, bash, root, env, "PostToolUse", "post_tool_use.json")
	require.Equal(t, 0, code, "stderr: %s", stderr)

	cached := filepath.Join(data, "bin", "on-event-"+osName+"-"+arch+"-"+version+exeSuffix)
	assert.FileExists(t, cached, "verified binary cached under the versioned name; stderr: %s", stderr)
	assert.NoFileExists(t, stale, "older cached version pruned")
	assert.Contains(t, rcv.requests(), "/v1/traces Bearer test-token", "stderr: %s", stderr)
}
