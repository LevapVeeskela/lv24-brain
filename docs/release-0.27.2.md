# Release 0.27.2 — no console windows from background helpers

Status: released.

## Root cause

Two separate paths opened a `cmd` window:

1. **OpenCode MCP servers.** OpenCode starts its MCP servers from the desktop
   app. Three entries were `.cmd` shims (`codegraph.cmd`, `npx.cmd`), and Node
   runs a `.cmd` through `cmd.exe /d /s /c`, which allocates a visible console.
   Every time the app (re)started its MCP set, a Windows Terminal tab flashed.
2. **`brain graphs`.** The code-graph refresh runs `codegraph sync` through
   `exec.Command`, which can allocate a console when the parent has none.

The desktop app and the scheduled tasks already used `CREATE_NO_WINDOW`; the
scheduled-task wrappers were verified to produce no visible window.

## Fix

- New `internal/winproc.Hide(*exec.Cmd)` sets `HideWindow` and
  `CREATE_NO_WINDOW` on Windows and is a no-op elsewhere. It is applied to the
  code-graph commands, the `gh` fallback and the remaining process spawns that
  can run under a GUI host.
- `brain-task.exe` gained a `--stdio` mode: it is a GUI-subsystem launcher, so
  the parent sees a windowless process, while the child keeps the inherited
  stdin/stdout/stderr pipes. It runs `.cmd`/`.bat` targets through a hidden
  `cmd /d /c`, preserving the npx environment.
- The three OpenCode MCP entries are now arrays:
  `["C:/0project/lv24-brain/bin/brain-task.exe", "--stdio", "..."]`. The server
  behaviour is unchanged; the console is gone. Backups of the config are kept
  next to it (`opencode.json.bak-before-*`).
- `brain-task.exe` is built locally with
  `go build -ldflags "-H windowsgui" -o bin/brain-task.exe ./cmd/braintask`;
  the release workflow does not ship it yet.

## Verification

- An MCP `initialize` handshake through `brain-task --stdio` returned a result
  for codegraph, context7 and the filesystem server.
- OpenCode loaded all three servers through the wrapper; 13 helper processes
  ran with no visible window in the window watcher.
- `Company Brain Update` and `Company Brain Graph Refresh` were triggered with
  the watcher running: no console window.
