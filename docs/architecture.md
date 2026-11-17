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
  or backups. The background agent keeps private runtime/schedule/log state in
  `agent/` (0600, self-ignored, never published).

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
  sourceoauth/             PKCE loopback, GitHub device flow, token refresh
  github/, gitlab/         read-only repository connectors
  imap/                    read-only IMAP connector
  folders/                 Markdown/Obsidian/Logseq folders
  jira/, confluence/       Jira and Confluence Cloud/Data Center
  linear/                  Linear issues
  slack/, notion/          Slack channels and Notion pages/databases
  sentry/                  Sentry issues with bounded events
  dbschema/                PostgreSQL/SQLite catalog-only schema snapshots
  feeds/                   RSS/Atom and sitemap-driven web pages
  mcpsource/               MCP servers over stdio/streamable HTTP
  connectorbuild/          source registry and native factory wiring
  localapi/                authenticated loopback control API
internal/connectors/       shared run records, retry/rate-limit helpers, redaction
internal/daemon/           background engine: scheduler, queue, runtime, logs
internal/agentcontrol/     local control API composition (daemon <-> localapi)
internal/migrate/          legacy .brain importer and engram importer
internal/mcpserver/        MCP server over stdio: 2026-07-28 and 2025-06-18
internal/server/           server config and process wiring
internal/update/           release check, verify, stage, apply, rollback
internal/version/          build identity
internal/textbudget/       shared token estimate for context and previews
internal/redact/           secret redaction applied before persistence
internal/mergedriver/      git merge drivers for state, registry and notes
eval/                      authored retrieval fixtures, runner and baseline
templates/                 state, decision, attempt, handoff, AGENTS.md
installers/windows/        install.ps1 and install.bat
installers/unix/           install.sh for macOS and Linux
integrations/opencode/     handoff hook template
deploy/                    Dockerfile, Compose, environment template, GoReleaser
docs/                      product, quickstart, install, update, deploy, migration, security, runbook;
                           reference pages: search, context-budget, review, relations, limits,
                           schema-compat, import-engram, eval, mcp, release-0.15/0.16/0.17
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
`schema_version`, `min_engine_version`, `update_channel`, and optional
`retired_paths` and `machine_id`. Local and Git modes require `data_dir`;
server mode requires `server_url` and has no data directory.

`machine_id` is an optional stable name for this machine (default: the OS
host name). It is provenance only: `SaveProject` stamps it as the owner of
each registry checkout path (`path_provenance`, path → machine id), and the
merge drivers write it as `- machine:` in conflict records, so a shared
registry says which machine registered what and recorded a conflict. Nothing
requires it; a registry without provenance is valid and older engines ignore
the key.

`retired_paths` maps a checkout that no longer exists to the project that
replaced it (`path` + `replaced_by`, a project id or alias). The engine ships
no entries: each workspace configures its own, e.g. `<root>/old-checkout`. A
selector inside a retired path resolves to the replacement with a warning,
`brain discover` skips it, `brain project set --path` refuses it, and
`brain doctor` reports it when the directory or a registration still exists.
Nothing is ever deleted or created by this guard.

## Data layout

Markdown is the source of truth in local and Git modes. `fsstore` uses:

```text
registry.json                          project index (managed by brain project)
context-allow.json                     explicit raw-context grants (no secrets; committed)
projects/<id>/state.md                 goal, active, blocked, next action, questions
projects/<id>/notes/YYYY-MM-DD.md      dated note sections
projects/<id>/decisions/YYYY-MM-DD-<slug>.md
projects/<id>/attempts/YYYY-MM-DD-<slug>.md
projects/<id>/handoffs/YYYY-MM-DD-HHMMSS.md
proposals/<project>/<proposal-id>.md    machine-local candidate records (ignored)
proposals/.gitignore                   "*" so nothing here is committed
connectors/runs/<UTC timestamp>-<source>-<run-id>.md
```

Every user-facing file carries YAML frontmatter with `project_id`, `kind`,
`updated_at`, `schema_version`, and optional `permalink`. Records add optional
validity keys (`valid_from`, `valid_to`, `superseded_by`, `supersedes`,
`confidence`); a current record that became true at its creation time writes
none of them. Writes are atomic
(temp file + rename). The SQLite index lives under `index/brain.db` and is
derived data: new writes upsert into it. `brain index` refreshes it from
existing Markdown, and `brain index --rebuild` deletes the database and sidecar
files before rebuilding. When a write cannot update the index, or `brain sync`
pulls commits from another machine, the engine sets `index/dirty`; sync then
reindexes at once, and the next search repairs the index if that failed.
`brain doctor` warns while the marker is set. A reindex removes entries whose
source files are gone. `context-allow.json` is a plain committed data file,
like `connectors/connections.json`, and holds no secrets. The data revision
(`CurrentRevision`) covers the top-level data directory except `secrets/`,
`index/`, `backups/`, `mail/`, `github/`, `gitlab/`, `connectors/`, `journal/`
and `proposals/`. So curated records and `brain.yaml`, `registry.json`,
`global/` and `projects/` are covered, while secrets, the index, backups,
connector dumps and runs, journal lines, and machine-local proposals are
excluded, so mail sync, journal lines and a proposal digest do not change it.

Listing never stops at one damaged file: unreadable, malformed or empty record
files are skipped and reported by `brain doctor` (check `records`). A daily
notes file that no longer parses is moved aside to
`<date>.md.corrupt-<n>` on the next note, so new notes keep working and the
damaged content is kept for repair.

Domain records: `Project`, `Note`, `Decision`, `Attempt`, `Handoff`, `State`,
`Proposal`, `User`, `Role` (`admin|member|viewer`), `AuthSource`,
`SyncRevision`, and `ConnectorRun`. `Item.State` (`open`, `closed`, `merged`,
`resolved`, `ignored`) is an optional additive field; it enters the item hash
only when non-empty, so existing raw items do not re-upsert.

## CLI

```text
brain init       create an instance and write brain.yaml
brain open       [path|id] bootstrap pack + ## Git + ## Graphs (--brief: state only; --include-unreviewed adds quarantined records; --as-of reads a past instant)
brain project    list, show, or set registry projects
brain bootstrap  build bounded agent context (--as-of, --include-superseded)
brain context    [task] context for one task (--raw appends granted raw excerpts; --include-unreviewed adds quarantined records; --as-of/--include-superseded; local/git only)
brain note       append a dated note (--supersedes, --superseded-by, --confidence)
brain decision   append a decision (same validity flags)
brain attempt    append an attempt (same validity flags)
brain handoff    append a session handoff (same validity flags)
brain status     show mode, paths, schema, engine version
brain review     state age, questions, evidence, attempts + findings (stale/contradictory knowledge, pending proposals); --fix applies heuristic state fixes (see below)
brain digest     cross-project morning summary; --source <conn> previews proposals from stored raw items (--write persists)
brain promote    promote or dismiss one proposal; the only path into curated records
brain catchup    what changed since the latest handoff (--include-unreviewed adds quarantined; --as-of reads a past instant)
brain record     list|show records (--as-of, --include-superseded/--all-time, show --history); status any record; supersede any record; review provenance; trash|restore|trash-list (local/git)
brain session    start|end handlers for agent session hooks
brain state      get|set the project state
brain sync       commit, rebase, optional push, --status, or --watch (local/git only)
brain index      refresh the local FTS5 index, or --rebuild it (local/git only)
brain search     query the local FTS5 index (local/git), or GET /v1/search (server); --dumps adds connector dumps, --kind filters, --as-of/--include-superseded select validity
brain journal    show journal/<user>.jsonl activity, newest first (local/git)
brain event      record an external event; `event commit` reads git for a post-commit hook
brain hooks      install|uninstall|status the post-commit journal hook
brain config     use <brain.yaml> | show | clear the user default config
brain discover   find git checkouts under --root and register new projects (--apply)
brain graphs     refresh graphify/codegraph indexes of registered projects
brain agents     sync the shared agent instructions (agents sync [--check])
brain mcp        serve the brain as an MCP server over stdio (for coding agents)
brain connector  run a read-only GitHub, GitLab, or IMAP connector
brain connection  manage named local/git connections
brain agent      run, inspect or stop the background connection agent
brain migrate    import a legacy .brain data directory (--rollback reverts an import)
brain secret     set, list, or remove OS-backed secrets
brain login      authenticate against a server-mode brain (local account)
brain logout     revoke the server session and clear stored tokens
brain password   change your server password (reads current and new from stdin)
brain doctor     check config, data/schema/index/git or server reachability
brain update     check, apply, or roll back a release
brain serve      run the HTTP server with development defaults
brain completion print a bash, zsh, fish or PowerShell completion script
brain version    print build identity
```

`brain review` only reports. `brain review --fix` applies heuristic state fixes:
it sets the next action to the one named by a later handoff, and removes open
questions that later decisions or notes appear to answer (the removed text is
kept in a review note). Run `brain review` first to see the findings and what
`--fix` would change.

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
`journal/<user>.jsonl`; shared files are edited normally. 0.21.1 parallel-write
hardening: raw-zone publish preparation takes the checkout sync lock around git
index mutations, publish marker lazy-creation is synchronized, topic upserts
require per-(project,kind,topic) locking and a replacing store on every backend
(otherwise the write is refused as unsupported), and exchange imports are
serialized with staged planning/manifesting to avoid interleaved partial runs.

- `brain sync --status [--json]` is read-only. It reports branch, ahead/behind,
  dirtiness, unmerged paths, and the revision recorded by the last completed
  pull or push (`<git-dir>/brain-sync.json`, never tracked).
- `brain sync` holds an interprocess lock on the data directory for the whole
  run, so two syncs in one checkout cannot interleave. It commits non-ignored
  changes, fetches, stashes a tree dirtied by a concurrent writer, rebases,
  restores the stash, and pushes only with `--push`. `--autostash=false`
  disables the stash step. `--watch` repeats the same run every `--interval`
  until SIGINT/SIGTERM; a conflict or transient failure is logged and retried
  on the next tick.
- A rejected push is retried up to 3 attempts with backoff; every retry fetches
  and rebases first. It never force-pushes.
- A conflict aborts the rebase, restores the autostash, prints the unmerged
  paths, and exits **3** (0 success, 1 runtime, 2 usage). A rebase or autostash
  left by a killed run is repaired at the **start** of `App.Sync`, before the
  conflict pre-check, so no manual `git rebase --abort` is required and a
  detached HEAD cannot survive a retry.
- `gitstore` shells out to the git CLI with an argument vector, a per-command
  timeout, `GIT_TERMINAL_PROMPT=0`, and sanitized errors so remote URLs with
  credentials do not leak.

### Merge drivers

The data-repository `.gitattributes` (maintained by `brain init` and
`brain doctor --fix`) routes single mutable files through deterministic
three-way drivers:

```gitattributes
* text=auto eol=lf
*.db binary
*.sqlite binary
*.zip binary
*.png binary
*.jpg binary
*.jpeg binary
*.gif binary
*.pdf binary
journal/*.jsonl merge=union
projects/*/state.md merge=lv24-state
registry.json merge=lv24-registry
projects/*/notes/*.md merge=lv24-notes
connectors/*/items/*.md merge=lv24-items
connectors/*/checkpoints/*.json -merge -text
connectors/*/*.lock -merge -text
```

Git stores drivers per clone in `.git/config`; `brain init`,
`brain doctor --fix` and the `brain sync` preflight register
`merge.<name>.driver` pointing at the current `brain` executable, and
`brain doctor` warns when they are missing. The preflight also appends missing
managed `.gitattributes` lines and refuses a data directory nested in a larger
work tree, where the drivers would apply to unrelated files. A clone without
drivers falls back to a normal content conflict, which is safe.

- **state** merges `goal` and `next_action` three-way and unions the ordered
  lists `active`, `blocked`, and `open questions`. When both sides changed the
  same scalar differently, the current version stays in the file and the
  incoming value is written to `projects/<id>/state-conflicts/<UTC>-<field>.md`.
  Unknown sections are preserved and merged by name.
- **notes** unions entries by heading plus body hash, orders them by marker
  `created_at`, and respects an intentional deletion when the surviving side is
  unchanged.
- **registry** unions projects by id, takes the record with the later
  `updated_at`, preserves unknown keys, unions checkout `paths` exactly like
  aliases (a path one machine added while the other edited the record
  survives, together with its `path_provenance` owner), and reports an alias
  or path claimed by two projects in `registry-conflicts/<UTC>-<field>.md`;
  the claim stays on the later `updated_at` and the artifact names both
  owners, so nothing is dropped silently.
- **items** normalizes the host-specific `observed_at` out of content-addressed
  raw-zone item files. When two hosts genuinely diverge, ours stays at the
  canonical path and theirs is written to a deterministic `*-<hash>.md` sibling;
  both survive and no rebase is blocked. Checkpoints stay `-merge -text` and
  gitignored.

The hidden `brain merge-driver <state|notes|registry|items> %O %A %B %L %P`
command is what git invokes. It rewrites `%A` and conflict files; diagnostics
name paths and field names, while a conflict file carries the current and
incoming values of that one field.

Driver-generated records under `state-conflicts/`, `registry-conflicts/` and
`merge-conflicts/` are committed in the same sync that produced them. `Status`
reports them in `ConflictRecords`; `sync --status --json` lists them in both
`conflicts` and `conflict_records` while `unmerged` stays false, so a dirty tree
with a preserved value is never reported as a blocked sync.

## Proposals, digest and promotion (Phase 4.1)

Ingestion writes the raw zone only; it never writes curated records. Phase 4.1
adds a review step between them:

- **Storage.** `proposals/<project>/<proposal-id>.md` sits at the data-dir root.
  `proposals/.gitignore` contains `*`, is created (and repaired) on the first
  proposal write, and keeps the whole zone out of git; `CurrentRevision` also
  excludes `proposals/`, so a digest never changes the data revision. Proposals
  are machine-local derived data: they are not synced, not exported and not
  served by the server API. There is one candidate per
  `(project, connection, external item)`; the id is
  `proposal-<connection>-<12 hex>`, where the hex is the first 12 characters
  of `sha256(project NUL connection NUL externalID)`. The proposal store is an
  optional adapter: server mode has none, so review and context simply report
  no proposals there.
- **Digest.** `brain digest --source <connection>` reads the stored items of one
  connection only. It never contacts the provider, never runs a connector, and
  has no LLM or network dependency. The default window is 30 days; `--since`,
  `--all`, `--limit` (default 500), `--rule`, `--project`, `--dry-run`, `--json`
  and `--write` narrow or persist the run. A preview is the default and writes
  nothing. The destination project comes from `--project`, else the
  connection's single configured project, else the unambiguous `ProjectID` of
  the stored items; an empty or ambiguous result is a validation error
  (exit 2).
- **Rules.** One deterministic rule per item, in precedence order:
  `closed-issue` (closed/resolved issue or closed/done/resolved/fixed/completed
  label) → decision; `decision-label` (`decision`, `adr`, `rfc`,
  `architecture`) → decision; `incident` (Sentry, or `incident`, `outage`,
  `sev0`–`sev3`, `production` labels) → attempt; `merged-change` (merged PR/MR)
  → note; `commit` → note. Confidence is a heuristic in `[0,1]` used for
  ordering and display only; nothing is auto-accepted. Reruns are idempotent,
  and a `promoted` or `dismissed` proposal is terminal, so a later digest never
  overwrites it.
- **Promotion.** `brain promote <id>` writes through the ordinary
  Decision/Attempt/Note use case. `--project` scopes the lookup (otherwise all
  registered projects are searched), `--as` overrides the kind, `--status`
  selects `proposed` or `accepted` for a decision, `--title` overrides the
  title, `--dismiss` rejects the candidate, `--dry-run` reports only, and
  `--json` prints the result. The curated id is `<kind>-<proposal id without
  its "proposal-" prefix>`; the record carries `source.proposal_id`,
  connection, external id, URL, observed time and confidence. Promotion is
  record-first: a retry scans curated records for `source.proposal_id` and
  repairs the proposal status instead of creating a second record. A source
  whose connection or provider is `local-only` (including an unknown provider)
  requires `--yes`; a stale or orphaned candidate requires `--force`.
- **Review and context.** `brain review` lists up to 20 pending proposals
  (kind, title, confidence, age, id) and adds three report-only findings:
  `proposal-stale` (not refreshed for 14 days, source changed, or no longer
  matches a rule), `proposal-orphan` (connection or item gone, tombstoned, or
  moved to another project) and `proposal-contradicted` (a later accepted
  decision or failed attempt with a similar title). `brain review --fix` never
  promotes, dismisses or rewrites a proposal. `brain context` adds only the
  header line `pending_proposals: N`; proposal bodies never enter context, and
  raw-item text does not enter it without `--raw` and a matching grant (see
  below). Proposals are not indexed in FTS5, so `brain search` stays
  curated-only.

## Temporal validity and supersede (0.16)

Notes, decisions, attempts and handoffs embed a `domain.Validity`:
`ValidFrom`, `ValidTo`, `SupersededBy`, `Supersedes` and an optional
`Confidence` (0..1). The zero value is backward compatible: a record is current
from its `created_at` and never ends, which is exactly how every record written
before 0.16 behaves.

- **Storage.** Decisions, attempts and handoffs carry `valid_from`, `valid_to`,
  `superseded_by`, `supersedes` and `confidence` in YAML frontmatter; notes
  carry them in their `<!-- brain:note {…} -->` marker. Only non-default values
  are written: a current record that became true at its `created_at` adds no
  keys, so old files stay clean. `valid_from` defaults to `created_at` on read.
  A present but unparseable timestamp is a parse error: the file is skipped and
  reported (check `records`), never silently treated as current.
- **Query semantics.** Every read surface — `brain search`, `brain context`,
  `brain open`/`brain bootstrap`, `brain catchup`, `brain record list|show` and
  their JSON forms — evaluates validity the same way:
  - default: keep `valid_to` empty or later than now; a record that ended at or
    before now is excluded;
  - `--as-of <RFC3339|date>`: keep `valid_from <= t` and (`valid_to` empty or
    `valid_to > t`), so a query returns the generation that was current at that
    instant; a bare date means midnight UTC;
  - `--include-superseded` (alias `--all-time`): keep ended records too. An
    `--as-of` instant takes precedence.
  The SQLite projection stores the same columns (schema version 6, with
  `(kind, valid_to)` and `(superseded_by)` indexes, plus the derived
  classification columns) and applies the same filter in SQL, so search and the
  record listing cannot disagree.
- **Supersede.** `brain record supersede <old> --by <new>` (any two records of
  one project) sets `valid_to = now` and `superseded_by = <new>` on the old
  record, a decision also gets `status: superseded`, and `<new>` gets
  `supersedes: <old>`. Both files stay in git: the chain is the audit log. The
  command is idempotent; a conflicting chain, a link to a missing record, a
  self-link and a cycle are validation errors. `brain decision|attempt|note|
  handoff --supersedes <id>` performs the same link when the replacement is
  written; `--superseded-by <id>` repairs history by writing a record that is
  already ended. `brain record show <id> --history` prints the whole chain.
- **Review and doctor.** `brain review` adds `supersession-gap` (a newer
  current record that reads like a replacement of an older current record but
  is not linked) and `stale-superseded` (a record whose `valid_to` passed and
  names no replacement). `brain doctor` reports invalid windows, a
  `superseded_by` without `valid_to`, and links to missing records (code
  `invalid-validity`). All of them are report-only.

## Provenance and quarantine (0.15)

Notes, decisions, attempts and handoffs embed a `domain.Provenance`: `Origin`,
`Reviewed` and `Trust`. The zero value is backward compatible (human, reviewed,
verified), so a record built without provenance behaves exactly like data written
before 0.15.

- **Storage.** Decisions, attempts and handoffs carry `origin`, `reviewed` and
  `trust` in their YAML frontmatter; a note carries them in its per-entry
  `<!-- brain:note {…} -->` marker, because one daily file mixes entries from
  different channels. The writer omits `reviewed` unless it is false and omits
  `trust` unless it is not verified; the reader fills the defaults. State has no
  provenance fields yet.
- **Validation.** `Origin` and `Trust` are closed enums on write. Reads keep an
  unknown value so `brain record show` and review can report it, but
  `Provenance.Quarantined` treats it as untrusted.
- **Write channels.** The CLI use cases default to human/reviewed/verified. The
  MCP adapter passes `agent` and reviews immediately unless
  `security.quarantine_agent` is set. The automatic session-end capture
  (`brain session end`, all hooks) passes `agent`, `trust: stated`, and the
  same review policy: the handoff is visible by default and quarantined when
  the config asks for it. Promotion passes `connector/reviewed/stated`.
  `NoteInput`, `DecisionInput`, `AttemptInput` and `HandoffInput` expose
  `Provenance` for the importer.
- **Read paths.** `Bootstrap`, `Context` and `Catchup` drop quarantined records
  (latest handoff and catchup baseline included) and report only ids via
  `quarantined: N (…)` / `Quarantined`. `--include-unreviewed` and MCP
  `include_unreviewed` opt in and label the records. `Findings` adds the
  `quarantined` finding; `Doctor` reports the queue.
- **Review.** `ports.RecordReviewer` lets `fsstore` mark one record reviewed
  without touching its body or timestamps. `brain record review <id>|--all`
  and the temporary MCP tool `brain_review_record` use it; the journal records
  a `review` event.

## Context and raw excerpts (Phase 4.2)

`brain context` is curated-first: state, review findings, the latest handoff,
the best-matching records and failed attempts. Quarantined records are excluded
(see [Provenance and quarantine](#provenance-and-quarantine-015)). `--raw` is off
by default and does not change that output; it may append a bounded set of stored
raw items.

- **Grant file.** `<data-dir>/context-allow.json` sits next to `brain.yaml`.
  The file is optional and contains no secrets; like
  `connectors/connections.json` it is committed. A missing file is an empty
  policy, not an error, so every raw excerpt is default-deny until a grant
  matches.
- **Schema.** `schemaVersion` is `1`. Each grant has required exact `project`
  and `connection`, optional `kinds` (empty means every kind: `issue`,
  `pull-request`, `merge-request`, `commit`, `message`, `page`, `document`,
  `event`, `schema`, `other`), optional `maxAgeDays` (`0` disables the age
  filter, maximum `3650`) and optional `maxExcerpts` (default `5`, range
  `1`–`5`). Unknown fields, a duplicate `project`+`connection`, invalid ids and
  invalid kinds are hard errors; nothing widens a malformed grant.
- **Selection.** Only local/git mode reads the file. A raw item is eligible only
  when its `ProjectID` equals the requested project exactly, its connection has
  a matching grant and its kind and age pass the grant. Synthetic
  `connection:<id>` items are excluded. Excerpts appear only when the curated
  context is thin (fewer than five records, failed attempts and findings in
  total), and at most five are added to one context. Each body is capped at 600
  runes and all bodies together at 2000 runes.
- **Presentation.** Bodies are re-redacted at read time with the ingestion
  redactor, marked as untrusted external text, and cited with connection,
  external id, URL and updated date. The stored raw item itself is never
  rewritten.
- **Scope.** Every raw read path is gated by the same policy: `brain context
  --raw`, `brain search --include-dumps`, `brain search --kind item`, MCP
  `brain_search` with `kinds:["item"]`, and the server-mode search client. A
  search that asks for raw text without a matching grant returns the curated
  hits only, plus one actionable stderr hint; legacy connector dumps have no
  connection identity and stay default-deny. The desktop app and LLM surfaces do
  not read raw excerpts. The MCP `brain_context` tool accepts an explicit `raw`
  boolean; like the CLI, it only works in local/git mode and only with a
  matching grant.

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
- `postgres.SchemaVersion` is 10. Migration `0002_auth_tokens.sql` adds the
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
- Migration `0007_search_fulltext.sql` adds generated `search_tsv` columns
  (`simple` configuration, title weighted above body) used by `GET /v1/search`.
- Migration `0008_project_access.sql` adds per-project access control (a
  restriction row per project and its members) and API token `scopes` and
  project allow-lists. Tokens minted before it keep their full role.
- Migration `0009_connector_items.sql` adds the server raw store
  (`connector_items`, `connector_checkpoints`, run `connection_id`/`stats`)
  used by the native item-level ingestion.
- Migration `0010_record_provenance.sql` adds record `origin`, `reviewed` and
  `trust` columns plus the server record review route's optimistic checks.
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

## Background agent and local control API

`brain agent` (local/git mode) is the foreground background engine: one process
per data dir (OS lock), a persisted schedule state, a bounded worker pool, a
parent-PID watchdog and rotating redacted logs under `<data-dir>/agent/`
(0600, self-ignored). The scheduler reads `Connection.Schedule` from the
catalogue each pass and skips overlapping runs of the same connection.

The same process owns the local control API. `internal/agentcontrol` wires the
daemon to `internal/adapters/localapi`; the API binds an ephemeral IPv4
loopback port and publishes `{port, token, pid, version}` in the owner-only
`agent/runtime.json`. Every request needs the bearer token and the exact
loopback `Host`; any `Origin` or `Referer` is rejected, CORS is not implemented,
and request bodies are size-limited. The API exposes health, connection
CRUD/test/run/credentials, run history with SSE logs, schedules, projects,
non-secret settings, an explicitly confirmed engine update and graceful
shutdown. It also serves the desktop memory surface: curated records (list and
detail with links/backlinks/relations/history), ranked search, project state
with a compare-and-swap write, the read-only review report, proposal
list/promote/dismiss and catchup. Run and log identifiers are job IDs for the
current process and persisted run IDs afterwards; job details and logs are
sanitized by the ingestion host and the handler. The wire contract is frozen as
`localapi-v1`; [localapi-v1.md](localapi-v1.md) defines auth/Host/Origin rules,
the error envelope and 405 `Allow` behavior, ID and 500-run cap semantics, SSE
snapshot/truncation rules, memory-route pagination caps and the state CAS error.

## Connectors and secrets

GitHub, GitLab, and IMAP adapters are read-only. IMAP selects mailboxes
read-only, uses `BODY.PEEK`, keeps metadata and a bounded snippet, and exposes
no send, delete, move, append, copy, or flag operations. Runs are idempotent and
build `ConnectorRun` values with sanitized errors.

The native Phase 1 sources follow the same `ports.Source` contract and are
registered in `internal/connectorbuild`: `folders`, `jira`, `confluence`,
`linear`, `slack`, `notion`, `sentry`, `postgres-schema`, `sqlite-schema`,
`rss`, and `web`. Each one owns a per-stream checkpoint cursor, bounds every
response, and reports no deletes unless the upstream has authoritative
tombstones (none of the Phase 1 sources do). Slack reads only explicitly listed
channels; Notion syncs shared pages/data sources and falls back to search;
Sentry stores issues with at most three bounded events each; the database
sources read catalogs only and never user rows; RSS/web revalidate with HTTP
validators. The `web` source defaults to the sitemap's host and port, honors
`robots.txt`, and never follows redirects.

The Phase 3 `mcp` source (v0.12.0) connects to Model Context
Protocol servers over stdio or streamable HTTP and maps read-only
search/list tools to items. It requires an explicit tool allow-list heuristic
(destructive names are always denied), bounds calls/results, and marks output
as recent-results-only; stdio `command`/`args` are local credentials, never the
synced catalogue, and inherit the agent environment (run only trusted servers).

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

In server mode the command calls `POST /v1/connectors/run`; the server builds
the native item-level connection from environment/OS secrets, runs it with
`BRAIN_CONNECTOR_TIMEOUT`, stores raw items and checkpoints in PostgreSQL
(schema 10) plus the run record through `httpapi.Store`, and maps items to an
existing project when the request carries `projectId`. The in-memory
development store has no raw zone and rejects the run as unsupported instead
of discarding output. Viewers can list and fetch runs
at `GET /v1/connectors/runs` and `GET /v1/connectors/runs/{id}`; `GET
/v1/connectors/runs/{id}/markdown` exports the run record. The run record keeps
status, counts and the sanitized error.

`internal/adapters/secrets` resolves credentials from
`BRAIN_SECRET_<NAME>` first and the platform store second: Windows Credential
Manager, macOS Keychain, or Linux Secret Service. A key such as `github/token`
maps to `BRAIN_SECRET_GITHUB_TOKEN`. Environment-provided values shadow the OS
store and cannot be overwritten through the store API. `brain secret
set|list|remove` manages the OS store; `set` reads the value from
stdin and prints only the secret name. Connection credentials use the
`conn/<id>/<field>` namespace (`brain connection secret set <id> <field>`
reads the value from stdin); the catalogue `connectors/connections.json`
stores settings only, never secrets.

OAuth foundation (Phase 3.1): `internal/adapters/sourceoauth` implements the
RFC 8252 loopback PKCE flow, GitHub device flow, a validated token bundle in
the secret store, and serialized refresh with rotation persisted before the new
access token is returned. `internal/oauthlogin` stores the bundle under
`conn/<id>/oauth` plus token-free metadata and makes an OAuth session override
the PAT for `token` lookups; `brain connection auth <id>` drives the flow
(GitHub device, GitLab PKCE) and `--remove` falls back to the PAT path.
Atlassian/Notion stay token-based for MVP.

Raw zone (v0.9.0; machine-local by default since 0.15): in local/git mode the
ingestion host stores one Markdown file per item at
`connectors/<id>/items/<40 hex>.md` (provenance frontmatter), one checkpoint
per stream at `connectors/<id>/checkpoints/<stream>.json`, and tombstones
(ref, deleted, observed_at only) for reported deletes. Items are written before
checkpoints; a run then updates the per-item index (`item:<conn>/<id>`, kind
`item`, excluded from default search) and persists the run record.

The raw zone is machine-local unless the connection says otherwise. The
effective policy is decided once per run: an explicit `publish: shared` or
`publish: local-only` on the connection always wins. Without one, a raw zone
that git already tracks keeps publishing exactly as before (grandfathered, so an
upgrade never untracks 2k files behind the owner's back) and a connection with
nothing tracked defaults to local-only. `brain connection publish <id>
shared|local-only` records the choice; local-only untracks `connectors/<id>`
(`git rm --cached -r`) and shared keeps items tracked. Files stay on disk and git
history is never rewritten.

A per-connection `connectors/<id>/.gitignore` marker carries the policy:
local-only ignores the whole directory; shared commits items and still ignores
`checkpoints/`, lock and temporary files, because a cursor belongs to the host
that read the stream. A shared item file omits `observed_at` (the host clock), so
two machines that read the same item write byte-identical files at the same path
instead of conflicting on every sync; switching a connection to shared rewrites
its stored local-only items without that field. A run with an unreadable git
index keeps the zone unchanged rather than guessing local-only. `brain doctor`
reports tracked files of an explicitly local-only connection (`raw-zone`, untrack
them) and a tracked zone without an explicit policy (`raw-zone-policy`, choose
with `brain connection publish`). Mail connections default to `local-only`; mail
addresses are hashed to `[email:<12 hex>]` (exposure reduction, not
anonymization). Sources never infer deletes from absence.

In server mode the same host writes raw items and checkpoints to PostgreSQL
(`connector_items`, `connector_checkpoints`, migration `0009`) and run
`connection_id`/`stats`. The server has no local-only raw zone, so the publish
policy is enforced at the API boundary: a connection whose policy resolves to
local-only (a provider default such as mail, or an explicit
`publish: local-only`) is refused with 403 and an audit event unless the
request carries the explicit admin opt-in `allowLocalOnly`; with the opt-in the
run stores its run record and checkpoints but drops every item (reported as
`skipped`). A provider that defaults to local-only therefore never reaches the
shared item table by accident. A request may pass `projectId` to map items to
an existing project; unmapped items stay stored but out of project-scoped
search. Server
search returns raw item hits only to admins; viewers and members get curated
kinds, and an explicit `kind=item`/`kind=dump` request is refused and audited.
The in-memory development store has no raw zone, so connector runs return
`unsupported` instead of reporting a false success.

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
- `brain eval` scores retrieval, temporal filtering and context packing against
  `eval/fixtures` and `eval/baseline.json` offline; the metric run is run
  locally and is not a CI gate (see [eval.md](eval.md)).
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
| Server raw items require PostgreSQL | connector runs persist raw items, checkpoints and run records with `BRAIN_DATABASE_URL`; the in-memory development store returns `unsupported` instead of discarding output |
| Raw-zone deletes are not inferred | sources report tombstones only; `Deletes` is false without authoritative tombstones |
| Curation is local and rule-based | `brain digest --source` and `brain promote` run in local/git mode only, read stored items, and write machine-local git-ignored proposals; server mode has no proposal surface, the local API exposes proposals read/promote/dismiss to the desktop (never automatically), and there is no scheduled digest or proposal merge/trash |
| Raw context excerpts are local/git only | `brain context --raw` and every raw search path read `context-allow.json`; excerpts exist only in local/git mode, and server search never returns raw items to viewers or members |
| `record review` is local/git only | server mode has no provenance-edit route yet; quarantined server records can only be changed through the ordinary update routes |
| Relations are store-and-index only | typed relations and the judged graph are persisted and projected, but no read path uses them for neighbour navigation yet; `brain timeline` and the context `linked` layer still follow `[[id]]` only |
| Merge is CLI-only | `brain record merge` plans, applies and rolls back in local/git mode; no MCP tool and no server route apply, trash or hard-delete records |
| MCP surface is the 15-tool `lv24` profile | `--profile=lv24` is the only profile; retired 0.16 tool names return `NOT_FOUND` with `try_instead`; no `engram`, `codegraph` or `graphify` alias profiles and no legacy aliases exist yet |
| No code graph or native code indexer yet | code symbols and impact (0.18) and optional embeddings are absent; search stays lexical, and `brain_capabilities` reports `brain_code`/`brain_graph` as false |
| CLI index is local-only | `brain search` in server mode uses `GET /v1/search`; no local index is built |
| No server-side Markdown import/export | migrate into Markdown first; server records go through the API |
| Server auto-update not implemented | upgrade by pulling a new image; `BRAIN_SERVER_AUTO_UPDATE` is reserved |

## Non-goals v1

CRDT co-editing, multi-tenant SaaS, billing, mobile app, paid hosting or CI,
automatic mail body storage, and Rust components (interfaces are reserved).

## Server record provenance review

Server-mode records carry origin, 
eviewed, and 	rust provenance. POST .../records/{kind}/{id}/review updates only those fields, bumps the revision, and audits the change; httpclient implements RecordReviewer so rain record review works against a server.

