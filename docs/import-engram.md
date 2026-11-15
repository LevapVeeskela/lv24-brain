# Importing engram memories

`brain migrate --from engram` copies an engram store into the LV24 Brain
Markdown tree. The source is opened read-only; every write goes through the
normal migration path with a manifest and a rollback command. The importer is
part of the 0.17 "One MCP" release and uses the schema-v1 frontmatter with the
additive engram-compatible fields (`subtype`, `scope`, `topic_key`,
`occurrences`, `relations`, `source.external_id`, `source.session`).

## Before the first import

1. Freeze engram writes: stop the engram MCP server and close the CLI/TUI. A
   live WAL is readable, but no new sessions should be written during the
   migration.
2. Back up `engram.db` (or export JSON) and the brain data directory.
3. Run a dry run first. It performs every read, mapping, dedup and path plan and
   writes nothing.
4. Apply only after the dry-run counts are understood. Keep the engram server
   disabled for a week, then remove it.

```sh
# Read-only plan; writes nothing, prints the tree hash before and after.
brain migrate --from engram --db ~/.engram/engram.db --dry-run

# Apply into the data directory from brain.yaml (or --data-dir <dir>).
brain migrate --from engram --db ~/.engram/engram.db

# Revert the newest migration byte-identically (created files, registry, config).
brain migrate --rollback latest

# Machine-readable report and a project remap override.
brain migrate --from engram --db ~/.engram/engram.db --dry-run --json
brain migrate --from engram --db ~/.engram/engram.db --dry-run --project-map engram-map.json
```

`--db` accepts `engram.db` or an `engram export` JSON file. The JSON reader
accepts either the tables at the top level (`observations`, `relations` or
`memory_relations`, `sessions`, `prompts` or `user_prompts`,
`observation_versions`) or nested under `tables`. An export does not repeat
`project` on every observation, so a missing observation project is taken from
its session; a real database keeps its NULL project rows quarantined.

Exit codes: `0` success, `1` a conflict/failed row or a runtime error, `2` a
usage error.

## What is imported

| engram | Brain record | Rule |
|---|---|---|
| `observations` type `decision` | `decision` | `status: accepted`; `context` from the labelled `What`/`Why`/`Where`/`Context` sections; `consequences` from `Learned`/`Consequences`; the source identity is in `evidence`. |
| `observations` type `policy` | `decision` | A policy is a decision-shaped rule. `subtype: policy` is preserved. |
| `observations` type `session_summary` | `note` subtype `session_summary` | Collapsed by `(project, normalized title)`: the latest body is current, older bodies go to `## History`, `occurrences` counts them. |
| all other observation types | `note` subtype `<type>` | Body preserved verbatim, timestamps preserved, `review_after` kept. |
| `memory_relations` | `relations[]` | Only `judgment_status = judged` and in-scope, same-project pairs. `related` maps to `relates_to`; `compatible`, `scoped`, `conflicts_with`, `supersedes`, `not_conflict` map directly. A judged pair keeps exactly one verdict (the newest engram row wins), because the brain relation model rejects two accepted verdicts for one pair. Pending, orphaned, superseded, duplicate and dangling relations are skipped and counted. |
| `sessions` | provenance only | `source.session` on each record. Session rows never become records. |
| `observation_versions` | `## History` | Previous bodies are kept newest first, bounded to 20 entries. |
| `user_prompts` | prompt zone | Only with `--include-prompts` and `prompts.enabled: true`; written under `projects/<id>/prompts/YYYY-MM-DD-engram.md` with `visibility: local`, a `.gitignore` containing `*`, and never indexed. Prompt rows whose project is unknown or a test path are skipped. |
| `sync_mutations`, cloud tables, tombstones | — | Never imported. |

Imported records carry `origin: import`, `trust: stated` and
`source.external_id: "engram:<sync_id>"`. Quarantined records additionally carry
`reviewed: false`, which keeps them out of trusted context (`brain_open`,
`brain_context`) until `brain review` / `brain_judge` clears them.

## Project mapping

Default rules (all overridable with `--project-map`):

| engram project | Brain project |
|---|---|
| `lv`, `lv24` | `lv24` |
| `main` | `IMS`, `lv24` or `lv24-harvest`, attributed through `sessions.directory` (longest registered directory prefix) |
| `lv24-brotato` | `lv24-harvest` (retired; no project is created) |
| `lv24-youtube-knowledge` | `lv24-ytk` |
| `inst` | `instaagent` |
| `lv24-code`, `lv24-ytk`, `ferrum-enterprise`, `lv24-harvest`, `lv24-identity`, `opencode-consilium`, `lv24-brain`, `lv24-seo-optimizer`, `lv24-vigil`, `lv24-brain-site`, `admin-assistant`, `instaagent`, `lv24-brain-docs` | direct; registered when missing |
| `veeskela` | personal scope under the last known repository, else the `personal` project |
| `""` (NULL), `\`, `unknown`, `.github` | quarantine project `engram-unmapped` |
| `bats` | skipped (test artifact) |

Path-named projects (for example `c:\0project\lv24`) are attributed by their
own path prefix, never by the session directory: a test prompt run inside a real
repository must not inherit that repository's identity. A path that matches no
prefix is quarantined.

The report prints one line per mapping decision with the source value, session
directory, target, action, reason and row count.

### Project-map file

```json
{
  "map": {"lv24-youtube-knowledge": "lv24-ytk", "inst": "instaagent"},
  "directories": {"c:\\0project\\ims": "IMS", "c:\\0project\\lv24": "lv24"},
  "skip": ["bats"],
  "quarantine": ["", "\\", "unknown", ".github"],
  "personal": ["veeskela"]
}
```

The file is merged over the defaults: map/directories entries are added or
overridden; a non-empty `skip`, `quarantine` or `personal` list replaces the
default list. Unknown keys and invalid project ids are rejected. Paths are
matched case-insensitively with `/` or `\` separators.

## Deduplication and idempotency

- Identity is `(source.system = engram, source.external_id = engram:<sync_id>)`.
  A rerun reports every file as `unchanged` and writes nothing.
- Target ids are deterministic:
  `note-<YYYY-MM-DD>-<sha1(external_id)[:8]>` and
  `decision-<YYYY-MM-DD>-<sha1(external_id)[:8]>` (the hash is extended only on
  an actual collision).
- Duplicate `sync_id` rows are collapsed to the newest row; the others are
  counted as collapsed.
- Only `session_summary` is collapsed automatically. Every other internal
  near-duplicate and every cross-source duplicate is **reported only**: the
  importer never merges, overwrites or deletes a curated record. After an apply,
  `brain review` and `brain duplicates` show the same pairs for a person or
  agent to judge.
- A target file that exists with different bytes is a conflict and is never
  overwritten. Edit the imported file, then rerun: exactly that file is reported
  as a conflict.

## Dry run, apply, rollback

A dry run prints:

- rows read and the disposition parity
  (`imported + collapsed + quarantined + skipped + failed == rows`);
- projected files per project and kind (`created`/`unchanged`/`conflicts`);
- every project mapping decision, collapse group and quarantine/skip row;
- relation and prompt counts;
- duplicate findings;
- the SHA-256 of the target tree before and after. They must be equal; a
  different value fails the run.

An apply writes `backups/migrate-<UTC timestamp>/manifest.json` before the CLI
updates `registry.json` and `brain.yaml`. `--rollback latest` deletes every file
the run created, restores the previous `registry.json` and config bytes, and
leaves files edited after the migration in place (reported as `modified since
migration`, exit code 1). The target tree is byte-identical afterwards, apart
from `backups/` and any derived `index/` built by a later `brain index`.

Every apply, including a verification rerun that reports only `unchanged`,
creates its own manifest. `--rollback latest` therefore reverses the *newest*
run; pass the manifest path printed by the import (or use `brain migrate
--rollback <path>`) to undo the import itself after a rerun. A rollback does not
delete the derived index: rebuild or remove `index/` after reverting.

## Report counts

```text
rows        4545   live observations read
imported    3691   source rows that became a curated record
collapsed    783   rows folded into a survivor (summaries + duplicate sync ids)
quarantined   70   records in engram-unmapped (127 source rows before collapse)
skipped        1   rows deliberately not imported (bats)
failed         0   rows with unusable required fields; a failure aborts before any write
created     1073   target files written (notes are grouped by project/day)
unchanged      0   files that already had identical bytes
conflicts      0   files that exist with different bytes; never overwritten
relations    700   typed relations attached (446 skipped: pending/orphaned/dangling/duplicate verdicts)
prompts      792   prompt entries imported only with --include-prompts (276 skipped)
```

These are the 2026-11-15 counts for the live engram 3.6 store. A growing source
changes them; the dry run is the authority. After an apply,
`brain index --rebuild` reported 3,761 documents (3,691 imported + 70
quarantined) and no skipped record file, so every generated file parses.

## Notes and limits

- Notes are written as `projects/<id>/notes/engram-YYYY-MM-DD.md`, at most 200
  entries per file (`-2`, `-3`, ... spill files). The prefix avoids a collision
  with a curated daily note, which is never edited.
- The importer writes data schema version 1. The new fields are additive and
  ignored by older engines, but an older engine that edits an imported file can
  drop them; upgrade every machine before editing imported records.
- `superseded_by_sync_id` is not turned into validity dates; a source
  `supersedes` relation is imported as a relation and reported as a warning if
  the source used the validity column.
- Prompt text is the most sensitive engram data. It is opt-in, local-only,
  gitignored and excluded from the index. The prompt zone must already be
  enabled in `brain.yaml`; the importer never turns it on.
- Secrets in imported bodies and prompts pass through the same redactor as
  curated writes. The report shows how many matches were replaced, never their
  values.
- The source database is opened with `mode=ro&_pragma=query_only(1)`. If a live
  WAL blocks the read, close the tool holding it and retry; the importer never
  copies or mutates the source.
