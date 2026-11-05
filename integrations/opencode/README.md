# OpenCode handoff hook

`brain-handoff.js` is an OpenCode plugin template. It appends a Brain handoff
on `session.idle` (10-minute debounce), `session.deleted`, and
`global.disposed` (1-minute debounce).

OpenCode loads the default export and calls `server(ctx)`; the returned
`event` hook runs `brain handoff --summary ... --config ...`. All failures are
swallowed, so a missing binary or config can never break an OpenCode session.

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
| `__HANDOFF_SUMMARY__` | short handoff summary |

Environment overrides take precedence: `BRAIN_BIN`, `BRAIN_CONFIG`,
`BRAIN_HANDOFF_SUMMARY`.

No secrets belong in the hook or in `brain.yaml`. Authentication for server
mode comes from BRAIN_TOKEN or the OS secret `server-token`; no token is stored in this file.
