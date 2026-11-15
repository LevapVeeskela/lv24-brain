# Relations and two-phase merge

Two records can be connected by a **typed relation**: a closed verb, a status, a
reason and the judge who decided it. Relations are how the brain records that two
records support, contradict, replace or depend on each other without rewriting
either one.

This document covers WB-1.7 (two-phase merge) and WB-1.8 (conflict surfacing and
judging). Nothing here is applied automatically: a candidate is only a prompt, a
verdict is always an explicit call, and a merge is always its own command.

- **Frontmatter is the source of truth.** Relations and merge history live in the
  record's own file. The SQLite `relations` table is a derived projection and is
  rebuilt when it disagrees with the Markdown.
- **Pending is not a claim.** A candidate relation carries `status: pending`,
  never affects search ranking or context, and is reported by review until a
  person or an agent judges it.
- **Judging edits one record.** A verdict replaces the pair's verb on the record
  that owns the relation. The other record's status and validity are never
  changed, so a conflict stays a conflict until someone resolves it deliberately.
- **Merging keeps a tombstone.** An absorbed record keeps its file in the trash
  and names its survivor; nothing is deleted, and a manifest restores the exact
  bytes.

## Vocabulary

The verb set is closed, so a caller can branch on it and a review can count it.

| Verb | Meaning | Acyclic |
|---|---|---|
| `relates_to` | Same subject; no claim about which record wins. The only verb candidate detection writes. | no |
| `supersedes` | The source replaces the target. Unlike a validity supersede it does not end the target by itself. | **yes** |
| `conflicts_with` | The two records cannot both be true. | no |
| `compatible` | The records can both hold at once. | no |
| `scoped` | Each record holds in its own scope. | no |
| `depends_on` | The source only holds while the target holds. | **yes** |
| `implements` | The source implements the design the target describes. | no |
| `blocks` | The source prevents the target from proceeding. | no |
| `evidences` | The source supports the claim of the target. | no |
| `contradicts` | The source is evidence against the target. | no |
| `not_conflict` | A suspected conflict was judged away; kept so the pair is not proposed again. | no |

A `supersedes` or `depends_on` chain must stay a directed acyclic graph: a cycle
makes "which record is current" and "what depends on what" unanswerable. A cycle
through `relates_to` is allowed because it claims no hierarchy. The two verbs are
checked separately, so a supersede edge and a dependency edge never combine into
a false cycle.

## Status and verdict metadata

| Status | Meaning |
|---|---|
| `pending` | A candidate nobody judged. Advisory: not navigable, not ranked, reported by review. |
| `accepted` | A judged relation. This is the graph the project believes in. |
| `rejected` | Considered and judged away. The row is kept so the pair is not proposed again. |

An empty status means `accepted`: a hand-written or legacy relation is an
authored claim, while candidate detection always writes `pending` explicitly. A
pending relation is by definition unjudged, so it cannot carry `judged_at` or
`verdict_by`; a stored pending relation with either is a validation error.

Each judged relation also stores:

| Field | Meaning |
|---|---|
| `reason` | Required on a verdict; at most 500 characters. |
| `confidence` | Optional author or judge confidence between 0 and 1. |
| `judged_at` | When the verdict was given. |
| `verdict_by` | `human` or `agent`. |

One pair has one accepted verdict. Two accepted verdicts to the same target are
refused, with one exception: `relates_to` is a generic link and may sit next to a
specific verdict. Pending rows and judgments can coexist on a pair while it is
being decided; judging replaces the pair.

## Writing a relation

Every write command accepts a repeatable `--relation` flag:

```
brain note "Cache design" --body "..." \
  --relation decision-2026-11-15-use-sqlite-01J...:supersedes:"replaces the earlier store call"
brain decision "Use SQLite" --context "..." \
  --relation note-2026-11-14-benchmarks-01J...:evidences
brain attempt "Try WAL" --hypothesis "..." --result "..." --reason "..." \
  --relation decision-2026-11-15-use-sqlite-01J...:implements
brain handoff "..." --summary "..." --next "..." \
  --relation note-2026-11-14-benchmarks-01J...:relates_to:"same experiment"
```

The form is `<target-id>:<verb>[:<reason>]`. The reason may contain colons; the
target and the verb never do. An explicit relation is written as `accepted`
with the calling judge (`human` for the CLI), because an authored edge is a
claim, not a candidate.

Rules enforced at write time:

- the target must be a live record of the same project, or a tombstone (a merged
  or trashed record); a qualified `<project>/<id>` reference is refused;
- a record cannot relate to itself;
- duplicate `(target, verb)` pairs collapse; entries with different verbs for one
  target are only refused when both are accepted;
- a `supersedes` or `depends_on` verdict that would close a cycle is refused;
- an unknown verb or status, a missing target and a bad confidence are refused
  with a field name, before anything is written.

Relations point inside one project only. There is no cross-project edge: a
project that needs another project's record copies the id into its own text.

## Candidate detection

When a record is written, the brain compares it with the live records of the
same kind in the same project:

- comparison uses the normalized title and body (shingles, with a simhash sketch
  for longer text); a similarity of 0.6 or more is a candidate, and an automatic
  session handoff (`[auto]`) needs 0.9 so routine sessions do not flood review;
- at most three candidates are proposed, most similar first;
- a pair that already carries any relation is skipped, so a judged verdict is
  never re-proposed;
- every candidate is written as `relates_to` with `status: pending`, the score as
  its confidence, and a reason such as `similar text (78% shingle overlap)`.

Detection runs **before** the record is published, so the record is written once
with its candidates attached; a save never pays for a second rewrite. Candidate
detection never merges, trashes, ends or accepts anything.

`brain review` reports each pending relation as a `warn` finding
(`pending-relations`) with the source, the target, the age and the exact
`brain relation judge` command to run. Up to ten pairs are listed individually;
the rest are summarized. Accepted conflicts are deliberately absent from the
report: a judged conflict is a decision, not a task.

## Judging a relation

```
brain relation list [project] [--pending|--accepted|--rejected]
                    [--relation <verb>]... [--id <record>] [--target <record>]
                    [--limit N] [--json]
brain relation show <id> [project] [--json]
brain relation judge <from> <to> --verdict <verb> --reason <text>
                      [--confidence 0..1] [--by human|agent] [--project p] [--json]
```

`list` filters by status and verb and can be scoped to one source (`--id`) or to
the records that point at one target (`--target`, backlinks). Rows are ordered
pending first, then by source and target, so repeated listings are stable.
`show` prints a record's outgoing relations and its backlinks.

`judge` is the only way a relation changes status. It:

- requires a reason (a verdict without one cannot be reviewed) and a documented
  verb; confidence, if given, must be between 0 and 1;
- replaces the pair's stored verb and judgment metadata on the `from` record;
- accepts a tombstone as the target, so a superseded or merged record can still
  be named;
- is idempotent: repeating the same verdict writes nothing and reports
  `changed: false`;
- refuses a self-link, an unknown record and a `supersedes`/`depends_on` cycle;
- never edits the target record's status, validity or text, and never resolves a
  conflict by ending either record.

The MCP server exposes the same pair of operations:

| Tool | Kind | Purpose |
|---|---|---|
| `brain_relations` | read-only | List relations with both ends, status, target state (`found`, `tombstone`, `missing`), age and direction. Paginated with the standard cursor contract. |
| `brain_judge` | mutating | Record one verdict with reason and confidence; the judge is `agent`. The server asks the user to confirm the pair and verb before writing. |

Both respect `--read-only` (the judge tool is omitted), the project allowlist and
the write scope; a write to a project outside the allowlist is refused before the
verdict reaches the store. `brain_record` shows a record's relations and
backlinks; `brain_review` reports pending candidates.

## Two-phase merge

`brain record merge` absorbs duplicate records into one survivor:

```
brain record merge <survivor> <duplicate>... [--dry-run|--apply] [--json]
brain record merge --rollback <manifest> [--json]
```

A merge keeps one kind: the survivor and every duplicate must be notes, or
decisions, or attempts, or handoffs. The survivor keeps its id and text; every
duplicate becomes a tombstone (its file moves to the project's trash, the record
names its survivor in `merged_into`) and the survivor lists the absorbed ids in
`merged_from`.

**Phase 1 — plan.** The plan is computed without writing: the survivor, the
duplicates, the records already merged into the survivor, every `[[id]]`
reference and relation target that would be rewritten, and every reason the
merge cannot proceed. `--dry-run` (and the absence of `--apply`) prints the plan
and writes nothing. The plan is refused when:

- the survivor does not exist or is itself merged into another record;
- a duplicate is in the trash, already merged elsewhere, or of another kind;
- the merge would close a merge cycle (a duplicate already lists the survivor);
- a record is protected by the limits configuration, or the project is
  protected (the whole plan is refused with `ErrForbidden`).

Plans are idempotent: a duplicate already merged into this survivor is reported
as `already_merged` and the rest of the plan still applies.

**Phase 2 — apply.** Before the first record changes, the merge publishes a
manifest under `<data-dir>/merges/`. The manifest records the project, the
survivor, the duplicates, the fingerprint and, for every touched file, the before
and after hash plus the exact before bytes. Files are rewritten so hand-written
sections survive; typed frontmatter carries the merge metadata and the rewritten
relation targets. The applied records are re-projected into the search index, and
a journal entry names the merge and its survivor.

**Rollback.** `--rollback <manifest>` restores every recorded file byte for byte
and rebuilds the index from the restored records. It is safe to repeat: a
manifest already rolled back reports `already_rolled_back` and writes nothing. A
rollback is refused as a whole when a touched file changed after the merge,
naming the files instead of overwriting newer work.

A merge is a curation act, so it is a CLI operation only; no MCP tool applies,
trashes or rewrites records on its own. Discovery is read-only (`brain_relations`
and the `near-duplicate` review finding); the decision and the apply stay with a
person.

## Safety rules

- Candidate detection cannot accept, reject, merge or end anything.
- A judged conflict does not end either record; only a validity supersede, a
  status change or a merge changes a record's lifecycle, and each is its own
  command.
- Judging a relation edits exactly one record: the one that carries it.
- A relation target is always a record of the same project, and a self-link is
  refused.
- Hierarchical verbs cannot loop, and the check runs on the graph as it would be
  after the write.
- A merge refuses protected projects and records, keeps one kind, publishes its
  manifest before touching a byte, and refuses a rollback that would overwrite
  newer work.
- The derived index never becomes the source of truth: listing relations falls
  back to a records scan when the index has no rows, and a schema mismatch
  rebuilds the table.

## Backward compatibility

Records written before this feature carry no `relations` or merge fields and
behave exactly as before: no candidate, no pending finding, no navigation edge.
Existing `[[id]]` links keep their meaning; a relation is additive metadata next
to them. All new CLI flags are optional, new JSON fields are additive, and the
existing record, review and search commands keep their output and exit codes.
