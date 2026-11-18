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
brain hooks status --agents                      # installed, trusted and last run per agent host
```

Approve the new brain hooks in Codex once (Codex keeps hooks disabled until
they are trusted). That is the whole loop: agents read `brain open` at session
start and leave an `[auto]` handoff at the end.

Before starting a specific task, ask for just the context it needs:

```sh
brain context "add retry to sync push"
brain context "token expiry" --kind decision --since 30d --json
brain context "retry budget" --raw
```

It prints the project state, review findings, the latest handoff, the records
that best match the task (decisions rank above attempts and notes, newer above
older, an explicit pin above both) and failed attempts to avoid repeating,
bounded by `--budget` (tokens; default 6000, maximum 32000). A layer that left
records out ends with `- вЂ¦ N more dropped`, so a short answer is never mistaken
for a complete one. Agents call the same thing through the MCP tool
`brain_context`.

`--raw` is off by default. It appends at most five granted raw excerpts when the
curated context is thin and only in local/git mode; without a matching grant in
`<data-dir>/context-allow.json` it adds nothing. See
[Raw context excerpts](#raw-context-excerpts-phase-42).

Records written by an agent or a connector can be quarantined until you review
them: `brain context` and `brain open` leave them out and print
`quarantined: N (ids)`. Review one with `brain record review <id>`, or the queue
with `brain record review --all`. To inspect without clearing, add
`--include-unreviewed`. See [Provenance and quarantine](#provenance-and-quarantine-015).

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

A retired checkout is never registered. Selecting a path inside one (or its
former bare name, e.g. `old-checkout`) resolves to the project that replaced it
and prints a warning; `brain discover` skips it and `brain project set --path`
refuses it. The engine ships no retired paths; a workspace adds its own with
`retired_paths` in `brain.yaml`:

```yaml
retired_paths:
  - path: <root>/old-checkout
    replaced_by: demo
```

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
`handoffs/`. `proposals/<id>/` holds machine-local candidates when
`brain digest --source` is used; it is git-ignored and never synced. Markdown
is the source of truth.

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
whose **content** changed within a window (a review or a snooze rewrites the
file without making the record recent):

```sh
brain search decisions                 # also finds "decision"
brain search "СЂРµС€РµРЅРёР№"                 # also finds "СЂРµС€РµРЅРёРµ", "СЂРµС€РµРЅРёСЏ"
brain search deploy --since 2026-11-01 # only records whose text changed since 1 November
brain search deploy --since 7d --kind decision
```

Ranking v2 normalizes lexical relevance per query and weights the signals
`bm25` 0.55, `recency` 0.20, `pin` 0.15, `importance` 0.05 and `trust` 0.05; a
strict all-terms pass that finds fewer than five hits is merged with an
any-term pass, and the strict hits always stay on top. `--explain` prints the
contribution of each signal, and the contributions add up to the score.

`brain search` prints one block per hit:

```text
note/demo/First note
  <snippet>
```

Server mode does not use the local index. `brain search` there calls
`GET /v1/search?q=...` on the configured server; `brain index` returns an
unsupported-mode error. Raw items stay out of default search on both sides;
`--kind item` opts in (`--dumps` keeps opting into legacy dumps; on the
server an excluded `dump` also excludes items).

### Connectors

The CLI runs read-only GitHub, GitLab, and IMAP connectors. Credentials come
from the environment or the OS secret store, never from `brain.yaml`.
The legacy snapshot path below keeps working; the connection catalogue
(v0.9.0) adds item-level runs on top of it:

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
They are searchable with `brain search --dumps`. Raw-zone items (v0.9.0)
are excluded by default; opt in with `brain search <query> --kind item`.

`brain connection` (local/git mode only) manages named connections in
`connectors/connections.json` (settings only, never secrets):

```sh
./bin/brain connection providers --config ~/brain/brain.yaml
./bin/brain connection add github --id gh-main \
  --setting repos=owner/repository --config ~/brain/brain.yaml
printf '%s\n' '<github-token>' | ./bin/brain connection secret set gh-main token \
  --config ~/brain/brain.yaml
./bin/brain connection list --config ~/brain/brain.yaml
./bin/brain connection test gh-main --config ~/brain/brain.yaml
./bin/brain connection run gh-main --limit 25 --config ~/brain/brain.yaml
./bin/brain connection migrate --copy-legacy-secrets --config ~/brain/brain.yaml
```

GitLab uses `--setting projects=group/project`; IMAP uses
`--setting username=... [--setting folders=INBOX]` with `--account <id>` and
`--base-url imaps://host[:port]`, and the secret field is `password`
(`brain connection secret set <id> password`, value from stdin). Credential
keys are `conn/<id>/<field>` in the OS keychain. `connection remove <id> --yes`
keeps items, checkpoints and credentials (purge hint printed on removal).
`brain connector run --all` runs every configured connection when the
catalogue is non-empty (optional `--project` filters by connection project);
when the catalogue is empty it falls back to the legacy data-derived targets.
`brain connection run <id> [--since RFC3339] [--days N] [--limit N] [--json]` runs one
connection through the ingestion host: per-connection lock (busy returns
`connection busy`), the publish policy applied before the first write
(shared-to-local-only untracks `connectors/<id>` from git via
`git rm --cached -r` but does not rewrite prior git history), secret
redaction (`[redacted]`), mail address hashing (`[email:<12 hex>]`, exposure
reduction, not anonymization), body cap (512 KiB with truncation marker),
per-stream checkpoints (`connectors/<id>/checkpoints/*.json`), tombstones for
reported deletes, index update, and a run record. Raw items and checkpoints
stay machine-local unless the connection sets `publish: shared`; a zone that git
already tracks keeps publishing unchanged (grandfathered) until you choose.
`brain connection publish <id> local-only|shared` records the choice and applies
it immediately: local-only untracks `connectors/<id>` (files and git history are
kept), shared publishes items without the host `observed_at` and keeps
checkpoints local. `brain doctor` reports both `raw-zone` (explicitly local-only
but tracked) and `raw-zone-policy` (tracked but no policy chosen). Sources do not
infer deletes from absence (`Deletes` is false without authoritative tombstones).

`brain connection test <id> [--json]` runs the provider's non-mutating check and
prints a redacted cause when it fails. `brain connection list` shows the newest
run of each connection (time, status, redacted cause), so a dead ingestion is
visible without reading logs.

### Connector identity migration (0.26)

The account label and connector-declared identity settings (the IMAP
`username`) are personal data. 0.26 stores them in the secret store
(`conn/<id>/account`, `conn/<id>/username`) and keeps only a marker in
`connectors/connections.json`. Reading the value is lazy: a locked keychain
never breaks `connection list`. Update every machine to 0.26 **before**
applying the migration, because a pre-0.26 binary reads the marker as a literal
account string. Run the dry run first, then apply:

```sh
./bin/brain connection migrate-identity --config ~/brain/brain.yaml --check
./bin/brain connection migrate-identity --config ~/brain/brain.yaml --apply
./bin/brain doctor --config ~/brain/brain.yaml
```

Statuses are `would-move`, `moved`, `already-migrated`, `missing-secret`,
`unreadable-secret` and `mismatched-secret`; a marker whose secret is gone is
repaired with `brain connection secret set <id> account` (the value must match
the stored fingerprint). A raw legacy value keeps working forever, so an
unmigrated connection is not broken вЂ” only its identity stays in git.

### Scrubbing e-mail addresses from raw zones

`brain doctor` warns about tracked raw-zone files that hold e-mail addresses or
mailbox user names (`tracked-pii`). GitHub and GitLab now hash addresses at
ingest like mail does, but data written before that still carries them. The
scrub script rewrites the addresses in place to the same
`[email:<12 hex>]` placeholder, recomputes the item content hash and is
idempotent:

```sh
python scripts/scrub-raw-pii.py <data-dir>            # dry run: counts per zone
python scripts/scrub-raw-pii.py <data-dir> --apply
go run scripts/scrub-raw-pii-hash-check.go <data-dir>
```

It covers `connectors/<id>/items`, `connectors/runs` and the legacy `mail/`,
`github/` and `gitlab/` dumps; `--include items,runs,dumps` narrows the set.
Only `--apply` writes, and only matched bytes are replaced.

The legacy `brain connector run --name ...` snapshot path uses
`--since <RFC3339>` to limit items by update time and `--limit <n>` to cap each
collection. The command prints source, status, counts, and a sanitized error.
In local or `git` mode it writes dumps below the data directory and
persists each run as Markdown under `connectors/runs/`; `connection run`
instead writes raw items/checkpoints under `connectors/<id>/`. In server mode the same
snapshot command calls `POST /v1/connectors/run`; with PostgreSQL the server persists
raw items and checkpoints (schema 10) plus the run record, while the in-memory
development store has no raw zone and rejects the run as unsupported. The server
has no local-only raw zone, so it stores items only when the run is explicitly
shared: pass `--publish shared`. A provider whose raw zone defaults to
local-only (mail, chat) is refused with 403 unless `--allow-local-only` is
given, and such a run stores the record and checkpoints but no items. Pass
`--brain-project <id>` to map items to a project; unmapped items stay stored but
out of project-scoped search. Viewers and members never receive raw items from
server search; `kind=item` is admin-only there.
`GET /v1/connectors/runs` lists records, `GET /v1/connectors/runs/{id}` returns
one, and `GET /v1/connectors/runs/{id}/markdown` exports it as Markdown.
`BRAIN_CONNECTOR_TIMEOUT` (default `5m`, clamped to `10s`вЂ“`1h`) bounds a server run.
The CLI allows `1h10s` for this synchronous request, including persistence;
ordinary API and authentication requests retain the `30s` default timeout.

### Digest and promotion (Phase 4.1)

Raw items stay raw until you review a candidate and promote it:

```sh
./bin/brain connection run gh-main --days 30 --config ~/brain/brain.yaml
./bin/brain digest --source gh-main --since 7d --config ~/brain/brain.yaml   # preview only
./bin/brain digest --source gh-main --project demo --write --config ~/brain/brain.yaml
./bin/brain review --project demo --config ~/brain/brain.yaml
./bin/brain promote proposal-gh-main-1a2b3c4d5e6f --as decision --status accepted --config ~/brain/brain.yaml
./bin/brain promote proposal-gh-main-1a2b3c4d5e6f --dismiss --config ~/brain/brain.yaml
```

`--source` reads the items that connection already stored; it never re-runs the
connector and has no network or model dependency. The preview writes nothing.
`--write` persists proposals under `<data-dir>/proposals/<project>/`; the zone
is machine-local, self-ignored with `proposals/.gitignore`, excluded from the
data revision, never synced, and never indexed for search. Rules are
deterministic (closed issue в†’ decision, decision/ADR label в†’ decision,
incident/Sentry в†’ attempt, merged PR в†’ note, commit в†’ note); confidence is a
heuristic and nothing is accepted automatically. `--project` is required when
the connection or its items do not resolve to one project. `--all` ignores the
window (default `30d`), `--limit` defaults to 500, `--rule` narrows the rules,
`--dry-run` is the default and cannot be combined with `--write`, and `--json`
prints the `mode: "proposals"` envelope. Server mode rejects `--source`.

`brain review` lists pending proposals and reports `proposal-stale`,
`proposal-orphan` and `proposal-contradicted`; `--fix` never changes a
proposal. `brain context` adds only `pending_proposals: N` unless `--raw` adds a
granted excerpt (see below). Promotion writes a normal decision, attempt or note
with `source.proposal_id`, a deterministic record id, and provenance
(connection, external id, URL, observed time). Reruns are idempotent. A
`local-only` source needs `--yes`; a stale or orphaned proposal needs `--force`;
`--dismiss` is terminal. Promotion is the only path from a proposal into
curated records.

Not in 4.1: scheduled digest, proposal merge/trash, and server/desktop proposal
surfaces. Raw context excerpts arrived in Phase 4.2 and are local/git only.

### Provenance and quarantine (0.15)

Promoted records are `origin: connector`, reviewed, `trust: stated`. Agent writes
over MCP and the automatic session-end handoff/captured decisions are
`origin: agent, trust: stated`; they are reviewed by default, or unreviewed when
`brain.yaml` contains:

```yaml
security:
  quarantine_agent: true
```

Unreviewed agent, connector and import records never enter `brain context`,
`brain open`, `brain catchup` or MCP `brain_context`/`brain_open`. Those commands
print only `quarantined: N (id, вЂ¦)`, and `brain review` adds a `quarantined`
finding. Project state uses the same provenance: `brain state set` writes
`origin: human, reviewed: true, trust: verified`; MCP `brain_state set` writes
`origin: agent, trust: stated` (reviewed unless `quarantine_agent` is set). A
quarantined state is reported as `state_quarantined: true` and its text is
replaced with a placeholder; clear it with `brain state review` (optionally
`--trust`). Clear the queue after reading the records:

```sh
brain record review <id>                            # one record
brain record review --all --trust stated            # every unreviewed record
brain context "task" --include-unreviewed           # inspect without clearing
brain catchup --include-unreviewed                  # include the quarantine in the delta
```

`brain doctor` reports the queue size. Old records without provenance keys are
human, reviewed and verified, so nothing changes until an agent or connector
writes a record. Server mode reviews records through
`POST .../records/{kind}/{id}/review`, so `brain record review` works there too;
`--review-after`/`--clear-review-after` and `brain state review` stay local/git
because the server state has no provenance columns.

### Edit a record in place

`brain record edit` changes the text of an existing note, decision, attempt or
handoff. It keeps the record ID, file name and `created_at`, preserves
provenance, validity, relations and review state, moves the previous body into
the bounded history, and reindexes the record for search. Editing an unreviewed
record does not mark it reviewed; a decision status or supersede link still
needs `brain record status` or `brain record supersede`.

```sh
brain record edit note-2026-11-15-fix-cache --title "Fix the cache key"
brain record edit decision-2026-11-01-use-sqlite \
  --body-file decision.md --expected-updated-at 2026-11-15T10:00:00.123456789Z
printf 'updated text\n' | brain record edit attempt-2026-11-02-retry --body-file -
brain record edit note-2026-11-15-fix-cache --tags cache,search
brain record edit note-2026-11-15-fix-cache --title "Preview" --dry-run
```

`body` is the note text, the decision context, the attempt result or the handoff
summary; a handoff has no title, so edit its summary with `--body-file`. Tags
apply to notes only. The optional `--expected-updated-at` makes the edit a
compare-and-swap: a stale value refuses with the current revision instead of
overwriting a concurrent edit; take the value from `brain record show <id>
--json` or the local API `updated_at`. `--allow-duplicate` accepts text that
another live record of the same kind already has. Limits, protected projects and
globs apply exactly as they do to a create.

### Temporal validity and supersede (0.16)

Decisions get replaced; the old one must stop looking current. Every record has
`valid_from` (default: when it was written) and `valid_to` (empty = current), so
"what did we decide in March?" is a query, not archaeology:

```sh
brain search "storage engine"                     # current records only
brain search "storage engine" --as-of 2026-03-15  # what was current then
brain record list --all-time                      # superseded records too
brain record show <id> --history                  # the supersede chain
```

Default reads (`brain search`, `brain context`, `brain open`, `brain catchup`,
`brain record list`) leave out a record whose `valid_to` has passed.
`--as-of <RFC3339|date>` keeps records where `valid_from <= t` and `valid_to` is
empty or later than `t`; a bare date means midnight UTC. `--include-superseded`
(alias `--all-time`) adds the ended ones. The summary of `brain open`/`context`
lists `as_of: вЂ¦` when a past instant was requested. Temporary caveat: the server
API does not expose validity yet, so `--as-of` and `--include-superseded` are
local/git only and return an error in server mode.

Link a replacement in one step:

```sh
brain decision "Use SQLite FTS5 for search" --status accepted \
  --context "local index, zero services" \
  --supersedes decision-2026-03-01-use-json-files
# or, for two records that already exist (any kinds):
brain record supersede decision-2026-03-01-use-json-files --by decision-2026-04-02-use-sqlite
```

The old record keeps its file and text: it gets `valid_to` and
`superseded_by`; the replacement gets `supersedes` (and a decision becomes
`status: superseded`). Both stay in git. The command is idempotent and rejects
conflicting chains, self-links, missing records and cycles. `--superseded-by`
on a write repairs history (an imported record that was already replaced), and
`--confidence 0..1` records author confidence. `brain review` reports
`supersession-gap` and `stale-superseded`; `brain doctor` reports invalid
validity (`invalid-validity`). All are report-only.

### Raw context excerpts (Phase 4.2)

Curated context is the default. To let an agent see a few stored raw items, add
an explicit grant in `<data-dir>/context-allow.json` next to `brain.yaml`;
without the file, or without a matching grant, nothing raw is shown. The same
grant gates `brain search --dumps`, `brain search --kind item` and MCP
`brain_search` with `kinds:["item"]`; curated hits always remain. For example:

```json
{
  "schemaVersion": 1,
  "grants": [
    {
      "project": "demo",
      "connection": "gh-main",
      "kinds": ["issue", "pull-request", "commit"],
      "maxAgeDays": 30,
      "maxExcerpts": 3
    }
  ]
}
```

```sh
./bin/brain context "retry budget" --raw --config ~/brain/brain.yaml
```

- `project` and `connection` are required and matched exactly; `connection` is
  the id from `brain connection list`.
- `kinds` is optional. Empty (or omitted) allows every item kind: `issue`,
  `pull-request`, `merge-request`, `commit`, `message`, `page`, `document`,
  `event`, `schema`, `other`.
- `maxAgeDays` is optional; `0` means no age limit and the maximum is `3650`.
- `maxExcerpts` is optional; it defaults to `5` and accepts `1`вЂ“`5`.
- Unknown fields, a duplicate `project`+`connection`, invalid ids and invalid
  kinds are hard errors. Nothing falls back to a wider grant.

Excerpts are added only when the curated context is thin (fewer than five
records, failed attempts and findings in total), at most five per context, with
each body capped at 600 runes and all bodies at 2000 runes. Only items stored
for the exact project and connection are eligible; synthetic `connection:<id>`
items are excluded. Every excerpt is cited with its connection, external id, URL
and updated date, is marked as untrusted external text, and is re-redacted with
the ingest redactor at read time. The file holds no secrets and is committed
like `connectors/connections.json`. Raw excerpts are local/git only; server
mode, the desktop app and LLM surfaces are deferred. The MCP `brain_context`
tool accepts the same explicit `raw` boolean and remains curated-only without
it.

### OAuth for GitHub and GitLab (v0.12.0)

Public-client OAuth uses the system browser and never ships a client secret.
Pass `--client-id` or set `BRAIN_OAUTH_CLIENT_ID`; GitHub uses the device flow,
GitLab uses PKCE on loopback port 47615. The resulting bundle is stored only in
the OS secret store (`conn/<id>/oauth` plus token-free metadata); an OAuth
session takes precedence over a PAT for `token` lookups, and `--remove` falls
back to the PAT/token path.

```sh
export BRAIN_OAUTH_CLIENT_ID='<public-client-id>'
./bin/brain connection auth gh-main --scopes repo,read:user --config ~/brain/brain.yaml
./bin/brain connection auth gl-main --scopes read_api,read_user --config ~/brain/brain.yaml
./bin/brain connection run gh-main --config ~/brain/brain.yaml
./bin/brain connection auth gh-main --remove --config ~/brain/brain.yaml
```

`brain connection add` still accepts PATs via `connection secret set`; the
OAuth session is optional.

Native Phase 1 sources (local/git) are added through the same catalogue:

```sh
# Local folders, Obsidian vaults and Logseq graphs (no credentials)
./bin/brain connection add folders --id notes \
  --setting paths='C:\notes,C:\vault' --config ~/brain/brain.yaml
./bin/brain connection run notes --limit 100 --config ~/brain/brain.yaml

# Jira Cloud or Data Center (token is secret; email is Cloud-only)
./bin/brain connection add jira --id jira-work \
  --base-url https://site.atlassian.net \
  --setting email=me@example.test --setting deployment=cloud \
  --setting projects=ENG,OPS --config ~/brain/brain.yaml
printf '%s\n' '<jira-api-token>' | ./bin/brain connection secret set jira-work token \
  --config ~/brain/brain.yaml
./bin/brain connection run jira-work --days 30 --config ~/brain/brain.yaml

# Confluence Cloud (base URL includes /wiki) or Data Center
./bin/brain connection add confluence --id wiki \
  --base-url https://site.atlassian.net/wiki \
  --setting email=me@example.test --setting spaces=ENG \
  --config ~/brain/brain.yaml
printf '%s\n' '<confluence-api-token>' | ./bin/brain connection secret set wiki token \
  --config ~/brain/brain.yaml
./bin/brain connection run wiki --config ~/brain/brain.yaml

# Linear (PAT by default; authscheme=oauth for Bearer)
./bin/brain connection add linear --id linear-work --setting teams=ENG \
  --config ~/brain/brain.yaml
printf '%s\n' '<linear-token>' | ./bin/brain connection secret set linear-work token \
  --config ~/brain/brain.yaml
./bin/brain connection run linear-work --config ~/brain/brain.yaml

# Slack (bot token; listing a channel is the explicit opt-in)
./bin/brain connection add slack --id slack-team \
  --setting channels=C0123456789,C9876543210 --config ~/brain/brain.yaml
printf '%s\n' '<xoxb-token>' | ./bin/brain connection secret set slack-team token \
  --config ~/brain/brain.yaml
./bin/brain connection run slack-team --limit 20 --config ~/brain/brain.yaml

# Notion (share pages/databases with the integration)
./bin/brain connection add notion --id notion-work \
  --setting databases=<data-source-id> --config ~/brain/brain.yaml
printf '%s\n' '<notion-token>' | ./bin/brain connection secret set notion-work token \
  --config ~/brain/brain.yaml
./bin/brain connection run notion-work --config ~/brain/brain.yaml

# Sentry (org slug required; optional projects/environment)
./bin/brain connection add sentry --id sentry-prod \
  --base-url https://sentry.io --setting org=acme \
  --setting projects=api,web --setting environment=production \
  --config ~/brain/brain.yaml
printf '%s\n' '<sentry-token>' | ./bin/brain connection secret set sentry-prod token \
  --config ~/brain/brain.yaml
./bin/brain connection run sentry-prod --days 7 --config ~/brain/brain.yaml

# Database schema snapshots (catalog-only; never reads user rows)
./bin/brain connection add postgres-schema --id app-schema \
  --setting host=db.example.test --setting database=app --setting user=brain_ro \
  --setting sslmode=verify-full --config ~/brain/brain.yaml
printf '%s\n' '<read-only-password>' | ./bin/brain connection secret set app-schema password \
  --config ~/brain/brain.yaml
./bin/brain connection run app-schema --config ~/brain/brain.yaml

./bin/brain connection add sqlite-schema --id local-schema \
  --setting 'path=/absolute/path/app.db' --config ~/brain/brain.yaml
./bin/brain connection run local-schema --config ~/brain/brain.yaml

# RSS/Atom feeds and sitemap-driven web pages (no credentials)
./bin/brain connection add rss --id news \
  --setting urls=https://example.test/feed.xml --config ~/brain/brain.yaml
./bin/brain connection run news --limit 50 --config ~/brain/brain.yaml

./bin/brain connection add web --id docs-site \
  --setting sitemaps=https://example.test/sitemap.xml \
  --setting maxpages=50 --config ~/brain/brain.yaml
./bin/brain connection run docs-site --config ~/brain/brain.yaml

# MCP server (Phase 3; stdio command or streamable HTTP URL)
./bin/brain connection add mcp --id mcp-docs \
  --setting transport=http --setting url=https://mcp.example.test/mcp \
  --setting tools=search_pages,list_recent --config ~/brain/brain.yaml
printf '%s\n' '<mcp-bearer-token>' | ./bin/brain connection secret set mcp-docs token \
  --config ~/brain/brain.yaml
./bin/brain connection run mcp-docs --limit 50 --config ~/brain/brain.yaml

# Or a local stdio server: command/args are local secrets, never synced
./bin/brain connection add mcp --id mcp-local --setting transport=stdio \
  --config ~/brain/brain.yaml
printf '%s\n' '/usr/local/bin/mcp-server' | ./bin/brain connection secret set mcp-local command \
  --config ~/brain/brain.yaml
printf '%s\n' '--root=/notes' | ./bin/brain connection secret set mcp-local args \
  --config ~/brain/brain.yaml
./bin/brain connection run mcp-local --config ~/brain/brain.yaml
```

These providers are available through `brain connection run`/`brain connector run --all`;
the legacy snapshot `brain connector run --name` still accepts only
`github`, `gitlab` and `imap`.

`brain agent` (WP2.1/WP2.2, v0.12.0) runs configured connections in the
background with persisted `Schedule` values:

```sh
./bin/brain agent --config ~/brain/brain.yaml          # foreground; one instance per data dir
./bin/brain agent status --config ~/brain/brain.yaml
./bin/brain agent stop --config ~/brain/brain.yaml
```

The agent writes only private `agent/` state (runtime/schedule/stop/log/settings
files, 0600 and excluded from git), keeps a parent-PID watchdog, serializes runs
of the same connection and never logs credentials. It also serves the
authenticated local control API on an ephemeral loopback port. The runtime file
`<data-dir>/agent/runtime.json` contains `{port, token, pid, version}`; it is
written with owner-only permissions and removed on a clean shutdown. Treat the
token like a password: never copy it into logs or commit it.

```sh
# Read the endpoint and token locally (PowerShell: Get-Content ...)
port=$(jq -r .port ~/brain-data/agent/runtime.json)
token=$(jq -r .token ~/brain-data/agent/runtime.json)
curl -s -H "Authorization: Bearer $token" "http://127.0.0.1:$port/v1/health"
curl -s -H "Authorization: Bearer $token" "http://127.0.0.1:$port/v1/connections"
```

Endpoints: `GET /v1/health`; `GET|POST /v1/connections`;
`GET|PATCH|DELETE /v1/connections/{id}`; `POST /v1/connections/{id}/test`;
`POST /v1/connections/{id}/run` (queues a backed run, returns `jobId`);
`POST /v1/connections/{id}/credentials/{field}`; `GET /v1/runs`,
`GET /v1/runs/{id}`, `GET /v1/runs/{id}/log` (SSE); `GET /v1/schedules`,
`PUT /v1/schedules/{id}`; `GET /v1/projects`; `GET|PUT /v1/settings`;
`GET /v1/records`, `GET /v1/records/{id}`, `GET /v1/search`, `GET /v1/review`,
`GET /v1/proposals`, `POST /v1/proposals/{project}/{id}/promote|dismiss`,
`GET /v1/catchup`, `GET|PUT /v1/projects/{id}/state`;
`POST /v1/engine/update` (requires `{"confirm":true}`);
`POST /v1/shutdown`. Requests require exactly one `Authorization: Bearer`
header, `Host` must be the loopback listener, and any `Origin`/`Referer` is
rejected (no CORS, no browser access). The API is local-only; server mode has
its own authenticated HTTP API. The frozen `localapi-v1` contract
([localapi-v1.md](localapi-v1.md)) covers the error envelope, 405 `Allow`
behavior, job/run IDs, the 500-run list cap, SSE snapshot limits and the memory
route caps and state CAS.

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

`brain sync` takes the data-directory lock, repairs an interrupted earlier run,
stages non-ignored files, commits when dirty, fetches, rebases onto the upstream
branch (stashing and restoring a tree dirtied by a concurrent writer), and
pushes only with `--push`. A rejected push is retried up to 3 times with
backoff, fetching and rebasing first. A freshly created, empty data repository
works too: the first `sync --push` creates its branch. It never force-pushes.

`brain init` and `brain doctor --fix` register the deterministic merge drivers
for `state.md`, `notes/*.md`, and `registry.json`, so concurrent edits usually
merge field by field. When both machines changed the same state field, the
current value stays in the file and the incoming value is recorded under
`projects/<id>/state-conflicts/`. `--status` is read-only. Exit codes are 0
success, 1 runtime, 2 usage, and 3 sync conflict.

Expected `sync --status` output:

```text
provider: git
branch: main
ahead: 0
behind: 0
changed: 0
dirty: false
```

A publish run reports the commits it created or moved:

```text
provider: git
branch: main
ahead: 0
behind: 0
changed: 1
dirty: false
```

The data repository `.gitignore` excludes `secrets/`, `index/`, `backups/`,
databases, and `.env`. Use an SSH key or the Git credential helper; never embed
a token in the repository URL.

## 3. Server mode

The HTTP API and the token-authenticated CLI client are implemented. Postgres
persistence is enabled by `BRAIN_DATABASE_URL` (or `DATABASE_URL`); without it
the server falls back to an in-memory store and loses data on restart. Server
schema 10 (migrations `0002_auth_tokens.sql` through `0010_record_provenance.sql`)
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

- `GET /openapi.json` вЂ” OpenAPI 3 document.
- `GET /v1/projects`, `GET /v1/search?q=...` вЂ” authenticated read paths.
- `GET /v1/events` вЂ” SSE revision stream with heartbeat and `Last-Event-ID`.

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

`brain sync` is not implemented in server mode вЂ” the API is the sync path.
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
| `brain open [path\|id]` | `--project <id\|alias>`; `--budget <tokens>` (default 6000, maximum 32000) caps the summary; `--brief` prints only the project and state fields (no git or graph sections); `--as-of <time\|date>`, `--include-superseded`/`--all-time` select validity |
| `brain bootstrap [path\|id]` | `--project <id\|alias>`; `--budget <runes>` (default 4000); `--as-of`, `--include-superseded`/`--all-time` |
| `brain note [text]` | `--project`, `--title`, `--body`, `--tags a,b`, `--supersedes <id>`, `--superseded-by <id>`, `--confidence 0..1`. Positional text becomes the body and the title is its first line (at most 80 characters). Do not combine positional text with `--body`. |
| `brain decision [title]` | `--project`, `--title`, `--status proposed\|accepted\|rejected\|superseded` (default `proposed`), `--context <text>`, `--alternatives <a,b>`, `--consequences <a,b>`, `--evidence <a,b>`, `--supersedes <id>`, `--superseded-by <id>`, `--confidence 0..1`. The three list flags are comma-separated. |
| `brain attempt [title]` | `--project`, `--title`, `--hypothesis <text>` (what was expected), `--result <text>` (what happened), `--reason <text>` (why it failed), `--supersedes <id>`, `--superseded-by <id>`, `--confidence 0..1` |
| `brain handoff [summary]` | `--project`, `--summary`, `--next <text>` (next action), `--git-context <text>` (default: read from the project checkout), `--no-git`, `--supersedes <id>`, `--superseded-by <id>`, `--confidence 0..1` |
| `brain journal` | `--project`, `--user`, `--event <name>` (for example `note`, `decision`, `attempt`, `handoff`, `commit`), `--since <duration>` (for example `24h`), `--limit <n>` (default 20). Local and git mode. |
| `brain event commit` | `--project <id>`, `--repo <path>` (default `.`), `--quiet` (print nothing on success; used by the hook). Local and git mode. |
| `brain search <query>` | `--project <id>`, `--limit <n>` (default 20), `--kind <kind>` (`note,decision,attempt,handoff,dump,item`; raw-zone `item` is excluded by default and needs a `context-allow.json` grant), `--dumps` (legacy dumps; also un-excludes server items, grant-gated), `--since <24h\|7d\|2w\|2026-11-01\|RFC3339>` (content change time), `--as-of <time\|date>`, `--include-superseded`/`--all-time`, `--match-mode all\|any`, `--relaxed`, `--min-score 0..1`, `--explain`, `--full`, `--page-size`, `--cursor`. English and Russian word forms match as a prefix; the default all-terms pass falls back to any-term when it finds fewer than five hits and keeps the strict hits on top. |
| `brain record` | `list` accepts `--kind`, `--since` (content change time), `--status`, `--limit`, `--as-of`, `--include-superseded`/`--all-time`; `show <id>` accepts `--history` and `--as-of`; `edit <id>` accepts `--title`, `--body-file` (`-` for stdin), `--tags`, `--expected-updated-at`, `--allow-duplicate`, `--dry-run`; `supersede <old> --by <new>`, `status`, `review`, `trash`, `restore`, `trash-list` as in the runbook. JSON includes a `validity` object (`valid_from`, `valid_to`, `superseded_by`, `supersedes`, `confidence`) and `contentUpdatedAt`. |
| `brain links <id>` | `--project <id>`, `--json`; `--broken` lists `[[id]]` references to missing records. Write `[[<id>]]`, `[[<id>\|text]]` or `[[<project>/<id>]]` in record text; `brain record show` lists Links and Backlinks. |
| `brain review` | `--project <id>`, `--since <7d>`, `--fix`, `--fix-kind <kinds>`, `--json`. Lists pending proposals (at most 20) and findings; `broken-link`, `near-duplicate`, `supersession-gap`, `stale-superseded`, `proposal-stale`, `proposal-orphan`, `proposal-contradicted` are report-only. Proposal findings are local/git only, and `--fix` never touches proposals. |
| `brain digest` | `--since <90m\|24h\|7d>` (default `24h`; with `--source` the default is `30d`), `--project <id>`, `--json`, `--write` (saves `<data_dir>/digests/YYYY-MM-DD.md`). With `--source <connection>`: `--all`, `--limit <n>` (default 500), `--rule <names>`, `--dry-run` (default; conflicts with `--write`), `--write` (persists proposals). `--source` is local/git only. |
| `brain promote <id>` | `--project <id>`, `--as decision\|attempt\|note`, `--status proposed\|accepted` (decision only), `--title <text>`, `--dismiss`, `--force` (stale/orphan), `--yes` (local-only source), `--dry-run`, `--json`. Local/git only. |
| `brain context <task>` | `--project <id>`, `--budget <tokens>` (default 6000, maximum 32000), `--explain` (per-layer table), `--kind decision\|attempt\|note` (repeatable), `--since <24h\|7d\|2w\|date>` (content change time), `--as-of <time\|date>`, `--include-superseded`/`--all-time`, `--json`, `--raw` (off by default; local/git only; appends at most five excerpts allowed by `<data-dir>/context-allow.json` when the curated context is thin). State, review findings, latest handoff, best-matching records and failed attempts for the task; layer caps are budget shares and every layer reports `- вЂ¦ N more dropped`; JSON adds `total_truncated` and `state_as_of`. MCP: `brain_context`. |
| `brain sync` | `--status` (read-only branch, ahead/behind, dirty, unmerged paths, last synced revision), `--json`, `--push` (push after committing), `--autostash` (default on; `--autostash=false` disables the stash around the rebase), `--watch` (run immediately and then every `--interval`, default `5m`, minimum `30s`; retries after a logged conflict/failure). Exit 3 means an unmerged conflict. |
| `brain doctor` | `--json`, `--fix` (register the git merge drivers and add their `.gitattributes` lines) |
| `brain hooks` | `install [--repo <path>] [--force] [--config <brain.yaml>]`, `uninstall [--repo <path>]`, `status [--repo <path>] [--json]`, `status --agents [--home <dir>] [--json]`. The post-commit hook journals each commit; `--agents` reports installed, trusted and last-run state for Claude Code, Codex and OpenCode. |
| `brain connection` | `providers`; `add <provider> --id <id> [--account <id>] [--base-url <url>] [--setting k=v]вЂ¦`; `list`; `test <id>` (non-mutating check, redacted cause); `run <id> [--since RFC3339] [--days N] [--limit N]`; `secret set <id> <field>` (value from stdin); `auth <id> [--client-id] [--scopes] [--remove]`; `publish <id> local-only\|shared`; `migrate [--copy-legacy-secrets]`; `migrate-identity [--check\|--apply]`; `remove <id> --yes`. Local/git mode. |
| `brain onboard` | `--json`, `--config`; runs `doctor --fix`, checks schema/PII/raw-zone/git sync, stamps this machine as the owner of registered checkout paths that exist here (`path_provenance`), prints the platform-specific `brain sync --watch` command. Exit 3 when unresolved conflicts exist. |
| `brain project` | `list`, `show <id>`, `set <id>` (aliases: `add`) with `--name/--kind/--repo/--branch/--path/--add-path/--alias/--clear-paths/--clear-aliases`, `members`, `adopt [--apply] [--json]`. `adopt` reports which registered paths exist on this machine and, with `--apply`, records this machine as their owner; an owner recorded by another machine is never overwritten. |
| `brain discover` | `--root <dir>` (required), `--apply` (register new repositories; default is a dry run), `--exclude a,b` |
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

- `no brain instance at ...; run brain init first` вЂ” the `--config` path is
  wrong or `init` was run for another data directory.
- `data_dir: does not exist` from `doctor` вЂ” create the directory with `init`
  or correct `brain.yaml`.
- `local mode has no remote` from `sync` вЂ” local mode does not sync; use `git`
  mode.
- `brain sync` exits `3` вЂ” the rebase conflicted; the engine already aborted it
  and restored the autostash, so fix the listed paths (and any
  `state-conflicts/` record), then rerun. The engine never discards changes.
- `brain doctor` warns about missing merge drivers вЂ” run
  `brain doctor --fix` on that clone.
- Server exits with `BRAIN_JWT_SECRET is required` вЂ” set a signing key of at
  least 32 bytes.
- `401` on server requests вЂ” the HTTP client refreshes once automatically, so a
  repeated `401` means the refresh session is expired or revoked: run
  `brain login` again (or set `BRAIN_TOKEN` for automation), then retry. Access
  tokens expire after 15 minutes by default; refresh tokens rotate.
- `server client is not configured` from `doctor` вЂ” the URL is missing or no
  token was found in `BRAIN_TOKEN` / the OS secret store.
- `local search index is not available` from `search` вЂ” run `brain index` (or
  `brain index --rebuild`) against the configured data directory.
- `index is local-only in server mode` вЂ” `brain index` runs only in local or
  `git` mode; in server mode `brain search` calls `GET /v1/search?q=...`.
- `sync in server mode: not implemented` вЂ” server mode does not use Git sync;
  reads and writes go through the HTTP API.
- `connection "вЂ¦" stores its account in the secret store вЂ¦ but the secret is
  missing` вЂ” the catalogue has an identity marker and this machine has no
  secret for it. Run `brain connection secret set <id> account` (the value must
  be the same address the marker fingerprints) or re-apply
  `brain connection migrate-identity --apply` on the machine that holds it.
- The first search after an engine upgrade reindexes once (index schema 9);
  `brain index` makes that visible instead of paying it inside the first search.

