# fiddler-coding-agent-plugin

A coding-agent plugin that captures per-turn OpenTelemetry traces and delivers them to Fiddler. Currently supports Claude Code.

## Requirements

- macOS or Linux (including WSL). Native Windows is not supported.
- Claude Code
- `curl` or `wget` (preinstalled on macOS and most Linux distributions)

## Install

### Claude Code

```
/plugin marketplace add fiddler-labs/fiddler-coding-agent-plugin
/plugin install fiddler-coding-agent-plugin@fiddler-plugins
```

Restart Claude Code after installing.

## Configure

### Claude Code

The plugin needs three options, declared as `userConfig` in `.claude-plugin/plugin.json`:

| Option | Value |
|---|---|
| `OTLP_URL` | Fiddler OTLP/HTTP endpoint URL |
| `APP_ID` | Fiddler application ID |
| `AUTH_TOKEN` | Fiddler ingest-only token: the bare value, **no** `Bearer` prefix |

Claude Code opens a configuration dialog for any option that isn't set when the plugin is installed or enabled from `/plugin`. Provide the values in one of the following ways.

#### Organization rollout (managed settings)

Administrators can pre-supply every option in Claude Code [managed settings](https://code.claude.com/docs/en/managed-settings). Users are then not prompted, and the plugin is installed and enabled automatically:

```json
{
  "extraKnownMarketplaces": {
    "fiddler-plugins": {
      "source": { "source": "github", "repo": "fiddler-labs/fiddler-coding-agent-plugin" }
    }
  },
  "enabledPlugins": {
    "fiddler-coding-agent-plugin@fiddler-plugins": true
  },
  "pluginConfigs": {
    "fiddler-coding-agent-plugin@fiddler-plugins": {
      "options": {
        "OTLP_URL": "https://<your-fiddler-endpoint>",
        "APP_ID": "<your application id>",
        "AUTH_TOKEN": "<your ingest-only token>"
      }
    }
  }
}
```

Default file locations:

- macOS: `/Library/Application Support/ClaudeCode/managed-settings.json`
- Linux/WSL: `/etc/claude-code/managed-settings.json`

If your organization already delivers managed settings from the claude.ai admin console or through MDM, add these keys there instead. By default Claude Code uses only the highest-ranked managed source and ignores `managed-settings.json` when another source is present. To check, run `/status`: the `Setting sources` line names the source in use, and `Skipped sources` lists any it ignored.

Managed values take precedence over user settings. One exception: a token that a user previously entered in the dialog is kept in the OS keychain (or `~/.claude/.credentials.json`), and that stored value overrides the managed one. Have that user uninstall and reinstall the plugin to clear it.

To check the result on a machine, run:

```bash
claude plugin configure fiddler-coding-agent-plugin@fiddler-plugins --json
```

The `unconfigured` list should be empty.

#### Individual users

Either fill in the dialog shown by `/plugin install`, or install from your shell with the values supplied up front. `claude plugin install` never prompts:

```bash
claude plugin install fiddler-coding-agent-plugin@fiddler-plugins \
  --config OTLP_URL=https://<your-fiddler-endpoint> \
  --config APP_ID=<your application id> \
  --config AUTH_TOKEN=<your ingest-only token>
```

To change values later, run `/plugin configure fiddler-coding-agent-plugin@fiddler-plugins` in Claude Code.

> `CLAUDE_PLUGIN_OPTION_*` variables set under `"env"` in `settings.json` still reach the plugin at runtime, but they don't stop the configuration dialog, and any value stored through the dialog or `pluginConfigs` takes precedence over them. The plugin doesn't read `OTEL_EXPORTER_OTLP_*` variables (Claude Code also strips them from hook processes). Prefer `pluginConfigs`.

Restart Claude Code after changing configuration values. Until all three options are set, the plugin does nothing.

### Upgrading from 0.6.x

In 0.7.0 the plugin was renamed from `fiddler-claude-code-plugin` to `fiddler-coding-agent-plugin`. Claude Code moves your `enabledPlugins` and `pluginConfigs` entries (the endpoint and application ID) to the new name automatically. Your saved ingestion token is not moved, so after updating:

1. If Claude Code reports that the plugin is not cached, run `/plugin install fiddler-coding-agent-plugin@fiddler-plugins` once.
2. Re-enter the ingestion token with `/plugin configure fiddler-coding-agent-plugin@fiddler-plugins`.

If your organization supplies the options through managed settings, update the plugin ID in `enabledPlugins` and `pluginConfigs` there. Claude Code doesn't rewrite managed settings.

## What the plugin does

The plugin registers hooks for Claude Code session, prompt, tool, permission, subagent, and stop events. Each hook runs a small script that starts the plugin's binary. The hooks never block or slow your session: if anything fails, the hook exits quietly and the session continues.

### Downloads

On first use of each plugin version, the script downloads the plugin's prebuilt binary for your OS and CPU from this repository's GitHub Releases (`https://github.com/fiddler-labs/fiddler-coding-agent-plugin/releases`) over HTTPS. It verifies the binary's SHA-256 checksum against the release's `checksums.txt` and refuses to run a binary that doesn't match. The verified binary is cached in the plugin's data directory, so it is downloaded once per version. The binary is built from the Go source in this repository.

### Data sent to Fiddler

The plugin sends OpenTelemetry trace data only to the Fiddler endpoint you configure, authenticated with the ingestion token you configure. It sends nothing anywhere else.

Each trace covers one turn of a session and includes:

- **Conversation content:** your prompts, Claude's responses, and the message history sent with each model call.
- **Tool activity:** each tool's name, arguments, and result, plus permission decisions and denial reasons.
- **Usage:** model name, token counts, finish reasons, timings, and errors.
- **Session context:** session ID, working directory, terminal type, how Claude Code was launched, and the Claude Code and plugin versions.
- **Developer identity:** your name and email from `git config` (falling back to the email on your Claude account), plus the hashed user ID, account UUID, and organization UUID that Claude Code stores in `~/.claude.json`. These match the identity attributes in Claude Code's own OpenTelemetry data.

Long content is truncated: most fields are capped at 64 KB, and the message history at about 1 MB per model call.

To leave out your name, email, and account UUID, set `FIDDLER_OMIT_USER_INFO=true` in the environment you start Claude Code from. The hashed user ID and organization UUID are still sent.

To produce token counts and message history, the plugin reads the session transcript file that Claude Code writes for the current session.

### Data kept on your machine

The plugin keeps short-lived state under its plugin data directory (`~/.claude/plugins/data/<plugin-id>/`) to link hook events into one trace. This state includes the current prompt and the inputs of tools that are still running. It is removed when the turn, tool call, or session ends, and leftover entries from interrupted sessions are swept automatically. It also writes a debug log, `plugin.log`, in the same directory. The log contains only IDs and counts, never content, identity, or tokens.

## Development

For working on the plugin itself or testing via `--plugin-dir`:

```bash
git clone git@github.com:fiddler-labs/fiddler-coding-agent-plugin.git
cd fiddler-coding-agent-plugin
cp .env.local.example .env.local   # fill in endpoint / token / app-id
make dev                           # starts Claude Code with the plugin loaded from this checkout
```

`make dev` exports the values in `.env.local` as the same `CLAUDE_PLUGIN_OPTION_*` variables an installed plugin receives, then runs `claude --plugin-dir .`. The plugin never reads `.env.local` itself. Pass extra flags to Claude Code with `make dev ARGS="--debug"`.

By default the shim downloads the released binary from GitHub Releases, so no build step is required.

To build from source, uncomment `FIDDLER_BINARY_SOURCE=local` in `.env.local`. `make dev` then runs `make build` before starting Claude Code. Exit and run `make dev` again to pick up changes.

## Releasing

See [docs/release-runbook.md](docs/release-runbook.md).
