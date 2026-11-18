# Security and privacy

The engine is public; brain data is private. Keep that boundary strict.

## Data boundary

Public `lv24-brain` may contain code, docs, templates, tests, installers, and
deployment files only. It must not contain:

- notes, decisions, attempts, handoffs, journals, or project state;
- Mail, connector dumps, raw-zone items, contact details, or other personal data;
- tokens, passwords, private keys, `.env` values, keychain exports;
- machine-specific paths or hostnames that identify a person;
- database files, indexes, or backups of private data.

Private `lv24-brain-data` stores approved brain data and owns `brain.yaml`.
Never commit `secrets/`, `index/`, `backups/`, `*.db`, `*.sqlite*`, `.env`, OS
keychain exports, access tokens, or passwords. The public repository
`.gitignore` excludes the same local paths, and `brain init --mode git` writes a
matching `.gitignore` into the data directory.

## Secrets

- Connector and server credentials resolve at runtime from the environment or
  the OS secret store. `internal/adapters/secrets` reads
  `BRAIN_SECRET_<NAME>` first, then the platform store.
- Platform backends: Windows Credential Manager, macOS Keychain, and Linux
  Secret Service. A host without one reports the store as unavailable rather
  than falling back to a plain file.
- An environment-provided value shadows the OS store. `Set` and `Delete` fail
  closed for such a key instead of silently changing the wrong source.
- `brain secret set <name>` reads the value from stdin and prints only the name;
  `brain secret list` returns names without values; `brain secret remove <name> --yes`
  deletes an OS-store entry (without `--yes` it refuses, since the deletion
  cannot be undone).
- Server-mode CLI sessions: `brain login` stores one JSON credential bundle
  under the OS secret-store key `server-session` (`serverUrl`, `accessToken`,
  `refreshToken`) in a single atomic write, bound to the normalized
  `server_url`. The HTTP client refreshes once on `401` and retries; a bundle
  stored for another server URL is never sent to the configured server.
  `brain logout` checks `/v1/auth/me` before rotating anything, revokes the JWT
  session and its refresh family, and deletes the bundle. The refresh family is
  revoked immediately; an access JWT already issued stays valid until its short
  TTL.
  `BRAIN_TOKEN` (or an environment-provided `BRAIN_SECRET_SERVER_TOKEN`) takes
  precedence over stored credentials: login is refused and logout leaves a
  stored session untouched and exits non-zero. `server-token` remains an
  explicit static API-token fallback with no refresh token and no binding;
  `server-refresh-token` and `server-auth-url` are no longer read, so a legacy
  unbound JWT pair requires a new `brain login`. A static API token cannot be
  revoked server-side: logout clears the local entries, warns, and exits
  non-zero.
- The token values are never written to `brain.yaml` or printed.
- `brain.yaml` stores names and non-secret settings only. Credentials never
  appear in Markdown, git history, indexes, logs, error text, or telemetry.
  Connection `baseUrl` values reject userinfo, fragments and query strings so
  the committed `connectors/connections.json` catalogue cannot carry tokens.
- Server secrets come from the deployment environment. `deploy/.env.example`
  references variables and never contains values; `deploy/.env` is ignored.
- Rotate any key that appears in a transcript, log, commit, or screenshot.

## Redaction and error handling

- Every knowledge write through the server API (notes, decisions, attempts,
  handoffs and project state, including updates) runs the same shared redactor
  as the CLI and ingestion (`internal/redact`) before persistence. A write that
  carried a credential is logged (`knowledge write redacted secrets`) and
  audited as `knowledge.redacted` with the entity and count only; the text is
  never echoed. The shared pattern set cannot drift between paths.
- Server request logs contain request id, method, path, status, and duration.
  Authorization headers and request bodies are not logged.
- Client-facing server errors are short typed envelopes; internal details stay
  in server logs.
- `httpclient` (server-mode store client) removes the configured token, the
  token sent with the failing request, and `Bearer <token>` patterns from
  returned error text, bounds response sizes, and reports 401/403 as typed
  errors. On a 401 it refreshes once with the stored refresh token and retries;
  a credential 401 keeps the original 401, while a transient refresh failure is
  surfaced instead of being hidden. Tokens and request bodies are never logged.
- `gitstore` runs git without a shell, disables interactive prompts, bounds each
  command with a timeout, and sanitizes command errors so remote URLs with
  embedded credentials or tokens do not leak.
- Connector run records redact caller-supplied secrets before truncating the
  error text, strip control characters, and collapse whitespace.
- The updater never prints a token; release URLs must be HTTPS except for
  loopback test servers.

## Connector privacy

- GitHub, GitLab, and IMAP connectors are read-only.
- IMAP selects mailboxes read-only, uses `BODY.PEEK`, and keeps envelope
  metadata plus a bounded plain-text snippet. The adapter has no send, delete,
  move, append, copy, or flag operation.
- Mail and connector metadata are explicit private data and still personal
  data. Store only what the user approved; full mail bodies are out of scope
  for v1.
- Connector runs are idempotent and produce status, counts, and a sanitized
  error. Local or `git` mode writes Markdown dumps under the data directory and
  persists the run under `connectors/runs/`. Connection runs (v0.9.0)
  store raw items at `connectors/<id>/items/*.md`, checkpoints at
  `connectors/<id>/checkpoints/*.json`, and tombstones for reported deletes;
  ingest redacts secrets to `[redacted]` and hashes mail addresses to
  `[email:<12 hex>]` (exposure reduction, not anonymization). The raw zone is
  machine-local unless the connection is shared (since 0.15): an explicit
  `publish: shared` commits items, a zone that git already tracks keeps
  publishing unchanged until the owner chooses (grandfathered on upgrade), and a
  new zone publishes nothing. An item file in shared mode omits the host
  `observed_at`; checkpoints are never committed and carry `-merge -text`. Sync preflight pins
   `* text=auto eol=lf` and binary rules in `.gitattributes` (so a Windows clone
   cannot create CRLF conflicts) and re-registers the deterministic drivers per
   clone with the current executable. If two hosts still diverge on a shared
   item, the item driver keeps both sides under distinct content-hash paths.
   Driver conflict records are committed in the same sync and surfaced in
   `sync --status --json`, never silently dropped. `brain doctor` reports tracked
  raw files instead of deleting them: `raw-zone` for an explicitly local-only
  connection (untrack with `git rm --cached -r`), `raw-zone-policy` for a tracked
  zone with no explicit policy (choose with `brain connection publish <id>
  shared|local-only`). Untracking never erases prior git history, and mail
  defaults to `local-only`.
- Legacy dump roots predate the publish policy: `mail/`, root `github/` and
  `gitlab/`, `merges/`, and the orphan `connectors/gitlab/`. Since 0.22 they are
  local-only and anchored in `.gitignore`; `brain doctor` reports any tracked
  file there as `pii-zone` with code `tracked-pii-zone` and the exact
  `git rm --cached` command. The untrack commit keeps the files on the machine
  that has them and keeps every blob in git history. If a clean machine loses a
  worktree copy while pulling that commit, restore it from history, for example
  `git checkout <commit>^ -- mail/gmail/2026-11-05.md`. No history is rewritten.
  Server mode runs the same
  native item-level path through `POST /v1/connectors/run` (admin only): with
  PostgreSQL it persists raw items, checkpoints and the run record (schema 10);
  the in-memory development store has no raw zone and rejects connector runs
  as unsupported instead of discarding output. The server has no local-only
  raw zone, so it enforces the policy at admission: a connection whose publish
  policy is local-only (any connection without an explicit `publish: shared`)
  is refused with 403 and `connector.local_only.denied` in the audit trail
  unless the request carries the explicit admin opt-in `allowLocalOnly`; with
  the opt-in the run stores its run record and checkpoints and drops every raw
  item (counted as `skipped`). The checkpoint still advances, so a later
  `publish: shared` run with the same targets reads only newer items; use
  `--since` to backfill deliberately. `brain connector run --publish shared`
  is the explicit way to store items for project members, and
  `--allow-local-only` is the explicit local-only acknowledgement. The per-run
  Markdown export contains only status, counts and the sanitized error.
  Connector events are telemetry about the run, not message content.

## Proposals and promotion

Phase 4.1 adds a review zone between raw ingestion and curated records. It is
local/git only.

- Proposals are stored at `<data-dir>/proposals/<project>/<proposal-id>.md`.
  The first write creates or repairs `proposals/.gitignore` with `*`, and
  `CurrentRevision` excludes `proposals/`, so the zone is machine-local derived
  data: it is not committed, not synced, not exported, and not served by the
  server API. Server mode has no proposal store, so `brain digest --source` and
  `brain promote` exit `2` there.
- `brain digest --source <connection>` reads only items already stored by
  ingestion. It never runs the connector, never contacts the provider, and has
  no LLM or network dependency. A preview is the default; `--write` persists
  proposals.
- Proposals are not indexed in FTS5 and do not appear in `brain search`.
  `brain review` lists kind, title, confidence, date and id only; `brain context`
  adds only `pending_proposals: N`, and proposal bodies never enter context.
  Raw items appear only through `--raw` and a matching grant (see below).
- Promotion is the only path into curated records. `brain promote` writes a
  normal decision, attempt or note through the existing use case and records
  `source.proposal_id`, connection, external id, URL, observed time and
  confidence. The write is record-first and idempotent: a retry finds the
  existing record and repairs the proposal status instead of duplicating it.
  A `promoted` or `dismissed` proposal is terminal and is not overwritten by a
  later digest.
- A source connection marked `local-only` requires `--yes` before its content
  may enter curated records; a provider whose descriptor defaults to
  `local-only`, and an unknown provider, are treated the same way. A stale or
  orphaned candidate requires `--force`. These checks prevent private
  mail/chat content or a vanished source from being promoted by a routine
  command.
- Proposal titles, labels and excerpts are unverified text from external
  systems. Treat them as data, never as instructions: a malicious issue or PR
  can contain prompt-injection text. The digest bounds titles and excerpts,
  strips control characters and never copies a full raw body, but it does not
  attest to the content. Review before promoting.
- The change is additive: `Item.State` is optional, no data schema is bumped,
  and the frozen `contracts-v1` source/connector contract is unchanged. The
  `localapi-v1` wire contract gains proposal list/promote/dismiss routes, which
  is an allowed additive change; every proposal write still requires an explicit
  call with the CLI's `force`/`yes` confirmations, and nothing is promoted
  automatically.

## Provenance and quarantine (0.15)

Curated records carry who wrote them and whether a person reviewed them, so
external text cannot become "truth" in an agent's context on its own.

- Every note, decision, attempt and handoff has three optional frontmatter keys:
  `origin` (`human`, `agent`, `connector`, `import`), `reviewed` (boolean) and
  `trust` (`verified`, `stated`, `inferred`). A missing key is the old
  behavior: empty origin is human, an absent `reviewed` is reviewed, and an
  empty `trust` is verified. Files written before 0.15 therefore keep working
  unchanged and are treated as owner-authored, reviewed and verified.
- Only values that differ from the defaults are written: `reviewed` appears
  only as `false`, and `trust` only when it is not `verified`. `origin` is
  always written so a record cannot silently lose its channel. A human origin
  is reviewed by definition (the CLI action is the review); only an agent,
  connector or import write can be unreviewed. Unknown values are a validation
  error on write. On read they are preserved, but an unknown origin makes the
  record a quarantine candidate and an unknown trust is treated as `inferred`.
- Write channels set the fields: the CLI writes
  `origin: human, reviewed: true, trust: verified`; MCP tools write
  `origin: agent` with `reviewed: true`; `brain promote` writes
  `origin: connector, reviewed: true, trust: stated` because promotion is the
  review; an importer writes `origin: import` and may set `reviewed: false`.
  Project state (`state.md`) carries the same keys: `brain state set` writes
  `origin: human, reviewed: true, trust: verified` and MCP `brain_state set`
  writes `origin: agent, trust: stated` (reviewed unless
  `security.quarantine_agent` is set). Partial field updates keep the stored
  provenance unless it is explicitly changed.
- The automatic session-end capture (`brain session end`, including the
  Claude/Codex/OpenCode hooks) is an agent write too: the handoff and any
  decision captured from the final message are `origin: agent, trust: stated`.
  They are reviewed by default so the next `brain open` still shows the latest
  handoff; `security.quarantine_agent: true` makes them unreviewed instead.
- Set `security.quarantine_agent: true` in `brain.yaml` to make every MCP write
  and session-end capture unreviewed. The default keeps agent writes reviewed,
  so existing agents do not lose context; enable it when the agent may read
  hostile repository or connector content.
- Quarantined records are excluded from `brain context`, `brain open`
  (bootstrap), `brain session start`, `brain catchup` and MCP `brain_context` /
  `brain_open`. The output adds only `quarantined: N (id, …)`; bodies never
  appear. Pass `--include-unreviewed` (CLI) or `include_unreviewed: true` (MCP
  context/open) to inspect them; the included records are labelled
  `quarantined`. Server mode applies the same default: `GET /v1/search` drops
  quarantined records before ranking, and the project listing routes (`notes`,
  `decisions`, `attempts`, `handoffs`) drop them unless `include_unreviewed=true`.
  Reading one record by id is an explicit request and still returns it. The
  server search has no opt-in yet; quarantined ids stay reachable by id and
  through the review route.
- A quarantined state is reported as `state_quarantined: true` and its text is
  replaced with a minimal placeholder in trusted context; pass
  `--include-unreviewed` to view it. Clear it with `brain state review`
  (optionally `--trust`).
- `brain review` reports a `quarantined` finding with the ids, and
  `brain doctor` reports the queue size. Clear one record with
  `brain record review <id>` or the whole queue with
  `brain record review --all`, optionally with `--trust verified|stated|inferred`.
  MCP exposes the same action as the temporary `brain_review_record` tool
  (id/all/trust); 0.17 merges it into `brain_judge`. Review is idempotent and
  never edits a body.
- This is the poisoning boundary: a malicious issue, PR, mail message or
  injected agent message can be stored, searched and reviewed, but it cannot
  become auto-injected context until a person (or an explicitly trusted
  workflow) marks it reviewed.

Known gaps in 0.15:

- `brain state review` clears a quarantined state in local/git mode only:
  `project_state` has no provenance columns yet, so a server state write stores
  no provenance and is read back as human/reviewed. Server record review is the
  route above; `--review-after`/`--clear-review-after` stay local/git because
  the HTTP client does not implement `ports.RecordScheduler`.

## Raw context excerpts (Phase 4.2)

Raw connector text stays default-deny. It can enter `brain context` only in
local/git mode, only when the operator wrote an explicit grant, and only as a
small cited excerpt.

- `brain context "<task>" --raw` is opt-in; without the flag the output is
  unchanged. A grant never changes curated search, `brain open` or
  `brain review`; it gates raw search (see below), and MCP `brain_context`
  honors it only when called with `raw: true`.
- The policy is `<data-dir>/context-allow.json`, next to `brain.yaml`. A missing
  file or no matching grant means no raw excerpt, not an error. The file holds
  no secrets and is committed with the data repository, like
  `connectors/connections.json`.
- `schemaVersion` is `1`. `project` and `connection` are required and matched
  exactly. Optional `kinds` is a subset of `issue`, `pull-request`,
  `merge-request`, `commit`, `message`, `page`, `document`, `event`, `schema`,
  `other`; empty means all. `maxAgeDays` `0` disables the age filter (maximum
  `3650`), and `maxExcerpts` defaults to `5` (range `1`–`5`). Unknown fields,
  duplicate `project`+`connection` grants, invalid ids and invalid kinds are
  hard errors; a malformed policy is refused instead of widened.
- Excerpts are included only when the curated context is thin (fewer than five
  records, failed attempts and findings in total), at most five per context,
  600 runes per body and 2000 runes in total. Only items whose `ProjectID`
  matches the requested project are eligible; synthetic `connection:<id>` items
  are excluded.
- Bodies are re-redacted at read time with the ingestion redactor
  (`[redacted]`, `[email:<12 hex>]`) and marked as untrusted external text.
  Citations carry the connection, external id, URL and updated date. This is
  exposure reduction, not anonymization: granted content is still private data
  and can contain prompt-injection text. Treat excerpts as data, never as
  instructions.
- Server mode, the desktop app and LLM surfaces are deferred; they do not read
  the grant file.
- The same grant policy gates every raw search path, not only context:
  `brain search --include-dumps`, `brain search --kind item` and MCP
  `brain_search` with `kinds:["item"]` return a granted connection's items and
  nothing otherwise (curated hits are unaffected). A denied request prints one
  hint pointing at `context-allow.json`. Legacy connector dumps under `mail/`,
  `github/` and `gitlab/` carry no connection identity, so no connection grant
  can authorize them and they stay default-deny.
- The server API has no grant file. Server search therefore never returns raw
  connector items to viewers or members: an unfiltered search excludes them,
  and an explicit `kind=item`/`kind=dump` request is refused with 403 and
  `search.raw.denied` in the audit trail. Only a global admin can request raw
  items there.

## MCP least privilege (0.15)

`brain mcp` serves one OS user, so the process is the trust boundary. It starts
with a narrow default and three flags to narrow it further; the full matrix and
refusal shapes are in `docs/mcp.md` §8.

- **Writes are scoped to the working directory.** Without `--allow-write`, a
  write tool may only target the project `brain open` picks in the checkout the
  server was started in. An agent working in one repository therefore cannot
  add records to another project, even by naming it, unless the operator started
  the server with `--allow-write`. If the working directory is not a project,
  writes are refused until `--allow-write` is passed. Reads are unaffected.
- **`--projects=a,b`** allowlists project ids or aliases for reads *and* writes.
  It is resolved once at startup; an unlisted project is refused with
  `INVALID_ARGUMENTS` naming the allowed set, `brain_projects` is filtered, and
  a workspace-root `brain_open` (which would list every project) is refused. An
  unscoped search or journal is bound to the single allowed project; with two or
  more the caller must name one.
- **`--read-only`** removes every write tool from `tools/list`. A call to a
  hidden tool returns `UNSUPPORTED` with `try_instead: brain_search or
  brain_context` and never reaches the store. This is the profile for an agent
  that reads hostile repository, issue or mail content: prompt injection can
  still influence what the agent says, but not what it writes into the shared
  brain.
- `--read-only` wins over `--allow-write`. `brain mcp --list-tools --json`
  reports the effective policy and the hidden tools, so the profile a host
  approved can be diffed.
- The tool list is `cacheScope: private` under `--read-only`; a shared cache must
  not hand a writable profile's tool list to a read-only client.

### Child environment of stdio MCP sources

An MCP server used as an ingestion *source* (`brain connection` with
`transport: stdio`) is a third-party executable. It no longer inherits the
brain's environment:

- The child receives only `PATH`, `HOME`/`USERPROFILE`, `TMP`/`TEMP`/`TMPDIR`,
  `SystemRoot`, `ComSpec`, `PATHEXT`, `APPDATA`, `LOCALAPPDATA`, `LANG` and
  `LC_*`. These carry locations and locale, not credentials.
- `BRAIN_TOKEN`, `BRAIN_DATABASE_URL`, `BRAIN_SECRET_*`, `DATABASE_URL`,
  `GITHUB_TOKEN`, `GITLAB_TOKEN`, `AWS_*`, `OPENAI_API_KEY`,
  `ANTHROPIC_API_KEY` and every `*_SECRET*` name are never inherited, even if a
  future allowlist would admit them.
- Extra variables are opt-in per connection and stored in the local credential
  store next to `command` and `args`, so a synced `connections.json` cannot add
  an environment on another machine:

  ```sh
  brain connection secret set <id> env   # one line: {"API_KEY":"..."}
  ```

  The value is a JSON object of at most 32 string variables, 16 KiB in total,
  with portable names (`[A-Za-z_][A-Za-z0-9_]*`), no NUL and no newline in a
  value. Invalid input is refused instead of dropped, and an explicit value
  overrides an inherited one. Configure only what the server needs: an explicit
  entry is passed through even if it names a secret.
- The child still runs as the same OS user with the same working directory and
  is not sandboxed. Use a container, a dedicated user or an OS sandbox when the
  server itself is untrusted.

## Local control API

`brain agent` (local/git mode) serves an authenticated API on an ephemeral IPv4
loopback listener; it is never bound to a public interface and there is no
reverse-proxy or browser mode.

- The bearer token is a 256-bit per-process secret in
  `<data-dir>/agent/runtime.json`. The file is written with owner-only
  permissions (0600/ACL), the parent `agent/` directory is self-ignored, and the
  runtime file is removed on a clean shutdown. Tokens are compared in constant
  time and never returned by an endpoint, status output or log line.
- Every request must carry exactly one `Authorization: Bearer` header and the
  exact loopback `Host` for the selected port. Any `Origin` or `Referer` is
  rejected; there are no CORS headers, so a web page cannot call the API even
  after DNS rebinding. Request bodies are size-limited and decoded strictly.
- Connection endpoints validate the provider descriptor; secret fields are
  rejected in connection settings and can only be written through
  `POST /v1/connections/{id}/credentials/{field}`. Responses never include
  secret values, and run/log output is redacted by the ingestion host and the
  handler.
- The memory routes expose curated records, ranked search, project state, the
  review queue, proposals and catchup. They never return raw-zone bodies:
  search refuses `dump`/`item` kinds and proposal listings carry identity and
  source health, not source text. Every response string passes through the
  handler redactor. The only memory writes are the state compare-and-swap and
  the explicit proposal promote/dismiss calls.
- `POST /v1/engine/update` requires explicit `{"confirm":true}` and uses the
  same signed-manifest verification as `brain update`; `POST /v1/shutdown` only
  requests the graceful stop of the local agent. Settings persist non-secret UI
  values only.

The frozen [localapi-v1 contract](localapi-v1.md) records these rules, the
sanitized error envelope and 405 `Allow` header, job/run ID semantics, the
500-run list cap, the bounded SSE snapshot/truncation signal, the memory route
caps and the state CAS error.

## Server hardening

Implemented in `internal/adapters/httpapi` and `internal/auth`:

- Argon2id password hashes with per-password salts and constant-time
  verification; failed logins use a dummy hash to equalize timing.
- HS256 JWT access tokens with issuer, audience, expiry, not-before, and
  allowed-method validation; 15-minute default TTL. Every request also checks
  the session/family revocation tombstones and reloads the user, so logout,
  refresh-token reuse, deletion and role changes take effect immediately.
- Rotating refresh tokens stored as SHA-256 hashes; reuse revokes the token
  family. In a Postgres deployment they are persisted by migrations
  `0002_auth_tokens.sql` and `0003_auth_revocations.sql` (server schema 10), so
  restarts do not invalidate sessions. Revocation tombstones have no expiry, so
  pruning expired token rows cannot resurrect a revoked family or session.
- Opaque API tokens with the `brain_` prefix, stored as SHA-256 hashes in the
  `api_tokens` table added by the same migration, with role and owner checks
  when minted. Tokens may carry an optional expiry (`expiresInDays` or
  `expiresAt` at mint time, migration `0006_api_token_expiry.sql`); an expired
  token is rejected exactly like a revoked one. Tokens without an expiry live
  until revoked. Tokens may also carry scopes and a project allow-list
  (migration `0008_project_access.sql`, see the authorization model below).
- OIDC authorization code with PKCE, `state`, and `nonce`; issuer metadata is
  discovered over HTTPS (plain `http` is accepted only for loopback issuers)
  and ID tokens are verified. The `state` is also bound to the starting
  browser by an HttpOnly, SameSite=Lax cookie, so a callback URL carrying
  another user's code is rejected (login CSRF).
- OIDC account linking: a first OIDC login is linked to an existing local
  account only when the provider asserts `email_verified` and the verified
  email equals the account login. Admin accounts are never linked implicitly,
  and an unverified email is never used as the login. Every other collision
  is refused with `403`.
- Security audit log: successful and failed logins, login rate limiting,
  logouts, refresh-token reuse, API-token creation and revocation, and OIDC
  login/link/signup/refusal are recorded with the actor and client address,
  never with passwords or tokens. Refused raw access is recorded too:
  `knowledge.redacted` (a write carried a credential, count only),
  `connector.local_only.denied` (a local-only connector run without the admin
  opt-in) and `search.raw.denied` (a project reader requested raw items from
  server search). They carry no revision, so only admins can
  read them (`GET /v1/audit`, filterable by actor, action, entity, and time);
  viewers never see them through revisions or SSE. Entity changes keep their
  revision plus audit row, now with the authenticated actor. Audit writes are
  best-effort: a failed write is logged and never fails the request. The
  in-memory development store keeps only the last 10,000 security events.
- Server-side authorization on every route (model below). `viewer` reads,
  `member` writes, `admin` manages users and destructive operations. Members
  create notes, decisions, attempts and handoffs and may update or delete only
  their own (owner = creating user id, `createdBy` in responses). Records with
  an unknown owner, created before ownership tracking, are admin-only (and
  maintainer-only inside a restricted project). Connector execution is
  admin-only (`POST /v1/connectors/run`); viewers may list, read and export run
  records. The runner is bounded by `BRAIN_CONNECTOR_TIMEOUT` and uses the same
  environment/OS secret store as the CLI.
- Login rate limiting per client address and, separately, per account
  (`BRAIN_LOGIN_RATE_LIMIT` attempts per minute each). Behind a reverse proxy
  set `BRAIN_TRUSTED_PROXIES`; only then is `X-Forwarded-For` honoured, walked
  from the right so spoofed left-hand entries are ignored.
- Connector base URLs (GitHub, GitLab) must be `https`; plain `http` is
  accepted only for loopback hosts. `brain login` has no `--password` flag.
- Bounded request bodies, request timeouts, recovery from
  panics, and security headers (`nosniff`, `DENY`, `no-referrer`, no-store,
  restrictive permissions policy).
- SSE accepts a bearer token in the header. Clients that cannot set headers
  (EventSource) obtain a single-use ticket from `POST /v1/events/ticket`
  (valid one minute) and pass it as `?ticket=`; access and API tokens are
  never accepted in the URL.

### Authorization model

Every authenticated request passes these checks in order:

1. **Token scopes** (API tokens only). A token minted with `scopes` may do
   only what they allow: `read` (GET requests and the event-stream ticket),
   `write` (knowledge, state and project writes, token minting), `admin`
   (admin-only endpoints: users, project members, audit log, connector runs,
   project deletion; requires an admin-role token). `write` and `admin` imply
   `read`. A missing scope answers `403 insufficient_scope` before any role or
   ACL check. Tokens without scopes (every token minted before migration
   `0008`) keep their full role, so nothing existing is widened or broken.
2. **Global role**: the route's minimum role (`viewer`, `member`, `admin`).
   An API token's own `role` can only narrow its owner's role.
3. **Project access**, enforced in every project-scoped handler: projects,
   state, notes/decisions/attempts/handoffs (list, create, read, update,
   delete by id), search, the revision trail and the SSE event stream.
   - A token `projectIds` allow-list hides every other project (admins too).
   - A project **without a restriction** is open to every authenticated user
     per their global role: the behaviour before project ACLs, so existing
     deployments are unchanged after the upgrade.
   - Adding the first member **restricts** the project. Then only its members
     and global admins can access it. The per-project role (`viewer`,
     `member`, `maintainer`) is capped by the global role: a global viewer is
     read-only in every project, a global member can act as member or
     maintainer where the project grants it. `maintainer` adds editing project
     metadata and updating or deleting other members' records in that project.
     Admins bypass membership.
   - Removing the last member keeps the project restricted (admin-only), and
     deleting a user removes only that user's memberships. A project is
     reopened only explicitly (`PUT /v1/projects/{id}/access`), and only when
     it has no members, so a cleanup never silently opens a project.
   - An unreadable project answers `404`, never `403`, so its existence does
     not leak. Lists and search drop unreadable projects in the query (before
     the limit, no titles or snippets leak); revisions and SSE events of
     unreadable projects are not delivered. Knowledge revisions carry
     `project_id`; older delete revisions without it are shown only to callers
     who can read every project.

Membership changes (`project.member.added/updated/removed`,
`project.access.restricted/opened`) and token creation (with its scopes and
project allow-list) are written to the security audit log. A token minted by
an API token may only narrow it: its scopes, project allow-list and expiry
must be within the calling token's. Project ACLs never change global roles, so
the last-admin guard is unaffected.

Current limits, stated plainly:

- Without `BRAIN_DATABASE_URL`, `brain-server` uses the in-memory store and
  loses data and tokens on restart. With a database URL it uses Postgres and
  applies embedded migrations on startup; startup logs redact the database URL.
- An existing Postgres schema with pending migrations refuses to start until a
  backup is taken and `BRAIN_DATABASE_BACKUP_CONFIRMED=1` is set. Migrations are
  forward-only; every upgrade (currently to schema 10) is gated this way.
- API tokens and refresh-token families are durable in Postgres (migrations
  `0002_auth_tokens.sql` and `0003_auth_revocations.sql`), so restart no longer
  forces re-authentication.
- Multi-replica deployment is still unsupported: the SSE broker and login rate
  limiter are per-process, so events and throttling are not shared between
  instances. Durable tokens do not fix that.
- `brain login` supports local accounts. There is no CLI OIDC flow; OIDC uses
  the browser callback. A static API token can still be supplied through
  `BRAIN_TOKEN` or `brain secret set server-token`.
- There is no browser session/cookie flow, so CSRF-safe cookie settings do not
  apply yet. Do not add browser sessions without that work.
- Server auto-update is not implemented. `BRAIN_SERVER_AUTO_UPDATE` is a
  reserved placeholder; container upgrades replace the image.

## Update integrity

- Release checks and downloads use HTTPS only.
- The manifest version must match the git tag, and its channel must match the
  GitHub prerelease flag.
- `release.json` must carry a valid detached Ed25519 signature
  (`release.json.sig`) from a key embedded in the binary; unsigned or
  untrusted releases are skipped. Every artifact is verified by the SHA-256
  from the signed manifest before apply; missing manifest, signature, or
  checksum fails closed. The Unix installer verifies the same signature when
  OpenSSL 3 is available; the Windows installer checks SHA-256 only.
- Manifest, response, and download sizes are capped; downloads are capped at
  256 MiB.
- Apply stages the binary and keeps `brain(.exe).previous`; a failed replacement
  leaves the current binary untouched.
- The updater never mutates the brain data repository.
- The signing seed exists only in the `BRAIN_RELEASE_SIGNING_KEY` Actions
  secret (and an offline backup); it is exposed to the signing step alone.
  Installers verify `checksums.txt` only, so a first install is
  checksum-verified, while every subsequent self-update is signature-verified.

## Supply chain and releases

- Dependencies are pinned in `go.mod`; new dependencies need justification.
- CI jobs: `gofmt`, `go vet`, `go test`, builds, a Node syntax check, Compose
  config validation, `race` (push only), and Postgres integration tests (run
  against a service container).
- Release artifacts are built on GitHub-hosted free-tier runners and published
  as GitHub Releases with `checksums.txt`, `release.json`, and
  `release.json.sig`. Existing releases are never overwritten.
- Run a secret scan before committing to the private data repository. The
  importer does not redact credentials because it cannot guess which text is
  knowledge and which is a secret; review every match.

## Reporting

Report a vulnerability privately to the maintainer instead of opening a public
issue with exploit details. Do not include secrets or personal data in reports.

## Server record provenance review

Server mode exposes `POST /v1/projects/{project}/records/{kind}/{id}/review`
(and the project-less `POST /v1/records/{kind}/{id}/review`) to update a
record's `origin`, `reviewed` and `trust` without rewriting its body. The caller
needs write access to the record's project, and a project named in the path must
match the record or the answer is `404`. The body carries the `revision` the
caller read plus optional `trust` and `origin`; `reviewed: false` is refused, a
stale revision is `409`, and a record without a revision is `409` instead of
last-write-wins. The write bumps the server revision and records an audit event.
Quarantined records stay out of server search and the default project listings
until reviewed, and remain reachable by id.

