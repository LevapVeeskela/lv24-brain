# LV24 Exchange Format (LvEF v1)

Curated records can be moved between brain instances with `brain export` and
`brain import`.

## Export

```powershell
brain export --out <dir-or-zip> [--project <id>] [--since <RFC3339>] [--json]
```

Writes `manifest.json`, `records.jsonl`, `projects.jsonl` and
`tombstones.jsonl` (empty in v1). Records are state, note, decision, attempt
and handoff only. Ordering is deterministic and LF-terminated.

## Import

```powershell
brain import --from <dir-or-zip> [--dry-run] [--json] [--create-projects]
```

Import is idempotent by content hash. Existing records with identical content
are reported as `unchanged`; differing content is reported as `conflict` and
never overwritten. Missing projects are created only with `--create-projects`.
Skips, conflicts and quarantined rows are reported but are not treated as
failures (exit 0 unless a real read/write/system error occurs).

0.21.1 parallel-write hardening: imports take a global import lock when the
store can serialize runs, compute a staged plan before publishing, write an
import manifest under `backups/import-*` when a data directory is available, and
report reruns idempotently without leaving ambiguous partial state.

## Round trip

`export -> import -> export` is byte-identical for the same data. Zip
containers use store/deflate only and are guarded against path traversal and
zip bombs.
