# Update

The updater is implemented in `internal/update` and satisfies
`ports.Updater`. It checks GitHub Releases, verifies the Ed25519 signature of
`release.json`, validates it, verifies the artifact SHA-256 from the signed
manifest, stages the download, replaces the binary atomically, and keeps the
previous binary for rollback.

## Channels

| Channel | Releases |
|---|---|
| `stable` | Non-prerelease releases only. Default. |
| `beta` | Stable and prerelease releases. |

The channel is stored in `brain.yaml` as `update_channel`. `brain update
--channel beta` overrides it for one run. Release selection uses
semantic-version ordering, not upload time.

## Checking and applying

```sh
brain update --check
brain update --yes
brain update --yes --channel beta
brain update --rollback
```

`brain update --check` only reports. `brain update --yes` downloads, verifies,
and applies when a newer release exists. A plain `brain update` without
`--check` or `--yes` behaves like a check and does not change the binary.
`brain update --rollback` restores the previous binary; it cannot be combined
with `--check` or `--yes`.

The updater package honors any truthy `BRAIN_NO_UPDATE` value (`1`, `true`,
`yes`, `on`) for the daily background check:

```powershell
$env:BRAIN_NO_UPDATE = '1'
```

The CLI runs the daily background check before ordinary commands (not `serve`,
`init`, `help`, `version`, or an explicit `update`). It is notify-only: at
most one release lookup per 24 hours, a 3-second timeout, a single-flight lock
file, and no download or apply. A failed lookup records only its attempt time
and backs off for 30 minutes before retrying. A newer release prints one line
to stderr and never changes the exit code. State lives in `last-check.json`
inside the update directory; explicit `brain update` runs ignore
`BRAIN_NO_UPDATE` and always proceed. The updater never mutates the brain data
repository.

## Download locations

Staged artifacts live under:

| OS | Default directory |
|---|---|
| Windows | `%LOCALAPPDATA%\lv24-brain\updates` |
| macOS | `~/Library/Caches/lv24-brain/updates` |
| Linux | `$XDG_CACHE_HOME/lv24-brain/updates` or `~/.cache/lv24-brain/updates` |

Override with `BRAIN_UPDATE_DIR`. A successful apply deletes every staged
download except the one it applied. The previous binary is always stored next to
the executable as `brain.exe.previous` on Windows or `brain.previous` on
macOS/Linux.

## GitHub API rate limit

Release lookups use the GitHub API, which allows 60 unauthenticated requests
per hour per IP; machines behind one address share that budget. A token lifts
the limit (the release repository is public, so any token with no scopes
works). The updater reads, in order: `BRAIN_GITHUB_TOKEN`, `GITHUB_TOKEN`,
`GH_TOKEN`; `brain update` then falls back to the `github/token` OS secret and
the `gh` CLI login, resolved only when it actually contacts GitHub. The token
is sent to API requests only, never to asset downloads; a token GitHub rejects
is dropped and the request retried without it.

## Release manifest

Each GitHub release must contain `release.json`:

```json
{
  "version": "1.2.3",
  "channel": "stable",
  "min_schema_version": 1,
  "published_at": "2026-11-05T12:00:00Z",
  "assets": [
    {
      "kind": "cli",
      "os": "windows",
      "arch": "amd64",
      "name": "brain_1.2.3_windows_amd64.exe",
      "url": "https://github.com/<owner>/lv24-brain/releases/download/v1.2.3/brain_1.2.3_windows_amd64.exe",
      "sha256": "<64 hex characters>"
    }
  ]
}
```

`kind` is `cli` for the engine and `server` for `brain-server`; the updater
never replaces the CLI with a server asset. Every asset must carry `sha256`:
the updater trusts checksums only from the signed manifest and ignores
`checksums_url`, `checksums.txt`, and GitHub asset digests, which come from the
same release unsigned. The inline `signature` field is reserved and ignored.

## Signatures

Each release publishes `release.json.sig` next to `release.json`: a base64
Ed25519 signature over the domain prefix `lv24-brain release manifest v1`
plus a newline, followed by the exact `release.json` bytes. The updater accepts a release only
if the signature verifies against a public key embedded in the running binary
(`trustedReleaseKeys` in `internal/update/signature.go`); an unsigned release,
a signature by any other key, or a tampered manifest is skipped, and a build
with no embedded key refuses to update. Listing several keys allows rotation:
ship a binary trusting old and new keys before signing with the new one.

Maintainer setup (one time, local, $0):

```sh
go run ./internal/tools/releasesign keygen -out <path outside the repo>
```

`keygen` writes only the private seed to a new file and prints only the public
key. Add the public key to `trustedReleaseKeys`, store the seed as the
`BRAIN_RELEASE_SIGNING_KEY` Actions secret (for example
`gh secret set BRAIN_RELEASE_SIGNING_KEY < <file>`), then keep an offline
backup and delete the file. Verify a downloaded release manually with:

```sh
go run ./internal/tools/releasesign verify -in release.json -sig release.json.sig
```

The installers still verify artifacts against `checksums.txt` only; verify the
signature manually for a first install when the download channel is not
trusted.

## Safety rules

- HTTPS release URLs only (plain HTTP is accepted solely for loopback test
  servers).
- Manifest and response sizes are capped; downloads are capped at 256 MiB.
- The manifest version must match the Git tag, and its channel must match the
  GitHub prerelease flag.
- The checksum must match before an artifact can be applied.
- Raw binaries, zip archives, and tar.gz archives are supported. Archives must
  contain `brain` or `brain.exe` (or exactly one regular file).
- Apply keeps `brain(.exe).previous`; a failed replacement leaves the current
  binary untouched.

## Atomic apply and rollback

- macOS/Linux: the staged binary is renamed over the current executable
  (atomic on the same filesystem).
- Windows: a running image cannot be overwritten but can be renamed, so when
  the direct move fails (for example a long-lived `brain mcp` holds the
  executable) the current file is renamed to `.brain.exe-old-<n>` and the new
  binary moved into place at once. Running processes keep the old image; new
  starts get the new one. Aside copies and interrupted `.brain.exe-new-*`
  files are deleted by a later update. If even the rename fails, a detached
  `cmd.exe` helper retries the move for up to 120 seconds.
- Rollback restores `brain(.exe).previous` and keeps that copy for retries.

`brain update --rollback` performs the restore. If the CLI itself cannot start
after a bad update, restore manually while the process is stopped:

```powershell
Stop-Process -Name brain -ErrorAction SilentlyContinue
Copy-Item "$env:LOCALAPPDATA\Programs\lv24-brain\brain.exe.previous" `
  "$env:LOCALAPPDATA\Programs\lv24-brain\brain.exe" -Force
```

```sh
mv "$(command -v brain).previous" "$(command -v brain)"
```

## Server updates

Server auto-update is not implemented. `BRAIN_SERVER_AUTO_UPDATE` is a
reserved placeholder and has no effect in this release. Container deployments
upgrade by pulling or building a new image and roll back by starting the
previous tag or digest:

```sh
docker compose --env-file deploy/.env -f deploy/docker-compose.yml up -d \
  brain-server
```

An update must not run while migrations or active jobs are in progress, and
migrations only run on startup after the backup/health gate.

## Release pipeline

`.github/workflows/release.yml` runs on `v*` tags, builds Windows/macOS/Linux
`amd64`/`arm64` binaries with GoReleaser, writes `checksums.txt`, generates
`release.json`, signs it into `release.json.sig` (the signing step fails when
`BRAIN_RELEASE_SIGNING_KEY` is missing or its public key is not embedded), and
publishes the files with the GitHub CLI. Releases are immutable: the workflow
refuses to touch a tag that already has a release. Only GitHub-hosted
free-tier `ubuntu-latest` runners and the free GitHub Releases storage are used.
No paid runners, storage add-ons, or third-party services are configured.

## Verifying the updater

```sh
go test ./internal/update
```

The tests use `httptest` for GitHub API and asset responses; they cover stable
and beta selection, signature checks (unsigned, untrusted key, tampered,
fallback past a bad release), signed-checksum enforcement and mismatch, staged
downloads, zip and
tar.gz archives, apply, rollback, `BRAIN_NO_UPDATE`, and version ordering.

## Current status

The updater package and the `brain update` command are wired, and release
artifacts are produced by CI. `--check` reports; `--yes` verifies and applies a
newer release; `--rollback` restores the previous binary. The daily background
check is notify-only and runs before ordinary CLI commands. Server auto-update
is not implemented.
