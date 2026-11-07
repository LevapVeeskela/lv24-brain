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

- Windows 10/11 with PowerShell 5.1 or newer; macOS or a current Linux
  distribution with bash 3.2+ and `curl`.
- Git only for `git` mode.
- `sha256sum`, `shasum`, or `openssl` on macOS/Linux for checksum verification.
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
3. Downloads the asset over HTTPS and verifies its SHA-256 checksum. Windows
   PowerShell has no built-in Ed25519, so the installer does not check
   `release.json.sig`; the installed `brain update` verifies every later
   release's signature.
4. Installs `brain.exe` under
   `%LOCALAPPDATA%\Programs\lv24-brain` (override with `-InstallDir`).
5. Writes `brain.yaml`, optionally adds the install directory to the user
   `PATH`, and optionally installs the OpenCode handoff hook. An existing
   `brain.yaml` is kept on reinstall; pass `-OverwriteConfig` to replace it.
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
credential. After installation, sign in with
`printf '%s\n' '<password>' | brain login --config <brain.yaml> --login <user> --password-stdin`.
The command stores the rotating tokens in the OS secret store. Exporting
`BRAIN_TOKEN` or storing a static API token with
`brain secret set server-token` remains supported for automation. OIDC is
browser-based; there is no CLI OIDC flow yet.

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

## macOS and Linux installer

From a checkout of this repository:

```sh
bash installers/unix/install.sh --mode local --yes
```

The script requires bash 3.2 or newer (the macOS default), `curl`, and one of
`sha256sum`, `shasum`, or `openssl`. It runs as the current user and never uses
`sudo`.

The installer:

1. Resolves the newest release on the selected channel (or `--version`).
2. Reads `release.json` and selects the `linux`/`darwin` `amd64`/`arm64` CLI
   asset, falling back to `checksums.txt` when the manifest is absent. When an
   OpenSSL 3 with Ed25519 is available (on PATH or Homebrew; macOS
   `/usr/bin/openssl` is LibreSSL and cannot verify it) the manifest's
   `release.json.sig` is verified and a bad or missing signature stops the
   install. Without such OpenSSL the installer warns and relies on HTTPS and
   SHA-256; set `BRAIN_REQUIRE_SIGNATURE=1` to stop instead.
3. Downloads over HTTPS from `github.com` or `objects.githubusercontent.com`
   and verifies SHA-256 before installing.
4. Installs `brain` to `~/.local/bin` (override with `--dir`), keeps the
   previous binary as `brain.previous`, and prints a `PATH` hint.
5. With `--yes` and `--mode`, runs `brain init`.

Options:

| Option | Meaning |
|---|---|
| `--version vX.Y.Z` | Exact release. |
| `--channel stable\|beta` | Release channel. Default `stable`. |
| `--dir <path>` | Binary directory. Default `~/.local/bin`. |
| `--mode local\|git\|server` | Run `brain init` after install when `--yes` is set. |
| `--data-dir <path>` | Data directory for `local`/`git`. |
| `--server-url <url>` | Server URL for `server` (HTTPS only). |
| `--config <path>` | `brain.yaml` path. |
| `--yes` | Run the init step without prompting. |
| `--no-path-hint` | Do not print PATH instructions. |
| `--help` | Show usage. |

Set a read-only `GITHUB_TOKEN` to raise GitHub API rate limits; the script
sends it only as a request header and never prints or stores it. Re-running the
installer is safe: an identical installed binary is left untouched and keeps
its `brain.previous` rollback copy.

Git mode does not clone. Point `--data-dir` at an existing clone of the private
data repository (default `~/brain-data`), or clone it first.

Verification:

```sh
~/.local/bin/brain version
~/.local/bin/brain doctor --config ~/brain/brain.yaml
```

Server mode prints the next step:

```sh
brain login --config ~/brain/brain.yaml
```

Rollback and uninstall:

```sh
mv ~/.local/bin/brain.previous ~/.local/bin/brain   # rollback
rm ~/.local/bin/brain ~/.local/bin/brain.previous   # uninstall
```

Remove the `PATH` line and delete the data directory only after you have backed
it up.

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
replaces two placeholders:

| Placeholder | Value |
|---|---|
| `__BRAIN_BIN__` | absolute path to `brain.exe` |
| `__BRAIN_CONFIG__` | absolute path to `brain.yaml` |

On session idle (10-minute debounce) and session close (1-minute debounce) it
runs `brain session end --hook opencode`, which writes an `[auto]` handoff only
when something was recorded since the latest handoff and no handoff is younger
than 30 minutes. It never blocks or breaks an OpenCode session. See
`integrations/opencode/README.md` for manual installation and environment
overrides (`BRAIN_BIN`, `BRAIN_CONFIG`).

## Claude Code session hooks

`brain session start|end --hook claude` are Claude Code hook handlers. Start
reads the hook JSON on stdin, resolves the project from its `cwd` and returns
the project context plus what changed since the latest handoff as
`additionalContext`. End writes an `[auto]` handoff from the agent's last
message in the transcript (skipped when a handoff is younger than 30 minutes).
Both are silent for unregistered directories and always exit 0. Add them to
`~/.claude/settings.json` as separate hook groups; SessionEnd needs an explicit
timeout because the default is very short:

```json
{
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "timeout": 10,
        "command": "brain session start --hook claude --config <path-to-brain.yaml>"}]}
    ],
    "SessionEnd": [
      {"hooks": [{"type": "command", "timeout": 10,
        "command": "brain session end --hook claude --config <path-to-brain.yaml>"}]}
    ]
  }
}
```

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
- The CLI runs a notify-only daily update check before ordinary commands (at
  most one release lookup per 24 hours; it never downloads or applies an
  update). `BRAIN_NO_UPDATE=1` disables it. Explicit `brain update` runs ignore
  the switch.
