# Code graph

`brain code` answers symbol questions from a code-graph index instead of a
memory search: where a name is defined, what calls it, how two symbols connect,
and what a change can reach. This page documents the first 0.18 slice: the
`ports.GraphIndex` contract and the read-only CodeGraph adapter.

The binary does not index code yet. A native Go indexer is the next work
package; until then the adapter reads an index that the CodeGraph CLI already
built, in place.

## Enable it

The code graph is off by default. Add a block to `brain.yaml`:

```yaml
graph:
  engine: codegraph      # off (default) | codegraph
  path: .codegraph/codegraph.db   # optional; relative to the checkout
```

`graph.path` may name the database file or the directory that contains
`.codegraph/codegraph.db`. Without it, the adapter looks for
`<checkout>/.codegraph/codegraph.db`.

The checkout comes from the project registry: the selector path of
`--project`, then the working directory, then the project's first registered
local path. `--root` overrides it, `--db` overrides the index path, and
`--engine codegraph|off` overrides the engine for one command.

An unconfigured or unknown engine is an error, not a silent fallback:

```
brain code status: the code graph is off; set graph.engine: codegraph in brain.yaml or pass --engine codegraph
brain code status: no codegraph index at C:\repo\.codegraph\codegraph.db; create one with `codegraph index` ...
```

## Command surface

| Command | Answers | Main flags |
| --- | --- | --- |
| `brain code status` | schema, versions, counts, index age, commit, dirty worktree | `--json` |
| `brain code search <query>` | where a name is defined | `--kind` (repeatable), `--file`, `--limit` (1–100, default 10) |
| `brain code node <symbol>` | one definition, its source, direct callers and callees | `--file`, `--code`, `--callers=false`, `--callees=false`, `--limit` |
| `brain code explore <query>` | source of the matching symbols plus flow and blast radius | `--max-files` (1–24, default 12) |
| `brain code impact <symbol>` | reverse blast radius, test files marked | `--file`, `--depth` (1–10, default 2) |

Every command accepts `--project`, `--engine`, `--db`, `--root`, `--json` and
`--config`. Text output is for a human; `--json` prints the typed DTOs of
`ports.GraphIndex` inside the usual `{schema, data}` envelope.

### Paths and line anchors

`explore` understands the anchors agents already write:

```
brain code explore "internal/app/search.go:120-180 why is recall low"
brain code explore "src/auth.ts#L88 token refresh"
```

A pinned path is removed from the text query, its definitions are added with
`match: "path"`, and its source is ranked first. A line window narrows which
definitions of that file are shown; when none overlaps the window the file's
definitions are shown with a note. A pin outside the checkout is reported, not
followed.

## Retrieval tiers and confidence

Search stops at the first tier that answers, and every hit says which tier it
came from. An exact indexed name is the only tier that proves identity.

| `match` | Source | `confidence` | Meaning |
| --- | --- | --- | --- |
| `exact` | `nodes.name` / `nodes.qualified_name` (`lower()` index) | 1.0 | the indexed name equals the query |
| `fts` | FTS5 over name, qualified name, signature, docstring | 0.8 | a full-text candidate; verify the definition |
| `like` | `%token%` substring on name and qualified name | 0.5 | last resort for a mistyped or partial name |
| `path` | every definition of a pinned file | 1.0 | the path, not the words, proved identity |

`confidence` is identity confidence, not relevance; `score` orders hits inside
a tier. An ambiguous bare name is never resolved by picking the first row:
`node` and `impact` return the candidate list and require `--file`.

## `explore` budget and shape

One answer is capped at **24 KiB** (`MaxGraphExploreBytes`). The adapter
allocates the source budget proportionally to rank, gives each file at least a
small allowance, and returns unused share to the pool. When a file is clipped,
the answer says so, adds a pointer, and keeps the line numbers, so a gap never
looks like contiguous code.

The answer contains, in priority order:

1. `symbols` — ranked hits with the tier that produced them;
2. `files` — numbered source of the matched symbol ranges, gap markers between
   ranges, per-file clip notes;
3. `flow` — one call chain of named symbols (functions, methods, types,
   routes, components), at most 7 nodes, preferring symbols that matched the
   query. It is a v1 flow, not a full call graph;
4. `blast_radius` — the nearest 16 reverse hits across at most 8 files, with
   test hits marked;
5. `pointers` — matching files that did not fit and how to read them;
6. `notes` — weak-tier, ambiguity, stale-source and truncation disclosures.

Generated bundles, `dist/`, `vendor/`, `node_modules/` and localization output
are down-weighted; test files are down-weighted but never hidden.

## `impact` semantics

`impact` is a breadth-first reverse walk over `calls`, `references`,
`instantiates`, `extends`, `implements`, `imports`, `navigates`, `overrides`
and `decorates`. `contains` is excluded: file ownership would make every symbol
in a file look like a dependent.

- depth is clamped to **10**; the answer reports the clamp;
- each hit carries its shallowest distance, the edge kind that reached it, the
  call site line, edge confidence and provenance (`exact-match`, `import`,
  `heuristic`, …);
- file nodes are traversed but not listed as hits: their path joins `files`
  instead of pretending a whole file is a changed definition;
- the walk stops at **500** nodes and sets `truncated`;
- test files are detected by suffix and directory (`_test.go`, `.test.ts`,
  `.spec.tsx`, `_test.py`, `__tests__/`, `tests/`, `e2e/`, …) and listed in
  `tests`;
- hops resolved below 0.7 confidence are counted in `notes`.

## What the adapter guarantees

- **Read-only.** The database is opened with SQLite `mode=ro` and
  `PRAGMA query_only=1`; there is no write statement anywhere in the package.
  If a live writer holds the WAL and the read-only open reports `BUSY`, the
  adapter retries with `immutable=1` and discloses that the answer reflects the
  last checkpoint. `Sync` reports `unsupported` and tells you to run
  `codegraph sync`.
- **Schema gate.** Schema `11`, extraction `27`, CodeGraph `>= 1.6.2`. Anything
  else is refused with a `*SchemaError` (it unwraps to `domain.ErrUnsupported`)
  that names the index, the version and the supported pair. A newer CodeGraph
  with the same schema is accepted with a note.
- **No source in the database.** Only ranges, signatures and docstrings are
  read from the index; source text is re-read from the checkout per query. A
  file whose size or mtime no longer matches the index is disclosed as stale
  instead of silently mixing two versions.
- **Untrusted input stays data.** A path from the index is validated against
  the checkout root (no `..`, no absolute path, no escape through a relative
  join), and ANSI escape sequences and control bytes are stripped from source
  before it reaches a terminal or an agent.
- **Deterministic output.** Ties break on file, line and name; maps are sorted
  before printing. Repeated queries against an unchanged index return the same
  answer.

## Limits

- One index per checkout; `project` only labels the answer.
- The graph knows what the indexer extracted. The Go extractor in CodeGraph has
  no `go/types` resolution, so interface dispatch and dynamic calls can be
  missing or heuristic; confidence and provenance expose that.
- No Markdown/doc nodes, no communities, no god nodes, no import-cycle report
  yet. Those arrive with the native indexer and the analytics work package.
- No cross-repository query, no merged multi-project graph, no embeddings.
- Search is lexical: exact name, FTS5, then substring. A semantic query still
  belongs to `brain search`.
- `brain code` does not run `codegraph`, does not install it and does not
  refresh the index. Use `brain graphs` for the external tool lifecycle.

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| `the code graph is off` | no `graph.engine` | set `engine: codegraph` or pass `--engine codegraph` |
| `no codegraph index at …` | index absent | run `codegraph index` in the checkout, or `brain graphs --init` |
| `refusing index … schema 12` | CodeGraph moved its schema | rebuild with a supported release; the native indexer will remove this coupling |
| `index_state is "indexing"` | a build was interrupted | finish or rerun the indexer; answers may be incomplete |
| `… changed since the index was built` | the checkout is newer than the index | refresh the index; the answer labels which side is stale |
| `opened immutable without WAL replay` | a live writer held the WAL | stop the writer or accept the last checkpoint |

## Next: the native indexer

The adapter exists to prove the interface on a real repository, not to make
CodeGraph a permanent dependency. The next slice replaces it behind the same
`ports.GraphIndex` contract:

- a Go store under `<brain-data>/code/<project>-<pathhash>.db`, one per
  checkout, registered in the brain;
- `go/ast` symbols and edges, `go/types` call and interface resolution;
- identifiers from path plus qualified name and a `body_hash`, not from a line
  number, so renames stop churning edges;
- pending references only, deleted once resolved, instead of a failed-reference
  ledger;
- the same DTOs, so `brain code` and the MCP surface do not change when the
  engine switches.
