# Agent Guidance

This file is the entrypoint for AI coding agents (Claude Code, Cursor, OpenCode, etc.) working in this repository. It captures invariants and conventions that are not enforced by CI alone.

The code is the authoritative source of truth for architecture and behavior. Read the packages under `internal/` for implementation details.

## Security

- **Never commit secrets or tokens.** `.env.local` is gitignored and must stay that way.
- The ingestion token (`AUTH_TOKEN` / `CLAUDE_PLUGIN_OPTION_AUTH_TOKEN`) is a scoped, ingest-only token, declared `"sensitive": true` in `plugin.json`. Treat it like a password: never commit it, never log it. When entered through the Claude Code dialog it is stored in the OS keychain or `~/.claude/.credentials.json`. Organizations may instead supply it in plain text via managed-settings `pluginConfigs` (see README). A keychain value overrides the managed one.
- **Credentials come only from `userConfig`** (the dialog, `claude plugin install --config`, or `pluginConfigs`), which Claude Code exports to hooks as `CLAUDE_PLUGIN_OPTION_*`. Never read a token that is already on the user's machine (env vars such as `OTEL_EXPORTER_OTLP_HEADERS`, config files, another tool's state) and send it anywhere, not even as a fallback, and do not suggest it in docs or examples. Anthropic's plugin directory holds plugins that do this for manual review ("Uses a credential from the user's machine").
- `internal/filelog` must **never** log content, secrets, or PII. Only IDs and counts are permitted. Grep for `filelog.` calls before adding new log statements.

## Fail-Open Invariant

All hooks are **observability hooks**: they must exit 0 and never block or slow a Claude Code session. This applies in both `release` and `local` binary modes.

- `cmd/on-event/main.go`: `main()` always calls `os.Exit(0)` regardless of errors.
- `scripts/on-event.sh`: every error branch must `exit 0`.

When touching `cmd/`, `scripts/on-event.sh`, or `internal/otlp/export.go`, verify that no new code path can cause a non-zero exit or a hang (the OTLP exporter has a 2-second timeout for this reason).

## Version Sync

Three values **must** agree on every release:

| Source | Field |
|---|---|
| Git tag | `vX.Y.Z` |
| `.claude-plugin/plugin.json` | `"version"` |
| `.claude-plugin/marketplace.json` | plugin entry `"version"` |

**Bump `plugin.json` and `marketplace.json` first, commit, then tag.** The shim derives its binary download target from `plugin.json`'s version. If they drift, consumers silently pin to the wrong release binary.

This is enforced by the version-guard step in `.github/workflows/release.yml`. If the tag does not match the manifests, the release fails before GoReleaser publishes.

## Plugin Name

- The plugin `name` (`fiddler-coding-agent-plugin`, in `plugin.json` and the `marketplace.json` entry) is permanent. Users' `enabledPlugins`, `pluginConfigs`, and saved token are keyed by it. Don't rename it.
- Keep the `renames` entry in `marketplace.json`. It migrates 0.6.x installs from `fiddler-claude-code-plugin`; removing it breaks them.

## Config Resolution

The endpoint, token, and app id have a single source: Claude Code plugin `userConfig` (`OTLP_URL`, `AUTH_TOKEN`, `APP_ID` in `plugin.json`), exported to hooks as `CLAUDE_PLUGIN_OPTION_*`. If any is missing, the hook does nothing (fail open).

Claude Code sets each `CLAUDE_PLUGIN_OPTION_*` value from, highest first: keychain (sensitive dialog values) > managed `pluginConfigs` > `--settings` `pluginConfigs` > user `pluginConfigs` (includes non-sensitive dialog values) > settings `env`. A value in settings `env` only reaches the hook when no stored value exists for that option; a stored value, even an empty one, overrides it. (Based on reading Claude Code v2.1.285; not documented by Anthropic.)

Key traps:

- **Bare token:** `AUTH_TOKEN` must be the bare token value with **no `Bearer ` prefix**. The plugin prepends `Bearer ` itself (`internal/otlp/export.go`). A leading `Bearer ` produces `Authorization: Bearer Bearer ...`.
- **No `OTEL_*` fallback:** the `OTEL_EXPORTER_OTLP_*` / `OTEL_RESOURCE_ATTRIBUTES` fallback was removed in 0.7.0 so the plugin never reads a credential from the user's environment (see Security). Do not reintroduce it; `TestLoad_IgnoresOTelEnv` guards this. (Claude Code also strips all `OTEL_*` variables from hook subprocesses, because it uses them for its own telemetry.)
- **Prompt suppression:** the `userConfig` dialog opens for any option not set in `pluginConfigs` (user, `--settings`, or managed settings) or in the keychain. Values in settings `env` (including `CLAUDE_PLUGIN_OPTION_*`) do not count. To roll out without prompts, set all three options in managed-settings `pluginConfigs["fiddler-coding-agent-plugin@fiddler-plugins"].options`. Claude Code accepts sensitive options there. Observed with Claude Code v2.1.285 using `claude plugin configure --json` against user, `--settings` and managed `pluginConfigs`; not documented by Anthropic and may change.

Source: `internal/config/config.go`.

## The Shim (`scripts/on-event.sh`)

- **No `jq` dependency.** Reads `plugin.json` with `grep`/`sed`. Do not add a `jq` requirement.
- **Reads no config or credential file.** Config reaches the binary only through the hook environment. Do not reintroduce sourcing `.env.local` (or any other file) from the shim.
- Checksum verification on binary download is **fail-closed** (integrity matters), but the overall hook still **fails open** (exit 0 if download fails).
- Concurrent hook invocations (parallel tool calls) are handled via per-invocation staging directories and atomic `mv`. Do not introduce shared mutable state.
- `FIDDLER_BINARY_SOURCE`: `release` (default) downloads from the public GitHub Release; `local` uses `bin/` from `make build`. Unknown values are rejected (no silent fallback to network download).
- **`exec` must not break fail-open.** The shim sets `shopt -s execfail` and `set +e` right before `exec`, so a binary that can't start (wrong architecture, blocked by policy) falls through to `exit 0` instead of bash's 126. `test/shim` covers this.
- **Windows (Git Bash).** `uname -s` reports `MINGW64_NT-…`/`MSYS_NT-…`/`CYGWIN_NT-…`, mapped to `windows` with `EXE=.exe`. Asset: `on-event-windows-<arch>.exe`; cache: `on-event-windows-<arch>-<version>.exe` (version before the extension). macOS/Linux names have no suffix and must stay unchanged so existing caches stay valid. Windows can't replace or delete a running `.exe`: if the install `mv` fails but the versioned binary already exists (a parallel hook won), use it; pruning old versions is best-effort. Skip `chmod` on Windows.
- **Windows ARM64.** Git Bash's tools are x64 programs running under emulation, so `uname -m` reports `x86_64`. Detect ARM64 from the `-ARM64` suffix on `uname -s` (for example `MINGW64_NT-10.0-26100-ARM64`), not from `uname -m`. Older Git for Windows versions without the suffix fall back to the amd64 binary, which still runs under emulation.
- **LF only.** `.gitattributes` forces LF; a CRLF shim fails in Git Bash. Don't remove it. CI checks the checked-out shim on Windows.
- **Quote the plugin root** in `hooks/hooks.json` (`"${CLAUDE_PLUGIN_ROOT}"/scripts/on-event.sh`): Windows profile paths often contain spaces.
- Without Git Bash, Claude Code runs hooks through PowerShell, which can't run the shim. That setup is unsupported (documented in the README).

## Dev / Validate Loop

```bash
make build             # compile to bin/on-event
make build-platform    # compile to bin/on-event-<os>-<arch>
make test              # go test ./...
make lint              # golangci-lint run ./...
make dev               # export .env.local, then claude --plugin-dir . (builds first if FIDDLER_BINARY_SOURCE=local)
```

CI (`.github/workflows/checks.yml`) runs `go mod verify`, `go build`, `go vet` and `go test` on `ubuntu-latest` and `macos-latest` (with `-race`) and on `windows-latest` (x64) and `windows-11-arm` (ARM64), plus `gofmt`, `bash -n` on the shim, and `golangci-lint v2` for both the host and `GOOS=windows`.

`main`'s ruleset requires two checks by exact name: `checks / build & test` and `checks / lint & format`. The per-OS jobs are named `test (<os>)`; `build & test` is a gate job that passes only when all of them pass, so adding or removing a platform needs no ruleset change. Don't rename `build & test` or `lint & format` unless the ruleset is updated at the same time, or merges into `main` will block.

`test/shim` runs the real `scripts/on-event.sh` through bash (Git Bash on Windows; never WSL's `bash.exe`) with a local build and a fake OTLP endpoint. It covers trace export, fail-open exits, an unstartable binary, that the shim picks this machine's binary from the real `uname` (the Windows ARM64 detection on `windows-11-arm`), and (on macOS/Linux, with a fake `uname`) the Windows x64/ARM64 binary names. `FIDDLER_SHIM_RELEASE_TAG=vX.Y.Z go test ./test/shim/` also tests downloading a published release; `release.yml` runs that on all four platforms after every release (`.github/workflows/release-smoke.yml`).

Windows test pitfalls: `os.UserHomeDir` reads `USERPROFILE`, not `HOME` (set both); Windows can't delete a file that is still open, so don't keep files open past a test (`internal/filelog` opens and closes `plugin.log` per write for this reason); `go build -o` doesn't add `.exe`.

**Local testing:**

```bash
cp .env.local.example .env.local   # fill in endpoint / token / app-id
# Uncomment: FIDDLER_BINARY_SOURCE=local
make dev
```

`make dev` exports `.env.local` into its own process (the plugin never reads the file), builds when `FIDDLER_BINARY_SOURCE=local`, and runs `claude --plugin-dir .`. Hooks inherit the exported `CLAUDE_PLUGIN_OPTION_*` values. Without make: `set -a; . ./.env.local; set +a; claude --plugin-dir .`

**Release-path testing:** comment out `FIDDLER_BINARY_SOURCE` in `.env.local`, then `make dev`. The shim downloads the published release matching `plugin.json`'s version, so that release must exist.

## PR and Commit Conventions

- **Squash-merge.** The PR title becomes the commit subject on `main`.
- **Fork PRs:** before approving a fork PR's workflow run, read the full diff including `.github/`. A fork PR can add or modify workflow files.
- **Formatting:** `gofmt` is enforced by CI. Run it before pushing.
- **Linting:** `golangci-lint` with `.golangci.yml` config (errcheck, govet, staticcheck, unused, misspell, ineffassign).
- **Dependencies:** the only external Go dependency is `github.com/stretchr/testify` (test only). Keep it lean; do not add the OTel SDK (the plugin hand-builds OTLP/JSON because spans ship before parents exist).

## Architecture Quick Reference

```
hooks/hooks.json          10 hooks registered (SessionStart .. SessionEnd)
       |
scripts/on-event.sh       shim: locates/downloads binary (.exe on Windows), forwards stdin
       |
cmd/on-event/main.go      entrypoint: config -> adapt -> pipeline (always exit 0)
       |
internal/
  config/                  reads CLAUDE_PLUGIN_OPTION_* (userConfig) only
  source/claudecode/       adapter: Claude Code hook payload -> neutral event.Event
  pipeline/                stateless dispatch: turn lifecycle, tool spans, recovery
  turnctx/                 per-session context file for cross-process trace stitching
  otlp/                    hand-built OTLP/JSON export (2s timeout, 64K attr limit)
  transcript/              JSONL parser for token usage (dedupes by message.id)
  filelog/                 debug logger (IDs/counts only, never content)
```

Single binary, no daemon, no OTel SDK. Each hook invocation is a short-lived process.
