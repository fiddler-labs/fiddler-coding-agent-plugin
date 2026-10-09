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
/plugin install fiddler-claude-code-plugin@fiddler-plugins
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
    "fiddler-claude-code-plugin@fiddler-plugins": true
  },
  "pluginConfigs": {
    "fiddler-claude-code-plugin@fiddler-plugins": {
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
claude plugin configure fiddler-claude-code-plugin@fiddler-plugins --json
```

The `unconfigured` list should be empty.

#### Individual users

Either fill in the dialog shown by `/plugin install`, or install from your shell with the values supplied up front. `claude plugin install` never prompts:

```bash
claude plugin install fiddler-claude-code-plugin@fiddler-plugins \
  --config OTLP_URL=https://<your-fiddler-endpoint> \
  --config APP_ID=<your application id> \
  --config AUTH_TOKEN=<your ingest-only token>
```

To change values later, run `/plugin configure fiddler-claude-code-plugin@fiddler-plugins` in Claude Code.

> **Note: migrating from `env`-based config.** `CLAUDE_PLUGIN_OPTION_*` variables set under `"env"` in `settings.json` still reach the plugin at runtime, but they don't stop the configuration dialog, and any value stored through the dialog or `pluginConfigs` takes precedence over them. `OTEL_EXPORTER_OTLP_*` variables are stripped from hook processes by Claude Code and don't work. Prefer `pluginConfigs`.

Restart Claude Code after changing configuration values.

## Development

For working on the plugin itself or testing via `--plugin-dir`:

```bash
git clone git@github.com:fiddler-labs/fiddler-coding-agent-plugin.git
cd fiddler-coding-agent-plugin
cp .env.local.example .env.local   # fill in endpoint / token / app-id
claude --plugin-dir .
```

By default the shim downloads the released binary from GitHub Releases — no build step required.

To build from source, set `FIDDLER_BINARY_SOURCE=local` in `.env.local` and run:

```bash
make build
claude --plugin-dir .
```

Rebuild and restart to pick up changes. See `.env.local.example` for the `OTEL_*` environment variable fallback used in `--plugin-dir` mode.

## Releasing

See [docs/release-runbook.md](docs/release-runbook.md).
