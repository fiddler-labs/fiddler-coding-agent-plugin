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

## Config Resolution

The endpoint, token, and app id have a single source: Claude Code plugin `userConfig` (`OTLP_URL`, `AUTH_TOKEN`, `APP_ID` in `plugin.json`), exported to hooks as `CLAUDE_PLUGIN_OPTION_*`. If any is missing, the hook does nothing (fail open).

Claude Code sets each `CLAUDE_PLUGIN_OPTION_*` value from, highest first: keychain (sensitive dialog values) > managed `pluginConfigs` > `--settings` `pluginConfigs` > user `pluginConfigs` (includes non-sensitive dialog values) > settings `env`. A value in settings `env` only reaches the hook when no stored value exists for that option; a stored value, even an empty one, overrides it. (Based on reading Claude Code v2.1.285; not documented by Anthropic.)

Key traps:

- **Bare token:** `AUTH_TOKEN` must be the bare token value with **no `Bearer ` prefix**. The plugin prepends `Bearer ` itself (`internal/otlp/export.go`). A leading `Bearer ` produces `Authorization: Bearer Bearer ...`.
- **No `OTEL_*` fallback:** the `OTEL_EXPORTER_OTLP_*` / `OTEL_RESOURCE_ATTRIBUTES` fallback was removed in 0.7.0 so the plugin never reads a credential from the user's environment (see Security). Do not reintroduce it; `TestLoad_IgnoresOTelEnv` guards this. (Claude Code also strips all `OTEL_*` variables from hook subprocesses, because it uses them for its own telemetry.)
- **Prompt suppression:** the `userConfig` dialog opens for any option not set in `pluginConfigs` (user, `--settings`, or managed settings) or in the keychain. Values in settings `env` (including `CLAUDE_PLUGIN_OPTION_*`) do not count. To roll out without prompts, set all three options in managed-settings `pluginConfigs["fiddler-claude-code-plugin@fiddler-plugins"].options`. Claude Code accepts sensitive options there. Observed with Claude Code v2.1.285 using `claude plugin configure --json` against user, `--settings` and managed `pluginConfigs`; not documented by Anthropic and may change.

Source: `internal/config/config.go`.

## The Shim (`scripts/on-event.sh`)

- **No `jq` dependency.** Reads `plugin.json` with `grep`/`sed`. Do not add a `jq` requirement.
- **Reads no config or credential file.** Config reaches the binary only through the hook environment. Do not reintroduce sourcing `.env.local` (or any other file) from the shim.
- Checksum verification on binary download is **fail-closed** (integrity matters), but the overall hook still **fails open** (exit 0 if download fails).
- Concurrent hook invocations (parallel tool calls) are handled via per-invocation staging directories and atomic `mv`. Do not introduce shared mutable state.
- `FIDDLER_BINARY_SOURCE`: `release` (default) downloads from the public GitHub Release; `local` uses `bin/` from `make build`. Unknown values are rejected (no silent fallback to network download).

## Dev / Validate Loop

```bash
make build             # compile to bin/on-event
make build-platform    # compile to bin/on-event-<os>-<arch>
make test              # go test ./...
make lint              # golangci-lint run ./...
make dev               # export .env.local, then claude --plugin-dir . (builds first if FIDDLER_BINARY_SOURCE=local)
```

CI (`.github/workflows/checks.yml`) runs: `go mod verify`, `go build`, `go vet`, `go test -race`, `gofmt` check, `golangci-lint v2`.

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
scripts/on-event.sh       shim: locates/downloads binary, forwards stdin
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
