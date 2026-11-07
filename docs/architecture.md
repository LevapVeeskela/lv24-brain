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
internal/app/              use cases (init, write, open, bootstrap, review, sync, index, search, update, doctor, secret, auth)
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
installers/unix/           install.sh for macOS and Linux
integrations/opencode/     handoff hook template
deploy/                    Dockerfile, Compose, environment template, GoReleaser
docs/                      product, quickstart, install, update, deploy, migration, security, runbook
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
registry.json                          project index (managed by brain project)
projects/<id>/state.md                 goal, active, blocked, next action, questions
projects/<id>/notes/YYYY-MM-DD.md      dated note sections
projects/<id>/decisions/YYYY-MM-DD-<slug>.md
projects/<id>/attempts/YYYY-MM-DD-<slug>.md
projects/<id>/handoffs/YYYY-MM-DD-HHMMSS.md
connectors/runs/<UTC timestamp>-<source>-<run-id>.md
```

Every user-facing file carries YAML frontmatter with `project_id`, `kind`,
`updated_at`, `schema_version`, and optional `permalink`. Writes are atomic
(temp file + rename). The SQLite index lives under `index/brain.db` and is
derived data: new writes upsert into it. `brain index` refreshes it from
existing Markdown, and `brain index --rebuild` deletes the database and sidecar
files before rebuilding. When a write cannot update the index, or `brain sync`
pulls commits from another machine, the engine sets `index/dirty`; sync then
reindexes at once, and the next search repairs the index if that failed.
`brain doctor` warns while the marker is set. A reindex removes entries whose
source files are gone. The data revision (`CurrentRevision`) covers curated
records only; connector dumps and runs, journals, the index and backups are
excluded, so mail syncs and journal lines do not change it.

Listing never stops at one damaged file: unreadable, malformed or empty record
files are skipped and reported by `brain doctor` (check `records`). A daily
notes file that no longer parses is moved aside to
`<date>.md.corrupt-<n>` on the next note, so new notes keep working and the
damaged content is kept for repair.

Domain records: `Project`, `Note`, `Decision`, `Attempt`, `Handoff`, `State`,
`User`, `Role` (`admin|member|viewer`), `AuthSource`, `SyncRevision`, and
`ConnectorRun`.

## CLI

```text
brain init       create an instance and write brain.yaml
brain open       [path|id] bootstrap pack + ## Git + ## Graphs (--brief: state only)
brain project    list, show, or set registry projects
brain bootstrap  build bounded agent context
brain note       append a dated note
brain decision   append a decision
brain attempt    append an attempt
brain handoff    append a session handoff
brain status     show mode, paths, schema, engine version
brain review     state age, questions, evidence, attempts + findings (stale/contradictory knowledge); --fix applies safe state fixes
brain catchup    what changed since the latest handoff
brain record     list|show records; status|supersede decisions; trash|restore|trash-list (local/git)
brain session    start|end handlers for agent session hooks
brain state      get|set the project state
brain sync       commit, rebase, optional push, or --status (local/git only)
brain index      refresh the local FTS5 index, or --rebuild it (local/git only)
brain search     query the local FTS5 index (local/git), or GET /v1/search (server); --dumps adds connector dumps, --kind filters
brain journal    show journal/<user>.jsonl activity, newest first (local/git)
brain event      record an external event; `event commit` reads git for a post-commit hook
brain hooks      install|uninstall|status the post-commit journal hook
brain config     use <brain.yaml> | show | clear the user default config
brain connector  run a read-only GitHub, GitLab, or IMAP connector
brain migrate    import a legacy .brain data directory
brain secret     set, list, or remove OS-backed secrets
brain login      authenticate against a server-mode brain (local account)
brain logout     revoke the server session and clear stored tokens
brain doctor     check config, data/schema/index/git or server reachability
brain update     check, apply, or roll back a release
brain serve      run the HTTP server with development defaults
brain version    print build identity
```

Server mode selects the HTTP client at bootstrap. `brain login --config <path>
--login <name> --password-stdin` (or `BRAIN_PASSWORD` without the flag, with
`BRAIN_LOGIN` as the login fallback; there is no `--password` flag) exchanges
local credentials at
`POST /v1/auth/login` and stores one JSON credential bundle under the OS
secret-store key `server-session` (`serverUrl`, `accessToken`,
`refreshToken`). A single `Set` keeps binding and token pair atomic, and the
bundle is bound to the normalized `server_url`; it is never sent to another
server URL and a mismatched binding is refused with both URLs named. The HTTP
client reads the access token lazily, refreshes once on `401`, and retries the
request; refreshes are serialized with a per-user lock whose wait exceeds the
HTTP timeout, so concurrent `401`s do not burn two refresh tokens.
`brain logout` inspects `GET /v1/auth/me` before rotating anything, revokes the
JWT session and its refresh family, and clears the bundle. The refresh family
is revoked immediately; an access JWT already issued stays valid until its
short TTL.

`server-token` remains as an explicit static API-token fallback with no refresh
token and no binding. The old `server-refresh-token` and `server-auth-url` keys
are no longer read, so a legacy unbound JWT pair requires `brain login` again.
A static API token cannot be revoked server-side by the CLI: logout clears the
local entries, warns, and exits non-zero.

`BRAIN_TOKEN` (or an environment-provided `BRAIN_SECRET_SERVER_TOKEN`) takes
precedence over stored credentials and disables the auth provider for that
process: `brain login` refuses to run and `brain logout` leaves a stored
session untouched and exits non-zero. A static API token can still be installed
with `brain secret set server-token` (value read from stdin). There is no CLI
OIDC flow yet. Missing or invalid tokens surface as a failing `server` check in
`brain doctor`. `brain sync` and `brain index` are not available in server
mode: the API is the sync path. `brain search` works in server mode through
`GET /v1/search`. Server search is PostgreSQL full-text search over generated
`search_tsv` columns (migration 0007, `simple` configuration, title weighted
above body) and ranks before it applies the limit. `kind` and `exclude_kind`
filters are honored in both server and local modes.

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
  `Last-Event-ID` replay. Authenticate with the `Authorization` header, or with
  a single-use one-minute ticket from `POST /v1/events/ticket` passed as
  `?ticket=` for EventSource clients.

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
- `postgres.SchemaVersion` is 6. Migration `0002_auth_tokens.sql` adds the
  `api_tokens` and `refresh_tokens` tables, so API tokens and refresh-token
  families survive restarts. Migration `0003_auth_revocations.sql` adds durable
  family/session revocation tombstones with no expiry, so pruning expired token
  rows cannot resurrect a revoked family. `serverstore` converts these records;
  Postgres performs the writes. Refresh-token rotation still revokes the whole
  family when a used token is presented again. Migration
  `0005_knowledge_owner.sql` adds `created_by` (the creating user's id) to
  notes, decisions, attempts and handoffs and backfills it from create audit
  events; members may update or delete only records they own. Migration
  `0004_security_audit.sql` lets security audit events (logins, logouts,
  refresh-token reuse, API-token lifecycle, OIDC linking) be stored without a
  revision, so they never appear in `/v1/revisions` or SSE; admins read them
  with `GET /v1/audit`.
- Migration `0006_api_token_expiry.sql` adds the nullable `api_tokens.expires_at`.
  NULL means the token never expires. Once reached, an expired token is not
  found by `FindAPITokenByHash`, exactly like a revoked token.
- The authenticated login is attached to the request context through
  `ports.WithActor`, which both the HTTP layer and the Postgres adapter use, so
  every revision and audit row records who made the change.
- Local and git modes append per-user activity to `journal/<user>.jsonl`
  (`brain.yaml` `user`, else the OS account name): one line per note,
  decision, attempt, handoff, and recorded commit. Each user writes only their
  own file, so git-mode writers never conflict on it; the format is the legacy
  tool's flat JSON Lines and legacy lines (with a BOM) remain readable.
- Multi-replica deployment remains unsupported even with durable tokens: the
  SSE broker and the login rate limiter are per-process, so events and login
  throttling are not shared between instances.

`deploy/docker-compose.yml` provisions `brain-server` and Postgres with
healthchecks and internal-only database networking; Compose supplies
`BRAIN_DATABASE_URL`, so the stack uses Postgres. Never publish port 5432.

## Connectors and secrets

GitHub, GitLab, and IMAP adapters are read-only. IMAP selects mailboxes
read-only, uses `BODY.PEEK`, keeps metadata and a bounded snippet, and exposes
no send, delete, move, append, copy, or flag operations. Runs are idempotent and
build `ConnectorRun` values with sanitized errors.

`brain connector run --name github|gitlab|imap` selects one adapter. In local or
git mode it writes Markdown dumps under the data directory (`github/...`,
`gitlab/...`, `mail/<account>/...`) and persists each run as Markdown at
`connectors/runs/<UTC timestamp>-<source>-<run-id>.md`, including status,
counts, and the sanitized error. Re-appending the same run record rewrites the
same file, so retries stay idempotent.

IMAP dumps are append-only: each batch of new messages is written to
`mail/<account>/<folder>/<date>-uid<first>-<last>.md`, a run without new mail
writes nothing, and the UID cursor advances only after the batch is stored
(after a failed fetch, to the last stored message). GitHub and GitLab follow
pagination (`Link` / `X-Next-Page`, at most 10 pages, `limit` up to 1000).

In server mode the command calls `POST /v1/connectors/run`; the server builds the
same adapter from environment/OS secrets, runs it with `BRAIN_CONNECTOR_TIMEOUT`,
discards the bulk Markdown dumps, and stores the run record through
`httpapi.Store` (PostgreSQL when configured). Viewers can list and fetch runs
at `GET /v1/connectors/runs` and `GET /v1/connectors/runs/{id}`; `GET
/v1/connectors/runs/{id}/markdown` exports the run record. The run record keeps
status, counts and the sanitized error. IMAP server runs are stateless unless
`BRAIN_IMAP_STATE` is set.

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

`brain update --check|--yes|--rollback` is wired through `internal/app` and
`internal/bootstrap`. `--rollback` restores `brain(.exe).previous` and cannot be
combined with `--check` or `--yes`. Before ordinary commands the CLI may run a
notify-only daily check: at most one successful release lookup per 24 hours, a
3-second timeout, a single-flight lock, and state in `last-check.json` under the
update directory. The check never downloads or applies anything, prints one
stderr line when a newer release exists, and is skipped for `serve`, `init`,
`help`, `version`, and `update`. `BRAIN_NO_UPDATE=1` opts out.

Remaining gap: server auto-update (`BRAIN_SERVER_AUTO_UPDATE`) is not
implemented — deployments upgrade by pulling a new image.

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
- `.github/workflows/ci.yml` (GitHub-hosted `ubuntu-latest` only, actions
  pinned by commit SHA, superseded runs cancelled, docs-only changes skipped)
  runs three jobs: `test` (gofmt, vet, tests, builds, JS syntax check, Compose
  config validation), `postgres` (integration tests against a
  `postgres:16-alpine` service container, with a guard that fails if they
  were skipped), and `race` (`go test -race`, pushes only).
- `.github/workflows/release.yml` runs GoReleaser on `v*` tags, writes
  `checksums.txt` and `release.json`, signs the manifest into
  `release.json.sig`, and creates a GitHub release; it never overwrites an
  existing one.

## Known gaps

| Gap | Consequence |
|---|---|
| No `BRAIN_DATABASE_URL` means in-memory server | data, including tokens, is lost on restart; use Postgres for durable storage |
| No CLI OIDC flow | `brain login` supports local accounts only; OIDC is browser-based |
| `brain sync` not implemented in server mode | API is the only server sync path |
| Multi-replica deployment unsupported | the SSE broker and login rate limiter are per-process even with durable tokens |
| Server-side connector dumps are not persisted | run records and per-run Markdown export are available; bulk GitHub/GitLab/IMAP Markdown dumps are discarded in server mode |
| CLI index is local-only | `brain search` in server mode uses `GET /v1/search`; no local index is built |
| No server-side Markdown import/export | migrate into Markdown first; server records go through the API |
| Server auto-update not implemented | upgrade by pulling a new image; `BRAIN_SERVER_AUTO_UPDATE` is reserved |

## Non-goals v1

CRDT co-editing, multi-tenant SaaS, billing, mobile app, paid hosting or CI,
automatic mail body storage, and Rust components (interfaces are reserved).
