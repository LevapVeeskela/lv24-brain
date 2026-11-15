# Release 0.16.0 — memory quality

Status: implementation complete; not tagged. Commits: `90a45e8` (temporal
validity, supersede, as-of, eval harness), `a769c79` (search v2, progressive
disclosure, write limits), `61e700a` (review v2, deterministic dedup),
`ec919c2` (layered context packer), `8646e6d` (relations, judge, two-phase
merge). Audit: `ideas/_audit/` (local only).

0.16 changes what the brain says, not how it is stored. Records become
temporally honest, search becomes explainable and budgeted, review findings
explain themselves, and duplicates or contradictions are surfaced and judged
instead of silently accumulating. Data schema stays `schema_version: 1`.

## Temporal validity

- Records carry `valid_from`, `valid_to`, `superseded_by`, `supersedes` and
  `confidence`. An ended record stays in git; only its status changes.
- `brain record supersede <old> --by <new>` ends the old record and links both
  sides. `brain note|decision|attempt|handoff --supersedes <id>` does the same
  when the replacement is written; `--superseded-by <id>` repairs history by
  writing a record that is already ended. The command is idempotent; a cycle, a
  self-link or a missing target is a validation error.
- Every read path — CLI, JSON and MCP — evaluates validity the same way. The
  default hides records that ended at or before now. `--as-of <RFC3339|date>`
  returns the generation that was current at that instant (a bare date means
  midnight UTC) and `--include-superseded` (alias `--all-time`) keeps ended
  records. An `--as-of` instant wins over the other two.
- `brain record show <id> --history` prints the whole chain.

## Search v2

- One composite score: `0.55 bm25 + 0.20 recency + 0.15 importance(kind) +
  0.05 pin + 0.05 link`, mapped to `[0, 1)` by `total / (total + 1)`. Ties break
  on `updated_at DESC`, then `id ASC`, so paging is stable.
- Every hit returns only the `match_reasons` that fired; `--explain` adds the
  weighted contribution of each signal, and the contributions add up to the
  score exactly.
- `--match-mode`, `--relaxed`, `--min-score` and `--full` control the answer.
  Relaxed retrieval is a separate, labelled fallback rather than a silent
  widening of the query.
- Server mode keeps its Postgres ranking and refuses the local ranking controls
  instead of ignoring them. Previews, pagination and `brain get` work in both
  modes.
- Weights live in one place (`internal/adapters/sqliteindex/rank.go`); a change
  is measured with `brain eval`, and the baseline gate refuses a regression.

## Progressive disclosure

`brain search` returns ranked previews, `brain get <id>` reads one record, and
`brain timeline <id>` shows what came before and after it. MCP matches the same
three steps: `brain_search` with compact previews and `expand_ids` for the few
ids worth reading in full. No command spends tokens on text the caller did not
ask for.

## Layered context packer

`brain context` and `brain_context` pack one answer in five ordered layers:
`core` (state, next action, open questions, latest handoff, review findings,
1 200 tokens), `decisions` (1 500), `attempts` (1 000), `query` (search-v2 hits
and failed attempts, 1 500), `linked` (1-hop `[[id]]` neighbours, 800). The
default total is 6 000 tokens, the maximum a caller may request is 32 000.

The core layer is never dropped. Everything else follows one rule: a record is
never split. A record that does not fit is skipped whole and listed in
`truncated[]` as `{id, section, reason}`. Ordering is deterministic, so two runs
over the same store and clock produce the same sections, tokens and summary.
Token counts come from the shared `internal/textbudget` estimate (four
characters per token); there is no model-specific tokenizer dependency.

## Review v2

- Every finding carries `severity` (`error`, `warn`, `info`), `why`, `next`,
  `confirm`, `evidence` and `fixable`/`fix`. A caller branches on severity
  instead of matching text; the same fields travel in `--json` and in MCP
  `brain_review`.
- `review_after` turns "looks current" into a dated promise. It is a prompt to
  re-check, never an expiry: an overdue record stays current until a person or
  agent confirms, reschedules or supersedes it. `brain review --due` lists what
  is due.
- `quarantined` remains first in the queue, so a short context or session
  summary never truncates the finding that says external text awaits review.
- Existing finding kinds and JSON keys keep their meaning; `severity`, `why`,
  `next` and `confirm` are additive.

## Deterministic dedup

- An exact duplicate body (same project and kind, whitespace-insensitive) is
  refused at write time with the existing record named in the error.
- Near duplicates are report-only clusters: `brain review` reports
  `near-duplicate` and `brain duplicates` lists the pairs. Nothing is merged,
  trashed or rewritten automatically.
- Duplicate detection is text-based and runs offline; there is no model and no
  embedding.

## Relations and judging

- A relation is a closed verb (`relates_to`, `supersedes`, `conflicts_with`,
  `compatible`, `scoped`, `depends_on`, `implements`, `blocks`, `evidences`,
  `contradicts`, `not_conflict`), a status (`pending`, `accepted`, `rejected`),
  a reason and the judge who decided it.
- Candidate detection writes only `pending` `relates_to` edges. A pending
  relation is advisory: it does not rank, does not enter context, and is
  reported by review until it is judged. Judging is an explicit call —
  `brain relation judge` / MCP `brain_judge` — and edits exactly one record.
- `supersedes` and `depends_on` chains must stay acyclic; a cycle through
  `relates_to` is allowed because it claims no hierarchy.
- Frontmatter is the source of truth. The SQLite `relations` table is a derived
  projection and is rebuilt when it disagrees with the Markdown.

## Two-phase merge

`brain record merge <survivor> <duplicate>...` absorbs duplicates into a
survivor: one kind only, tombstones in the trash, `merged_into`/`merged_from`
recorded, `[[id]]` references and relation targets rewritten, hand-written
sections preserved.

- Phase 1 is a plan: it is computed without writing and refused for a missing
  record, a cross-kind duplicate, a merge cycle, or a protected record or
  project. `--dry-run` prints the plan only. Plans are idempotent.
- Phase 2 publishes a manifest under `<data-dir>/merges/` before the first file
  changes, with the before bytes and the before/after hashes of every touched
  file, then re-projects the index and journals the merge.
- `brain record merge --rollback <manifest>` restores every file byte for byte
  and is safe to repeat. It is refused as a whole when a touched file changed
  after the merge, naming the files instead of overwriting newer work.
- A merge is a curation act, so it is CLI-only: no MCP tool applies, trashes or
  rewrites records on its own.

## Evaluation harness

`brain eval` scores retrieval, temporal filtering and context packing against
authored golden fixtures, fully offline (temporary data directory, local index,
no network, no model). Exit codes: `0` matches the baseline, `1` a metric
regression or fixture error, `2` a usage error.

Baseline (`eval/baseline.json`, 63 tasks across retrieval, Russian morphology,
paraphrase, temporal, staleness and as-of): recall@5 **0.984**, precision@5
**0.197**, MRR **0.952**, superseded-leak **0.000**, tokens/answer **98.3**,
p95 **4.5 ms**. Every ranking-derived metric is deterministic; only latency
varies between runs. See [eval.md](eval.md).

## Write-time limits

`brain.yaml` can bound one record write and refuse protected paths:
`max_body_bytes` (1 MiB), `max_title_chars` (200), `max_metadata_bytes`
(16 KiB), `protected_projects`, `protected_globs`. Limits are enforced at the
app boundary, so the CLI, MCP, session hooks and `brain promote` obey the same
rule. An omitted key and an explicit `0` both select the default — zero never
disables a cap. `brain doctor` reports a negative limit as a configuration
error and warns when a positive limit is too small to be useful. See
[limits.md](limits.md).

## Schema changes

- Records gain optional `valid_from`, `valid_to`, `superseded_by`, `supersedes`,
  `confidence`, `review_after` and typed `relations[]`. No key is required and
  `schema_version` stays 1, so an older engine still reads a 0.16 file and
  ignores what it does not know.
- The SQLite projection gains validity and relation columns. The index stores
  its own schema version in `user_version` and rebuilds itself when the stored
  version differs, so no manual migration step is needed.

## Visible changes

- `brain search` hides ended records by default; pass `--include-superseded` or
  `--as-of` when history is the question.
- Search results are previews unless `--full` is passed.
- `brain review` adds `severity`, `why`, `next` and `confirm` to every finding;
  text consumers that matched on the old single-line format should branch on
  severity or read JSON.
- `brain context` returns layered sections under a 6 000-token default budget
  and names what it dropped in `truncated[]`.
- The MCP server exposes **19** tools instead of 17: typed relations and
  judging are served (due views ride on `brain_review`). 0.17 consolidates the
  surface to 15; see [release-0.17.md](release-0.17.md).

## Migration

1. Upgrade the engine on every machine. Data schema is unchanged; an older
   engine still reads new files but cannot see validity or relation fields.
2. Run `brain index --rebuild` once (or let the automatic schema-version
   rebuild happen on the first index open) and `brain doctor`.
3. Restart MCP hosts to pick up the 19-tool surface.
4. Optional: run `brain eval --fixtures eval/fixtures --baseline
   eval/baseline.json` to confirm retrieval on the local data-independent
   baseline.
5. Optional: add a `limits` block to `brain.yaml`; without it the documented
   defaults apply.

## Known gaps

- `state.md` still has no provenance; `brain_state_set` text is trusted.
- `record review` is local/git only; server mode has no provenance-edit route.
- Relations are stored and indexed but not yet used for neighbour navigation.
- Merge is CLI-only (no MCP apply).
- No code graph (0.18), no embeddings, no desktop memory UI.
- The MCP tool surface is 19; 0.17 must consolidate to 15 with compatibility
  profiles.
