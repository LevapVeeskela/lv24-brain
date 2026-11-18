# Release 0.26.1 — hidden Windows tasks, moved repositories

Status: released.

## Windows scheduled tasks no longer open a console

Task Scheduler starts an interactive task in the user's session, so a `.bat` or
a console program such as `brain.exe` flashed a cmd window every interval.
`cmd/braintask` is a GUI-subsystem launcher (`brain-task.exe`) that runs the
command with `CREATE_NO_WINDOW`, waits for it and returns its exit code, so
`LastTaskResult` still reports failures. The `Company Brain Update`, `Company
Brain Graph Refresh` and the local SEO indexing task now run through it, and
the task `Hidden` flag is set as well. `brain onboard` and the runbook describe
the launcher for new machines.

## Connectors follow same-host redirects

GitHub answers a renamed repository with `301 Moved Permanently` on
`api.github.com`. The shared HTTP helper disabled redirects to keep the token
safe, so the daily connector run failed with `http 301` and the whole scheduled
task exited 1. GitHub and GitLab now follow up to three redirects **only** when
the host and scheme stay the same; a cross-host or scheme-changing redirect is
still refused, so the token never leaves the configured base URL. A unit test
covers the policy.
