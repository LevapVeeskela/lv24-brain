---
title: LV24 Brain — public product and private data repos
date: 2026-11-05
status: approved-for-implementation
owner: LevapVeeskela
license: Apache-2.0
---

# LV24 Brain

## 1. Decision summary

- Public repo: `LevapVeeskela/lv24-brain` — Apache-2.0, Go monorepo.
- Private repo: `LevapVeeskela/lv24-brain-data` — data-only instance.
- Go core v1; Rust components (heavy index/embedding) later behind stable interfaces.
- Three install modes: `local`, `git`, `server`.
- Server v1: single-node, Postgres, OIDC, JWT/API tokens, roles
  `admin|member|viewer`, SSE, Docker Compose.
- Markdown is source of truth in local/git modes. In server mode Postgres is
  canonical, with Markdown export/import for backup and portability.
- Auto-update is part of v1: CLI self-update, server opt-in update, signed
  release manifest and rollback.
- Team-first sync is Git-backed by default; server mode is an alternative
  selected by the user during install.
- Private repo contains all brain data except secrets, indexes, and backups.
- Zero paid GitHub/GitLab services. Only free public/private repos and GitHub
  Actions within the free allowance.

## 2. Repository split

### Public `lv24-brain`

Contains only generic product code, docs, templates, installers, tests.
No user data, mail, connector dumps, machine paths, credentials, or private
project notes.

```text
cmd/brain/                    CLI entrypoint
cmd/brain-server/             HTTP server entrypoint
internal/domain/              entities and value objects
internal/app/                 use cases
internal/ports/               Store, Index, Sync, Secrets, Connectors, Auth
internal/adapters/fsstore/    Markdown + YAML frontmatter
internal/adapters/gitstore/   git-backed team store
internal/adapters/sqliteindex/FTS5 local index
internal/adapters/postgres/   server metadata/content
internal/adapters/httpapi/    REST, SSE, OpenAPI
internal/adapters/oidc/       generic OIDC provider
internal/adapters/imap/       IMAP connector
internal/adapters/github/     GitHub connector
internal/adapters/gitlab/     GitLab connector
internal/auth/                JWT, API tokens, roles
internal/config/              brain.yaml, mode selection
internal/update/              release check, verify, stage, replace
templates/                    state/decision/attempt/handoff/AGENTS
installers/windows/           install.ps1 + install.bat
integrations/opencode/        session hook plugin
deploy/                       Dockerfile, docker-compose.yml, Postgres
docs/                         quickstart, modes, architecture, security
tests/                        unit, integration, e2e, contract
```

Module path: `github.com/levapveeskela/lv24-brain`.

### Private `lv24-brain-data`

Data-only:

```text
brain.yaml                    instance config and data schema version
projects/<id>/                state.md, notes, decisions, attempts, handoffs
mail/<account>/               IMAP notes (user-approved private data)
connectors/github/            GitHub sync dumps
connectors/gitlab/            GitLab sync dumps
connectors/mail.json          account metadata (no passwords/tokens)
connectors/mail-state.json    sync cursors
journal/                      user journals and sync logs
registry.json                 project registry
global/                       private conventions
```

Never commit: `secrets/`, `index/`, `backups/`, `*.db`, `*.sqlite*`,
OS keychain exports, `.env`, access tokens, passwords.

## 3. Modes

| Mode | Store | Index | Sync | Use |
|---|---|---|---|---|
| `local` | filesystem | SQLite FTS5 | none | single user |
| `git` | filesystem + git | SQLite FTS5 | pull/rebase/commit/push | team default |
| `server` | HTTP API | Postgres | API + SSE | central team server |

`brain.yaml` records the mode, server URL, user, project registry path, data
schema version, minimum engine version, and update channel.

The CLI, server, and adapters share `internal/domain` and `internal/app`.
Adapters are selected at bootstrap through `internal/ports`; domain/app must
not import OS, git, HTTP, SQL, or connector packages.

## 4. Domain model

- `Project`: stable id, name, path(s), aliases, kind, repo metadata.
- `Note`: dated Markdown section attached to a project.
- `Decision`: title, status, context, alternatives, consequences, evidence.
- `Attempt`: title, hypothesis, result, why it failed.
- `Handoff`: summary + git context + next action + timestamp.
- `State`: goal, active, blocked, next action, open questions, freshness.
- `User`: id, login, display name, role, auth source.
- `Role`: `admin`, `member`, `viewer`.
- `SyncRevision`: git commit or server revision used for optimistic checks.
- `ConnectorRun`: source, started/finished, status, counts, error.

All user-facing Markdown uses YAML frontmatter with `project_id`, `kind`,
`updated_at`, `schema_version`, and optional `permalink`.

## 5. Sync model

### Git mode

- One private data repo is the shared brain.
- Each user owns `journal/<user>.jsonl`; do not edit another user's journal.
- Shared knowledge files are edited normally; conflicts are resolved by Git.
- `brain sync` performs `fetch`, `pull --rebase`, optional commit, push.
- `brain sync --status` is read-only and shows divergence/conflicts.
- `brain doctor` checks data schema and engine compatibility.
- No force-push; no automatic destructive resolution.
- Connector dumps and mail notes are ordinary tracked data in the private repo.
- Secrets are never written into the repo.

### Server mode

- Server is canonical; CLI talks to the HTTP API.
- Postgres stores users, roles, projects, notes, decisions, attempts,
  handoffs, connector runs, revisions, and audit events.
- SSE endpoint pushes revision/change notifications; no CRDT in v1.
- Optimistic concurrency by `revision`; conflicts return `409` with current
  revision and a merge hint.
- Markdown export/import is available for backup and migration.
- `brain-server` runs migrations on startup only after a backup/health gate;
  update path blocks while migrations or active jobs are running.

## 6. Server and auth

- Go HTTP server, single binary, `net/http` router.
- Postgres via `pgx`; SQL migrations checked into `internal/adapters/postgres/migrations`.
- Auth:
  - local accounts: email + password hash (Argon2id) for bootstrap/admin;
  - OIDC: authorization code + PKCE, generic issuer/client config;
  - JWT access tokens (short TTL) + refresh rotation;
  - API tokens for CLI/connectors with scoped permissions.
- Roles:
  - `viewer`: read/search/export;
  - `member`: write own/shared knowledge, run own sync;
  - `admin`: users, roles, connectors, server settings, backups.
- OIDC group mapping is configurable; default maps groups to roles.
- `/health`, `/ready`, `/version`, `/metrics` (optional, no secrets).
- SSE: `GET /v1/events` with heartbeat; clients reconnect with `Last-Event-ID`.
- OpenAPI 3 generated/checked in CI; Swagger UI optional.
- Docker Compose: `brain-server` + Postgres + volume; no cloud dependency.

## 7. Connectors

- GitHub: token via secret store/env, read-only repository metadata, PRs,
  issues, recent commits; markdown dumps.
- GitLab: read-only PAT, visible projects, MRs/issues/commits.
- IMAP: read-only, `BODY.PEEK`, metadata/snippets only, configurable folders,
  per-account state; never send/delete/move/flag.
- Connector runs are idempotent and write `ConnectorRun` records.
- Local/git: connectors write Markdown; server: server-side runs write
  Postgres and export Markdown on demand.
- Secrets: OS keychain or env; never in `brain.yaml`, logs, or repo.
- Rate limits and backoff; no paid third-party services.

## 8. Install, update, release

### Install modes

`installers/windows/install.ps1` asks:
1. `local` — install binary, init brain dir, optional OpenCode hook.
2. `git` — clone private data repo or point to existing clone, configure sync.
3. `server` — enter server URL, `brain login`, optional OIDC.

Batch wrapper `install.bat` calls the PowerShell installer. macOS/Linux install
script is a thin shell wrapper around the same release assets.

### Auto-update

- `brain update --check`, `brain update --yes`, `--channel stable|beta`.
- Daily background check; disable with `BRAIN_NO_UPDATE=1`.
- Download to `%LOCALAPPDATA%\lv24-brain\updates` (or platform equivalent).
- Verify SHA-256 and optional cosign/Sigstore signature.
  Status: implemented as a required Ed25519 detached signature
  (`release.json.sig`) over the manifest, checked against keys embedded in the
  binary. cosign/Sigstore is not used. See `docs/update.md` (Signatures).
- Windows: helper replaces binary after process exit; keep previous binary for
  rollback. Unix: atomic rename; same rollback.
- `brain doctor` reports engine/data schema compatibility.
- Server auto-update is opt-in (`BRAIN_SERVER_AUTO_UPDATE=1`); default is
  notify-only. Docker images pinned by digest; rollback to previous digest.
  Status: server auto-update is planned; not implemented in v1.
  `BRAIN_SERVER_AUTO_UPDATE` is a placeholder variable with no effect. Server
  upgrades replace the image.
  Status: `deploy/Dockerfile` and `deploy/docker-compose.yml` pin images by tag
  (`golang:1.27-alpine`, `alpine:3.22`, `postgres:17-alpine`; the server image is
  built locally as `lv24-brain-server:local`). Digest pinning is a
  deployment-time recommendation; the repository does not enforce it.
- Update never mutates the private data repo automatically.
- Release manifest `release.json`: version, channel, assets, checksums,
  min engine/schema version, signature metadata.

### Releases

- GitHub Releases + GoReleaser.
- Windows/macOS/Linux, amd64/arm64.
- `checksums.txt`, `release.json`, optional signature.
  Status: `release.json.sig` is published with every release and is required
  by the updater (see the status note under Auto-update).
- Public CI: GitHub Actions only within free allowance; no paid runners or
  storage add-ons.

## 9. Security and privacy

- Public repo is data-free; CI secret scan and `git grep` guard.
- Private data repo: secret scan pre-commit; `.gitignore` for secrets/indexes.
- Mail/connector data is explicitly approved private data; it is still PII.
- Redact secrets in logs; never print tokens; no full mail bodies unless a
  future explicit mode is designed.
- Server: password hashing Argon2id, JWT rotation, OIDC state/nonce/PKCE,
  CSRF-safe cookie settings if browser sessions are added, rate limits.
- Audit log for admin/role/connector changes.
- Backup/restore documented; encrypted backups recommended.

## 10. Testing and DoD

- `go test ./...`, `go vet ./...`, `gofmt` check.
- Unit tests for domain/app/ports; adapter tests with temp dirs and SQLite.
- Integration: git mode with a temp bare remote; server with Postgres container.
- E2E: install local, init, note/decision/attempt/handoff/bootstrap/review.
- Server: OIDC mock, roles, 401/403/409, SSE reconnect, migrations, backup.
- Installer smoke on Windows; release manifest verification; updater rollback.
- Secret scan; no personal paths/data in public repo.
- Docs: README quickstart for all three modes, architecture, security, update,
  migration, team runbook.
  Status: the team runbook is `docs/runbook.md`.
- Public repo CI green; private data repo only secret scan (no build).

## 11. Migration from current `.brain`

1. Create `lv24-brain-data` from current `.brain` data directories only.
2. Copy `projects/`, `mail/`, `connectors/`, `journal/`, `registry.json`,
   `global/`; add `brain.yaml`; exclude secrets/indexes/backups.
3. Run secret scan; validate no tokens/passwords.
4. Initial private commit; push to private GitHub.
5. Implement Go engine; `brain migrate --from <brain-data>` imports data
   idempotently and writes `schema_version`.
6. Keep current `.brain` read-only fallback until Go engine passes parity tests.
7. Switch OpenCode/agents to Go binary; archive legacy PowerShell in private
   local history, not in the public repo.

## 12. Work packages

1. Foundation: module, domain, ports, config, CLI skeleton, brain.yaml.
2. Filesystem store + Markdown/frontmatter + templates + migration.
3. SQLite index + search + bootstrap/review/status/doctor.
4. Git sync + per-user journal + conflict/status handling.
5. Postgres schema/migrations + HTTP API + auth/JWT/roles + OIDC.
6. SSE + server CLI client + server-mode store adapter.
7. Connectors: GitHub, GitLab, IMAP + secret store + runs.
8. Installers, OpenCode hook, auto-update, releases, Docker.
9. Tests, docs, security scan, CI, migration validation.
10. Private data repo packaging and first private commit/push.
11. Public repo first commit/push, release dry-run.

## 13. Non-goals v1

- CRDT collaborative editing / real-time co-editing.
- Multi-tenant SaaS, billing, organization isolation beyond one server.
- Mobile app.
- Paid hosting, paid CI, marketplace integrations.
- Automatic mail body storage; metadata/snippets only.
- Rust components in v1 (interfaces reserved).

## 14. Acceptance criteria

- `brain` runs on Windows/macOS/Linux as a single binary.
- `brain init`, `note`, `decision`, `attempt`, `handoff`, `bootstrap`, `review`,
  `doctor`, `sync`, `update` work.
- Local, git, and server modes are installable and documented.
- Server supports OIDC + local admin + roles + SSE + Postgres migrations.
- Auto-update verifies checksums, supports rollback, and can be disabled.
- Public repo contains no private data; private repo contains no secrets.
- Migration from current `.brain` is reversible and verified on a copy.
- `go test ./...`, installer smoke, Docker Compose smoke, secret scan pass.
