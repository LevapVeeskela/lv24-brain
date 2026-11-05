# Security and privacy

The engine is public; brain data is private. Keep that boundary strict.

## Data boundary

Public `lv24-brain` may contain code, docs, templates, tests, installers, and
deployment files only. It must not contain:

- notes, decisions, attempts, handoffs, journals, or project state;
- mail, connector dumps, contact details, or other personal data;
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
  `brain secret list` returns names without values; `brain secret remove`
  deletes an OS-store entry.
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
- Server secrets come from the deployment environment. `deploy/.env.example`
  references variables and never contains values; `deploy/.env` is ignored.
- Rotate any key that appears in a transcript, log, commit, or screenshot.

## Redaction and error handling

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
  error. `brain connector run` prints that record, writes Markdown dumps under
  the data directory, and persists the run under `connectors/runs/` in local or
  `git` mode. Server mode rejects connectors, and no server-side run store is
  wired yet. Connector events are telemetry about the run, not message content.

## Server hardening

Implemented in `internal/adapters/httpapi` and `internal/auth`:

- Argon2id password hashes with per-password salts and constant-time
  verification; failed logins use a dummy hash to equalize timing.
- HS256 JWT access tokens with issuer, audience, expiry, not-before, and
  allowed-method validation; 15-minute default TTL.
- Rotating refresh tokens stored as SHA-256 hashes; reuse revokes the token
  family. In a Postgres deployment they are persisted by migrations
  `0002_auth_tokens.sql` and `0003_auth_revocations.sql` (server schema 3), so
  restarts do not invalidate sessions. Revocation tombstones have no expiry, so
  pruning expired token rows cannot resurrect a revoked family or session.
- Opaque API tokens with the `brain_` prefix, stored as SHA-256 hashes in the
  `api_tokens` table added by the same migration, with role and owner checks
  when minted.
- OIDC authorization code with PKCE, `state`, and `nonce`; issuer metadata is
  discovered over HTTPS and ID tokens are verified.
- Server-side authorization on every route. `viewer` reads, `member` writes,
  `admin` manages users and destructive operations.
- Login rate limiting, bounded request bodies, request timeouts, recovery from
  panics, and security headers (`nosniff`, `DENY`, `no-referrer`, no-store,
  restrictive permissions policy).
- SSE accepts a bearer token in the header; the query-token path is limited to
  the event stream for clients that cannot set headers.

Current limits, stated plainly:

- Without `BRAIN_DATABASE_URL`, `brain-server` uses the in-memory store and
  loses data and tokens on restart. With a database URL it uses Postgres and
  applies embedded migrations on startup; startup logs redact the database URL.
- An existing Postgres schema with pending migrations refuses to start until a
  backup is taken and `BRAIN_DATABASE_BACKUP_CONFIRMED=1` is set. Migrations are
  forward-only; upgrading schema 1 to schema 3 is gated this way.
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
- Every artifact is verified by SHA-256 before apply; missing manifest or
  checksum fails closed.
- Manifest, response, and download sizes are capped; downloads are capped at
  256 MiB.
- Apply stages the binary and keeps `brain(.exe).previous`; a failed replacement
  leaves the current binary untouched.
- The updater never mutates the brain data repository.
- Signature metadata is reserved for a future cosign/Sigstore step. v1 verifies
  checksums only; do not describe an artifact as signature-verified yet.

## Supply chain and releases

- Dependencies are pinned in `go.mod`; new dependencies need justification.
- CI runs `gofmt`, `go vet`, `go test ./...`, both builds, a JavaScript syntax
  check, and Compose config validation.
- Release artifacts are built on GitHub-hosted free-tier runners and published
  as GitHub Releases with `checksums.txt` and `release.json`.
- Run a secret scan before committing to the private data repository. The
  importer does not redact credentials because it cannot guess which text is
  knowledge and which is a secret; review every match.

## Reporting

Report a vulnerability privately to the maintainer instead of opening a public
issue with exploit details. Do not include secrets or personal data in reports.
