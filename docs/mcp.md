# MCP server contract

`brain mcp` serves the brain over stdio as a Model Context Protocol server. It
speaks two protocol revisions from one process, so current hosts and older
handshake clients both work: **`2026-07-28`** (stateless, per-request metadata)
and **`2025-06-18`** (handshake). This page is the contract for hosts, for
`skills` and for anyone pinning our tool definitions.

The default profile is `lv24`: **15 tools**, no resources and no prompts. Stdout
carries protocol messages only; diagnostics go to stderr.

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
  "capabilities":{"tools":{"listChanged":false}},
  "instructions":"Company Brain is the curated memory of this project. ...",
  "_meta":{"io.modelcontextprotocol/serverInfo":{"name":"lv24-brain","version":"0.17.0"}},
  "ttlMs":3600000,
  "cacheScope":"public"
}}
```

## 3. Cache hints

`tools/list` and `server/discover` carry `ttlMs` (300000 for the tool list,
3600000 for discovery). `server/discover` is `cacheScope: "public"`: every
profile advertises the same revisions and capabilities. `tools/list` is
`public` for a writable server and `private` under `--read-only`, because a
read-only profile lists eight tools where a writable one lists fifteen (§9).
Tool results are never cached: each reflects current state.

## 4. Multi Round-Trip Requests

A 2026 client has no server-initiated requests. When a call needs the user to
confirm a curation change, the server answers with `resultType:
"input_required"` and an elicitation request; the client retries the same call
with a new JSON-RPC id, the `inputResponses`, and the echoed `requestState`.

Two actions ask for confirmation: `brain_judge(action=judge)`, because the
verdict replaces any existing relation between the pair, and
`brain_judge(action=supersede)`, because it changes which decision is current:

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
| `brain_judge` | `pin`, `unpin` | `brain note "<why this record matters>"` (pinning lands in 0.18) |
| `brain_judge` | `consolidate` | `brain record supersede <old-id> --by <new-id>` |

`brain_capabilities` returns the same list, so a host can discover a gap
without calling it.

## 6. The 15 tools

Each tool has one intent; a write action never hides inside a tool annotated
read-only. `action` defaults to the historical read behaviour.

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
| `brain_judge` | destructive | `judge` (default), `list`, `judge_many`, `compare`, `review`, `accept`, `reject`, `supersede`, `pin`, `unpin`, `consolidate` | change the status of existing knowledge |
| `brain_projects` | read | `list` (default), `current`, `register`, `merge` | project registry; `current` reports the resolution source |
| `brain_prompt` | destructive | `save`, `search`, `discard` | opt-in local-only prompt zone |
| `brain_capabilities` | read | – | profile, engine, tools, features, limits, gaps, refusal codes, endpoints |

The write postures are exact: **8 read, 4 append-only, 3 destructive**
(`brain_state`, `brain_judge`, `brain_prompt`). `brain_projects` is read-only:
`register` is gated by the write scope and `merge` is refused; on a read-only
server every write action of a mixed tool is refused before the backend.
`x-lv24-read-actions` names the actions of a mixed tool that never mutate, so a
host does not have to infer it from the four hints.

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
`x-lv24-read-actions` when it mixes read and write actions. The four hints are
always present (MCP defaults would otherwise apply):

| Group | Tools | readOnly | destructive | idempotent | openWorld |
|---|---|---|---|---|---|
| Read | `brain_open`, `brain_context`, `brain_search`, `brain_record`, `brain_catchup`, `brain_review`, `brain_projects`, `brain_capabilities` | true | false | true | false |
| Append | `brain_note`, `brain_decision`, `brain_attempt`, `brain_handoff` | false | false | false | false |
| Destructive | `brain_state`, `brain_judge` | false | true | true | false |
| Destructive | `brain_prompt` | false | true | false | false |

`openWorldHint` is false everywhere: no tool reaches the network.
`idempotentHint` is false for the append tools because a repeated call creates a
second record — use `brain_record` to check before retrying. `brain_prompt` is
not idempotent because `save` appends and `discard` removes. Hosts must still
treat annotations as untrusted unless they trust this server, and pin the
definitions they approved.

A read-only server (`--read-only`, see §9) omits the Append and Destructive rows
entirely, so `tools/list` differs from a writable server. That is why the tool
list is `cacheScope: private` for a read-only profile (`public` otherwise): a
cache must not serve one profile's list to another.

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
  `tools/list`; exactly the eight read tools remain. A call to a omitted tool is
  refused before the store is touched, and the read action of a writable-only
  tool (for example `brain_judge(action=list)`) is not served either: use
  `brain_record(action=relations)`.

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

## 10. Profiles and inspecting the contract

`brain mcp --profile=lv24` is the default and, in this release, the only
profile. `--profile-version=1` pins the major revision; an unknown profile or
version fails at startup with a plain error instead of serving another
vocabulary. `--strict` is reserved for migration profiles and has no effect on
`lv24`.

```sh
brain mcp --list-tools --json                   # profile, policy, 15 tools, capabilities
brain mcp --list-tools                          # one line per tool: name, policy, title; then the profile and policy
brain mcp --list-tools --json --read-only       # the 8 tools a read-only server would expose
```

The JSON document keeps the 0.16 keys (`server`, `protocol_versions`, `policy`,
`tools`, `omitted_tools`) and adds:

- `profile`: `{name, version, strict}`.
- `tools[*].policy`: `read`, `append` or `destructive`.
- `tools[*].x-lv24-read-actions`: the actions of a mixed tool that never mutate.
- `capabilities`: the same payload `brain_capabilities` returns.

The listing needs no `brain.yaml`, no data directory and no backend, so it can be
diffed in CI, hashed for review, or compared across releases. The same set is
what the running server reports.

## 11. Migration from 0.16

The default profile lists 15 names instead of 19. Seven names were retired;
calling one returns `NOT_FOUND` with the replacement in `try_instead`:

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
- `brain_records` and the other retired names are no longer valid tool names.

A host that has not been updated can keep the old names only by pinning the old
engine; this build serves the `lv24` profile exclusively.

## 12. Not implemented, and why

- **Resources and prompts** are not advertised. The 15 tools are the contract;
  `memory://` resources and the two workflow prompts are planned for a later
  release together with the cache and allowlist work they need.
- **`brain_code` and `brain_graph`** do not exist; no code graph or knowledge
  graph is served. `brain_capabilities` reports both as false rather than
  returning empty collections.
- **Pin, unpin, consolidation and prompt discard** are refused with guidance
  (§5); the underlying app methods land later.
- **In-place record edit, MCP delete and hard delete** stay CLI-only. The
  Markdown truth is append-only: evolution is `brain_note(topic_key=…)`,
  lifecycle is `brain_judge`, and removal is `brain record trash` (reversible).
