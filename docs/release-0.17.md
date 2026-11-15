# Release 0.17.0 — one MCP surface, engram-compatible memory

Status: implementation complete; not tagged. Commits: `9171627` (engram-compatible
schema and prompt zone), `8aae610` (one MCP surface and engram importer).

0.17 makes one brain replace engram and code-graph tooling for the owner's
agents: the record schema grows the fields engram writes, an importer moves an
existing engram store in with a dry run and byte-identical rollback, and the MCP
server serves 15 intent tools instead of 19 narrow ones. Data schema stays
`schema_version: 1` — every new field is optional and omitted at its default.

## Schema: subtype, scope, topic_key

`Note`, `Decision`, `Attempt` and `Handoff` share four optional classification
fields:

| Key | Type | Default | Meaning |
|---|---|---|---|
| `subtype` | string, ≤ 120 chars | empty | Free-form subtype; the engram observation type maps here (`architecture`, `bugfix`, `discovery`, `session_summary`, `policy`, ...). |
| `scope` | `project` \| `personal` \| `global` | `project` | Where the record applies. Omitted at the default, so a legacy record is project-scoped. |
| `topic_key` | string, ≤ 120 chars, lowercase | empty | Stable id of an evolving topic. Empty means append-only. |
| `occurrences` | integer ≥ 1 | `1` | How many topic writes the record represents; written only above one. |

`source.external_id` may carry an import identity (for example
`engram:<sync_id>`); handoffs now have `source` too.

Scope is a classification, not an access-control boundary: a scope filter
narrows a result set and never grants or denies access to a project.

`brain search`, `brain record list` and `brain context` accept `--subtype`,
`--scope` and `--topic`. Omitting them keeps the previous behaviour. The search
index stores the fields as derived columns (schema version 6) with indexes on
`(project_id, subtype)`, `(project_id, scope)` and
`(project_id, kind, topic_key)`. Ranking, context packing, review, dedup and
relation logic are unchanged. See [schema-compat.md](schema-compat.md).

## Topic upsert and `## History`

A write with `--topic` and no explicit id updates one record instead of
appending a copy:

```bash
brain note --project lv24-brain --topic architecture/auth-model --body "..."
```

- Lookup is by `(project, kind, topic_key)`; the oldest match is updated.
- The stored body is prepended to a `## History` section, newest first, bounded
  to 20 entries. The current body is never history.
- `occurrences` increments, `updated_at` moves, and the record id, `created_at`
  and provenance (`origin`, `reviewed`, `trust`, `source`) are preserved.
- Validity, supersede links, typed relations and decision status are preserved:
  an upsert changes text and classification only.
- An identical body is a no-op reported as `(unchanged)`; nothing is written.
- Concurrent upserts of one topic are serialized per `(project, kind,
  topic_key)` across processes, so two agents cannot both create the first
  record. An upsert that finds no record creates one under the same lock.

An explicit id or a `--supersedes`/`--superseded-by` link is always a normal
create, so history is never rewritten silently.

## Prompt zone (opt-in, local-only)

Prompts are the most sensitive text the tool can see, so the zone is disabled by
default.

```yaml
prompts:
  enabled: true
  retention_days: 30   # omit for 30; 0 keeps forever
```

- Prompts live in `projects/<id>/prompts/YYYY-MM-DD.md` with
  `visibility: local`; a `prompts/.gitignore` containing `*` is written and
  repaired on every save, so `git add --all` cannot publish one.
- `CurrentRevision` skips the prompt directory: saving a prompt does not change
  the data revision and cannot make a sync commit it.
- Prompts are not records: they are not listed, not indexed and never returned
  by search, context, bootstrap or MCP tools.
- `brain prompt save|list` refuse with a validation error while
  `prompts.enabled` is false. `brain doctor` reports whether the zone is on,
  that it is local-only, the retention window and how many prompts are older
  than it — retention is a report, nothing is deleted.
- The zone exists in local and git mode only; server mode has no prompt store.

## Engram importer

`brain migrate --from engram` copies an engram store (SQLite or `engram export`
JSON) into the Markdown tree. The source is opened
`mode=ro&_pragma=query_only(1)` and never mutated; every write goes through the
migration path with a manifest and `--rollback`.

Dry run on the live 3.6 store (2026-11-15), which writes nothing and proves the
target tree hash is unchanged:

| Count | Value |
|---|---|
| rows read | 4545 |
| imported records | 3691 |
| collapsed | 783 |
| quarantined | 70 records (127 source rows before collapse) |
| skipped | 1 (`bats`) |
| created files | 1073 (notes grouped by project/day) |
| relations | 700 (446 skipped: pending/orphaned/dangling/duplicate verdicts) |
| prompts | 792 with `--include-prompts` (276 skipped) |
| redacted | 33 secret matches replaced, values never logged |

- `observations` map to decisions (type `decision`/`policy`), notes
  (`session_summary` and everything else, subtype preserved);
  `observation_versions` and older summary bodies go to `## History` bounded to
  20; `memory_relations` become typed relations only when judged and
  same-project; `sessions` contribute `source.session` provenance only.
- Imported records are `origin: import`, `trust: stated`,
  `source.external_id: "engram:<sync_id>"`. Quarantined records are also
  `reviewed: false`, so they stay out of trusted context until reviewed.
- Identity is `(source.system = engram, source.external_id)`, so a rerun reports
  every file as `unchanged`. Target ids are deterministic
  (`note-<date>-<sha1(external_id)[:8]>`).
- Only `session_summary` and duplicate `sync_id` rows collapse automatically.
  Every other near and cross-source duplicate is report-only: the importer never
  merges, overwrites or deletes a curated record. A target file with different
  bytes is a conflict and is never overwritten.
- `--rollback latest` deletes the files the run created, restores the previous
  `registry.json` and config bytes, and leaves files edited after the migration
  in place (reported, exit 1). Every apply — including a verification rerun —
  writes its own manifest, so pass the import's manifest path to undo the import
  itself after a rerun.
- A rollback does not delete the derived index; rebuild or remove `index/`.
- Project remap: `lv`/`lv24` → `lv24`, `main` attributed through session
  directories, `lv24-brotato` → `lv24-harvest`, path-named projects attributed
  by their own prefix; NULL, `\`, `unknown` and `.github` go to the quarantine
  project `engram-unmapped`. `--project-map` overrides the defaults.

See [import-engram.md](import-engram.md).

## 15-tool MCP surface

One tool per intent, with actions where a resource has several verbs:
`brain_open`, `brain_context`, `brain_search`, `brain_record`
(`get|list|relations|history|links`), `brain_note` (`record|suggest_topic`),
`brain_decision`, `brain_attempt`, `brain_handoff`,
`brain_state` (`get|set`), `brain_catchup`
(`changes|journal|timeline`), `brain_review` (`findings|due|doctor`),
`brain_judge` (11 actions), `brain_projects`, `brain_prompt`, and
`brain_capabilities`.

- Write posture is exact: **8 read, 4 append-only, 3 destructive**
  (`brain_state`, `brain_judge`, `brain_prompt`). A read-only server lists eight
  tools and refuses the write actions of a mixed tool before the backend.
- `brain_capabilities` reports profile, engine, tools, features, limits, gaps,
  refusal codes and endpoints, so a host can discover what is served instead of
  guessing.
- Pagination is per action with opaque cursors bound to the query that issued
  them; a cursor from another query is refused instead of silently mixing
  result sets.
- Unsupported actions are explicit `UNSUPPORTED` refusals, never faked.

The seven retired names return `NOT_FOUND` with the replacement in
`try_instead`: `brain_state_get`, `brain_state_set`, `brain_journal`,
`brain_records`, `brain_relations`, `brain_review_record`,
`brain_decision_status`. The full mapping is in [mcp.md](mcp.md#11-migration-from-016).

## Compatibility profiles

`--profile` selects an exclusive vocabulary instead of adding names to the
native 15:

| Profile | Listed tools | Notes |
|---|---:|---|
| `lv24` (default) | 15 | 8 read / 4 append / 3 destructive |
| `lv24-legacy` | 22 | 15 native + 7 bridge aliases; deprecated metadata |
| `engram` | 28 | 27 aliases + `brain_capabilities`; A 3 / B 15 / C 7 / D 2 |
| `codegraph` | 9 | honest stubs until 0.18; queries return `UNSUPPORTED` |
| `graphify` | 11 | honest stubs until 0.18; no fake data |

`--strict` turns every approximate (C) mapping into an explicit refusal; an
unknown profile or version exits 2. `brain mcp --list-tools --json` reports the
profile, per-tool grade/target, aliases and a contract digest. A/B mappings are
argument-translated; D mappings (`mem_delete`, `mem_merge_projects`) refuse with
a CLI remediation.

Read-only resources are served over `memory://`: projects, project state, one
record and the review queue, with `cacheScope:private`, size caps, allowlist
enforcement and quarantine hidden unless explicitly requested. `code://` is not
advertised until the native code index exists.

## Migration for clients

1. Upgrade the engine everywhere. Data schema stays 1; an older engine can still
   read new files but drops unknown fields when it edits one.
2. Restart every MCP host (MCP loads the tool list at startup) and diff the new
   contract with `brain mcp --list-tools --json`; expect 15 tools.
3. Rewrite client calls with the rename table in
   [mcp.md](mcp.md#11-migration-from-016). One argument change is refused
   rather than ignored: `brain_decision(status=...)` must become
   `brain_judge(action=accept|reject|supersede)`. Retired tool names are no
   longer valid names at all.
4. `brain index --rebuild` to project the new columns and indexes; `brain doctor`
   afterwards.
5. Engram import, in this order: stop engram writes, back up the store and the
   data directory, `brain migrate --from engram --db <path> --dry-run`, read the
   counts, then apply, then `brain migrate --rollback latest` if anything is
   wrong. Keep engram disabled for a week before removing it.
6. Prompt capture: only if wanted, set `prompts.enabled: true` first; the
   importer never turns the zone on.

## Known gaps

- `codegraph` and `graphify` profiles are honest stubs until 0.18; their query
  tools return `UNSUPPORTED`, and `code://` resources are not advertised.
- `state.md` has no provenance; `brain_state_set` text is trusted.
- `record review` is local/git only; server mode has no provenance-edit route.
- Relations are stored and indexed but not yet used for neighbour navigation.
- Merge stays CLI-only; no MCP tool applies, trashes or hard-deletes records.
- Pin, unpin, consolidation and prompt discard are refused with guidance.
- MCP resources and prompts are not advertised; the 15 tools are the contract.
- No code graph or native code indexer (0.18), no embeddings, no desktop memory
  UI.
- Imported `superseded_by_sync_id` is not converted to validity dates; a source
  `supersedes` is imported as a relation and reported as a warning.
