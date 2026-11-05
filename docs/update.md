# Update

The updater is implemented in `internal/update` and satisfies
`ports.Updater`. It checks GitHub Releases, validates `release.json`, verifies
SHA-256, stages the download, replaces the binary atomically, and keeps the
previous binary for rollback.

## Channels

| Channel | Releases |
|---|---|
| `stable` | Non-prerelease releases only. Default. |
| `beta` | Stable and prerelease releases. |

The channel is stored in `brain.yaml` as `update_channel`. `brain update
--channel beta` overrides it for one run once the CLI update use case is
wired. Release selection uses semantic-version ordering, not upload time.

## Checking and applying

```sh
brain update --check
brain update --yes
brain update --yes --channel beta
```

The daily background check is disabled by any truthy `BRAIN_NO_UPDATE` value
(`1`, `true`, `yes`, `on`):

```powershell
$env:BRAIN_NO_UPDATE = '1'
```

Explicit update runs can bypass the environment switch through the updater's
`IgnoreDisabled` option. The updater never mutates the brain data repository.

## Download locations

Staged artifacts live under:

| OS | Default directory |
|---|---|
| Windows | `%LOCALAPPDATA%\lv24-brain\updates` |
| macOS | `~/Library/Caches/lv24-brain/updates` |
| Linux | `$XDG_CACHE_HOME/lv24-brain/updates` or `~/.cache/lv24-brain/updates` |

Override with `BRAIN_UPDATE_DIR`. The previous binary is always stored next to
the executable as `brain.exe.previous` on Windows or `brain.previous` on
macOS/Linux.

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
      "url": "https://github.com/LevapVeeskela/lv24-brain/releases/download/v1.2.3/brain_1.2.3_windows_amd64.exe",
      "sha256": "<64 hex characters>"
    }
  ]
}
```

`kind` is `cli` for the engine and `server` for `brain-server`; the updater
never replaces the CLI with a server asset. `checksums_url` is an optional
fallback when an asset omits `sha256`. `signature` metadata is reserved for a
future cosign/Sigstore verification step; v1 verifies SHA-256 only.

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
- Windows: when the target is the running executable, a detached `cmd.exe`
  helper waits up to 120 seconds and performs the move after the process
  exits. The update takes effect on the next start.
- Rollback restores `brain(.exe).previous` and keeps that copy for retries.

Until the CLI exposes `brain update --rollback`, restore manually after
stopping the process:

```powershell
Stop-Process -Name brain -ErrorAction SilentlyContinue
Copy-Item "$env:LOCALAPPDATA\Programs\lv24-brain\brain.exe.previous" `
  "$env:LOCALAPPDATA\Programs\lv24-brain\brain.exe" -Force
```

```sh
mv "$(command -v brain).previous" "$(command -v brain)"
```

## Server updates

Server auto-update is opt-in with `BRAIN_SERVER_AUTO_UPDATE=1`; the default is
notify-only. Container deployments should pin an image tag or digest and roll
back by starting the previous digest:

```sh
docker compose --env-file deploy/.env -f deploy/docker-compose.yml up -d \
  brain-server
```

An update must not run while migrations or active jobs are in progress, and
migrations only run on startup after the backup/health gate.

## Release pipeline

`.github/workflows/release.yml` runs on `v*` tags, builds Windows/macOS/Linux
`amd64`/`arm64` binaries with GoReleaser, writes `checksums.txt`, generates
`release.json`, and publishes the files with the GitHub CLI. Only GitHub-hosted
free-tier `ubuntu-latest` runners and the free GitHub Releases storage are used.
No paid runners, storage add-ons, or third-party services are configured.

## Verifying the updater

```sh
go test ./internal/update
```

The tests use `httptest` for GitHub API and asset responses; they cover stable
and beta selection, checksum fallback and mismatch, staged downloads, zip and
tar.gz archives, apply, rollback, `BRAIN_NO_UPDATE`, and version ordering.

## Current scaffold status

The updater package is complete, and release artifacts are produced by CI. The
`brain update` CLI use case and background scheduler are wired in a later work
package; until then the command reports the typed `not implemented` error.
Manual `.previous` restoration remains the supported rollback path.
