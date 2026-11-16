# Release 0.18.0 — code graph, one MCP, durable memory

Status: implementation complete; not tagged. Engine commits: `12412fd` (native
Go code indexer), `908f2f2` (local API memory routes), `be72375` (`brain_code`
and `brain_graph` MCP tools). Desktop 1.2: `428ae47` on `feat/1.2-memory`.
Data: `b82f6e2` (pre-migration snapshot), `ec4c3d6` (engram import), `710e950`
(post-import state and handoff).

0.18 closes the "one brain" migration: the MCP server grows from 15 to 17
tools, the engine indexes its own Go code graph, the desktop app gains a memory
UI, and the owner's engram store is imported and retired. The record schema
stays `schema_version: 1`; the changes are additive and older engines keep
reading the files.

This note summarizes 0.16–0.18. Detailed contracts remain in the per-release
notes: [release-0.16.md](release-0.16.md), [release-0.17.md](release-0.17.md),
[release-0.15.md](release-0.15.md).

## Memory quality (0.16)

- **Temporal validity.** Records carry `valid_from`, `valid_to`,
  `superseded_by`, `supersedes` and `confidence`. `brain record supersede` ends
  the old record and links both sides; every read path hides ended records by
  default, `--as-of` returns the generation that was current at an instant, and
  `--include-superseded` keeps history.
- **Search v2.** One composite score (`0.55 bm25 + 0.20 recency + 0.15
  importance + 0.05 pin + 0.05 link`), mapped to `[0, 1)`. Hits carry only the
  `match_reasons` that fired; `--explain` shows weighted contributions.
  `--match-mode`, `--relaxed`, `--min-score` and `--full` control the answer.
- **Progressive disclosure.** `brain search` returns previews, `brain get`
  reads one record under a token budget, `brain timeline` shows the neighbours
  in time. MCP `brain_search` matches the same three steps with `expand_ids`.
- **Layered context.** `brain context` packs `core`, `decisions`, `attempts`,
  `query` and `linked` under a 6 000-token default (32 000 maximum). A record is
  never split; a record that does not fit is skipped whole and named in
  `truncated[]`.
- **Review v2.** Every finding carries `severity`, `why`, `next`, `confirm`,
  `evidence` and `fixable`/`fix`. `review_after` is a dated prompt to re-check,
  never an expiry; `brain review --due` lists what is due.
- **Deterministic dedup.** An exact duplicate is refused at write time with the
  existing id; near duplicates are report-only clusters. Nothing is merged
  automatically.
- **Relations and judging.** A closed verb set (`relates_to`, `supersedes`,
  `conflicts_with`, `compatible`, `scoped`, `depends_on`, `implements`,
  `blocks`, `evidences`, `contradicts`, `not_conflict`), a status (`pending`,
  `accepted`, `rejected`), a required reason and the judge. Candidate detection
  writes only pending `relates_to` edges; judging is an explicit call.
- **Two-phase merge.** `brain record merge` plans without writing, publishes a
  manifest under `<data-dir>/merges/`, rewrites references, keeps tombstones in
  the trash, and rolls back byte for byte. Merge stays CLI-only.

## One MCP surface: 15 → 17 tools (0.17–0.18)

0.17 replaced 19 narrow tools with 15 intent tools. 0.18 adds `brain_code` and
`brain_graph`:

| Group | Tools |
|---|---|
| Read (9) | `brain_open`, `brain_context`, `brain_search`, `brain_record`, `brain_catchup`, `brain_review`, `brain_projects`, `brain_graph`, `brain_capabilities` |
| Append (4) | `brain_note`, `brain_decision`, `brain_attempt`, `brain_handoff` |
| Mixed (1) | `brain_code` — five read actions plus idempotent local `index`/`sync` |
| Destructive (3) | `brain_state`, `brain_judge`, `brain_prompt` |

- Read-only servers list ten tools: the nine read tools plus `brain_code` for
  its read actions; `index`/`sync` are refused before the engine opens.
- `brain_record` carries `get|list|relations|history|links`; `brain_catchup`
  carries `changes|journal|timeline`; `brain_judge` carries the 11 lifecycle,
  review and relation actions.
- Typed inputs, typed refusals (`code`, `try_instead`, `current_revision`,
  `retryable`), per-action cursors bound to the query, and explicit
  `UNSUPPORTED` refusals replace silent no-ops.

Compatibility profiles are exclusive vocabularies: `lv24` (17, default),
`lv24-legacy` (24, the seven 0.16 names as a bridge removed in 0.19), `engram`
(28: 27 aliases plus `brain_capabilities`), `codegraph` (9) and `graphify`
(11). `--strict` turns approximate mappings into refusals; an unknown profile
or version exits 2.

Resources are read-only, project-scoped and capped: `memory://projects`,
`memory://project/{id}/state`, `memory://record/{id}`, `memory://review` and,
when a graph backend is configured, `code://{project}/symbol/{id}`. Every
resource uses `cacheScope: private`; quarantined records stay hidden unless
explicitly requested.

The server speaks `2026-07-28` (stateless requests, `server/discover`,
`resultType`, cache hints, multi-round-trip elicitation) and `2025-06-18` (and
older) handshakes from one process. Both surfaces were already documented for
0.15–0.17; 0.18 only adds the two code tools.

## Native Go code graph

`brain code` answers symbol questions from a derived SQLite graph instead of a
memory search. The native engine is off by default and enabled per workspace:

```yaml
graph:
  engine: native                 # off (default) | native | codegraph
```

- Extractor: `go/packages` with `Tests: true`, `go/ast` and `go/types`.
  Symbols are `package`, `type`, `func`, `method`, `const`, `var`; edges are
  `imports`, `calls`, `implements`, `embeds`, `references`, with `types` (1.0)
  or `syntax` (0.8/0.5) provenance.
- No function bodies are stored. A row keeps the line range, declaration
  signature, doc comment and package; source is re-read from the checkout at
  query time.
- One SQLite file per checkout under `<data-dir>/index/graph/`, schema 2,
  WAL mode. A schema or extractor change requires `brain code index --rebuild`.
- `brain code sync` re-extracts only changed packages. A body edit keeps every
  symbol id; removing or renaming a symbol escalates to a full rebuild so
  incoming edges never point at a dead id.
- Commands: `index`, `sync`, `status`, `search`, `node`, `explore`, `impact`.
  `explore` is capped at 24 KiB; `impact` walks at most 10 hops and 500 nodes.
- Measurements on the engine checkout: 749 files → 10 788 symbols / 53 480
  edges, 20.8 MiB, 4.9 s full index, under 1 s for a no-change sync.

MCP `brain_code` serves the same engine with actions `status`, `search`,
`node`, `explore`, `impact`, `index` and `sync`. A missing engine, absent index,
empty index or foreign schema is a typed `UNSUPPORTED` with the command that
fixes it.

`brain_graph` serves `stats` and `neighbors`. The analytics actions (`path`,
`communities`, `god_nodes`, `pr_impact`) are listed so a caller gets a typed
refusal, but they are **not implemented in 0.18.0**; they are the next slice.

## Codegraph adapter

The external CodeGraph index remains readable for multi-language repositories:

- read-only (`mode=ro`, `PRAGMA query_only=1`); `Sync` reports `unsupported`;
- schema gate: CodeGraph schema 11, extraction 27, CLI ≥ 1.6.2;
- the same query surface as the native engine, so both can be compared on one
  repository;
- if a live writer holds the WAL, the adapter retries with `immutable=1` and
  discloses that the answer reflects the last checkpoint;
- `brain code index` cannot write it; use the CodeGraph CLI.

The audited index holds 8 792 files / 181 409 nodes / 514 612 edges. The native
Go engine is the fast path; the adapter is the multi-language bridge until
tree-sitter extractors land.

## Desktop 1.2 memory UI

Desktop 1.2 requires engine 0.18 and consumes the frozen local API memory
routes:

| Screen | Content |
|---|---|
| `/memory` | project-scoped filters, paginated record table, preview pane |
| `/memory/:kind/:id` | read-only detail: body, provenance, links/backlinks, relations, validity, history |
| `/inbox` | proposals with confidence/source health, accept/dismiss/title edit; review findings and due reviews |
| `/projects/:projectId` | editable state with compare-and-swap save, latest handoff, recent records |
| `/search` | ranked results, filters, explain data, cursor paging |
| `/activity` | runs plus catchup memory events, grouped by day and project |
| command palette | `Cmd/Ctrl+K` record search and navigation |

The left rail owns the project selector and the pending-proposal badge. The UI
is read-only except proposal promotion/dismissal and project-state saves.
Generic record editing, review-fix execution, proposal snooze, an all-projects
scope in the UI, the quarantine list and run actor/project metadata remain
gaps. `npm run check` is green: typecheck, 165 tests, production build.

## Engram migration (applied)

The dry run was approved and applied on 2026-11-16:

| Count | Value |
|---|---|
| records imported | 3 693 |
| files created | 1 075 |
| collapsed | 783 (summaries and duplicate sync ids) |
| quarantined | 70 records (127 source rows) |
| relations | 703 |
| secrets redacted | 33 |
| failed | 0 |

The source database is opened read-only and was not modified. A backup tag
`pre-engram-migration-20261116-052417` and the manifest under
`backups/migrate-20261116T022654Z/` allow a byte-identical rollback. After the
import:

- the index reports 8 878 documents / 4 256 records / 46 projects;
- the journal was repaired for 289 records;
- the engram MCP server is disabled in OpenCode and Claude;
- the engram database is untouched and stays read-only for a week.

A rerun is idempotent: identity is `(source.system = engram,
source.external_id)`, so unchanged files are reported and not rewritten. The
importer never merges, overwrites or deletes a curated record; conflicts are
reported.

## Visible changes

- MCP hosts see 17 tools after a restart (the tool list is loaded at startup).
  The seven 0.16 names still work only under `--profile=lv24-legacy`.
- `brain_graph` accepts six action names; four return `UNSUPPORTED` until the
  analytics slice. `brain_code` needs `graph.engine` or `--engine`.
- Code graph indexes live under the brain data directory, never in a checkout,
  and can be deleted and rebuilt.
- Desktop 1.2 adds a memory browser, record detail, inbox, project state,
  search page, command palette and Activity v2. Record editing stays a CLI or
  Markdown action.
- Engram is no longer a live MCP source; imported records are ordinary brain
  records with `origin: import`, `trust: stated` and an `engram:` external id.
- Pin, unpin, consolidation and prompt discard still refuse with a CLI path.
- `state.md` still has no provenance; `record review` remains local/git only.

## Migration checklist

1. Upgrade the engine on every machine; data schema stays 1.
2. Run `brain doctor` and fix what it reports. The search index rebuilds
   itself when its schema version changes; `brain index --rebuild` is the
   explicit path.
3. Restart every MCP host and diff the contract:
   `brain mcp --list-tools --json`. Expect 17 names in the default profile.
4. Rewrite retired calls with the 0.16 → 0.17 table in [mcp.md](mcp.md) or run
   with `--profile=lv24-legacy` until 0.19.
5. Optional: enable the native graph per workspace, run `brain code index`,
   then `brain code status`. The engine stays `off` until configured.
6. Upgrade Desktop to 1.2.0 with engine 0.18; the app bundles a pinned engine,
   so the CLI installation does not have to be replaced.
7. Engram: the import is already applied on this machine. Keep the engram MCP
   disabled, keep the database read-only for a week, and do not point an older
   engine at the imported files if it may edit them.
8. Review the 70 quarantined records and the duplicate clusters
   (`brain review`, `brain duplicates`) before deleting the engram backup.

## Known gaps

- Graph analytics (`path`, `communities`, `god_nodes`, `pr_impact`) and
  multi-language extractors (tree-sitter) are not implemented.
- The native extractor understands Go modules only; one module and one
  checkout per store, no daemon or file watcher.
- Quarantine still holds 70 records, including junk `--project`/`--title`
  imports; duplicates are 173 exact and 242 near in 20 clusters, report-only.
- 39 records are past their review date; 6 legacy titles exceed 200 characters.
- Desktop 1.2 does not expose an all-projects scope, record editing, review-fix
  execution, proposal snooze, the quarantine list or run actor/project
  metadata.
- `state.md` has no provenance; `record review` is local/git only.
- Pin, unpin, consolidation and prompt discard are refused over MCP.
- The `codegraph` index cannot be refreshed through `brain`; use the CodeGraph
  CLI. `code://` resources exist only when a backend serves `Node`.
