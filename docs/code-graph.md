# Code graph

`brain code` answers symbol questions from a code graph instead of a memory
search: where a name is defined, what calls it, how two symbols connect, and
what a change can reach. Two engines sit behind the same `ports.GraphIndex`
contract:

| Engine | Direction | Store | When |
| --- | --- | --- | --- |
| `native` | builds and reads its own index | `<brain-data>/index/graph/` | Go, TypeScript, TSX, JavaScript and JSX checkouts; the default choice |
| `codegraph` | reads an external CodeGraph index in place | `<checkout>/.codegraph/codegraph.db` | a repo already indexed by the CodeGraph CLI |
| `off` | disabled | none | the default until a workspace opts in |

Both return the same DTOs, so `brain code` and the MCP surface do not change
when the engine switches.

## Enable it

The code graph is off by default. Add a block to `brain.yaml`:

```yaml
graph:
  engine: native                 # off (default) | native | codegraph
  path: ""                       # optional; see Paths below
```

Then build the index once:

```bash
brain code index
```

`graph.path` may name the database file or a directory that receives the
default file name. A relative path is resolved against the project checkout.
With no `graph.path`:

- native: `<data-dir>/index/graph/<project>-<pathhash>.db`
- codegraph: `<checkout>/.codegraph/codegraph.db`

An unconfigured or unknown engine is an error, not a silent fallback:

```
brain code status: the code graph is off; set graph.engine: native in brain.yaml or pass --engine native
brain code status: no native graph index at C:\...\index\graph\demo-1a2b3c4d.db; run brain code index --project demo
```

The checkout comes from the project registry: the selector path of
`--project`, then the working directory, then the project's first registered
local path. `--root` overrides it, `--db` overrides the index path, and
`--engine native|codegraph|off` overrides the engine for one command.

## Storage and schema

The native store is derived data. It lives in the brain data directory, never
inside a checkout, and the repository `.gitignore` already ignores `/index/`
for a data directory that is itself a git repository. It can be deleted at any
time and rebuilt from source.

One SQLite file per checkout, WAL mode, `synchronous=NORMAL`, schema `2`,
extractor `2`. The file name includes a short hash of the canonical checkout
path so a worktree and its main tree never share a path-keyed index.

| Table | Columns | Purpose |
| --- | --- | --- |
| `files` | `path, sha256, size, mtime_ns, package, language, indexed_at, unresolved` | file identity and the per-file unresolved-reference count |
| `symbols` | `id, path, package, kind, name, qualname, start_line, end_line, signature, doc, exported` | definitions: `package`, `module`, `type`, `func`, `method`, `const`, `var` |
| `edges` | `source, target, kind, line, confidence, provenance` | `imports`, `calls`, `implements`, `extends`, `embeds`, `references` |
| `communities` | `id, name, top_symbol, size, cohesion, signature` | one row per detected community, ordered by size at query time |
| `community_members` | `community_id, symbol_id` | the partition: every symbol belongs to exactly one community |
| `meta` | `key, value` | `schema_version`, `extractor_version`, `engine`, `module`, `git_sha`, `indexed_at`, `index_state`, `unresolved_refs`, `skipped_files`, `package_errors`, `communities`, `community_algorithm` |
| `symbols_fts` | FTS5 over name, qualname, signature, doc | full-text tier of search |

Symbol ids are `sym_<sha256(namespace key + qualname)[:20]>`: the key is the Go
package path for Go and the repository file path for TypeScript/JavaScript, so
ids never collide across languages. Package ids are `pkg_<sha256(import
path)[:16]>`. The id never contains a line number, so an edit that only shifts
lines keeps every incoming edge valid.

**No source is stored.** A symbol row keeps its line range, declaration
signature and doc comment; the body is re-read from the checkout at query time.
A test asserts that a function-body marker never appears in the database or its
WAL.

A schema or extractor version mismatch is not migrated: `brain code index
--rebuild` drops and recreates the store. A read against a mismatched store
fails with "run brain code index --rebuild" instead of misreading old columns.

## Command surface

| Command | Answers | Main flags |
| --- | --- | --- |
| `brain code index` | build or rebuild the native store | `--rebuild`, `--tags`, `--json` |
| `brain code sync [path...]` | re-extract changed packages, optionally limited to paths | `--tags`, `--json` |
| `brain code status` | schema, versions, counts, index age, commit, dirty worktree | `--json` |
| `brain code search <query>` | where a name is defined | `--kind` (repeatable), `--file`, `--limit` (1–100, default 10) |
| `brain code node <symbol>` | one definition, its source, direct callers and callees | `--file`, `--code`, `--callers=false`, `--callees=false`, `--limit` |
| `brain code explore <query>` | source of the matching symbols plus flow and blast radius | `--max-files` (1–24, default 12) |
| `brain code impact <symbol>` | reverse blast radius, test files marked | `--file`, `--depth` (1–10, default 2) |

Every command accepts `--project`, `--engine`, `--db`, `--root`, `--json` and
`--config`. `index` and `sync` also accept `--tags`; `index` accepts `--rebuild`.
Text output is for a human; `--json` prints the typed DTOs of `ports.GraphIndex`
inside the usual `{schema, data}` envelope.

`codegraph` cannot be written through `brain code index`: the command explains
that the CodeGraph CLI owns that index.

The analytics actions (`communities`, `god_nodes`, `path`, `pr_impact`, and
`stats` with its `analytics` block) are served by the `brain_graph` MCP tool
over both engines; they read the same store `brain code` reads.

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

## Native extraction rules

The extractor runs `go/packages` with `Tests: true` and `go/ast` + `go/types`.
Build constraints are evaluated with the host `GOOS`/`GOARCH` and any `--tags`;
generics, methods, embedded fields, embedded interfaces, aliases and test files
are indexed.

| Symbol kind | Source |
| --- | --- |
| `package` | one node per loaded package, qualified name is the module-relative package path |
| `type` | every `type` declaration, including aliases and generic type parameters |
| `func` | package-level function, including generic functions and `init` |
| `method` | concrete methods and every explicit method of an interface |
| `const`, `var` | package-level declarations; local variables and struct fields are not symbols |

Qualified names are `<module-relative package path>.<name>` for package-level
definitions and `<path>.<Type>.<method>` for methods, for example
`internal/cache.MemoryStore.Get`. Search accepts the short name, the dotted
qualified name or the `Type::method` spelling.

Edges and confidence:

| Edge | Meaning | Confidence |
| --- | --- | --- |
| `imports` | one indexed package imports another indexed package | 1.0 (`types`) |
| `calls` | the callee resolves through `go/types`, including interface dispatch | 1.0 (`types`) |
| `implements` | a concrete named type satisfies an indexed interface (value or pointer method set, promoted methods included) | 1.0 (`types`) |
| `embeds` | a struct embeds a named type, or an interface embeds another type/union term | 1.0 (`types`) |
| `references` | a package-level identifier is used as a value, type or constant | 1.0 (`types`) |
| `calls` | a package whose type information failed fell back to AST name resolution | 0.8 (`syntax`) |
| `calls` | the fallback could only match a unique method name | 0.5 (`syntax`) |

Only module-local edges are stored. A call to `fmt.Println` or an import of
`errors` has no target symbol and is not written; `status.unresolved` counts
only module-local package-level references that should have resolved. That is
why the store does not grow a failed-reference ledger.

`implements` uses `types.Implements` on the value and pointer method sets.
Candidates are interfaces of the package itself plus the exported interfaces of
its transitive imports, bucketed by the first method name, so a full index does
not compare every type against every interface. Interfaces with more than 12
methods are skipped as generated port/marker noise.

Test files are indexed like any other file. `impact` marks a hit as a test by
path (`_test.go`, `.test.ts`, `tests/`, `__pycache__`-style suites, …), so
"which tests reach this" needs no second query.

### TypeScript, TSX, JavaScript and JSX

The script extractor is a single WASI reactor built from pinned tree-sitter
core and grammar sources and executed by `wazero` (pure Go, no CGO). The gzip
asset is embedded; nothing is downloaded or compiled at run time.

| Language | Extensions | Grammar | Grammar tag / commit | ABI |
| --- | --- | --- | --- | --- |
| TypeScript | `.ts` | tree-sitter-typescript | `v0.23.2` / `f975a62` | 14 |
| TSX | `.tsx` | tree-sitter-typescript `tsx/` | `v0.23.2` / `f975a62` | 14 |
| JavaScript | `.js`, `.jsx` | tree-sitter-javascript | `v0.25.0` / `44c892e` | 15 |

Core is tree-sitter `v0.27.2` (`3fb7be4`). All sources are MIT. The asset is
`internal/adapters/codeindex/wasm/ts.wasm.gz`:
**444,436 bytes gzip / 3,851,887 bytes raw**, SHA-256 recorded in
`wasm/PROVENANCE.md` and verified at startup together with the language ABI
version and every compiled query. A mismatch fails the index instead of
returning a partial symbol set. The same reactor also carries the
tree-sitter-python grammar from the spike build; it is exported but not queried
until the Python extractor lands, so Python is not a claimed language.

Declarations are mapped onto the shared symbol kinds:

| Script declaration | Stored kind |
| --- | --- |
| function declaration, function signature, arrow/function-valued `const`/`let` | `func` |
| class, abstract class, interface, type alias, enum, namespace | `type` |
| class method, interface/abstract method signature, constructor | `method` |
| `const` binding | `const` |
| `let`/`var` binding | `var` |
| the file itself | `module` (name and qualified name are the repository path) |

Qualified names are the declaration name for a top-level definition,
`Class.method` inside a class/interface and `Namespace.Class.method` inside a
namespace. Ids are `sym_sha256(path + qualname)`, never line numbers, so an
edit that shifts lines keeps every incoming edge. Local declarations (inside a
function) are not symbols. A declaration that contains an ERROR span is
dropped; the file is counted as partial in `status` and the index notes.
`.d.ts`, `*.min.*`, `*.generated.*` and files under `dist/`, `build/`, `out/`,
`coverage/`, `node_modules/` and `.next/` are not indexed. A nested Git
checkout below the root is a different repository and is not walked; one store
describes one checkout.

Edges are syntax-only and never claim type resolution:

| Edge | Meaning | Confidence |
| --- | --- | --- |
| `imports` | a relative specifier resolves to an indexed script file (`./x` → `x.ts`, `x.tsx`, `x.js`, `x.jsx`, `x/index.*`) | 1.0 (`tree-sitter`) |
| `calls` | a bare or `this.` call matches exactly one function/method in the same file | 0.6 (`tree-sitter`) |
| `calls` | a call through a named relative import resolves to the exported target | 0.5 (`tree-sitter`) |
| `calls` | a call through `import * as ns` resolves to an exported target | 0.4 (`tree-sitter`) |
| `extends` / `implements` | class or interface heritage names a unique same-file type | 0.7 (`tree-sitter`) |
| `extends` / `implements` | heritage names a resolved imported type | 0.5 (`tree-sitter`) |
| `references` | `new Name()`, a type annotation or an alias value names a resolved type | 0.4–0.6 (`tree-sitter`) |

Bare package specifiers (`react`, `node:fs`), tsconfig `paths`/`baseUrl`
aliases, re-export chains and dynamic `import()` are deliberately unresolved.
An unresolved **relative** specifier increments the file's `unresolved` count;
a member call on a local object is not counted because it is not evidence of a
missing project symbol. Ambiguous names are skipped, never guessed. There is no
type checker behind the script extractor, so it never emits a type-resolved
`calls` edge or an `implements` edge derived from a method set.

Runtime cost measured on Windows 11 (the asset is platform-neutral; wazero
supports windows/darwin/linux amd64+arm64): first use in the product is about
**370–530 ms** (engine init, one compiled query set per language, first parse).
Once warm, a 35 KB TSX file takes **34 ms**, a 380 KB TypeScript file **222 ms**
(1.7 MB/s) and a 769 KB file **404 ms** (1.9 MB/s); the declarations-only spike
query measured 100/195 ms on the same files, and the product query additionally
extracts calls and type references (1,166 calls and 822 references in the 380 KB
file). The wasm runtime adds roughly 5 MB to the binary and 7–17 MB of wasm
memory while indexing.

## Incremental sync

`brain code sync` is the cheap path:

1. Walk the checkout and hash every Go, TypeScript, TSX, JavaScript and JSX
   file. A file whose size and mtime match the `files` row reuses its stored
   SHA-256 without being read.
2. Compare hashes. Files with changed content and files that disappeared are
   the change set; a path argument limits the set to that prefix.
3. Go changes: group changed files by directory and load only those packages
   (plus their dependencies for type information). Unchanged packages are not
   re-parsed.
4. Script changes: re-extract only the changed files plus the files that import
   them, so an import edge and a cross-file call edge are re-resolved. Delete
   stale symbols and outgoing edges, insert the new rows, then delete every
   dangling edge. The Go half of the store is untouched.
5. Rebuild the FTS mirror and recompute the unresolved count.

A body edit keeps every symbol id, so incoming edges from unchanged packages
stay valid and sync stays O(changed packages / changed scripts). If a Go change
**removes or renames** a symbol, incoming edges from unchanged packages would
point at an id that no longer exists; `sync` detects that and escalates to a
full rebuild with a note. Adding a symbol or a file is incremental, and a
script deletion is handled file-scoped. A change that touches Go and script
files in one call escalates to a full rebuild, which is the conservative choice
for cross-language edge correctness. A path argument narrows which files are
inspected; an escalation still rebuilds the whole checkout.

`sync` on an empty store runs a full index.

## Retrieval tiers and confidence

Search stops at the first tier that answers, and every hit says which tier it
came from. An exact indexed name is the only tier that proves identity.

| `match` | Source | `confidence` | Meaning |
| --- | --- | --- | --- |
| `exact` | `symbols.name` / `symbols.qualname` (`lower()` index) | 1.0 | the indexed name equals the query |
| `fts` | FTS5 over name, qualified name, signature, doc | 0.8 | a full-text candidate; verify the definition |
| `like` | `%token%` substring on name and qualified name | 0.5 | last resort for a mistyped or partial name |
| `path` | every definition of a pinned file | 1.0 | the path, not the words, proved identity |

`confidence` is identity confidence, not relevance; `score` orders hits inside a
tier. An ambiguous bare name is never resolved by picking the first row: `node`
and `impact` return the candidate list and require `--file`.

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
3. `flow` — one call chain of functions/methods, at most 7 nodes, preferring
   symbols that matched the query;
4. `blast_radius` — the nearest 16 reverse hits across at most 8 files, with
   test hits marked;
5. `pointers` — matching files that did not fit and how to read them;
6. `notes` — weak-tier, ambiguity, stale-source and truncation disclosures.

Generated bundles, `dist/`, `vendor/`, `node_modules/` and localization output
are down-weighted; test files are down-weighted but never hidden.

## `impact` semantics

`impact` is a breadth-first reverse walk over `calls`, `references`, `imports`,
`implements`, `extends` and `embeds`.

- depth is clamped to **10**; the answer reports the clamp;
- each hit carries its shallowest distance, the edge kind that reached it, the
  call site line, edge confidence and provenance (`types`, `syntax` or
  `tree-sitter`);
- the walk stops at **500** nodes and sets `truncated`;
- test files are detected by suffix and directory and listed in `tests`;
- hops resolved below 0.7 confidence are counted in `notes`.

## Analytics

`brain_graph` serves the analytics half of the same graph through the optional
`ports.GraphAnalytics` interface. The native engine stores the partition; the
codegraph adapter computes it in memory from the stored edges and caches it per
index file identity (size and mtime), because a CodeGraph index has no
community table. Both engines use the same deterministic kernel
(`internal/adapters/graphalgo`), so a native and a codegraph answer on the same
edges choose the same communities and the same path.

### Communities

- The graph is the undirected projection of the dependency edges (`calls`,
  `references`, `imports`, `implements`, `embeds`). A repeated
  `(source, target)` pair counts once; repeated call sites do not inflate a
  relationship.
- Partitioning is **deterministic Louvain**: every node starts in its own
  community, nodes are visited in id order, a move must strictly improve
  modularity at resolution 1.0 (up to 12 passes), and equal-gain moves keep the
  current community. Communities then become the nodes of the next level (up to
  8 levels). Each level runs both a forward and a reverse node order and the
  better modularity wins, because one greedy order can strand a dense group.
  There is no random seed and no dependence on Go map order.
- On graphs of at least 500 nodes the top **5%** by degree are excluded from
  the partition and reattached to the community they share the most edge weight
  with; a one-member community is then attached to its strongest neighbour. A
  hub such as `NewValidationError` (398 relationships) otherwise glues the graph
  into a few hundred-node blobs. On this repository that yields 124 communities,
  the largest at 666 of 11,189 symbols, with 11 true singletons (the orphans).
  Without the refinements the same graph collapses into 50 communities of up to
  1,300 symbols.
- `cohesion = internal edge weight / (n·(n−1)/2)`: plain internal density of
  the member subgraph, 0 for a singleton.
- `signature = sha256(sorted member ids)[:16]`. Ids are stable while the member
  set is unchanged: an index rebuild or an incremental sync reuses the id of
  every community whose signature still exists, and a new community gets the
  next free id. On a fresh store, ids follow size order. Ordering is by size,
  not by id.
- `name` is the highest-degree member (the graphify hub-label fallback); `top`
  is that symbol. No LLM naming pass exists.
- Limits: **100** communities per listing, **200** members of one selected
  community. `truncated` and a note disclose a cut list.

### God nodes

- Degree counts **distinct dependency relationships** in both directions over
  the same edge kinds. A reciprocal call counts once in each direction; five
  call sites of the same pair count once.
- Native excludes `package` nodes; codegraph excludes `file`, `import`,
  `module`, `export` and `parameter` nodes. An import hub is not an abstraction.
  Definitions with degree 0 are orphans and are not ranked.
- `centrality = degree / (definitions − 1)` is a degree-centrality proxy, not
  betweenness. The `why` field states the formula and both degree directions
  instead of guessing importance.
- Tie-breaks: name, file, start line, id. The ranking is deterministic.
- Limit: **50** nodes (default 10). Test definitions are marked `test`, never
  hidden.

### Path

- Breadth-first search over the dependency edges, outgoing direction first. If
  no directed path exists within the hop limit, the same search runs over both
  directions and the answer sets `directed:false` and says a hop was reversed.
  This avoids graphify's opaque "no directed path" dead end.
- Each hop carries its edge kind, line, confidence and `reverse` marker;
  `confidence` of the path is the weakest hop, and hops below 0.7 are counted in
  `notes`.
- Limits: default **8** hops, maximum **16**, at most **20** nodes in the
  reported chain. `max_hops` is a search bound, unlike graphify's post-check.
- Frontier and neighbour order are sorted before expansion, so the chosen path
  does not depend on database row order.

### PR impact

- `git diff --name-status --no-renames --no-ext-diff -z <base> <head>` in the
  checkout. It is read-only, fetches nothing (both refs must resolve locally),
  and works on any git ref: `HEAD~1`, `origin/main`, a tag, a commit hash.
  Rename detection is off, so a rename is a delete plus an add.
- Ref validation refuses an option-shaped ref (`-x`), a range (`a..b`), a path
  (`HEAD:file`) or control characters. An unknown ref is a validation error.
- Changed files map to every indexed definition in that file (file granularity,
  not hunk granularity). Then a multi-source reverse BFS over dependency edges
  at depth **2** finds the callers and importers; every hit carries its
  shallowest distance and the edge that reached it.
- Risk tiers, shared by both engines:
  - **high** — a deleted file, ≥50 reachable definitions, or ≥25 changed
    definitions;
  - **medium** — ≥10 reachable or ≥5 changed definitions;
  - **low** — everything else.
  The notes repeat the inputs, so a tier is never unexplained.
- Tests are the test files among the changed files and among the reachable
  symbols. A deleted file is always high risk because its callers break at
  compile time; symbols still present in the index are included as seeds.
- Limits: **200** changed files, **500** changed definitions, **500** reachable
  definitions. A cut sets `truncated` and adds a note.
- No GitHub/GitLab API is called and no pull-request metadata is stored.

### Whole-graph stats

`stats` keeps the index envelope (`ports.GraphStatus`) and adds `analytics`:
files, symbols, edges, communities, orphans, languages and node kinds. An
**orphan** is a definition with no dependency edge in either direction
(structural file/import nodes are excluded on codegraph); it is the honest
"knowledge gap" count.

## Codegraph adapter

The `codegraph` engine stays available for a repository that already has an
index from the CodeGraph CLI:

- **Read-only.** The database is opened with SQLite `mode=ro` and
  `PRAGMA query_only=1`; `Sync` reports `unsupported` and tells you to run
  `codegraph sync`. If a live writer holds the WAL and the read-only open
  reports `BUSY`, the adapter retries with `immutable=1` and discloses that the
  answer reflects the last checkpoint.
- **Schema gate.** Schema `11`, extraction `27`, CodeGraph `>= 1.6.2`. Anything
  else is refused with a `*SchemaError` (it unwraps to `domain.ErrUnsupported`).
- **Same query surface.** Search tiers, node, explore, impact, stale-source
  notes and untrusted-path handling behave like the native engine, so the two
  can be compared on one repository. Analytics uses the same deterministic
  kernel (including 5% hub exclusion); the partition is computed in memory over
  `edges` and cached per index file identity because a CodeGraph index stores no
  communities. On the audited `lv24` index (184,785 nodes, 523,853 edges) the
  first `stats` or `communities` call takes 4–17 s (cold/warm OS cache) and
  produces 97,705 communities, 91,071 of them singletons (structural file/import
  nodes and orphan definitions); later calls in the same server process reuse
  the cached partition. The native engine is the fast path.

## Limits and remaining gaps

- **Go is type-checked; scripts are syntax-only.** The native extractor
  understands Go modules and TypeScript/TSX/JavaScript/JSX files. Python, Rust
  and the rest still need the external CodeGraph index or a future extractor.
  Script edges never claim type resolution, and a checkout with no indexed
  symbols at all still answers `UNSUPPORTED` for analytics instead of an empty
  ranking.
- One module and one checkout per store. A nested `go.mod`, a nested Git
  checkout/submodule below the root is skipped, a second checkout gets its own
  file, and cross-repository queries do not exist.
- No daemon or file watcher. `sync` is on demand (post-edit hook or a manual
  command); `brain code index` is the full rebuild.
- Go resolution is static. Dynamic dispatch through an interface is represented
  as a call to the interface method plus `implements` edges to the concrete
  types; reflection, `unsafe` and runtime dispatch are not followed. Script
  resolution is syntactic: tsconfig aliases, re-export chains, `export { X as Y }`
  alias lists, CommonJS destructuring, dynamic imports and type-only dispatch
  are not followed.
- A script file with a syntax error is partial: clean declarations are indexed,
  declarations intersecting ERROR spans are skipped, and `status` reports the
  partial count. A Go package that fails to type-check keeps its syntax edges at
  0.8/0.5.
- No import-cycle report, surprising-connection score or coupled
  test-coverage view yet; communities, god nodes, paths, PR impact and orphan
  counts are served.
- Search is lexical: exact name, FTS5, then substring. A semantic query still
  belongs to `brain search`.
- `codegraph` cannot be refreshed through this binary; use `brain graphs` for
  the external tool lifecycle.

## Measurements

Measured on Windows 11, Go 1.27, the `lv24-brain` checkout itself (765 Go files,
~190k tracked lines, 63 packages):

| Metric | Value |
| --- | --- |
| full index, warm cache | 5–8 s |
| full index, cold (`go run` build included) | 12.7 s |
| symbols / edges | 11,189 / 55,840 |
| communities / largest community | 124 / 666 symbols |
| unresolved module-local references | 20 |
| database size | 21 MiB (~112 KB per 1k LOC) |
| incremental body edit | one package re-parsed, no rebuild |
| no-change sync | under 1 s, only stats and hashes |
| `brain_graph(stats)` (native, warm) | 58 ms |
| `brain_graph(communities, limit=100)` | 5 ms (reads the stored partition) |
| `brain_graph(god_nodes, limit=50)` | 126 ms (loads the whole edge list) |
| `brain_graph(path)` | 134 ms (2 hops, undirected fallback) |
| `brain_graph(pr_impact)` on a 20-file commit | 83 ms, 500 changed symbols (capped), 225 reachable, risk high |
| full index including the partition | 7.6 s (the partition itself is about 70 ms) |

A synthetic 924-line module (12 packages, 120 symbols) indexes in about 0.5 s
into a 94 KiB database (~102 KB per 1k LOC), measured by
`TestIndexMeasurement`. For comparison, the audited CodeGraph index was about
390 KB per 1k LOC because it keeps a large unresolved-reference ledger.

Script extraction, measured with the spike fixtures, the desktop checkout and
two large TypeScript sources:

| Metric | Value |
| --- | --- |
| reactor asset | 3,851,887 B raw / 444,436 B gzip |
| first use, product (engine + query sets + first parse) | 370–530 ms |
| first use, declarations-only spike | about 120 ms |
| 1.6 KB TypeScript file, parse + extract (spike) | 1.5 ms |
| 35 KB TSX desktop file, warm | 34 ms |
| 380 KB TypeScript file, full extraction | 222 ms (1.7 MB/s); declarations-only spike: 100.7 ms |
| 769 KB TypeScript file, full extraction | 404 ms (1.9 MB/s); declarations-only spike: 195.2 ms |
| 160-file desktop checkout, full index | 1.24 s (872 symbols, 1,093 edges) |
| no-change sync of that checkout | 71 ms |
| peak RSS while indexing | 48–90 MB |
| binary growth (wazero + asset) | about 5 MB |

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| `the code graph is off` | no `graph.engine` | set `engine: native` or pass `--engine native` |
| `no native graph index at …` | index absent | run `brain code index --project <id>` |
| `graph index is empty` | store exists, no files indexed | run `brain code index`; check `--root` |
| `graph index schema mismatch` | the binary's schema/extractor moved | run `brain code index --rebuild` |
| `no supported source file was found` | the checkout contains only Python, Rust or another language the native engine does not parse | use the `codegraph` engine or wait for that language pack |
| `no Go packages were loaded` | a Go checkout has no `go.mod`, or all Go files are excluded by build tags; a script-only checkout legitimately has no Go packages | check the checkout and `--tags`; this is a note, not an index failure |
| `… package(s) reported load or type errors` | dependencies missing or Go code does not type-check | run `go mod download` / fix the reported package; syntax edges remain at 0.8/0.5 |
| `… file(s) had syntax errors` | a TypeScript/JavaScript file does not parse | fix the reported file; clean declarations around the ERROR span are still indexed |
| `no matching symbol` after a rename | sync escalated to a rebuild | re-run the query; check `brain code status` |
| `… changed since the index was built` | the checkout is newer than the index | run `brain code sync` |
| `opened immutable without WAL replay` | codegraph writer held the WAL | stop the writer or accept the last checkpoint |

## Next: Python and graph analytics

The store, query layer, budgeted explore and impact walk are
language-independent. TypeScript, TSX, JavaScript and JSX now write the same
`symbols`/`edges` rows as Go, so one store answers across a mixed repository
while the Go extractor keeps its `go/types` precision. Python is the next
tree-sitter grammar to wire into the same reactor; it stays pending until the
wasm rebuild and its golden tests land.
