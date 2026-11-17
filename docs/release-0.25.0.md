# Release 0.25.0 — second-brain correctness

Status: release candidate on `main`.

0.25 turns the audit findings into guards. Nothing in the data schema changed;
the release is a drop-in upgrade for engine, desktop and the data repository.

## Connectors

- `brain connection list` shows the last run time, status and a redacted failure
  cause; a dead ingestion is visible without reading logs.
- `brain connection test <id> [--json]` runs the same check the agent runs.
- The local API `/v1/connections/{id}/test` returns stable codes
  (`credential_missing`, `connection_invalid`, `connection_unsupported`,
  `connection_check_failed`, `connection_unavailable`) instead of a generic 500.
- GitLab author e-mail addresses are treated as personal data and hashed in the
  raw zone even when the connection publishes shared items.

## Privacy

- New `brain doctor` check `tracked-pii` (warning): tracked `connectors/`
  files that hold e-mail addresses or mailbox user names are named. Moving the
  identity fields to the OS keychain is the 0.26 follow-up; until then the
  check warns and changes nothing.

## Safety

- `brain record edit` redacts credentials (bearer tokens, URL query tokens)
  before writing Markdown; previously only responses were scrubbed.
- `merges/*.json` with `status: applied` no longer count as pending conflicts
  in `doctor` and `review`; real pending artifacts stay visible.
- Merge apply and rollback take per-file locks and re-verify the plan
  fingerprint (`sha256/size/mtime`). Drift returns `stale_plan` and writes
  nothing.
- A failed `git stash pop` keeps the stash ref and names it; it is never
  dropped. `publish` and `sync` share one owner-aware `sync.lock`.
- Journal appends are cross-process locked and fsynced; reads dedupe by
  `(ts, event, id)`.
- MCP state updates use `SetStateIfRevision` (atomic check-and-write).

## Projects

- `SaveProject` rejects an alias equal to another project's id or alias, and a
  path registered by another project.
- `brain search --project` resolves id, alias and existing path; an unknown
  value is an error, not an empty result.
- `brain_journal` and `brain_prompt(action=search)` require a project or
  `all=true`.
- `discover --apply` no longer registers a second clone of the same repository
  in one scan; dump attribution for a shared repo URL is deterministic and
  warned.
- A nonexistent path under a project is an error instead of resolving to the
  enclosing project.

## Import and exchange

- engram import is plan-then-commit: a conflict aborts before writing anything
  (`--skip-conflicts` writes only clean files). Dry runs write nothing and hash
  only `projects/`.
- Each imported note keeps its own `updated_at`; `--repair-timestamps` migrates
  legacy daily files idempotently.
- Exchange v2 carries `path_provenance` and `pinned`; v1 bundles still import,
  and export → import → export is byte-identical for the fixture.
- Manifest SHA256 is verified before planning.

## Hooks

- `brain agents sync --hooks` creates a missing Claude Code `settings.json`
  instead of reporting the host as not installed.
- `brain hooks status --agents [--json]` reports installed, trusted, last run
  and last error per host.
- Hook invocations append one redacted JSON line to `index/hooks.log`.
- OpenCode receives the final assistant message on stdin, so `Decision:` lines
  are captured like Claude and Codex.
- Codex trust-hash drift is reported with re-approval instructions.

## Desktop 1.6.2

- Quarantine has a correct section title.
- Project rows open the project detail and preserve `?project=`.
- Project detail shows `fresh_until` and a stale badge.
- A render error shows a recoverable boundary instead of a blank window.
- Inbox A/D shortcuts ignore key repeat and focused controls.

## Performance

- Context linked candidates resolve from one batch listing: context on the
  large project drops from ~1.2–1.5 s to ~0.6–0.8 s with byte-identical output.
- Reindex batches one transaction with a prepared statement (12.3 s → 3.9 s on
  the current corpus) and truncates the WAL after `optimize`.

## Deferred to 0.26

- Ranking v2 (BM25 normalization, OR fallback, trust-aware ranking) and the
  budget/truncation rework.
- Temporal consistency for `--as-of` and `--since`.
- Desktop merge plan/apply/rollback, digest and adopt/onboard surfaces.
- Moving connector identity fields to the OS keychain (the `tracked-pii`
  warning is the interim signal).
- Server-mode review/provenance completion.
