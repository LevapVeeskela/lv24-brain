# Deploy a brain server

The v1 server is single-node: one `brain-server` container plus Postgres,
managed by Docker Compose. There is no cloud dependency and no paid service.

## Files

| File | Purpose |
|---|---|
| `deploy/Dockerfile` | Multi-stage build of `brain-server`; non-root Alpine runtime. |
| `deploy/docker-compose.yml` | `brain-server` + Postgres, healthchecks, volumes. |
| `deploy/.env.example` | Environment template. Copy to `deploy/.env`; never commit it. |
| `deploy/goreleaser.yaml` | Release binaries used by the CLI updater. |

## Requirements

- Docker Engine 24+ with Compose v2.
- A host reachable over HTTPS through a reverse proxy you control.
- A backup location outside the Docker volumes.

Postgres is published only on the internal Compose network. Do not add a
`ports:` mapping for `5432`; there is no public database access in v1.

## Quick start

```sh
cd deploy
cp .env.example .env
# Fill every required value in .env.
docker compose --env-file .env -f docker-compose.yml up -d --build
docker compose --env-file .env -f docker-compose.yml ps
```

PowerShell uses the same commands.

Generate local secrets (never reuse these examples):

```sh
openssl rand -base64 48
```

```powershell
$bytes = New-Object byte[] 48
[Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
[Convert]::ToBase64String($bytes)
```

`BRAIN_DB_PASSWORD` and `BRAIN_DATABASE_URL` are separate values on purpose.
Keep the password in the URL in sync with `BRAIN_DB_PASSWORD`, and
percent-encode URL-special characters in the URL form. `BRAIN_JWT_SECRET` must
be at least 32 bytes.

Check health:

```sh
curl -fsS http://127.0.0.1:8080/health
curl -fsS http://127.0.0.1:8080/ready
curl -fsS http://127.0.0.1:8080/version
```

The container is read-only apart from `/tmp` and the `brain_data` volume. It
runs as UID/GID `10001` and drops new privileges.

## Environment

| Variable | Required | Meaning |
|---|---|---|
| `BRAIN_JWT_SECRET` | yes | JWT signing key, at least 32 bytes. |
| `BRAIN_DATABASE_URL` | yes | Postgres URL for server persistence. |
| `BRAIN_DB_PASSWORD` | yes | Postgres password; keep in sync with the URL. |
| `BRAIN_BOOTSTRAP_ADMIN_LOGIN` | no | First local administrator login. |
| `BRAIN_BOOTSTRAP_ADMIN_PASSWORD` | no | First administrator password; set with the login. |
| `BRAIN_OIDC_ISSUER` | no | OIDC issuer URL; empty disables OIDC. |
| `BRAIN_OIDC_CLIENT_ID` | with OIDC | OIDC client id. |
| `BRAIN_OIDC_CLIENT_SECRET` | no | OIDC client secret for confidential clients. |
| `BRAIN_OIDC_REDIRECT_URL` | with OIDC | Public callback URL. |
| `BRAIN_BIND_ADDR` | no | Host bind address, default `127.0.0.1`. |
| `BRAIN_PORT` | no | Host port, default `8080`. |
| `BRAIN_SERVER_AUTO_UPDATE` | no | Opt-in server auto-update, default `0`. |

Keep `deploy/.env` out of Git. The repository `.gitignore` already ignores
`.env` while allowing `.env.example`.

## Reverse proxy and TLS

Terminate TLS at a reverse proxy and forward to `127.0.0.1:8080`. Set
`BRAIN_OIDC_REDIRECT_URL` to the public HTTPS callback URL. Do not expose
Postgres through the proxy. If the server is reachable directly from another
host, set `BRAIN_BIND_ADDR=0.0.0.0` deliberately and firewall all ports except
HTTPS.

## Backup

Back up Postgres with `pg_dump` while the server is running; the custom format
is restorable and compresses well:

```sh
cd deploy
docker compose --env-file .env -f docker-compose.yml exec -T postgres \
  pg_dump -U brain -d brain -Fc -f /tmp/brain.dump
docker compose --env-file .env -f docker-compose.yml cp \
  postgres:/tmp/brain.dump ./brain-$(date +%F).dump
```

`docker compose cp` works the same on Windows and avoids binary redirection
problems in Windows PowerShell. Store backups outside the host or encrypt
them; they contain private brain data.

Markdown export/import is the portability path and is preferred for migration
between servers. Backups are never committed to the private data repository
either; `backups/` is excluded.

## Restore

Restoring over an existing database replaces data. Stop the server, take a
fresh backup first, and perform a restore only after an explicit decision:

```sh
cd deploy
docker compose --env-file .env -f docker-compose.yml stop brain-server
docker compose --env-file .env -f docker-compose.yml cp \
  ./brain-2026-11-05.dump postgres:/tmp/brain.dump
docker compose --env-file .env -f docker-compose.yml exec -T postgres \
  pg_restore -U brain -d brain --clean --if-exists /tmp/brain.dump
docker compose --env-file .env -f docker-compose.yml start brain-server
```

`--clean --if-exists` is destructive. Prefer restoring into a fresh database
and validating before switching the server to it.

A volume snapshot is an alternative when the stack is stopped:

```sh
docker compose --env-file .env -f docker-compose.yml stop
docker run --rm -v lv24-brain_brain_pgdata:/data -v "$PWD":/backup \
  alpine tar czf /backup/brain-pgdata.tgz -C /data .
docker compose --env-file .env -f docker-compose.yml start
```

## Upgrade and rollback

Server updates are opt-in and notify-only by default
(`BRAIN_SERVER_AUTO_UPDATE=0`). For containers, pin the image tag or digest and
roll back by starting the previous one:

```sh
docker compose --env-file .env -f docker-compose.yml build brain-server
docker compose --env-file .env -f docker-compose.yml up -d brain-server
```

Before an upgrade, take a backup. In server mode, migrations are designed to
run on startup only after the backup/health gate; the update path must not run
while migrations or active jobs are in progress. Roll back by restoring the
previous image digest (and the matching database backup if a migration ran).
Data directories and Postgres volumes are never deleted by an update.

## Healthchecks

- Postgres: `pg_isready -U brain -d brain`.
- Server: `GET /health` every 15 seconds.
- Compose waits for Postgres health before starting `brain-server`.
- `GET /ready` and `GET /version` are available for external monitoring.

## Current scaffold status

Postgres and the healthchecks are provisioned by this stack. The foundation
scaffold currently serves the HTTP API from an in-memory store; the Postgres
store adapter is wired by the server-mode work package. Until that wiring
lands, treat this Compose stack as an integration/deployment fixture, not as a
durable production data store.
