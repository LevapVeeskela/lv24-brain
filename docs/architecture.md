# Architecture

Go monorepo for the LV24 Brain engine. The approved product design is
[`product-design.md`](product-design.md); this document describes what the code
actually does today and where the remaining gaps are.

## Repository split

- `lv24-brain` (this repository): generic product code, docs, templates, tests,
  installers, and deployment files. No brain data, credentials, connector dumps,
  mail, or machine paths.
- `lv24-brain-data` (private): `brain.yaml`, `projects/`, `mail/`, `connectors/`,
  `journal/`, `registry.json`, and `global/`. Never contains secrets, indexes,
  or backups.

## Module layout

```text
cmd/brain/                 CLI entry point and flag parsing
cmd/brain-server/          HTTP server entry point
internal/domain/           entities, value objects, validation errors
internal/app/              use cases (init, write, open, bootstrap, review, sync, index, search, update, doctor, secret)
internal/ports/            Store, Index, SyncProvider, SecretStore, AuthProvider, Updater
internal/bootstrap/        CLI composition root; selects adapters by mode
internal/config/           brain.yaml load/save/validate
internal/markdown/         Markdown and frontmatter primitives
internal/auth/             Argon2id, JWT, rotating refresh tokens, API tokens, auth service
internal/adapters/
  fsstore/                 Markdown + YAML frontmatter data store
  gitstore/                git CLI sync provider
  sqliteindex/             SQLite FTS5 index
  httpapi/                 REST, auth, SSE, OpenAPI, in-memory store
  httpclient/              server-mode Store client with bearer tokens
  postgres/                Postgres store, schema, embedded migrations, audit
  serverstore/             bridge from postgres.Store to httpapi.Store
  oidc/                    generic OIDC discovery and code exchange
  secrets/                 environment + OS secret store
  github/, gitlab/         read-only repository connectors
  imap/                    read-only IMAP connector
internal/connectors/       shared run records, retry/rate-limit helpers, redaction
internal/migrate/          legacy .brain importer
internal/server/           server config and process wiring
internal/update/           release check, verify, stage, apply, rollback
internal/version/          build identity
templates/                 state, decision, attempt, handoff, AGENTS.md
installers/windows/        install.ps1 and install.bat
integrations/opencode/     handoff hook template
deploy/                    Dockerfile, Compose, environment template, GoReleaser
docs/                      product, quickstart, install, update, deploy, migration, security
```

Module path: see `go.mod` (`github.com/<owner>/lv24-brain`). Go 1.27.

## Dependency rule

```text
cmd/* / internal/bootstrap   composition roots
internal/app                 use cases; depends on domain, ports, config
internal/ports               interfaces only
internal/domain              pure entities and validation errors
internal/adapters/*          implement ports; selected at bootstrap
```

Dependencies point inward. `internal/domain` imports no project packages and no
I/O libraries. `internal/ports` imports only `context`, `time`, and domain.
Adapters never leak into domain or app.

## Modes

| Mode | Store | Index | Sync | Status |
|---|---|---|---|---|
| `local` | filesystem (`fsstore`) | SQLite FTS5 | none | implemented |
| `git` | filesystem (`fsstore`) | SQLite FTS5 | `gitstore` pull/rebase/commit/push | implemented |
| `server` | HTTP API (`httpclient` in the CLI) | Postgres when `BRAIN_DATABASE_URL` is set; in-memory fallback | API + SSE; no `brain sync` | server API and CLI client implemented |

`brain.yaml` records `mode`, `data_dir`, `server_url`, `user`,
`schema_version`, `min_engine_version`, and `update_channel`. Local and Git
modes require `data_dir`; server mode requires `server_url` and has no data
directory.

## Data layout

Markdown is the source of truth in local and Git modes. `fsstore` uses:

```text
registry.json                          project index
projects/<id>/state.md                 goal, active, blocked, next action, questions
projects/<id>/notes/YYYY-MM-DD.md      dated note sections
projects/<id>/decisions/YYYY-MM-DD-<slug>.md
projects/<id>/attempts/YYYY-MM-DD-<slug>.md
projects/<id>/handoffs/YYYY-MM-DD-HHMMSS.md
```

Every user-facing file carries YAML frontmatter with `project_id`, `kind`,
`updated_at`, `schema_version`, and optional `permalink`. Writes are atomic
(temp file + rename). The SQLite index lives under `index/brain.db` and is
derived data: new writes upsert into it. `brain index` refreshes it from
existing Markdown, and `brain index --rebuild` deletes the database and sidecar
files before rebuilding.

Domain records: `Project`, `Note`, `Decision`, `Attempt`, `Handoff`, `State`,
`User`, `Role` (`admin|member|viewer`), `AuthSource`, `SyncRevision`, and
`ConnectorRun`.

## CLI

```text
brain init       create an instance and write brain.yaml
brain open       show the active project context
brain bootstrap  build bounded agent context
brain note       append a dated note
brain decision   append a decision
brain attempt    append an attempt
brain handoff    append a session handoff
brain status     show mode, paths, schema, engine version
brain review     report state age, questions, evidence, attempts
brain sync       commit, rebase, optional push, or --status (local/git only)
brain index      refresh the local FTS5 index, or --rebuild it (local/git only)
brain search     query the local FTS5 index (local/git only)
brain connector  run a read-only GitHub, GitLab, or IMAP connector
brain migrate    import a legacy .brain data directory
brain secret     set, list, or remove OS-backed secrets
brain doctor     check config, data/schema/index/git or server reachability
brain update     check or apply a release
brain serve      run the HTTP server with development defaults
brain version    print build identity
```

Server mode selects the HTTP client at bootstrap. The token comes from
`BRAIN_TOKEN`, then the OS secret store key `server-token` (stored with
`brain secret set server-token`, value read from stdin). Missing or invalid
tokens surface as a failing `server` check in `brain doctor`; there is no
`brain login` or CLI OIDC flow yet. `brain sync`, `brain index`, and
`brain search` are not available in server mode: the API is the sync and search
path (`GET /v1/search`).

List-like capabilities are optional Store extensions: an adapter that does not
implement `ListNotes` / `ListDecisions` / `ListAttempts` / `ListHandoffs` still
supports writes; context sections simply report nothing.

## Git sync

One private data repository is the shared brain. Each user owns
`journal/<user>.jsonl`; shared files are edited normally and conflicts are
resolved by Git.

- `brain sync --status` is read-only.
- `brain sync` commits staged non-ignored changes, runs `fetch --prune`,
  `pull --rebase`, and pushes only with `--push`.
- No force-push and no automatic destructive conflict resolution.
- A conflict returns the unmerged paths and leaves the repository for manual
  repair.
- `gitstore` shells out to the git CLI with an argument vector, a per-command
  timeout, `GIT_TERMINAL_PROMPT=0`, and sanitized errors so remote URLs with
  credentials do not leak.

## Server

`brain-server` is a single Go binary using `net/http`. It fails fast on invalid
configuration and requires a signing key of at least 32 bytes.

Implemented surface:

- `GET /health`, `GET /ready`, `GET /version`, `GET /openapi.json`.
- `POST /v1/auth/login`, `POST /v1/auth/refresh`,
  `POST /v1/auth/logout`, `GET /v1/auth/me`.
- `GET /v1/auth/oidc/login`, `GET /v1/auth/oidc/callback`.
- Admin user management at `/v1/users`.
- API token management at `/v1/tokens`.
- Projects, state, notes, decisions, attempts, handoffs, search, and revisions.
- `GET /v1/events` SSE with `retry`, `ready`, revision events, heartbeat, and
  `Last-Event-ID` replay.

Authentication and roles:

- Local accounts use Argon2id password hashes.
- Access tokens are HS256 JWTs with issuer/audience validation, a 15-minute
  default TTL, and a 30-second leeway.
- Refresh tokens rotate; reuse revokes the token family.
- API tokens use the `brain_` prefix, are stored as SHA-256 hashes, and are
  accepted as `Authorization: Bearer`.
- OIDC uses authorization code with PKCE, `state`, and `nonce`.
- Roles are enforced server-side on every route: `viewer` reads, `member`
  writes, `admin` manages users, roles, and deletion.

Persistence:

- With `BRAIN_DATABASE_URL` (or `DATABASE_URL`) set, `internal/server` opens
  Postgres, checks schema health, applies embedded migrations, and wraps
  `postgres.Store` with `serverstore`. Data survives restarts. The database URL
  is redacted in startup logs.
- Without a database URL the server deliberately falls back to
  `httpapi.MemoryStore`, which is development-only and loses data on restart.
- Migrations are forward-only. An existing schema with pending migrations
  refuses to start until a backup is taken and
  `BRAIN_DATABASE_BACKUP_CONFIRMED=1` is set. A fresh database migrates
  immediately.
- The `0001_init.sql` schema has no `api_tokens` or `refresh_tokens` tables, so
  API tokens and refresh-token families remain in process memory in the bridge.
  Clients re-authenticate after a restart, and multi-replica deployment remains
  unsupported.

`deploy/docker-compose.yml` provisions `brain-server` and Postgres with
healthchecks and internal-only database networking; Compose supplies
`BRAIN_DATABASE_URL`, so the stack uses Postgres. Never publish port 5432.

## Connectors and secrets

GitHub, GitLab, and IMAP adapters are read-only. IMAP selects mailboxes
read-only, uses `BODY.PEEK`, keeps metadata and a bounded snippet, and exposes
no send, delete, move, append, copy, or flag operations. Runs are idempotent and
build `ConnectorRun` values with sanitized errors.

`brain connector run --name github|gitlab|imap` selects one adapter and writes
Markdown dumps under the data directory (`github/...`, `gitlab/...`,
`mail/<account>/...`). The command prints the run record (source, status,
counts, sanitized error) but does not persist `ConnectorRun` records yet.
Connectors require local or git mode with `data_dir`.

`internal/adapters/secrets` resolves credentials from
`BRAIN_SECRET_<NAME>` first and the platform store second: Windows Credential
Manager, macOS Keychain, or Linux Secret Service. A key such as `github/token`
maps to `BRAIN_SECRET_GITHUB_TOKEN`. Environment-provided values shadow the OS
store and cannot be overwritten through the store API.
`brain secret set|list|remove` manages the OS store; `set` reads the value from
stdin and prints only the secret name.

## Auto-update

`internal/update` checks GitHub Releases, validates `release.json`, selects
stable or beta by semantic version, verifies SHA-256, stages the download under
the platform cache directory, applies it atomically, and keeps
`brain(.exe).previous` for rollback. On Windows a detached helper swaps the
running executable after exit.

`brain update --check|--yes` is wired through `internal/app` and
`internal/bootstrap`. Remaining gaps: no daily background scheduler, no
`--rollback` flag (the package method exists), and server auto-update
(`BRAIN_SERVER_AUTO_UPDATE`) is not implemented — deployments upgrade by
pulling a new image.

## Migration

`internal/migrate` imports knowledge records from a legacy `.brain` directory
into an `fsstore` data directory. It is idempotent, never overwrites a differing
target file, validates paths, rejects symlinks, skips temp files, and writes
schema version 1. Mail, connectors, journal, legacy registry metadata, and
global conventions are intentionally not imported; the CLI registers imported
project ids afterward.

The command is wired:

```sh
brain migrate --from <legacy-dir> --data-dir <target-dir> --dry-run
brain migrate --from <legacy-dir> --data-dir <target-dir>
```

Without `--data-dir`, the target comes from `brain.yaml` (`--config` or the
current directory). The report prints `created`, `unchanged`, `conflicts`, and
`failed` totals plus conflict/failure details. A successful non-dry run sets
`schema_version: 1` in the target configuration; existing conflicting files are
left untouched and the command exits non-zero.

## Tests and CI

- `go test ./...`, `go vet ./...`, `gofmt -l .`.
- Adapter tests use temporary directories; Postgres integration tests require a
  database DSN; server/client tests use `httptest`.
- `.github/workflows/ci.yml` runs formatting, vet, tests, builds, JS syntax
  check, and Compose config validation.
- `.github/workflows/release.yml` runs GoReleaser on `v*` tags, writes
  `checksums.txt` and `release.json`, and publishes a GitHub release.

## Known gaps

| Gap | Consequence |
|---|---|
| No `BRAIN_DATABASE_URL` means in-memory server | data is lost on restart; use Postgres for durable storage |
| No `brain login` / CLI OIDC flow | server token must be copied from the API and set via `BRAIN_TOKEN` or `brain secret` |
| `brain sync` not implemented in server mode | API is the only server sync path |
| API/refresh tokens in process memory | clients re-authenticate after restart; no multi-replica |
| Connector runs are not persisted | `brain connector run` prints counts and writes Markdown dumps only |
| CLI search/index are local-only | server mode uses `GET /v1/search`; no local index is built |
| No server-side Markdown import/export | migrate into Markdown first; server records go through the API |
| No background update check or `--rollback` flag | updates are manual; restore `.previous` by hand |
| Server auto-update not implemented | upgrade by pulling a new image; `BRAIN_SERVER_AUTO_UPDATE` is reserved |
| Windows-only installer | macOS/Linux install release binaries manually |

## Non-goals v1

CRDT co-editing, multi-tenant SaaS, billing, mobile app, paid hosting or CI,
automatic mail body storage, and Rust components (interfaces are reserved).
