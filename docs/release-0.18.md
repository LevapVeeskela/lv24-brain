# Release 0.18.0 — code graph, one MCP, durable memory

Status: implementation complete; not tagged. Engine commits: `12412fd` (native
Go code indexer), `908f2f2` (local API memory routes), `be72375` (`brain_code`
and `brain_graph` MCP tools), `6be350a` (graph analytics and TS/TSX
extraction), `711305d` (record edit and desktop API gaps). Desktop 1.2:
`428ae47` (memory browser, inbox, state, palette), `bdb40e4` (record editing,
quarantine, review fix, all-projects). Data:
`b82f6e2` (pre-migration snapshot), `ec4c3d6` (engram import), `710e950`
(post-import state and handoff), `8e9647b` (quarantine and duplicate cleanup).

0.18 closes the "one brain" migration: the MCP server grows from 15 to 17
tools, the engine indexes its own Go and TypeScript/TSX/JavaScript/JSX code
graph, the desktop app gains a full memory UI, and the owner's engram store is
imported, cleaned up and retired. The record schema stays `schema_version: 1`;
the changes are additive and older engines keep reading the files.

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
older) handshakes from one process. That protocol surface was already
documented for 0.15–0.17; 0.18 adds the two code tools.

## Native code graph (Go and scripts)

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
- Script extractor: TypeScript (`.ts`), TSX (`.tsx`), JavaScript (`.js`) and
  JSX (`.jsx`) are parsed by a single embedded tree-sitter WASI reactor run by
  `wazero` (pure Go, no CGO, no runtime download). The asset is 444,436 bytes
  gzip / 3,851,887 bytes raw and its SHA-256 is verified at startup. Script
  symbols use the same store: `module`, `type`, `func`, `method`, `const`,
  `var`; edges are syntax-only (`imports` 1.0, calls 0.4–0.6, heritage
  0.5–0.7, references 0.4–0.6). A 160-file desktop checkout indexed in 1.24 s
  (872 symbols, 1,093 edges). Python and Rust are not claimed; the shared
  reactor carries an unused Python grammar until that extractor lands.
- No function bodies are stored. A row keeps the line range, declaration
  signature, doc comment and package; source is re-read from the checkout at
  query time.
- One SQLite file per checkout under `<data-dir>/index/graph/`, schema 2,
  WAL mode. A schema or extractor change requires `brain code index --rebuild`.
- `brain code sync` re-extracts only changed packages and script files. A body
  edit keeps every symbol id; removing or renaming a symbol escalates to a full
  rebuild so incoming edges never point at a dead id.
- Commands: `index`, `sync`, `status`, `search`, `node`, `explore`, `impact`.
  `explore` is capped at 24 KiB; `impact` walks at most 10 hops and 500 nodes.
- Measurements on the engine checkout: 765 Go files → 11 189 symbols / 55 840
  edges, 21 MiB, 5–8 s warm full index, under 1 s for a no-change sync.

MCP `brain_code` serves the same engine with actions `status`, `search`,
`node`, `explore`, `impact`, `index` and `sync`. A missing engine, absent index,
empty index or foreign schema is a typed `UNSUPPORTED` with the command that
fixes it.

## Graph analytics

`brain_graph` serves all six read-only actions over the same store. `stats`
returns the index envelope plus analytics, and `neighbors` returns one symbol
with both edge directions.

- **`communities`** — deterministic Louvain over the undirected dependency
  graph, with the top 5% of hubs reattached on graphs of at least 500 nodes.
  A listing returns at most 100 communities; one selected community returns at
  most 200 members with cohesion and a stable signature. The native engine
  stores the partition; the codegraph adapter computes and caches it in memory.
- **`god_nodes`** — definitions ranked by distinct dependency relationships in
  both directions, at most 50 (default 10). `centrality` is a degree proxy, not
  betweenness; test definitions are marked.
- **`path`** — breadth-first search between two symbols, outgoing first with an
  undirected fallback. Default 8 hops, maximum 16, at most 20 nodes; each hop
  carries its edge, line, confidence and reverse marker.
- **`pr_impact`** — `git diff` between two local refs, changed files to indexed
  definitions, then a depth-2 reverse blast radius. Risk is high for a deleted
  file, ≥50 reachable or ≥25 changed definitions; medium for ≥10 reachable or
  ≥5 changed definitions. Caps are 200 changed files and 500 changed plus 500
  reachable definitions. It fetches nothing and stores no pull-request data.
- **`stats`** now adds an `analytics` block: files, symbols, edges,
  communities, orphans, languages and node kinds.

Both engines use the same deterministic kernel (`internal/adapters/graphalgo`),
so native and codegraph answers on the same edges choose the same communities
and paths. Caps set `truncated` and add a note; they are never silent.

## Codegraph adapter

The external CodeGraph index remains readable for multi-language repositories:

- read-only (`mode=ro`, `PRAGMA query_only=1`); `Sync` reports `unsupported`;
- schema gate: CodeGraph schema 11, extraction 27, CLI ≥ 1.6.2;
- the same query and analytics surface as the native engine, so both can be
  compared on one repository;
- if a live writer holds the WAL, the adapter retries with `immutable=1` and
  discloses that the answer reflects the last checkpoint;
- `brain code index` cannot write it; use the CodeGraph CLI.

The audited index holds 184 785 nodes / 523 853 edges. The native engine is the
fast path; the adapter remains the bridge for languages the native extractor
does not cover (Python, Rust, and the rest).

## Desktop 1.2 memory UI

Desktop 1.2 requires engine 0.18 and consumes the frozen local API memory
routes:

| Screen | Content |
|---|---|
| `/memory` | project or all-projects filters, paginated table, preview pane |
| `/memory/:kind/:id?project=` | detail with body, provenance, links/backlinks, relations with target-kind deep links, validity, history, inline edit with CAS and trash |
| `/inbox` | proposals with confidence/source health and bounded body preview, accept/dismiss/title edit; safe-fix dry run/apply; review findings and due reviews |
| `/inbox/proposal/:project/:id` | read-only proposal body (2 000-token bound) and truncation notice |
| `/quarantine` | quarantine inbox: reason, origin, trust, age, project filter, review with trust choice |
| `/projects/:projectId` | editable state with compare-and-swap save, latest handoff, recent records |
| `/search` | ranked results, filters, explain data, all-projects toggle, cursor paging |
| `/activity` | runs plus catchup memory events, grouped by day and project; run rows show `project_ids` |
| command palette | `Cmd/Ctrl+K` record search and navigation |

The left rail owns the project selector and the pending-proposal badge. Writes
are explicit: proposal promotion/dismissal, project-state saves with
`expected_revision`, record text edit (`title`/`body`/tags for notes, with
`expected_updated_at`), reversible trash/restore, quarantine review, and the
safe `review --fix` set after a dry-run preview. Destructive actions confirm,
and a trashed record stays recoverable from the Memory page's undo banner.
Decision status changes and supersede links remain CLI actions. Proposal
snooze is not exposed, and run `actor` stays omitted because runs do not
persist a trigger. `npm run check` is green on 1.2.0: typecheck, 199 tests,
production build.

## Engram migration (applied)

The dry run was approved and applied on 2026-11-16. The import counts below
are the state at import time; a later curation pass changed the live totals.

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

- at import time the index reported 8 878 documents / 4 256 records / 46
  projects;
- the journal was repaired for 289 records;
- the engram MCP server is disabled in OpenCode and Claude;
- the engram database is untouched and stays read-only for a week.

A rerun is idempotent: identity is `(source.system = engram,
source.external_id)`, so unchanged files are reported and not rewritten. The
importer never merges, overwrites or deletes a curated record; conflicts are
reported.

A cleanup pass followed the import (data commit `8e9647b`): 31 junk records
were trashed, 153 exact duplicate records were merged into 5 survivors, and
the quarantine fell from 70 to 39 records. Exact duplicates are now 0; 311
near duplicates remain in 11 report-only clusters. The store holds 4 086
records, down from 4 270 before cleanup.

## Visible changes

- MCP hosts see 17 tools after a restart (the tool list is loaded at startup).
  The seven 0.16 names still work only under `--profile=lv24-legacy`.
- `brain_graph` serves all six actions: stats, neighbours, communities, god
  nodes, paths and local-diff PR impact. `brain_code` needs `graph.engine` or
  `--engine`.
- The native code graph indexes Go and TypeScript/TSX/JavaScript/JSX. Go edges
  are type-checked; script edges are syntax-only and carry lower confidence.
- Code graph indexes live under the brain data directory, never in a checkout,
  and can be deleted and rebuilt.
- `brain record edit` and `PATCH /v1/records/{id}` edit record text in place
  with an optional `expected_updated_at` compare-and-swap. Desktop 1.2 uses
  them, and adds trash/restore, a quarantine page, safe review fixes,
  proposal bodies, an all-projects scope and run `project_ids`.
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
   then `brain code status`. Go and TypeScript/TSX/JavaScript/JSX files are
   indexed; the engine stays `off` until configured.
6. Upgrade Desktop to 1.2.0 with engine 0.18; the app bundles a pinned engine,
   so the CLI installation does not have to be replaced.
7. Engram: the import is already applied on this machine. Keep the engram MCP
   disabled, keep the database read-only for a week, and do not point an older
   engine at the imported files if it may edit them.
8. Review the 39 remaining quarantined records and the 311 near duplicates in
   11 clusters (`brain review`, `brain duplicates`) before deleting the engram
   backup. Exact duplicates are 0; they were merged into curated survivors.

## Known gaps

- The native extractor covers Go and TypeScript/TSX/JavaScript/JSX. Python,
  Rust and the rest still need the external CodeGraph index or a future
  extractor. Go is type-checked; script edges are syntax-only.
- One module or checkout per store, no daemon or file watcher. A nested
  `go.mod` or nested Git checkout below the root is skipped.
- Quarantine holds 39 records. Duplicates are 0 exact and 311 near in 11
  clusters, report-only. The store holds 4 086 records.
- Some records are past their review date, and some legacy titles still exceed
  200 characters.
- Desktop 1.2 does not expose proposal snooze; run `actor` stays omitted
  because runs do not persist a trigger. Decision status changes and supersede
  links remain CLI actions.
- `state.md` has no provenance; `record review` is local/git only.
- Pin, unpin, consolidation and prompt discard are refused over MCP.
- The `codegraph` index cannot be refreshed through `brain`; use the CodeGraph
  CLI. `code://` resources exist only when a backend serves `Node`.
