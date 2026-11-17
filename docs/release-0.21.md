# Release 0.21.0 — consolidation, review routes, multi-machine hardening

Status: released; tag `v0.21.0`.

0.21 turns duplicate reports into a reviewed job, closes the local review loop
for records and state, and hardens the shared git brain for multiple machines
and parallel agent sessions.

## Consolidation (proposal-only)

- `brain consolidate [--plan|--apply] [--op <id>]... [--max N]
  [--archive-after D] [--scan] [--rollback <manifest>] [--json]`.
- `brain_judge(action=consolidate)` with `phase=plan|apply`, `ids` and
  `fingerprint`; `mem_consolidate` maps to it.
- Ops: exact-cluster merges, near clusters at similarity `>= 0.98` of the same
  kind/project, and archive of superseded records older than N days with no
  live links. Below-threshold near clusters stay report-only.
- Apply re-derives the plan, refuses unknown/stale ops, is serialized, and
  revalidates archive ops at apply time. Nothing is deleted: merges and
  archives move files to trash; a manifest under `backups/consolidate-<ts>/`
  supports reverse rollback.

## Review routes

- Local API `POST /v1/records/{id}/review` and
  `POST /v1/projects/{id}/state/review`: provenance-only (`reviewed`, `trust`),
  optional `expected_updated_at` / `expected_revision` CAS, idempotent,
  allowlist-aware. Record list/detail and state JSON expose
  `origin/reviewed/trust`.
- Desktop 1.5.0 surfaces those fields and adds review actions for quarantined
  records and state; the quarantine page reuses the record adapter.

## Multi-machine and parallel-write hardening

- `brain sync` recovers an interrupted rebase/autostash before the conflict
  check, instead of exiting 3 with a detached HEAD.
- Sync preflight self-heals `.gitattributes` (`* text=auto eol=lf`, binary
  rules, driver routes) and re-registers missing merge drivers with the current
  executable; a nested data directory is rejected.
- Driver-generated `state-conflicts/`, `registry-conflicts/` and
  `merge-conflicts/` are committed in the same sync and surfaced by
  `sync --status --json`; `review`/`doctor` report them.
- Shared raw-zone items use the deterministic `lv24-items` merge driver:
  host `observed_at` is stripped and both sides are kept under content-hash
  paths. Checkpoints remain local-only.
- Registry paths are unioned across machines; `repo_url` recognizes the same
  repository at a new checkout, and `discover` adopts the path instead of
  registering a duplicate project. Optional `machine_id` is recorded for
  provenance.
- Consolidation apply is serialized and archive ops revalidated; topic upsert
  requires a real per-topic lock; LvEF import is staged, serialized and
  manifested; raw publish prep holds the sync lock.

## Migration notes

- Data schema stays `schema_version: 1`; all changes are additive.
- Run `brain doctor --fix` once per clone to register drivers/attributes, or
  simply run `brain sync` (preflight self-heals).
- Existing raw-zone items are normalized by the `lv24-items` driver on the
  first shared merge; checkpoints stay untracked.
- Desktop 1.5.0 pins engine 0.21.0; Postgres schema remains 10.

## Known gaps

- Consolidation is manual; no schedule.
- 50-op cap per apply run.
- No cross-host runtime lock: two machines coordinate through git only.
- PII cleanup (tracked legacy mail/dumps) and server-canonical migration are
  later work.
- `lv24-legacy` remains deprecated.
