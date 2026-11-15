# Release 0.15.0 — correctness, multi-writer, one protocol

Status: implementation complete; not tagged. Audit: `ideas/_audit/` (local only).

0.15 adds no new knowledge features. It removes data-loss and multi-writer
hazards, makes the raw zone safe to share, gives records provenance, and brings
the MCP server to the 2026-07-28 revision while keeping 2025 clients working.

## Storage and multi-writer

- **Globally unique record ids.** Decisions, attempts, handoffs and notes now
  get `<kind>-<date>-<slug>-<ULID>`; the id is allocated once in the app layer
  and the file name carries it. Two writers in the same second no longer
  produce duplicate frontmatter ids. Old files keep their names and stay
  readable.
- **Exclusive publish.** `record restore` publishes through the same hard-link
  claim as a normal write and returns a typed `target exists` conflict instead
  of overwriting a concurrently published record.
- **Owner-safe locks.** Lock files carry pid, boot id, host, creation time, TTL
  and a release token. A live owner is never broken; a foreign lock is not
  deleted; stale locks are reported by `brain doctor`.
- **Promote is serialized per proposal.** Concurrent promotions create exactly
  one curated record; dismiss-vs-promote leaves one terminal state.

## Git sync and merges

- **Merge drivers** for `state.md`, `registry.json` and dated notes:
  `lv24-state`, `lv24-registry`, `lv24-notes`. Different state fields merge
  cleanly; the same field produces a conflict record under `state-conflicts/`
  and both values are kept. Registry is a union; notes are a union deduped by
  heading and content hash.
- `brain init` and `brain doctor --fix` register the drivers in the local git
  config; `.gitattributes` is committed with the data.
- **Bounded recoverable sync:** interprocess lock, autostash/restore, rebase
  abort on conflict, push retry with fetch+rebase, never force. A conflict
  exits **3** (new) and names the unmerged paths. `sync --status --json`
  reports ahead/behind, dirty state, conflicts and the last synced revision.
- Interrupted rebases and autostashes from a killed run are repaired on the
  next sync.

## Raw zone (connectors, mail, dumps)

- New connections default to **`local-only`** raw items and never publish
  checkpoints. Items use content-addressed paths without host timestamps.
- **Legacy tracked zones are grandfathered:** a connection with tracked
  `connectors/<id>` files and no explicit `publish` keeps working as shared,
  with a run notice and a doctor finding. Nothing is untracked automatically.
- Choose explicitly with `brain connection publish <id> shared|local-only`.
  Switching to local-only untracks the files but keeps them on disk and keeps
  git history (history is not erased).
- `brain doctor` reports three states: explicit local-only with tracked files
  (warn to untrack), grandfathered shared (warn to choose), new local-only (ok).

## Provenance and quarantine

- Records carry `origin` (`human|agent|connector|import`), `reviewed` and
  `trust` (`verified|stated|inferred`). Old files read as
  human/reviewed/verified.
- `brain context`, `brain open`, `brain bootstrap`, `brain session start`,
  MCP `brain_open`/`brain_context` and `brain catchup` exclude quarantined
  records by default and print `quarantined: N (ids)`. Opt in with
  `--include-unreviewed` / `include_unreviewed`.
- Session-end handoffs are `origin=agent, trust=stated`; with
  `security.quarantine_agent: true` they are also unreviewed.
- Clear quarantine with `brain record review <id>|--all [--trust]` or MCP
  `brain_review_record`. Promoted proposals are connector/stated but reviewed.
- `brain doctor` reports unreviewed counts.

## Security

- One shared redactor applies to every server write (note, decision, attempt,
  handoff, state) before persistence; counts are audited, text is never logged.
- `local-only` is enforced server-side: items from a local-only connection are
  not stored in the shared table and need an explicit admin opt-in.
- `context-allow.json` grants gate every raw path: CLI `--include-dumps`,
  MCP item search, server search and context excerpts. Legacy dumps are
  default-deny.
- MCP least privilege:
  - default writes target only the project resolved from the current directory;
  - `--allow-write` allows writes to any allowed project;
  - `--projects=a,b` restricts reads and writes to an allowlist;
  - `--read-only` omits all write tools and refuses calls;
  - stdio MCP child servers get a minimal environment; `BRAIN_*`,
    `DATABASE_URL`, provider tokens and `*_SECRET*` are never inherited.

## MCP protocol

- The server is dual-era: `2026-07-28` clients get `server/discover`,
  per-request `_meta`, `resultType`, cache hints and multi-round-trip
  confirmation; `2025-06-18` (and older) clients keep the `initialize` flow and
  the old result shape.
- All tools have titles, annotations, `outputSchema`, typed refusals
  (`code`, `try_instead`, `current_revision`, `retryable`) and cursor
  pagination where lists exist.
- `brain mcp --list-tools --json` reports tools, schemas and the effective
  read/write policy without serving.

## Retired checkouts

- `retired_paths` in `brain.yaml` redirects selection (`brotato` ->
  `lv24-harvest`), is skipped by `discover`, refused by `project add`, and
  reported by `doctor`. The engine ships no built-in paths.

## Migration

1. Upgrade the engine on every machine (schema stays 1; no hard cutover).
2. Run `brain doctor --fix` once per data repo to register merge drivers.
3. Review `brain doctor`:
   - `journal` -> `brain journal --repair`;
   - `raw-zone` -> decide per connection with
     `brain connection publish <id> shared|local-only`;
   - `merge-drivers` -> fixed by `--fix`.
4. MCP configs: add `--allow-write` only if agents must write to projects other
   than the current directory. Use `--read-only` for untrusted clients.
5. `brain sync` once to commit `.gitattributes`/`.gitignore` and the repaired
   journal.

## Visible changes

- Record file names grow by about 46 characters (`...-<ULID>.md`); scripts that
  parse `decision-<project>-...` must accept the suffix.
- A sync conflict now exits 3 and aborts the rebase instead of leaving it in
  progress.
- Server-mode IMAP needs `--publish shared` to store items or
  `--allow-local-only` to run without items.
- Bare e-mail addresses are not treated as credentials (credentials in URLs or
  assignments are). E-mail pseudonymization remains ingest-only.
- Default MCP writes are scoped to the current project; this is intentional.

## Known gaps (next phases)

- `state.md` has no provenance yet; `brain_state_set` text is trusted.
- `record review` is local/git only; server mode needs a provenance-edit route.
- No code graph yet (0.18); no embeddings, no desktop memory UI (0.16/1.2).
- Legacy pinned ids keep the old `-N` suffix rule for compatibility.
