# Project status and release history

Last verified: 2026-11-27. This page is the single home for release state, shipped capability,
known gaps and the roadmap. The product front door is [../README.md](../README.md); the system
design is [ARCHITECTURE.md](ARCHITECTURE.md).

## Release state

| Item | Value |
|---|---|
| Engine version | `0.28.0` (release commit; `v0.28.0` tag pending) |
| Repository HEAD | this release commit — "release: 0.28.0" (2026-11-27) |
| npm | `lv24-brain@0.27.2` is the latest published (registry check, 2026-11-27); 0.28.0 not published yet |
| PyPI launcher | `lv24-brain==0.27.2` is the latest published (registry check, 2026-11-27); 0.28.0 not published yet |
| Go | 1.27 (`go.mod`) |
| Data schema | 1 (Markdown + YAML frontmatter) |
| Local index schema | 11 (SQLite FTS5) |
| Postgres schema | 10 (server mode) |
| MCP profile | `lv24`, 18 tools plus `memory://` resources |
| Server image | `ghcr.io/levapveeskela/lv24-brain-server` (`linux/amd64`, `linux/arm64`) |
| Docs site | <https://brain.lv24.by> (en / ru / zh) |
| License | Apache-2.0 |

## Requirements

- No Go toolchain is needed to run a release binary.
- Go 1.27 or newer builds the engine from source.
- Git is required only for `git` mode and source checkouts.
- Docker with Compose v2 is optional and only for the server deployment files.
- Node.js 18 or newer is required for the npm package and the PyPI launcher.

## Shipped capability

| Area | State |
|---|---|
| CLI | `init`, `doctor`, `onboard`, `open`, `project`, `bootstrap`, `context`, `note`, `decision`, `attempt`, `handoff`, `idea`, `status`, `review`, `duplicates`, `consolidate`, `promote`, `sync`, `index`, `search`, `get`, `timeline`, `eval`, `journal`, `event`, `hooks`, `config`, `discover`, `graphs`, `code`, `agents`, `state`, `record`, `prompt`, `relation`, `links`, `catchup`, `digest`, `session`, `mcp`, `connector`, `connection`, `agent`, `migrate`, `export`, `import`, `secret`, `login`, `logout`, `password`, `token`, `serve`, `update`, `completion`. `brain help` is authoritative; the docs site has the [CLI reference](https://brain.lv24.by/reference/cli/). |
| Storage | `fsstore`: Markdown + YAML frontmatter, atomic writes, `registry.json`, per-project state/notes/decisions/attempts/handoffs/ideas, machine-local proposals, local raw zone. Data schema 1. |
| Ideas | One Markdown file per idea under `projects/<id>/ideas/` (data schema 1): statuses `open`/`doing`/`done`/`dropped`/`someday`, timed `due_at` or all-day `due_date`, priority, tags, reminder lead, reversible trash and problem rows for conflicted files. CLI `brain idea`, MCP `brain_idea`, local API `/v1/ideas`, a capped session-start block and a one-way `.ics` export; local and git modes only. |
| Local search | SQLite FTS5 index (`index/brain.db`, schema 11), English and Russian word-form prefix matching, `--since`, validity filters, support tiers (`supported` / `related` / `none`), ranked signals, `--explain` and the `idea` kind with due/priority filters. Raw items are opt-in. |
| Git sync | `fetch`, `pull --rebase`, commit, optional push, `--status [--json]`, `--watch`; interprocess lock, retry with backoff, never force-push, exit 3 on conflict; deterministic merge drivers for state, notes, registry and raw items; conflict artifacts are committed. |
| Server | Single Go binary on `net/http`; health/readiness/version/OpenAPI; JWT access + rotating refresh; API tokens with scopes and project allow-lists; OIDC PKCE/state/nonce; roles `admin`/`member`/`viewer`; per-project ACL; audit log; SSE with `Last-Event-ID` and single-use tickets; Postgres schema 10 with forward-only migrations; in-memory fallback for development. |
| MCP | `brain mcp` over stdio, profile `lv24`, 18 tools: open/context/search/record/catchup/review/projects/graph/capabilities (read), note/decision/attempt/handoff (append), code/idea (mixed; read-only keeps 11 tools), state/judge/prompt (destructive). Protocol 2026-07-28 and 2025-06-18. |
| Connectors | Native item-level sources: `folders`, `jira`, `confluence`, `linear`, `slack`, `notion`, `sentry`, `postgres-schema`, `sqlite-schema`, `rss`, `web`, `mcp`; legacy read-only GitHub/GitLab/IMAP adapters. Per-connection checkpoints, bounded responses, no inferred deletes, raw zone local-only by default unless explicitly shared. |
| Background agent | `brain agent`: foreground engine with one process per data dir, persisted schedules, bounded workers, watchdog, rotating redacted logs, and the authenticated desktop local API (SSE run logs, connections, projects, memory routes, merge, digest, sync, update, shutdown). |
| Desktop | `localapi-v1` is frozen and additive; the Tauri desktop app lives in the separate `lv24-brain-desktop` repository (1.7.2 at the last check). |
| Distribution | npm `lv24-brain` with platform packages; PyPI thin launcher that delegates to the same npm version; signed Windows PowerShell/BAT and macOS/Linux bash installers; GitHub Releases; GHCR server image; `brain update` with Ed25519-signed manifest, SHA-256 verification and rollback. |
| Migration | `brain migrate` imports legacy `.brain` knowledge (idempotent, dry-run, backup manifest, rollback), and engram import is supported. |

## Known gaps

- **Server auto-update is not implemented.** `BRAIN_SERVER_AUTO_UPDATE` is a reserved placeholder;
  upgrade by replacing the image.
- **Multi-replica server deployment is unsupported.** The SSE broker and login rate limiter are
  per-process even with durable Postgres tokens.
- **Server raw items require Postgres.** The in-memory development store returns `unsupported` for
  connector runs instead of discarding output. There is no server-side Markdown import/export.
- **`brain sync` and `brain index` are local/git only.** In server mode the API is the sync path and
  search goes through `GET /v1/search`.
- **There is no CLI OIDC flow.** `brain login` supports local accounts; OIDC is browser-based.
- **Curation is local and rule-based.** `brain digest --source` reads already-stored items and writes
  machine-local, git-ignored proposals; `brain promote` is the only path into curated records. There
  is no scheduled digest and no proposal merge/trash.
- **Raw context is local/git only.** `brain context --raw` and raw search paths require an exact
  grant in `context-allow.json`; server mode never returns raw items to viewers or members.
- **Providers never infer deletes.** A missing item is not a tombstone unless the upstream reports
  one.
- **Phase 1 connector limits.** `folders` reads Markdown/text/RST/HTML only; Jira Data Center and
  Confluence page-offset backfills can miss an item updated during a long pass; Slack reads only
  listed channels and needs a user token for older edits; Notion has no `updated_since`; Sentry
  fetches at most three newest events per issue; schema sources read catalogs only; RSS/web never
  infer deletes; the `mcp` source is recent-results-only.
- **Relations are store-and-index only.** Typed relations and the judged graph are persisted and
  projected, but no read path uses them for neighbour navigation yet; `brain timeline` and the
  context `linked` layer still follow `[[id]]` links.
- **Merge is CLI-only.** `brain record merge` plans, applies and rolls back in local/git mode; no MCP
  tool and no server-mode route apply, trash or hard-delete records.
- **Ideas are local/git only.** Server mode returns `unsupported` for every ideas route and CLI
  action; the domain model is ready for a later Postgres table. There is no custom idea merge
  driver: a conflicted idea file is reported as a problem row and must be resolved in git.
- **Idea reminders are desktop-only.** The scheduler runs while the desktop app is open or in the
  tray; quitting the app stops reminders, and a missed trigger becomes one digest on the next
  launch. `.ics` is a one-way file export, not a calendar subscription, and there is no two-way
  Google/Apple/CalDAV sync.

## Roadmap

| Item | State |
|---|---|
| 0.28 Ideas follow-ups | Server-mode storage, an `lv24-ideas` merge driver, OS-scheduled reminders and live calendar subscriptions are candidates after 0.28.0. None has a date. |
| Later | Server auto-update, multi-replica coordination, richer relation navigation, scheduled digest/merge for proposals. No dates committed. |

## Release history

| Release | Theme |
|---|---|
| [0.28.0](release-0.28.0.md) | Ideas: an agent-first TODO layer |
| [0.27.2](release-0.27.2.md) | No console windows from background helpers |
| [0.27.1](release-0.27.1.md) | Review before retirement |
| [0.27.0](release-0.27.0.md) | Honest retrieval and index integrity |
| [0.26.3](release-0.26.3.md) | Connector checkpoint recovery, wider background budget |
| [0.26.2](release-0.26.2.md) | Native connector redirects, background run budget |
| [0.26.1](release-0.26.1.md) | Hidden Windows tasks, moved repositories |
| [0.26.0](release-0.26.0.md) | Ranking, budgets, time and identity |
| [0.25.0](release-0.25.0.md) | Second-brain correctness |
| [0.24.0](release-0.24.0.md) | Path provenance backfill |
| [0.23.0](release-0.23.0.md) | Desktop git sync, machine provenance, onboarding |
| [0.22.2](release-0.22.2.md) | Postgres migration and provenance fix |
| [0.22.1](release-0.22.1.md) | `sync --watch` and the three-machine proof |
| [0.22.0](release-0.22.md) | PII untrack, legacy profile removal, auto-sync |
| [0.21.0](release-0.21.md) | Consolidation, review routes, multi-machine hardening |
| [0.20.0](release-0.20.md) | State provenance, server review, pin/unpin, LvEF exchange, graph surprises |
| [0.19.0](release-0.19.md) | Python extraction, graph analytics, tags and snooze |
| [0.18.0](release-0.18.md) | Code graph, one MCP, durable memory |
| [0.17.0](release-0.17.md) | One MCP surface, engram-compatible memory |
| [0.16.0](release-0.16.md) | Memory quality |
| [0.15.0](release-0.15.md) | Correctness, multi-writer, one protocol |

## Verification

CI (`ci.yml`) runs four free-tier jobs on GitHub-hosted runners: `test` (gofmt, vet, `go test`,
builds, JS syntax, Compose config), `postgres` (integration tests against `postgres:16-alpine` with
skip guards), `race` (`go test -race`, pushes only), and `release-check` (GoReleaser snapshot and
asset/manifest validation). Release (`release.yml`) runs GoReleaser on `v*` tags, writes
`checksums.txt` and `release.json`, signs the manifest, and never overwrites an existing release.

For a local check:

```sh
go test ./...
go vet ./...
gofmt -l .
```

The retrieval metric run (`brain eval`) is local, not a CI gate. Postgres integration tests need
`BRAIN_TEST_DATABASE_URL`.

## Security boundary

- Public repository: product code, docs, templates, installers and tests only.
- Private repository or local data directory: brain content and approved mail/connector data.
- Never commit secrets, tokens, passwords, indexes, databases or backups.
- Connector and server credentials resolve from the environment or the OS secret store at runtime;
  they are never written to `brain.yaml`.

See [security.md](security.md) for the full model.
