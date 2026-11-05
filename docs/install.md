# Install

LV24 Brain runs as a single Go binary on Windows, macOS, and Linux. Choose one
of three modes:

| Mode | Storage | Sync | Who it is for |
|---|---|---|---|
| `local` | Markdown files on this machine | none | one person |
| `git` | Markdown files in a private Git repository | pull/rebase/commit/push | a team (default) |
| `server` | team brain server + Postgres | HTTP API + SSE | a central team server |

The binary and installers live in the public product repository. Brain data
lives in a separate private repository or data directory. Never put secrets,
tokens, passwords, `secrets/`, `index/`, or `backups/` into a brain data
repository.

## Requirements

- Windows 10/11 with PowerShell 5.1 or newer.
- Git for Windows only for `git` mode.
- Go 1.27+ only when building from source.
- An OpenCode installation only if you want the optional handoff hook.

## Windows installer

From a checkout of this repository:

```powershell
.\installers\windows\install.ps1 -Mode local -InstallOpenCodeHook
```

Or use the batch wrapper:

```bat
installers\windows\install.bat -Mode local -InstallOpenCodeHook
```

The installer:

1. Resolves the newest release on the selected channel (or `-Version`).
2. Reads `release.json` and selects the Windows `amd64`/`arm64` CLI asset.
3. Downloads the asset over HTTPS and verifies its SHA-256 checksum.
4. Installs `brain.exe` under
   `%LOCALAPPDATA%\Programs\lv24-brain` (override with `-InstallDir`).
5. Writes `brain.yaml`, optionally adds the install directory to the user
   `PATH`, and optionally installs the OpenCode handoff hook.
6. Never asks for, writes, or logs secrets.

### Install modes

Local mode:

```powershell
.\installers\windows\install.ps1 -Mode local
```

Git mode with a private data repository:

```powershell
.\installers\windows\install.ps1 `
  -Mode git `
  -RepoUrl git@github.com:your-org/your-brain-data.git
```

The installer clones only when the target data directory is absent or empty.
Use an SSH key or the Git credential helper. Never embed an access token in the
repository URL.

Server mode:

```powershell
.\installers\windows\install.ps1 `
  -Mode server `
  -ServerUrl https://brain.example.com
```

The installer writes `server_url` to `brain.yaml` and never creates or stores a
credential. After installation, obtain a token from `POST /v1/auth/login` (or
mint an API token), then either export `BRAIN_TOKEN` or store the token with
`brain secret set server-token`. There is no `brain login` or OIDC CLI flow yet.

Local and Git modes write a `data_dir`; server mode writes only `server_url`.

### Installer options

| Option | Meaning |
|---|---|
| `-Mode local\|git\|server` | Install mode. Prompted when omitted unless `-Yes`. |
| `-DataDir <path>` | Data directory. Defaults to `%USERPROFILE%\brain` (local) or `%USERPROFILE%\brain-data` (git). |
| `-RepoUrl <url>` | Private data repository for `git` mode. |
| `-ServerUrl <url>` | Brain server URL for `server` mode. |
| `-InstallDir <path>` | Binary directory. |
| `-Channel stable\|beta` | Release channel recorded in `brain.yaml`. |
| `-Version <version>` | Exact release, or `latest`. |
| `-SourceBinary <path>` | Install a local binary instead of downloading. |
| `-Sha256 <hex>` | Expected checksum for `-SourceBinary` or an override. |
| `-InstallOpenCodeHook` | Install the OpenCode handoff plugin. |
| `-NoPath` | Do not modify the user `PATH`. |
| `-SkipChecksum` | Explicitly skip checksum verification (not recommended). |
| `-Yes` | Non-interactive; defaults to `local` when `-Mode` is omitted. |

## Manual install

1. Download the release asset for your OS/architecture from GitHub Releases.
2. Download `checksums.txt` and verify the file:

   Windows:

   ```powershell
   (Get-FileHash .\brain_<version>_windows_amd64.exe -Algorithm SHA256).Hash
   ```

   macOS/Linux:

   ```sh
   grep 'brain_<version>_<os>_<arch>$' checksums.txt
   shasum -a 256 brain_<version>_<os>_<arch>
   ```

3. Place the binary on `PATH` (for example
   `%LOCALAPPDATA%\Programs\lv24-brain\brain.exe` on Windows or
   `~/.local/bin/brain` on macOS/Linux) and make it executable.
4. Create an instance:

   ```sh
   brain init --mode local --data-dir <brain-data> --config <brain-data>/brain.yaml
   ```

   For `server` mode use `--mode server --server-url <url>` and
   `--config <config-path>` instead of `--data-dir`; for `git` mode use
   `--mode git` and point `--data-dir` at the clone of the private data
   repository. The generated `brain.yaml` records `mode`, `data_dir`
   (local/git), `server_url` (server), `schema_version`, and `update_channel`.

5. Run `brain status --config <path-to-brain.yaml>`.

6. For server mode, obtain a token from `POST /v1/auth/login` and set
   `BRAIN_TOKEN` or store it with `brain secret set server-token`.

## OpenCode handoff hook

The hook template is `integrations/opencode/brain-handoff.js`. The installer
copies it to `%USERPROFILE%\.config\opencode\plugins\brain-handoff.js` and
replaces three placeholders:

| Placeholder | Value |
|---|---|
| `__BRAIN_BIN__` | absolute path to `brain.exe` |
| `__BRAIN_CONFIG__` | absolute path to `brain.yaml` |
| `__HANDOFF_SUMMARY__` | short handoff summary |

It appends a handoff on session idle (10-minute debounce) and on session close
(1-minute debounce), and never blocks or breaks an OpenCode session. See
`integrations/opencode/README.md` for manual installation and environment
overrides (`BRAIN_BIN`, `BRAIN_CONFIG`, `BRAIN_HANDOFF_SUMMARY`).

## Updating and uninstalling

- Check for updates with `brain update --check`; see [update.md](update.md).
- To uninstall, remove the binary directory, remove its entry from the user
  `PATH`, remove the OpenCode hook, and delete the local data directory only
  after you have backed it up. Deleting data is destructive and is never done
  automatically by the installer.

## Troubleshooting

- `brain: command not found` after install: open a new terminal; the installer
  updates the user `PATH`, not already-open shells.
- `SHA-256 mismatch`: the download is corrupt or the release is not trusted.
  Do not run it; retry or report the release.
- `Could not replace brain.exe`: stop a running `brain` process and retry.
- `BRAIN_NO_UPDATE` only affects background update checks. No background check
  ships yet, and explicit `brain update` runs ignore the switch.
