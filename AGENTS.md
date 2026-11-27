# Agent entry

This is the five-minute read path for coding agents working in `lv24-brain`. It is not product
documentation; the user-facing entry is [README.md](README.md).

## Start here

1. [README.md](README.md) — what the product is, install, quick start, status.
2. [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — modes, components, storage, flows, invariants.
3. [docs/README.md](docs/README.md) — document map by audience.
4. Code entry points: `cmd/brain/main.go`, `cmd/brain-server/main.go`,
   `internal/bootstrap/bootstrap.go`, then the touched use case or adapter.

## What this repository is / is not

- **Is:** the public Apache-2.0 Go engine — CLI, HTTP server, MCP server, connectors, installers,
  deploy files, docs and diagrams.
- **Is not:** a brain data repository. Notes, mail, connector dumps, journals, indexes, backups,
  credentials and machine paths belong in a private data repository or a local data directory.
- **Is not:** a multi-tenant SaaS. Server mode is single-node; multi-replica deployment is
  unsupported. See [docs/status.md](docs/status.md) for gaps.

## Commands (verified)

```sh
go build -o bin/brain ./cmd/brain
go build -o bin/brain-server ./cmd/brain-server
go test ./...
go vet ./...
gofmt -l .
go test -race ./...
```

Postgres integration tests need a database DSN:

```sh
BRAIN_TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/brain?sslmode=disable' \
  go test -count=1 -p 1 ./internal/adapters/postgres ./internal/adapters/serverstore ./internal/server
```

Windows PowerShell:

```powershell
go build -o bin\brain.exe .\cmd\brain
go build -o bin\brain-server.exe .\cmd\brain-server
go test ./...
```

Optional helpers:

```sh
go run ./cmd/brain eval --config ~/brain/brain.yaml   # offline retrieval metric
go build -ldflags "-H windowsgui" -o bin/brain-task.exe ./cmd/braintask
```

`cmd/braintask` is a Windows GUI-subsystem launcher for hidden `.cmd`/`.bat` MCP servers. It is
built locally; the release workflow does not ship it.

## Repository map

| Path | Purpose |
|---|---|
| `cmd/brain/` | CLI dispatch and flags |
| `cmd/brain-server/` | HTTP server process |
| `cmd/braintask/` | Windows windowless launcher |
| `internal/domain/` | Pure entities, value objects, validation |
| `internal/app/` | Use cases |
| `internal/ports/` | Interfaces only |
| `internal/adapters/` | fsstore, gitstore, sqliteindex, httpapi, httpclient, postgres, connectors, localapi |
| `internal/bootstrap/` | Composition root; selects adapters by mode |
| `internal/mcpserver/` | MCP stdio surface (18 tools) |
| `internal/daemon/`, `internal/agentcontrol/` | Background agent and local API wiring |
| `internal/mergedriver/` | Git merge drivers |
| `internal/server/` | Server config, migrations, retention, lifecycle |
| `deploy/` | Dockerfile, Compose, GoReleaser, env template |
| `docs/architecture/` | Diagram sources, SVGs and index |

## Invariants (never break)

1. **Dependencies point inward.** `domain` ← `ports` ← `app`; adapters and `bootstrap` depend on
   them, never the reverse. `domain` imports no project packages and no I/O libraries; `ports`
   depends only on the standard library and domain.
2. **MCP stdout is protocol-only.** Diagnostics go to stderr. Tests reject any non-JSON stdout line.
3. **No secrets or brain data in the public repo.** Redact before persistence; resolve credentials
   from `BRAIN_SECRET_*` or the OS secret store; never write them to `brain.yaml`, docs or tests.
4. **Writes are atomic; the index is derived.** Temp file + rename, `index/dirty` on failure, and
   search-time repair. Never make FTS5 the source of truth.
5. **Merge drivers preserve both sides.** State, notes, registry and raw items merge
   deterministically; conflicts become committed artifacts, never silent drops.
6. **Git sync never force-pushes.** Retry fetch + rebase; a conflict exits 3 and leaves the paths
   visible.
7. **`localapi-v1` is frozen and additive.** Never remove or rename a route; update
   `internal/adapters/localapi/openapi.json`, `testdata/localapi-v1.operations`, the route table in
   `docs/localapi-v1.md` and the contract tests together.
8. **Server migrations are forward-only.** Add a new embedded migration and bump
   `postgres.SchemaVersion`; never edit an applied migration. The deployment never publishes
   Postgres on 5432.

## Gotchas

- **Windows paths.** Use `.\cmd\brain` and `bin\brain.exe`; PowerShell does not reliably expand
  `~/brain` for native commands — prefer `$env:USERPROFILE\brain`. Do not use `/tmp` in repo
  scripts; use `$env:TEMP`.
- **No console windows.** GUI-hosted spawns must go through `internal/winproc.Hide` or the
  `brain-task --stdio` wrapper; a plain `.cmd` shim flashes a console (0.27.2 regression class).
- **Release order.** Bump `python/pyproject.toml` and `python/src/lv24_brain/__init__.py` before
  tagging. Push `vX.Y.Z`; `release.yml` builds binaries and calls `npm-publish`. Because the
  release is created with `GITHUB_TOKEN`, the PyPI release event does not fire: run
  `gh workflow run pypi-publish.yml --ref main -f version=X.Y.Z` after the GitHub release, then
  verify PyPI and npm show the same version.
- **Docs change rules.** README is the landing page (≤200 lines); status lives in
  `docs/status.md`; deep design in `docs/architecture-deep.md`; the front door is
  `docs/ARCHITECTURE.md`. Do not duplicate the command list or status tables across them.
- **Case-only docs path.** Windows aliases `docs/ARCHITECTURE.md` and the old
  `docs/architecture.md`. The deep file is now `docs/architecture-deep.md`. When staging on
  Windows, use `git -c core.ignorecase=false add -A` (or a case-only `git mv`) so Git records
  `docs/ARCHITECTURE.md` and deletes the old path; a plain `git add -A` keeps the lowercase name
  and breaks case-sensitive links.
- **Diagrams.** Sources are `docs/architecture/NN-*.mmd`. Regenerate with
  `lv24-diagram render <file.mmd> --saved "<title>"` from the repo root; never hand-edit an SVG.
  Keep ≤15 nodes, depth ≤3, edge labels ≤40 characters and no lint warnings. The tool writes a
  Russian table header; restore the English header and intro after a re-render.
- **One data dir, one agent.** `brain agent` takes an OS lock per data directory; `brain sync` takes
  an interprocess lock for the whole run. Do not start a second writer against the same checkout.
- **Server mode differences.** No `brain sync`, no local `brain index`, no CLI OIDC flow, no server
  auto-update, and no multi-replica support. Do not "fix" these silently.
- **Build identity.** A local `go build` reports `0.1.0`; release builds inject the tag with ldflags. That is expected.

## Where to record decisions

The brain project id for this repository is **`lv24-brain`**. At the end of a session:

```sh
brain open lv24-brain
brain note "what changed" --project lv24-brain
brain decision "title" --context "..." --project lv24-brain
brain attempt "title" --hypothesis "..." --result "..." --reason "..." --project lv24-brain
brain handoff "summary" --next "next action" --project lv24-brain
```

Never store secrets, tokens, passwords or personal data in brain records. End the final message with
one `Decision: <short title>` line per decision so the session hook can propose it for review.

## Links

- [Architecture front door](docs/ARCHITECTURE.md) · [Deep architecture](docs/architecture-deep.md)
- [Docs map](docs/README.md) · [Status](docs/status.md) · [MCP](docs/mcp.md) · [CLI](docs/cli.md)
- [Deploy](docs/deploy.md) · [Security](docs/security.md) · [Runbook](docs/runbook.md)
- [Documentation site](https://brain.lv24.by) · [Repository](https://github.com/LevapVeeskela/lv24-brain)
