# Release 0.27.1 — review before retirement

Status: release candidate on `main`.

0.27.1 makes a supersede a review case instead of an immediate retirement, adds
an archive for records that were invalidated without a successor, and reports
suspected contradictions between current records. The data schema stays 1 and
the Markdown format is unchanged; the local search index stays schema 10, so an
index built by 0.27.0 is reused and an older one rebuilds once on first open.
Search and context responses keep their shape; context adds one optional field.

## Supersede is a review case

`brain record supersede <old-id> --by <new-id>` now opens a **proposed** case
when the store has an activity journal (local and git modes). The successor's
`supersedes` link is written immediately, but the predecessor is left
byte-for-byte untouched: it stays current, listed and searchable until a review
accepts the case.

A case records both ends, the reason, its status and revision, the proposal and
decision times, the predecessor's previous validity tuple, the tuple the
approval wrote, and the state hash used by rollback:

| Field | Meaning |
|---|---|
| `kind`, `old_id`, `new_id` | the predecessor kind and both record ids |
| `reason` | why the case was opened; default `superseded by <new-id>` |
| `status` | `proposed`, `accepted`, `rejected` or `rolled_back` |
| `revision` | position in the case timeline, not a clock tie-break |
| `proposed_at`, `decided_at` | when the case opened and when it was decided |
| `previous`, `applied` | the validity tuple before and after an approval |
| `expected_hash` | fingerprint of the predecessor as the approval left it |

`brain review` prints the pending cases as their own block:

```text
## Supersede cases (1 proposed)
- [proposed] decision-...-one → decision-...-two: the second shard is read-only
```

### Accept, reject, roll back

| Action | Allowed when | Effect on the predecessor |
|---|---|---|
| accept | `proposed` | ends it at the approved instant: writes `valid_to` and `superseded_by`, marks a decision `superseded`, and records both tuples plus the state hash |
| reject | `proposed` | writes nothing to it; the case stores the refusal reason |
| rollback | `accepted` | restores the exact previous `valid_from`, `valid_to`, `superseded_by` tuple and, for a decision, its previous status; clears the successor's `supersedes` claim when it names this predecessor |

A rejection requires a written reason. An acceptance or rollback note is
optional and defaults to `approved` or `rolled back`. Every case reason and note
is a single line of at most 240 characters; a newline or an empty rejection
reason is a validation error.

A rejected or rolled-back case may be proposed again with the same or another
successor; the new proposal takes the next revision. A second successor cannot
claim the same predecessor while a case is still proposed. A predecessor whose
validity already ended cannot be superseded, and a rejected decision cannot be
the successor of an approved case.

Rollback is guarded by `SupersedeStateHash`: identity, kind, status, the
`valid_from`, `valid_to`, `superseded_by` and `supersedes` fields, `updated_at`
and the normalized record text. The approval stores the hash; a rollback
compares it (or the caller's `expected_hash`) with the record as it is now. A
stale hash (a mismatch) is a revision conflict
(`domain.ErrConflict`, rendered as `409 conflict` by HTTP surfaces) and nothing
is written. A rollback also refuses when the record is no longer superseded by
that successor, or when the case is not `accepted`.

The existing decision lifecycle settles a case too: accepting or rejecting the
successor decision through `brain record status <successor> accepted|rejected`
(or MCP `brain_judge` with `action=accept|reject`) applies the same review to
every proposed case that names it. Over MCP, `brain_judge(action=supersede)` with
`id=<old-id>` and `to=<new-id>` opens the same proposed case. The direct review
action is `brain record supersede-review <old-id> --accept|--reject|--rollback`
with an optional `--reason` (required for reject) and `--expected-hash` (refuse
a stale rollback). Stores without an activity journal, including server mode,
have no case queue and keep the immediate supersede.

### Legacy `--supersedes` still applies at once

Writing `brain note ... --supersedes <old-id>` or
`brain decision ... --supersedes <old-id>` still declares the link in the same
write and ends the old record immediately. There is no case to review because
the author is the reviewer. The same applies to attempts and handoffs and to the
`--superseded-by` repair write. Use `brain record supersede` when the retirement
itself should wait for a second look.

`brain record supersede` prints the link as soon as it is written, even when the
case is only proposed. Read `brain review` or `brain record show <old-id>` to
see whether `valid_to` is still empty; `brain record show` prints the validity
tuple and `--history` prints the chain.

## Invalidated archive

An **invalidation** retires a record without naming a successor. It writes only
`valid_to` to the frontmatter, keeps the text, and records an `invalidate`
journal event with the reason and the end instant. The record is not moved,
trashed or deleted.

- The reason is required (a single line, at most 240 characters). An empty
  reason is a validation error, and a repeat invalidation is an idempotent
  no-op that keeps the first instant.
- Default search, context and listings hide the record. An archive query keeps
  it: `brain record archive [--json]` lists the ended records with reason and
  instant; list/search accept `--include-superseded` or `--all-time`, and the
  list API names the filter `include_invalidated`. MCP search and context have
  no archive switch.
- The engine's archive read reports `kind`, `id`, `invalidated_at`, `reason` and
  `superseded_by`. The last field is set for an approved supersede and empty for
  an explicit invalidation.
- A rejected case never reaches the archive, and rolling an accepted case back
  removes its entry: the archive reads the record's current validity and the
  winning journal event.
- The reason is operational history in the journal; the record file keeps its
  text and gains the validity end, while the reason stays in the journal.
  `brain journal --event invalidate` shows the invalidation lines of the local
  data directory.

## Suspected contradictions

Review has a new finding kind, `suspected-contradiction`. It is a `warn`,
report-only finding: it never edits, never supersedes, and `brain review --fix`
does not apply it.

The check compares the project's current decisions and notes, newest 200. It
skips stubs, rejected decisions, records whose validity ended, and any pair
already linked by `supersedes`/`superseded_by` — a declared replacement is a
decision, not a suspicion. Cross-project records never pair.

Before a heuristic runs, the pair must repeat at least 40 % of the shorter
record's stemmed words and at least two words. Two records about different
subjects therefore cannot be flagged by a shared "not" or "deprecated".

| Reason | Fires when | Extra gate |
|---|---|---|
| `temporal` | the newer record carries an override marker and the older does not: `no longer`, `instead`, `replace*`, `deprecated`, `obsolete`, `sunset`, `phased out`, `больше не`, `вместо`, `замен*`, `устарел`, `неактуал*` | 60 % overlap |
| `negation` | exactly one side negates (`not`, `no`, `never`, `cannot`, `не`, `нет`, `без`, contractions); both negating is agreement | 50 % overlap |
| `antonym` | the records use opposite sides of a closed pair: enable/disable, allow/deny, increase/decrease, keep/remove, before/after | base gate; same negation polarity |
| `numeric` | the same recognized unit carries different values (`90 days` against `180 days`, `14d` against `30d`) | base gate |

The heuristics run in that order, so one pair yields at most one finding with
the most specific reason. The message names the reason, both record ids and both
titles; the evidence is `<older-id> <-> <newer-id>`; the next action is to open
both records and accept or reject one. The check is deterministic and makes no
model call.

False negatives are expected: the heuristics need shared vocabulary, numeric
changes need a recognized unit, and only the newest 200 decisions and notes are
compared. Treat the finding as a prompt to read the pair, not as proof.

## Path relevance in context

Context ranking can prefer decisions whose `paths` overlap the checkout the task
is running in. The signal is **off by default**; opt in with
`BRAIN_RANKING_PATHS=1` (also `true`, `yes` or `on`). The YAML key
`ranking.paths` is the intended configuration spelling but is not wired in
0.27.1: only the environment switch has an effect.

Only decisions carry `paths`, and only candidates in the query layer are
rescored. The signal is additive and capped at **0.05**:

| Tier | Bonus | Match |
|---|---|---|
| exact | 0.05 | a decision path is a changed file |
| directory | 0.03 | a decision path is a changed file's directory or an ancestor |
| branch | 0.013 | a decision path shares a word of at least three letters with a non-default branch name |

The checkout read is best-effort and read-only: `git diff --name-only HEAD` plus
untracked `git ls-files --others --exclude-standard`, normalized to the project
checkout and lowercased, with a 2 000-file cap and a 2-second timeout. A
directory that is not a repository, a missing git binary or a timeout yields no
signal, not an error. Paths are compared case-insensitively; `main`, `master`,
`trunk` and `develop` contribute no branch words.

`brain context --explain` reports the switch and each contribution:

```text
paths: on, 12 changed files, branch feature/ret-5
paths: decision-2026-11-19-paths +0.050 exact internal/app/context.go
paths: decision-2026-11-19-paths +0.030 dir internal/app
paths: decision-2026-11-19-paths +0.013 branch feature/ret-5
```

JSON adds `path_signal` to `records[]` and `failed_attempts[]` when the bonus is
non-zero. `sections[].records[]` keep the score only; the path lines live in
`explain[]`. The bonus is folded into `score` before ordering and the score is
rounded to three decimals after the addition, so the printed number and the
order agree.

The acceptance fixture moves ten expected records ahead of their no-path
competitor with the switch on and reproduces the flagless order byte for byte
with it off. The feature stays opt-in until an eval delta justifies a default.

## Surfaces and compatibility

- Search JSON is unchanged. Context JSON is additive: `path_signal` on ranked
  records, and the `paths:` lines in `explain[]` when `explain` is requested.
- Review JSON accepts a new `kind` value, `suspected-contradiction`; no field is
  removed or renamed.
- The data schema, Markdown and index schema are unchanged. No migration and no
  index rebuild.
- Server mode has no activity journal, so `brain record supersede` still applies
  immediately there and no case queue exists. Path relevance stays opt-in and
  needs a local checkout to read.
- A pre-0.27.1 binary does not know cases: it applies a supersede immediately
  and cannot read the case queue. Use the same version on machines that review
  the same records.

## Known limits

- **Cases live in the activity journal.** The case is not in the Markdown
  record. Only a machine that has the proposing user's journal line can list or
  review it; a clone with the records but without that line sees the successor's
  `supersedes` claim and no case. Until a sync brings the line, only the
  proposing machine can review the case. Journal files are part of the data
  repository and union-merge on sync, so another machine can review a case after
  that sync; treat the proposing machine as the reviewer of record.
- **`--supersedes` is still immediate.** A note or decision written with
  `--supersedes` ends the old record in the same write. It does not open a case,
  so it cannot be rejected or rolled back through review.
- **`ranking.paths` YAML is not wired.** Only `BRAIN_RANKING_PATHS` enables path
  relevance. A `ranking:` block in `brain.yaml` is ignored by 0.27.1.
- **Case reasons are defaulted on the CLI and MCP.** `brain record supersede` and
  `brain_judge(action=supersede)` open the case with the default
  `superseded by <new-id>` reason; only the engine write `SupersedeWithReason`
  accepts a custom reason in 0.27.1.
- **A rejected case keeps the successor's claim.** Rejection records the
  decision and leaves the predecessor untouched, but it does not clear the
  successor's `supersedes` frontmatter. There is no CLI flag to remove that
  claim in 0.27.1; write and accept a new case, or repair the records outside the
  CLI.
- **No new CLI subcommand for explicit invalidation or direct rollback.** The
  engine actions exist; the CLI exposes decision-case accept/reject through
  `brain record status` and lists the queue through `brain review`.

## Upgrade

The upgrade is a binary update: no data migration, no new index schema and no
required configuration. An index built by 0.27.0 is reused as is; an older one
rebuilds once on first open.

```sh
brain update --yes
brain doctor
brain review
```

Run `brain review` once after the update to see whether any project already has
pending cases from a newer machine, and use the same engine version on machines
that write supersede links. `BRAIN_RANKING_PATHS` stays unset unless path
relevance is being evaluated.
