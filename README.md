# fiddler-coding-agent-plugin

A coding-agent plugin that captures per-turn OpenTelemetry traces and delivers them to Fiddler. Currently supports Claude Code.

## Requirements

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

Add the following to your `settings.json` or `settings.local.json` under `"env"`:

```json
{
  "env": {
    "CLAUDE_PLUGIN_OPTION_OTLP_URL": "https://<your-fiddler-endpoint>/",
    "CLAUDE_PLUGIN_OPTION_APP_ID": "<your application id>",
    "CLAUDE_PLUGIN_OPTION_AUTH_TOKEN": "<your API key>"
  }
}
```

`AUTH_TOKEN` is the bare token value — do **not** add a `Bearer` prefix.

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
