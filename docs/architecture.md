# Architecture

Compact map of the engine. The approved product design is
`lv24/main/docs/plans/2026-11-05-lv24-brain-product-design.md`.

## Repository split

- `lv24-brain` (this repository): generic product code, docs, templates, tests.
  No user data, credentials, connector dumps, mail, or machine paths.
- `lv24-brain-data` (private): `brain.yaml`, `projects/`, `mail/`, `connectors/`,
  `journal/`, `registry.json`, `global/`. Never contains secrets, indexes, or
  backups.

## Modes

| Mode | Store | Index | Sync | Audience |
|---|---|---|---|---|
| `local` | filesystem | SQLite FTS5 | none | single user |
| `git` | filesystem + git | SQLite FTS5 | pull/rebase/commit/push | team default |
| `server` | HTTP API | Postgres | API + SSE | central server |

`brain.yaml` records `mode`, `data_dir`, `server_url`, `user`,
`schema_version`, `min_engine_version`, and `update_channel`.

## Layers and dependency rule

```text
cmd/*            composition roots
internal/app     use cases; depends on domain + ports (+ config)
internal/ports   interfaces only
internal/domain  pure entities, value objects, validation errors
```

Dependencies point inward. `internal/domain` imports no project packages and no
I/O libraries. `internal/ports` imports only `context`, `time`, and domain.
Adapters implement ports and are selected at bootstrap; adapters never leak
into domain or app.

Planned adapters: `fsstore` (Markdown + YAML frontmatter), `gitstore`,
`sqliteindex` (FTS5), `postgres`, `httpapi`, `oidc`, `imap`, `github`, `gitlab`.
Heavy index/embedding work may move to Rust behind the same interfaces.

## Domain model

- `Project`: stable id, name, paths, aliases, kind, repository metadata.
- `Note`: dated Markdown section attached to a project.
- `Decision`: title, status, context, alternatives, consequences, evidence.
- `Attempt`: title, hypothesis, result, why it failed.
- `Handoff`: summary, git context, next action, timestamp.
- `State`: goal, active, blocked, next action, open questions, freshness.
- `User`, `Role` (`admin|member|viewer`), `AuthSource`.
- `SyncRevision`: git commit or server revision for optimistic checks.
- `ConnectorRun`: source, start/finish, status, counts, error.

Every user-facing Markdown file uses YAML frontmatter with `project_id`, `kind`,
`updated_at`, `schema_version`, and optional `permalink`.

## Sync

### Git

One private data repository is the shared brain. Each user owns
`journal/<user>.jsonl`; shared knowledge files are edited normally and
conflicts are resolved by Git. `brain sync` runs fetch, `pull --rebase`,
optional commit, push. `brain sync --status` is read-only. No force-push and no
automatic destructive resolution. Connector dumps and approved mail notes are
ordinary tracked data.

### Server

The server is canonical. Postgres stores users, roles, projects, knowledge,
connector runs, revisions, and audit events. SSE notifies clients of revisions;
there is no CRDT in v1. Writes use optimistic concurrency by `revision`; a
conflict returns `409` with the current revision and a merge hint. Markdown
export/import provides backup and migration.

## Server surface (planned)

- `GET /health`, `GET /ready`, `GET /version` (implemented in the scaffold).
- `GET /v1/events` SSE with heartbeat and `Last-Event-ID` reconnect.
- OpenAPI checked in CI; Swagger UI optional.
- Single binary, `net/http`, Postgres via `pgx`.

## Connectors (planned)

GitHub, GitLab, and IMAP are read-only. IMAP uses `BODY.PEEK`, metadata and
snippets only, never send/delete/move/flag. Runs are idempotent and write
`ConnectorRun` records. Credentials come from the OS keychain or environment,
never from `brain.yaml`, logs, or the repository.

## Update (planned)

`brain update --check|--yes`, channels `stable|beta`. Downloads are verified by
SHA-256 and optional Sigstore signature, staged, then atomically replaced with
the previous binary kept for rollback. Update never mutates the data repository.

## Work packages

1. Foundation (this scaffold).
2. Filesystem store, Markdown/frontmatter, templates, migration.
3. SQLite index, search, bootstrap/review/status/doctor.
4. Git sync, journals, conflict handling.
5. Postgres, HTTP API, auth/JWT/roles, OIDC.
6. SSE, server-mode client store.
7. Connectors and secret store.
8. Installers, OpenCode hook, auto-update, releases, Docker.
9. Tests, docs, security scan, CI, migration validation.
10. Private data repository packaging.
11. First public release dry-run.

## Non-goals v1

CRDT co-editing, multi-tenant SaaS, billing, mobile app, paid hosting or CI,
automatic mail body storage, and Rust components (interfaces are reserved).
