# Release 0.21.0 — proposal-only consolidation

Status: unreleased; not tagged. Engine work: the `brain consolidate` job and the
`brain_judge(action=consolidate)` MCP action.

0.21 turns the duplicate report into a reviewed job. `brain consolidate` plans
deterministic ops (merge an exact duplicate cluster, archive a superseded record
with no live links) and writes nothing; `--apply` executes only the reviewed ops
and records a manifest that `--rollback` restores. Nothing is deleted: a merge
trashes the duplicate, an archive trashes the superseded record, and the trash
keeps the file.

## Consolidation

- `brain consolidate [--plan|--apply] [--op <id>]... [--max N] [--archive-after D]
  [--scan] [--rollback <manifest>] [--json]`.
- `brain_judge(action=consolidate)` with `phase=plan|apply`, `ids` and
  `fingerprint`; `mem_consolidate` maps to it.
- Ops: `merge-<survivor>` (exact clusters, and near clusters at similarity
  `>= 0.98` of the same kind and project) and `archive-<id>` (superseded for
  more than `--archive-after` days, default 30, with no live links). Near
  clusters below the threshold are report-only.
- Apply re-derives the plan, refuses an unknown op id and a stale fingerprint,
  and writes `backups/consolidate-<ts>/manifest.json`; `--rollback` restores it.
- Docs: [review.md](review.md#consolidation),
  [mcp.md](mcp.md#63-brain_judgeactionconsolidate).

## Known gaps

- The job plans and applies; it does not run on a schedule.
- `--max` and the MCP op cap are fixed at 50 per run.
