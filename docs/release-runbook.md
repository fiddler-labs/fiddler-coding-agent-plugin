# Release Runbook

Step-by-step release process for `fiddler-coding-agent-plugin`.

## Prerequisites

- A repository admin role on `fiddler-labs/fiddler-coding-agent-plugin`: only
  admins can create tags, and new branches, in this repo.
- `gh` CLI installed and authenticated (`gh auth login`).
- A clean, up-to-date `main` branch.

## Release workflow: bump → commit → tag → publish → pin

The version in `.claude-plugin/plugin.json` is the single source of truth.
The shell shim (`scripts/on-event.sh`) reads it at runtime to derive which
GitHub Release asset to download, and Claude Code uses it as the plugin's
update signal. The CI version-guard in `.github/workflows/release.yml` **fails
the release on drift** — it does not auto-correct — so the order below must be
followed exactly.

### 1. Bump the version

On a branch from an up-to-date `main`, edit `.claude-plugin/plugin.json`:

| File | Field | Example |
|---|---|---|
| `.claude-plugin/plugin.json` | `"version"` | `"0.4.0"` |

`marketplace.json` has no version. It pins the release commit fresh installs
get, and you move it in [step 6](#6-pin-the-marketplace-to-the-new-release)
after the release is out.

### 2. Open the bump PR

Regenerate `checksums.txt` so it matches the binaries this release will build,
commit it alongside the version bump, and open a PR. `main` doesn't accept
direct pushes, and the PR runs the `checksums up to date` check, which
`build & test` requires: a bump with stale hashes can't merge.

```bash
git checkout main && git pull origin main
git checkout -b release/v0.4.0
# edit plugin.json
make checksums
git add .claude-plugin/plugin.json checksums.txt
git commit -m "chore(release): v0.4.0"
git push -u origin release/v0.4.0   # admins; otherwise push to your fork
gh pr create --fill --repo fiddler-labs/fiddler-coding-agent-plugin --base main
```

Without the admin role, open the PR from a fork; an admin then tags the merge
commit in step 3. Squash-merge the PR once it's approved.

### 3. Tag and push

Tag the merge commit on `main`:

```bash
git checkout main && git pull origin main
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
2. **Version-guard step** — extracts the tag version, reads `plugin.json`, and
   fails with an `::error` annotation if its version does not match the tag, or
   if the `marketplace.json` entry sets a `version`. If this fails, see
   [Fixing version drift](#fixing-version-drift).
3. **Checksum gate** — builds the release binaries with the pinned Go toolchain
   (a GoReleaser `--snapshot` build) and fails before publishing if they do not
   match the committed `checksums.txt`. Also fails if `checksums.txt` is missing.
4. **GoReleaser** — builds `on-event-<os>-<arch>` binaries for
   `linux/darwin/windows × amd64/arm64` (Windows assets end in `.exe`) and
   creates a GitHub Release at the pushed tag. A tag with a pre-release suffix
   (`v0.8.0-rc.1`) is published as a GitHub pre-release.
5. **Release smoke** — after GoReleaser publishes, `release-smoke.yml` runs on
   Linux, macOS, Windows x64 and Windows ARM64. It runs the real hook shim in
   release mode (through Git Bash on Windows): it downloads this release's
   binary, verifies it against the committed `checksums.txt`, caches and runs
   the binary, and checks that a trace reaches a local fake endpoint. It doesn't
   start Claude Code itself. The release is already public when this runs; if it
   fails, don't announce the release, fix the problem, and cut the next version.

### 5. Verify the release

```bash
gh release view v0.4.0 --repo fiddler-labs/fiddler-coding-agent-plugin
```

Confirm the release contains:

- `on-event-linux-amd64`
- `on-event-linux-arm64`
- `on-event-darwin-amd64`
- `on-event-darwin-arm64`
- `on-event-windows-amd64.exe`
- `on-event-windows-arm64.exe`
- `checksums.txt`

Confirm the **release smoke** job in the Release run passed on all four
platforms. To re-run it later against any published tag (the workflow file
must be on `main` for manual runs):

```bash
gh workflow run release-smoke.yml --repo fiddler-labs/fiddler-coding-agent-plugin -f tag=v0.4.0
```

Optionally verify the shim download path end-to-end in a fresh Claude Code
session, or check one asset manually against the `checksums.txt` committed at
the tag, which is what the shim checks (not the release's own copy):

```bash
gh release download v0.4.0 --repo fiddler-labs/fiddler-coding-agent-plugin \
  -p on-event-darwin-arm64
git show v0.4.0:checksums.txt | grep ' on-event-darwin-arm64$' | shasum -a 256 -c
```

### 6. Pin the marketplace to the new release

Fresh installs and `claude plugin update` follow the commit pinned in
`.claude-plugin/marketplace.json` (`source.sha`), not `main`. After the
release smoke test passes, open a PR that moves the pin to the tag commit:

```bash
git rev-parse v0.4.0^{commit}   # the commit to pin
```

Set the entry's `ref` to `v0.4.0` and `sha` to that commit. Before merging,
from the PR branch
(`/plugin marketplace add fiddler-labs/fiddler-coding-agent-plugin#<branch>`),
confirm that a fresh install works and that `claude plugin update` moves an
existing install to the new version. Users stay on the previous release until
this PR merges.

## Release candidates

Use a release candidate to test a release, for example a platform change, on
real GitHub release assets before users get it:

1. On the feature branch (not `main`), set `plugin.json` to `0.8.0-rc.1`, run
   `make checksums`, commit `plugin.json` and `checksums.txt`, and push the
   branch.
2. Tag that commit and push the tag:

   ```bash
   git tag v0.8.0-rc.1
   git push origin v0.8.0-rc.1
   ```

   The version guard accepts the pre-release suffix, and GoReleaser publishes
   a GitHub pre-release. Installed plugins are unaffected: they follow the
   commit pinned in `marketplace.json`.
3. Check the release smoke job in that Release run. To try the release in
   Claude Code, check out the branch and run `make dev` (release mode).
4. When it passes, set `plugin.json` to `0.8.0`, merge, and follow the normal
   release steps above.

Treat a pushed tag as final: if `v0.8.0-rc.1` turns out broken, fix it on the
branch and cut `v0.8.0-rc.2`.

## Integrity verification

The expected SHA-256 hash of each release binary is committed to this repository
in `checksums.txt`, generated by `make checksums` with the Go toolchain pinned in
`go.mod`. Because the hash lives in the reviewed plugin commit, that commit
determines exactly which binary may run. **No code signing or notarization is
configured** — integrity relies on these pinned checksums. That includes Windows:
the `.exe` assets carry no Authenticode signature, which strict Windows
application-control policies may block (the README describes this for users).

The shim (`scripts/on-event.sh`) enforces this **fail-closed**: it reads the
expected hash from the plugin's own `checksums.txt`, downloads the binary,
computes the local SHA-256, and refuses to install the binary if:

- `checksums.txt` has no entry for the asset,
- no `sha256sum` / `shasum` command is available, or
- the computed hash does not match.

The shim refuses any binary that does not match the committed `checksums.txt`.
The release workflow also builds the binaries at the tag and refuses to publish
if they do not match it.

## Binary download

The shim (`scripts/on-event.sh`) downloads release assets over HTTPS from
`https://github.com/fiddler-labs/fiddler-coding-agent-plugin/releases/download/v<version>/`
using `curl` (or `wget` if curl is missing). Machines running the plugin need
outbound HTTPS access to github.com and the release-asset host it redirects to.

Publish the release immediately after merging a version bump. The marketplace
pin keeps fresh and updated installs on the previous release, but anything that
loads `main` directly (a `--plugin-dir` checkout, or a directory listing that
tracks `main`) can't download a binary until `v<version>` exists. The hook fails
open and sends no telemetry.

## Fixing version drift

If the version-guard step fails, the release was **not published**. Fix the
drift and re-release.

If the tag can't be replaced, leave it in place and release the next patch
version instead.

```bash
# 1. Delete the tag (locally and remotely)
git tag -d v0.4.0
git push origin :refs/tags/v0.4.0

# 2. On a branch, fix the version in plugin.json (make it match the intended
#    version), and remove any "version" from the marketplace.json entry
git checkout main && git pull origin main
git checkout -b fix/release-v0.4.0

# 3. Commit, push the branch and open a PR (main doesn't accept direct pushes)
git add .claude-plugin/plugin.json .claude-plugin/marketplace.json
git commit -m "fix(release): align versions to 0.4.0"
git push -u origin fix/release-v0.4.0   # admins; otherwise push to your fork
gh pr create --fill --repo fiddler-labs/fiddler-coding-agent-plugin --base main

# 4. After the PR merges, re-tag the merge commit and push
git checkout main && git pull origin main
git tag v0.4.0
git push origin v0.4.0
```

GoReleaser is configured with `mode: replace`, which overwrites partial
artifacts only **before** a release is published. Immutable releases are enabled
on this repo, so once a release is published its assets can't be replaced — if a
published release is broken, cut the next version instead.

## Common failure modes

| Symptom | Cause | Fix |
|---|---|---|
| `::error::plugin.json version (X) does not match tag (Y)` | Forgot to bump `plugin.json` before tagging, or bumped to the wrong value. | Delete the tag, fix the version, re-tag (see above). |
| `::error::marketplace.json entry for '...' sets "version" (X)` | A `version` was added to the pinned marketplace entry. | Remove it, then delete and re-push the tag (see above). |
| `::error::Tag 'vfoo' is not a valid v<semver> release tag` | Tag is not `v<semver>` (e.g. `vfoo`, `release-1`). | Delete the tag, create a proper `v<semver>` tag. |
| Shim reports "no released binary available" | The release does not exist, neither `curl` nor `wget` is installed, or github.com is unreachable (proxy/firewall). | Verify the release exists (`gh release view` or the Releases page); check `curl -I https://github.com`. |
| Shim reports "checksum mismatch" | Corrupted download, or the published release doesn't match the `checksums.txt` users installed. | Re-download. If it persists, don't regenerate `checksums.txt` on `main`; fix the build and cut a new release. |

## Version-sync reference

Release-time locations:

| Location | Field | Used by |
|---|---|---|
| Git tag | `v<semver>` | CI trigger; GoReleaser release name |
| `.claude-plugin/plugin.json` | `"version"` | Shim binary download target; Claude Code update signal |
| `.claude-plugin/marketplace.json` | plugin entry `source.ref` / `source.sha` | Pins the release commit fresh installs get (moved after each release) |
| `checksums.txt` | SHA-256 of the release binaries | Shim verifies the downloaded binary |
