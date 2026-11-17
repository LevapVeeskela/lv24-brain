# Release 0.22.0 — PII untrack, legacy profile removal, auto-sync

Status: release candidate on `main`; tag `v0.22.0`.

0.22 closes the legacy publishing gap and removes the last deprecated MCP
vocabulary. The data schema stays `schema_version: 1`; there is no data
migration and no history rewrite.

## Removed: the `lv24-legacy` MCP profile

The 0.16 compatibility bridge was deprecated in 0.17 and announced for removal
in 0.19. It is gone:

- `--profile=lv24-legacy` is a startup error with exit code 2 and lists the
  supported profiles: `lv24`, `engram`, `codegraph`, `graphify`.
- The seven retired names (`brain_state_get`, `brain_state_set`,
  `brain_journal`, `brain_records`, `brain_relations`, `brain_review_record`,
  `brain_decision_status`) are no longer served. On `lv24` they still return
  `NOT_FOUND` with the replacement in `try_instead`.

| 0.16 name | Replacement |
|---|---|
| `brain_state_get` | `brain_state(action=get)` |
| `brain_state_set` | `brain_state(action=set)` |
| `brain_journal` | `brain_catchup(mode=journal)` |
| `brain_records` | `brain_record(action=list)` |
| `brain_relations` | `brain_record(action=relations)` |
| `brain_review_record` | `brain_judge(action=review)` |
| `brain_decision_status` | `brain_judge(action=accept\|reject\|supersede)` |

## PII and legacy dump zones

The publish policy governs `connectors/<id>`; the older `mail/`, root
`github/`, `gitlab/` and `merges/` trees predated it and were still published.

- `mail/` (584 files), root `github/` (16), root `gitlab/` (3), `merges/` (21)
  and the orphan `connectors/gitlab/` (5) are untracked with
  `git rm --cached`: the files stay on disk, git history is not rewritten, and
  a local `backups/pii-untrack-*.zip` copy is kept.
- `.gitignore` now anchors `/mail/`, `/github/`, `/gitlab/` and `/merges/`, so
  `git add -A` cannot publish them again. `connectors/github` and
  `connectors/gitlab-gitlab-com` are unaffected and stay governed by their
  connection policy.
- `brain doctor` reports any tracked file in these zones as `pii-zone` with
  code `tracked-pii-zone` and the exact untrack command. It reports only; it
  never deletes files or rewrites history.
- A worktree copy removed by pulling the untrack commit can be restored from
  history: `git checkout <commit>^ -- mail/gmail/<date>.md`.

## Automatic sync

On Windows, the 30-minute "Company Brain Update" task now runs the signed
engine update and then `brain sync --push`; a clean tree is a cheap no-op and an
interrupted rebase is recovered before the pull. The nightly "Company Brain
Graph Refresh" task still runs graphs, connectors, `sync --push` and
`agents sync`. Both tasks call `lv24-brain/scripts/`; the retired `.brain`
wrapper folder was archived and removed.

`brain sync --status --json` lists unmerged paths and driver conflict records;
`brain review` and `brain doctor` report pending conflict artifacts. The
desktop git-sync page is not part of 0.22.

## Migration notes

- Run `brain doctor --fix` once per clone after upgrading; it registers the
  current merge drivers and appends the new ignore lines.
- If a clean other machine pulls the untrack commit, its `mail/`, root
  `github|gitlab/` and `merges/` worktree copies disappear. The blobs remain in
  git history; restore individual files with `git checkout <commit>^ -- <path>`
  before relying on them. Connector runs regenerate GitHub/GitLab dumps; IMAP
  mail is re-fetched from the server.
- Desktop 1.5.1 bundles engine 0.22.0 and adds one-shot recovery from the
  WebView2 `missing Origin header` IPC startup race; the error banner offers an
  explicit Reload action for a persistent failure.

## Known gaps

- Desktop git-sync/conflict UI and a scheduled sync on macOS remain 0.23 work.
- Server-canonical mode waits for a second human writer; git stays canonical
  through 0.22–0.23.
- `merges/` is local-only, so conflict manifests are not shared across
  machines; `sync --status --json` still reports the local set.
