# Release 0.26.2 — native connector redirects, background run budget

Status: released.

0.26.1 fixed the legacy GitHub connector; the native source used by
`brain connection run` and the daily connector task still failed on a moved
repository. 0.26.2 closes that path.

## Native sources follow same-host redirects

`github.NewSource` and `gitlab.NewSource` now wrap whatever HTTP client the
runtime passes with the same-host redirect policy: up to three redirects are
followed only when the host and scheme stay those of the configured base URL.
A renamed repository answers `301 Moved Permanently` on `api.github.com`, which
previously ended the connection run with `http 301` and made the scheduled task
exit 1; a cross-host redirect is still refused, so the token never leaves the
configured host.

The legacy `github.Connector` gets the same treatment even when a caller
supplies its own client (the runtime passes one with redirects disabled).

## Background connector budget

One background connector run had a five-minute budget. After the raw-zone PII
scrub every GitHub item changed, so the first run had to re-index 2,576 items
and hit `context deadline exceeded`. The budget is now 15 minutes; a completed
run is incremental again. The scheduled task finished with exit 0 on the live
data.

## Scheduled tasks

`brain-task.exe` remains the launcher for Windows scheduled tasks (0.26.1); the
`Company Brain Update`, `Company Brain Graph Refresh` and the local SEO indexing
task all run through it with no console window.
