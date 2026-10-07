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
# Windows: Claude Code runs hooks through Git Bash (Git for Windows), which
# reports the OS as MINGW64_NT-…/MSYS_NT-…/CYGWIN_NT-…. Those map to "windows"
# and the binary gets an ".exe" suffix. Without Git Bash, Claude Code runs hooks
# through PowerShell, which cannot run this script (unsupported).
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

# Detect OS and architecture for the correct binary name. EXE is the suffix
# GoReleaser appends to Windows builds; it is empty elsewhere, so macOS/Linux
# asset and cache names are unchanged.
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
EXE=""
WINDOWS_ARM64=false
case "$OS" in
  mingw*|msys*|cygwin*)
    # On Windows ARM64, Git Bash's tools are x64 programs running under
    # emulation, so `uname -m` reports x86_64. The runtime marks the real host
    # with an "-ARM64" suffix on `uname -s` (MINGW64_NT-10.0-26100-ARM64).
    # Older Git for Windows versions without the suffix get the amd64 binary,
    # which still runs under emulation.
    case "$OS" in
      *-arm64) WINDOWS_ARM64=true ;;
    esac
    OS="windows"; EXE=".exe" ;;
esac
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64)  ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
esac
if [ "$WINDOWS_ARM64" = true ]; then
  ARCH="arm64"
fi

ASSET_BASE="on-event-${OS}-${ARCH}"
ASSET="${ASSET_BASE}${EXE}"
BINARY=""
SOURCE="${FIDDLER_BINARY_SOURCE:-release}"

case "$SOURCE" in
  local)
    # --- Local mode: use only a locally built binary, never download ---
    if [ -x "${PLUGIN_ROOT}/bin/${ASSET}" ]; then
      BINARY="${PLUGIN_ROOT}/bin/${ASSET}"
    elif [ -x "${PLUGIN_ROOT}/bin/on-event${EXE}" ]; then
      BINARY="${PLUGIN_ROOT}/bin/on-event${EXE}"
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
      # Version goes before the extension so the cached file still ends in
      # ".exe" on Windows (on-event-windows-amd64-0.8.0.exe).
      VERSIONED_BINARY="${BIN_DIR}/${ASSET_BASE}-${VERSION}${EXE}"

      if [ -x "$VERSIONED_BINARY" ]; then
        BINARY="$VERSIONED_BINARY"
      elif command -v curl &>/dev/null || command -v wget &>/dev/null; then
        # Download from the public GitHub Release.
        if ! mkdir -p "$BIN_DIR" 2>/dev/null; then
          echo "on-event: could not create ${BIN_DIR}; doing nothing" >&2
          exit 0
        fi
        TAG="v${VERSION}"
        BASE_URL="https://github.com/${REPO}/releases/download/${TAG}"

        # Download + verify in a private per-invocation temp dir, then move the
        # verified binary into its versioned path with a single atomic mv. This
        # keeps concurrent hook invocations (Claude Code can fire hooks from
        # parallel tool calls) from racing on a shared download filename: each
        # process works in its own STAGE_DIR and the final mv is atomic, so the
        # worst case is redundant downloads of identical, verified content.
        if ! STAGE_DIR=$(mktemp -d "${BIN_DIR}/.stage.XXXXXX" 2>/dev/null); then
          echo "on-event: could not create a staging directory in ${BIN_DIR}; doing nothing" >&2
          exit 0
        fi
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
            # Windows has no executable bit; skip a no-op that could still fail.
            if [ "$OS" != "windows" ]; then
              chmod +x "${STAGE_DIR}/${ASSET}" || true
            fi
            # Atomic install into the versioned path (same filesystem as BIN_DIR).
            # Windows refuses to replace an .exe that another process is
            # running, which happens when a parallel hook won this race and has
            # already started it. Its file is the same verified build, so use it
            # instead of failing (and instead of tripping `set -e`).
            if mv -f "${STAGE_DIR}/${ASSET}" "$VERSIONED_BINARY" 2>/dev/null || [ -x "$VERSIONED_BINARY" ]; then
              BINARY="$VERSIONED_BINARY"
            else
              echo "on-event: could not install the verified binary into ${BIN_DIR}" >&2
            fi

            # Prune stale cached binaries for this OS/arch from earlier versions.
            # Best effort: on Windows an old .exe still running in another
            # session can't be deleted; a later run removes it.
            for old in "${BIN_DIR}/${ASSET_BASE}"-*; do
              [ -e "$old" ] || continue          # no matches → literal glob
              [ "$old" = "$VERSIONED_BINARY" ] && continue
              rm -f "$old" 2>/dev/null || true
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
#
# A binary can pass the -x test and still fail to start: a wrong-architecture
# file, or (on Windows) an application-control policy blocking it. By default a
# failed exec makes bash exit 126/127, which would break the fail-open
# invariant. execfail keeps the shell alive after a failed exec, and `set +e`
# stops `set -e` from exiting first, so the line after exec runs only on failure.
shopt -s execfail
set +e
exec "$BINARY" "$@"
echo "on-event: could not execute ${BINARY}; doing nothing" >&2
exit 0
