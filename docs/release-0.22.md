# Release 0.22.0 — remove the `lv24-legacy` profile

Status: unreleased; not tagged.

The `lv24-legacy` MCP profile was a bridge for clients written against 0.16.
It was deprecated in 0.17, announced for removal in 0.19, and is removed now.

## Removed

- The `lv24-legacy` profile and its seven retired tool names
  (`brain_state_get`, `brain_state_set`, `brain_journal`, `brain_records`,
  `brain_relations`, `brain_review_record`, `brain_decision_status`).
- `--profile=lv24-legacy` is now a startup error with exit code 2; the error
  lists the supported profiles. There is no silent fallback.
- The `deprecated` flags that only the legacy profile set in `Profile`,
  `_meta.lv24.brain/compat` and `features`.

The native `lv24` profile is unchanged: 17 tools. `engram`, `codegraph` and
`graphify` remain served.

## Migration

A client that still uses a 0.16 name must switch to the native call:

| 0.16 name | Replacement |
|---|---|
| `brain_state_get` | `brain_state(action=get)` |
| `brain_state_set` | `brain_state(action=set)` |
| `brain_journal` | `brain_catchup(mode=journal)` |
| `brain_records` | `brain_record(action=list)` |
| `brain_relations` | `brain_record(action=relations)` |
| `brain_review_record` | `brain_judge(action=review)` |
| `brain_decision_status` | `brain_judge(action=accept\|reject\|supersede)` |

Calling a retired name on `lv24` still returns `NOT_FOUND` with the replacement
in `try_instead`. The Markdown data format is unchanged; no data migration is
needed.

## Known gaps

- PII cleanup (tracked legacy mail/dumps), auto-sync/conflict UX and the
  server-canonical decision remain 0.22 work.
