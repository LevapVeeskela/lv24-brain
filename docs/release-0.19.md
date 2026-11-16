# Release 0.19.0 — Python extraction, graph analytics, tags and snooze

Status: implementation complete; not tagged. Engine commits: `fd1f4d2`
(Python extraction and graph analytics polish), `3fe6d9f` (record tags,
proposal snooze, eval CRLF fix). Desktop 1.3: shipped (`82007cb`) with tags prefill and the snooze UI.
Data schema stays `schema_version: 1`; the changes are additive and older
engines keep reading the files, with one index rebuild for Python.

0.19 closes the language gap in the native code graph and polishes its
analytics, then adds two memory features on top of the 0.18 record model:
note tags are visible everywhere a record is listed or read, and proposals
can be snoozed out of the inbox with a compare-and-swap. The eval fixture
digest is made line-ending agnostic.

## Python extraction

The native extractor now covers Python through the same vendored
tree-sitter WASI reactor used for TypeScript/TSX/JavaScript/JSX (grammar ABI
15, `tree-sitter-python` v0.25.0). It is syntax-only: it never imports or
executes a module and never infers a receiver type.

- Declarations: `class` -> `type`, module-level `def` -> `func`, methods
  (including `@property`, `@staticmethod`, `@classmethod`) -> `method`,
  PEP 695 `type` aliases -> `type`, upper-case module/class assignments ->
  `const`, other assignments -> `var`, and the file itself (including
  `__init__.py`) -> `module`.
- Qualified names: `Class.method`, `Outer.Inner.method`. Nested functions are
  local and are not symbols. Decorators stay in the declaration range and
  signature; a docstring becomes `doc`, with a contiguous `#` block as
  fallback. `exported` follows the leading-underscore convention, except
  dunder names such as `__init__`.
- Edges: `imports` (1.0), `calls` (0.6 same file, 0.5 through an imported
  name, 0.4 through an imported module, 0.5 through an imported class name),
  `extends` (0.7 / 0.5), `references` (constructor 0.6 / 0.4, annotations
  0.5 / 0.4). Absolute imports resolve from the repository root; relative
  imports (`.` / `..pkg`) resolve from the importing file's package.
- `__init__.py` and `__main__.py` are indexed even though the Go rule skips
  underscore-prefixed files; that rule now applies only to Go files, and an
  underscore-prefixed directory is kept when it is a Python package (it has
  `__init__.py`). `__pycache__/`, `venv/`, `site-packages/` and `.pyi`
  stubs are not indexed.
- Files with an `ERROR` span drop the intersecting declarations and are
  counted as partial; `unresolved` counts only relative specifiers.
- Stable ids: `sym_<sha256(path + qualname)[:20]>`, so a body edit keeps
  every incoming edge. File-scoped sync re-extracts changed files plus their
  importers and relinks importer edges; a change touching both Go and Python
  escalates to a full rebuild.

Measurements on Python 3.13 stdlib (`Lib/`, Windows 11): 1,760 files ->
76,276 symbols / 84,091 edges, full index 70.2 s, no-change sync 68 ms,
18,990 unresolved call sites (mostly re-exports, C extensions and dynamic
receivers). Peak RSS 48-90 MB; binary growth about 5 MB.

## Graph analytics polish

The analytics half of `brain_graph` is made faster and more complete without
changing its answers:

- **Digest fast path.** `meta.graph_digest` hashes the symbol id set and the
  distinct dependency pairs. A sync whose digest matches the stored partition
  leaves every community id, name, cohesion and signature untouched, so a
  comment-only or body-only edit keeps the partition byte-identical. A digest
  change recomputes and reuses the id of every community whose signature
  still exists.
- **Import cycles.** `cycles` reports strongly connected components of the
  `imports` subgraph only (not recursive calls). The kernel is an iterative
  Tarjan walk with sorted adjacency and deterministic root order; self-imports
  are counted and disclosed separately. Limits: 50 cycles (default 10), 50
  members per cycle (default 20); `total` and `nodes` stay exact.
- **Index diff.** `diff` compares `project` with `base_project`: file, symbol,
  edge and (native) community deltas keyed by id, path and
  `(source, target, kind)`. A line shift or comment is not a change. Up to
  100 samples per side per category (default 20); counts stay exact. The
  kernel merges two ordered key streams. Communities are diffed by signature
  on native; a codegraph partition is computed in memory and not stored, so a
  codegraph diff reports zero community deltas and says so.
- **Codegraph cache.** The in-process partition cache is keyed by database
  path plus size and mtime of `codegraph.db` and its `-wal`; it holds at
  most 2 partitions / 256 MiB. `communities`, `god_nodes` and `stats` carry
  `cached: true` when the partition came from the cache; the first call in a
  process reports `false`.
- **MCP.** `brain_graph` now serves eight actions: `stats`, `neighbors`,
  `communities`, `god_nodes`, `path`, `cycles`, `diff` and `pr_impact`.
  `base_project` is required for `diff` and must differ from `project`.

Measured on the engine checkout (native index: 808 files, 11,639 symbols,
58,321 edges, 153 communities): `stats` 451 ms, `communities` 26 ms,
`cycles` 57 ms, `diff` 141 ms. On the audited `lv24` codegraph index (184,902
nodes, 524,553 edges): cold `stats` 11.5 s, cached `communities` about
90 ms, `cycles` 553 ms, `diff` 1.95 s. Go-only `lv24-brain` is acyclic and
answers `cycles` in about 45 ms.

## Record tags

Notes expose their tags in every read path:

- `brain_record(get)` and `brain_record(list)` include `tags` for note
  records; other kinds omit the field.
- `PATCH /v1/records/{id}` returns the updated `tags` in the detail payload,
  so a client that edits a note sees the canonical tags back.
- MCP `brain_record` mirrors the same field. Tags are additive to the
  existing record schema; no migration is required.

## Proposal snooze

A proposal can be hidden from the review queue until a time:

- `POST /v1/proposals/{project}/{id}/snooze` with `{until, expected_updated_at?}`;
  `until` accepts an RFC3339 time, a calendar date, a positive duration such
  as `7d`, or `null` to clear. Repeating the stored value writes nothing.
- `DELETE /v1/proposals/{project}/{id}/snooze` clears the snooze and is
  idempotent.
- `expected_updated_at` makes the write a compare-and-swap; a stale value
  returns `409` with `error.current_revision`.
- Snoozing a promoted or dismissed proposal is a `400` validation error;
  terminal transitions clear any existing snooze.
- `GET /v1/proposals` hides snoozed candidates by default; `status` filter or
  `include_snoozed=true` keeps them. The review summary gains
  `snoozed_count` (pending or stale candidates hidden by an active snooze).
- CLI: `brain promote --snooze <duration|date>` and `--unsnooze`.
- The inbox payload carries `snoozed_until` and `snoozed` for each candidate.

## Eval

- Fixture digests normalize CRLF to LF before hashing, so the committed
  baseline is valid on both Windows and Linux checkouts.

## Desktop 1.3

Shipped (`lv24-brain-desktop` commit `82007cb`, tag `v1.3.0`): note tags are
prefilled and saved through the record edit form, and the inbox can snooze a
proposal for 7/30 days or a custom date, unsnooze it, and show a snoozed badge
with the remaining time. Review pending counts exclude snoozed candidates and
expose `snoozed_count`. Desktop 1.3 bundles engine 0.19.0; 1.2.0 remains the
previous stable release.

## Migration notes

- Data schema stays `schema_version: 1`; record tags and proposal snooze are
  additive. Older engines keep reading the files.
- The native code graph gained a Python extractor and an extractor version
  bump. An existing store built by 0.18 (Go/TS only) must be rebuilt before
  Python files are indexed: run `brain code index --rebuild` on each
  checkout, or `brain code sync` once the extractor version is accepted (a
  mismatched store refuses reads with "run brain code index --rebuild").
- No migration of proposals or records is required; snooze fields default to
  unset and hidden-by-default means existing inboxes are unchanged.
- MCP hosts must be restarted to see the new `cycles` and `diff` actions; the
  tool list is loaded at startup. `brain_graph` remains eight actions in the
  default profile.
- The `lv24-legacy` bridge profile is still served but deprecated; the seven
  0.16 names remain available only under it.

## Known gaps

- The native extractor covers Go, TypeScript/TSX/JavaScript/JSX and Python.
  Rust and the rest still need the external CodeGraph index or a future
  extractor. Go is type-checked; script and Python edges are syntax-only.
- Python gaps remain explicit: `.pyi` stubs, `src/` layout or custom
  `sys.path`, inherited-method and metaclass resolution, dynamic imports,
  function-local shadowing. Closing them needs configuration or a type
  checker, not another query tweak.
- No import-cycle report for the whole repository beyond the import subgraph,
  and no coupled test-coverage view. Communities, god nodes, paths, PR impact
  and orphan counts are served.
- Quarantine holds 39 records; 311 near duplicates remain in 11 report-only
  clusters. Exact duplicates are 0.
- Some records are past their review date, and some legacy titles still exceed
  200 characters.
- `state.md` has no provenance; `record review` remains local/git only.
- Pin, unpin, consolidation and prompt discard are refused over MCP.
- The `codegraph` index cannot be refreshed through `brain`; use the
  CodeGraph CLI.
- The bridge profile `lv24-legacy` is deprecated and may be removed in a
  later release.
