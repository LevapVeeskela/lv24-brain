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
| 400 | `validation`, `invalid_json`, `confirmation_required` | Invalid input or missing explicit confirmation. |
| 401 | `unauthorized` | Bearer token missing, duplicated or wrong. |
| 403 | `forbidden` | Host, `Origin` or `Referer` rejected. |
| 404 | `not_found` | Unknown route, connection, schedule or run. |
| 405 | `method_not_allowed` | Wrong method; `Allow` lists the supported methods. |
| 409 | `conflict` | Resource busy or conflicting with existing data. |
| 413 | `body_too_large` | Body exceeds the configured limit. |
| 415 | `unsupported_media_type` | Body is not `application/json`. |
| 500 | `internal_error`, `unavailable` | Sanitized failure; never contains internal text. |

`code` is stable and safe to branch on. `message` is diagnostic only; do not
parse it. Clients must tolerate future codes by falling back to the HTTP status.
Method mismatches are always `405` with an `Allow` header (for example
`Allow: GET, PUT`), never `400`.

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
- Run views and logs exclude error text and credential-bearing fields; logs are
  redacted by both the ingestion host and the handler.

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
