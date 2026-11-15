# MCP server contract

`brain mcp` serves the brain over stdio as a Model Context Protocol server. It
speaks two protocol revisions from one process, so current hosts and older
handshake clients both work: **`2026-07-28`** (stateless, per-request metadata)
and **`2025-06-18`** (handshake). This page is the contract for hosts, for
`skills` and for anyone pinning our tool definitions.

The server exposes 17 tools and no resources or prompts. Stdout carries protocol
messages only; diagnostics go to stderr.

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
  "_meta":{"io.modelcontextprotocol/serverInfo":{"name":"lv24-brain","version":"0.15.0"}},
  "ttlMs":3600000,
  "cacheScope":"public"
}}
```

## 3. Cache hints

`tools/list` and `server/discover` are deterministic and do not vary per
connection, so their results carry `ttlMs` (300000 for the tool list, 3600000
for discovery). `server/discover` is `cacheScope: "public"`: every profile
advertises the same revisions and capabilities. `tools/list` is `public` for a
writable server and `private` under `--read-only`, because a read-only profile
lists ten tools where a writable one lists seventeen (§8). Tool results are
never cached: each reflects current state.

## 4. Multi Round-Trip Requests

A 2026 client has no server-initiated requests. When a call needs the user to
confirm a curation change, the server answers with `resultType:
"input_required"` and an elicitation request; the client retries the same call
with a new JSON-RPC id, the `inputResponses`, and the echoed `requestState`.

The only confirmation today is `brain_decision_status` with `superseded_by`
(marking a decision superseded changes what the project treats as current):

```json
{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{
  "name":"brain_decision_status",
  "arguments":{"id":"dec-2026-11-10-sqlite","superseded_by":"dec-2026-11-14-ulid"},
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
| `NOT_FOUND` | Unknown project, record or decision | no |
| `CONFLICT` | The write collides with existing data | yes, after re-reading |
| `UNSUPPORTED` | The mode or the call window does not serve this | no |
| `DECLINED` | The user refused to confirm; nothing changed | no — do not retry blindly |
| `FAILED` | Unexpected failure | no |

`current_revision` is empty in local mode, where records have no revision;
it is populated by backends that report one. `outputSchema` describes successful
results only: a refusal is the envelope above and is not validated against it.

## 6. Pagination

The list-like tools take `limit` (page size) and `cursor`, and return `page`,
`page_size`, `has_more` and `next_cursor` next to their items. Ordering is
stable, so a walk over the pages neither repeats nor skips a row.

| Tool | Default page | Max page | Largest offset served |
|---|---|---|---|
| `brain_search` | 20 | 100 | 200 (index hit cap) |
| `brain_records` | 50 | 200 | unbounded |
| `brain_journal` | 20 | 500 | 1000 (journal read cap) |

- Cursors are opaque and bound to the tool and to the query that issued them
  (project, kinds, since, status, query text). A cursor from another query is
  refused with `INVALID_ARGUMENTS`, so a caller cannot silently mix two result
  sets.
- A cursor past the served offset is refused with `UNSUPPORTED` and
  `try_instead: narrow the query ...`. Nothing is dropped silently.
- `next_cursor` is present only when `has_more` is true. The text content of the
  result repeats it as `more: pass cursor="..." for page N`.

`brain_record` returns its links and backlinks inline rather than as a page: both
are bounded by the record's own reference count, which is a handful of lines in
one Markdown file, so a cursor would add a page turn without bounding anything.

## 7. Tool annotations

Every tool in `tools/list` carries `title`, `annotations` and `outputSchema`.
The four hints are always present (MCP defaults would otherwise apply):

| Group | Tools | readOnly | destructive | idempotent | openWorld |
|---|---|---|---|---|---|
| Read | `brain_open`, `brain_context`, `brain_search`, `brain_journal`, `brain_projects`, `brain_state_get`, `brain_catchup`, `brain_review`, `brain_records`, `brain_record` | true | false | true | false |
| Append | `brain_note`, `brain_decision`, `brain_attempt`, `brain_handoff` | false | false | false | false |
| Mutate | `brain_state_set`, `brain_decision_status`, `brain_review_record` | false | true | true | false |

`openWorldHint` is false everywhere: no tool reaches the network. `idempotentHint`
is false for the append tools because a repeated call creates a second record —
use `brain_record` to check before retrying. Hosts must still treat annotations
as untrusted unless they trust this server, and pin the definitions they
approved.

A read-only server (`--read-only`, see §8) omits the Append and Mutate rows
entirely, so `tools/list` differs from a writable server. That is why the tool
list is `cacheScope: private` for a read-only profile (`public` otherwise):
a cache must not serve one profile's list to another.

`brain_review_record` is the temporary 0.15 quarantine tool
(`id`/`all`/`trust`); 0.17 merges it into `brain_judge`. Agent writes carry
`origin: agent`; with `security.quarantine_agent: true` they are unreviewed and
stay out of `brain_open`/`brain_context` until a review, which the tool performs
behind the same write path as the other mutating tools.

## 8. Least-privilege profiles

A stdio server sees every caller as the same OS user, so the profile is chosen
when the process starts. Three flags narrow it; `--read-only` wins over
`--allow-write`.

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
- An unscoped `brain_search` or `brain_journal` is bound to the single allowed
  project. With two or more allowed projects the call needs an explicit
  `project`, because one cursor cannot describe a result stream that spans
  several projects. Without `--projects` an unscoped search still covers every
  registered project.
- `--read-only` removes `brain_note`, `brain_decision`, `brain_attempt`,
  `brain_handoff`, `brain_state_set`, `brain_decision_status` and
  `brain_review_record` from `tools/list`. A call to one is refused before the
  store is touched: it never runs even if a host still sends it.

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

## 9. Inspecting the contract without serving

```sh
brain mcp --list-tools --json                   # name, title, description, annotations, schemas, limits, policy
brain mcp --list-tools                          # one line per tool: name, read|write, title; then the policy
brain mcp --list-tools --json --read-only       # the list a read-only server would expose
```

The listing needs no `brain.yaml`, no data directory and no backend, so it can be
diffed in CI, hashed for review, or compared across releases. The same set is
served by `tools/list`, which is deterministic and cacheable. The `policy`
object reports `read_only`, `allow_write`, `projects` and `all_projects`;
`omitted_tools` names the tools the profile hides, so the full contract stays
visible when a profile is applied.

## 10. Running the harness

The protocol tests speak raw newline-delimited JSON-RPC over a pipe; they use no
external SDK, so they check the wire shape rather than a library's view of it.

```sh
go test ./internal/mcpserver -run 'TestProtocol|TestContract' -v
go test ./cmd/brain -run TestMCP -v
```

They cover: `server/discover` → `tools/list` → `tools/call` without initialize;
`initialize` → `tools/list` → `tools/call` with the legacy shape; an unsupported
version; a three-page walk without duplicates or omissions; the refusal
envelope; annotations and output schemas on all 17 tools; the MRTR confirm /
decline / forged-state paths; and the least-privilege profile: a read-only
`tools/list`, the refusal of a hidden write tool, an allowlist on reads and
writes, and the working-directory write scope.

Manual probe against a real instance:

```sh
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"brain_open","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}' \
| brain mcp --config ~/brain/brain.yaml
```

## 11. Not implemented, and why

Deliberate gaps, so a host is not surprised:

- **Resources and prompts.** Tools only. A `memory://` resource surface is
  planned; until then the same data is available through the tools above.
- **`subscriptions/listen` and list-changed notifications.** The tool set is
  fixed when the process starts, so `listChanged` is false and no stream is
  opened. Changing the profile requires a restart.
- **Tasks extension, sampling, roots, logging.** Not needed for a local memory
  server; sampling and roots are deprecated in 2026-07-28.
- **Streamable HTTP transport.** `brain mcp` is stdio only. The team deployment
  uses the REST API, not MCP over HTTP.
- **Per-call approval of writes.** A profile is set per process, not per call.
  Use `--read-only` for an agent that must not write, and the quarantine flow
  (`brain_review_record`) for agent writes that need review.
