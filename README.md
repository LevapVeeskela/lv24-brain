# LV24 Brain

Shared project memory for humans and agents: project state, notes, decisions,
attempts, and handoffs in Markdown, with local, Git-backed, and server modes.

- Public engine (this repository): Apache-2.0.
- Private data instance: a separate private data repository, never this repository.
- Product design (source of truth): [docs/product-design.md](docs/product-design.md).

Brain data never lives in this repository. Notes, mail, connector dumps,
journals, indexes, backups, credentials, and machine paths belong in the private
data repository or a local data directory.

## Current status

The engine has moved past the initial scaffold. Implemented and tested today:

| Area | State |
|---|---|
| CLI, local and Git modes | `init`, `open`, `bootstrap`, `note`, `decision`, `attempt`, `handoff`, `status`, `review`, `doctor`, `index [--rebuild]`, `search <query>`, `sync`, `migrate`, `secret`, `update`, `version`, `help` |
| Storage | Markdown + YAML frontmatter (`fsstore`), SQLite FTS5 index, `registry.json` project index |
| Local search | `brain index` refreshes FTS5 from Markdown; `brain index --rebuild` discards and rebuilds it; `brain search` queries it. Both are local/`git` only |
| Git sync | `fetch`, `pull --rebase`, commit, optional push; read-only `--status`; conflict reporting |
| HTTP server | Health/readiness/version, OpenAPI JSON, JWT + rotating refresh tokens, API tokens, local admin bootstrap, OIDC (PKCE/state/nonce), roles `admin\|member\|viewer`, SSE revision stream; Postgres when `BRAIN_DATABASE_URL` is set, in-memory fallback otherwise |
| Server-mode CLI | Token-authenticated HTTP client; token from `BRAIN_TOKEN` or the OS secret store key `server-token`; no `brain login` command |
| Connectors | GitHub, GitLab, and IMAP adapters plus `brain connector run`; Markdown dumps under the data directory; run records are printed but not persisted |
| Auto-update | `brain update --check` and `brain update --yes`; release check, SHA-256 verification, staged apply, previous binary kept for manual rollback; GoReleaser release workflow |
| Installers | Windows PowerShell/BAT installer and optional OpenCode handoff hook |
| Migration | `brain migrate` imports legacy `.brain` knowledge (idempotent, dry-run, path validation), registers projects, and sets `schema_version` |

Known gaps, stated plainly:

- `brain-server` uses Postgres when `BRAIN_DATABASE_URL` (or `DATABASE_URL`) is
  set. Without a database URL it falls back to an in-memory store that loses
  data on restart. Migrations run at startup; an existing schema with pending
  migrations requires `BRAIN_DATABASE_BACKUP_CONFIRMED=1` after a backup.
- Server mode uses a bearer token. There is no `brain login` or CLI OIDC flow
  yet: sign in through `POST /v1/auth/login`, then export `BRAIN_TOKEN` or store
  the token with `brain secret set server-token`.
- `brain sync` is not implemented in server mode; the server API is the sync
  path. `brain index` and `brain search` are local-only; server-mode search uses
  `GET /v1/search`.
- API tokens and refresh-token families are process-memory in the server bridge
  until `api_tokens` / `refresh_tokens` migrations land.
- `brain connector run` writes Markdown dumps but does not persist
  `ConnectorRun` records. Connectors require local or Git mode with `data_dir`.
- Updates are manual (`brain update`); the daily background check is not wired.
  `brain update --rollback` is not exposed; restore `brain.previous` manually.
  Server auto-update is not implemented.
- Only the Windows installer ships. macOS and Linux use release binaries
  installed manually.

## Requirements

- No Go toolchain is needed to run a release binary.
- Go 1.27 or newer builds the engine from source.
- Git is required only for `git` mode and for source checkouts.
- Docker with Compose v2 is optional and only for the server deployment files.

## Quickstart

The full walkthrough with expected output is in
[docs/quickstart.md](docs/quickstart.md).

### Local mode

```sh
go build -o bin/brain ./cmd/brain
go build -o bin/brain-server ./cmd/brain-server
./bin/brain init --mode local --data-dir ~/brain
./bin/brain status --config ~/brain/brain.yaml
./bin/brain note --project demo --title "First note" --body "Local smoke" \
  --config ~/brain/brain.yaml
./bin/brain search "Local smoke" --config ~/brain/brain.yaml
```

`brain search` reads the FTS5 index. New writes update it automatically; run
`brain index` after adding Markdown outside the CLI, or
`brain index --rebuild` to discard stale entries and rebuild from scratch.

Windows PowerShell:

```powershell
go build -o bin\brain.exe .\cmd\brain
go build -o bin\brain-server.exe .\cmd\brain-server
.\bin\brain.exe init --mode local --data-dir "$env:USERPROFILE\brain"
.\bin\brain.exe status --config "$env:USERPROFILE\brain\brain.yaml"
```

### Git mode

```sh
git clone <private-data-repo-url> ~/brain-data
brain init --mode git --data-dir ~/brain-data --config ~/brain-data/brain.yaml
brain sync --status --config ~/brain-data/brain.yaml
```

`brain sync` commits local changes, rebases onto the remote, and pushes only
with `--push`. It never force-pushes. Secrets, indexes, and backups are ignored
by the data repository's `.gitignore`.

### Server mode

```sh
export BRAIN_JWT_SECRET="<at least 32 bytes>"
export BRAIN_BOOTSTRAP_ADMIN_LOGIN="admin"
export BRAIN_BOOTSTRAP_ADMIN_PASSWORD="<strong password>"
# Optional: durable Postgres store. Without it the server stays in memory.
export BRAIN_DATABASE_URL="postgres://brain:<url-encoded-password>@127.0.0.1:5432/brain?sslmode=disable"
./bin/brain-server --addr 127.0.0.1:8080
```

Smoke checks:

```sh
curl -fsS http://127.0.0.1:8080/health
curl -fsS http://127.0.0.1:8080/ready
curl -fsS http://127.0.0.1:8080/version
```

The API accepts a JWT access token or an opaque API token (`brain_…`) as
`Authorization: Bearer <token>`. `GET /openapi.json` describes the surface.

Point the CLI at the server, then supply the token from `POST /v1/auth/login`:

```sh
./bin/brain init --mode server --server-url http://127.0.0.1:8080 \
  --config ~/brain-server.yaml
export BRAIN_TOKEN="<access-token>"
./bin/brain status --config ~/brain-server.yaml
./bin/brain note --project demo --title "First note" --body "Server smoke" \
  --config ~/brain-server.yaml
```

On a desktop, `brain secret set server-token` reads the token from stdin and
stores it in the OS secret store instead of the environment. There is no
`brain login` command yet. See the server-mode caveats above and
[docs/deploy.md](docs/deploy.md) before production use.

## Windows installer

When a release is published, the installer downloads the newest asset on the
selected channel, verifies its SHA-256, installs under
`%LOCALAPPDATA%\Programs\lv24-brain`, and optionally adds it to `PATH`:

```powershell
.\installers\windows\install.ps1 -Mode local -InstallOpenCodeHook
```

Build from source if no release is available yet. Release assets are produced
by `.github/workflows/release.yml` on `v*` tags.

## Documentation

- [Quickstart](docs/quickstart.md)
- [Install](docs/install.md)
- [Update](docs/update.md)
- [Server deploy](docs/deploy.md)
- [Migration from the legacy Company Brain](docs/migration.md)
- [Architecture](docs/architecture.md)
- [Security and privacy](docs/security.md)
- [Product design](docs/product-design.md)

## Security boundary

- Public repository: product code, docs, templates, installers, and tests only.
- Private repository or local data directory: brain content and approved mail
  or connector data.
- Never commit secrets, tokens, passwords, indexes, databases, or backups.
- Connector and server credentials resolve from the environment or the OS
  secret store at runtime; they are never written to `brain.yaml`.

See [docs/security.md](docs/security.md) for the full model.

## License and cost

Apache-2.0. See [LICENSE](LICENSE).

The project is designed for zero paid hosting and CI: GitHub Actions on the
free allowance, GitHub Releases, and self-hosted Docker Compose. No paid
runners, storage add-ons, or marketplace services are required or configured.
