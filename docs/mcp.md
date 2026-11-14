# MCP server contract

`brain mcp` serves the brain over stdio as a Model Context Protocol server. It
speaks two protocol revisions from one process, so current hosts and older
handshake clients both work: **`2026-07-28`** (stateless, per-request metadata)
and **`2025-06-18`** (handshake). This page is the contract for hosts, for
`skills` and for anyone pinning our tool definitions.

The server exposes 16 tools and no resources or prompts. Stdout carries protocol
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

`tools/list` and `server/discover` are deterministic, identical for every caller
and do not vary per connection, so their results carry
`ttlMs` (300000 for the tool list, 3600000 for discovery) and
`cacheScope: "public"`. Tool results are never cached: each reflects current
state.

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
| Mutate | `brain_state_set`, `brain_decision_status` | false | true | true | false |

`openWorldHint` is false everywhere: no tool reaches the network. `idempotentHint`
is false for the append tools because a repeated call creates a second record —
use `brain_record` to check before retrying. Hosts must still treat annotations
as untrusted unless they trust this server, and pin the definitions they
approved.

## 8. Inspecting the contract without serving

```sh
brain mcp --list-tools --json   # name, title, description, annotations, schemas, limits
brain mcp --list-tools          # one line per tool: name, read|write, title
```

The listing needs no `brain.yaml`, no data directory and no backend, so it can be
diffed in CI, hashed for review, or compared across releases. The same set is
served by `tools/list`, which is deterministic and cacheable.

## 9. Running the harness

The protocol tests speak raw newline-delimited JSON-RPC over a pipe; they use no
external SDK, so they check the wire shape rather than a library's view of it.

```sh
go test ./internal/mcpserver -run 'TestProtocol|TestContract' -v
go test ./cmd/brain -run TestMCP -v
```

They cover: `server/discover` → `tools/list` → `tools/call` without initialize;
`initialize` → `tools/list` → `tools/call` with the legacy shape; an unsupported
version; a three-page walk without duplicates or omissions; the refusal
envelope; annotations and output schemas on all 16 tools; and the MRTR
confirm / decline / forged-state paths.

Manual probe against a real instance:

```sh
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"brain_open","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}' \
| brain mcp --config ~/brain/brain.yaml
```

## 10. Not implemented, and why

Deliberate gaps, so a host is not surprised:

- **Resources and prompts.** Tools only. A `memory://` resource surface is
  planned together with least-privilege profiles; until then the same data is
  available through tools.
- **`subscriptions/listen` and list-changed notifications.** The tool set is
  static, so `listChanged` is false and no stream is opened.
- **Tasks extension, sampling, roots, logging.** Not needed for a local memory
  server; sampling and roots are deprecated in 2026-07-28.
- **Streamable HTTP transport.** `brain mcp` is stdio only. The team deployment
  uses the REST API, not MCP over HTTP.
- **Tool-list profiles.** `--read-only` / `--allow-write` / `--projects` from the
  least-privilege work package are not wired yet. Today every caller sees the
  same 16 tools, which is why the list is `cacheScope: public`. When profiles
  land, the list will vary by authorization and the cache scope becomes
  `private`; the per-request rule from 2026-07-28 already allows that.
