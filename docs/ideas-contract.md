# Ideas contract (0.28)

Status: **frozen for engine 0.28.0 / desktop 1.8.0**
Owner: WS0 (contract freeze)
Authority: `docs/spec-0.28-ideas.md`; this document freezes the exact domain,
storage, CLI, MCP and local-API interface that every later workstream builds
against. If this document and the spec disagree, the spec wins and this
document is fixed in the same change.

Scope: the engine-side contract only. Storage, CLI, MCP, local API, session
brief, index and desktop code are implemented by WS1-WS10; none of them may
rename a field, route, flag or transition listed here without updating this
document and every surface in one change.

- [1. Record model](#1-record-model)
- [2. Frontmatter](#2-frontmatter)
- [3. Go domain contract](#3-go-domain-contract)
- [4. Validation rules](#4-validation-rules)
- [5. Status transitions](#5-status-transitions)
- [6. Time semantics](#6-time-semantics)
- [7. Project resolution](#7-project-resolution)
- [8. Store interface](#8-store-interface)
- [9. CLI](#9-cli)
- [10. MCP `brain_idea`](#10-mcp-brain_idea)
- [11. Local API](#11-local-api)
- [12. Index and search](#12-index-and-search)
- [13. Session brief and reminders](#13-session-brief-and-reminders)
- [14. Problem rows and conflicts](#14-problem-rows-and-conflicts)
- [15. Limits, redaction, privacy](#15-limits-redaction-privacy)
- [16. Compatibility and non-goals](#16-compatibility-and-non-goals)

## 1. Record model

One Markdown file per idea:

```
projects/<project_id>/ideas/<idea-id>.md
```

`<idea-id>` is `idea-<YYYY-MM-DD>-<slug>-<ULID>`:

- the date is the **UTC** date of `created_at`, zero padded (`2006-01-02`);
- the slug is `domain.Slugify(title)`, lowercase ASCII, at most 60 bytes,
  `item` when the title has no usable characters;
- the ULID is a lowercase 26-character Crockford base32 id (`domain.NewULID`);
- the id is also the file-name stem, so it must satisfy
  `domain.IsRecordID("idea", id)`.

The id is globally unique; the project is metadata, not part of the id or the
file name. The same id therefore addresses the idea on every surface without a
project selector.

Rules:

- Append is create-only. An existing file with the same id is an
  `ErrTargetExists`; the writer never overwrites it and never adds a numeric
  suffix to the id.
- Edits are in place on the same file (status, due, title, body, tags,
  project). Moving an idea to another project rewrites `project_id` and renames
  the file to `projects/<new_project>/ideas/<same-id>.md`.
- Delete is a move to `projects/<project_id>/trash/ideas/<file>`; it is
  restorable and never removes the file from git history on its own.
- The body is the Markdown after the frontmatter, verbatim. The title is stored
  only in frontmatter and is not repeated in the body.
- A file that cannot be parsed or validated is a problem row, not a silent
  drop; see §14.

## 2. Frontmatter

The on-disk format follows the other brain records: YAML frontmatter delimited
by `---`, one blank line, then the body. `schema_version` is **1** (the data
schema does not change for ideas).

```yaml
---
project_id: lv24-brain-desktop
kind: idea
updated_at: 2026-11-26T10:00:00Z
schema_version: 1
origin: agent
id: idea-2026-11-26-calendar-dark-theme-01jcs6abcdefghjkmnpqrstvwx
title: Тёмная тема для календаря
status: open
priority: normal
due_at: 2026-11-27T18:00:00+03:00
remind_before_minutes: 120
tags:
    - ui
    - calendar
source:
    type: session
    id: ses_01jcs6example
    harness: opencode
created_at: 2026-11-26T10:00:00Z
---
Сделать тёмную тему в календаре. Проверить контраст и клавиатуру.
```

An all-day idea uses `due_date` instead of `due_at`:

```yaml
due_date: 2026-11-27
```

A completed idea adds `completed_at` and may add `completion_note`:

```yaml
status: done
completed_at: 2026-11-27T15:30:00Z
completion_note: Тема проверена на контраст.
```

### 2.1 Field table

| Key | Type | Written | Default / notes |
| --- | --- | --- | --- |
| `project_id` | string | always | registry id; never invented |
| `kind` | string | always | `idea` |
| `updated_at` | RFC3339 UTC | always | record revision; every write moves it |
| `schema_version` | int | always | `1` |
| `origin` | `human\|agent\|connector\|import` | always | `human` when empty |
| `reviewed` | bool | only `false` | absent means reviewed |
| `trust` | `verified\|stated\|inferred` | only non-verified | absent means verified |
| `valid_from` | RFC3339 | only when different from `created_at` | record validity |
| `valid_to` | RFC3339 | optional | empty means current |
| `superseded_by` | record id | optional | same relation as other records |
| `supersedes` | record id | optional | same relation as other records |
| `confidence` | 0..1 | only non-zero | optional author confidence |
| `relations` | list of `{target, relation, reason?, confidence?, status?, judged_at?, verdict_by?}` | only non-empty | typed relation set |
| `merged_from` | list of record ids | only non-empty | merge bookkeeping |
| `merged_into` | record id | optional | merge tombstone |
| `subtype` | string | optional | classification |
| `scope` | `project\|personal\|global` | only non-default | absent means `project` |
| `topic_key` | string | optional | classification |
| `occurrences` | int | only non-zero | classification |
| `review_after` | RFC3339 | optional | review scheduling |
| `reviewed_at` | RFC3339 | optional | review scheduling |
| `history` | list of `{at?, body}` | only non-empty | bounded previous bodies |
| `id` | string | always | `idea-...`; see §1 |
| `title` | string | always | 1..200 Unicode code points |
| `status` | enum | always | `open` when absent on read |
| `priority` | enum | always | `normal` when absent on read |
| `due_at` | RFC3339 with offset | optional | timed due; exclusive with `due_date` |
| `due_date` | `YYYY-MM-DD` | optional | all-day due; exclusive with `due_at` |
| `remind_before_minutes` | int 0..10080 | optional | absent or `0` = no lead |
| `tags` | list of strings | optional | at most 10, each 1..40 code points |
| `source` | map | optional | `{type, id?, harness?}`; see §3.3 |
| `completed_at` | RFC3339 UTC | only with `status: done` | omitted or `null` when unset |
| `completion_note` | string | optional | reason for `dropped`, note for `done` |
| `created_at` | RFC3339 UTC | always | creation instant |

Not written for ideas: `content_updated_at` (ideas are not knowledge records)
and `permalink` (the id is the stable reference). Readers must ignore both if
they appear in a hand-edited file. Readers must also ignore unknown keys, as
`markdown.Parse` does for every kind.

`history` is reserved and is not written or read for ideas in v1:
`ideaFrontmatter` has no `history` field, so an unknown `history:` key is
ignored, and the body is never split into a `## History` section. A later
revision that needs history must define the render and split rule first.

`tags` may be written in YAML block or flow style; readers accept both. The
writer uses the block style produced by `gopkg.in/yaml.v3`. Field order in the
file is an implementation detail; readers must not depend on it.

### 2.2 YAML types

- Timestamps (`updated_at`, `created_at`, `completed_at`, validity, review,
  relations) are RFC3339 strings, normalized to UTC for the writer.
- `due_at` is the one timestamp that keeps its offset; see §6.
- `due_date` is a string, never a timestamp, so no YAML date coercion can shift
  it.
- `remind_before_minutes` is a plain integer, never a duration string.

## 3. Go domain contract

Package `internal/domain`, file `idea.go`.

### 3.1 Status and priority

```go
type IdeaStatus string

const (
    IdeaOpen    IdeaStatus = "open"
    IdeaDoing   IdeaStatus = "doing"
    IdeaDone    IdeaStatus = "done"
    IdeaDropped IdeaStatus = "dropped"
    IdeaSomeday IdeaStatus = "someday"
)

func (s IdeaStatus) Valid() bool
func (s IdeaStatus) Terminal() bool // done or dropped
func ParseIdeaStatus(value string) (IdeaStatus, error)

type IdeaPriority string

const (
    IdeaPriorityLow    IdeaPriority = "low"
    IdeaPriorityNormal IdeaPriority = "normal"
    IdeaPriorityHigh   IdeaPriority = "high"
)

func (p IdeaPriority) Valid() bool
func ParseIdeaPriority(value string) (IdeaPriority, error)
```

`ParseIdeaStatus` trims surrounding whitespace, does not fold case, rejects the
empty value and returns a `*ValidationError` with field `idea.status` otherwise.
`ParseIdeaPriority` trims, maps empty to `normal`, does not fold case, and
rejects unknown values with field `idea.priority`.

An empty `Idea.Status` reads as `open`, an empty `Idea.Priority` reads as
`normal`. The store writes the concrete value, so the file is always explicit.

### 3.2 `Idea`

```go
type Idea struct {
    ID                  string       `json:"id"`
    ProjectID           string       `json:"projectId"`
    Title               string       `json:"title"`
    Body                string       `json:"body,omitempty"`
    Status              IdeaStatus   `json:"status"`
    Priority            IdeaPriority `json:"priority,omitempty"`
    DueAt               *time.Time   `json:"dueAt,omitempty"`
    DueDate             string       `json:"dueDate,omitempty"`
    RemindBeforeMinutes *int         `json:"remindBeforeMinutes,omitempty"`
    Tags                []string     `json:"tags,omitempty"`
    Source              *IdeaSource  `json:"source,omitempty"`
    CompletedAt         *time.Time   `json:"completedAt,omitempty"`
    CompletionNote      string       `json:"completionNote,omitempty"`
    Provenance
    Validity
    MergeInfo
    Taxonomy
    TopicHistory
    Relations RelationList `json:"relations,omitempty"`
    ReviewSchedule
    CreatedAt time.Time `json:"createdAt"`
    UpdatedAt time.Time `json:"updatedAt"`
}

func (i Idea) Validate() error
```

The embedded fields are the same record metadata every other kind carries, so
`Provenance`, `Validity`, `MergeInfo`, `Taxonomy`, `TopicHistory`, `Relations`
and `ReviewSchedule` validate through the existing helpers and the redaction and
limits paths apply unchanged.

Exported limit constants:

```go
const (
    IdeaDateLayout             = "2006-01-02"
    MaxIdeaTitleChars          = 200
    MaxIdeaTags                = 10
    MaxIdeaTagChars            = 40
    MaxIdeaRemindBeforeMinutes = 10080 // one week
)
```

The body size limit is the existing `limits.max_body_bytes` (default 1 MiB) and
is enforced at the app boundary, not hard-coded in the domain. Metadata
(`tags`) is also checked against `limits.max_metadata_bytes` by the app.

### 3.3 `IdeaSource`

```go
type IdeaSource struct {
    Type    string `json:"type"`
    ID      string `json:"id,omitempty"`
    Harness string `json:"harness,omitempty"`
}

func (s IdeaSource) Validate() error
```

Valid `Type` values: `session`, `note`, `decision`, `commit`, `manual`,
`desktop`. A source is optional; when present, `Type` is required.

- `id` is required for `session`, `note`, `decision` and `commit`; it may be
  empty for `manual` and `desktop`, and may still be set for them.
- `id` is at most 256 bytes and `harness` at most 64 bytes; both must be
  trimmed, valid UTF-8 and free of control characters.
- `source` is a small idea-specific value object, not `domain.SourceRef`. It has
  no project id: an idea captured in project A may be tagged to project B.
- A `session` id is provenance only; the session itself is never stored.

### 3.4 ID helper

```go
func NewIdeaID(date time.Time, slug string) (string, error)
```

Returns `idea-<UTC date>-<Slugify(slug)>-<ULID>`. It normalizes the slug, so a
title is accepted as well as an already slugified string; an empty or unusable
slug becomes `item`. When the OS random source fails it returns the `NewULID`
error instead of an id with a missing or reused suffix. `IsRecordID("idea", id)`
is true for every non-empty id it returns.

## 4. Validation rules

`Idea.Validate` checks, in this order:

| Field | Rule | Error field |
| --- | --- | --- |
| `project_id` | required | `idea.projectId` |
| `title` | required, not blank, at most 200 code points, valid UTF-8, no control characters | `idea.title` |
| `body` | valid UTF-8, no NUL | `idea.body` |
| `status` | empty or one of the five statuses | `idea.status` |
| `priority` | empty or one of the three priorities | `idea.priority` |
| `due_at` / `due_date` | exactly one or neither; `due_at` non-zero; `due_date` a real `YYYY-MM-DD` (zero padded, leap years honored) | `idea.due`, `idea.dueAt`, `idea.dueDate` |
| `remind_before_minutes` | when set, 0..10080 | `idea.remindBeforeMinutes` |
| `tags` | at most 10; each non-blank, at most 40 code points, valid UTF-8, no control characters | `idea.tags` |
| `source` | optional; see §3.3 | `idea.source.*` |
| `completed_at` | only when status is `done`; when set, non-zero | `idea.completedAt` |
| `completion_note` | valid UTF-8, no NUL | `idea.completionNote` |
| embedded metadata | provenance, validity, merge, relations, taxonomy, history rules | `idea.*` |

A `done` idea without `completed_at` is accepted by `Validate` (the field is
one-way constrained); every writer sets it when moving an idea to `done`.
Reopening clears `completed_at` and `completion_note`. A `dropped` idea keeps
the reason in `completion_note`.

The 1 MiB body cap and the 16 KiB metadata cap are `LimitError`s from the app
boundary, not `ValidationError`s.

## 5. Status transitions

`CanTransition(from, to IdeaStatus) bool`:

| from \ to | `open` | `doing` | `someday` | `done` | `dropped` |
| --- | --- | --- | --- | --- | --- |
| `open` | yes (idempotent) | yes | yes | yes | yes |
| `doing` | yes | yes (idempotent) | yes | yes | yes |
| `someday` | yes (unpark) | no | yes (idempotent) | yes | yes |
| `done` | yes (reopen) | no | no | yes (idempotent) | no |
| `dropped` | yes (reopen) | no | no | no | yes (idempotent) |

- `open <-> doing` is the working pair.
- Any non-terminal status may move to `done`, `dropped` or `someday`.
- `done` and `dropped` reopen only to `open`.
- `someday` unparks to `open`; without that edge a parked idea could never be
  started. It does not jump straight to `doing`.
- A same-status write is allowed and idempotent.
- Empty `from` reads as `open`; empty or unknown `to` is refused; unknown
  statuses are always refused.
- `done` is never inferred from a commit or a code change. Only an explicit
  user action (CLI, desktop, MCP `done`, or an explicit `edit --status done`)
  sets it.
- A create with `status: done` is also an explicit action: the writer sets
  `completed_at` to now, so no done idea is written without it.

## 6. Time semantics

### 6.1 Timed ideas (`due_at`)

- Stored as RFC3339 **with the supplied offset**, e.g.
  `2026-11-27T18:00:00+03:00`; the offset is data, not a display artifact.
- The writer keeps the offset; the reader renders it in the machine's local
  time. Round-tripping through git or SQLite must not silently convert it to
  UTC.
- Comparisons (`DueBefore`/`DueAfter`, overdue, reminders) use the instant.
- The zone name is not preserved (a fixed offset is enough); callers must not
  depend on `Europe/Moscow` surviving a round trip.

### 6.2 All-day ideas (`due_date`)

- Stored as `YYYY-MM-DD`, a **local calendar date**. It does not shift when
  travelling or when the machine zone changes.
- The effective instant is **09:00 in the machine's local zone** on that date.
  This is the reminder anchor and the sort anchor; the list windows of §6.3
  compare the local date instead.
- Day grouping ("today", "overdue", "this week") uses the local date directly,
  so an all-day idea due today is not "overdue" at 10:00.

### 6.3 Windows

A window is a pair of bounds; callers pass instants:

- `DueAfter` is **inclusive**: keep ideas whose effective due is at or after it.
- `DueBefore` is **exclusive**: keep ideas whose effective due is before it.
- A timed idea compares its `due_at` instant.
- An all-day idea compares its `due_date` to the local calendar date of the
  bound: `due_date >= localDate(DueAfter)` and
  `due_date < localDate(DueBefore)`. The local zone is the reading machine's
  `time.Local`; no stored zone is consulted.
- An undated idea is excluded when either bound is set.

Examples (`now = 2026-11-27T10:00+03:00`, local):

| Query | `DueAfter` | `DueBefore` |
| --- | --- | --- |
| overdue | zero | local midnight today |
| today | local midnight today | local midnight tomorrow |
| this week | local midnight today | local midnight + 7 days |
| next 2 hours | now | now + 2h |

The UI may group a timed item due earlier today under "today" rather than
"overdue"; that is a presentation choice, not a different filter.

### 6.4 Reminder lead

`remind_before_minutes` is concrete and optional:

- absent or `0`: notify at the effective due instant only;
- `N > 0`: notify at `effective due - N minutes` and again at the due instant;
- all-day ideas anchor at 09:00 local; the lead applies to that anchor;
- the desktop's default lead is applied when an idea is **created or edited**
  and written into the record; changing the setting never rewrites old ideas;
- the agent may omit the field, which means no lead.

## 7. Project resolution

Every write resolves its project before touching storage:

1. **Explicit** — `--project` / MCP `project` / API `project`, matched exactly
   like existing writes: id, alias or checkout path. An unknown selector is a
   `400 validation` / `ErrValidation` with candidate suggestions; a project is
   never invented from the title or body.
2. **Working directory** — when omitted, the registered project of the current
   working directory.
3. **Default** — when the cwd is not registered, the existing `personal`
   project.

The result is reported as `project_source`:

| Value | Meaning |
| --- | --- |
| `explicit` | the caller passed a project selector |
| `cwd` | resolved from the working directory |
| `default` | fell back to `personal` |

`brain idea add` (CLI), MCP `add`, and `POST /v1/ideas` return
`project_source`; list responses return the resolved scope's
`project_source` when a project was resolved implicitly and `explicit`
otherwise. A global list (`--project all`, MCP `all: true`) is not a project
resolution: it has no single `project_source`, so the field is omitted, and
`app.IdeaListResult.ProjectSource` stays empty for that scope.

Default-scope reads use the same resolution: `brain idea list` without
`--project`, MCP `list` without `project`, and `brief --scope current` all
resolve cwd → `personal`. `brain idea list --project all` and MCP
`list {all: true}` list every project. The local API has no working directory,
so `GET /v1/ideas` without `project` is global; that is the call the desktop's
"All projects" default uses.

Semantic tagging is the LLM's job, not the engine's:

- the agent receives the current project and the registry;
- when the idea clearly names another project, the agent looks up the exact
  id/alias with `brain project list --json`;
- it never invents ids; if ambiguous it asks one short question or uses
  `personal`;
- a wrong tag is visible as a chip and fixed with `edit --project` / PATCH.

Moving an idea rewrites `project_id` and renames the file. If a project later
disappears from the registry, the idea is still readable and shows the raw id;
nothing is deleted because a registry entry changed. A move to a project that is
not in the registry is rejected.

Prerequisite: `C:\0project\lv24-brain-desktop` must be registered (WS0/WP0 in
the plan) before ideas can be tagged to it. Until then the engine rejects that
explicit id with candidates; the agent may choose `personal` instead, but the
engine never falls back for an explicit selector.

## 8. Store interface

New optional port in `internal/ports` (file `ideas.go`), so `ports.Store` does
not break for server clients:

```go
const RecordIdea = "idea"

type IdeaFilter struct {
    ProjectID string
    Statuses  []domain.IdeaStatus
    Tags      []string
    DueBefore time.Time
    DueAfter  time.Time
    Query     string
    Limit     int
}

type IdeaStore interface {
    AppendIdea(ctx context.Context, idea domain.Idea) (domain.SyncRevision, error)
    ListIdeas(ctx context.Context, filter IdeaFilter) ([]domain.Idea, error)
    GetIdea(ctx context.Context, id string) (domain.Idea, error)
    UpdateIdea(ctx context.Context, idea domain.Idea) error
    TrashIdea(ctx context.Context, projectID, id string) error
    RestoreIdea(ctx context.Context, projectID, id string) error
}
```

The compile-time assertion belongs to the adapter, not to `ports`:
`var _ ports.IdeaStore = (*Store)(nil)` lives in `internal/adapters/fsstore`.
`internal/ports` must not import an adapter.

`IdeaFilter` semantics:

| Field | Meaning |
| --- | --- |
| `ProjectID` | empty = all registered projects plus orphan `projects/*/ideas` directories; otherwise one registered project |
| `Statuses` | empty = all five; otherwise the listed statuses |
| `Tags` | empty = all; otherwise exact case-insensitive AND match over the idea tags; applied before `Limit` |
| `DueBefore` | exclusive upper bound (§6.3): timed compares the instant, all-day the bound's local date; zero = no bound |
| `DueAfter` | inclusive lower bound (§6.3): timed compares the instant, all-day the bound's local date; zero = no bound |
| `Query` | case-insensitive substring over title, body and tags; empty = all |
| `Limit` | at most this many rows; `<= 0` = no store limit (the caller caps) |

Store behaviour:

- `AppendIdea` accepts an already resolved, concrete `ProjectID` and only
  checks that it is a registered id: it never resolves an alias or path,
  registers a project, or falls back to `personal`. Project resolution,
  registration and the cwd/personal fallback stay in `internal/app`
  (`ensureProject` and the idea project resolver). The store sets
  `created_at`/`updated_at` when zero, defaults `status` to `open` and
  `priority` to `normal`, normalizes provenance, and allocates
  `NewIdeaID(createdAt, title)` (propagating its error) when `ID` is empty. A
  non-empty `ID` must satisfy `IsRecordID("idea", id)`; otherwise the write is
  rejected. The writer never falls back to a legacy name and never adds a
  numeric suffix: it writes atomically and refuses an existing id with
  `ErrTargetExists`. It returns the new revision.
- `ListIdeas` scans the project idea directories of registered projects (and,
  for the empty `ProjectID` scope, orphan `projects/*/ideas` directories so a
  removed project's idea stays visible), skips unparsable files and records
  them for `ProblemReporter` (kind `idea`, path + reason). It applies every
  filter, including `Tags`, before `Limit`, and returns a deterministic order:
  1. status rank: `open`, `doing`, `someday`, `done`, `dropped`;
  2. ideas with a due before ideas without one;
  3. due ascending (effective instant; all-day uses 09:00 local);
  4. priority rank: `high`, `normal`, `low`;
  5. `updated_at` descending;
  6. id ascending.
- `GetIdea` finds an idea by globally unique id across registered projects and
  orphan `projects/*/ideas` directories, so a removed project's idea stays
  readable and shows the raw id. `ErrNotFound` when absent. A problem file
  returns a `domain.ProblemError{Path, Kind, Reason}` (a typed conflict), never
  a zero idea.
- `UpdateIdea` locates the stored file by id, preserves `created_at`, sets
  `updated_at` to now (UTC), validates the whole record, and rewrites
  atomically. `updated_at` is strictly increasing: when the write clock has not
  advanced past the stored revision (the store clock has second precision), it
  writes `stored.updated_at + 1s`, so a same-second write never repeats a
  revision. If `ProjectID` differs it also moves the file to the new
  project's directory, and the new project must be registered; an in-place edit
  of an idea whose project was later removed from the registry still works. It
  never changes the id. A problem file returns `domain.ProblemError` and is not
  modified. Last-write-wins locally; the app enforces the optional
  `expected_updated_at` (§11.4) against the stored revision and returns
  `domain.RevisionConflictError` without writing on a mismatch. A git conflict
  is surfaced as a problem row (§14).
- `TrashIdea` moves `projects/<project>/ideas/<file>` to
  `projects/<project>/trash/ideas/<file>` and `RestoreIdea` moves it back.
  `ErrNotFound` for a missing live record/trash entry; `ErrConflict` when a
  live file already uses the id. A problem file is not moved: `TrashIdea`
  returns `domain.ProblemError` and leaves the file for manual resolution.
  Both operations require a registered project: an orphan
  `projects/*/ideas` idea is readable, listable and editable in place, but it
  cannot be trashed or restored until its project is registered again.
- The optional `ProblemReporter` (`Problems(ctx, projectID)`) reports
  `RecordProblem{Path, Kind: "idea", Reason}` exactly as it does for other
  kinds. It takes a registered project id, so the global list reports problems
  for registered projects only; an orphan problem file is still surfaced by
  `GetIdea` as a `domain.ProblemError`.
- MCP `list` and `GET /v1/ideas` must expose the same `tags` filter (exact
  case-insensitive AND) in WS4/WS5; the store already applies it.

The generic `RecordEditor` (`TrashRecord`/`RestoreRecord`) is not extended to
`idea` in v1: `brain record trash` and `brain record restore` refuse ideas;
only `brain idea rm|restore` (and the API routes in §11) move them. `brain
record list|show` may read ideas by adding `idea` to its kind allowlist.

A store without the optional `IdeaStore` (server mode) has no ideas:
`brain record list` still lists knowledge records and simply omits ideas, while
the dedicated idea use cases (`AddIdea`, `ListIdeas`, `GetIdea`, `UpdateIdea`,
`TrashIdea`, `RestoreIdea`) return `ErrUnsupported`.

## 9. CLI

`brain idea <action>`. Ids are globally unique, so only commands that can
create or move an idea take `--project`; reads accept `--project` to narrow.

```
brain idea add <title> [--project ID|auto] [--body-file -|PATH]
                       [--due today|tomorrow|YYYY-MM-DD|RFC3339] [--at HH:MM]
                       [--remind 2h|1d|MINUTES] [--priority low|normal|high]
                       [--tag TAG ...] [--source TYPE:ID] [--harness NAME] [--json]
brain idea list [--open|--done|--all] [--status open,doing,done,dropped,someday]
                [--project ID] [--due today|tomorrow|week|overdue]
                [--tag TAG ...] [--search TEXT] [--limit N] [--json]
brain idea show <id> [--json]
brain idea edit <id> [--title TEXT] [--body-file -|PATH] [--project ID]
                     [--due today|tomorrow|YYYY-MM-DD|RFC3339] [--at HH:MM]
                     [--clear-due] [--remind 2h|1d|MINUTES] [--clear-remind]
                     [--status open|doing|done|dropped|someday]
                     [--priority low|normal|high] [--tags a,b] [--clear-tags] [--json]
brain idea done <id> [--note TEXT] [--json]
brain idea reopen <id> [--json]
brain idea drop <id> [--reason TEXT] [--json]
brain idea rm <id>
brain idea restore <id>
brain idea brief [--project ID] [--max N] [--scope current|all] [--json]
brain idea mute [--days N] [--off]
brain idea ical [--project ID] > ideas.ics
```

### 9.1 Flag semantics

| Flag | Semantics |
| --- | --- |
| `--project ID\|auto\|all` | for `add`/`edit` and the default-scope reads, `auto` is the default and means §7 resolution; an id/alias/path is explicit and must exist. On `list`, `all` lists every registered project and is the only way to override the cwd default. `brief` is global by default (`--scope all`), so it needs no `--project` |
| `--due` | `today`/`tomorrow` = all-day local date; `YYYY-MM-DD` = all-day; RFC3339 = timed and keeps the offset |
| `--at HH:MM` | upgrades the `--due` date to a timed `due_at` in the machine's local zone; requires a date from `--due` |
| `--remind` | `2h`, `1d` or plain minutes; written as a concrete integer; `0` means no lead |
| `--tag` | repeatable; comma-separated values are also accepted; at most 10 total |
| `--source TYPE:ID` | `session`, `note`, `decision`, `commit`, `manual` or `desktop`; `ID` may be omitted for manual/desktop |
| `--harness NAME` | source harness, e.g. `opencode`; requires `--source` (or is ignored without one) |
| `--status` on list | repeatable/comma-separated; overrides the `--open`/`--done`/`--all` shortcut |
| `--open` | default: `open` and `doing` |
| `--done` | `done` only |
| `--all` | all five statuses |
| `--due` on list | `today`, `tomorrow`, `week`, `overdue`; maps to §6.3 windows |
| `--tag` on list | exact case-insensitive tag match; multiple tags are AND |
| `--search` | case-insensitive substring over title/body/tags |
| `--limit` | default 100, maximum 1000; `0` means no cap |
| `--max` on brief | default 3, range 1..10 |
| `--scope` on brief | `current` = the resolved project; `all` = global (default) |
| `--days` on mute | default 7, 1..365 |
| `--off` on mute | clears the mute |

`--due`/`--at` and `--clear-due` are mutually exclusive; the same holds for
`--remind`/`--clear-remind` and `--tags`/`--clear-tags`.

### 9.2 Human output

```
OPEN (4)
  [ ] today 18:00  Тёмная тема календаря   lv24-brain-desktop  🔔2h
  [!] overdue 2d   Позвонить стоматологу   personal
  [ ] high         Закрыть issue #1        lv24-brain

DONE (2)
  [x] ~~Починить футер~~                   lv24-brain
```

- `[ ]` open/doing/someday, `[!]` overdue, `[x]` done, `[~]` dropped.
- Done and dropped titles are wrapped in `~~...~~`; nothing is deleted.
- `today HH:MM` for timed items, `today` for all-day, `overdue Nd` for past
  days, `high`/`low` for non-normal priority, `Fri` style for the current week.
- Project chip and reminder chip are shown when set.
- `brain idea list` adds a `PROBLEMS (n)` section with `path: reason` rows.
- `brain idea show` prints the metadata block, body, source and completion
  note.
- Empty list prints `no ideas` and exits 0.

### 9.3 JSON output

Every `--json` command uses the existing envelope:

```json
{"schema": 1, "data": { ... }}
```

Data shapes (snake_case, same field names as §11):

- `add`, `edit`, `done`, `reopen`, `drop`, `restore`:
  `{"idea": <idea row>, "project_source": "explicit|cwd|default"}`.
- `show`: `{"idea": <idea row>}`; an id is resolved without a project selector,
  so there is no `project_source` to report.
- `list`: `{"ideas": [<row>], "problems": [{"path","kind","reason"}], "total": N, "project_source": "explicit|cwd|default"}`.
  `project_source` is omitted for `--project all`, where there is no single
  resolved project.
- `brief`: `{"text": "...", "ideas": [<row>], "open": N, "due_today": N, "muted": bool}`.
- `rm`: `{"id": "...", "trashed": true}`.
- `mute`: `{"muted_until": "RFC3339"|null}`.

An idea row is:

```json
{
  "id": "idea-...",
  "project_id": "lv24-brain",
  "title": "Тёмная тема",
  "body": "...",
  "status": "open",
  "priority": "normal",
  "due_at": "2026-11-27T18:00:00+03:00",
  "due_date": null,
  "remind_before_minutes": 120,
  "tags": ["ui"],
  "source": {"type": "session", "id": "ses_...", "harness": "opencode"},
  "completed_at": null,
  "completion_note": "",
  "created_at": "2026-11-26T10:00:00Z",
  "updated_at": "2026-11-26T10:00:00Z",
  "path": "projects/lv24-brain/ideas/idea-....md",
  "provenance": {"origin": "agent", "reviewed": true, "trust": "verified"},
  "validity": {"valid_from": "...", "valid_to": null, "superseded_by": "", "supersedes": "", "confidence": 0},
  "relations": [],
  "review": {"review_after": null, "reviewed_at": null}
}
```

`body` is omitted from list rows and included by `show`/`add`/`edit`. Null means
"not set"; empty string means an empty value.

Exit codes: `0` success (including an empty list), `1` runtime/validation error,
`2` usage error. `--json` writes one document to stdout and nothing else; errors
go to stderr.

## 10. MCP `brain_idea`

One action-based tool, matching `brain_record`; total MCP surface becomes 18
tools. Input properties are snake_case.

### 10.1 Actions

`add | list | get | update | done | reopen | drop | rm | restore | brief`

### 10.2 Input schema

| Property | Type | Actions | Notes |
| --- | --- | --- | --- |
| `action` | enum, required | all | one of the ten names |
| `id` | string | get, update, done, reopen, drop, rm, restore | globally unique id |
| `title` | string | add, update | 1..200 code points |
| `body` | string | add, update | Markdown; 1 MiB cap |
| `project` | string | add, list, brief | id/alias/path; omit for §7 resolution |
| `all` | boolean | list | `true` lists every project; default `false` |
| `status` | enum | update | `open`, `doing`, `done`, `dropped`, `someday` |
| `priority` | enum | add, update | `low`, `normal`, `high` |
| `due_at` | string | add, update | RFC3339 with offset; mutually exclusive with `due_date` |
| `due_date` | string | add, update | `YYYY-MM-DD`; mutually exclusive with `due_at` |
| `remind_before_minutes` | integer | add, update | 0..10080 |
| `tags` | array of strings | add, update, list | on add/update it replaces the list (at most 10); on list it is an exact case-insensitive AND filter (WS5) |
| `source` | object | add | `{type, id?, harness?}` |
| `note` | string | done | completion note |
| `reason` | string | drop | stored in `completion_note` |
| `statuses` | array of enum | list | empty = all |
| `due_before` | string | list | RFC3339 exclusive upper bound |
| `due_after` | string | list | RFC3339 inclusive lower bound |
| `query` | string | list | case-insensitive substring |
| `limit` | integer | list | default 50, max 200 |
| `max` | integer | brief | default 3, range 1..10 |
| `scope` | enum | brief | `current` or `all` (default `all`) |
| `clear` | array of enum | update | `due`, `reminder`, `tags`; see below |

`clear` is the only way to remove a value in `update`; an omitted field means
"unchanged". `clear: ["due"]` removes both `due_at` and `due_date`;
`clear: ["reminder"]` removes the lead; `clear: ["tags"]` empties the list.
Setting `due_at` or `due_date` replaces and clears the other. Moving a project is
`project` on `update`. `status: done` sets `completed_at` when absent; reopening
clears `completed_at` and `completion_note`. `all: true` together with `project`
is a validation error.

### 10.3 Responses

- `add`, `update`, `done`, `reopen`, `drop`, `restore`: the idea in the
  snake_case row shape of §9.3, plus `project_source` on writes.
- `get`: the idea row with body; no `project_source` (the id is global).
- `list`: `{"ideas": [...], "problems": [...], "total": N}`; list rows omit
  `body`.
- `brief`: `{"text": "...", "ideas": [...], "open": N, "due_today": N, "muted": bool}`;
  the same function that renders the session block.
- `rm`: `{"id": "...", "trashed": true}`.
- Errors use the existing MCP refusal format; unknown project and unknown id are
  separate refusals, never a silent empty result.

### 10.4 Tool description rules

The description teaches exactly three rules:

1. Capture only on explicit intent ("запомни идею", "добавь в туду",
   "не забыть"); never auto-capture asides.
2. Tag with the current project or an exact registry id; never invent a
   project.
3. Never mark an idea done without the user saying so.

`brain_record` learns `kind: idea` for **list/show only**; it does not gain
status mutation or trash/restore. `brain_idea` is the only writer.

## 11. Local API

Additive to `localapi-v1`; bearer token and localhost-only rules are unchanged.
Routes use snake_case JSON and the existing error envelope
(`{"error":{"code","message"}}`).

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/v1/ideas` | list with filters |
| `POST` | `/v1/ideas` | create |
| `GET` | `/v1/ideas/{id}` | read one, with body |
| `PATCH` | `/v1/ideas/{id}` | edit one |
| `DELETE` | `/v1/ideas/{id}` | move to trash (soft delete) |
| `POST` | `/v1/ideas/{id}/restore` | restore from trash |
| `GET` | `/v1/ideas/brief` | capped session brief |
| `GET` | `/v1/ideas/ical` | `.ics` file export |

The literal `brief` and `ical` paths are matched before `{id}`. An idea id is
globally unique; `project` is a resolver, not part of the identity.

### 11.1 `GET /v1/ideas`

Query: `project`, `status` (repeated or comma-separated), `tags` (repeated or
comma-separated, exact case-insensitive AND; WS4), `due_before`, `due_after`
(RFC3339), `q`, `limit` (default 100, max 1000).

```json
{
  "ideas": [<row without body>],
  "problems": [{"path": "projects/x/ideas/y.md", "kind": "idea", "reason": "git conflict markers"}],
  "total": 4,
  "project_source": "explicit"
}
```

An omitted `project` lists all registered projects and omits `project_source`;
an explicit one reports `project_source: "explicit"`. `total` counts matches
before `limit`. An unknown selector is `400 validation`; a missing project is
never created.

### 11.2 `POST /v1/ideas`

Request:

```json
{
  "title": "Тёмная тема",
  "body": "…",
  "project": "lv24-brain-desktop",
  "status": "open",
  "priority": "high",
  "due_at": "2026-11-27T18:00:00+03:00",
  "due_date": null,
  "remind_before_minutes": 120,
  "tags": ["ui"],
  "source": {"type": "desktop"}
}
```

All fields except `title` are optional. `due_at` and `due_date` are mutually
exclusive; send one or neither. The response is `201` with
`{"idea": <row with body>, "project_source": "..."}`. Validation errors are
`400`; an over-limit field is `413`; a protected project is `403`.

### 11.3 `GET /v1/ideas/{id}`

Returns `{"idea": <row with body>}`. Unknown id `404`; a problem file `409` with
the path and reason in the message (never a zero idea).

### 11.4 `PATCH /v1/ideas/{id}`

Request: any mutable field from `POST` plus `expected_updated_at?`,
`clear_due?`, `clear_reminder?`, `clear_tags?`, `project?`, `status?`.

- Omitted field = unchanged.
- Setting `due_at`/`due_date` replaces and clears the other.
- `clear_due: true` removes both; `clear_reminder: true` removes the lead;
  `clear_tags: true` empties the list.
- `status` follows §5; moving to `done` sets `completed_at` to now when absent;
  reopening clears `completed_at` and `completion_note`.
- `expected_updated_at` is the revision the client read. A stale value returns
  `409` with `error.current_revision` and writes nothing; omitting it keeps
  last-write-wins.
- Response `200` with `{"idea": <row with body>}`; `project_source` is added
  only when the request moved the idea (`project` present).

### 11.5 `DELETE /v1/ideas/{id}`

Moves the file to trash and returns `200`:

```json
{"id": "idea-...", "trashed": true}
```

It is reversible with `POST /v1/ideas/{id}/restore`, which returns
`{"idea": <row with body>}`. A missing live id is `404`; a restore that would
collide with a live file is `409`.

### 11.6 `GET /v1/ideas/brief`

Query: `project`, `scope` (`current|all`, default `all`), `max` (default 3,
1..10).

```json
{
  "text": "## Ideas — 4 open · 1 due today\n- [ ] today 18:00 — …",
  "ideas": [<row without body>],
  "open": 4,
  "due_today": 1,
  "muted": false
}
```

The response is exactly the block the session hook prints (§13), so CLI, MCP and
the desktop cannot drift. Body text never enters `text`.

### 11.7 `GET /v1/ideas/ical`

Returns `Content-Type: text/calendar; charset=utf-8` and a file export (not a
subscription). One `VEVENT` per dated idea, with `VALARM` from the lead time:

- `DTSTART;VALUE=DATE:YYYYMMDD` for all-day ideas, no `TZID`;
- `DTSTART;TZID=...:...` or a UTC `DTSTART` for timed ideas;
- `VALARM` with `TRIGGER:-PT<N>M` when `remind_before_minutes > 0`;
- `SUMMARY` = title, `UID` = idea id, `DESCRIPTION` = body (optional);
- `STATUS:COMPLETED` is not emitted; done ideas are omitted unless
  `include_done=1` is passed (default omitted).

No network listener, no tokenized subscription URL in v1.

### 11.8 Errors

Use the existing table: `400 validation`/`invalid_json`/`unsupported`,
`401 unauthorized`, `403 forbidden`, `404 not_found`, `409 conflict` with
`error.current_revision` for a stale `expected_updated_at`, `413
limit_exceeded`, `500 internal_error`. Server mode returns `400 unsupported`
with code `unsupported` for every ideas route.

## 12. Index and search

Index schema **11** (local, gitignored, derived; a mismatch drops and rebuilds).
`documents` gains:

| Column | Type | Value |
| --- | --- | --- |
| `due_at` | INTEGER NULL | Unix milliseconds of a timed due instant |
| `due_date` | TEXT NULL | `YYYY-MM-DD` for an all-day idea |
| `priority` | TEXT NOT NULL DEFAULT `'normal'` | `low\|normal\|high` |

`status` already exists and carries the idea status. `kind` is `idea`.
`updated_at` keeps its current meaning and is the revision; because ideas do
not write `content_updated_at`, the index projects `content_updated_at =
updated_at` for them.

- Reindex includes `projects/<id>/ideas/*.md`; unparsable files are recorded as
  problems and not indexed.
- `brain search --kind idea` searches idea title/body through FTS5.
- Ideas are excluded from `brain context` and knowledge retrieval, and are not
  ranked as decisions/notes.
- `brain doctor` reports the `idea` kind, the problem-file count and the index
  schema version.
- The index is a cache: deleting it and running `brain index` must reproduce
  the same rows from Markdown.

## 13. Session brief and reminders

### 13.1 Session block

`brain session start` appends, after the handoff and before decisions:

```
## Ideas — 4 open · 1 due today
- [ ] today 18:00 — Тёмная тема календаря (lv24-brain-desktop)
- [!] overdue 2d — Позвонить стоматологу (personal)
- [ ] high — Закрыть issue #1 (lv24-brain)
+1 more · brain idea list
```

- Best-effort and budgeted: at most 3 items, at most 700 characters, one
  indexed lookup, never a second bootstrap or a `Findings` pass.
- Only when the hook input's `source` is `startup` (not `compact`/`resume`).
- Scope is global but ordered for the current project: up to two items from the
  current project, then up to one global overdue/today item, then the global
  count.
- Only title, project and due are shown; body text never enters the block.
- A failure or timeout removes only the block, never the rest of the context.
- If the greeting is muted, the block is
  `ideas: 4 open (greeting muted)` and no items/questions are shown.
- `brain idea mute [--days N] [--off]` stores machine-local state in
  `<data-dir>/agent/ideas.json`:
  `{"greeting_muted_until":"2026-12-04T00:00:00Z"}`. The `agent/` directory is
  self-ignored, 0600 and never published.
- `brain open` adds one line
  (`ideas: 2 open in this project · 4 open overall, 1 due today`) and, when the
  budget allows, the project's top items (new context-pack layer, ~400 chars).
- The same renderer serves CLI, MCP `brief`, the API and the session hook.

### 13.2 Reminders (desktop)

- A Rust scheduler ticks every 30 s while the app or tray is alive, reads open
  ideas with a due, and fires at the effective due and, when set, at the lead.
- Dedupe key is `{idea_id, due_key, trigger}` where `due_key` is the RFC3339
  `due_at` or the `due_date` and `trigger` is `due` or `lead`. Changing the due
  resets the key; completing/dropping cancels it. The one watermark is local:
  `<data-dir>/agent/ideas-reminders.json`, never synced.
- Quiet hours default 22:00–08:00: no toast/sound; items join the next open
  digest.
- Missed triggers while the app was closed become one launch digest, never a
  burst of backdated toasts.
- Quit stops the scheduler; that is a documented v1 limitation.
- `brain idea ical` and the desktop export are file exports, not a subscription.

## 14. Problem rows and conflicts

A file under `ideas/` is a problem row when:

- it has git conflict markers (`<<<<<<<`, `=======`, `>>>>>>>` at line start);
- the frontmatter is missing or unterminated;
- YAML fails to decode, or a required field fails `Idea.Validate`;
- the id does not satisfy `IsRecordID("idea", id)`, or the file name stem and
  the frontmatter id disagree;
- the frontmatter `project_id` does not match the project directory it is
  stored in.

The store returns a problem file as `domain.ProblemError{Path, Kind, Reason}`,
a typed error that unwraps to `domain.ErrConflict` (the local API maps it to
`409`; the CLI exits `1`). `GetIdea`, `UpdateIdea` and `TrashIdea` return it
instead of a zero idea, and `UpdateIdea`/`TrashIdea` leave the file untouched.

Behaviour:

- The file is **never deleted, rewritten or silently dropped**. It stays in git
  for manual resolution.
- Other ideas in the same project and other projects stay usable.
- `brain idea list` prints a `PROBLEMS (n)` section with `path: reason`; `--json`
  includes `problems`. The command exits 0 even when problems are present, so a
  single conflicted file cannot break an agent's session. Problems are collected
  per registered project: a problem file in an orphan `projects/*/ideas`
  directory is not in the global list's `problems`, but `brain idea show <id>`
  still returns the typed `ProblemError` with its path and reason.
- `brain idea show <id>` for a problem file exits 1 with the path and reason.
- `brain doctor` reports a warning with the count and paths (not a hard
  failure); the desktop Ideas page shows a warning row with a "resolve in git"
  hint.
- `brain idea restore` never overwrites a live file; a collision is
  `ErrConflict`/`409`.
- A git merge never auto-resolves an idea file in v1. A field-level
  `lv24-ideas` merge driver is a documented follow-up.

## 15. Limits, redaction, privacy

- Title: 200 Unicode code points (`limits.max_title_chars` default 200).
- Body: `limits.max_body_bytes` default 1 MiB.
- Metadata (tags): `limits.max_metadata_bytes` default 16 KiB; count 10 and
  per-tag 40 code points in the domain.
- `remind_before_minutes`: 0..10080.
- No NUL and no invalid UTF-8 in title, body, tags, source, completion note.
- The existing redaction runs before every write; a token pasted into a title
  or body is redacted like any other record.
- Ideas live in the private data repo. Notification titles can be hidden;
  quiet hours apply; there is no telemetry and no cloud service.
- Local API stays localhost + bearer token; no new network listener.
- Project validation prevents typo-created projects.

## 16. Compatibility and non-goals

### 16.1 Compatibility

- Data schema stays `1`; an old engine (0.27.2 was verified) ignores the idea
  files: its record listings do not scan `ideas/`, `brain doctor` stays OK,
  nothing is indexed and nothing is corrupted. Update all machines before
  relying on idea sync.
- Index schema becomes `11`; it is local and rebuilt automatically. No user data
  is touched.
- Every addition is additive: new files, new kind, new optional port, new
  routes, new CLI action, one new MCP tool. Existing flags, routes, tool names
  and record kinds keep their meaning.
- Server mode is explicitly unsupported (`ErrUnsupported`) in v1; the domain
  model is ready for a later Postgres table.

### 16.2 Non-goals (v1)

- Recurring ideas/habits, kanban, sprints, assignees, comments, dependencies.
- Two-way Google/Apple/CalDAV sync or live calendar subscriptions.
- Mobile/Telegram/email capture; Telegram/ntfy push reminders.
- Embeddings or semantic duplicate detection.
- Auto-capture of conversational asides; auto-completion from commits (a
  commit-trailer close is `Later`).
- Server-mode parity.
- A custom merge driver for idea files.

## 17. Contract test checklist

A change to any surface must keep these true:

1. `go test ./internal/domain` covers all five statuses, three priorities, the
   due exclusivity, all-day date parsing, tag limits, completed-at rules,
   every transition pair and the id shape.
2. `go vet ./internal/domain` is clean.
3. A round trip writes the frontmatter of §2, reads it back, and preserves the
   `due_at` offset and the `due_date` string.
4. An unknown project is rejected with candidates; `personal` is used only when
   the cwd/default path applies.
5. A conflicted idea file appears in `problems` and the other ideas still list;
   `GetIdea`, `UpdateIdea` and `TrashIdea` return `domain.ProblemError` and do
   not modify it.
6. The same brief text is produced by `brain idea brief`, MCP `brief`, the API
   and the session hook.
7. Deleting an idea moves the file to trash, and restore brings back the same
   id and content; a live collision is `ErrConflict`.
8. `IdeaFilter.Tags` is an exact case-insensitive AND filter applied before
   `Limit`, and the same filter is exposed by MCP/API list inputs.
9. An idea of a project removed from the registry is still readable by its
   global id and appears in the global list; no registry entry is invented.
