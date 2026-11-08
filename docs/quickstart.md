# Quickstart

Three ways to run LV24 Brain. Pick one, finish the smoke checks, then read the
mode-specific document. The documentation site is https://brain.lv24.by.

## Five-minute path (one machine, coding agents)

Install a release (see [install.md](install.md); the Unix installer verifies
the release signature), then:

```sh
brain init --mode local --data-dir ~/brain        # or --mode git with a clone of your private data repo
brain config use ~/brain/brain.yaml              # commands work without --config from now on
brain project set myapp --path ~/src/myapp
brain note --project myapp --title "First note" --body "It works"
cd ~/src/myapp && brain open                     # the project is found from the directory
brain agents sync --hooks                        # Claude Code, Codex and OpenCode start with this context
```

Approve the new brain hooks in Codex once (Codex keeps hooks disabled until
they are trusted). That is the whole loop: agents read `brain open` at session
start and leave an `[auto]` handoff at the end.

Before starting a specific task, ask for just the context it needs:

```sh
brain context "add retry to sync push"
brain context "token expiry" --kind decision --since 30d --json
```

It prints the project state, review findings, the latest handoff, the records
that best match the task (decisions rank above attempts and notes, newer above
older) and failed attempts to avoid repeating, bounded by `--budget` (default
4000 runes). Agents call the same thing through the MCP tool `brain_context`.

| Mode | Storage | Sync | Best for |
|---|---|---|---|
| `local` | Markdown on this machine | none | one person |
| `git` | Markdown in a private Git repository | pull/rebase/commit/push | a team |
| `server` | HTTP API; Postgres when `BRAIN_DATABASE_URL` is set, in-memory fallback | API + SSE | a central server |

Prerequisites:

- A release binary, or Go 1.27+ to build from source.
- Git only for `git` mode.
- Docker Compose only if you use `deploy/docker-compose.yml` for the server.

Build once:

```sh
go build -o bin/brain ./cmd/brain
go build -o bin/brain-server ./cmd/brain-server
```

```powershell
go build -o bin\brain.exe .\cmd\brain
go build -o bin\brain-server.exe .\cmd\brain-server
```

Keep brain data outside the source checkout. The paths below are examples; use
your own directory names and never commit the data directory.

## Project registry

`brain project` manages `registry.json` explicitly; you never need to edit it
by hand. The same commands work in local, git, and server mode because they go
through the configured store (filesystem or HTTP API).

```sh
./bin/brain project set demo --name "Demo" --path ~/src/demo \
  --kind repository --repo https://example.test/demo.git --branch main \
  --config ~/brain/brain.yaml
./bin/brain project list --config ~/brain/brain.yaml
./bin/brain project show demo --config ~/brain/brain.yaml
```

Omitted flags keep their stored values, so
`brain project set demo --branch dev` only changes the branch.
`--clear-paths` and `--clear-aliases` store an explicitly empty list; in server
mode they send empty arrays (`[]`), not omitted fields. `--json` prints the
registry record for scripts. Projects are still created automatically on first
write; `brain project set` just makes the metadata explicit.

## 1. Local mode

Single user, filesystem data, no network.

```sh
./bin/brain init --mode local --data-dir ~/brain
./bin/brain status --config ~/brain/brain.yaml
```

```powershell
.\bin\brain.exe init --mode local --data-dir "$env:USERPROFILE\brain"
.\bin\brain.exe status --config "$env:USERPROFILE\brain\brain.yaml"
```

Write and read a record:

```sh
./bin/brain note --project demo --create --title "First note" --body "Local smoke check" \
  --config ~/brain/brain.yaml
./bin/brain open --project demo --config ~/brain/brain.yaml
./bin/brain bootstrap --project demo --config ~/brain/brain.yaml
./bin/brain review --project demo --config ~/brain/brain.yaml
```

`--create` registers `demo` on this first write; without it an unknown
project is an error, so a typo never creates a project. If exactly one project
exists, `--project` can be omitted.

Expected `status` output (paths and version numbers vary):

```text
mode: local
data_dir: <data-dir>
server_url:
user:
schema_version: 1
engine_version: <version>
update_channel: stable
```

Expected record output:

```text
note note-demo-YYYYMMDD-HHMMSS
```

Expected `doctor` output:

```text
ok: config (configuration is valid: <data-dir>/brain.yaml)
ok: data_dir (exists: <data-dir>)
ok: schema (schema 1)
ok: index (SQLite FTS5 is usable)
ok: records (all record files readable (1 projects))
ok: journal (every record of the last 30 days is journaled)
ok: ids (1 record ids are unique)
ok: git (not required in local mode)
```

The data directory contains `brain.yaml`, `registry.json`, and
`projects/<id>/` with `state.md`, `notes/`, `decisions/`, `attempts/`, and
`handoffs/`. Markdown is the source of truth.

### Local search

New writes update the SQLite FTS5 index automatically. Markdown added outside
the CLI (including a migration import) needs an explicit reindex:

```sh
./bin/brain index --config ~/brain/brain.yaml
./bin/brain index --rebuild --config ~/brain/brain.yaml
./bin/brain search "Local smoke" --config ~/brain/brain.yaml
./bin/brain search "release" --project demo --limit 5 \
  --config ~/brain/brain.yaml
```

`brain index` upserts every project, state, note, decision, attempt, and handoff
into the local index. `brain index --rebuild` first deletes `index/brain.db`
(and its sidecar files), then rebuilds from Markdown. `brain index` is local and
`git` mode only; `brain search` also works in server mode.

Search matches common English and Russian word forms (plural, case and verb
endings) as a prefix, and exact forms rank first; `--since` keeps documents
updated within a window:

```sh
brain search decisions                 # also finds "decision"
brain search "решений"                 # also finds "решение", "решения"
brain search deploy --since 2026-11-01 # only records updated since 1 November
brain search deploy --since 7d --kind decision
```

`brain search` prints one block per hit:

```text
note/demo/First note
  <snippet>
```

Server mode does not use the local index. `brain search` there calls
`GET /v1/search?q=...` on the configured server; `brain index` returns an
unsupported-mode error.

### Connectors

The CLI runs read-only GitHub, GitLab, and IMAP connectors. Credentials come
from the environment or the OS secret store, never from `brain.yaml`:

```sh
export BRAIN_GITHUB_REPOS="owner/repository"
printf '%s\n' '<github-token>' | ./bin/brain secret set github/token
./bin/brain connector run --name github --config ~/brain/brain.yaml
```

For GitLab use `BRAIN_GITLAB_PROJECTS` and the secret key `gitlab/token`. For
IMAP set `BRAIN_IMAP_ACCOUNT`, `BRAIN_IMAP_ADDRESS`, `BRAIN_IMAP_USERNAME`, and
store the password under `imap/<account-slug>/password` (or override the key
with `BRAIN_IMAP_PASSWORD_KEY`). Dumps are written below the data directory:
`github/<owner>/<repo>.md`, `gitlab/<group>/<project>.md`, and one file per
batch of new mail, `mail/<account>/<folder>/<date>-uid<first>-<last>.md`.
They are searchable with `brain search --dumps`.

`--since <RFC3339>` limits items by update time; `--limit <n>` caps each
collection. The command prints source, status, counts, and a sanitized error.
In local or `git` mode it also writes dumps below the data directory and
persists each run as Markdown under `connectors/runs/`. In server mode the same
command calls `POST /v1/connectors/run`; the server stores the run record in
PostgreSQL (or the in-memory development store) and discards the bulk dumps.
`GET /v1/connectors/runs` lists records, `GET /v1/connectors/runs/{id}` returns
one, and `GET /v1/connectors/runs/{id}/markdown` exports it as Markdown.
`BRAIN_CONNECTOR_TIMEOUT` (default `5m`, clamped to `10s`–`1h`) bounds a server run.
The CLI allows `1h10s` for this synchronous request, including persistence;
ordinary API and authentication requests retain the `30s` default timeout.
IMAP server runs are stateless unless `BRAIN_IMAP_STATE` is set.

## 2. Git mode

Team mode: a private data repository is the shared brain.

```sh
git clone <private-data-repo-url> ~/brain-data
./bin/brain init --mode git --data-dir ~/brain-data --config ~/brain-data/brain.yaml
./bin/brain sync --status --config ~/brain-data/brain.yaml
```

```powershell
git clone <private-data-repo-url> "$env:USERPROFILE\brain-data"
.\bin\brain.exe init --mode git --data-dir "$env:USERPROFILE\brain-data" `
  --config "$env:USERPROFILE\brain-data\brain.yaml"
.\bin\brain.exe sync --status --config "$env:USERPROFILE\brain-data\brain.yaml"
```

Write, then publish:

```sh
./bin/brain note --project demo --create --title "Team note" --body "Git smoke check" \
  --config ~/brain-data/brain.yaml
./bin/brain sync --config ~/brain-data/brain.yaml --push
./bin/brain sync --status --config ~/brain-data/brain.yaml
```

`brain sync` stages non-ignored files, commits when dirty, fetches, rebases onto
the upstream branch, and pushes only with `--push`. A freshly created, empty
data repository works too: the first `sync --push` creates its branch. It never force-pushes and
never resolves conflicts automatically. `--status` is read-only.

Expected `sync --status` output:

```text
provider: git
ahead: 0
behind: 0
changed: 0
```

A publish run reports the commits it created or moved:

```text
provider: git
ahead: 0
behind: 0
changed: 1
```

The data repository `.gitignore` excludes `secrets/`, `index/`, `backups/`,
databases, and `.env`. Use an SSH key or the Git credential helper; never embed
a token in the repository URL.

## 3. Server mode

The HTTP API and the token-authenticated CLI client are implemented. Postgres
persistence is enabled by `BRAIN_DATABASE_URL` (or `DATABASE_URL`); without it
the server falls back to an in-memory store and loses data on restart. Server
schema 8 (migrations `0002_auth_tokens.sql` through `0008_project_access.sql`)
stores API tokens (with optional scopes and project allow-lists), refresh-token
families, the security audit log, knowledge-record owners, and project access
rules durably. See
[deploy.md](deploy.md) for Compose, backups, and the migration gate.

Start the server with an explicit signing key, one bootstrap administrator, and
(optionally) a Postgres URL:

```sh
export BRAIN_JWT_SECRET="<at least 32 bytes>"
export BRAIN_BOOTSTRAP_ADMIN_LOGIN="admin"
export BRAIN_BOOTSTRAP_ADMIN_PASSWORD="<strong password>"
export BRAIN_DATABASE_URL="postgres://brain:<url-encoded-password>@127.0.0.1:5432/brain?sslmode=disable"
./bin/brain-server --addr 127.0.0.1:8080
```

```powershell
$env:BRAIN_JWT_SECRET = '<at least 32 bytes>'
$env:BRAIN_BOOTSTRAP_ADMIN_LOGIN = 'admin'
$env:BRAIN_BOOTSTRAP_ADMIN_PASSWORD = '<strong password>'
$env:BRAIN_DATABASE_URL = 'postgres://brain:<url-encoded-password>@127.0.0.1:5432/brain?sslmode=disable'
.\bin\brain-server.exe --addr 127.0.0.1:8080
```

Omit `BRAIN_DATABASE_URL` for a disposable in-memory run. On an existing
database with pending migrations, the server refuses to start until a backup is
taken and `BRAIN_DATABASE_BACKUP_CONFIRMED=1` is set; a fresh database migrates
immediately.

Expected startup line:

```text
brain-server <version> listening on 127.0.0.1:8080
```

Smoke checks:

```sh
curl -fsS http://127.0.0.1:8080/health
curl -fsS http://127.0.0.1:8080/ready
curl -fsS http://127.0.0.1:8080/version
```

Expected responses:

```text
{"status":"ok"}
{"status":"ready"}
{"version":"<version>","commit":"<commit>","date":"<date>","go":"<go version>"}
```

Log in through the CLI (server mode only), then use the stored session:

```sh
./bin/brain init --mode server --server-url http://127.0.0.1:8080 \
  --config ~/brain-server.yaml
printf '%s\n' '<strong password>' | ./bin/brain login \
  --config ~/brain-server.yaml --login admin --password-stdin
./bin/brain doctor --config ~/brain-server.yaml
./bin/brain note --project demo --title "Server note" --body "Server smoke" \
  --config ~/brain-server.yaml
./bin/brain logout --config ~/brain-server.yaml
```

`--login` falls back to `BRAIN_LOGIN`; without `--password-stdin` the password
comes from `BRAIN_PASSWORD`. `--password-stdin` reads one line from stdin, and
there is no `--password` flag; the pipeline itself may
still be recorded by shell history. `brain login` stores one JSON credential
bundle under the OS secret-store key `server-session` (`serverUrl`,
`accessToken`, `refreshToken`) in a single atomic write, bound to the configured
`server_url`. The HTTP client refreshes once on `401` and retries. A platform
secret service must be available; otherwise keep using `BRAIN_TOKEN`.
`brain logout` checks `GET /v1/auth/me` before rotating anything, revokes the
JWT session and its refresh family, and clears the bundle. The refresh family
is revoked immediately; an access JWT already issued stays valid until its
short TTL. `server-token` remains an explicit static API-token fallback with no
refresh token and no binding; `server-refresh-token` and `server-auth-url` are
no longer read, so a legacy unbound JWT pair requires `brain login` again. A
static API token cannot be revoked by the CLI: logout clears the local entries,
warns, and exits non-zero.

`BRAIN_TOKEN` still takes precedence over stored credentials: login is refused
and logout leaves a stored session untouched and exits non-zero. That is the
path for automation with a static API token. A login-managed bundle stored for
another server URL is never sent to the configured server, and a mismatched
binding is refused with both URLs named. The login and token endpoints are also
available over HTTP for scripts:

```sh
curl -fsS -X POST http://127.0.0.1:8080/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"login":"admin","password":"<strong password>"}'

curl -fsS -X POST http://127.0.0.1:8080/v1/tokens \
  -H 'Authorization: Bearer <access-token>' \
  -H 'Content-Type: application/json' \
  -d '{"name":"quickstart","role":"member"}'
```

The login response contains `accessToken`, `refreshToken`, `expiresAt`, and
`user`. The token response contains the one-time raw `token` value and its
metadata; store it in the OS secret store or an environment variable, not in a
repository. Send either token as `Authorization: Bearer <token>`. A static API
token can be stored with
`printf '%s\n' '<token>' | ./bin/brain secret set server-token`.

Other useful endpoints:

- `GET /openapi.json` — OpenAPI 3 document.
- `GET /v1/projects`, `GET /v1/search?q=...` — authenticated read paths.
- `GET /v1/events` — SSE revision stream with heartbeat and `Last-Event-ID`.

Expected `doctor` output in server mode (revision number varies):

```text
ok: config (configuration is valid: <config-path>)
ok: server (API reachable (revision <n>))
ok: data_dir (not used in server mode)
ok: schema (schema 1)
ok: index (server mode uses the remote database)
ok: git (not used in server mode)
```

A successful write prints `note note-<project>-YYYYMMDD-HHMMSS`.

The `schema` check reports the data schema from `brain.yaml`; the server
database schema (`postgres.SchemaVersion`, currently 6) is separate.

`brain sync` is not implemented in server mode — the API is the sync path.
`brain index` is local-only; `brain search` works in server mode through
`GET /v1/search?q=...`. Server-side Markdown import/export is not wired yet.
`brain doctor` fails the `server` check when the URL is wrong or no usable token
is stored.

## Command flags

Flags that the mode sections do not show. Every value below is a flag defined
in `cmd/brain`; run `brain <command> --help` for the full list. Each command
also accepts `--config <path>`.

| Command | Flags and defaults |
|---|---|
| `brain open [path\|id]` | `--project <id\|alias>`; `--budget <runes>` (default 4000) caps the summary; `--brief` prints only the project and state fields (no git or graph sections) |
| `brain bootstrap [path\|id]` | `--project <id\|alias>`; `--budget <runes>` (default 4000) |
| `brain note [text]` | `--project`, `--title`, `--body`, `--tags a,b`. Positional text becomes the body and the title is its first line (at most 80 characters). Do not combine positional text with `--body`. |
| `brain decision [title]` | `--project`, `--title`, `--status proposed\|accepted\|rejected\|superseded` (default `proposed`), `--context <text>`, `--alternatives <a,b>`, `--consequences <a,b>`, `--evidence <a,b>`. The last three are comma-separated lists. |
| `brain attempt [title]` | `--project`, `--title`, `--hypothesis <text>` (what was expected), `--result <text>` (what happened), `--reason <text>` (why it failed) |
| `brain handoff [summary]` | `--project`, `--summary`, `--next <text>` (next action), `--git-context <text>` (default: read from the project checkout), `--no-git` (record no git context) |
| `brain journal` | `--project`, `--user`, `--event <name>` (for example `note`, `decision`, `attempt`, `handoff`, `commit`), `--since <duration>` (for example `24h`), `--limit <n>` (default 20). Local and git mode. |
| `brain event commit` | `--project <id>`, `--repo <path>` (default `.`), `--quiet` (print nothing on success; used by the hook). Local and git mode. |
| `brain search <query>` | `--project <id>`, `--limit <n>` (default 20), `--kind <kind>`, `--dumps`, `--since <24h\|7d\|2w\|2026-11-01\|RFC3339>`. English and Russian word forms match as a prefix. |
| `brain context <task>` | `--project <id>`, `--budget <runes>` (default 4000), `--kind decision\|attempt\|note` (repeatable), `--since <24h\|7d\|2w\|date>`, `--json`. State, review findings, latest handoff, best-matching records and failed attempts for the task. MCP: `brain_context`. |
| `brain sync` | `--status` (read-only divergence and conflict report), `--push` (push after committing) |
| `brain index` | `--rebuild` (delete the local index and rebuild it from Markdown) |

Positional text is joined with spaces. Flags may appear before or after it.

Day-to-day use, sync, hooks, server operations, upgrades, and incident
steps are in the [team runbook](runbook.md).

## Agent usage

Agents call the engine directly; no wrapper script is needed.

```sh
brain config use /path/to/lv24-brain-data/brain.yaml   # once per machine
cd /path/to/some/registered/repo
brain open .                  # bootstrap pack, ## Git, ## Graphs
brain note "Fixed the cache warmup"
brain decision "Adopt the Go engine"
brain attempt "Pinning the old driver"
brain handoff "Session summary"   # git context is captured automatically
brain hooks install           # record every commit in journal/<user>.jsonl
```

Without `--project`, commands resolve the project like the legacy
`brain.ps1`: an id or alias, else the longest registered path containing the
working directory (or the given path), else the single registered project
under it. Flags may follow positional text. The default config is found in
this order: `--config`, `$BRAIN_CONFIG`, `./brain.yaml`, then the pointer set
by `brain config use` (stored under the OS user config directory).

## Smoke checklist

| Check | Command | Pass condition |
|---|---|---|
| CLI identity | `brain version` | prints version, commit, build date, platform |
| Local instance | `brain doctor --config <path>` | no `fail:` lines; SQLite FTS5 `ok` |
| Local index | `brain index --config <path>` | exits `0`, reports non-zero `documents` after records exist |
| Local search | `brain search "<term>" --config <path>` | exits `0`, prints hit blocks |
| Git remote | `brain sync --status --config <path>` | exits `0`, prints provider and divergence |
| Server liveness | `GET /health` | `{"status":"ok"}` |
| Server readiness | `GET /ready` | `{"status":"ready"}` |
| Server auth | `POST /v1/auth/login` | `200` with `accessToken` and `refreshToken` |
| Server contract | `GET /openapi.json` | JSON OpenAPI document |
| Server-mode CLI | `brain doctor --config <path>` | `ok: server (API reachable ...)` |

## Troubleshooting

- `no brain instance at ...; run brain init first` — the `--config` path is
  wrong or `init` was run for another data directory.
- `data_dir: does not exist` from `doctor` — create the directory with `init`
  or correct `brain.yaml`.
- `local mode has no remote` from `sync` — local mode does not sync; use `git`
  mode.
- `brain sync` fails with a conflict — resolve the Git conflict manually, then
  rerun. The engine never discards changes.
- Server exits with `BRAIN_JWT_SECRET is required` — set a signing key of at
  least 32 bytes.
- `401` on server requests — the HTTP client refreshes once automatically, so a
  repeated `401` means the refresh session is expired or revoked: run
  `brain login` again (or set `BRAIN_TOKEN` for automation), then retry. Access
  tokens expire after 15 minutes by default; refresh tokens rotate.
- `server client is not configured` from `doctor` — the URL is missing or no
  token was found in `BRAIN_TOKEN` / the OS secret store.
- `local search index is not available` from `search` — run `brain index` (or
  `brain index --rebuild`) against the configured data directory.
- `index is local-only in server mode` — `brain index` runs only in local or
  `git` mode; in server mode `brain search` calls `GET /v1/search?q=...`.
- `sync in server mode: not implemented` — server mode does not use Git sync;
  reads and writes go through the HTTP API.
