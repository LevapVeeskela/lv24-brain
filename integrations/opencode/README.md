# OpenCode brain hook

`brain-handoff.js` is an OpenCode plugin template with two parts.

**Session start (context injection).** The first `context` call of each
OpenCode session runs `brain session start --hook opencode --cwd <dir>` once
and appends its plain-text project context to the system prompt. The text is
cached per session, so later turns append the same context without running the
command again. This is the OpenCode equivalent of the Claude SessionStart hook.
The command is killed after 10 seconds; empty output, a non-zero exit or an
error injects nothing. The context is not refreshed after a compaction.
Older OpenCode builds without `ctx.session.hook` skip injection silently.

**Session end (handoff).** On `session.idle` (10-minute debounce),
`session.deleted`, and — on V1 only — `global.disposed` (1-minute debounce) it
runs `brain session end --hook opencode --cwd <dir>`. The engine writes an
`[auto]` handoff summarizing what was recorded since the latest handoff, and
skips it when nothing was recorded or a handoff is less than 30 minutes old. A
`session.deleted` event also drops that session's cached start context.

One file supports both runtimes. V1 (1.18.29–2.1.x) loads the default export
and calls `server(ctx)`; V2 (2.2.x) requires an `id` and calls `setup(ctx)`, so
the V2 entrypoint subscribes to `ctx.event.subscribe({ signal })` and forwards
events to the same handler. V2 streams end when the shared server restarts, so
the subscription reconnects with a bounded backoff and the cleanup returned by
`setup` aborts it. All failures are swallowed, so a missing binary or config can
never break an OpenCode session.

Tests: `node --test integrations/opencode/` (no dependencies).

## Install

The Windows installer copies and configures the hook:

```powershell
.\installers\windows\install.ps1 -Mode local -InstallOpenCodeHook
```

Manual install: copy `brain-handoff.js` to
`%USERPROFILE%\.config\opencode\plugins\brain-handoff.js` and replace the
placeholders:

| Placeholder | Value |
|---|---|
| `__BRAIN_BIN__` | absolute path to `brain.exe`, or `brain` on PATH |
| `__BRAIN_CONFIG__` | absolute path to `brain.yaml` |

Environment overrides take precedence: `BRAIN_BIN`, `BRAIN_CONFIG`.

No secrets belong in the hook or in `brain.yaml`. Authentication for server
mode comes from BRAIN_TOKEN or the OS secret `server-token`; no token is stored in this file.
