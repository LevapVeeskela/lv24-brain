# Security and privacy

The engine is public; brain data is private. Keep that boundary strict.

## Data boundary

Public `lv24-brain` may contain code, docs, templates, tests, and installers
only. It must not contain:

- notes, decisions, attempts, handoffs, journals, or project state;
- mail, connector dumps, contact details, or other personal data;
- tokens, passwords, private keys, `.env` values, keychain exports;
- machine-specific paths or hostnames that identify a person;
- database files, indexes, or backups of private data.

Private `lv24-brain-data` stores approved brain data and owns `brain.yaml`.
Never commit `secrets/`, `index/`, `backups/`, `*.db`, `*.sqlite*`, `.env`,
OS keychain exports, access tokens, or passwords. The repository `.gitignore`
covers these paths; a pre-commit secret scan is added with the data repository.

## Secrets

- Connector and server credentials live in the OS keychain or environment
  variables, resolved at runtime.
- `brain.yaml` stores names and non-secret settings only.
- Secrets are never written to Markdown, git history, indexes, logs, error
  messages, or telemetry.
- Server secrets are provided by the deployment environment; Docker Compose
  files reference variables rather than embedding values.
- Rotate any key that is exposed in a transcript, log, or commit.

## Connector privacy

- GitHub, GitLab, and IMAP connectors are read-only.
- IMAP uses `BODY.PEEK`, fetches metadata and snippets only, and never sends,
  deletes, moves, or flags messages.
- Mail and connector metadata are explicit private data and still personal data;
  store only what the user approved.
- Logs redact credentials and message bodies. Full mail bodies are out of scope
  for v1.

## Server hardening (planned server mode)

- Local bootstrap accounts: Argon2id password hashes.
- OIDC authorization code with PKCE, `state`, and `nonce` validation.
- Short-lived JWT access tokens with refresh rotation and revocation.
- Scoped API tokens for CLI and connectors.
- Roles: `viewer` reads/searches/exports; `member` writes own/shared knowledge
  and runs own sync; `admin` manages users, roles, connectors, settings, and
  backups. Authorization is checked server-side on every request.
- Rate limits on auth and write paths; audit log for admin, role, and connector
  changes.
- Browser sessions, if added later, need CSRF-safe cookies and strict headers.
- `/health`, `/ready`, and `/version` expose no secrets or user data.

## Logging and errors

- Log structured events with context; never log tokens, passwords, mail bodies,
  or full connector payloads.
- Client-facing errors are typed and short; details stay in server logs.

## Supply chain and releases

- Release artifacts are checksummed (SHA-256) and optionally signed with
  Sigstore; the updater verifies before replacing the binary.
- Dependency versions are pinned; new dependencies need justification.
- CI runs formatting, vet, tests, and a secret scan. No paid runners, storage,
  or marketplace add-ons; only the free provider allowance is used.

## Reporting

Report a vulnerability privately to the maintainer instead of opening a public
issue with exploit details. Do not include secrets or personal data in reports.
