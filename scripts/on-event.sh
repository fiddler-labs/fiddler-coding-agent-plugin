#!/usr/bin/env bash
# Shell shim that locates and executes the on-event Go binary.
#
# Binary source is controlled by FIDDLER_BINARY_SOURCE (default: release):
#
#   release  Download the released binary from the public GitHub Release over
#            HTTPS (curl, or wget if curl is unavailable). Cached in
#            $CLAUDE_PLUGIN_DATA/bin/ so it is downloaded only once per version.
#
#   local    Use only a locally built binary (make build / make build-platform).
#            Export this in the shell you start Claude Code from when
#            developing the plugin.
#
# Both modes fail open (exit 0) — a hook never blocks the session.
#
# The shim reads no config or credential file. Plugin config (endpoint, token,
# app id) reaches the binary only through the hook environment, as the
# CLAUDE_PLUGIN_OPTION_* variables Claude Code exports from userConfig.

set -euo pipefail

PLUGIN_ROOT="${CLAUDE_PLUGIN_ROOT:?CLAUDE_PLUGIN_ROOT not set}"
REPO="fiddler-labs/fiddler-coding-agent-plugin"

# download URL DEST: fetch URL over HTTPS into DEST. Prefers curl and falls
# back to wget. Returns non-zero on any failure (HTTP error such as 404,
# timeout, or neither tool installed), so callers can fail open.
download() {
  if command -v curl &>/dev/null; then
    curl -fsSL --proto '=https' --connect-timeout 10 --max-time 45 -o "$2" "$1"
  elif command -v wget &>/dev/null; then
    wget -q --timeout=20 --tries=2 -O "$2" "$1"
  else
    return 1
  fi
}

# Detect OS and architecture for the correct binary name.
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  ARCH="amd64" ;;
  aarch64) ARCH="arm64" ;;
  arm64)   ARCH="arm64" ;;
esac

ASSET="on-event-${OS}-${ARCH}"
BINARY=""
SOURCE="${FIDDLER_BINARY_SOURCE:-release}"

case "$SOURCE" in
  local)
    # --- Local mode: use only a locally built binary, never download ---
    if [ -x "${PLUGIN_ROOT}/bin/${ASSET}" ]; then
      BINARY="${PLUGIN_ROOT}/bin/${ASSET}"
    elif [ -x "${PLUGIN_ROOT}/bin/on-event" ]; then
      BINARY="${PLUGIN_ROOT}/bin/on-event"
    else
      echo "on-event: FIDDLER_BINARY_SOURCE=local but no local build found (run make build)" >&2
      exit 0
    fi
    ;;

  release)
    # --- Release mode (default): use the released binary ---

    # Read version from plugin.json (single source of truth).
    # grep/sed only — no jq dependency. plugin.json is our own controlled file.
    MANIFEST="${PLUGIN_ROOT}/.claude-plugin/plugin.json"
    VERSION=""
    if [ -f "$MANIFEST" ]; then
      VERSION=$(grep '"version"' "$MANIFEST" | sed 's/.*: *"\(.*\)".*/\1/' || true)
    fi

    if [ -n "$VERSION" ] && [ -n "${CLAUDE_PLUGIN_DATA:-}" ]; then
      BIN_DIR="${CLAUDE_PLUGIN_DATA}/bin"
      VERSIONED_BINARY="${BIN_DIR}/${ASSET}-${VERSION}"

      if [ -x "$VERSIONED_BINARY" ]; then
        BINARY="$VERSIONED_BINARY"
      elif command -v curl &>/dev/null || command -v wget &>/dev/null; then
        # Download from the public GitHub Release.
        mkdir -p "$BIN_DIR"
        TAG="v${VERSION}"
        BASE_URL="https://github.com/${REPO}/releases/download/${TAG}"

        # Download + verify in a private per-invocation temp dir, then move the
        # verified binary into its versioned path with a single atomic mv. This
        # keeps concurrent hook invocations (Claude Code can fire hooks from
        # parallel tool calls) from racing on a shared download filename: each
        # process works in its own STAGE_DIR and the final mv is atomic, so the
        # worst case is redundant downloads of identical, verified content.
        STAGE_DIR=$(mktemp -d "${BIN_DIR}/.stage.XXXXXX")
        trap 'rm -rf "$STAGE_DIR"' EXIT

        if download "${BASE_URL}/${ASSET}" "${STAGE_DIR}/${ASSET}"; then

          # Fail closed: install only if the checksum is present AND verified.
          # If checksums.txt can't be downloaded, has no entry for this asset,
          # or no sha256 tool is available, we do NOT install the binary —
          # integrity is the whole point of this step.
          CHECKSUM_OK=false
          if download "${BASE_URL}/checksums.txt" "${STAGE_DIR}/checksums.txt"; then

            # `|| true`: a missing entry must not trip `set -e` (fail open).
            EXPECTED=$(grep "  ${ASSET}$" "${STAGE_DIR}/checksums.txt" | cut -d' ' -f1 || true)
            ACTUAL=""
            if command -v sha256sum &>/dev/null; then
              ACTUAL=$(sha256sum "${STAGE_DIR}/${ASSET}" | cut -d' ' -f1)
            elif command -v shasum &>/dev/null; then
              ACTUAL=$(shasum -a 256 "${STAGE_DIR}/${ASSET}" | cut -d' ' -f1)
            fi

            if [ -z "$EXPECTED" ]; then
              echo "on-event: no checksum entry for ${ASSET} in checksums.txt; not installing" >&2
            elif [ -z "$ACTUAL" ]; then
              echo "on-event: no sha256sum/shasum available to verify checksum; not installing" >&2
            elif [ "$ACTUAL" != "$EXPECTED" ]; then
              echo "on-event: checksum mismatch (expected ${EXPECTED}, got ${ACTUAL}); not installing" >&2
            else
              CHECKSUM_OK=true
            fi
          else
            echo "on-event: could not download checksums.txt; not installing (fail-closed)" >&2
          fi

          if [ "$CHECKSUM_OK" = true ]; then
            chmod +x "${STAGE_DIR}/${ASSET}"
            # Atomic install into the versioned path (same filesystem as BIN_DIR).
            mv -f "${STAGE_DIR}/${ASSET}" "$VERSIONED_BINARY"
            BINARY="$VERSIONED_BINARY"

            # Prune stale cached binaries for this OS/arch from earlier versions.
            for old in "${BIN_DIR}/${ASSET}"-*; do
              [ -e "$old" ] || continue          # no matches → literal glob
              [ "$old" = "$VERSIONED_BINARY" ] && continue
              rm -f "$old"
            done
          fi
        fi

        rm -rf "$STAGE_DIR"
        trap - EXIT
      fi
    fi

    if [ -z "$BINARY" ]; then
      echo "on-event: no released binary available for v${VERSION:-?} (download failed or could not be verified; see any message above. Requires curl or wget and HTTPS access to github.com)" >&2
      exit 0
    fi
    ;;

  *)
    # Unknown source value: do NOT silently fall back to a network download.
    # A typo like FIDDLER_BINARY_SOURCE=locall must not flip a developer from
    # "local only" into "fetch from GitHub". Fail open (never block the hook).
    echo "on-event: unknown FIDDLER_BINARY_SOURCE='${SOURCE}' (expected 'release' or 'local'); doing nothing" >&2
    exit 0
    ;;
esac

# Forward stdin (the hook payload JSON) to the binary.
exec "$BINARY" "$@"
