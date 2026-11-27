# Release 0.28.0 - Ideas: an agent-first TODO layer

Status: released 2026-11-27.

## Summary

Engine 0.28.0 adds **Ideas**, a small mutable TODO layer that sits between an
idea in conversation and a project task. One Markdown file per idea lives in
the private data repository, carries an optional due time and reminder lead,
and appears as a capped block at the start of an agent session. The engine owns
storage, validation, the index, the CLI, MCP and the local API; desktop 1.8.0
consumes the same contract for its list, editor, calendar and reminders.

Data schema stays **1**. The local index moves to **11** and rebuilds itself
once. Server mode is explicitly unsupported in this release.

## What changed

### Record model and storage

- One file per idea at
  `projects/<project_id>/ideas/idea-<YYYY-MM-DD>-<slug>-<ULID>.md`, using the
  existing YAML frontmatter, atomic write, redaction and write limits. The id
  is globally unique, so the same id addresses the idea on every surface.
- Statuses `open`, `doing`, `done`, `dropped`, `someday`; priority
  `low`/`normal`/`high`; tags; optional `remind_before_minutes` (0-10080).
- Exactly one of timed `due_at` (RFC3339 with offset) or all-day `due_date`
  (`YYYY-MM-DD`) may be set. `due_date` does not shift when travelling.
- Add is create-only. Edits are in place, including a project move that renames
  the file. Delete is a reversible move to `projects/<id>/trash/ideas/`;
  restore never overwrites a live file.
- A conflicted or unparsable idea file becomes a typed problem row: it is never
  deleted, rewritten or silently dropped, and other ideas stay usable. The CLI
  lists it under `PROBLEMS`, `brain doctor` warns, and the local API answers
  `409`.

### CLI

`brain idea` adds `add`, `list`, `show`, `edit`, `done`, `reopen`, `drop`,
`rm`, `restore`, `brief`, `mute` and `ical`. Project resolution only accepts a
registered project or the current checkout, with `personal` as the documented
fallback; it never invents a project. `brain idea brief` prints the same capped
block the session hook uses, and `brain idea ical` writes a one-way `.ics`
export.

### MCP

- New tool `brain_idea` with actions `add`, `list`, `get`, `update`, `done`,
  `reopen`, `drop`, `rm`, `restore` and `brief`. `rm` is reversible trash;
  `list`/`get`/`brief` are the read actions.
- The `lv24` profile now has **18 tools**. A read-only server lists **11** of
  them: the nine annotated read tools plus `brain_code` and `brain_idea`, whose
  write actions are refused before the backend runs.
- `brain_record` exposes ideas through `kinds:["idea"]`; `brain_search --kind
  idea` searches title and body.

### Local API

Additive routes on the frozen `localapi-v1` contract:

| Method | Route | Purpose |
|---|---|---|
| `GET` / `POST` | `/v1/ideas` | list with filters / create |
| `GET` / `PATCH` / `DELETE` | `/v1/ideas/{id}` | read / edit with CAS / move to trash |
| `POST` | `/v1/ideas/{id}/restore` | restore from trash |
| `GET` | `/v1/ideas/brief` | capped session brief |
| `GET` | `/v1/ideas/ical` | `.ics` file export |

`PATCH` accepts `expected_updated_at`; a stale revision returns `409` with
`error.current_revision` and writes nothing. Problem files are `409`, unknown
ids are `404`. Server mode returns `400 unsupported` for every ideas route.

### Session start and `brain open`

- `brain session start` adds an Ideas block only for `source=startup`, after the
  handoff and before decisions: at most 3 items, at most 700 characters, one
  indexed lookup, no body text. A failure removes only the block.
- The block is global but ordered for the current project (up to two project
  items, then one global overdue/today item, then the count).
- `brain idea mute [--days N|--off]` stores a machine-local greeting mute.
- `brain open` adds one pointer line and, when the budget allows, the project's
  top items.

### Search and index

- Local index schema **11** adds `due_at`, `due_date` and `priority`; a version
  mismatch drops and rebuilds the derived index automatically. Markdown remains
  the source of truth.
- `brain index` includes `projects/<id>/ideas/*.md`; unparsable files are
  problem rows and are not indexed.
- Ideas are excluded from `brain context` and knowledge retrieval. `brain
  doctor` reports the `idea` kind, problem-file count and index schema.

## Compatibility

- **Data schema stays 1.** Every change is additive: new files, a new kind,
  new optional frontmatter fields, new routes, a new CLI command and one new MCP
  tool. Existing record kinds, flags, routes and tool names keep their meaning.
- **An old engine ignores idea files without corruption.** Verified with the
  0.27.2 binary: it indexed 0 idea documents, returned no idea hits, reported
  `brain doctor` ok, and left the idea file byte-for-byte unchanged (SHA-256
  match). An old engine simply cannot list, complete or remind.
- **The local index is 11.** It is gitignored and derived; the first command
  after the upgrade rebuilds it once. No user data is touched.
- **Upgrade every machine before relying on idea sync.** An un-upgraded machine
  keeps writing records normally but does not show ideas.
- **Register every project an idea may target.** `brain idea add` uses the
  current checkout or `personal`; an unknown id is rejected with candidates.
  After a checkout moves, `brain project set <id> --path <checkout>` restores
  resolution. See the runbook's "Upgrade to 0.28" section.
- **Server mode is unsupported.** The CLI, MCP and local API return
  `unsupported`/`ErrUnsupported`; the domain model is ready for a later
  Postgres table.

## Limitations (v1)

- **Server mode has no ideas.** No Postgres table, no server routes, no
  multi-replica story.
- **Reminders are desktop-only.** The Rust scheduler runs while desktop 1.8.0
  is open or in the tray; quitting stops reminders. A missed trigger becomes one
  digest on the next launch, and quiet hours are 22:00-08:00 by default.
- **Calendar support is one-way.** `.ics` is a file export, not a live
  subscription; there is no two-way Google/Apple/CalDAV sync or event creation.
- **No idea merge driver.** A git conflict in an idea file is reported as a
  problem row and must be resolved manually; a field-level `lv24-ideas` driver
  is a follow-up.
- **Non-goals for v1:** recurring ideas, kanban/sprints/assignees/comments,
  mobile or Telegram capture, push reminders outside the desktop app, embeddings
  and semantic duplicate detection.

## Validation

Local verification on Windows (Go 1.27), from the release tree:

| Check | Result |
|---|---|
| `go build ./...` | pass |
| `go vet ./...` | pass |
| `go test ./... -count=1` | pass, every package; no `-p` override and no flake |
| `gofmt` on changed Go files | no formatting diff (the worktree uses CRLF via `core.autocrlf=true`; the LF-normalized file is clean) |
| `brain mcp --list-tools --json` | 18 tools, 9 annotated read-only |
| `brain mcp --list-tools --json --read-only` | 11 listed tools |
| 0.27.2 reads a 0.28 data dir with an idea file | 0 idea documents, no hits, `doctor` ok, SHA-256 unchanged |
| 0.28 indexes the same file | 1 idea document; `--kind idea` search finds it; `brain idea brief` shows it |

Contract tests cover stale-revision CAS (`409`, no write), problem-file
handling, trash/restore collisions, project resolution, due-date offsets across
DST, the session block (startup only, mute, workspace root, non-fatal failure),
the read-only MCP policy and the local-API routes. The branch workstreams also
exercised a live CAS conflict returning `409` and the 18-tool MCP listing.

## Upgrade

```sh
brain update --yes                          # on every machine
brain project list
brain project set <id> --path <checkout>    # register a missing project
brain doctor
brain index
brain idea brief
```

The detailed steps and rollback notes are in the
[runbook](runbook.md#upgrade-to-028). No data migration and no new environment
variable are required.
