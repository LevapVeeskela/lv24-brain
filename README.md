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
| CLI | `init`, `doctor`, `open`, `project list\|show\|set`, `bootstrap`, `note`, `decision`, `attempt`, `handoff`, `status`, `review`, `sync`, `index [--rebuild]`, `search <query>`, `journal`, `event commit`, `hooks`, `config`, `discover`, `graphs`, `agents sync [--check]`, `state get|set`, `mcp` (stdio MCP server for agents), `connector run`, `migrate [--rollback]`, `secret`, `update`, `serve`, `version`, `help`; `login` and `logout` in server mode |
| Storage | Markdown + YAML frontmatter (`fsstore`), SQLite FTS5 index, `registry.json` project index |
| Local search | `brain index` refreshes FTS5 from Markdown; `brain index --rebuild` discards and rebuilds it; `brain search` queries it in local/`git` mode. `brain index` is local-only; `brain search` also works in server mode through `GET /v1/search` |
| Git sync | `fetch`, `pull --rebase`, commit, optional push; read-only `--status`; conflict reporting |
| HTTP server | Health/readiness/version, OpenAPI JSON, JWT + rotating refresh tokens, API tokens, durable auth tables (migrations `0002_auth_tokens.sql` and `0003_auth_revocations.sql`, server schema 6: `0004_security_audit.sql`, `0005_knowledge_owner.sql`, `0006_api_token_expiry.sql`), security audit log (`GET /v1/audit`, admin), local admin bootstrap, OIDC (PKCE/state/nonce), roles `admin\|member\|viewer`, SSE revision stream; Postgres when `BRAIN_DATABASE_URL` is set, in-memory fallback otherwise |
| Server-mode CLI | `brain login` / `brain logout`; one atomic OS secret-store bundle `server-session` (server URL, access token, refresh token) bound to the configured `server_url`; `server-token` remains an explicit static API-token fallback; the HTTP client refreshes once on `401` and retries; `BRAIN_TOKEN` overrides stored credentials |
| Connectors | GitHub, GitLab, and IMAP adapters plus `brain connector run`; local/`git` writes Markdown dumps and run records under the data directory. Server mode runs through `POST /v1/connectors/run`, stores run records in PostgreSQL, and exports each run as Markdown at `GET /v1/connectors/runs/{id}/markdown` |
| Auto-update | `brain update --check`, `--yes`, and `--rollback`; notify-only daily check; release check, Ed25519-signed manifest, SHA-256 verification, staged apply, previous binary kept; GoReleaser release workflow |
| Installers | Windows PowerShell/BAT installer (with optional OpenCode handoff hook) and macOS/Linux bash installer |
| Migration | `brain migrate` imports legacy `.brain` knowledge (idempotent, dry-run, path validation), registers projects, and sets `schema_version`; every import writes a backup manifest and `brain migrate --rollback <manifest.json \| latest>` reverts it (edited files are kept) |

API tokens and refresh-token families are durable in Postgres (migrations
`0002_auth_tokens.sql` and `0003_auth_revocations.sql`; the server schema is
version 6 after `0006_api_token_expiry.sql`), so they survive restarts and refresh rotation still revokes the whole family on
reuse. Revocation tombstones have no expiry, so pruning expired token rows
cannot resurrect a revoked family or session. Without
`BRAIN_DATABASE_URL`, `brain-server` still falls back to an in-memory store that
loses all data on restart; migrations run at startup, and an existing schema
with pending migrations requires `BRAIN_DATABASE_BACKUP_CONFIRMED=1` after a
backup. Multi-replica deployment remains unsupported even with durable tokens:
the SSE broker and login rate limiter are per-process.

Known gaps, stated plainly:

- Server auto-update is not implemented; `BRAIN_SERVER_AUTO_UPDATE` is a
  reserved placeholder and deployments upgrade by replacing the image.
- Server-side connector runs persist the run record, status, counts and
  sanitized error in PostgreSQL, but discard the bulk connector Markdown dumps.
  The per-run Markdown export contains the run record, not the dump content.
  IMAP server runs are stateless unless `BRAIN_IMAP_STATE` is set.
- `brain sync` is not implemented in server mode; the server API is the sync
  path.
- `brain index` is local-only. `brain search` works in server mode through
  `GET /v1/search`.

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

Point the CLI at the server and log in with the bootstrap administrator (or
another local account):

```sh
./bin/brain init --mode server --server-url http://127.0.0.1:8080 \
  --config ~/brain-server.yaml
printf '%s\n' '<admin password>' | ./bin/brain login --config ~/brain-server.yaml \
  --login admin --password-stdin
./bin/brain status --config ~/brain-server.yaml
./bin/brain note --project demo --title "First note" --body "Server smoke" \
  --config ~/brain-server.yaml
./bin/brain connector run --name github --config ~/brain-server.yaml
./bin/brain logout --config ~/brain-server.yaml
```

`brain login` is server-mode only. `--login` falls back to `BRAIN_LOGIN`; the
password is read from stdin with `--password-stdin` or from `BRAIN_PASSWORD`.
There is no `--password` flag, so the password never appears in the
`brain login` arguments. The command stores one JSON
credential bundle under the OS secret-store key `server-session`
(`serverUrl`, `accessToken`, `refreshToken`) in a single write, bound to the
normalized `server_url`. The HTTP client refreshes once on `401` and retries; a
login-managed bundle is never sent to a different server URL, and a mismatched
binding is refused with both URLs named. `brain logout` checks
`GET /v1/auth/me` before rotating anything, revokes the JWT session and its
refresh family, and clears the bundle. The refresh family is revoked
immediately; an access JWT that was already issued stays valid until its short
TTL.

If a static API token is also stored it cannot be revoked by the CLI: logout
still revokes the login-managed family, clears local credentials, warns, and
exits non-zero. After logout the refresh family is gone, so no new access token
can be minted. The old three-key layout is no longer used as a login pair:
`server-refresh-token` and `server-auth-url` are ignored, and only
`server-token` remains as an explicit static API-token fallback with no refresh
and no binding. A legacy unbound JWT pair is not refreshed anymore; run
`brain login` again.

For automation, `BRAIN_TOKEN` still works and takes precedence over stored
credentials; while it is set, `brain login` refuses to run and `brain logout`
leaves a stored session untouched and exits non-zero. A static API token can
also be stored with
`printf '%s\n' '<token>' | brain secret set server-token`. See
[docs/deploy.md](docs/deploy.md) before production use.

## Installers

When a release is published, the Windows installer downloads the newest asset on
the selected channel, verifies its SHA-256, installs under
`%LOCALAPPDATA%\Programs\lv24-brain`, and optionally adds it to `PATH`:

```powershell
.\installers\windows\install.ps1 -Mode local -InstallOpenCodeHook
```

On macOS and Linux, `installers/unix/install.sh` reads `release.json` or
`checksums.txt`, verifies SHA-256, installs `brain` under `~/.local/bin`, and
keeps `brain.previous` for rollback:

```sh
./installers/unix/install.sh --mode local --yes
```

Build from source if no release is available yet. Release assets are produced
by `.github/workflows/release.yml` on `v*` tags. See
[docs/install.md](docs/install.md) for installer options.

## Documentation

- [Quickstart](docs/quickstart.md)
- [Install](docs/install.md)
- [Update](docs/update.md)
- [Server deploy](docs/deploy.md)
- [Migration from the legacy Company Brain](docs/migration.md)
- [Architecture](docs/architecture.md)
- [Team runbook](docs/runbook.md)
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
