# Release 0.24.0 — path provenance backfill

Status: released on `main` (2026-11-17).

0.23.0 added `path_provenance` to the project registry but only stamped it when
a machine registered a path. Registries written earlier had paths without
owners, so the desktop Projects page could not name the machine behind an
existing checkout. 0.24.0 closes that gap.

## `brain project adopt`

- Reads the registry and classifies every registered path for the current
  machine: `owned` (a machine is recorded), `stamped` (the path exists here and
  this machine becomes its owner) or `missing` (no local checkout).
- Dry run by default; `--apply` writes the owners. `--json` returns the report
  in the standard `{schema, data}` envelope.
- Never overwrites an owner recorded by another machine and never stamps a path
  that does not exist locally, so a shared `C:\0projects\...` path keeps the
  first machine that registered it.
- Idempotent: a second run reports the paths as `owned`.

## `brain onboard` stamps provenance

`brain onboard` now runs the same adoption after the doctor checks and reports
`ok: provenance (N paths stamped for <machine>, M already owned, K not on this
machine)`. One onboarding run per machine is enough to make the registry name
every checkout it holds. `Provenance` is part of the `--json` report.

## Desktop

Desktop **1.6.1** ships with engine **0.24.0** and fixes the machine chip on
macOS and Linux: the app bar reads the host name with `gethostname(2)` when the
GUI process has no `HOSTNAME` environment variable, so the chip matches the
machine id used in `path_provenance`.

## Upgrade

Engine 0.24.0 and desktop 1.6.1 are drop-in; the data schema is unchanged. Run
`brain onboard` on each machine once (or `brain project adopt --apply`) and
`brain sync --push` so the provenance reaches the other machines.
