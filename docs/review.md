# Review v2: findings, review dates and deterministic duplicates

`brain review` is the deterministic queue of knowledge that may be wrong, stale
or duplicated. Nothing in this document runs a model: every finding, review date
and duplicate cluster is computed from record text, frontmatter, git and the
clock.

- **Findings** explain themselves: every one carries a severity, a why, a safe
  next action and the fact to confirm before acting.
- **Review dates** (`review_after`) turn "looks current" into a dated promise.
  They are prompts to re-check, never expiries: an overdue record stays current
  until a person confirms, reschedules or supersedes it.
- **Deterministic dedup** refuses an exact copy at write time and reports near
  copies; it never merges, trashes or rewrites a record on its own.

Everything here is additive. Existing findings, JSON keys, records and commands
keep their meaning; new fields and flags sit next to them.

## Finding schema

| Field | Meaning |
|---|---|
| `kind` | The heuristic, for example `next-behind-handoff` or `review-overdue`. Stable. |
| `severity` | `error`, `warn` or `info` (see below). |
| `message` | What was observed, with the ids a caller needs. |
| `why` | One sentence about the risk of leaving the finding alone. |
| `next` | The safe next action for a person or agent. |
| `confirm` | What to verify before acting. |
| `evidence` | The record id, commit hash or quoted text that supports it. |
| `fixable` / `fix` | Whether `brain review --fix` applies a repair, and which one. |

Severity is the only ordering a caller should branch on:

| Severity | Meaning | Examples |
|---|---|---|
| `error` | Context may be **wrong**. | `quarantined`, `decision-contradicted`, `review-overdue` (oldest overdue by 30+ days) |
| `warn` | Context may be **stale**. | `state-expired`, `next-behind-handoff`, `decision-code-changed`, `near-duplicate`, `suspected-contradiction` |
| `info` | Curation hint, nothing is broken. | `next-maybe-done`, `question-answered`, `broken-link`, `supersession-gap` |

Finding kinds: `quarantined`, `state-quarantined`, `state-expired`,
`state-behind-commits`, `next-behind-handoff`, `next-maybe-done`,
`question-answered`, `decision-contradicted`, `suspected-contradiction`,
`decision-code-changed`, `broken-link`, `near-duplicate`, `supersession-gap`,
`stale-superseded`, `proposal-stale`, `proposal-orphan`,
`proposal-contradicted`, `review-overdue`, `pending-relations`,
`merge-conflict`, `registry-conflict`.

`merge-conflict` and `registry-conflict` (both `warn`) are read straight
from the conflict artifacts the merge drivers commit: `projects/<id>/state-conflicts/`,
`projects/<id>/notes/merge-conflicts/`, `merges/*.json` for the project
(`merge-conflict`), and `registry-conflicts/` files naming the project
(`registry-conflict`). `brain doctor` counts the same files in its `conflicts`
check and warns about one repository URL registered under several ids in
`duplicate-repo`.

`quarantined` is always reported first, so a short context or session summary
never truncates the queue that says external text awaits review.

Text output of `brain review`:

```
## Findings
- [warn] next-behind-handoff: a later handoff names a different next action, so the state points at older work -> run `brain review --fix` to adopt the handoff's next action.
  found: a later handoff names a different next action: "new step" (evidence: handoff-...)
  confirm: the handoff's next action is the work that should continue.
  fix (brain review --fix): set next_action to "new step"
```

The same fields travel in `brain review --json` and through the MCP tool
`brain_review`, so an agent can branch on `severity` instead of matching text.

## Suspected contradictions

`suspected-contradiction` is a report-only `warn`: two current records of one
project read as if they disagree about the same subject. It never edits,
supersedes or merges anything, and `brain review --fix` does not apply it.

The check compares the project's current decisions and notes, newest 200. It
skips stubs, rejected decisions, records whose validity ended, and any pair
already linked by `supersedes`/`superseded_by`: a declared replacement is a
decision, not a suspicion. Records of different projects never pair.

A pair is compared only when it repeats at least **40 %** of the shorter
record's stemmed words and at least two words. That overlap gate is what keeps a
shared "not" or "deprecated" in unrelated text from becoming a finding.

| Reason | Fires when | Extra gate |
|---|---|---|
| `temporal` | the newer record marks the older claim as overridden and the older does not: `no longer`, `instead`, `replace*`, `deprecated`, `obsolete`, `sunset`, `phased out`, `больше не`, `вместо`, `замен*`, `устарел`, `неактуал*` | 60 % overlap |
| `negation` | exactly one side negates (`not`, `no`, `never`, `cannot`, `не`, `нет`, `без`, contractions); both negating is agreement | 50 % overlap |
| `antonym` | the records use opposite sides of a closed pair: enable/disable, allow/deny, increase/decrease, keep/remove, before/after | base gate; same negation polarity |
| `numeric` | the same recognized unit carries different values, for example `90 days` against `180 days` or `14d` against `30d` | base gate |

The heuristics run in that order, so one pair yields at most one finding with
the most specific reason. The message carries the reason and both ids and
titles, the evidence is `<older-id> <-> <newer-id>`, and the next action is to
open both records and accept or reject one.

False negatives are expected: the check needs shared vocabulary, a numeric
change needs a recognized unit, and only the newest 200 records are compared.
Treat the finding as a prompt to read the pair, not as proof. The acceptance
fixture is eight true pairs (negation, antonym, numeric and temporal, including
Russian wording) and eight near-miss pairs that stay unflagged.

## Supersede cases

`brain record supersede <old-id> --by <new-id>` opens a **proposed** case in
local and git modes. The successor's `supersedes` link is written at once, but
the predecessor is left untouched: it stays current, listed and searchable until
a review accepts the case. Review it with
`brain record supersede-review <old-id> --accept|--reject|--rollback`
(`--reason` is required for reject, `--expected-hash` refuses a stale rollback).

```text
## Supersede cases (1 proposed)
- [proposed] decision-...-one → decision-...-two: the second shard is read-only
```

A case carries both ends, the reason, its status (`proposed`, `accepted`,
`rejected`, `rolled_back`), a revision that orders the case timeline, the
proposal and decision times, the predecessor's previous validity tuple, the
tuple an approval wrote, and the state hash a rollback checks.

| Action | Allowed when | Effect |
|---|---|---|
| accept | `proposed` | ends the predecessor: writes `valid_to` and `superseded_by`, marks a decision `superseded`, records the previous and applied tuples and the state hash |
| reject | `proposed` | writes nothing to the predecessor; stores the refusal reason on the case |
| rollback | `accepted` | restores the exact previous `valid_from`, `valid_to`, `superseded_by` tuple and, for a decision, its previous status; clears the successor's `supersedes` claim when it names this predecessor |

A rejection requires a written reason. An acceptance or rollback note is
optional and defaults to `approved` or `rolled back`. Every case reason and note
is one line of at most 240 characters; a newline or an empty rejection reason is
a validation error. A rejected or rolled-back case can be proposed again with a
new revision. A second successor cannot claim the same predecessor while a case
is still proposed, and a record whose validity already ended cannot be
superseded.

Rollback is guarded by `SupersedeStateHash`: identity, kind, status, the
`valid_from`, `valid_to`, `superseded_by` and `supersedes` fields, `updated_at`
and the normalized text. The approval stores the hash; a rollback compares it
(or the caller's `expected_hash`) with the record as it is now. A stale hash (a
mismatch) is a revision conflict (`domain.ErrConflict`, rendered as
`409 conflict` by HTTP surfaces) and nothing is written. A rollback also refuses
when the record is no longer superseded by that successor, or when the case is
not `accepted`.

Accept and reject are also reachable through the successor decision:
`brain record status <successor> accepted|rejected`, or MCP
`brain_judge(action=accept|reject, id=<successor>)`, settles every proposed case
that names it. That path records the fixed note `successor decision accepted` or
`successor decision rejected`. The case itself opens through
`brain record supersede` or MCP
`brain_judge(action=supersede, id=<old-id>, to=<new-id>)`. The direct
accept/reject/rollback review action is the engine's `ReviewSupersede`; the CLI
has no separate `--review` flag in 0.27.1. Stores without an activity journal,
including server mode, have no case queue and apply the link immediately.

### Invalidated records and the archive

An explicit invalidation ends a record without naming a successor. It writes
only `valid_to` and records an `invalidate` event with the reason and the end
instant; the text, path and id stay. Default search, context and listings hide
the record. The archive selector keeps it: the list API names the filter
`include_invalidated`; the CLI spells it `--include-superseded` or `--all-time`.
A separate `--archive` flag is not wired in 0.27.1, and MCP search and context
have no archive switch.

The engine's archive read returns `kind`, `id`, `invalidated_at`, `reason` and
`superseded_by` (empty for an explicit invalidation). A rejected case never
reaches the archive, and a rollback removes its entry. `brain review --due`
skips invalidated records like superseded ones; the archive is history, not a
review queue.

### Case limits

- The case is in the activity journal, not in the Markdown record. A machine
  without the proposing user's journal line sees the successor's `supersedes`
  claim but no case. Until a sync brings the line, only the proposing machine
  can review the case. Journal files sync with the data repository and
  union-merge, so another machine can review it after that sync; treat the
  proposing machine as the reviewer of record.
- `--supersedes` on a new note or decision is still immediate. It ends the old
  record in the same write, opens no case and cannot be rejected or rolled back
  through review.
- Rejection does not clear the successor's `supersedes` frontmatter. There is no
  CLI flag to remove that claim in 0.27.1; write and accept a new case, or repair
  the records outside the CLI.

## Review dates

Each curated record can carry two optional frontmatter keys:

```yaml
---
review_after: 2027-02-05T00:00:00Z   # when to re-check the claim
reviewed_at: 2026-11-15T09:00:00Z    # when it was last reviewed
---
```

Notes keep the same keys inside their per-entry marker, next to `created_at`.

At write time the engine counts the kind's window from the write instant and
stores `review_after`. The window comes from `brain.yaml`:

```yaml
review:
  defaults:
    decision: 90d
    attempt: 60d
    note: 180d
    handoff: 90d
    state: 14d
    proposal: 14d
```

| Key | Default | Applies to |
|---|---|---|
| `decision` | `90d` | decisions |
| `attempt` | `60d` | attempts |
| `note` | `180d` | notes |
| `handoff` | `90d` | handoffs |
| `state` | `14d` | the project state in `brain review --due` |
| `proposal` | `14d` | pending proposals that were not refreshed |

Values accept a whole number of days (`90d`), weeks (`2w`) or a Go duration
(`2160h`). Malformed, zero or negative values are configuration errors
(`config.review.defaults.<kind>`) and `brain doctor` reports them. The block is
optional: a configuration without it keeps the defaults above.

Rules:

- A record without `review_after` (written before this feature) is measured from
  its creation time and the kind's current default. `brain review --due` marks
  the source so the difference is visible.
- A repair write (`superseded_by` set) is history and gets no review date.
- Review dates do not expire anything and do not touch `valid_to`. Superseding
  (via a review case or a `--supersedes` write) and explicit invalidation are the
  only ways to retire a claim (`brain record supersede`).
- `brain review --due` skips records that are superseded, invalidated, rejected
  or quarantined; quarantined records are in the quarantine queue instead.

### Due workflow

```bash
brain review --due                 # records, state and proposals past their date, oldest first
brain review --due --json          # the same list as machine-readable `due[]`
brain record review <id> --review-after 90d
brain record review <id> --review-after 2027-02-05
brain record review <id> --clear-review-after
```

`brain record review` also clears quarantine (`--trust verified|stated|inferred`)
and stamps `reviewed_at`. `--review-after` and `--clear-review-after` apply to
one record; they cannot be combined with `--all`.

`review-overdue` aggregates the overdue records of a project into one finding,
like the quarantine queue: the message names the count, the oldest record and a
sample of ids. Its severity is `warn` up to 30 days past the oldest review date
and `error` beyond it.

`brain doctor` reports the same counts in its `reviews` check (`code:
review-overdue`) and never changes a date.

### Proposal snooze

A pending or stale proposal can be deferred without dismissing it. The snooze
is stored as `snooze_until` in the proposal frontmatter and never changes the
lifecycle status or the candidate text:

```bash
brain promote <id> --snooze 7d          # also: 2026-12-01 or an RFC3339 time
brain promote <id> --unsnooze
```

- `brain review` lists active candidates only and shows the hidden ones as a
  separate count, for example `## Proposals (2 pending, 1 snoozed)`.
- `brain review --json` reports `snoozed_proposal_count`; the pending list and
  `pending_proposals` counts exclude snoozed candidates.
- `brain review --due` and the proposal findings skip a snoozed candidate until
  its snooze comes due; it then returns to the normal queues on its own.
- A digest rerun that refreshes the candidate keeps the snooze, so new source
  text does not bring a deferred proposal back into the inbox.
- Snoozing a promoted or dismissed proposal is a validation error, and so is an
  `until` that is not in the future. Promotion and dismissal clear the snooze
  together with the inbox state.
- The local API exposes the same write as
  `POST /v1/proposals/{project}/{id}/snooze` (see
  [localapi-v1.md](localapi-v1.md)); repeating the stored value is idempotent.

## Deterministic duplicates

Two records are **exact duplicates** when their normalized text is identical:

1. casefold, fold `ё` to `е`;
2. drop every rune that is not a letter or digit (punctuation, Markdown markup);
3. collapse whitespace;
4. SHA-256 the result.

What is compared per kind:

| Kind | Text |
|---|---|
| note | title + body |
| decision | title + context + alternatives + consequences |
| attempt | title + hypothesis + result + reason |
| handoff | summary + next action |

Metadata is deliberately excluded: tags, evidence, paths, provenance and links
may legitimately differ between two copies of the same claim.

**On write** (CLI, MCP and `brain promote`) an exact duplicate in the same
project and kind is refused with a typed `DuplicateError` that names the
existing record. Nothing is written, merged or suffixed. Overrides:

| Surface | Override |
|---|---|
| `brain note` / `decision` / `attempt` / `handoff` | `--allow-duplicate` |
| `brain promote` | `--allow-duplicate` |
| MCP write tools | no override: the refusal carries the existing id in `try_instead` |
| API | `AllowDuplicate` on the input struct |

A deliberate history write (`--supersedes` or `--superseded-by`) is not refused:
superseding a record with identical text is how a repair is recorded. A write
identical to one made in the last ten minutes is still the documented retry and
returns the existing record instead of an error.

**MCP refusal** for a duplicate:

```json
{"code":"DUPLICATE","message":"note \"note-...\" already records the same text: duplicate record",
 "try_instead":"read the existing note with brain_record id=note-..., or write different content",
 "current_revision":"","retryable":false}
```

An identical retry cannot succeed, so `retryable` is false; read the existing
record or write different text.

### Near duplicates

Near duplicates are reported, never refused or merged:

- a 64-bit **simhash** sketch of the normalized text; a Hamming distance of
  `<= 3` is near. Texts shorter than 8 normalized words are skipped by this
  detector because the sketch is too coarse for them;
- the shingle (Jaccard) detector used by the `near-duplicate` finding.

Both detectors pass a gate first: the two records must not name different dotted
release versions (release notes are a series, not duplicates), and their
**bodies** must overlap by at least 0.5. Titles and weekly-report templates
collide too easily on their own, so a shared heading alone is not a duplicate.

`brain review` reports a newer record once, against its most similar older
record, as a `near-duplicate` finding with both ids in the message. The shingle
detector stays deliberately conservative for automatic session handoffs (they
copy the state's next action on every session).

### Listing duplicates

```bash
brain duplicates                       # the selected project
brain duplicates --scan                # every registered project
brain duplicates --json                # clusters with ids, titles and reason
```

A cluster is `exact`, `simhash` or `shingles`. Exact clusters group records whose
fingerprint matches; near clusters group the records the simhash or shingle
detector links (a pair within the Hamming distance always shares one of four
16-bit bands, so the candidate search never hides a reportable pair). Members are
ordered oldest first: the first id is the one a person most likely keeps. No
command in this flow deletes, trashes or rewrites a record.

`brain doctor` counts exact and near clusters in its `duplicates` check and
names each project with clusters. Duplicates are a curation task, not damage, so
the check warns and never fails.

## Consolidation

`brain consolidate` turns the duplicate report into a reviewed, reversible job.
The default phase is a plan: it computes deterministic ops and writes nothing.

```bash
brain consolidate                          # plan the selected project
brain consolidate --scan                   # plan every registered project
brain consolidate --json                   # ops, near clusters and the plan fingerprint
brain consolidate --apply --op <id>        # apply only the reviewed ops
brain consolidate --apply --fingerprint <plan fingerprint>
brain consolidate --rollback <manifest>    # undo an apply
```

| Op | Proposed when | Risk | Undo |
|---|---|---|---|
| `merge-<survivor>` | an exact duplicate cluster, or a near cluster at similarity `>= 0.98` of the same kind and project | low / medium | roll back the printed merge manifest |
| `archive-<id>` | a superseded record older than `--archive-after` days (default 30) that no live record links to | medium | `brain record restore <id>` |

Near clusters below the threshold stay report-only in `plan.near`; the job never
merges them. `--max` caps the plan (default 50; `truncated` reports the cut) and
`--op` selects the ops to apply (repeatable or comma-separated).

An apply re-derives the plan from the records instead of replaying a stored op
list, so an op that is no longer current is `not in the current plan`, and a
stale `--fingerprint` is refused. 0.21.1 hardening: apply is serialized globally
(per data directory/project lock when available) and every archive op is
revalidated at apply time (record still exists, unchanged fingerprint, old
enough, no new live links/protection). Stale archive ops are refused/skipped
instead of applied from plan-time state. The run records
`backups/consolidate-<ts>/manifest.json`, and `--rollback` restores every merge
and archive it lists. Nothing is deleted: a merge trashes the duplicate and an
archive trashes the superseded record, and the trash keeps the file.

The MCP surface is `brain_judge(action=consolidate)` with `phase=plan|apply`,
`ids` and `fingerprint`; see
[mcp.md](mcp.md#63-brain_judgeactionconsolidate).

## Related

- Write-time caps and protected paths: [limits.md](limits.md)
- Search ranking and previews: [search.md](search.md)
- Security model: [security.md](security.md)
