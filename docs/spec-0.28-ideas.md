# 0.28 — Ideas: an agent-first TODO layer

Status: **draft for owner approval**
Date: 2026-11-26
Owner: LevapVeeskela
Targets: engine `0.28.0`, desktop `1.8.0`
Research: five parallel scouts (engine, data/sync, desktop, harnesses, prior art)

## 1. Intent

The owner wants one place for ideas and small TODOs that is:

- **captured through conversation** — "запомни идею…" is enough, no app switch;
- **tagged to a real project** — the LLM chooses from the existing registry, never
  invents a project; personal ideas go to the existing `personal` project;
- **shown at the start of every agent session** — a short list and one question:
  "хочешь заняться чем-то из этого?";
- **visibly completed** — done ideas are struck through, not deleted;
- **managed in lv24-brain-desktop** — view, edit, delete, filter, with as little
  UI ceremony as possible;
- **schedulable** — optional date/time, optional reminder a configurable number
  of hours before, and a notification when the time arrives;
- **local, git-backed, zero-cost** — no cloud TODO, no second source of truth,
  no telemetry, works on all three machines.

Why now: the brain already stores notes, decisions, attempts, handoffs and
project state, and every harness already receives the shared `AGENTS.md` and
`brain open`/`brain session start`. The missing layer is the small, mutable,
human-facing list that sits between "an idea" and "a project task". Today it
lives in chat history and in `state.active`, where it is neither complete nor
checkable.

## 2. What exists today (facts)

- Engine `0.27.2`; data schema `1`; index schema `10`.
- One file per decision/attempt/handoff under
  `projects/<id>/{decisions,attempts,handoffs}/`, ID
  `<kind>-<date>-<slug>-<ULID>`, create-only, atomic write, redacted, no
  per-write git commit (`brain sync` commits).
- Notes are sections inside a daily file, not individual files; they cannot
  carry a mutable checkbox cheaply.
- Index is local, gitignored SQLite FTS5; reindex is hard-coded per kind; `kind`
  is a free column, but `due_at`/`due_date`/`priority` filters need a schema
  bump.
- Local control API (used by desktop) is additive-only, frozen and golden-tested;
  records are read/PATCH/trash, **there is no create route today**.
- MCP exposes 17 tools, including `brain_record` (action-based) and
  `brain_open`/`brain_state`/`brain_search`.
- `brain session start` prints bootstrap + catchup + findings; `brain open` uses
  a layered budget. Both are the natural greeting surface.
- Desktop is Tauri 2.12 + React 19 + MobX; tray, close-to-tray, autostart and
  single-instance already work. There is **no notification plugin**, no
  scheduler, no calendar library, and the engine process dies when the app quits.
- Harness reality: `brain session start` has Claude, Codex and OpenCode hook
  protocols; OpenCode appends the text to the system prompt on every turn,
  Claude/Codex receive it as `additionalContext`. The Claude hook has a hard
  10 s deadline and already times out on 5/23 starts (the expensive part is
  `Findings`, not bootstrap). **Codex hooks are installed but have never run**
  and Codex has no brain MCP. Cursor, Gemini/Antigravity and Kimi have no brain
  session hook at all. The shared `AGENTS.md` (20k chars) is the only channel
  that reaches every harness; it is synced to four files by `brain agents sync`
  (nightly task, can lag a day).
- Prior art: Things (Inbox/Today/Someday, hidden detail fields), Todoist (quick
  add, configurable reminder lead), Obsidian Tasks (checkbox in context),
  Notion (`@remind`), Sunsama (weekly review). The recurring failure modes are
  notification fatigue, guilt mechanics, duplicate systems of record, and
  agents that auto-create noise.

## 3. Goals, non-goals, anti-goals

### Goals

1. Capture an idea from any harness in one tool call, with source provenance.
2. Resolve the project from the registry; reject unknown project ids.
3. Show a compact Ideas block at session start and ask once.
4. Complete/reopen/drop an idea from chat and desktop; render done as
   strikethrough.
5. Desktop: Focus/All/Calendar views; create/edit/delete/restore; filters;
   keyboard navigation; three locales.
6. Reminders while the app is running (including in the tray), with a default
   lead time and per-idea override; a missed-reminder digest on launch.
7. Calendar month view with due ideas; drag to reschedule; optional one-way
   `.ics` export.
8. Stay additive: old engines ignore the new kind; data schema remains `1`;
   index bumps locally and rebuilds.
9. Zero paid services, no new cloud dependency, no telemetry.

### Non-goals (v1)

These are the recommended v1 boundary; the owner can pull one in via §11.

- Two-way Google/Apple/CalDAV sync, calendar event creation, meeting scheduling.
- Recurring ideas/habits.
- Kanban, sprints, assignees, comments, dependencies.
- Mobile app or Telegram/email capture (later); a Telegram/ntfy reminder push is
  the one optional v1 extension in open decision #2.
- Server-mode parity (Postgres table); git/local modes first.
- Embeddings or semantic duplicate detection.
- Auto-capturing every conversational aside.
- Auto-completion without explicit user confirmation (except an opt-in commit
  trailer, later).

### Anti-goals

- A second backlog that creates guilt. No streaks, karma, red "47 waiting".
- Notification spam. One notification per trigger, quiet hours, no repeats.
- A second source of truth. Ideas are not mirrored to Todoist/Jira.
- Context bloat. The session block is capped (≤ 3 items, ≤ ~700 chars).
- Silent auto-tagging. A wrong project is visible and fixable in one click.

## 4. Approaches considered

| Approach | What it gives | Cost | Verdict |
|---|---|---|---|
| **A. Engine-native `idea` record** | Git sync, CLI/MCP/desktop/API, one source of truth, provenance, search | L | **Recommended** |
| B. Reuse `note` with `kind: idea` frontmatter | Fastest write path | M | Rejected: notes are daily sections; mutable status/due and per-idea files fight the format |
| C. Desktop-local `ideas.json` | Fast UI, no engine work | S | Rejected: agents cannot capture/see, no git sync, violates the core request |
| D. External SaaS (Todoist/Things API) | Mature reminders/calendar | M | Rejected: cloud, tokens, paid tiers, duplicate source of truth |

Recommended: **A**, modeled on `Decision` (status enum + in-place update), one
file per idea, under `projects/<project_id>/ideas/`. The project is metadata,
not part of the file name. Untagged ideas use the existing `personal` project.

Merge risk: status/due edits are in-place, like `decision` status edits today.
With one human across three machines, conflicts are rare and visible; v1 ships
without a custom merge driver. If conflicts appear, add a field-level
`lv24-ideas` driver (documented as a follow-up, not a blocker).

## 5. Design

### 5.1 Record model

File: `projects/<project_id>/ideas/<idea-id>.md`, e.g.
`projects/lv24-brain-desktop/ideas/idea-2026-11-26-calendar-dark-theme-01jcs....md`
(the desktop checkout is registered in WP0; until then the engine would reject
that project id and fall back to `personal`).

```yaml
---
schema_version: 1
kind: idea
id: idea-2026-11-26-calendar-dark-theme-01jcs...
project_id: lv24-brain-desktop
title: "Тёмная тема для календаря"
status: open              # open | doing | done | dropped | someday
priority: normal          # low | normal | high
due_at: 2026-11-27T18:00:00+03:00   # timed; exactly one of due_at/due_date
# due_date: 2026-11-27              # all-day; local calendar date
remind_before_minutes: 120
tags: [ui, calendar]
source:
  type: session           # session | note | decision | commit | manual | desktop
  id: ses_...
  harness: opencode
completed_at: null
completion_note: ""
created_at: 2026-11-26T10:00:00Z
updated_at: 2026-11-26T10:00:00Z
---
Сделать тёмную тему в календаре. Проверить контраст и клавиатуру.
```

- `title` ≤ 200 chars, body ≤ existing 1 MiB limit, tags ≤ 10.
- **Time semantics:** a timed idea has `due_at` (RFC3339 with offset); an
  all-day idea has `due_date` (`YYYY-MM-DD`). They are mutually exclusive.
  `due_date` is a local calendar date and does not shift when travelling;
  `due_at` keeps its offset and is rendered in the machine's local time.
  "Today"/"overdue" grouping uses the current local date.
- `remind_before_minutes` is a concrete optional integer. Absent/`0` means no
  lead: notify at the due time only. The desktop's "default lead" is applied
  **when the idea is created or edited** and written into the record; changing
  the setting does not retroactively rewrite existing ideas. The agent may omit
  it and the idea gets no lead. For all-day ideas the anchor is **09:00 local**
  and the lead applies to that anchor.
- `status: done` + `completed_at` is the strikethrough state; `dropped` keeps a
  reason in `completion_note`. Nothing is hard-deleted by default; delete moves
  to the existing trash and can be restored.
- `source` is a small **idea-specific** value object (`type`, `id`, `harness`),
  not the strict `domain.SourceRef` used by proposals/items: it is optional,
  `id` may be empty for `manual`/`desktop`, and the source project may differ
  from the idea's project (capture in project A, tag to project B). It renders
  as a link when resolvable, otherwise as raw text.
- A file with git conflict markers fails to parse. The engine must not drop it
  silently: v1 lists it as a **problem row** (path + reason) in
  `brain idea list`, `brain doctor` and the desktop Ideas page, while other
  ideas stay usable. The conflicted file with both edits stays in git for manual
  resolution. No custom merge driver in v1.
- Every kind embeds provenance/validity/taxonomy/relations; ideas follow the
  same validator and redaction path.

### 5.2 Project resolution and tagging

- The engine resolves `--project` exactly like writes today: id, alias or
  path; unknown ids are an error with candidate suggestions.
- Default when omitted: the project of the current working directory; if the
  cwd is not registered, `personal`.
- The **LLM does the semantic tagging**: it receives the current project and,
  when the idea clearly names another project, looks up the exact id/alias in
  `brain project list --json`. It never invents ids; if ambiguous, it asks one
  short question or leaves `personal`.
- CLI/API return `project_source: cwd|explicit|default` so the agent can correct
  a wrong guess in one follow-up.
- The desktop project selector is a searchable select over the registry; moving
  an idea rewrites `project_id` and renames the file. If a project later
  disappears from the registry, keep the idea and show the raw id; never delete
  user data because a registry entry changed.
- **Prerequisite:** `C:\0project\lv24-brain-desktop` is a local checkout but is
  **not** in `registry.json` today (the example above would be rejected until
  it is). Before the demo, register missing local checkouts with
  `brain discover --root C:\0project` (preview) then `--apply`, or
  `brain project set lv24-brain-desktop --add-path …`. This is an owner
  decision; the engine never invents a project.

### 5.3 CLI

```
brain idea add "…" [--project ID|auto] [--due 2026-11-27T18:00+03:00|today|tomorrow|YYYY-MM-DD]
                  [--at 18:00] [--remind 2h|1440] [--priority high] [--tag ui]
                  [--source session:<id>] [--json]
brain idea list  [--open|--done|--all|--status …] [--project ID] [--due today|week|overdue]
                  [--tag …] [--search …] [--json]
brain idea show  <id> [--json]
brain idea edit  <id> [--title …] [--body-file -] [--project …] [--due …] [--remind …]
                  [--status open|doing|done|dropped|someday] [--priority …] [--tags …]
brain idea done  <id> [--note "…"]
brain idea reopen <id>
brain idea drop  <id> [--reason "…"]
brain idea rm    <id>        # trash, restorable
brain idea restore <id>
brain idea brief [--project ID] [--max 3] [--scope current|all] [--json]
brain idea mute  [--days 7] [--off]
brain idea ical  [--project ID] > ideas.ics
```

`--due today|tomorrow|YYYY-MM-DD` creates an all-day idea; add `--at HH:MM`
or pass an RFC3339 timestamp for a timed one. `--remind` accepts `2h`, `1d` or
minutes and is written as a concrete value; it is not a hidden "use default".

Human output uses a checkbox and strikethrough:

```
OPEN (4)
  [ ] today 18:00  Тёмная тема календаря   lv24-brain-desktop  🔔2h
  [!] overdue 2d   Позвонить стоматологу   personal
DONE (2)
  [x] ~~Починить футер~~                    lv24-brain
```

`--due today|tomorrow` and `--remind 2h|1d` are small convenience parsers, not
a general natural-language engine; the agent passes ISO when it computes dates.

### 5.4 MCP

One action-based tool, matching `brain_record` instead of adding five tools:

`brain_idea` with `action`:
`add | list | get | update | done | reopen | drop | rm | restore | brief`.

- `add`: title, project?, due_at/due_date?, remind_before_minutes?, priority?,
  tags?, body?, source? → returns the idea and `project_source`.
- `list`/`brief`: filters, limit; `brief` is the session form.
- `update`: id + mutable fields (including status).
- `drop` changes status to `dropped`; `rm` moves to trash, `restore` undoes it.
- Tool description teaches one rule: capture only on explicit intent; never
  mark done without the user saying so.

Total MCP surface: 18 tools. `brain_record` learns `kind: idea` for list/show
only; trash/restore is handled by `brain_idea` (`rm`/`restore`), so the
read-only `brain_record` contract does not change.

### 5.5 Session greeting (all harnesses)

The greeting must work even when a hook is missing, slow, or never runs. Three
layers, each cheap:

1. **`brain session start`** (Claude, OpenCode, and Codex where the hook runs)
   appends a global `## Ideas` block after the handoff, before decisions:
   ```
   ## Ideas — 4 open · 1 due today
   - [ ] today 18:00 — Тёмная тема календаря (lv24-brain-desktop)
   - [!] overdue 2d — Позвонить стоматологу (personal)
   - [ ] high — Закрыть issue #1 (lv24-brain)
   +1 more · brain idea list
   ```
   It is **best-effort and budgeted** (≤ 700 chars, ≤ 3 items): a failure or
   timeout never removes the rest of the context. The current hook already
   times out on some Claude starts; the ideas query must be one indexed lookup,
   not a second full bootstrap. The block is only emitted when the hook input's
   `source` is `startup` (not `compact`/`resume`), so a compaction does not
   repeat the question. At a workspace root, it shows the global list.
   **Scope:** the block is global but ordered for the current project — up to
   two items from the current project, then up to one global overdue/today
   item, then the global count. `brain idea brief --scope current|all`
   (default `all`) and `--project` refine it. The rail badge and the Ideas page
   are global; `brain open` is project-scoped. Only titles, project and due are
   shown; body text never enters the session block.
2. **`brain open`** stays a project-context command: it adds one line
   (`ideas: 2 open in this project · 4 open overall, 1 due today`) and the
   project's top items only when the budget allows (new context-pack layer,
   ~400 chars, below state/handoff). This is the safety net for agents that
   always run `brain open` before work, without repeating a global list on
   every task.
3. **Shared `AGENTS.md`** gets a compact protocol (≈70 words) that reaches
   Claude (via `CLAUDE.md` import), OpenCode, Codex, Cursor, Gemini, Kimi and
   any future harness: if the session context already contains `# Company
   Brain:`/`## Ideas`, do not re-run `brain open`; otherwise run
   `brain idea brief` once at the start. Show at most three items, ask once
   whether to start one, and do not repeat after "no" or after a compaction.
   Capture only on explicit request; tag with the current project or a registry
   id; mark done only on confirmation. `brain agents sync` distributes it; run
   it immediately after editing the canonical
   `lv24-brain-data/global/AGENTS.md` (the nightly task can lag a day).

Codex has no brain MCP and its hooks have never fired on this machine, so the
CLI path (`brain idea …`) is mandatory, not optional. Cursor/Gemini/Kimi only
get the `AGENTS.md` rule. OpenCode re-appends session context on every turn, so
the block is capped; the full list is one command away.

The block is generated by one function used by CLI, MCP and session, so the
wording and ordering are identical. If the user mutes the greeting
(`brain idea mute --days 7`, machine-local), the block is replaced by
`ideas: 4 open (greeting muted)` and the agent stays quiet.

### 5.6 Desktop UX

A new rail entry **Ideas** with an open-count badge (no red guilt styling).
Route `/ideas`; `?focus=<id>` opens the drawer. The page defaults to **All
projects**; the rail selector can narrow it, but the badge stays global.

**Focus (default view)**

```
Ideas                                          4 open
[Focus] [All] [Calendar]        [All projects ▾]  [Search]
+ Capture an idea…                                        (Enter)
──────────────────────────────────────────────────────────
OVERDUE
 ○ Позвонить стоматологу             personal     ! 2d
TODAY
 ○ Тёмная тема календаря             desktop      18:00  🔔
THIS WEEK
 ○ Закрыть issue #1                  lv24         Fri
LATER (2)
 ○ …
✓ DONE TODAY (2)                                          (collapsed)
  ~~Починить футер~~                 lv24
```

- Checkbox completes with a short strike/fade; an undo toast appears; the row
  moves to Done. Clicking a done row reopens it.
- Rows are dense, one line; project, due and reminder are small chips. No cards,
  no progress bars, no streaks.
- `n` capture, `/` search, `j/k` move, `x` toggle done, `e` edit, `Delete`
  trash, `Esc` close.
- Quick add is title-only; project defaults to the rail selection or
  `personal`; date and reminder are one click away. The agent handles natural
  language in chat; the desktop does not need an NL parser.
- Detail drawer: title, status, project, due date/time, reminder lead, priority,
  tags, source link, body, created/updated, Delete. Save on blur/Enter with a
  dirty guard; no modal-heavy form.
- Calendar: zero-dependency month grid built with `Intl` + CSS tokens; chips on
  due days; click a day to create; drag a chip to reschedule; agenda below for
  timed items. Keyboard and forced-colors support.
- Dashboard gets a compact "Today" card (max 3) linking to Ideas; the tray menu
  gets "New idea" and "Today · N".
- A conflicted/unparsable idea file appears as a problem row with its path and
  a "resolve in git" hint; the rest of the list stays usable.
- Empty state: "Ideas captured in chat appear here." One line, no illustration
  circus.

### 5.7 Reminders and notifications

- While the desktop runs (window or tray), a **Rust scheduler** ticks every
  30 s, reads open ideas with `due_at` or `due_date`, and fires at the due time
  (09:00 local for all-day) and, if set, at the lead before it.
- Use `tauri-plugin-notification` (one new dependency). Windows toasts require
  the installed app/AUMID; macOS requires a signed bundle (signing is pending),
  so an in-app banner is always the fallback. If permission is denied, show a
  one-time Settings notice and keep the in-app banner. Test button in Settings.
- Clicking a notification opens the window at `/ideas?focus=<id>` when the
  plugin exposes a click event; otherwise it opens the Ideas page.
- Dedupe watermark is local (`agent/ideas-reminders.json` or app data):
  `{idea_id, due_at|due_date, trigger}` → `notified_at`. Changing the due time
  resets the key; completing/dropping cancels.
- Quiet hours (default 22:00–08:00): no toast/sound; items appear in the next
  open digest. Optional "hide titles in notifications" for lock-screen privacy.
- If the app was closed at trigger time: on launch, one banner "Пока тебя не
  было: 2 идеи просрочены" and the list; never a burst of backdated toasts.
- Quit stops the scheduler (engine dies with desktop). This is a documented v1
  limitation; an OS Task Scheduler/launchd helper or Telegram/ntfy push is a
  later extension.
- Settings: enable reminders, default lead (0/10m/1h/3h/1d), quiet hours, sound,
  hide titles, autostart, test notification. The default lead is applied to
  **new or edited** ideas only; each record stores the concrete value.
- Calendar export: `brain idea ical > ideas.ics` and a desktop "Export .ics"
  button emit VEVENTs with `VALARM` from the lead time (`DATE` for all-day,
  `DATE-TIME` for timed). This is a **file export**, not a live subscription; a
  tokenized localhost subscription URL is a later extension. No two-way sync.

### 5.8 Index, search, API

- Index: add `due_at`, `due_date`, `priority` columns and bump schema to `11`
  (local drop and rebuild). `kind: idea` flows into search; ideas are excluded
  from `brain context`/knowledge retrieval and included in
  `brain search --kind idea`.
- Local API (additive `localapi-v1`): `GET/POST /v1/ideas`,
  `GET/PATCH/DELETE /v1/ideas/{id}`, `POST /v1/ideas/{id}/restore`,
  `GET /v1/ideas/brief`, `GET /v1/ideas/ical` (desktop file export only, not a
  public subscription). Regenerate OpenAPI + golden operations + desktop client
  (`npm run api:generate`).
- `brain digest` gets an ideas line (`N open · M due today · K done in 24h`).
  The weekly review/aging surface stays in Later.
- Server mode is explicitly unsupported in v1 (`ErrUnsupported`), with the
  domain model ready for a later Postgres table.

### 5.9 Security and privacy

- Ideas are private data in the private data repo; the existing redaction runs
  before every write. Never log tokens or bodies.
- Notification titles can be hidden; quiet hours; no telemetry.
- Local API stays localhost + bearer token; no new network listener.
- Project validation prevents typo-created projects.

## 6. Agent protocol (the contract we teach)

**Capture** — user says "запомни идею / добавь в туду / не забыть / идея:".
1. Choose project: current cwd project, or exact registry id/alias if the idea
   names another project; otherwise `personal`.
2. Call `brain_idea add` once; if the title is obviously close to an open idea,
   mention it instead of silently duplicating.
3. Reply with one line: title, project, due/reminder if any.

**Greeting** — session start.
1. Read the Ideas block from session context; if absent, `brain idea brief`.
2. Show at most three items (overdue/today first), then one question.
3. "Not now" ends it for the session; never re-ask in the same session.

**Complete** — user says "сделал / готово / вычеркни".
1. Find the matching open idea by id or title; if ambiguous, ask which.
2. `brain_idea done <id> [--note …]`; reply with `~~title~~` and the note.
3. Never infer completion from a commit or a code change alone.

**Edit/drop/delete** — only on explicit request. `drop` changes status;
`rm` moves to trash and `restore` undoes it; nothing is purged from chat.

## 7. Work packages

| # | Package | Effort | Depends |
|---|---|---|---|
| WP0 | Register missing local checkouts (`brain discover --root C:\0project`, owner approves `--apply`) | S | — |
| WP1 | Contract: domain `Idea`, status enum, schema, fixtures, docs stub | S | WP0 |
| WP2 | fsstore append/list/get/update/trash + problem rows (unparsable/conflicted) + optional ports (no `Store` break) | M | WP1 |
| WP3 | Index columns + reindex + search filters (schema 11) + problem surfacing | S/M | WP2 |
| WP4 | CLI `brain idea …` (add/list/edit/done/reopen/drop/rm/restore/brief/mute/ical), limits/redaction/kind allowlists, tests | M | WP2, WP3 |
| WP5 | MCP `brain_idea` (incl. `rm`/`restore`) + `brain_record` kind + capabilities/contract tests | S/M | WP4 |
| WP6 | Session block (best-effort, ≤700 chars, `source=startup` only) + `brain open` pointer + context-pack layer + digest line + mute state (`agent/ideas.json`) | S/M | WP2, WP3 |
| WP7 | Shared `AGENTS.md` protocol + `brain agents sync` check + harness matrix test (Claude/OpenCode/Codex/Cursor/Gemini/Kimi) | S | WP6 |
| WP8 | Local API ideas routes + OpenAPI/golden + desktop client regen | M | WP4 |
| WP9 | Desktop Ideas store + Focus/All list + filters + quick add + drawer + problem/conflict rows | M | WP8 |
| WP10 | Done/strikethrough, trash/undo, keyboard, i18n en/ru/zh, tests | M | WP9 |
| WP11 | Rust reminder scheduler + notification plugin + tray/settings/missed digest | L | WP8 |
| WP12 | Calendar month view + drag reschedule + agenda + accessibility | M | WP9 |
| WP13 | `.ics` file export + calendar-app import docs | S | WP11 |
| WP14 | Docs: architecture, schema-compat, limits, quickstart, MCP, localapi, release notes, VitePress en/ru/zh | M | all |
| WP15 | Release: engine 0.28.0 + desktop 1.8.0, smoke on Windows/macOS, deployed docs | S/M | all |

Rough total: ~5–8 engine days + ~10–15 desktop days; with the swarm split
across WP2–WP13, one calendar week is realistic, with WP14–WP15 after review.

Suggested order: WP1–WP6 (engine + greeting) → WP8–WP10 (desktop core) →
WP11–WP12 (time) → WP7/WP13–WP15 (polish/release). Ship engine first; desktop
1.8.0 requires engine ≥ 0.28.0.

Version note: this displaces the planned 0.28 entity layer to 0.29. Confirm
that reorder before implementation.

## 8. Acceptance criteria

1. From OpenCode, Claude Code and Codex, "запомни идею X" creates one file,
   resolves the project (or `personal`), and returns the id in one tool call.
2. An unknown `--project` is rejected with candidates; no project is invented.
3. `brain session start` shows the Ideas block; `brain open` shows the pointer
   and project items within budget; the shared `AGENTS.md` makes the agent ask
   once in every harness, including those without hooks, and not repeat after a
   compaction. `brain idea mute --days 7` suppresses the block for that period.
4. `brain idea done` + desktop checkbox render `~~title~~`; `reopen` restores.
5. Desktop: create/edit/delete/restore, filter by project/status/due, search,
   keyboard, empty/loading/error states, en/ru/zh, no layout break at 760×540
   and 200% zoom.
6. Reminders fire at due and at the configured lead while the app runs; no
   duplicate toasts; quiet hours suppress; missed items appear as one digest.
7. Calendar month view shows dated ideas; drag reschedules; no new calendar
   dependency; keyboard/forced-colors pass.
8. All-day and timed ideas round-trip: `--due today` creates an all-day item,
   `--at`/RFC3339 creates a timed one; all-day reminders anchor at 09:00 local;
   travel does not shift `due_date`.
9. Two machines: create on A, sync, see/edit on B; no lost records; a
   concurrent edit produces a visible problem row/banner and the conflicted
   file is preserved, while other ideas stay usable.
10. Old engine ignores ideas without corruption; `brain doctor` reports the new
    kind; data schema stays `1`; index rebuilds to 11.
11. `go test ./...`, `go vet`, desktop `npm run check`, `cargo fmt/clippy/test`,
    `npm run api:check`, docs build in three languages, all green.
12. No paid service, no new cloud dependency, no telemetry; only the
    notification plugin is added.

## 9. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Claude session hook already hits its 10 s deadline on 5/23 starts | Ideas are one indexed lookup, best-effort and capped at ≤700 chars; never depend on `Findings`; measure p95 and keep the existing 10 s guard. If the hook times out, the `AGENTS.md` path still runs `brain idea brief`. |
| Codex hooks have never fired; Codex has no brain MCP | CLI is the primary path, taught in `AGENTS.md`; add a Codex smoke check to WP7. |
| Cursor/Gemini/Kimi have no session hook | The shared `AGENTS.md` rule is the only channel; test that each reads it. |
| OpenCode re-appends session context every turn | Cap the block at 3 items; the full list is a command, not context. |
| In-place status/due edits can conflict across machines | One file per idea, `updated_at`, visible conflict (never silent overwrite); a `lv24-ideas` merge driver is a documented follow-up. |
| Reminders do not fire when the app is quit | Documented v1 limitation (tray only); missed digest on launch; Telegram/ntfy or an OS scheduler is a later extension. |
| Windows toast needs an installed AUMID; macOS needs signing | Test on installed builds; in-app banner is always the fallback; test button in Settings. |
| Notification fatigue | One notification per trigger, quiet hours, no repeats, mute command, no streaks/guilt. |
| Index schema bump to 11 | Local, derived, rebuilt automatically; no user data loss; `doctor` reports. |
| Old engine sees the new kind | Verified on 0.27.2: idea files are ignored (doctor OK, 0 indexed, no corruption); additive, data schema stays `1`; update all three machines before relying on sync. |
| Wrong project tag | Default cwd project, visible chip, one-click move, registry validation, agent asks only when ambiguous. |
| Scope creep (two-way calendar, recurrence, Telegram) | Explicit non-goals; each listed separately under Later and needs its own approval. |

## 10. Later (designed for, not built now)

Ranked by value/effort; each needs its own approval.

1. **Commit-trailer auto-close** — `Idea: <id>` in a commit footer closes the
   idea with the commit as evidence (deterministic, opt-in).
2. **Telegram capture + push** — bot with inline `done/snooze`, long polling,
   no public server; free phone capture and reminders when the PC is off.
3. **Global hotkey capture** — `Ctrl+Alt+I` from any app; small always-on-top
   window; no context switch.
4. **Weekly review** — stale/no-date ideas, one suggestion, done-this-week;
   reuse `brain digest`.
5. **Duplicate hint at capture** — BM25/trigram top-3, "similar idea?".
6. **"What can I do in 20 minutes"** — explicit `effort: S/M/L`, filter by
   time/context; never let the LLM estimate effort.
7. **Idea ↔ note/decision/commit links** — reuse `brain links`/relations;
   source panel in the drawer.
8. **Promote to project task** — create a GitHub/GitLab issue or a project
   active item with a backlink; never mirror both ways.
9. **ICS subscription URL** — tokenized localhost/file-backed subscription for
   phone/desktop visibility (v1 only exports a file).
10. **Random resurfacing** — max one old idea per week, matched to the current
    project; icebox, not nagging.
11. **Snooze** — "not now" for 7/30/90 days (greeting mute is already v1);
    machine-local.
12. **Focus mode** — open the project and show one idea + optional 25-min timer.

## 11. Open decisions for the owner

1. **Calendar depth**: internal month + optional `.ics` (recommended) vs real
   Google/CalDAV two-way sync (much larger, auth friction). 
2. **Reminder reach**: desktop tray only (recommended v1; Quit means a missed
   digest) vs an OS-level scheduler/helper that fires when the app is closed
   (larger, per-OS) vs phone push via Telegram/ntfy in v1.
3. **Greeting size**: max 3 items + one question (recommended) vs the full open
   list every session.
4. **Untagged ideas**: use the existing `personal` project (recommended) vs a
   separate `inbox` project.
5. **Version**: ship as engine 0.28.0 + desktop 1.8.0 and move the entity layer
   to 0.29 (recommended), or keep 0.28 for entities and ship ideas as 0.29.
6. **Done retention**: keep done items in a collapsed Done section indefinitely
   (recommended; searchable, no guilt) vs auto-archive after 90 days.
