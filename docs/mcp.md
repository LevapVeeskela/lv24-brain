# MCP server contract

`brain mcp` serves the brain over stdio as a Model Context Protocol server. It
speaks two protocol revisions from one process, so current hosts and older
handshake clients both work: **`2026-07-28`** (stateless, per-request metadata)
and **`2025-06-18`** (handshake). This page is the contract for hosts, for
`skills` and for anyone pinning our tool definitions.

The default profile is `lv24`: **17 tools** plus the read-only `memory://`
resources and the `code://` symbol resource. Migration profiles (`engram`), the
code-graph vocabularies (`codegraph`, `graphify`) and the resources are
described in §10–§14. Stdout carries protocol messages only; diagnostics go to
stderr.

## 1. Revisions

| Revision | Opened by | Result shape | Notes |
|---|---|---|---|
| `2026-07-28` | `_meta.io.modelcontextprotocol/protocolVersion` on each request, `server/discover` optional | `resultType` plus `_meta.io.modelcontextprotocol/serverInfo` on every result; `ttlMs`/`cacheScope` on list and discovery results | No `initialize`, no session, no server-initiated requests (MRTR instead) |
| `2025-06-18` | `initialize` handshake | The pre-2026 shape: no `resultType`, no cache hints | Unchanged from previous releases |
| `2025-03-26`, `2024-11-05` | `initialize` with that version | As `2025-06-18` | Echoed back when a client asks for it |

Rules the server follows:

- A request that carries `_meta["io.modelcontextprotocol/protocolVersion"] = "2026-07-28"`
  is served statelessly under the 2026 rules.
- A request that carries no `_meta`, or declares one of the supported legacy
  versions, is served under the legacy shape. `initialize` always selects legacy
  semantics, as the spec requires for dual-era servers.
- A request that declares an unsupported version is refused with the MCP
  `UnsupportedProtocolVersion` error (`-32022`) and `data.supported` listing the
  revisions above, so the client can retry with one of them.
- `clientCapabilities` is read leniently. A 2026 request that omits it is served
  as if it declared nothing; the server never relies on a capability the client
  did not declare.

## 2. Discovery

`server/discover` needs no handshake and is always answered in the 2026 shape,
even when the probe itself declares no revision:

```json
{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}
```

```json
{"jsonrpc":"2.0","id":1,"result":{
  "resultType":"complete",
  "supportedVersions":["2026-07-28","2025-06-18","2025-03-26","2024-11-05"],
  "capabilities":{"tools":{"listChanged":false},"resources":{"listChanged":false,"subscribe":false}},
  "instructions":"Company Brain is the curated memory of this project. ...",
  "_meta":{"io.modelcontextprotocol/serverInfo":{"name":"lv24-brain","version":"0.17.0"}},
  "ttlMs":3600000,
  "cacheScope":"public"
}}
```

`_meta` also carries `lv24.brain/capabilities`: the same payload
`brain_capabilities` returns (profile, grades, gaps, limits). It is additive, so
a client that does not know the key ignores it.

## 3. Cache hints

`tools/list` and `server/discover` carry `ttlMs` (300000 for the tool list,
3600000 for discovery). `server/discover` is `cacheScope: "public"`: every
profile advertises the same revisions and capabilities. `tools/list` is
`public` for a writable server and `private` under `--read-only`, because a
read-only profile lists ten tools where a writable one lists seventeen (§9).
Tool results are never cached: each reflects current state. `resources/list`
carries `ttlMs` 30000 and `resources/read` carries a per-resource TTL, always
with `cacheScope: "private"` because the allowlist is part of the answer (§14).

## 4. Multi Round-Trip Requests

A 2026 client has no server-initiated requests. When a call needs the user to
confirm a curation change, the server answers with `resultType:
"input_required"` and an elicitation request; the client retries the same call
with a new JSON-RPC id, the `inputResponses`, and the echoed `requestState`.

Three actions ask for confirmation: `brain_judge(action=judge)`, because the
verdict replaces any existing relation between the pair;
`brain_judge(action=supersede)`, because it changes which decision is current;
and `brain_judge(action=consolidate, phase=apply)`, because it writes merges and
archives (a plan never asks and never writes):

```json
{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{
  "name":"brain_judge",
  "arguments":{"action":"supersede","id":"dec-2026-11-10-sqlite","to":"dec-2026-11-14-ulid"},
  "_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"elicitation":{}}}}}}
```

```json
{"jsonrpc":"2.0","id":7,"result":{
  "resultType":"input_required",
  "inputRequests":{"confirm":{"method":"elicitation/create","params":{
    "mode":"form",
    "message":"Mark decision dec-2026-11-10-sqlite as superseded by dec-2026-11-14-ulid? ...",
    "requestedSchema":{"type":"object","properties":{"confirm":{"type":"boolean"}},"required":["confirm"],"additionalProperties":false}}}},
  "requestState":"eyJ2IjoxLCJ0b29sIjoi..."
}}
```

The retry carries `"inputResponses":{"confirm":{"action":"accept","content":{"confirm":true}}}`
and `"requestState":"..."` next to `arguments`. Details:

- The prompt is sent only to clients that declared `elicitation`. A client
  without it (and every legacy client) gets the pre-2026 behaviour: the call
  runs without a prompt. The server never blocks a call on a capability the
  client lacks.
- `requestState` is HMAC-signed with a per-process key and binds the tool name,
  a canonical digest of the arguments, and a 10-minute expiry. A state that is
  forged, expired, from another process, or from a different argument set is
  refused with `INVALID_ARGUMENTS` and `retryable: true`.
- `action: "decline"` or `"cancel"` returns `DECLINED` and changes nothing.
- Sampling, roots and logging are deprecated in 2026 and are not used.

## 5. Typed refusals

A failed tool call is not a JSON-RPC error: the result carries `isError: true`,
the envelope in `structuredContent`, and a readable line in `content[0].text`.
The envelope is closed: exactly five keys.

```json
{"code":"INVALID_ARGUMENTS","message":"invalid arguments: unknown field \"bogus\"",
 "try_instead":"check the arguments against inputSchema","current_revision":"","retryable":false}
```

| Code | Meaning | Retryable |
|---|---|---|
| `INVALID_ARGUMENTS` | Unknown field, missing required value, bad cursor | no |
| `NOT_FOUND` | Unknown project, record, decision or retired tool | no |
| `CONFLICT` | The write collides with existing data or an expected revision moved | yes, after re-reading |
| `DUPLICATE` | The normalized text already exists in the project and kind | no |
| `UNSUPPORTED` | The mode, profile or call window does not serve this | no |
| `DECLINED` | The user refused to confirm; nothing changed | no — do not retry blindly |
| `FAILED` | Unexpected failure | no |

`current_revision` is empty in local mode unless a state write reports one;
`brain_state` carries the revision it read. `outputSchema` describes successful
results only: a refusal is the envelope above and is not validated against it.

An action this release accepts but cannot serve is always `UNSUPPORTED` with a
CLI `try_instead`, never a silent no-op. The gaps are:

| Tool | Action | Try instead |
|---|---|---|
| `brain_record` | `list due_review=true` | `brain_review(action=due)` |
| `brain_search` | `include_prompts=true` | `brain_prompt(action=search)` |
| `brain_prompt` | `discard` | edit `projects/<id>/prompts/<date>.md` by hand |
| `brain_projects` | `merge` | `brain project set <id> --alias <old-id>` or `brain import --map-project` |

A graph engine that serves symbol navigation but not analytics refuses the
analytics actions of `brain_graph` at call time; both shipped engines serve
them, and an off or absent index refuses with the `brain_code` recovery path.

`brain_capabilities` returns the same list, so a host can discover a gap
without calling it. A migration profile adds its own gaps to the payload: every
grade-C alias names the approximation and every grade-D alias names the CLI
path that can do the work (§12).

## 6. The 17 tools

Each tool has one intent; a write action never hides inside a tool annotated
read-only. `action` defaults to the historical read behaviour. `brain_code` is
the one mixed tool: five read actions and two idempotent local index writes, so
its `readOnlyHint` is false while `x-lv24-read-actions` names the reads.

| Tool | Policy | Actions | Intent |
|---|---|---|---|
| `brain_open` | read | – (`include` adds `state`, `handoff`, `recent`, `counts`) | session-start brief |
| `brain_context` | read | – | task-shaped context pack |
| `brain_search` | read | – | full-text search over records |
| `brain_record` | read | `get` (default), `list`, `relations`, `history`, `links` | one record, a listing, relations, supersede chain, links |
| `brain_note` | append | `record` (default), `suggest_topic` | append or evolve a note (`topic_key`, `subtype`, `scope`, `source`) |
| `brain_decision` | append | – | record a decision (lifecycle is `brain_judge`) |
| `brain_attempt` | append | – | record an attempt and its reason |
| `brain_handoff` | append | – (`sections` template) | close a session; git context is captured |
| `brain_state` | destructive | `get` (default), `set` | read or replace project state (`expected_revision`) |
| `brain_catchup` | read | `changes` (default), `journal`, `timeline` | what changed, the journal, one record in context |
| `brain_review` | read | `findings` (default), `due`, `doctor` | stale knowledge, the review queue, engine checks |
| `brain_judge` | destructive | `judge` (default), `list`, `judge_many`, `compare`, `review`, `accept`, `reject`, `supersede`, `pin`, `unpin`, `consolidate` | change the status of existing knowledge; `consolidate` plans or applies the proposal-only dedup job (§6.3) |
| `brain_projects` | read | `list` (default), `current`, `register`, `merge` | project registry; `current` reports the resolution source |
| `brain_prompt` | destructive | `save`, `search`, `discard` | opt-in local-only prompt zone |
| `brain_code` | mixed | `status` (default), `search`, `node`, `explore`, `impact`, `index`, `sync` | query or index one checkout's code graph |
| `brain_graph` | read | `stats` (default), `neighbors`, `path`, `communities`, `god_nodes`, `cycles`, `diff`, `pr_impact`, `surprises` | graph counts, neighbours, communities, degree ranking, path, import cycles, index deltas, local-diff impact and surprising cross-community edges |
| `brain_capabilities` | read | – | profile, engine, tools, features, limits, gaps, refusal codes, endpoints |

The write postures are exact: **9 read, 5 non-destructive writes, 3
destructive** (`brain_state`, `brain_judge`, `brain_prompt`). `brain_projects`
is read-only: `register` is gated by the write scope and `merge` is refused; on
a read-only server every write action of a mixed tool is refused before the
backend. `x-lv24-read-actions` and `x-lv24-write-actions` name the actions of a
mixed tool, so a host does not have to infer them from the four hints.

### 6.1 `brain_code`

`brain_code` answers from `ports.GraphIndex`: the native Go index or an
external codegraph index, chosen by `graph.engine` or the per-call `engine`.

| Action | Required | Reads | Answer |
|---|---|---|---|
| `status` (default) | `project` | index identity | engine, root, schema, counts, age, notes |
| `search` | `query` | symbols | exact-first hits with kind, location and match tier |
| `node` | `symbol` | symbol + source + edges | declaration, numbered source, callers, callees |
| `explore` | `query` | symbols + source | ranked symbols, rendered files, flow, blast radius |
| `impact` | `symbol` | reverse edges | hits by distance, files and test files |
| `index` | `project` | native writer | full/incremental index; `changed` |
| `sync` | `project` | native writer | refresh changed files; `changed` |

Inputs: `project` (required — a graph belongs to one checkout), `query`,
`symbol`, `file`, `engine` (`native`/`codegraph`), `depth` 1–10 (default 2),
`limit` (hits ≤100, neighbours ≤100, explore files ≤24), `max_bytes` ≤24576.
The action answers are enveloped as `{action, project, status|sync|search|node|
explore|impact}`, and the inner object mirrors the port DTO.

`index` and `sync` are `idempotent`, `destructive:false` and local: they write a
derived index, never the Markdown truth, and a read-only server lists
`brain_code` for its reads while refusing both actions with `UNSUPPORTED`
before the backend runs.

Typed refusals, never empty answers:

| Condition | Code | `try_instead` |
|---|---|---|
| `graph.engine: off` | `UNSUPPORTED` | set `graph.engine: native`, pass `engine=codegraph`, or `brain code status` |
| no checkout or no index file | `UNSUPPORTED` | `brain code index --project <id>` or `codegraph index` |
| index empty or written by another schema | `UNSUPPORTED` | rebuild with `brain code index --project <id>` |
| `index`/`sync` on the codegraph engine | `UNSUPPORTED` | `codegraph index` in the checkout, or switch to native |
| answer above `max_bytes` | `UNSUPPORTED` | raise `max_bytes` (≤24576) or lower `limit` |

### 6.2 `brain_graph`

`brain_graph` exposes the graph as a graph rather than as a symbol reader. All
eight actions are read-only; `pr_impact` runs `git diff` against two local refs
and `diff` reads a second index, and neither fetches or stores pull-request
data.

| Action | Answer |
|---|---|
| `stats` (default) | the same index envelope as `brain_code(status)`, plus `analytics`: communities, orphans, languages and node kinds |
| `neighbors` | one symbol with both edge directions, no source |
| `path` | the shortest chain between two symbols, its hops, direction and weakest confidence |
| `communities` | the detected communities by size, or one community with its members, cohesion and signature |
| `god_nodes` | definitions ranked by distinct dependency relationships, with both degree directions and centrality |
| `cycles` | import cycles (Tarjan SCC) of Go packages and TypeScript/Python modules, with members and self-import count |
| `diff` | file, symbol, edge and (native) community deltas between this index and `base_project` |
| `pr_impact` | changed files → changed symbols → reverse blast radius, risk tier and test files for `base..head` |
| `surprises` | dependency edges whose endpoints live in different communities, scored by confidence, degree and rarity |

Algorithms, limits and the risk tiers are documented in
[`code-graph.md`](code-graph.md#analytics).

Inputs: `project` (required), `symbol`, `file`, `engine` (`native`/`codegraph`,
default `graph.engine`), `limit` (neighbours ≤100, communities ≤100, god nodes
≤50, cycles ≤50, diff samples ≤100), `community_id`, `member_limit` (≤200 for a
community, ≤50 for a cycle), `depth` (path hops, 1–16, default 8), `base`
(required for `pr_impact`), `head` (default `HEAD`) and `base_project` (required
for `diff`; a second checkout or worktree of the same repository). Answers are
enveloped as `{action, project, stats|analytics|neighbors|path|communities|
god_nodes|cycles|diff|pr_impact}` and the inner object mirrors the port DTO.

Caps are applied by the engine and disclosed, never silent: 100 communities,
200 members of one community, 50 god nodes, 50 cycles, 50 members of one cycle,
100 diff samples per side, 20 path nodes, 200 changed files, 500 changed or
reachable PR symbols. A cap that cuts an answer sets `truncated` and adds a
note; `cycles.total` and the diff counts stay exact.

Typed refusals, never empty answers:

| Condition | Code | `try_instead` |
|---|---|---|
| engine off, index absent or empty | `UNSUPPORTED` | `brain code index --project <id>` or `codegraph index` |
| engine serves navigation but not analytics | `UNSUPPORTED` | `stats`/`neighbors` still work; switch to `graph.engine: native` |
| unknown community id | `NOT_FOUND` | `communities` lists ids |
| unknown symbol, ambiguous name | `NOT_FOUND` / `INVALID_ARGUMENTS` | `brain_graph(neighbors)`, `brain_code(search)` or add `file` |
| `base` missing, ref malformed or unknown | `INVALID_ARGUMENTS` | pass two refs such as `base=HEAD~1 head=HEAD` |
| `base_project` missing or equal to `project` | `INVALID_ARGUMENTS` | name a second checkout or worktree of the same repository |
| base index of another engine or schema | `UNSUPPORTED` | index the base checkout with the same engine and rebuild it |
| an argument above its cap | `INVALID_ARGUMENTS` | lower it to the documented maximum |


### 6.3 `brain_judge(action=consolidate)`

The dedup job is proposal-only. `phase=plan` (the default) computes deterministic
ops and writes nothing; `phase=apply` executes only the named `ids` and requires
the plan `fingerprint`, so a stale plan is refused with `CONFLICT` instead of
being replayed.

| Field | Meaning |
|---|---|
| `phase` | `plan` (default, read-only) or `apply` |
| `ids` | op ids to apply; required with `phase=apply`, at most 50 |
| `fingerprint` | the plan fingerprint from `phase=plan`; required with `phase=apply` |

Ops are deterministic: `merge-<survivor>` merges an exact duplicate cluster (or a
near cluster at similarity `>= 0.98` of the same kind and project), and
`archive-<id>` trashes one superseded record older than the archive age that no
live record links to. Near clusters below the threshold are reported in
`plan.near` and never merged. Every op carries `risk` and `reversible` (the merge
manifest, or `brain record restore <id>` for an archive). An apply writes a
manifest under `backups/consolidate-<ts>/manifest.json` that
`brain consolidate --rollback <manifest>` restores; nothing is ever deleted. The
CLI adds `--max`, `--archive-after`, `--scan` and `--op` (§`review.md`).

Typed refusals:

| Condition | Code | `try_instead` |
|---|---|---|
| `phase=apply` without `fingerprint` or `ids` | `INVALID_ARGUMENTS` | run `phase=plan`, then apply the reviewed op ids with the fingerprint |
| unknown `phase` | `INVALID_ARGUMENTS` | `plan` or `apply` |
| plan fingerprint mismatch | `CONFLICT` | re-run `phase=plan` and apply the fresh ops |
| more than 50 op ids | `INVALID_ARGUMENTS` | apply in batches |

## 7. Pagination

The list-like actions take `limit` (page size) and `cursor`, and return `page`,
`page_size`, `has_more` and `next_cursor` next to their items. Ordering is
stable, so a walk over the pages neither repeats nor skips a row.

| Action | Default page | Max page | Largest offset served |
|---|---|---|---|
| `brain_search` | 20 | 100 | 200 (index hit cap) |
| `brain_record(action=list)` | 50 | 200 | unbounded |
| `brain_catchup(mode=journal)` | 20 | 500 | 1000 (journal read cap) |
| `brain_record(action=relations)` / `brain_judge(action=list)` | 50 | 200 | unbounded |

- Cursors are opaque and bound to the action and to the query that issued them
  (project, kinds, since, status, query text). A cursor from another query is
  refused with `INVALID_ARGUMENTS`, so a caller cannot silently mix two result
  sets. Cursors issued before 0.17 for the journal still decode.
- A cursor past the served offset is refused with `UNSUPPORTED` and
  `try_instead: narrow the query ...`. Nothing is dropped silently.
- `next_cursor` is present only when `has_more` is true. The text content of the
  result repeats it as `more: pass cursor="..." for page N`.

`brain_record(action=get|links)` and `brain_catchup(mode=timeline)` return
bounded content inline rather than as a page: a record has a handful of links,
and a timeline has at most five neighbours per side.

## 8. Tool annotations

Every tool in `tools/list` carries `title`, `annotations`, `outputSchema`, and
`x-lv24-read-actions`/`x-lv24-write-actions` when it mixes read and write
actions. The four hints are always present (MCP defaults would otherwise apply):

| Group | Tools | readOnly | destructive | idempotent | openWorld |
|---|---|---|---|---|---|
| Read | `brain_open`, `brain_context`, `brain_search`, `brain_record`, `brain_catchup`, `brain_review`, `brain_projects`, `brain_graph`, `brain_capabilities` | true | false | true | false |
| Append | `brain_note`, `brain_decision`, `brain_attempt`, `brain_handoff` | false | false | false | false |
| Mixed | `brain_code` | false | false | true | false |
| Destructive | `brain_state`, `brain_judge` | false | true | true | false |
| Destructive | `brain_prompt` | false | true | false | false |

`openWorldHint` is false everywhere: no tool reaches the network.
`idempotentHint` is false for the append tools because a repeated call creates a
second record — use `brain_record` to check before retrying. `brain_prompt` is
not idempotent because `save` appends and `discard` removes. `brain_code` is
idempotent and non-destructive: `index`/`sync` rewrite a derived local index,
and repeating them changes nothing. Hosts must still treat annotations as
untrusted unless they trust this server, and pin the definitions they approved.

A read-only server (`--read-only`, see §9) omits the Append and Destructive rows
entirely and keeps `brain_code` for its read actions only, so `tools/list`
differs from a writable server. That is why the tool list is
`cacheScope: private` for a read-only profile (`public` otherwise): a cache must
not serve one profile's list to another.

Agent writes carry `origin: agent`; with `security.quarantine_agent: true` they
are unreviewed and stay out of `brain_open`/`brain_context` until
`brain_judge(action=review)` clears them.

## 9. Least-privilege profiles

A stdio server sees every caller as the same OS user, so the policy is chosen
when the process starts. `--read-only` wins over `--allow-write`.

| Flags | Read scope | Write scope |
|---|---|---|
| *(none)* | every registered project | the project of the working directory (the one `brain open` picks) |
| `--allow-write` | every registered project | every registered project |
| `--projects=a,b` | only the listed projects | the project of the working directory, if it is listed |
| `--projects=a,b --allow-write` | only the listed projects | only the listed projects |
| `--read-only` (with or without the others) | the listed projects, or every project | none: the seven write tools are not listed at all |

Rules:

- The default is backward compatible for the single-project workflow: an agent
  started in a project checkout reads and writes that project, and can still
  *read* any other registered project. Start the server with `--allow-write`
  when the agent must write to a project other than the checkout it runs in.
- If the working directory is not a project (for example a workspace root) and
  `--allow-write` is absent, every write tool refuses; the server prints one
  line to stderr at startup so the reason is visible in the host log.
- `--projects` takes project ids **or aliases**, comma-separated. The flag is
  resolved once at startup; an unknown or ambiguous entry is a usage error
  (`brain mcp: --projects: "x" is not a registered project id or alias`, exit 2).
  `--list-tools` echoes the entries as given because it needs no `brain.yaml`.
- An allowlisted server refuses a selector outside the list and names the set
  it may use. `brain_projects` is filtered to the same set, and a workspace-root
  `brain_open` (which would list every project) is refused: the agent must name
  one.
- An unscoped `brain_search` or `brain_catchup(mode=journal)` is bound to the
  single allowed project. With two or more allowed projects the call needs an
  explicit `project`, because one cursor cannot describe a result stream that
  spans several projects. Without `--projects` an unscoped search still covers
  every registered project.
- `--read-only` removes `brain_note`, `brain_decision`, `brain_attempt`,
  `brain_handoff`, `brain_state`, `brain_judge` and `brain_prompt` from
  `tools/list`; the nine read tools and the mixed `brain_code` remain. A call
  to an omitted tool is refused before the store is touched, and the read action
  of a writable-only tool (for example `brain_judge(action=list)`) is not served
  either: use `brain_record(action=relations)`. `brain_code` keeps
  `status`/`search`/`node`/`explore`/`impact` and refuses `index`/`sync` with
  `UNSUPPORTED` before the engine is opened.

Refusal shapes (the §5 envelope):

```json
{"code":"UNSUPPORTED","message":"brain_note is not available: this server runs read-only",
 "try_instead":"brain_search or brain_context","current_revision":"","retryable":false}
{"code":"INVALID_ARGUMENTS","message":"project beta is outside this server's allowed projects (alpha, gamma)",
 "try_instead":"check the arguments against inputSchema","current_revision":"","retryable":false}
{"code":"UNSUPPORTED","message":"writes are limited to the project of the working directory (alpha); beta is outside it",
 "try_instead":"start brain mcp with --allow-write to write to another project, or run the equivalent brain command in that checkout",
 "current_revision":"","retryable":false}
```

Recommended profiles:

| Client | Command |
|---|---|
| Trusted agent in one repository | `brain mcp --projects=<id>` |
| Reviewer, auditor, or an agent that must not write | `brain mcp --read-only --projects=<id>` |
| Agent that reads hostile content (issues, mail, PRs) | `brain mcp --read-only --projects=<id>` |
| Curator working across a known set | `brain mcp --allow-write --projects=a,b` |
| Legacy single-project setup | `brain mcp` (write scope: the working-directory project) |

The child-environment policy for MCP servers used as *sources* (`brain
connection` with `transport: stdio`) is in `docs/security.md`.

## 10. Profiles

A profile is a vocabulary, not an add-on: `tools/list` in a profile never mixes
two names for the same intent. `--profile-version` pins the revision
(`major.minor`; `1` pins the major). This build serves `1.0`. An unknown profile
or version is a startup error with exit code 2, never a silent downgrade.

```sh
brain mcp --profile=lv24                     # default: 17 native tools
brain mcp --profile=engram [--strict]        # 27 engram names + brain_capabilities
brain mcp --profile=codegraph                # 8 codegraph names mapped to brain_code
brain mcp --profile=graphify                 # 10 graphify names mapped to brain_code/brain_graph
```

| Profile | Listed names | Read | Append | Destructive | Aliases | Grades |
|---|---:|---:|---:|---:|---|---|
| `lv24` (default) | 17 | 9 | 5 | 3 | – | – |
| `engram` | 28 | 12 | 7 | 9 | 27 | A 3, B 15, C 7, D 2 |
| `codegraph` | 9 | 9 | 0 | 0 | 8 | B 5, C 3 |
| `graphify` | 11 | 11 | 0 | 0 | 10 | B 2, C 5, D 3 |

`brain_capabilities` is the one native name every migration profile keeps: it
has no peer alias and is how a host reads the profile's grades and gaps. It is
counted as a native tool, not as an alias.

**Grades** describe how faithfully an alias serves its legacy caller:

| Grade | Meaning | Per-call behaviour |
|---|---|---|
| **A** | Same inputs and compatible output fields | Exact call; native facts and field names |
| **B** | Inputs accepted, same facts in a different envelope | Translation; dropped fields echoed in `_meta` |
| **C** | Accepted but approximate | Succeeds outside `--strict` with `degraded:true` and a note |
| **D** | Cannot be served | `UNSUPPORTED` + CLI remediation, never a no-op |

This release serves no stub: the codegraph and graphify names are real
translations. The three PR names stay D because this server fetches no
pull-request data; the native `pr_impact` diffs two local refs instead.

`--strict` turns every C into an explicit `UNSUPPORTED` and every dropped
argument into `INVALID_ARGUMENTS` naming the field; D always refuses, with or
without the flag. There is no flag that turns a D into a success.

A successful alias call carries the per-call envelope:

```json
"_meta":{"lv24.brain/compat":{
  "legacy":"mem_save","grade":"B","target":"brain_note",
  "ignored_fields":["session_id","org"]
}}
```

`degraded:true` and `note` appear only where they apply. Dropped fields are
never silently ignored.

## 11. Removed: the `lv24-legacy` bridge

The 0.16 compatibility profile was removed in 0.22.0. `--profile=lv24-legacy`
now fails at startup with exit code 2 and lists the supported profiles; there
is no silent fallback. Use the native `lv24` profile, or `engram` for an engram
client. A retired name called on `lv24` returns `NOT_FOUND` with the replacement
in `try_instead`.

| 0.16 name | Replacement | Translation | Grade |
|---|---|---|---|
| `brain_state_get` | `brain_state(action=get)` | – | A |
| `brain_state_set` | `brain_state(action=set)` | `next`→`next` | A |
| `brain_journal` | `brain_catchup(mode=journal)` | `limit`, `cursor` 1:1 | A |
| `brain_records` | `brain_record(action=list)` | all filters 1:1 | A |
| `brain_relations` | `brain_record(action=relations)` | `id`, `target`, `relation`, `status` 1:1 | A |
| `brain_review_record` | `brain_judge(action=review)` | `all=true`→`ids:["*"]` | A |
| `brain_decision_status` | `brain_judge(action=accept\|reject\|supersede)` | `superseded_by`→`to`; `status=proposed` is refused | B |

A client that still sends `brain_decision(status=proposed)` must record a new
decision with `brain_decision`; the lifecycle never moves a decision back to
proposed.

## 12. `engram` profile (27 aliases + introspection)

Every engram name maps to one native call. A, B and C names are callable; D
names are listed so a client gets a typed `UNSUPPORTED` with the CLI path
instead of "unknown tool". The 0.17 profile already includes the admin names,
so there is no separate `engram,admin` spelling.

| Grade | Names |
|---|---|
| **A** (3) | `mem_get_observation`→`brain_record(get)`, `mem_current_project`→`brain_projects(current)`, `mem_list_projects`→`brain_projects(list)` |
| **B** (15) | `mem_save` (routes by `type` to `brain_note`/`brain_decision`/`brain_attempt`), `mem_search`, `mem_search_prompts`, `mem_context`, `mem_save_prompt`, `mem_session_summary`, `mem_session_finalize`, `mem_review`, `mem_suggest_topic_key`, `mem_judge`, `mem_judge_many`, `mem_compare`, `mem_consolidate`, `mem_stats`, `mem_doctor` |
| **C** (7) | `mem_timeline`, `mem_update`, `mem_pin`, `mem_unpin`, `mem_session_start`, `mem_session_end`, `mem_capture_passive` |
| **D** (2) | `mem_delete`, `mem_merge_projects` |

Translation rules that matter to a client:

- `mem_save` routes by `type`: `decision`/`policy` → `brain_decision`,
  `attempt`/`failed_attempt` → `brain_attempt`, everything else →
  `brain_note(subtype=type)`. `session_id`, `project_choice_reason`,
  `recovery_token` and `org` are dropped and echoed in `ignored_fields`.
- `mem_search` accepts `match_mode=all|any` (the native `match_mode`),
  `response_format=compact|full`, `scope`, `all_projects`, `types`/`type` and
  `since`. Hits carry `type` and `content` next to the native fields.
- `mem_get_observation` accepts `<project>/<record>` and emits `content` and
  `type`. An engram integer id is `NOT_FOUND` with `try_instead: mem_search`;
  it is not guessed from a path.
- `mem_judge` maps the vocabulary (`related`→`relates_to`,
  `conflict`→`conflicts_with`, `unrelated`→`not_conflict`) and accepts a
  `judgment_id` in the deterministic `<source>-><target>` form this server
  exposes.
- `mem_timeline` returns previews (at most 160 characters) and caps
  `before`/`after` at 5 per side. `mem_update` never edits in place: it appends
  a new body under `topic_key` and leaves the old body under History.
- `mem_pin` and `mem_unpin` are graded C: the native
  `brain_judge(action=pin|unpin)` sets or clears the `pinned` flag, but the
  engram ranking boost is not reproduced. No fake frontmatter is written.
- `mem_consolidate` is graded B and plans by default. `phase=apply` needs the op
  `ids` and the plan `fingerprint`; an apply writes merges and archives, all
  reversible through `brain consolidate --rollback <manifest>` (§6.3).
- `mem_session_start` and `mem_session_end` are synthetic: they return a
  `<project>/<date>` handle and write nothing unless a non-empty summary is
  forwarded to `brain_handoff`.
- `mem_delete` and `mem_merge_projects` are D. `try_instead` is
  `brain record trash <id>` (reversible) and
  `brain import --from engram --map-project <from>=<to>` respectively.

## 13. `codegraph` and `graphify` profiles (mapped to the real graph)

Both profiles keep a peer vocabulary working against the same code graph. Every
name is an alias with a published grade; there are no placeholders and no stub
answers. A call without a graph backend (or with `graph.engine: off`) is an
honest `UNSUPPORTED`, never an empty collection.

### 13.1 `codegraph` (8 names)

| Name | Calls | Grade | Notes |
|---|---|---|---|
| `codegraph_explore` | `brain_code(action=explore)` | B | `maxFiles`→`limit`; source under the byte budget |
| `codegraph_search` | `brain_code(action=search)` | B | ranked hits; codegraph's `kind` filter is dropped |
| `codegraph_node` | `brain_code(action=node)` | B | symbol + source + both neighbour lists |
| `codegraph_callers` | `brain_code(action=node)` | C | returns `node.callers`; callees are included too |
| `codegraph_callees` | `brain_code(action=node)` | C | returns `node.callees`; callers are included too |
| `codegraph_impact` | `brain_code(action=impact)` | B | `depth` 1–10 |
| `codegraph_status` | `brain_code(action=status)` | B | adds `indexed:true`, `files`, `nodes`, `edges`, `db_path` |
| `codegraph_files` | `brain_code(action=status)` | C | returns `file_count` and `db_path`; no empty file list is invented |

`projectPath` is a project selector: it must name a registered checkout, the
same rule as `project`. `includeCode`, `kind`, `offset`, `symbolsOnly`, `line`,
`path`, `pattern`, `format` and `includeMetadata` are documented as unsupported
and echoed in `ignored_fields`; `--strict` refuses them.

### 13.2 `graphify` (10 names)

| Name | Calls | Grade | Notes |
|---|---|---|---|
| `query_graph` | `brain_code(action=search)` | C | symbol hits for the question, not a rendered subgraph; traversal/budget fields dropped |
| `get_node` | `brain_code(action=node)` | B | `label` or `node_id` selects the symbol |
| `get_neighbors` | `brain_graph(action=neighbors)` | C | callers and callees with edge kinds; relation filter dropped |
| `graph_stats` | `brain_graph(action=stats)` | B | adds `indexed:true`, `nodes`, `edges`, `communities_available:true` and the community/orphan counts |
| `shortest_path` | `brain_graph(action=path)` | C | BFS with an undirected fallback; `max_hops` bounds the search, `undirected` is reported as dropped |
| `get_community` | `brain_graph(action=communities)` | C | one community with members; ids are this engine's 1-based community ids |
| `god_nodes` | `brain_graph(action=god_nodes)` | C | distinct dependency relationships; `exclude_hubs_percentile` is reported as dropped |
| `get_pr_impact` | `brain_graph(action=pr_impact)` | D | takes a PR number; this server fetches no PR data and diffs local refs instead |
| `list_prs`, `triage_prs` | – | D | no PR data is stored; `try_instead` names `brain_code(action=impact)` and `gh pr list` |

The D names refuse with a typed `UNSUPPORTED` and a working alternative. Their
`target`/`action` still record the intended mapping, so a later release can
serve them without renaming a profile.

`brain_graph(action=cycles)` and `brain_graph(action=diff)` are native actions
of the `brain_graph` tool. Graphify publishes no cycle or graph-diff tool in its
ten-name vocabulary (its `graph_diff` is an internal snapshot helper), so no
alias is invented for them; a graphify client reaches them through the native
`brain_graph` name. The `codegraph` profile keeps its eight names unchanged.

## 14. Resources

Resources are host-controlled context, so they are read-only, project-scoped and
capped. `resources/list` advertises the concrete state resource of every allowed
project plus the two aggregate resources; `resources/templates/list` advertises
the two parameterised memory forms and, when the backend can open the graph, the
`code://` symbol form.

| URI | MIME | Returns | Cap | TTL |
|---|---|---|---:|---:|
| `memory://projects` | `application/json` | Allowed projects, their kind and checkout paths | 8 KiB | 30 s |
| `memory://project/{id}/state` | `text/markdown` | Curated state as Markdown (`state.md` projection) | 16 KiB | 30 s |
| `memory://record/{id}` | `text/markdown` | One record with YAML frontmatter, body, links and relations | 1 MiB | 300 s |
| `memory://review` | `application/json` | Stale-knowledge findings, one project with `?project=`, every allowed project otherwise | 64 KiB | 60 s |
| `code://{project}/symbol/{id}` | `text/markdown` | One indexed symbol: declaration, numbered source, direct callers and callees | 32 KiB | 30 s |

Rules:

- Every project-scoped answer is `cacheScope: "private"`; nothing is shared
  across server profiles or allowlists.
- A project outside `--projects` is `NOT_FOUND`, never a redirect; a project the
  brain does not know is `NOT_FOUND` too.
- `memory://record/{id}` probes the allowed projects unless `?project=` names
  one. A quarantined record is withheld unless
  `?include_unreviewed=true`; an explicit id is otherwise an explicit request,
  matching `brain_record(action=get)`.
- A body above its cap is cut at a UTF-8 boundary, prefixed with
  `[truncated: showing first N of M bytes]`, and the `_meta` entry
  `lv24.brain/resource` repeats `bytes`, `cap` and `truncated`.
- `?limit=` is clamped, not rejected. An unknown URI is `NOT_FOUND`. Resource
  errors are JSON-RPC `InvalidParams` with the same five-key refusal envelope in
  `error.data`.
- `code://` is advertised only by a backend that implements the graph capability
  and serves `Node`; otherwise the template is not listed and a read is
  `UNSUPPORTED`. The id is a node id or a symbol name the graph resolves; an
  unknown id is `NOT_FOUND`. A graph engine that is off, absent or unindexed
  refuses with the same typed envelope as `brain_code`.
- Prompts are **never** exposed as resources: the prompt zone is local-only and
  the least governed data in the brain.

## 15. Inspecting the contract

```sh
brain mcp --list-tools --json                          # profile, policy, tools, aliases, capabilities, digest
brain mcp --list-tools --profile=engram                # the engram vocabulary with grades
brain mcp --list-tools --json --profile=engram --strict
brain mcp --list-tools --json --profile=codegraph      # the 8 mapped codegraph names
brain mcp --list-tools --json --read-only              # the 10 tools a read-only server exposes
```

The JSON document keeps the 0.16 keys (`server`, `protocol_versions`, `policy`,
`tools`, `omitted_tools`) and adds:

- `profile`: `{name, version, strict, native}`.
- `tools[*].policy`: `read`, `append` or `destructive`; `brain_code` is an
  idempotent non-destructive write, classified as `append` for the closed
  vocabulary while `x-lv24-read-actions`/`x-lv24-write-actions` carry the exact
  split.
- `tools[*].legacy`, `target`, `action`, `grade`, `unsupported`, `note`: the
  alias table as tool metadata.
- `tools[*].x-lv24-read-actions` and `x-lv24-write-actions`: the actions of a
  mixed tool that never mutate and the ones that do.
- `aliases`: the legacy-to-native table for every migration profile, including D
  names whose remediation is the CLI path.
- `capabilities`: the same payload `brain_capabilities` returns, including
  `tools.grades` (`A`/`B`/`C`/`D` counts) and `gaps`.
- `digest`: `sha256:…` over the profile, effective tools, aliases and omitted
  names. The same binary, profile and policy print the same bytes twice; a
  policy change changes the digest.

The listing needs no `brain.yaml`, no data directory and no backend, so it can be
diffed in CI, hashed for review, or compared across machines. The running server
reports the same vocabulary.

## 16. Migration from 0.16

The default profile lists 17 names instead of 19. Calling a retired name in the
default profile returns `NOT_FOUND` with the replacement in `try_instead`:

| 0.16 name | 0.17 call |
|---|---|
| `brain_state_get` | `brain_state(action=get)` |
| `brain_state_set` | `brain_state(action=set)` |
| `brain_journal` | `brain_catchup(mode=journal)` (old `limit`/`cursor` still work) |
| `brain_records` | `brain_record(action=list)` (filters and output unchanged) |
| `brain_relations` | `brain_record(action=relations)`; pending view also `brain_judge(action=list)` |
| `brain_review_record` | `brain_judge(action=review)`; `all=true` becomes `ids:["*"]` |
| `brain_decision_status` | `brain_judge(action=accept\|reject\|supersede)`; `superseded_by` becomes `to` |

Two argument changes are refused rather than silently ignored:

- `brain_decision(status=…)` returns `INVALID_ARGUMENTS` with
  `try_instead: brain_judge(action=accept|reject|supersede)`. Recording an
  accepted decision through the append tool would leave it `proposed`.
- `brain_records` and the other retired names are no longer valid tool names in
  `lv24`.

A host that has not been updated in the same release can use the migration
profiles without touching the data:

```sh
brain mcp --profile=engram                      # a client written against engram
brain import --from engram --dry-run            # preview an engram migration
```

The `lv24-legacy` profile was removed in 0.22.0; `--profile=lv24-legacy` is now
a startup error. The `engram` profile stays served for two minor releases after
0.17.

## 17. Not implemented, and why

- **Pull-request data** — `list_prs`, `triage_prs` and the graphify
  `get_pr_impact` name stay `UNSUPPORTED`: this server fetches and stores no
  GitHub/GitLab pull requests. The native `brain_graph(action=pr_impact)`
  answers the same underlying question for two local git refs and never writes
  to the checkout.
- **Prompt discard** is refused with guidance. Consolidation is served by
  `brain_judge(action=consolidate)` in proposal-only form (§6.3), and
  `pin`/`unpin` by `brain_judge(action=pin|unpin)` (§12).
- **In-place record edit, MCP delete and hard delete** stay CLI-only. The
  Markdown truth is append-only: evolution is `brain_note(topic_key=…)`,
  lifecycle is `brain_judge`, and removal is `brain record trash` (reversible).
- **Prompts as resources** are excluded by design; the local prompt zone is
  available only through `brain_prompt` and is never committed or indexed.
- **Per-file code listings** are not served: the index answers counts and
  symbol/edge queries, so `codegraph_files` reports `file_count` instead of a
  fabricated tree.
