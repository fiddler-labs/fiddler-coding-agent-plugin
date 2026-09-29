// Command on-event is the hook binary for the Fiddler coding-agent plugin.
//
// Claude Code spawns this as a short-lived process on each hook event,
// with the event name as argv[1] (set per hook in hooks.json) and the
// event payload JSON on stdin. The binary resolves config, adapts the
// payload into a neutral event, and dispatches it to the pipeline.
//
// Exit code is always 0 (fail open) — the plugin must never block or slow
// the developer's session.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/config"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/filelog"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/identity"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/pipeline"
	"github.com/fiddler-labs/fiddler-coding-agent-plugin/internal/source/claudecode"
)

func main() {
	if err := run(); err != nil {
		filelog.Error("on-event: %v", err)
	}
	// Always exit 0 — fail open.
	os.Exit(0)
}

func run() error {
	// The hook event name is passed as argv[1] by hooks.json.
	eventName := ""
	if len(os.Args) > 1 {
		eventName = os.Args[1]
	}
	if eventName == "" {
		filelog.Warn("no event name in argv")
		return nil
	}

	cfg := config.Load()
	cfg.AgentName = claudecode.AgentName
	cfg.Provider = claudecode.Provider
	cfg.ServiceName = claudecode.ServiceName

	// Resolve locally-sourced identity/workspace attributes (best-effort) so
	// spans carry the same identity signal as Claude Code's native OTEL
	// resource. Empty fields are omitted downstream.
	id := identity.Resolve()
	cfg.UserID = id.UserID
	cfg.OrgID = id.OrgID
	cfg.AccountUUID = id.AccountUUID
	cfg.UserName = id.Name
	cfg.UserEmail = id.Email
	cfg.IdentitySource = id.Source
	cfg.AppEntrypoint = id.Entrypoint
	cfg.TerminalType = id.TerminalType
	cfg.Cwd = id.Cwd

	if !cfg.Valid() {
		filelog.Warn("config invalid: endpoint=%t apikey=%t appid=%t",
			cfg.Endpoint != "", cfg.APIKey != "", cfg.AppID != "")
		return nil
	}

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	if len(raw) == 0 {
		filelog.Warn("empty stdin")
		return nil
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("parsing JSON from stdin: %w", err)
	}

	ev, ok := claudecode.Adapt(eventName, payload)
	if !ok {
		filelog.Info("unhandled event: %s", eventName)
		return nil
	}

	// Stamp the observed Claude Code version (app.version resource attribute),
	// read from the session transcript — the hook payload does not carry it.
	// Best-effort: empty when the transcript is unavailable, then omitted.
	cfg.AppVersion = claudecode.ReadAgentVersion(payload)

	filelog.Info("event received: %s session=%s", eventName, ev.SessionID)

	pipeline.Process(ev, cfg)
	return nil
}
