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
| `warn` | Context may be **stale**. | `state-expired`, `next-behind-handoff`, `decision-code-changed`, `near-duplicate` |
| `info` | Curation hint, nothing is broken. | `next-maybe-done`, `question-answered`, `broken-link`, `supersession-gap` |

Finding kinds: `quarantined`, `state-expired`, `state-behind-commits`,
`next-behind-handoff`, `next-maybe-done`, `question-answered`,
`decision-contradicted`, `decision-code-changed`, `broken-link`,
`near-duplicate`, `supersession-gap`, `stale-superseded`, `proposal-stale`,
`proposal-orphan`, `proposal-contradicted`, `review-overdue`.

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
  is still the only way to retire a claim (`brain record supersede`).
- `brain review --due` skips records that are superseded, rejected or
  quarantined; quarantined records are in the quarantine queue instead.

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

## Related

- Write-time caps and protected paths: [limits.md](limits.md)
- Search ranking and previews: [search.md](search.md)
- Security model: [security.md](security.md)
