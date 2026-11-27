# Team runbook

This runbook covers daily use, git-mode sync, hooks, server operations,
upgrades, database migrations, legacy import and rollback, and incident
checklists. It describes the behaviour of this repository. Flag-level
reference is in [quickstart.md](quickstart.md#command-flags). Installation and
deployment details are in [install.md](install.md), [deploy.md](deploy.md),
and [update.md](update.md).

Placeholders: `~/brain-data` is an example data directory (the private data
repository in git mode), `<data-dir>` is the brain data directory of any
instance, and `https://brain.example.test` is an example server URL. Replace
them with your own values. Never commit the data directory or put secrets in
examples.

## 1. Daily use (agents and humans)

### Set the default instance

Once per machine, point the CLI at the data directory's `brain.yaml`:

```sh
brain config use ~/brain-data/brain.yaml
brain config show
brain config clear          # remove the pointer
```

The config is resolved in this order: `--config`, `$BRAIN_CONFIG`,
`./brain.yaml`, then the pointer set by `brain config use`.

### Open a project

```sh
cd <path-to-project-checkout>
brain open .
brain open --brief .
brain bootstrap demo
```

`brain open` prints the bootstrap pack, a `## Git` section, and a `## Graphs`
section. `--brief` prints only the project and state fields. The selector is an
id, an alias, or a path. Without one, the project is resolved from the working
directory: the longest registered path containing it, else the single
registered project under it. Use `brain project list` to see the registry.
A write to an unknown project fails unless `--create` is passed (for example
`brain note --create --project demo "..."`), which registers it; without the
flag a typo or unregistered alias never starts a new project. `brain project set`
makes the metadata of a registered project explicit.

Before the summary, `brain open` and `brain status` print `project_id`, `path`,
`tier` and `index` for a resolved project. `brain status` reports
`tier: ABSENT` when no project can be resolved for the directory (no registered
checkout and no single-project fallback), and still exits 0.
`tier` is `EMPTY` (registered, no records/state content/handoff) or `POPULATED`
(at least one of them), computed from stored records rather than the index.
`index` is `complete at <sha>`, `stale at <sha> (HEAD <sha>)` or
`stale (never indexed)`.
When the working directory is outside the resolved checkout, both commands also
print `cwd:` and `registered_paths:` so a wrong directory cannot silently show
another project's context. `brain status --json` keeps its configuration shape
and does not include these lines.

### Task context

```sh
brain context "add retry to sync push"
brain context "token expiry" --kind decision --since 30d --json
brain context "retry budget" --raw
```

`brain context` prints project state, review findings, the latest handoff, the
records that best match the task and failed attempts to avoid, bounded by
`--budget` (tokens; default 6000, maximum 32000). Layer caps are shares of that
budget (`core` 25 %, `query` 35 %, `decisions` 20 %, `attempts` 12 %, `linked`
8 %) and are fungible: a thin layer lends its unused share to the next one.
There is no fixed record count, so a larger budget returns more records rather
than more padding. Every layer that left records out ends with
`- … N more dropped (over-budget, lower-rank)`, and the query layer adds
`- … N lower-ranked omitted` for its near misses. `--explain` prints
`layer: used/cap tokens, N records` and `total: used/budget tokens, D dropped`.
`--project` selects another project, `--kind` keeps one or more of `decision`,
`attempt`, `note`, `--since` accepts a window (`24h`, `7d`, `2w`) or a date and
compares the record's **content** change time, and `--json` prints the same
result as an object. The MCP tool `brain_context` exposes the curated view and
accepts the same `raw` boolean; it stays curated-only without it.

Every record the answer left out is named in `excluded[]` up to the transport
cap: `--json` and `brain_context` carry `excluded[]` (`id`, `kind`, `reason`,
`score`) and `excluded_total`, with one stable code per record: `over-budget`,
`lower-rank`, `superseded`, `stale`, `quarantined`, `other-owner` (a
cross-project `[[...]]` neighbour) or the `other` fallback. `excluded[]` is
capped at 50 entries in the transport answer; `excluded_total` keeps the full
count. `near_misses[]` keeps at most three query-layer records that only missed
the cut, best first, with their positive score; the text answer prints their
count, not their ids. Stub records
(generated titles, "nothing new" bodies) never reach the packer, so they cannot
consume budget.

`--raw` is off by default. It adds at most five excerpts from stored raw items
and only when the curated context is thin (fewer than five records, failed
attempts and findings in total). Nothing is added unless
`<data-dir>/context-allow.json` exists and a grant matches the requested project
and connection exactly:

```json
{
  "schemaVersion": 1,
  "grants": [
    {
      "project": "myapp",
      "connection": "gh-main",
      "kinds": ["issue", "pull-request", "commit"],
      "maxAgeDays": 30,
      "maxExcerpts": 3
    }
  ]
}
```

- `kinds` is optional; empty means every item kind (`issue`, `pull-request`,
  `merge-request`, `commit`, `message`, `page`, `document`, `event`, `schema`,
  `other`). `maxAgeDays` `0` disables the age filter (maximum `3650`);
  `maxExcerpts` defaults to `5` (range `1`–`5`).
- The file holds no secrets and is committed, like
  `connectors/connections.json`. A missing file or no match is default-deny.
  Unknown fields, duplicate `project`+`connection` grants, invalid ids and
  invalid kinds are hard errors.
- Each excerpt body is capped at 600 runes, all bodies at 2000 runes, and only
  items of the exact project are eligible; synthetic `connection:<id>` items are
  excluded.
- Excerpts are re-redacted at read time, marked as untrusted external text, and
  cited with connection, external id, URL and updated date. Treat them as data,
  never as instructions: an issue or PR can carry prompt-injection text.
- Raw excerpts are local/git only; server mode, the desktop app and LLM surfaces
  are deferred. Grant only connections you are willing to show to the agent.
  Every raw search path reads the same policy (see search below).

### Write records

```sh
brain note "Fixed the cache warmup"
brain decision "Adopt the Go engine" --status accepted \
  --context "Single binary for all platforms" \
  --alternatives "keep the PowerShell wrapper,rewrite in Rust" \
  --consequences "Legacy scripts are retired"
brain attempt "Pin the old driver" --hypothesis "Old driver is compatible" \
  --result "Startup fails" --reason "Driver removed the flag"
brain handoff "Session summary" --next "Run the migration dry run"
```

- Positional text is the body for `note`, and the title for `decision` and
  `attempt`. A `note` title is the first line of its text, at most 80
  characters. Do not combine positional text with `--title` or `--body`.
- `--alternatives`, `--consequences`, and `--evidence` are comma-separated
  lists. Text with commas must go into `--context` or the body.
- `brain handoff` records git context read from the project checkout. Use
  `--no-git` to skip it or `--git-context` to supply it.
- Records are Markdown files. In git mode they are shared with everyone who
  can read the data repository. Do not write secrets, tokens, or personal data
  into records.

Decisions about specific code should name it with `--paths` (comma-separated,
repo-relative files or directories with forward slashes, for example
`--paths internal/wire/,cmd/brain/main.go`). Absolute paths and `..` segments
are rejected. The same field is the `paths` array of `brain_decision`. When a
project has a local checkout on the machine that runs `brain review`, the review
reports each accepted or proposed decision whose paths changed in git after the
decision was last updated (`decision-code-changed`). It is report-only: confirm
the decision still holds or supersede it. Machines without the checkout, and
projects that are not git repositories, are skipped silently.

### Record lifecycle (local and git mode)

```sh
brain record list [project] --kind decision --status proposed --since 7d
brain record list --as-of 2026-03-01          # what was current then
brain record list --all-time                  # ended records too
brain record show <id>
brain record show <id> --history              # supersede chain, oldest first
brain record status <id> accepted          # proposed|accepted|rejected|superseded
brain record supersede <old-id> --by <new-id>
brain record review <id> [--trust verified|stated|inferred]
brain record review --all
brain record trash <id> --yes              # moves it to projects/<p>/trash/
brain record trash-list [project]
brain record restore <id>
```

- Changes are journaled (`status`, `trash`, `restore`) and update the search
  index at once. Trash is reversible and lives in the data repository, so it
  syncs like any record. Notes written by hand without an id marker cannot be
  trashed; edit the daily file instead.
- Agents get `brain_record` (list/get) and `brain_judge` (accept/reject/supersede)
  over MCP; trash and restore stay CLI-only.

### Temporal validity and supersede

A record is current from `valid_from` (default: its `created_at`) until
`valid_to`; an empty `valid_to` means it is still current. Reads never delete
history:

- `brain search`, `brain context`, `brain open`, `brain catchup` and
  `brain record list` show current records only by default.
- `--as-of <RFC3339|2026-03-01>` answers "what was current then": a record is
  returned when `valid_from <= t` and `valid_to` is empty or later than `t`.
  A bare date means midnight UTC. The latest handoff is selected at the same
  instant. State and review findings have no history: `brain context` prints
  `state_as_of: current (state and review findings describe the current data)`
  and `--json` returns `state_as_of: "current"`, so a past-instant answer never
  pretends the state was historical.
- `--include-superseded` (alias `--all-time`) lists ended records too. On
  `brain record show` the record is always printed (you named it) and `--as-of`
  reports `valid_at: yes|no`.

```sh
# Write the replacement; the old decision becomes superseded in one step.
brain decision "Use SQLite FTS5 for search" --status accepted \
  --context "local index, zero services" --supersedes decision-2026-03-01-use-json-files
# Link two records that already exist.
brain record supersede decision-2026-03-01-use-json-files --by decision-2026-04-02-use-sqlite
# Repair an import: the new record is already replaced.
brain decision "Old rollout plan" --superseded-by decision-2026-05-01-rollout-v2
```

The old record keeps its text and file: it gets `valid_to: <now>` and
`superseded_by: <new>`, the replacement gets `supersedes: <old>` (a decision
also gets `status: superseded`), so `git log` stays a readable audit trail. The
command is idempotent, and a second call prints `already superseded by`. A
conflicting chain (the old record is already superseded by a different record,
or the new one already supersedes another), a self-link, a missing record and a
cycle are validation errors; nothing is written in those cases.

`brain review` reports `supersession-gap` when a newer current record reads like
a replacement of an older current record but is not linked, and
`stale-superseded` when a record's `valid_to` passed with no replacement named.
`brain doctor` reports invalid windows and links to missing records
(`invalid-validity`). Both are report-only: confirm or supersede by hand.
Server mode has no validity route yet, so the temporal flags are local/git only.

### Quarantine and provenance review (0.15)

Every record has `origin` (human/agent/connector/import), `reviewed` and `trust`
(verified/stated/inferred). Missing keys are the old defaults: human, reviewed,
verified. Quarantined records are unreviewed non-human writes or records whose
origin is unknown; they are left out of `brain context`, `brain open` and the
MCP equivalents. The output lists only their ids:

```text
quarantined: 2 (decision-2026-11-14-raise-retry-budget-01j…, note-2026-11-14-ci-flake-01j…)
```

Routine:

```sh
brain review                                   # quarantined finding + ids
brain record show <id>                         # read the record before trusting it
brain record review <id>                       # clear one record
brain record review --all --trust stated       # clear the queue
brain context "task" --include-unreviewed      # inspect without clearing
brain doctor                                   # quarantine: warn with N records
```

- Review is idempotent, keeps the body and timestamps, and journals a `review`
  event. It edits the marker of a note or the provenance keys of a decision,
  attempt or handoff.
- A hand-written note section has no marker and cannot be reviewed; edit the
  daily file instead.
- Set `security.quarantine_agent: true` in `brain.yaml` to make MCP writes and
  the automatic session-end capture unreviewed. Leave it off if agents are
  trusted and you want their records in context immediately. `brain promote`
  always writes reviewed records with `trust: stated`; session-end handoffs and
  captured decisions are `origin: agent, trust: stated`.
- `brain catchup` follows the same rule: quarantined notes, decisions, attempts
  and handoffs are left out (and cannot become the baseline handoff) unless you
  pass `--include-unreviewed`; the report prints `quarantined: N (ids)` either
  way.
- `brain record review` is local/git only for now; server mode has no
  provenance-edit route. `brain_judge(action=review)` is the temporary MCP
  equivalent and is listed only for backends that can review records. `state.md` has no
  provenance yet, so state writes are always trusted.

### Links between records (since v0.8.0)

Reference another record by writing `[[<id>]]`, `[[<id>|text]]` or, across
projects, `[[<project>/<id>]]` in a note body, decision context, alternatives,
consequences or evidence, attempt hypothesis, result or reason, or handoff
summary or next action. Links inside inline code and code fences are ignored.

```sh
brain note "Rolled back, see [[decision-lv24-20261108-101500|the cache decision]]"
brain record show <id>                 # adds Links and Backlinks sections
brain links <id> [--project p] [--json]
brain links --broken [--project p]     # references to missing records or projects
```

Backlinks are computed on read from the same project's records; nothing is
stored and no migration is needed. MCP `brain_record` returns `links` (state
found, missing or unavailable, with kind, title and status) and `backlinks`.

### Review hygiene: broken links and near-duplicates (since v0.8.0)

- `broken-link`: a `[[...]]` reference to a record or project that does not
  exist. Fix the text by hand; there is no automatic fix.
- `near-duplicate`: two records of the same kind in one project with nearly the
  same text (3-word shingle similarity of at least 80%, or the same title and a
  body of at least 60%; `[auto]` handoffs compare only the summary, at 90%).
  The older id comes first. Keep one: for decisions
  `brain record supersede <old> --by <new>`, otherwise
  `brain record trash <id> --yes` (undo with `brain record restore <id>`). Only
  the 500 most recent records per kind are compared; trashed, rejected and
  superseded records are ignored. `brain doctor` shows a `duplicates` check
  with counts per project.

Both are report-only: `brain review --fix` never touches them.

Proposal findings (`proposal-stale`, `proposal-orphan`,
`proposal-contradicted`) are also report-only and are covered in the curation
section below. `brain review` additionally lists up to 20 pending proposals
with their id, kind, confidence and age.

### Journal and search

```sh
brain journal --since 24h --limit 50
brain journal --project demo --event decision
brain search "cache warmup" --project demo --limit 10
brain index                 # refresh the local index after Markdown changed outside the CLI
brain journal --repair --dry-run   # records of the last 30 days without a journal line
```

- `brain journal` shows the per-user activity file `journal/<user>.jsonl`,
  newest first. It works in local and git mode. A retried write is journaled
  once. When a journal append failed after a record was stored, `brain
  doctor` (check `journal`) reports it and `brain journal --repair` appends
  the missing lines, timed at the record's creation and marked `repaired`.
- `brain index` also drops index entries whose records are gone (deleted,
  renamed or quarantined on another machine) and reports them as `pruned`.
- `brain search` queries the local FTS5 index in local and git mode and calls
  `GET /v1/search` in server mode. It searches curated records by default;
  `--dumps` adds legacy connector dumps (mail, GitHub/GitLab issues and PRs),
  `--kind item` adds raw-zone items (excluded by default), and
  `--kind note,decision` (or `--kind dump`) filters by kind. GitHub/GitLab
  dumps are indexed under the registered project whose `repo_url` they
  mirror, so `brain search --project ims --kind dump "flaky"` finds that
  repository's issues. Connector runs index their dumps; item runs update the
  per-item index (`brain index` prints `items`). Server mode applies
  `--kind` too (Postgres full-text search, ranked).
- Raw search is grant-gated: `--dumps`, `--kind item` and MCP
  `brain_search` with `kinds:["item"]` return raw text only for a connection
  with a matching `context-allow.json` grant. Without one the curated hits are
  still returned and stderr carries a hint. Legacy dumps have no connection
  identity, so they stay default-deny. In server mode raw items are admin-only:
  an unfiltered search excludes them, and `kind=item`/`kind=dump` from a
  viewer or member is refused with 403 and audited.
- Local search (FTS5) matches each query term of 4 or more characters by
  prefix, so `warm` finds `warmup`; shorter terms match exactly. It first
  requires all terms; when that finds fewer than five hits, it retries with any
  term and **merges** the pages: strict hits stay on top, OR-only hits follow,
  and `relaxed: true` reports that the fallback added something. The two passes
  normalize relevance over different match sets, so an OR-only hit never
  outranks a record that matched every term.
- Ranking v2 normalizes lexical relevance per query (min-max over the matched
  set, top-anchored for one or two matches) and weights `bm25` 0.55, `recency`
  0.20, `pin` 0.15, `importance` 0.05 and provenance `trust` 0.05. The score is
  the weighted sum in `[0, 1]`, so `--min-score` filters the same number the
  output shows and `--explain` contributions add up to it.
- Every page carries a support verdict: `supported` when a hit contains every
  rare query term (document frequency ≤ 20 % of the project, floor one) and at
  least half of the remaining subject terms, `related` when the page only
  overlaps the topic, `none` when nothing answers it. `none` empties `hits` and
  lists the matched records in `excluded[]` as `lower-rank` with the reason;
  a `related` page stays visible. `--match-mode all` runs only the strict pass
  unless `--relaxed` is given; an explicit `--relaxed` or `--match-mode any`
  reports the page as `related` ("partial matching was requested explicitly").
  Server mode has no support gate.
- Duplicate content collapses to one row within a page: the index hashes
  normalized kind/title/body, the oldest record keeps the slot and every other
  copy is listed once as `duplicate`. Stub records (generated label titles, whole-body
  "nothing new"/"session ended"/"no changes"/"no updates") stay in the index and
  in counts but are hidden from search and context; `brain search
  --include-stubs` shows them. MCP `brain_search` always hides them.
- `--since` compares `content_updated_at`, the last title/body/tag change.
  Review, snooze, validity, supersede and merge bookkeeping rewrite the file
  without moving the content time, so a metadata pass no longer re-reports the
  corpus. Records written before the field existed fall back to `updated_at`.
- The local index schema is 10. It adds `content_hash`, `stub` and a `meta`
  table holding `index_generation`, `indexed_from` and `schema_version`. A
  schema mismatch recreates the index and sets the reindex flag, and the next
  command reindexes once from Markdown; an explicit `brain index` does the same
  work up front. `brain open`/`brain status` report `index: complete at <sha>`
  or `index: stale at <sha> (HEAD <sha>)`; `brain doctor` reports the
  `index-generation` check and warns only when the data directory is dirty or
  the index is more than one commit behind. A one-commit lag is normal.
- Server mode also supports `brain record list|show|status|supersede|review`,
  `catchup`, `review` and `journal --repair` reads; trash and restore are
  local/git only, and `record review --review-after/--clear-review-after` and
  `state review` stay local/git (server state has no provenance columns).
- Writes made through the CLI update the local index, and `brain sync`
  reindexes after pulling records from another machine. Markdown changed by
  hand needs `brain index` (or `brain index --rebuild` to rebuild it from
  Markdown).

### Retrieval quality evaluation (0.16)

```sh
brain eval --fixtures eval/fixtures                 # metrics and per-category table
brain eval --fixtures eval/fixtures --json          # machine-readable report
brain eval --fixtures eval/fixtures --baseline eval/baseline.json
brain eval --fixtures eval/fixtures --write-baseline eval/baseline.json
```

- The harness builds a temporary brain from the authored fixtures, reindexes
  it through the public app API and scores recall@5, precision@5, MRR,
  superseded-leak rate, tokens per answer and p95 search latency. No network
  and no external service are used.
- `--baseline` exits 1 on a regression: rates may drop by at most 0.02, tokens
  by 10%, p95 by 2x+5 ms, and the superseded leak must not rise at all. The
  report warns when the fixture digest differs from the baseline.
- Fixtures live in `eval/fixtures/`; add a query, label the record that should
  answer it, and keep a known synonym miss as a miss. The full format, metric
  definitions and licensing note are in [docs/eval.md](../docs/eval.md).
- `go test ./eval ./cmd/brain` is the deterministic check for CI. The metric
  run is documented as a non-blocking job in `docs/eval.md`; no paid workflow
  is added.

### Session routine for agents

1. `brain open .` at the start, and `brain sync` first in git mode.
2. Record decisions and failed attempts when they happen, not at the end.
3. `brain handoff "<summary>" --next "<next action>"` at the end.
4. In git mode, run `brain sync --push` after the handoff (see section 2).

## 1a. Onboard a new machine

One command prepares a checkout and prints the next step for its platform:

```sh
brain update --yes
brain onboard --config ~/brain-data/brain.yaml
```

`brain onboard` runs the safe doctor repairs (merge drivers and managed
`.gitattributes`/`.gitignore` lines), checks schema, PII/raw-zone status and git
divergence, and prints the exact `brain sync --watch --push --interval=5m`
command plus the service-manager hint for macOS, Linux or Windows. It never
installs a service or rewrites records. Use `--json` for a machine-readable
report; unresolved conflicts return exit 3.

It also stamps path provenance: every registered checkout that exists on this
machine and has no recorded owner becomes owned by this machine
(`path_provenance` in `registry.json`). Run the same onboarding on every
machine once so the desktop Projects page can name the machine behind each
path. An owner recorded by another machine is never overwritten, so a shared
`C:\0projects\...` path keeps the first machine that registered it. To inspect
or re-apply the mapping without the other onboarding checks:

```sh
brain project adopt            # dry run: what would be stamped
brain project adopt --apply    # record the owners
```

## 2. Git-mode sync routine

The data repository is private and holds the shared brain. The sync commands
are available in git mode only; server mode uses the API instead.

```sh
brain sync --status --config ~/brain-data/brain.yaml        # read-only summary
brain sync --status --json --config ~/brain-data/brain.yaml # ahead/behind, dirty, unmerged, conflict records, last revision
brain sync --config ~/brain-data/brain.yaml                 # commit local changes, then pull (rebase)
brain sync --config ~/brain-data/brain.yaml --push          # the same, then push
brain index --config ~/brain-data/brain.yaml                # after pulled Markdown changed
```

What `brain sync` does, in order:

1. Takes the data-directory lock; a second sync waits instead of colliding on
   `.git/index.lock`.
2. Repairs a rebase or an autostash left by a killed earlier run **before**
   checking for conflicts, so a killed rebase never leaves a detached HEAD and
   an unreachable recovery path.
3. Self-heals the clone: appends any missing managed `.gitattributes` lines (LF
   normalization, binary rules, driver routes) and re-registers missing merge
   drivers in `.git/config` with the current `brain` executable. A data
   directory nested in a larger git work tree is refused with an actionable
   error instead of silently misconfiguring that repository. `--status` skips
   these writes.
4. Stops with exit **3** if conflicts remain after recovery: no commit, rebase,
   or push is attempted (the preflight repair from step 3 may already have
   updated the local setup files).
5. Commits local changes when the working tree is dirty, including the
   `.gitattributes` upgrade from step 3. The message is `brain sync: <timestamp>`.
6. Fetches, stashes a tree dirtied by a concurrent writer, rebases onto the
   upstream branch, and restores the stash. `--autostash=false` disables the
   stash step; the default is on. Driver-generated conflict records written
   during the rebase are **committed in the same run** and listed in
   `brain sync --status --json` under both `conflicts` and `conflict_records`;
   they are informational, not unmerged paths, so they never block the next
   sync or set `unmerged:true`.
7. Pushes only with `--push`. A rejected push is retried up to 3 times with
   backoff, fetching and rebasing before each retry. Nothing is ever forced.

Exit codes: **0** success, **1** runtime error, **2** usage error, **3** sync
conflict with unmerged paths. `--status` never changes anything.

### Automatic sync

On this Windows machine the "Company Brain Update" task runs every 30 minutes:
it applies a signed engine update and then runs `brain sync --push`, so a clean
tree is a cheap no-op and an interrupted rebase is recovered before the pull.
The nightly "Company Brain Graph Refresh" task still runs graphs, connectors,
`brain sync --push` and `brain agents sync`. Both tasks call the scripts in
`lv24-brain/scripts/`; the retired `.brain` wrapper folder is no longer used.

On macOS and Linux, run the same loop under a service manager:

```sh
brain sync --watch --push --interval=5m --config ~/brain-data/brain.yaml
```

`--watch` runs once immediately, then on every interval; SIGINT/SIGTERM stops
it. A conflict or transient failure is logged and retried on the next tick, so
the other machines keep publishing. A macOS LaunchAgent or a systemd user timer
can run this command; the daily LaunchAgent remains a fallback.

### Three machines writing at once

The data model is built for independent concurrent writers: records are
create-only ULID files, notes union by entry, state merges field by field,
registry paths/aliases union, and shared raw items keep both sides. Run this
on every machine before the first three-machine day:

```sh
brain update --yes
brain doctor --fix
git -C ~/brain-data config --show-origin --get-regexp 'merge\.'
brain sync --status --json
```

Two rules make the guarantees explicit:

- different records, notes, state fields, project paths and aliases merge
  without a manual conflict; a same-field state edit or an alias claimed by two
  machines is kept deterministically and reported as a conflict artifact for
  review;
- `brain sync --watch` or the scheduled task must run on every machine. One
  machine that never syncs cannot see or merge the others' writes.

A clean three-machine test (`TestThreeMachinesWriteConcurrently`) runs three
clones with two concurrent sessions each, then verifies every note, all state
fields and the registry entry survive the merge.

### Windows scheduled tasks run hidden

Task Scheduler starts an interactive task in the user's console, so a `.bat` or
a console program such as `brain.exe` opens a cmd window on every interval. Run
the job through `brain-task.exe`, the GUI-subsystem launcher in `bin/`:

```powershell
$action = New-ScheduledTaskAction -Execute 'C:\0project\lv24-brain\bin\brain-task.exe' `
  -Argument 'C:\0project\lv24-brain\scripts\brain-update.bat'
Set-ScheduledTask -TaskName 'Company Brain Update' -Action $action
```

The launcher runs the command (batch files go through `cmd /d /c`) with
`CREATE_NO_WINDOW`, waits for it and returns its exit code, so
`LastTaskResult` still reports failures. Build it with
`go build -ldflags "-H windowsgui" -o bin/brain-task.exe ./cmd/braintask`.
The task's `Hidden` setting is cosmetic and does not suppress the window on its
own. The same launcher works for other CLIs scheduled on the machine.

### MCP hosts and console windows

An MCP host that starts a server through a `.cmd` shim (for example
`codegraph.cmd` or `npx.cmd`) runs it through `cmd.exe /d /s /c`, which opens a
console window. `brain-task --stdio <command> [args...]` is a GUI-subsystem
proxy: the host sees a windowless process, the child keeps the inherited
stdin/stdout/stderr pipes, and a `.cmd` target still runs through a hidden
`cmd /d /c`. In an MCP client whose config takes a command array, use:

```json
"command": [
  "C:/0project/lv24-brain/bin/brain-task.exe",
  "--stdio",
  "C:/nvm4w/nodejs/codegraph.cmd",
  "serve",
  "--mcp"
]
```

The command array form matters: a plain string is a different (malformed)
config value in OpenCode. Native `.exe` servers already spawned directly by the
host do not need the wrapper.

### Legacy PII dump zones

`mail/`, root `github/` and `gitlab/`, `merges/` and orphan
`connectors/gitlab/` predate the connection publish policy. They are local-only
and anchored in `.gitignore`. `brain doctor` reports a tracked file there as
`pii-zone` / `tracked-pii-zone` with the untrack command; it never deletes or
rewrites anything. The files stay on disk and every blob stays in git history,
so a worktree copy removed by a pull can be restored with
`git checkout <commit>^ -- <path>`. Use `brain connection publish <id>
shared|local-only` for the current raw zone under `connectors/<id>`.

### Conflicts and merge drivers

`brain sync --status` lists conflicting paths as `conflict: <path>` lines.
`brain sync` refuses to run while they exist, and reports
`unresolved conflicts in <paths>` with exit 3.

Most single-file conflicts never reach you: `.gitattributes` routes
`projects/*/state.md`, `registry.json`, `projects/*/notes/*.md` and shared raw
items (`connectors/*/items/*.md`) through deterministic drivers. It also pins
`* text=auto eol=lf` and marks binaries, so a Windows clone with
`core.autocrlf=true` cannot create whole-file CRLF conflicts. `brain init`,
`brain doctor --fix` and every real `brain sync` register the drivers in
`.git/config` with the current executable; `brain doctor` warns when a clone is
missing them. A clone without drivers falls back to a normal Git conflict until
the next sync self-heals it.

- A state field both sides changed differently keeps the current value in
  `state.md` and writes both values to
  `projects/<id>/state-conflicts/<UTC>-<field>.md`.
- A registry alias claimed by two projects keeps one owner and writes
  `registry-conflicts/<UTC>-<alias>.md`.
- Notes are unioned by heading plus body hash and ordered by creation time.
- Shared raw items are content-addressed. The item driver strips the host
  `observed_at`; when two hosts still diverge it keeps ours at the canonical
  path and writes theirs to a deterministic `*-<content-hash>.md` sibling, so
  neither side is lost and the rebase completes.

To resolve a real conflict:

1. Inspect the repository: `git -C ~/brain-data status`. `brain sync` aborts the
   rebase before returning, so no rebase is in progress; the local commits and
   the autostash are restored.
2. Edit the conflicting Markdown files by hand. Keep the content that is still
   correct. When both sides hold different knowledge, keep both as separate
   records (for example with `brain note`), and open the newest
   `state-conflicts/` or `registry-conflicts/` record when one exists.
3. Stage the files with `git add` and commit, or remove the conflict markers the
   stash restore left and stage the result. If a stash restore conflicts,
   `brain sync` keeps the stash ref and names it in the error; resolve the
   markers and stage the result, then verify with `git stash list` and drop the
   entry only after the resolved files are committed.
4. Run `brain sync --status` until it reports no conflicts, then `brain sync --push`.

Do not use `git reset --hard`, do not take one side of a whole directory with
`--ours` or `--theirs`, and do not force-push. If a conflict cannot be resolved
safely, stop and ask the maintainer.

`brain import` (LvEF) never overwrites an existing file with different content:
a skipped record is a reported outcome (`conflicts`/`skipped` in the report and
`conflictIds` in `--json`), not a failure. Partial imports therefore exit **0**;
automation must read the report rather than infer success from the exit code.

### Raw zone and publishing (since 0.15)

`connectors/<id>/items/` and `connectors/<id>/checkpoints/` are a machine-local
cache: `brain sync` does not commit them unless the connection is shared, so two
machines that ingest the same connection never conflict on `observed_at` or
cursor bytes. Checkpoints are never published, not even for a shared connection.

Each run decides the policy in this order:

1. An explicit `publish: shared` or `publish: local-only` on the connection
   always wins.
2. No explicit policy, but git already tracks the connection's items or
   checkpoints: the zone keeps publishing exactly as before (grandfathered). The
   run changes nothing — no marker, no untrack, no rewrite — and prints a notice.
   This is what protects a pre-0.15 data repository on upgrade.
3. No explicit policy and nothing tracked: the zone is local-only.

Choose explicitly with `brain connection publish <id> shared|local-only`:

```sh
brain connection publish gh-acme local-only   # untrack, keep files and history
brain connection publish gh-acme shared       # publish items, keep checkpoints local
```

- `local-only` records the policy, writes the local marker and untracks
  `connectors/<id>` (`git rm --cached -r`). The next `brain sync --push` records
  the removal. The files stay on disk and blobs already committed stay in git
  history; untracking never rewrites history.
- `shared` records the policy, keeps items tracked and untracks
  `connectors/<id>/checkpoints/`. Shared item files are written without the host
  `observed_at`, so both machines produce the same bytes at the same path. Items
  written while the zone was local-only are rewritten once, at the switch. A
  legacy item that still carries a host clock is normalized by the
  `lv24-items` merge driver; a genuine divergence is kept under two distinct
  paths. Checkpoints stay `-merge -text` and gitignored. An explicit
  `publish: shared` is never silently downgraded to local-only by sync.
- Both are idempotent and print how many raw-zone files git tracked before and
  after. The command takes the connection lock, so it waits for an active run to
  finish instead of changing a zone mid-run.

Upgrading from an engine before 0.15 needs no manual step: connections whose raw
zone was already published keep publishing (grandfathered), and `brain doctor`
reports `raw-zone-policy` with the exact `brain connection publish` command to
make the choice explicit. A tracked zone of a connection that is already
explicitly local-only is reported as `raw-zone`; untrack it with
`git rm --cached -r connectors/<id>` (or `brain connection publish <id>
local-only`), which drops it from the index without deleting the working tree.

### Retired checkouts

`brain doctor` reports a `retired-paths: warn` when a retired directory still
exists on disk or is still registered to a project. The engine ships no
entries; a workspace lists its own in `retired_paths` in `brain.yaml` (for
example `<root>/old-checkout` → `harvest`). Selecting a path inside one
resolves to the replacement with a `brain: warning:` line, `brain discover`
skips it, and `brain project set --path` refuses it, so the folder cannot
silently return to the registry. The check reports only: delete the directory
yourself (`Remove-Item -Recurse`) and repoint any project that still lists it
with `brain project set --path`.

## 3. Hooks

The git hook records each commit in the journal. It is installed per clone, in
the repository's hooks directory (`.git/hooks` in a normal clone), and is not
committed.

```sh
brain hooks install --repo <path-to-project-checkout> --config ~/brain-data/brain.yaml
brain hooks status --repo <path-to-project-checkout>
brain hooks status --agents --json
brain hooks uninstall --repo <path-to-project-checkout>
```

- `install` flags: `--repo <path>` (default `.`), `--force`, and
  `--config <brain.yaml>`. The config path is recorded in the hook. Without
  `--config`, it falls back to `$BRAIN_CONFIG`, and if that is unset the path
  is omitted.
- `status` and `uninstall` accept `--repo <path>`.
- `status --agents [--home <dir>] [--json]` reports the coding-agent hooks
  instead of the git hook: installed, trusted, last run and last error per host
  (`claude`, `codex`, `opencode`). It reads the same files the local API
  `GET /v1/hooks` returns to Desktop, and it never edits an agent config. Codex
  trust drift is reported with the re-approval instruction
  (`/hooks` in Codex).
- The hook runs `brain event commit --quiet --repo <top-level>`.
- A post-commit hook that was not written by lv24-brain is not overwritten
  unless `--force` is given. With `--force` it is moved to a numbered
  `post-commit.lv24-brain-backup` file. Existing backups are never overwritten.
- Hook activity is read with `brain journal --event commit`.

## 3a. Connectors, code graphs, and project discovery

Local and git modes take connector targets from the brain data, so a scheduled
refresh needs no environment variables:

```sh
brain connector run --all --days 7          # github, gitlab, every mail account
brain connector run --name imap --account <id> --days 2
brain graphs --changed-only                 # graphify update / codegraph sync
brain graphs --dry-run                      # show what would run
brain discover --root <workspace>           # dry run: list new checkouts
brain discover --root <workspace> --apply   # register the new ones
```

- GitHub repositories come from registered projects' `repo_url`; GitLab
  projects from `repo_url` and the `gitlab`/`origin` remotes of registered
  checkouts; mail accounts from `<data-dir>/connectors/mail.json` (account id,
  host, port, username, folders; folder names may be stored in IMAP modified
  UTF-7). `BRAIN_GITHUB_REPOS`, `BRAIN_GITLAB_PROJECTS` and `BRAIN_IMAP_*`
  still override these defaults.
- Secrets: `gitlab/token`, `imap/<account>/password` in the OS store
  (`brain secret set <name>` reads stdin). Without `github/token`, the token
  is read from `gh auth token` at run time when the GitHub CLI is signed in;
  it is never stored.
- Each run is bounded to five minutes and a failing account does not stop the
  others; the command exits 1 if any run failed.
- Server mode: `brain connector run --name <provider>` calls
  `POST /v1/connectors/run` (admin only). Raw items are stored only with
  `--publish shared`; without it the connection's raw zone is local-only and
  the server refuses the run with 403 and an audit event, because a
  server-side local-only connection has no storage. Mail and chat default to
  local-only, so `brain connector run --name imap --allow-local-only` runs
  without storing any item (the run record counts them as skipped). Check
  `GET /v1/connectors/runs` for the record; item bodies never appear there.
- Named connections (v0.9.0, local/git only):
  `brain connection add github --id <id> --setting repos=o/r`,
  `brain connection add gitlab --id <id> --setting projects=g/p`,
  `brain connection add imap --id <id> --account <mail-id> --base-url imaps://host[:port] --setting username=... [--setting folders=...]`;
  secrets via stdin (`brain connection secret set <id> token|password`,
  stored as `conn/<id>/<field>`); `brain connection run <id> [--since RFC3339]
  [--days N] [--limit N] [--json]`; `brain connector run --all` prefers the catalogue
  when non-empty and falls back to the legacy targets above when empty.
  `--name` stays legacy snapshot (Markdown dumps); it is not native
  item-level. Deletes are never inferred. Stored items are the only input to
  `brain digest --source <id>`; the digest never re-runs the connection.
- OAuth (v0.12.0): `brain connection auth <id>` runs GitHub's
  device flow or GitLab's PKCE loopback flow (port 47615) using a public client
  ID from `--client-id` or `BRAIN_OAUTH_CLIENT_ID`. The bundle is stored only
  in the OS secret store (`conn/<id>/oauth` plus token-free metadata), never in
  `connections.json`. An OAuth session takes precedence over a PAT for `token`
  lookups; `--remove` deletes it and falls back to the PAT/token path. Refresh
  rotation is persisted before the new access token is used.
- Phase 1 native providers (v0.10.0, local/git only) are registered next to
  GitHub/GitLab/IMAP: `folders` (`paths`, optional `extensions`/`ignore`; no
  credentials; Obsidian/Logseq text is preserved), `jira` (`token` secret,
  optional `email`/`deployment`/`projects`/`jql`), `confluence` (`token`
  secret, optional `email`/`deployment`/`spaces`/`cql`), `linear` (`token`
  secret, optional `teams`/`authscheme`), `slack` (`token` secret,
  `channels` required, optional `threads`/`limit`; DMs and private channels
  are never discovered), `notion` (`token` secret, optional
  `databases`/`pages`/`version`), `sentry` (`token` secret, `org` required,
  optional `projects`/`environment`; at most three newest events per issue),
  `postgres-schema` (`host`/`database`/`user` + `password` secret, optional
  `port`/`sslmode`/`includesystem`), `sqlite-schema` (absolute `path`, optional
  `includesystem`), `rss` (`urls`, optional `limit`) and `web` (`sitemaps`,
  optional `maxpages`/`allowhosts`/`respectrobots`). Add/run them with
  `brain connection add|run`; `brain connector run --name` still accepts only
  `github`, `gitlab`, `imap`. The database sources read catalogs only and the
  web source never follows redirects or leaves the sitemap's host/port unless
  `allowhosts` permits it.
- The Phase 3 `mcp` source (v0.12.0, local/git only) connects to MCP servers:
  `transport=stdio` with `command` and newline-separated `args` stored as local
  credentials (never in the synced catalogue), or `transport=http` with an
  https `url` (loopback http for tests) and an optional `token` secret.
  Optional `tools` is an exact read-tool allow-list; without it the source
  selects names containing search/list/recent/query/find and always denies the
  destructive verbs or a destructive annotation. `maxitems` (default 100, max
  500), `query` (default `*`) and `timeout` (default 30s, max 5m) are bounded.
  Results are marked recent-only, and stdio commands inherit the agent
  environment: run only trusted servers.
- `brain graphs` runs `graphify` and `codegraph` only where their indexes
  already exist (`--init` creates missing ones), skips missing tools, and logs
  tool output to `<user cache dir>/lv24-brain/graphs.log`, outside the data
  repository.
- `brain discover` scans only the immediate children of `--root` (a `.git`
  directory or file, or `main/.git`), never modifies existing projects, and
  reports id conflicts instead of registering them.

### Connection checks

`brain connection test <id> [--json]` runs the provider's non-mutating check
(the same `Source.Check` a run uses) and exits non-zero on failure with a
redacted cause. `brain connection list` prints the newest run of each
connection — time, status and a redacted failure reason — so a connector that
stopped working is visible without opening logs.

### Identity migration (0.26)

The account label and the connector-declared identity settings (the IMAP
`username`) are personal data. 0.26 keeps the value in the secret store under
`conn/<id>/account` and `conn/<id>/username`, and leaves a marker in
`connectors/connections.json`:

```json
{"$keychain": "account", "fingerprint": "<16 hex>"}
```

Reading the value is lazy: a run or an explicit check resolves it when it needs
the identity, and `connection list` resolves it only to display the account. A
locked or missing keychain never fails the command; it prints `<keychain>` plus
one warning.

**Update every machine to 0.26 before applying the migration.** A pre-0.26
binary reads the marker as a literal account string, so its identity
comparison and `connection add` would be wrong; a second machine still on 0.25
could also import the legacy account back as a duplicate. The dry run is the
default and writes nothing; migrate in this order:

```sh
brain --version                                   # every machine: 0.26.0
brain connection migrate-identity --check
brain connection migrate-identity --apply
brain sync --push
```

`--json` returns the same report in either mode.

Statuses: `would-move`, `moved`, `already-migrated`, `missing-secret`,
`unreadable-secret`, `mismatched-secret`. Apply writes each secret, reads it
back, and only then rewrites the catalogue; a failed step never leaves a marker
without a secret, and re-running is idempotent. A raw legacy value stays a valid
fallback forever. Repair a missing secret with
`brain connection secret set <id> account` — the value must match the stored
fingerprint, because changing the account is a migration, not a secret set. A
marker whose secret is missing or unreadable fails the run with the key name and
the repair command, and `connection list` reports the same problem as a warning.

### Scrubbing raw-zone e-mail addresses

GitHub and GitLab items now hash e-mail addresses at ingest, like mail. Data
written before that, and legacy dumps, still carry raw addresses. The scrub
script rewrites only matched bytes to the same `[email:<12 hex>]` placeholder,
recomputes the item content hash, and is idempotent:

```sh
python scripts/scrub-raw-pii.py <data-dir>                  # dry run, per-zone counts
python scripts/scrub-raw-pii.py <data-dir> --include runs,items,dumps --apply
go run scripts/scrub-raw-pii-hash-check.go <data-dir>       # verify every item hash
```

It covers `connectors/<id>/items`, `connectors/runs` and the legacy `mail/`,
`github/` and `gitlab/` dumps. A file whose item frontmatter does not parse is
left untouched and reported in `unparsed`. Run it before `brain sync --push` so
the scrub is one commit, and keep a backup of the data directory.

## 3a2. MCP server for coding agents

`brain mcp` serves the brain over stdio with two protocol revisions: the
stateless `2026-07-28` revision (`server/discover`, per-request `_meta`,
`resultType` on every result, MRTR confirmations) and the `2025-06-18`
handshake revision for older clients. Tool annotations, output schemas, the
typed refusal envelope, pagination limits and the raw JSON-RPC harness are
documented in [mcp.md](mcp.md).

The 18 tools are (9 read-only, 6 append-only — the mixed `brain_code`/`brain_idea` are counted here — and 3 destructive):

- `brain_open` - load a project's state, recent decisions, attempts and last handoff.
- `brain_context` - context for one task: state, review findings, latest handoff, best-matching decisions/attempts/notes and failed attempts to avoid (`query`, optional `project`, `budget`, `kinds`, `since`).
- `brain_search` - full-text search over notes, decisions, attempts and handoffs (English and Russian word forms match; optional `since`); connector dumps only with `include_dumps` or `kinds` containing `dump`.
- `brain_record` - read records by id: `get`, `list` (paginated), `relations`, `history` and `links`; ideas are readable with `kinds:["idea"]`.
- `brain_idea` - the agent-first TODO layer: `add`, `list`, `get`, `update`, `done`, `reopen`, `drop`, `rm`, `restore` and `brief`; `rm` moves to trash and `restore` undoes it.
- `brain_note` - append a dated note; `suggest_topic` proposes a topic key without writing.
- `brain_decision` - record a decision with its context, alternatives and consequences.
- `brain_attempt` - record an attempt with its result and reason.
- `brain_handoff` - record a session handoff with a summary and next action.
- `brain_state` - read (`get`) or update the goal, next action, active work, blockers and open questions; only the given fields change.
- `brain_catchup` - what changed since the latest handoff (or the last `since_hours`); also lists the journal and places one record in a timeline.
- `brain_review` - find stale or contradictory knowledge; read-only, `brain review --fix` applies fixes in a shell.
- `brain_judge` - judge or reclassify existing knowledge: relation verdicts, accept/reject/supersede, quarantine review, pin/unpin and dedup consolidation (`list` is the read action).
- `brain_projects` - list registered projects with kind, paths and repository URL; `current` resolves the working directory.
- `brain_prompt` - store, search or discard prompts in the local-only prompt zone (`search` is the read action); every action is refused when `prompts.enabled` is false.
- `brain_code` - query the code graph of one checkout: `status`, `search`, `node`, `explore`, `impact`, plus the local `index`/`sync` refresh.
- `brain_graph` - inspect the graph: `stats`, `neighbors`, `path`, `communities`, `god_nodes`, `cycles`, `diff`, `pr_impact` and `surprises`.
- `brain_capabilities` - report the active profile, engine version, tool counts, features, limits, gaps and refusal codes.

`brain mcp --list-tools --json` prints the whole contract (titles, annotations,
schemas, page limits) without a config or a running server.

Register it once per machine with an explicit config, because
agents may start servers without the variables that locate the default config:

```sh
claude mcp add --scope user lv24-brain -- <brain> mcp --config <data-dir>/brain.yaml
codex mcp add lv24-brain -- <brain> mcp --config <data-dir>/brain.yaml
```

For OpenCode add the same command to the `mcp` section of
`~/.config/opencode/opencode.json`. Projects resolve from the server's working
directory (the agent's project root) or the `project` argument; writes never
create projects.

## 3a3. Background agent and local control API

`brain agent` runs in the foreground and is the single background engine for a
data directory; a second instance exits because the OS lock is held.

```sh
brain agent --config <data-dir>/brain.yaml
brain agent status --config <data-dir>/brain.yaml
brain agent stop --config <data-dir>/brain.yaml
```

- Private state lives in `<data-dir>/agent/` (0600, self-ignored):
  `runtime.json`, `schedule.json`, `stop.json`, `settings.json`, `agent.log`
  (rotated) and the process lock. Nothing in that directory is published.
- `runtime.json` holds `{port, token, pid, version, started_at}`. The token is
  a per-process 256-bit bearer secret; never log it, paste it into tickets or
  commit it. The file is removed on a clean shutdown.
- Schedules come from `Connection.Schedule` (`manual`, `@hourly`, `@daily`,
  `@every <duration>`, minimum 1m). The scheduler catches up once after sleep
  and never replays a backlog; a run that is already queued or running for the
  connection is skipped.
- The local API is loopback-only and ephemeral: use the port and token from
  `runtime.json`. It requires exactly one `Authorization: Bearer` header, the
  exact loopback `Host`, rejects `Origin`/`Referer`, has no CORS and bounds
  request bodies. It exposes:
  `GET /v1/health`; connections CRUD, `POST /v1/connections/{id}/test|run`,
  `POST /v1/connections/{id}/credentials/{field}`; `GET /v1/runs`,
  `GET /v1/runs/{id}`, `GET /v1/runs/{id}/log` (SSE); schedules, projects,
  non-secret settings; the desktop memory surface (`GET /v1/records`,
  `GET /v1/records/{id}`, `GET /v1/search`, `GET /v1/review`,
  `GET /v1/proposals`, `POST /v1/proposals/{project}/{id}/promote|dismiss`,
  `GET /v1/catchup`, `GET|PUT /v1/projects/{id}/state` with
  `expected_revision` CAS); `POST /v1/engine/update` with `{"confirm":true}`;
  `POST /v1/shutdown`. Consumers should not cache the token across restarts.
- `POST /v1/engine/update` downloads, verifies and stages a signed release and
  swaps the binary; the running process keeps the old image until restart, and
  `brain update --rollback` remains the recovery path.
- If the agent is unhealthy, check `agent status` first, then the tail of
  `agent/agent.log`; a stale runtime/token after a crash is cleaned on the next
  start while the lock is held. `brain agent stop` validates the PID is a brain
  executable before requesting shutdown.

## 3b. Shared agent instructions

One file in the private data repository, `<data-dir>/global/AGENTS.md`, holds
the instructions every coding agent must follow on every machine. `brain agents
sync` writes it into a managed block (between `LV24_SHARED_START` and
`LV24_SHARED_END` markers) in:

- `<workspace>/AGENTS.md` — the folder that contains the data directory;
- `~/.config/opencode/AGENTS.md` and `~/.codex/AGENTS.md`, when those tools are installed;
- `~/.claude/CLAUDE.md`, as a single `@<data-dir>/global/AGENTS.md` import line.

Text outside the markers is never touched. Edit the canonical file, run
`brain sync --push`, then `brain agents sync` on each machine (`--check` reports
drift without writing and exits 1).

`brain agents sync --hooks` (opt-in; the nightly job does not pass it) also installs
brain session hooks: SessionStart and SessionEnd in `~/.claude/settings.json`,
SessionStart, Stop and SessionEnd in `~/.codex/hooks.json`, and an OpenCode plugin in
`~/.config/opencode/plugins/` (`brain-session-end.js` if present, else `brain-handoff.js`).
Existing settings and hooks are kept, the commands are refreshed in place when the
binary or config path changes, and the first change to a file saves
`<file>.pre-brain-hooks`. Codex keeps new hooks disabled until you approve them in
Codex yourself (the owner approves hooks; brain does not). Brain never edits
`~/.codex/config.toml`. `--check --hooks` reports what would change and exits 1.

Scheduled refresh: on Windows the nightly "Company Brain Graph Refresh" task
runs the checkout's `scripts/brain-graphs.bat`
(`C:\0project\lv24-brain\scripts\brain-graphs.bat`), which applies
`brain update --yes` first, then runs graphs, connectors, `brain sync --push`
and `brain agents sync`; the 30-minute "Company Brain Update" task runs
`scripts/brain-update.bat` (signed update, then `brain sync --push`). The graph
task starts the batch file through `%LOCALAPPDATA%\lv24-brain\brain-graphs.vbs`
with `wscript.exe //B //Nologo`, so no console window appears; recreate the task
with that action (not the `.bat` directly) if it is ever registered again. On macOS
the LaunchAgent `by.lv24.brain-daily` (`~/Library/LaunchAgents/`, script
`~/.local/bin/brain-daily.sh`, log `~/Library/Logs/lv24-brain-daily.log`) runs
`brain sync --push` and `brain agents sync` daily at 09:30 or on wake after a
missed run; it runs in the login session, so git can use the keychain.
Run it now with `launchctl kickstart gui/$(id -u)/by.lv24.brain-daily`. The instructions refer to `brain` on PATH,
so they are identical on Windows and macOS.

### Daily digest

`brain digest` is the morning summary across all registered projects: what changed
in the window (default `24h`; Go durations such as `90m` or `24h`, or `7d`), per
project with activity, then a global "Needs you" section with proposed decisions
(listed whatever their age), open questions, blocked items and review finding counts.
Projects with nothing to report are omitted. Flags: `--project <id>` for one project,
`--json` for the envelope, `--write` to save the human digest to
`<data_dir>/digests/YYYY-MM-DD.md` (local/git mode; one file per day, overwritten on
rerun). The digests directory syncs with the data repo by design. Optional: add
`brain digest --write` to the nightly job (Windows
`C:\0project\lv24-brain\scripts\brain-graphs.bat`, macOS
`~/.local/bin/brain-daily.sh`); the scripts are not changed by brain.

`brain digest --source <connection>` is a separate, offline mode: it previews
proposal candidates from the raw items that connection already stored. It never
runs the source, calls no model, and writes nothing unless `--write` is given.
The default window is `30d`. Flags that only apply here: `--all` (ignore the
window), `--limit <n>` (default 500, must be positive), `--rule <names>`
(comma-separated; canonical names are `closed-issue`, `decision-label`,
`incident`, `merged-change`, `commit`, and aliases such as `adr`, `sentry`,
`merged-pr` are accepted), and `--dry-run` (the default; rejects `--write`).
`--project` chooses the destination when the connection or its items do not
resolve to exactly one project; an ambiguous or empty result exits `2`.
`--json` prints the proposal envelope with `mode: "proposals"`. Server mode
rejects the command.

### Curation: proposals and promotion (Phase 4.1)

Ingestion stays raw; curation is a separate, reviewed step:

```sh
brain connection run gh-main --days 30            # 1. store raw items
brain digest --source gh-main --since 7d          # 2. preview; writes nothing
brain digest --source gh-main --project myapp --write
brain review --project myapp                      # 3. read pending proposals + findings
brain promote proposal-gh-main-1a2b3c4d5e6f --as decision --status accepted
brain promote proposal-gh-main-1a2b3c4d5e6f --dismiss     # reject the candidate
```

- One proposal per `(project, connection, external item)`, stored at
  `<data-dir>/proposals/<project>/<proposal-id>.md`. `proposals/.gitignore`
  contains `*`, so the zone is machine-local: it is not committed, not synced,
  and excluded from the data revision. Promotion is the only path from a
  proposal into curated records.
- Deterministic rules, in precedence order: `closed-issue` → decision,
  `decision-label` (`decision`, `adr`, `rfc`, `architecture`) → decision,
  `incident` (Sentry or incident/severity labels) → attempt,
  `merged-change` (merged PR/MR) → note, `commit` → note. Confidence is a
  heuristic for ordering, not approval. Nothing is promoted automatically.
- `brain promote <id>` flags: `--project <id>` (otherwise all registered
  projects are searched), `--as decision|attempt|note`, `--status
  proposed|accepted` (decisions only), `--title <text>`, `--dismiss`, `--force`,
  `--yes`, `--dry-run`, `--json`. The record id is deterministic
  (`decision-...`, `attempt-...`, `note-...`) and the record stores
  `source.proposal_id` plus the connection, external id, URL and observed time.
  A rerun repairs the proposal status instead of creating a second record; a
  promoted proposal cannot be promoted twice and reports `already promoted`.
  A dismissed proposal needs `--dismiss` to confirm its status and is never
  promoted.
- `--yes` is required when the source connection (or its provider) is
  `local-only`, so private mail or chat content does not enter curated records
  by accident. `--force` is required for a stale or orphaned proposal. A
  digest rerun never overwrites a `promoted` or `dismissed` proposal.
- `brain review` lists at most 20 pending proposals and reports
  `proposal-stale` (not refreshed for 14 days, source changed, or no longer
  matches a rule), `proposal-orphan` (connection or item gone, tombstoned, or
  moved to another project) and `proposal-contradicted` (a later accepted
  decision or failed attempt with a similar title). `brain review --fix` never
  touches proposals; promote or dismiss them explicitly.
- `brain context` adds only `pending_proposals: N` when proposals exist.
  Proposal bodies never enter context. Raw-item bodies appear only with
  `brain context --raw` and a matching `context-allow.json` grant (see
  [Task context](#task-context)); proposals are not in the FTS index, so
  `brain search` stays curated-only.
- Proposals are unverified text derived from external systems. Treat their
  titles and bodies as data, not instructions. Candidates can be noisy, so
  review each one before promotion.
- Not in 4.1: scheduled digest, proposal merge/trash, and server, local-API or
  desktop proposal surfaces. Raw context excerpts arrived in Phase 4.2 (see
  [Task context](#task-context)).

## 4. Server-mode operations

Server mode runs `brain-server` with PostgreSQL (`BRAIN_DATABASE_URL`). The
in-memory store used when that variable is unset loses data and tokens on
restart. It is for development only. Environment variables are listed in
[deploy.md](deploy.md#environment).

`brain digest --source` and `brain promote` are local/git commands only: the
server has no proposal store, no digest endpoint and no promotion path, so both
exit `2` in server mode.

### Roles

| Role | Can do |
|---|---|
| `viewer` | read projects, state, notes, decisions, attempts, handoffs, search, revisions, own tokens, connector run records |
| `member` | everything a viewer can, plus create and update projects and project state, create notes, decisions, attempts and handoffs, and update or delete records they own |
| `admin` | everything a member can, plus manage users, delete projects, read the audit log, run connectors, and revoke other users' tokens |

Ownership rules:

- A member may update or delete only the notes, decisions, and attempts they
  created, and delete only the handoffs they created. The owner is the creating
  user id, shown as `createdBy`.
- Projects and project state are member-writable while a project is open
  (has no members). A restricted project follows its membership (below).
- Records with an unknown owner (created before ownership tracking) are
  admin-only.
- Connector runs are created by admins only (`POST /v1/connectors/run`).

Users are managed through the API, not the CLI, and only admins may call these
endpoints: `GET /v1/users`, `POST /v1/users`, `GET /v1/users/{id}`,
`PATCH /v1/users/{id}` (display name, role, auth source, with a `revision`),
and `DELETE /v1/users/{id}?revision=<n>`. Request fields are in
`GET /openapi.json`.

The first administrator can be created at startup with
`BRAIN_BOOTSTRAP_ADMIN_LOGIN` and `BRAIN_BOOTSTRAP_ADMIN_PASSWORD`. Set both or
neither; the password must pass the server's password policy. The account is
created once. On later starts an existing login is ignored, so the password
variable is not needed after the first successful start. Remove it from the
environment after that start.

### Project members (restricted projects)

A project without members is open to every user per their global role. Add
members to restrict it; then only members and admins can see it at all
(others get `404`, and it disappears from lists, search, revisions and SSE).
The project role is capped by the global role: a global viewer stays
read-only.

| Project role | Can do in that project |
|---|---|
| `viewer` | read |
| `member` | read, create records, put state, update/delete own records |
| `maintainer` | member, plus edit project metadata and update/delete any record |

Admin CLI (server mode, signed in as an admin):

```sh
brain project members acme                         # restriction state and members
brain project members acme add alice --role maintainer
brain project members acme add bob --role member   # same command changes a role
brain project members acme remove bob
brain project members acme restrict                # admin-only, no members yet
brain project members acme open                    # reopen; refused while members exist
```

API: `GET /v1/projects/{id}/members`, `PUT /v1/projects/{id}/members/{user}`
(`{"role": ...}`, user id or login), `DELETE /v1/projects/{id}/members/{user}`,
`PUT /v1/projects/{id}/access` (`{"restricted": true|false}`). All are
admin-only and audited (`GET /v1/audit?entityKind=project`).

Removing the last member keeps the project restricted (admin-only); reopen it
explicitly. Before restricting a busy project, add every member who still
needs it, then check `brain project members <id>`.

### Login and logout (clients)

```sh
brain init --mode server --server-url https://brain.example.test --config ~/brain-server.yaml
printf '%s\n' '<password>' | brain login --config ~/brain-server.yaml \
  --login <login> --password-stdin
brain status --config ~/brain-server.yaml
brain logout --config ~/brain-server.yaml
```

- `--password-stdin` reads one line from stdin. `BRAIN_PASSWORD` can be used
  instead of the flag, and `BRAIN_LOGIN` supplies the login when `--login` is
  omitted.
- `brain login` stores one credential bundle in the OS secret store, bound to
  the normalized server URL. It is never sent to another server.
- `brain login` warns when the URL is plain `http` and the host is not
  loopback. Use HTTPS for any shared server.
- `brain logout` revokes the session and its refresh-token family, then clears
  the stored bundle. An access token already issued stays valid until it
  expires (`BRAIN_ACCESS_TTL`, 15 minutes by default).

### Static tokens and BRAIN_TOKEN

- `BRAIN_TOKEN` (or `BRAIN_SECRET_SERVER_TOKEN`) is a static API token from the
  environment. It is intended for automation, not for a personal machine.
- While it is set, it takes precedence over stored credentials. `brain login`
  is refused, and `brain logout` leaves a stored session untouched and exits
  non-zero.
- A static token can be stored with `brain secret set server-token` (the value
  is read from stdin) and removed with `brain secret remove server-token --yes`.
- A static API token has no refresh token and cannot be revoked by
  `brain logout`. Revoke it on the server (see the incident checklist in
  section 8).

### API tokens

API tokens (prefix `brain_`) are created and revoked through the API:

- `POST /v1/tokens` with `name`, `role`, and `userId` (admins may mint for
  other users). The token role cannot exceed the role of the owner or of the
  caller. The plaintext is returned once; only a hash is stored.
- Set an expiry with `expiresInDays` (1 to 3650) or `expiresAt` (RFC 3339, in
  the future, at most 10 years away); not both. Without either, the token never
  expires until revoked. For automation, recommend `expiresInDays` (for example
  `90`) so a leaked or abandoned token stops working on its own. An expired token
  is rejected like a revoked one, and the response `apiToken.expiresAt` shows the
  expiry instant.
- `GET /v1/tokens` lists the caller's tokens. Admins may pass `userId`, or
  `all=true` to list every token.
- `DELETE /v1/tokens/{id}` revokes a token. Only the owner or an admin may do it.
- Scope a token with `scopes` (`read`, `write`, `admin`) and limit it to
  projects with `projectIds`. Omitted means unscoped and all projects, which is
  how every older token keeps working. `read` allows only GET requests, `write`
  adds knowledge/state/project writes, `admin` (admin-role tokens only) adds
  admin endpoints. Prefer the narrowest token for automation, for example a
  read-only token for dashboards:

  ```sh
  brain token create --name dashboard --scope read --project acme --expires 90d
  brain token create --name ci-writer --scope write --project acme --project infra --expires 720h
  brain token list            # admins: every user's tokens; others: their own
  brain token revoke <token-id>
  ```

  `create` prints the raw token once; it cannot be shown again. `list` never
  returns secrets or hashes, only id, name, owner, role, scopes, projects,
  created/expires/last-used time and status (active/expired/revoked).
  `--expires` takes days (`30d`) or a Go duration (`720h`), at most 3650 days;
  omit it for a token that never expires (`--expires-in-days N` still works).
  `--json` uses the standard `{schema, data}` envelope (`data.token`,
  `data.api_token`); since v0.6.3, v0.6.x printed `{token, apiToken}`. In local
  or git mode these commands exit 2.

  A token minted by another token can only narrow it (scopes, projects and
  expiry within the caller's). Scopes and projects are recorded in the
  `auth.token.created` audit event.

### Database pool, timeouts and request logs

- `BRAIN_DATABASE_MAX_CONNS` (default 10, range 1-200) sizes the pgx pool of
  each replica. Keep the sum across replicas below Postgres `max_connections`.
- `BRAIN_DATABASE_STATEMENT_TIMEOUT` (default `30s`, `0` disables, range
  1s-1h) is sent as `statement_timeout` on every connection. Startup migrations
  use the same limit. Before a large migration on a big table, raise it in
  `deploy/.env`, restart, and restore the value afterwards.
- Invalid values stop the server at startup with an error that names the
  variable.
- Request logs are JSON records on stderr: `msg="http request"`, `request_id`,
  `method`, `route` (the router pattern, never the raw path), `status`,
  `duration_ms`, and `user_id` for authenticated requests. Tokens, headers,
  query strings, and bodies are never logged. Server errors log at ERROR and
  health probes at DEBUG. Other logs keep the text format.

### Metrics

Metrics are off by default. With `BRAIN_METRICS_ENABLED=1` a second listener
serves Prometheus text format on `BRAIN_METRICS_ADDR` (default
`127.0.0.1:9090`). The public API port never serves `/metrics`.

- Series: `http_requests_total{method,route,code}`,
  `http_request_duration_seconds{method,route}` (histogram with fixed buckets
  from 5 ms to 10 s), `http_requests_in_flight`, `sse_clients`,
  `db_pool_acquired_connections`, `db_pool_idle_connections`,
  `db_pool_total_connections`, `db_pool_max_connections` (PostgreSQL only), and
  `brain_build_info{version,commit,go_version}`.
- Labels use the route pattern, such as `/v1/projects/{id}`. Unmatched paths
  share the label `unmatched`, and non-standard methods share `OTHER`, so
  cardinality stays bounded.
- The endpoint has no authentication. Keep it on loopback or on a private
  network. An address on a public interface exposes operational data.
- In the Compose stack the listener lives inside the container. To scrape it
  from another service on the same Compose network, set
  `BRAIN_METRICS_ADDR=0.0.0.0:9090` and scrape `brain-server:9090`. Do not
  publish the port to the host unless you need to.

### Retention

The retention job runs in every replica. The first run starts about one minute
after boot, then every `BRAIN_RETENTION_INTERVAL` (default `6h`). A Postgres
advisory lock lets only one replica sweep at a time. Set
`BRAIN_RETENTION_DISABLED=1` to turn it off. It is skipped in memory mode.

Each run deletes:

| Data | Rule |
|---|---|
| `audit_events` | `created_at` older than `BRAIN_AUDIT_RETENTION` (default `180d`). |
| `revisions` | `created_at` older than `BRAIN_REVISION_RETENTION` (default `90d`), and not referenced by any remaining audit event or as an entity's current revision. Audit rows are swept first, so revisions they held are freed in the same run. |
| `refresh_tokens` | `expires_at` in the past. Revoked rows are removed when they expire. |
| `auth_revocations` | `revoked_at` older than `BRAIN_REFRESH_TTL` (default `720h`). Tokens guarded by a tombstone have expired by then. |
| `api_tokens` | `expires_at` more than 30 days in the past. Tokens without an expiry are never removed. |

Effects to expect: `/v1/revisions` and SSE replay cover a shorter history. A
client that reconnects with an older `Last-Event-ID` receives only the events
that remain. Deletes run in batches of 1000 rows per statement, at most 500
batches per table per run, so locks stay short. Leftover rows go to the next
run. Each run logs `retention completed` with per-table counts, or
`retention failed` with the error.

### Audit log

Security events are kept in an audit log that only admins can read:

```sh
curl -sS -H "Authorization: Bearer $BRAIN_TOKEN" \
  "https://brain.example.test/v1/audit?action=auth.refresh.reused&limit=100"
```

`GET /v1/audit` (admin only) filters by `actor`, `action`, `entityKind`,
`entityId`, `since` and `until` (RFC 3339), with `limit` (default 50, maximum
500) and `offset`. The response is `{"items": [...]}`.

Recorded actions:

| Action | Meaning |
|---|---|
| `auth.login.succeeded`, `auth.login.failed`, `auth.login.rate_limited` | local password logins |
| `auth.logout` | session logout |
| `auth.refresh.reused` | a rotated refresh token was presented again |
| `auth.oidc.login`, `auth.oidc.linked`, `auth.oidc.signup`, `auth.oidc.refused` | OIDC sign-in outcomes |
| `auth.token.created`, `auth.token.revoked` | API token lifecycle |

Audit records carry the actor, the entity, and the client address in the
`client` detail field. They never contain passwords or tokens. Writes are
best-effort: a failed audit write is logged and does not fail the request.
Viewers and members never see these events.

### Proxies and rate limits

- Behind a reverse proxy, set `BRAIN_TRUSTED_PROXIES` to the proxy addresses,
  as comma-separated IPs or CIDRs. Only from those peers is `X-Forwarded-For`
  honoured, and it is walked from the right so spoofed left-hand entries are
  ignored. Empty means the header is ignored. In that case every client shares
  the proxy's address for rate limiting.
- Under Compose the peer is the Docker network gateway, for example
  `172.16.0.0/12` (see [deploy.md](deploy.md)).
- `BRAIN_LOGIN_RATE_LIMIT` sets the attempts per minute allowed per client
  address and, separately, per account.

### OIDC

OIDC uses a browser flow (`GET /v1/auth/oidc/login` and the callback). There is
no CLI OIDC flow. Settings: `BRAIN_OIDC_ISSUER`, `BRAIN_OIDC_CLIENT_ID`,
`BRAIN_OIDC_CLIENT_SECRET`, `BRAIN_OIDC_REDIRECT_URL`, `BRAIN_OIDC_SCOPES`,
`BRAIN_OIDC_GROUPS_CLAIM`, `BRAIN_OIDC_GROUP_ROLES` (`group=role` pairs),
`BRAIN_OIDC_DEFAULT_ROLE`, and `BRAIN_OIDC_ALLOW_SIGNUP`.

Account linking rules, stated plainly:

- A first OIDC login is linked to an existing local account only when the
  provider asserts `email_verified` and the verified email equals the account
  login.
- Admin accounts are never linked implicitly.
- An unverified email is never used as the login.
- Every other collision is refused with `403` and audited as
  `auth.oidc.refused`. Linking is audited as `auth.oidc.linked`.

## 5. Upgrades

### Client binary

```sh
brain update --check                    # report only
brain update --check --channel beta     # check a named channel
brain update --yes                      # verify and apply the newest release
brain update --rollback                 # restore brain(.exe).previous
```

- `--channel` takes `stable` or `beta`; the default comes from `brain.yaml`.
  It cannot be combined with `--rollback`. `--rollback` cannot be combined
  with `--check` or `--yes`.
- `--check` reports only. `--yes` downloads, verifies the signature and the
  SHA-256 checksum, and replaces the `brain` executable. On Windows the
  replacement happens after the process exits, so the new version takes effect
  on the next start.
- The updater never changes the data repository.
- Before ordinary commands the CLI performs a notify-only daily check. Set
  `BRAIN_NO_UPDATE=1` to disable it.

### Upgrade to 0.27

Engine 0.27.0 keeps data schema 1 and the Markdown format unchanged; the local
index moves to schema 10 and rebuilds itself once. It adds the support gate
(`supported`/`related`/`none`), content-hash duplicate collapse, stub filtering,
context `excluded[]`/`near_misses[]`, the index freshness line and the
`index-generation` doctor check. Server mode is unchanged and has no support
gate.

```sh
brain update --yes
brain doctor
brain index
brain open .
brain search "release" --json
```

1. Mixed versions are safe: Markdown is unchanged, so a pre-0.27 binary can
   keep writing. That machine keeps its old index and does not collapse
   duplicates or hide stubs until it is upgraded and reindexed; update it when
   you want the same retrieval there. An older binary that opens a schema-10
   index recreates it at its own version, so alternating versions on one machine
   reindexes each time.
2. `brain index` makes the one-time schema-10 rebuild explicit; otherwise the
   first command pays it. `brain doctor` should then report `index-generation`
   as ok. A one-commit lag is normal and does not warn.
3. In a registered checkout, `brain open` and `brain status` should print
   `tier: POPULATED` and `index: complete at <sha>`. `tier: ABSENT` (not
   registered) or a `cwd:`/`registered_paths:` block means this is not the
   checkout you meant; `index: stale at` means the index was not rebuilt after a
   commit or sync.
4. Optional checks: an absent-topic query should return `support.tier: none`
   with empty `hits` and `lower-rank` entries in `excluded[]`;
   `brain search --include-stubs` should bring back session stubs;
   `brain note --similar` should print up to three similar records.

No data migration and no new environment variable are required.

### Upgrade to 0.26

The data schema stays 1 and records stay readable in both directions. The local
search index moves to version 9 and rebuilds itself once.

1. **Update every machine first.** `brain update --yes` or rebuild and replace
   the binary; confirm `brain --version` on each. Do not apply the identity
   migration until every machine reports 0.26.0: a pre-0.26 binary does not
   understand the keychain marker stored in `connectors/connections.json`.
2. `brain doctor`. Fix what it names; `brain connection list` warns about an
   identity marker whose secret is missing or unreadable.
3. `brain index` (or accept the one automatic reindex inside the first search).
4. `brain connection migrate-identity --check`, then `--apply`, then
   `brain sync --push` so the markers reach the other clones.
5. Optional, if raw zones predate e-mail hashing: dry-run
   `python scripts/scrub-raw-pii.py <data-dir>`, then `--apply` and
   `go run scripts/scrub-raw-pii-hash-check.go <data-dir>`.

`brain context` budgets are tokens in this release (default 6000, maximum
32000), not runes; a config that sets `context.budget` is honoured unchanged.

### Upgrade to 0.16 / 0.17 (engine 0.15 and newer)

A short checklist; the reasons are in [release-0.16.md](release-0.16.md) and
[release-0.17.md](release-0.17.md).

1. **Upgrade every machine first.** Either `brain update --yes` or rebuild from
   the branch and replace the binary; confirm with `brain --version`. Data
   schema stays 1, but an older engine that edits an imported or classified
   record drops the new optional fields.
2. **`brain doctor`.** Expect merge drivers registered, no negative `limits`
   values, and — after an engram import — the quarantine and prompt-zone counts.
   Fix what it names before writing records.
3. **Rebuild the index:** `brain index --rebuild`. The projection is at schema
   version 6 (validity, relations, `subtype`/`scope`/`topic_key`); the index
   rebuilds itself when its stored version differs, but a manual rebuild is the
   cheapest way to prove it.
4. **Restart every MCP host.** The tool list is read at startup.
   `brain mcp --list-tools --json` must report 18 tools (11 under `--read-only`);
   `--profile=lv24` is the only profile in 0.17. A retired 0.16 name answers
   `NOT_FOUND` with `try_instead`; move the client to the new tool and action,
   not just to a renamed name.
5. **Re-check client calls** against [mcp.md](mcp.md#11-migration-from-016):
   `brain_state_get/set`, `brain_journal`, `brain_records`, `brain_relations`,
   `brain_review_record` and `brain_decision_status` are gone, and
   `brain_decision(status=...)` is refused in favour of `brain_judge`.
6. **Engram import, in order:** stop engram writes, back up `engram.db` and the
   data directory, `brain migrate --from engram --db <path> --dry-run`, read the
   counts, apply, then `brain migrate --rollback latest` if the result is wrong.
   See [import-engram.md](import-engram.md). A rollback does not remove
   `index/`; rebuild after reverting.
7. **Prompt capture is opt-in.** Set `prompts.enabled: true` in `brain.yaml`
   before `brain prompt save`; the zone is local-only, gitignored and excluded
   from the index and revision. `brain doctor` reports retention without
   deleting anything.

Optional after the upgrade: `brain eval --fixtures eval/fixtures --baseline
eval/baseline.json` for the retrieval baseline, and a `limits` block in
`brain.yaml` if agents write to this data directory.

### Signed manifests

Each release publishes `release.json` and `release.json.sig`. The updater
accepts a release only when the signature verifies against a key embedded in
the binary (`trustedReleaseKeys` in `internal/update/signature.go`). An unsigned
release, a release signed by another key, or a tampered manifest is skipped. A
build with no embedded key refuses to update. The installers verify
`checksums.txt` only. See [update.md](update.md#signatures).

### Release signing setup (maintainer, once)

```sh
go run ./internal/tools/releasesign keygen -out <path-outside-the-repo>/release-signing.key
```

- `keygen` creates the file (it must not already exist) with the base64 private
  seed, and prints the public key.
- Add the public key to `trustedReleaseKeys` and commit it. A public key is not
  a secret.
- Store the seed as the `BRAIN_RELEASE_SIGNING_KEY` Actions secret, for example
  `gh secret set BRAIN_RELEASE_SIGNING_KEY < <path-outside-the-repo>/release-signing.key`.
  Then keep an offline backup and delete the file.
- The release workflow fails when the secret is missing. The signing step
  (`releasesign sign -in release.json -out release.json.sig`) reads the seed from
  `BRAIN_RELEASE_SIGNING_KEY` and refuses to sign unless the matching public key
  is embedded in the build. `--allow-unembedded` exists for testing only and must
  not be used for a release.
- Verify a manifest locally with
  `go run ./internal/tools/releasesign verify -in release.json -sig release.json.sig`
  (add `-pub <base64>` to check against a specific key).

### Key rotation

The updater accepts every key in `trustedReleaseKeys`, so rotation does not
break installed binaries:

1. Run `keygen` for the new key pair.
2. Add the new public key next to the old one and release a binary. Sign that
   release with the old key, which is still the active secret.
3. Once users are on that release, replace the `BRAIN_RELEASE_SIGNING_KEY`
   secret with the new seed.
4. Sign later releases with the new key.
5. In a later release, remove the old public key. Releases signed only by the
   old key are then refused by binaries built from that release.

## 6. Database migrations and the upgrade gate

- Server schema version: **10** (`SchemaVersion` in
  `internal/adapters/postgres/postgres.go`). Migrations are embedded from
  `internal/adapters/postgres/migrations/`, from `0001_init.sql` to
  `0010_record_provenance.sql`.
- On startup the server applies embedded migrations to an empty database.
- For an existing schema with pending migrations, the server refuses to start
  unless `BRAIN_DATABASE_BACKUP_CONFIRMED=1` is set. The message names the
  pending versions.
- Migrations are forward-only. There are no down-migrations. Rollback means
  restoring a backup and running the previous image.

Gate procedure for an upgrade:

1. Confirm that no migration or long-running job is in progress.
2. Take a backup with `pg_dump -Fc` as described in
   [deploy.md](deploy.md#backup). Store it outside the Docker volumes.
   Check that it is readable with `pg_restore --list <dump-file>`.
3. Start the new server with `BRAIN_DATABASE_BACKUP_CONFIRMED=1` set in its
   environment.
4. Check `GET /ready`, and run `brain doctor --config <path>` and check the
   `server` line.
5. Remove `BRAIN_DATABASE_BACKUP_CONFIRMED` from the environment. Leaving it set
   would let a later migration run without the gate.

Rollback after a failed upgrade: start the previous image (tag or digest) and
restore the backup taken in step 2, following [deploy.md](deploy.md#restore).

## 7. Legacy migration and rollback

Use this only for a legacy `.brain` directory. For the overall plan see
[migration.md](migration.md).

```sh
brain migrate --from <legacy-dir> --data-dir <data-dir> --dry-run
brain migrate --from <legacy-dir> --data-dir <data-dir>
brain migrate --rollback latest --data-dir <data-dir>
brain migrate --rollback <manifest.json> --data-dir <data-dir> [--dry-run]
```

- `--from` is required for an import. `--data-dir` names the target, or
  `--config` can name a `brain.yaml` that holds it. `--rollback` cannot be
  combined with `--from`.
- Always run `--dry-run` first. It reports created, unchanged, conflicting and
  failed files without writing.
- The import is idempotent. It never overwrites a target file whose content
  differs. Conflicts and failures make the command exit non-zero.
- A successful import writes `schema_version: 1` to the target configuration.
- Each import writes a backup manifest under `<data-dir>/backups/`
  (`migrate-*`). `--rollback latest` picks the newest one. A manifest is
  accepted only for the target directory it was written for.
- Mail, connectors, the journal, legacy registry metadata, and global
  conventions are not imported.
- Keep the legacy directory read-only until the new engine has passed the
  parity checks.

## 8. Incident checklists

### Lost or leaked API token

Applies to `brain_` API tokens, the static `server-token` secret, and
`BRAIN_TOKEN` values. The server stores only hashes, so the listing never shows
a token value.

1. Find the token id with `GET /v1/tokens` (as the owner) or
   `GET /v1/tokens?all=true` (as an admin).
2. Revoke it: `DELETE /v1/tokens/{id}` (owner or admin).
3. Confirm the revocation: `GET /v1/audit?action=auth.token.revoked` (admin).
4. Check for unexpected mints during the exposure window:
   `GET /v1/audit?action=auth.token.created&since=<RFC 3339 time>`.
5. Issue a replacement with `POST /v1/tokens` and give it to the owner through
   a channel that is not this repository or a chat log.
6. On each client, run `brain secret remove server-token --yes` and
   `brain secret set server-token`. For automation, update the `BRAIN_TOKEN`
   value in the secret store of the job.
7. If the leaked value cannot be matched to a token id, revoke every token of
   that owner from the list and reissue them.

A leaked JWT access token is cut off as soon as its session is revoked: run
`brain logout` on the affected client (or delete/demote the user), and the
server rejects that session's access tokens on the next request.

### Refresh-token reuse alert

The event `auth.refresh.reused` means a refresh token that had already been
rotated was presented again. The server treats this as a leak and revokes the
whole token family (`ErrRefreshReused`). The user must sign in again.

1. Pull the events around the alert:
   `GET /v1/audit?action=auth.refresh.reused&since=<RFC 3339 time>`, and the
   `auth.login.*` and `auth.oidc.*` events in the same window.
2. Compare the `client` detail with the usual addresses for the account. The
   reuse event itself has no actor, so correlate by time and by the login events.
3. Ask the account owner whether they signed in from that client. If not,
   treat the account as compromised.
4. Review what the account wrote during the window with `GET /v1/audit?actor=<user-id>`.
5. The v1 API has no password-change and no revoke-all-sessions endpoint. For a
   compromised account, remove it with `DELETE /v1/users/{id}?revision=<n>`
   and create a new one with `POST /v1/users`, using a new password.

### Suspected release signing-key leak

Treat any exposure of `BRAIN_RELEASE_SIGNING_KEY`, its offline backup, or its
file as a compromise.

1. Stop signing. Remove or replace the `BRAIN_RELEASE_SIGNING_KEY` secret.
2. Find releases published after the suspected exposure. Remove any that are
   not authentic, with the GitHub UI or `gh release delete`. The release
   workflow never overwrites an existing release, so this step is manual.
3. Generate a new key pair (section 5). Build a release whose
   `trustedReleaseKeys` contains only the new key, and sign it with the new seed.
4. Limit of this procedure: installed binaries still trust the leaked key until
   they are replaced. The updater cannot revoke a key remotely, so an attacker
   with the seed can publish a manifest that those binaries accept. The new
   binary reaches users only by a manual download and install. Publish the
   checksums (`checksums.txt`) with it, and tell users to reinstall.
5. Server images are built from source and are not affected by the updater.
   Rebuild them with the new code if the leak touched the build pipeline.
6. Record the incident and the cut-over date in the private data repository
   with `brain decision` or `brain note`. Do not record the seed or any value
   from it.

### Data repository conflict

Follow section 2, Conflicts. Summary:

1. `brain sync --status` lists the conflicting paths; a conflicted sync exits 3
   after aborting its rebase and restoring the autostash.
2. Resolve them by hand in the data repository. Check the newest
   `projects/<id>/state-conflicts/` and `registry-conflicts/` records for values
   the merge drivers preserved, then commit the resolution.
3. `brain sync --status` must report no conflicts.
4. `brain sync --push`, then `brain index` on each machine that pulled changes.
5. Never force-push and never discard the other side's changes without reviewing them.
