# Quickstart

Three ways to run LV24 Brain. Pick one, finish the smoke checks, then read the
mode-specific document.

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
./bin/brain note --project demo --title "First note" --body "Local smoke check" \
  --config ~/brain/brain.yaml
./bin/brain open --project demo --config ~/brain/brain.yaml
./bin/brain bootstrap --project demo --config ~/brain/brain.yaml
./bin/brain review --project demo --config ~/brain/brain.yaml
```

`--project demo` is created on first write with an empty state. If exactly one
project exists, `--project` can be omitted.

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
note note-YYYYMMDD-HHMMSS
```

Expected `doctor` output:

```text
ok: config (configuration is valid: <data-dir>/brain.yaml)
ok: data_dir (exists: <data-dir>)
ok: schema (schema 1)
ok: index (SQLite FTS5 is usable)
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
(and its sidecar files), then rebuilds from Markdown. Both commands are local
and `git` mode only.

`brain search` prints one block per hit:

```text
note/demo/First note
  <snippet>
```

Server mode does not use the local index. Its search path is
`GET /v1/search?q=...`; the CLI `brain search` and `brain index` commands
return an unsupported-mode error there.

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
`github/<owner>/<repo>.md`, `gitlab/<group>/<project>.md`, and
`mail/<account>/<folder>.md`.

`--since <RFC3339>` limits items by update time; `--limit <n>` caps each
collection. The command prints source, status, counts, and a sanitized error.
Connector run records are not persisted yet.

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
./bin/brain note --project demo --title "Team note" --body "Git smoke check" \
  --config ~/brain-data/brain.yaml
./bin/brain sync --config ~/brain-data/brain.yaml --push
./bin/brain sync --status --config ~/brain-data/brain.yaml
```

`brain sync` stages non-ignored files, commits when dirty, fetches, rebases onto
the upstream branch, and pushes only with `--push`. It never force-pushes and
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
the server falls back to an in-memory store and loses data on restart. See
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

Authenticate over HTTP and mint an API token for scripts:

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
repository. Send either token as `Authorization: Bearer <token>`.

Other useful endpoints:

- `GET /openapi.json` — OpenAPI 3 document.
- `GET /v1/projects`, `GET /v1/search?q=...` — authenticated read paths.
- `GET /v1/events` — SSE revision stream with heartbeat and `Last-Event-ID`.

Point the CLI at the server and supply the token from the login response:

```sh
./bin/brain init --mode server --server-url http://127.0.0.1:8080 \
  --config ~/brain-server.yaml
export BRAIN_TOKEN="<access-token>"
./bin/brain doctor --config ~/brain-server.yaml
./bin/brain note --project demo --title "Server note" --body "Server smoke" \
  --config ~/brain-server.yaml
```

Instead of an environment variable, store the token in the OS secret store when
a platform secret service is available:

```sh
printf '%s\n' '<access-token>' | ./bin/brain secret set server-token
```

`brain secret set` reads the value from stdin, prints only the secret name, and
never writes the value to `brain.yaml`. The command itself does not echo the
value, but the terminal or shell history may record it; use the method your
environment protects. If no secret service exists, keep using `BRAIN_TOKEN`.

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

There is no `brain login` command yet; obtain the token through
`POST /v1/auth/login` or an API token minted at `POST /v1/tokens`.
`brain sync` is not implemented in server mode — the API is the sync path.
`brain index` and `brain search` are local-only; use `GET /v1/search?q=...` for
server-side search. Server-side Markdown import/export is not wired yet.
`brain doctor` fails the `server` check when the URL is wrong or the token is
missing/expired.

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
- `401` on server requests — set `BRAIN_TOKEN` or run
  `brain secret set server-token`, then retry. JWT access tokens expire after 15
  minutes by default; refresh tokens rotate.
- `server client is not configured` from `doctor` — the URL is missing or no
  token was found in `BRAIN_TOKEN` / the OS secret store.
- `local search index is not available` from `search` — run `brain index` (or
  `brain index --rebuild`) against the configured data directory.
- `search uses the local index; server search is not wired` — the CLI search
  command is local-only; use `GET /v1/search?q=...` in server mode.
- `sync in server mode: not implemented` — server mode does not use Git sync;
  reads and writes go through the HTTP API.
