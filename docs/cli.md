# CLI

`brain` is the single command-line entry point. It works in `local` and `git` modes directly against
a data directory, and in `server` mode through the HTTP API. This page is a map; the generated
command reference lives at <https://brain.lv24.by/reference/cli/>.

## Configuration

Commands resolve `brain.yaml` in this order: `--config <path>`, `$BRAIN_CONFIG`, `./brain.yaml`,
then the pointer written by `brain config use <path>`. Local and git modes need `data_dir`; server
mode needs `server_url`.

```sh
brain init --mode local --data-dir ~/brain
brain config use ~/brain/brain.yaml
brain status
```

## Command groups

| Group | Commands |
|---|---|
| Start and onboard | `init`, `onboard`, `doctor`, `config`, `discover`, `project`, `agents` |
| Session context | `open`, `bootstrap`, `context`, `catchup`, `session`, `mcp` |
| Write records | `note`, `decision`, `attempt`, `handoff`, `state` |
| Curate and review | `review`, `duplicates`, `consolidate`, `promote`, `digest`, `record`, `prompt` |
| Read and navigate | `search`, `get`, `timeline`, `links`, `relation`, `journal`, `status` |
| Sync and index | `sync`, `index`, `hooks`, `event`, `graphs`, `code` |
| Connectors | `connection`, `connector`, `agent`, `secret` |
| Server mode | `login`, `logout`, `password`, `token`, `serve` |
| Portability | `migrate`, `export`, `import` |
| Maintenance | `update`, `eval`, `completion`, `version`, `help` |

`brain help` is authoritative for flags. Common task-shaped entry points:

```sh
brain open                                  # session-start brief for the current project
brain context "add retry to sync push"      # task-shaped context pack
brain search "checkpoint" --since 7d         # full-text search
brain review                                # stale knowledge and pending proposals
brain sync --status --json                  # read-only sync state
brain sync --push                           # commit, rebase, push
brain record list --as-of 2026-11-01         # historical view
```

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Success |
| `1` | Runtime failure |
| `2` | Usage or validation error |
| `3` | Unmerged git conflict (reported by `sync` and `onboard`) |

## Agent surfaces

- MCP over stdio: `brain mcp` — see [mcp.md](mcp.md).
- Session hooks: `brain agents sync --hooks` installs the Claude Code, Codex and OpenCode hooks;
  `brain session start|end` is what the hooks call.
- Server client: `brain login` stores one OS-secret-store bundle bound to `server_url`; `BRAIN_TOKEN`
  overrides stored credentials for automation.

## See also

- [Quickstart](quickstart.md) — the full first-run walkthrough with expected output.
- [Architecture](ARCHITECTURE.md) — how commands map to use cases and adapters.
- [MCP](mcp.md) — the 17-tool agent surface.
- [Status](status.md) — what is shipped and what is still a gap.
