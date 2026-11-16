# Code graph

`brain code` answers symbol questions from a code graph instead of a memory
search: where a name is defined, what calls it, how two symbols connect, and
what a change can reach. Two engines sit behind the same `ports.GraphIndex`
contract:

| Engine | Direction | Store | When |
| --- | --- | --- | --- |
| `native` | builds and reads its own index | `<brain-data>/index/graph/` | Go projects; the default choice |
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

One SQLite file per checkout, WAL mode, `synchronous=NORMAL`, schema `1`,
extractor `1`. The file name includes a short hash of the canonical checkout
path so a worktree and its main tree never share a path-keyed index.

| Table | Columns | Purpose |
| --- | --- | --- |
| `files` | `path, sha256, size, mtime_ns, package, language, indexed_at, unresolved` | file identity and the per-file unresolved-reference count |
| `symbols` | `id, path, package, kind, name, qualname, start_line, end_line, signature, doc, exported` | definitions: `package`, `type`, `func`, `method`, `const`, `var` |
| `edges` | `source, target, kind, line, confidence, provenance` | `imports`, `calls`, `implements`, `embeds`, `references` |
| `meta` | `key, value` | `schema_version`, `extractor_version`, `engine`, `module`, `git_sha`, `indexed_at`, `index_state`, `unresolved_refs`, `skipped_files`, `package_errors` |
| `symbols_fts` | FTS5 over name, qualname, signature, doc | full-text tier of search |

Symbol ids are `sym_<sha256(package path + qualname)[:20]>`; package ids are
`sym`-independent `pkg_<sha256(import path)[:16]>`. The id never contains a line
number, so an edit that only shifts lines keeps every incoming edge valid.

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

## Incremental sync

`brain code sync` is the cheap path:

1. Walk the checkout and hash every Go file. A file whose size and mtime match
   the `files` row reuses its stored SHA-256 without being read.
2. Compare hashes. Files with changed content and files that disappeared are
   the change set; a path argument limits the set to that prefix.
3. Group changed files by directory and load only those packages (plus their
   dependencies for type information). Unchanged packages are not re-parsed.
4. Re-extract symbols and edges for those packages, delete stale rows for the
   changed files, and leave rows of unchanged packages alone.
5. Rebuild the FTS mirror and recompute the unresolved count.

A body edit keeps every symbol id, so incoming edges from unchanged packages
stay valid and sync stays O(changed packages). If a change **removes or renames**
a symbol, incoming edges from unchanged packages would point at an id that no
longer exists; `sync` detects that and escalates to a full rebuild with a note.
Adding a symbol or a file is incremental. Deleting a file therefore rebuilds,
which is the conservative choice for edge correctness. A path argument narrows
which files are inspected; an escalation still rebuilds the whole module.

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
`implements` and `embeds`.

- depth is clamped to **10**; the answer reports the clamp;
- each hit carries its shallowest distance, the edge kind that reached it, the
  call site line, edge confidence and provenance (`types` or `syntax`);
- the walk stops at **500** nodes and sets `truncated`;
- test files are detected by suffix and directory and listed in `tests`;
- hops resolved below 0.7 confidence are counted in `notes`.

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
  can be compared on one repository.

## Limits and remaining gaps

- **Go only.** The native extractor understands Go modules. TypeScript,
  JavaScript, Python, Rust and the rest still need the external CodeGraph index
  or a future extractor; that is the largest remaining gap.
- One module and one checkout per store. A nested `go.mod` below the root is
  skipped, a second checkout gets its own file, and cross-repository queries do
  not exist.
- No daemon or file watcher. `sync` is on demand (post-edit hook or a manual
  command); `brain code index` is the full rebuild.
- Resolution is static. Dynamic dispatch through an interface is represented as
  a call to the interface method plus `implements` edges to the concrete types;
  reflection, `unsafe` and runtime dispatch are not followed.
- No code-graph analytics yet: no communities, god nodes, import-cycle report or
  coupled test-coverage view.
- Search is lexical: exact name, FTS5, then substring. A semantic query still
  belongs to `brain search`.
- `codegraph` cannot be refreshed through this binary; use `brain graphs` for
  the external tool lifecycle.

## Measurements

Measured on Windows 11, Go 1.27, the `lv24-brain` checkout itself (749 Go files,
~179k tracked lines, 63 packages):

| Metric | Value |
| --- | --- |
| full index, warm cache | 5–8 s |
| full index, cold (`go run` build included) | 12.7 s |
| symbols / edges | 10,787 / 53,470 |
| unresolved module-local references | 14 |
| database size | 19.9 MiB (~112 KB per 1k LOC) |
| incremental body edit | one package re-parsed, no rebuild |
| no-change sync | under 1 s, only stats and hashes |

A synthetic 924-line module (12 packages, 120 symbols) indexes in about 0.5 s
into a 94 KiB database (~102 KB per 1k LOC), measured by
`TestIndexMeasurement`. For comparison, the audited CodeGraph index was about
390 KB per 1k LOC because it keeps a large unresolved-reference ledger.

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| `the code graph is off` | no `graph.engine` | set `engine: native` or pass `--engine native` |
| `no native graph index at …` | index absent | run `brain code index --project <id>` |
| `graph index is empty` | store exists, no files indexed | run `brain code index`; check `--root` |
| `graph index schema mismatch` | the binary's schema/extractor moved | run `brain code index --rebuild` |
| `no Go packages were loaded` | no `go.mod`, or all files excluded by build tags | check the checkout and `--tags` |
| `… package(s) reported load or type errors` | dependencies missing or code does not type-check | run `go mod download` / fix the reported package; syntax edges remain at 0.8/0.5 |
| `no matching symbol` after a rename | sync escalated to a rebuild | re-run the query; check `brain code status` |
| `… changed since the index was built` | the checkout is newer than the index | run `brain code sync` |
| `opened immutable without WAL replay` | codegraph writer held the WAL | stop the writer or accept the last checkpoint |

## Next: multi-language extractors

The store, query layer, budgeted explore and impact walk are
language-independent. The next slice adds TypeScript/JavaScript and Python
extractors writing the same `symbols`/`edges` rows, so one store can answer
across a mixed repository while the Go extractor keeps its `go/types` precision.
