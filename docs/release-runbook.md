# Release Runbook

Step-by-step release process for `fiddler-coding-agent-plugin`.

## Prerequisites

- Push / write access to `fiddler-labs/fiddler-coding-agent-plugin`.
- `gh` CLI installed and authenticated (`gh auth login`).
- A clean, up-to-date `main` branch.

## Release workflow: bump → commit → tag → publish

The version in `.claude-plugin/plugin.json` is the single source of truth.
The shell shim (`scripts/on-event.sh`) reads it at runtime to derive which
GitHub Release asset to download, and Claude Code uses it as the plugin's
update signal. The CI version-guard in `.github/workflows/release.yml` **fails
the release on drift** — it does not auto-correct — so the order below must be
followed exactly.

### 1. Bump the version

Edit both files on `main`:

| File | Field | Example |
|---|---|---|
| `.claude-plugin/plugin.json` | `"version"` | `"0.4.0"` |
| `.claude-plugin/marketplace.json` | `plugins[0].version` | `"0.4.0"` |

The `marketplace.json` version is technically optional (Claude Code uses
`plugin.json`), but if it is present **it must match**. The version-guard
checks this.

### 2. Commit the bump

```bash
git checkout main && git pull origin main
# edit the two files
git add .claude-plugin/plugin.json .claude-plugin/marketplace.json
git commit -m "chore(release): v0.4.0"
git push origin main
```

### 3. Tag and push

```bash
git tag v0.4.0
git push origin v0.4.0
```

The tag **must** be `v<version>` where `<version>` matches the value you set in
step 1. The version-guard rejects tags that are not valid `v<semver>`.

After pushing the tag, check the Actions tab and confirm there is exactly one
Release run for it.

### 4. CI publishes

Pushing the tag triggers `.github/workflows/release.yml`:

1. **Checks job** — runs the full `checks.yml` suite (build, lint, test).
2. **Version-guard step** — extracts the tag version, reads `plugin.json` and
   `marketplace.json`, and fails with an `::error` annotation if any version
   does not match. If this fails, see [Fixing version drift](#fixing-version-drift).
3. **GoReleaser** — builds `on-event-<os>-<arch>` binaries for
   `linux/darwin × amd64/arm64`, generates `checksums.txt` (SHA-256), and
   creates a GitHub Release at the pushed tag.

### 5. Verify the release

```bash
gh release view v0.4.0 --repo fiddler-labs/fiddler-coding-agent-plugin
```

Confirm the release contains:

- `on-event-linux-amd64`
- `on-event-linux-arm64`
- `on-event-darwin-amd64`
- `on-event-darwin-arm64`
- `checksums.txt`

Optionally verify the shim download path end-to-end in a fresh Claude Code
session, or check one asset's checksum manually:

```bash
gh release download v0.4.0 --repo fiddler-labs/fiddler-coding-agent-plugin \
  -p on-event-darwin-arm64 -p checksums.txt
grep ' on-event-darwin-arm64$' checksums.txt | shasum -a 256 -c
```

## Integrity verification

GoReleaser produces a `checksums.txt` file containing SHA-256 hashes of every
release asset. **No code signing or notarization is configured** — integrity
relies on checksums only.

The shim (`scripts/on-event.sh`) enforces this **fail-closed**: it downloads
both the binary and `checksums.txt` from the release, computes the local
SHA-256, and refuses to install the binary if:

- `checksums.txt` could not be downloaded,
- the asset has no entry in `checksums.txt`,
- no `sha256sum` / `shasum` command is available, or
- the computed hash does not match.

This means a corrupted or tampered download is never executed, but the
integrity guarantee depends on the GitHub Release transport (HTTPS), not on a
cryptographic signature tied to a release key.

## Binary download

The shim (`scripts/on-event.sh`) downloads release assets over HTTPS from
`https://github.com/fiddler-labs/fiddler-coding-agent-plugin/releases/download/v<version>/`
using `curl` (or `wget` if curl is missing). Machines running the plugin need
outbound HTTPS access to github.com and the release-asset host it redirects to.

Publish the release immediately after merging a version bump: until
`v<version>` exists, updated installs cannot download a binary (the hook fails
open and sends no telemetry).

## Fixing version drift

If the version-guard step fails, the release was **not published**. Fix the
drift and re-release:

```bash
# 1. Delete the tag (locally and remotely)
git tag -d v0.4.0
git push origin :refs/tags/v0.4.0

# 2. Fix the version in plugin.json and/or marketplace.json
#    (make them match the intended version)

# 3. Commit and push
git add .claude-plugin/plugin.json .claude-plugin/marketplace.json
git commit -m "fix(release): align versions to 0.4.0"
git push origin main

# 4. Re-tag and push
git tag v0.4.0
git push origin v0.4.0
```

GoReleaser is configured with `mode: replace`, so re-publishing to the same
tag overwrites any partial release artifacts.

## Common failure modes

| Symptom | Cause | Fix |
|---|---|---|
| `::error::plugin.json version (X) does not match tag (Y)` | Forgot to bump `plugin.json` before tagging, or bumped to the wrong value. | Delete the tag, fix the version, re-tag (see above). |
| `::error::marketplace.json version for '...' (X) does not match tag (Y)` | `marketplace.json` version is present but was not bumped. | Same fix — bump `marketplace.json` to match, re-tag. |
| `::error::Tag 'vfoo' is not a valid v<semver> release tag` | Tag is not `v<semver>` (e.g. `vfoo`, `release-1`). | Delete the tag, create a proper `v<semver>` tag. |
| Shim reports "no released binary available" | The release does not exist, neither `curl` nor `wget` is installed, or github.com is unreachable (proxy/firewall). | Verify the release exists (`gh release view` or the Releases page); check `curl -I https://github.com`. |
| Shim reports "checksum mismatch" | Corrupted download or stale `checksums.txt`. | Re-download; if persistent, re-publish the release. |

## Version-sync reference

The following locations must agree on the version at release time:

| Location | Field | Used by |
|---|---|---|
| Git tag | `v<semver>` | CI trigger; GoReleaser release name |
| `.claude-plugin/plugin.json` | `"version"` | Shim binary download target; Claude Code update signal |
| `.claude-plugin/marketplace.json` | `plugins[].version` | Marketplace listing (optional but must match if present) |
