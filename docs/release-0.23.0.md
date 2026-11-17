# Release 0.23.0 вЂ” desktop git sync, machine provenance, onboarding

Status: released; tag `v0.23.0`.

0.23 closes the вЂњthree machines can write but the desktop cannot see syncвЂќ
gap and makes a new machine a one-command setup.

## Desktop git sync

- `GET /v1/sync` reports the data repository state: branch, ahead/behind,
  changed, dirty, unmerged paths, conflict records, recovery/reindex flags and
  the last synced revision.
- `POST /v1/sync` with `{"push":true}` runs the same commit/rebase/push use
  case as the CLI. Unmerged paths return `409 sync_conflict`.
- The desktop has a **Git sync** page with refresh and *Sync now* actions;
  conflicts are shown as file paths and driver conflict records.
- The page works against the agent's local API; the webview never runs git or
  sees a token.

## Machine and key provenance

- `GET /v1/projects` now returns each project's registered `paths` and
  `path_provenance` (`path -> machine id`).
- The Projects page shows the path and the machine that registered it, and a
  machine summary card. Connection credentials state which machine's OS
  keychain holds them; Settings shows the current machine.
- This makes it visible which machine contributed a checkout/key path instead
  of presenting an anonymous shared registry.

## Unified onboarding

- `brain onboard [--json] [--config <path>]` runs the safe doctor repairs
  (merge drivers, managed `.gitattributes`/`.gitignore`), checks schema, PII,
  raw zones and git divergence, and prints the platform-specific
  `brain sync --watch --push --interval=5m` command.
- It never installs a service or rewrites records. Exit 3 reports unresolved
  conflicts.

## Desktop shell

The desktop now follows the stronger SEO-desktop layout: a persistent left
sidebar with brand, project selector and grouped navigation; a sticky app bar
with the section title, machine chip, engine status and locale; content cards
in a constrained reading column. The old top brand card and horizontal rail
are gone.

## Versions

- Engine **0.23.0**; desktop **1.6.0** bundles engine 0.23.0.
- Data schema unchanged; no migration.
- Server-canonical mode is still deferred until a second human writer or
  same-field real-time writes require it.

