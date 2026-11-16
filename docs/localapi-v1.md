# Local control API contract (`localapi-v1`)

The authenticated loopback API served by `brain agent` is a frozen wire
contract. The machine-readable schema is
`internal/adapters/localapi/openapi.json` and declares
`"x-contract": "localapi-v1"`. The documented operation set is pinned by
`internal/adapters/localapi/testdata/localapi-v1.operations`.

## Transport, Host and authentication

- The listener is IPv4 loopback only on an ephemeral port. The `Host` header
  must be exactly `127.0.0.1:<port>` from `agent/runtime.json`; `localhost`,
  `[::1]`, other ports, suffixes and DNS names are rejected with `403`.
- Every request must carry exactly one `Authorization: Bearer <token>` header.
  Missing, duplicate or wrong credentials return `401` with
  `WWW-Authenticate: Bearer`.
- Any `Origin` or `Referer` header, including an empty one, returns `403`.
  There is no CORS support, no preflight handling and no browser mode.
- JSON request bodies must use `Content-Type: application/json`, are limited to
  1 MiB and reject unknown fields. Requests and responses are never logged.
- Local-only: this contract does not describe server mode, which has its own
  authenticated HTTP API.

## Error envelope

Every error response is JSON:

```json
{"error":{"code":"method_not_allowed","message":"method is not allowed for this endpoint"}}
```

| Status | Codes | Meaning |
| --- | --- | --- |
| 400 | `validation`, `invalid_json`, `confirmation_required`, `unsupported` | Invalid input, missing explicit confirmation, or a capability the current mode cannot serve. |
| 401 | `unauthorized` | Bearer token missing, duplicated or wrong. |
| 403 | `forbidden` | Host, `Origin` or `Referer` rejected, or the write targets a protected project or glob. |
| 404 | `not_found` | Unknown route, connection, schedule, run, record or proposal. |
| 405 | `method_not_allowed` | Wrong method; `Allow` lists the supported methods. |
| 409 | `conflict`, `duplicate` | Resource busy, conflicting with existing data, a stale record or state revision, or an exact duplicate record. |
| 413 | `body_too_large`, `limit_exceeded` | Request body or a record field exceeds the configured limit. |
| 415 | `unsupported_media_type` | Body is not `application/json`. |
| 500 | `internal_error`, `unavailable` | Sanitized failure; never contains internal text. |

An unregistered `project` selector is `400 validation`, not `404`: project ids,
aliases and paths follow the engine's resolution rules, and a typo is invalid
input rather than a missing record.

`code` is stable and safe to branch on. `message` is diagnostic only; do not
parse it. Clients must tolerate future codes by falling back to the HTTP status.
Method mismatches are always `405` with an `Allow` header (for example
`Allow: GET, PUT`), never `400`.

A refused state compare-and-swap is the one error that carries structured data:
`409` with `code: "conflict"` and `error.current_revision` set to the revision
that is stored now.

## Route table

Every operation of `localapi-v1`, pinned by `testdata/localapi-v1.operations`
and by a test that reads this table. `1.x` may add rows; it may never remove or
rename one.

| Method | Path |
| --- | --- |
| `GET` | `/v1/health` |
| `GET` | `/v1/connections` |
| `POST` | `/v1/connections` |
| `GET` | `/v1/connections/{id}` |
| `PATCH` | `/v1/connections/{id}` |
| `DELETE` | `/v1/connections/{id}` |
| `POST` | `/v1/connections/{id}/credentials/{field}` |
| `POST` | `/v1/connections/{id}/test` |
| `POST` | `/v1/connections/{id}/run` |
| `GET` | `/v1/runs` |
| `GET` | `/v1/runs/{id}` |
| `POST` | `/v1/runs/{id}/cancel` |
| `GET` | `/v1/runs/{id}/log` |
| `GET` | `/v1/schedules` |
| `PUT` | `/v1/schedules/{connectionId}` |
| `GET` | `/v1/projects` |
| `GET` | `/v1/projects/{id}/state` |
| `PUT` | `/v1/projects/{id}/state` |
| `GET` | `/v1/records` |
| `GET` | `/v1/records/{id}` |
| `PATCH` | `/v1/records/{id}` |
| `POST` | `/v1/records/{id}/trash` |
| `POST` | `/v1/records/{id}/restore` |
| `GET` | `/v1/search` |
| `GET` | `/v1/review` |
| `POST` | `/v1/review/fix` |
| `GET` | `/v1/quarantine` |
| `POST` | `/v1/quarantine/{id}/review` |
| `GET` | `/v1/proposals` |
| `GET` | `/v1/proposals/{project}/{id}` |
| `POST` | `/v1/proposals/{project}/{id}/promote` |
| `POST` | `/v1/proposals/{project}/{id}/dismiss` |
| `GET` | `/v1/catchup` |
| `GET` | `/v1/settings` |
| `PUT` | `/v1/settings` |
| `POST` | `/v1/engine/update` |
| `POST` | `/v1/shutdown` |

## Memory routes (desktop 1.2)

The memory routes expose the curated record zone, ranked search, project state,
the review queue and catchup. They are read-only except the state write, the
explicit proposal promote/dismiss calls, the safe review fix, the quarantine
review and the reversible record trash/restore.

- Field names are `snake_case` and follow the engine's search-v2 and MCP
  contract (`match_reasons`, `next_cursor`, `has_more`). The older routes
  predate that convention and keep their frozen `camelCase` fields.
- `project` is optional on `/v1/records`, `/v1/search` and `/v1/review`: omitting
  it (or passing `all_projects=true`) reads every project the API serves. The
  agent's project allowlist bounds that scope; a non-empty allowlist with
  several projects requires an explicit `project`, and a project outside the
  allowlist is `400 validation`. Named selectors are resolved to their
  canonical project id before the allowlist is checked.
- An all-projects listing returns the global newest page, not one page per
  project. Every row already carries `project_id`; an all-projects review sets
  `project_id` to `*`, lists its scope in `project_ids` and sets
  `all_projects: true`, and its findings and due reviews carry their own
  `project_id`. The aggregate review caps findings and due reviews at 1000 and
  proposal candidates at 100; `findings_truncated`, `due_reviews_truncated` and
  `proposals_truncated` report that a list hit its cap.
- Raw-zone text is never returned. `/v1/search` refuses `kind=dump` and
  `kind=item`, `/v1/records` lists curated records only, and every response
  string passes through the handler redactor.

### Records

`GET /v1/records` lists records newest first. Filters: `kind` (repeated or
comma-separated note/decision/attempt/handoff), `status`, `scope`, `subtype`,
`topic`, `since`, `due_review`. A row carries `id`, `project_id`, `kind`,
`title`, `status`, `path`, `permalink`, the classification, `created_at`,
`updated_at`, `validity`, `provenance` and `review`. `path` is the logical
data-directory path (`projects/<project>/<dir>/<id>.md`); the record id is the
stable file stem even when the physical file also carries a date and slug.

`GET /v1/records/{id}?project=&max_tokens=&history=` adds the `body` bounded by
`max_tokens` (default 4000, 1–32000) with `body_truncated` and `body_tokens`,
`links` (resolved `[[...]]` references), `backlinks`, typed `relations` and,
with `history=1`, the timeline of the same kind around the record (oldest
first, superseded records included). A relation row carries `target_kind` when
the target is still live, so a client can deep-link it. An unknown record id
returns `404`; an unregistered project selector returns `400`.

`POST /v1/records/{id}/trash?project=` and `.../restore?project=` move a record
through the same lifecycle use case as `brain record trash` and
`brain record restore`. Both return the record row, never delete a file, and
are reversible. An unknown live record is `404`; a hand-written note without an
id marker is `400 validation` because it is listed but not addressable. A trash
or restore move takes no compare-and-swap; the record keeps its ID and file, and
a restore after an edit brings back the edited text.

`PATCH /v1/records/{id}?project=` edits the text of one record in place. The
request is `{title?, body?, tags?, expected_updated_at?, allow_duplicate?}` and
at least one of `title`, `body` and `tags` must be present. `body` is the note
text, the decision context, the attempt result or the handoff summary; a
handoff has no title, so `title` on one is `400 validation`. `tags` is a note
field and replaces the stored list. The ID, kind, project, file path,
`created_at`, provenance, validity, relations, review state and classification
never change, the previous body is kept in the record's bounded history, and an
unreviewed record stays unreviewed.

`expected_updated_at` is the `updated_at` the client read. When it is present
the write is a compare-and-swap inside the record lock: a stale value returns
`409` with `error.current_revision` naming the stored revision, and nothing is
written. Omitting it keeps last-write-wins. `allow_duplicate: true` accepts text
that another live record of the same project and kind already holds; without it
an exact duplicate is `409` with code `duplicate`. The response is the edited
record with its body, links, relations and validity.

Edit refusals keep their typed envelopes: `400 validation` (empty patch, bad
timestamp, handoff title, empty decision/attempt title), `403 forbidden` (protected
project or glob), `404 not_found`, `409 conflict` (stale revision) or
`409 duplicate`, and `413 limit_exceeded` (title/body/tags over the configured
write limits). Changing a decision status and linking a supersede chain remain
`brain record status` and `brain record supersede`; the PATCH route never
changes them. The local and git stores implement the edit; a server-backed store
reports `400 unsupported` until it has the same use case.

### Search

`GET /v1/search?q=&project=&kind=&limit=&cursor=&mode=&min_score=&explain=`
returns the search-v2 shape: `query`, `relaxed`, an optional `notice`, `hits`
(`id`, `kind`, `title`, `preview`, `score`, `match_reasons`, optional
`explain`, validity) and the page fields. `mode` is `all` or `any`; `min_score`
is between 0 and 1; `explain` adds the weighted signal contributions.

### State and CAS

`GET /v1/projects/{id}/state` returns the state fields plus `revision`, an
opaque value derived from the last update (`"none"` before the first write).

`PUT /v1/projects/{id}/state` changes only the given fields; an empty string or
array clears one and omitting a field keeps it. `fresh_until` is an RFC3339
time, `""` to clear it, or omitted to keep it. When `expected_revision` is set,
the write is a compare-and-swap: it runs inside the store's atomic state update
and a mismatch returns `409` with `error.current_revision` and writes nothing.
Omitting `expected_revision` keeps the CLI's last-write-wins behaviour.

### Review

`GET /v1/review?project=` is read-only: `findings` (severity, message, why,
next, confirm, evidence, fixable, fix, ordered error → warn → info),
`due_reviews`, a `proposals` summary (pending count and up to 20 candidates) and
`quarantined_count`.

`POST /v1/review/fix?project=` applies exactly the safe repairs of
`brain review --fix` and nothing else: it adopts the next action of a later
handoff (`set_next_action`) and removes open questions a later record answers
(`drop_question`), keeping the removed questions in a review note. Semantic
repairs are never planned or applied.

- The default is a dry run: `dry_run` defaults to `true` and the response lists
  the target `findings`, the exact `changes` and the stored `state` unchanged.
- Writing requires the explicit `apply: true`. `dry_run: true` with `apply:
  true` is `400 validation`; `dry_run: false` without `apply` is `400
  confirmation_required`.
- `kinds` optionally narrows the fix to named finding kinds. The response is
  the `findings` targeted, the `changes` (`kind`, `action`, `field`, `value`,
  `evidence`) and the resulting state.

### Quarantine

`GET /v1/quarantine?project=&limit=&cursor=` is the review inbox: records that
are excluded from trusted context because they are unreviewed external writes
(`reason: unreviewed`) or carry an origin this engine does not know
(`reason: unknown_origin`). Rows carry `id`, `kind`, `project_id`, `title`,
`reason`, `origin`, `trust`, `created_at` and `age_seconds`. Bodies are never
returned here.

`POST /v1/quarantine/{id}/review?project=` marks one record reviewed and
optionally replaces its `trust` (`verified`, `stated`, `inferred`), mirroring
`brain record review <id>`. Reviewing an unknown origin does not clear its
quarantine: review cannot repair a malformed origin, which still needs a hand
edit. The review call is idempotent and takes no compare-and-swap; the record
edit route is the one place that exposes `expected_updated_at`.

### Proposals and promotion

`GET /v1/proposals?project=&status=&kind=&limit=&cursor=&include_body=&max_tokens=`
lists candidates with `source_stale`, `source_orphan`, `local_only` and a
diagnostic `source_health`. Proposal bodies stay out of the list by default.
With `include_body=true` each item carries the redacted candidate text as
`body` with `body_truncated`, `body_tokens` and `max_tokens` (1–2000, default
600). One page shares a 16000-token body budget; an item past it is returned
with `body_omitted: true` instead of silently shrinking the page.

`GET /v1/proposals/{project}/{id}?max_tokens=` returns one candidate with its
bounded redacted body (1–32000, default 600).

`POST /v1/proposals/{project}/{id}/promote` and `.../dismiss` mirror
`brain promote`: nothing is ever promoted automatically, `as`/`status`/`title`
override the created record, `force` allows a stale or orphaned source and
`yes` confirms a local-only source. Both are idempotent: a repeated promotion
returns the same record with `already: true`, a repeated dismissal returns
`already: true` and `dismissed: true`. A terminal proposal never changes state
again: dismissing a promoted proposal is a no-op receipt (`already: true`,
`dismissed: false`) and promoting a dismissed one returns `409`.

### Catchup

`GET /v1/catchup?project=&since=&include_unreviewed=&limit=` reports what
changed since the latest handoff (or `since`, or a seven-day default):
`since_source` (`handoff`, `explicit`, `default`), the state and whether it
changed, the new notes/decisions/attempts/handoffs as record rows, journal
lines (commit branch/head/subject only) and `quarantined_count`.

## Pagination and size caps

- List routes return `items` (`hits` for search) plus `page`, `page_size`,
  `has_more` and, when another page exists, `next_cursor`.
- A cursor is opaque and bound to the query that issued it: reusing it with a
  different project or filter returns `400`. Cursors are additive; a client
  that loses one restarts from the first page.
- Caps: records `limit` 1–200 (window 1000), search 1–100 (window 200, the
  index hit cap), proposals 1–200 (window 200), catchup 1–100, quarantine
  1–200 (window 1000), record `max_tokens` 1–32000, proposal list `max_tokens`
  1–2000 (page budget 16000 tokens), proposal detail `max_tokens` 1–32000. A
  cursor beyond its window returns `400` instead of a silently empty page.
- An all-projects page stays one page: the handler asks for the same bounded
  scan across the allowed projects, merges it newest first (search by score)
  and slices once.
- No endpoint returns raw-zone bodies, secret fields or internal error text.

## Memory route dependencies

The memory routes answer `500 unavailable` when the engine was started without
the use-case bundle (for example a connection-only control process). They are
served by `brain agent` in local and git mode.

## Jobs, run IDs and the run list

- `POST /v1/connections/{id}/run` returns `202` with
  `{"jobId":"<opaque-id>"}`. The job ID addresses the queued/running job in the
  current agent process.
- `GET /v1/runs/{id}` and `GET /v1/runs/{id}/log` accept a current-process job
  ID or a persisted run ID. Persisted runs remain addressable after a restart;
  job IDs do not outlive the process. Unknown or expired IDs return `404`.
- IDs are opaque path segments. Do not parse them, derive ordering from them or
  reuse them across engines; escape them as a single URL path segment.
- `GET /v1/runs` returns summaries newest first. `limit` defaults to `50` and
  must be between `1` and `500`; anything else returns `400`. The server never
  returns more than 500 runs, and v1 has no cursor or pagination token.
- A run row keeps the frozen `connectionId` and adds the snake_case
  `connection_id` with the same value, plus `project_ids` from the connection
  record when it is known. `actor` is reserved: run records do not persist who
  triggered a run, so the field is omitted rather than guessed.
- Run views and logs exclude error text and credential-bearing fields; logs are
  redacted by both the ingestion host and the handler.

## Canceling jobs

`POST /v1/runs/{id}/cancel` requests cancellation of a queued or running job.
A `202` with `{"status":"canceling"}` means the request was accepted, not that
the job has already stopped: a running job stops at its next cancellation
checkpoint (for example between source reads), so clients should keep watching
`GET /v1/runs/{id}` until the run is no longer `running`.

- Unknown, expired and persisted-only run IDs return `404`; the endpoint only
  cancels jobs in the current agent process.
- A job that already finished returns `409` with the `conflict` code.
- Canceling a queued job skips it before it starts.
- A canceled run is reported as `partial` with `stats.truncated: "canceled"`.
  The same representation is used for a canceled queued job that never produced
  a connector run record. Clients must not infer cancellation from item counts.

## SSE log snapshot

`GET /v1/runs/{id}/log` returns `200` with `Content-Type: text/event-stream`,
`Cache-Control: no-cache, no-transform` and `X-Accel-Buffering: no`.

- The endpoint streams a bounded snapshot of the lines currently available for
  that ID. It is not a follow/tail stream: it closes when the snapshot is
  exhausted, and reconnecting returns a fresh snapshot. There is no
  `Last-Event-ID` replay and no terminal `event: done`.
- Each line is emitted as one unnamed event, `data: <line>`, with line breaks
  flattened. A line is capped at 4096 bytes.
- While waiting for lines the server sends the comment `: heartbeat`
  approximately every 15 seconds.
- At most 1000 data events are emitted. When the cap is reached the stream ends
  with `event: truncated` followed by `data: log line limit reached`. Clients
  must treat `truncated` as a possibly incomplete log, not as completion, and
  must ignore unknown event names.

## Desktop 1.2 gap closure

The 1.2 desktop gaps are served additively in this engine build: all-projects
records/search/review, bounded proposal bodies plus a proposal detail route, the
safe `review/fix` equivalent, the quarantine inbox and its review call,
reversible record trash/restore, generic record text editing, run
`connection_id`/`project_ids`, and relation `target_kind`. Run `actor` stays
omitted because run records do not persist a trigger; the field is reserved
rather than guessed.

The desktop repository is not changed by the engine. Its pinned contract copy is
refreshed from this file as described below; the desktop release that consumes
these additive fields must regenerate its types and keep refusing a contract
major it does not know.

## Compatibility rule

`localapi-v1` is additive-only:

- Allowed in 1.x: new endpoints, new optional response fields, new optional
  request fields, new error codes and new SSE event names.
- Clients must ignore unknown JSON fields, skip unknown SSE events and fall back
  to the HTTP status for unknown error codes.
- Not allowed in 1.x: removing or renaming endpoints, fields, codes or headers;
  changing status codes, auth/Host/Origin rules, ID semantics, the 500-run cap
  or SSE framing; or changing the meaning of an existing value.
- Any such breaking change requires a new contract: document `info.version`
  `2.0.0`, `"x-contract": "localapi-v2"`, a new golden operation file and
  documentation (the OpenAPI specification version stays `3.0.3`).
  The desktop application pins the contract major it ships with (it must refuse
  a mismatched major rather than guess), so a breaking change ships only with a
  coordinated desktop release.

The operation set is tested against the golden file on every run:
`go test ./internal/adapters/localapi/...`. To deliberately accept a change,
regenerate the golden file, review the diff, and update the contract version if
the change is not additive:

```sh
UPDATE_LOCALAPI_CONTRACT=1 go test ./internal/adapters/localapi -run TestLocalAPIV1OperationSetIsFrozen
```

PowerShell: `$env:UPDATE_LOCALAPI_CONTRACT=1; go test ./internal/adapters/localapi -run TestLocalAPIV1OperationSetIsFrozen`.

## Desktop client copy

`lv24-brain-desktop` keeps a byte-identical copy of `openapi.json` at
`src/shared/api/openapi/localapi-v1.json` and generates its types from it. Do not
edit the desktop copy by hand; after an engine contract change run, from the
engine repository:

```sh
cp internal/adapters/localapi/openapi.json ../lv24-brain-desktop/src/shared/api/openapi/localapi-v1.json
cd ../lv24-brain-desktop && npm run api:generate && npm run api:check
```

PowerShell: `Copy-Item internal/adapters/localapi/openapi.json ../lv24-brain-desktop/src/shared/api/openapi/localapi-v1.json`
followed by `npm run api:generate` in the desktop checkout. Commit the contract
copy and the regenerated `src/shared/api/generated/schema.d.ts` together.
