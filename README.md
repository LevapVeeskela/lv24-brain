![LV24 Brain — shared memory for AI coding agents](.github/assets/banner.jpg)

# LV24 Brain

[![npm](https://img.shields.io/npm/v/lv24-brain?label=npm&color=cb3837&logo=npm)](https://www.npmjs.com/package/lv24-brain)
[![PyPI](https://img.shields.io/pypi/v/lv24-brain?label=PyPI&color=3775A9&logo=pypi&logoColor=white)](https://pypi.org/project/lv24-brain/)
[![release](https://img.shields.io/github/v/release/LevapVeeskela/lv24-brain?color=0b6b5f&logo=github)](https://github.com/LevapVeeskela/lv24-brain/releases)
[![GHCR](https://img.shields.io/badge/GHCR-lv24--brain--server-2496ED?logo=docker&logoColor=white)](https://github.com/LevapVeeskela/lv24-brain/pkgs/container/lv24-brain-server)
[![docs](https://img.shields.io/badge/docs-brain.lv24.by-5fd4c4)](https://brain.lv24.by)
[![license](https://img.shields.io/badge/license-Apache--2.0-2ea44f)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white)](go.mod)

**Your coding agents forget everything between sessions. LV24 Brain doesn't.**

Claude Code, Codex, OpenCode and any MCP client read the same project memory at the start of every
session and write back what they decided, what failed and what comes next — on every machine you
work on. The data is plain Markdown in your own private git repository: no cloud, no vendor lock-in,
no database to run.

**Docs:** [Documentation site](https://brain.lv24.by) · [Architecture](docs/ARCHITECTURE.md) ·
[Agent entry](AGENTS.md) · [MCP](docs/mcp.md) · [CLI](docs/cli.md)

> **Кратко по-русски.** Общая память проектов для ИИ-агентов и людей: состояние проекта, решения,
> неудачные попытки и handoff'ы между сессиями. Агенты открывают сессию с контекстом проекта и
> оставляют итог в конце — на всех ваших машинах. Данные — обычный Markdown в вашем приватном
> git-репозитории. Установка: `npm i -g lv24-brain`, затем `brain agents sync --hooks`.
> Документация на русском — [brain.lv24.by/ru](https://brain.lv24.by/ru/).

## What it is

- **Memory that survives the session.** State, decisions, attempts and handoffs are written as the
  work happens and read back at the next start by any agent, on any machine.
- **Works with the agents you use.** Session hooks for Claude Code, Codex and OpenCode, plus an MCP
  server with 18 tools (`brain_open`, `brain_context`, `brain_search`, `brain_idea`, …).
- **Your data, your repository.** Markdown + YAML in a private git repo you own, synced with plain
  git. Secrets are redacted on every write; this public repository never holds brain data.
- **Finds what you mean.** Full-text search matches English and Russian word forms
  (`decisions` → `decision`, `решений` → `решение`), with `[[id]]` links and backlinks.
- **Keeps itself honest.** `brain review` flags stale next steps, answered questions, decisions
  contradicted by later attempts or changed code, broken links, near-duplicates and proposals.
- **Grows into a team server.** Optional `brain-server` with Postgres, roles, per-project ACL,
  scoped API tokens, OIDC and an audit log.

**Not:** a cloud SaaS, a vector database, a multi-tenant platform, a CRDT editor, or a replacement
for git. The public repository is the engine; your brain data lives elsewhere.

## Status

| Item | Value |
|---|---|
| Version | `0.28.0` (release commit; `v0.28.0` tag pending) |
| Ship state | Engine 0.28.0 release commit; tag, registry publish and docs deploy pending (2026-11-27) |
| Visibility | Public engine, private data |
| Repo | [LevapVeeskela/lv24-brain](https://github.com/LevapVeeskela/lv24-brain) |
| Docs site | <https://brain.lv24.by> (en / ru / zh) |
| Hosted server | none shipped — self-host with [deploy/](deploy/) |
| License | Apache-2.0 |

Detailed capability, known gaps and release history: [docs/status.md](docs/status.md).

## At a glance

| Stack | Entry points | Ports | Data | Deploy |
|---|---|---|---|---|
| Go 1.27, stdlib HTTP, pgx, modernc SQLite FTS5, MCP Go SDK | `cmd/brain`, `cmd/brain-server` | `8080` (server), ephemeral loopback (desktop) | Markdown + SQLite FTS5, or Postgres schema 10 | npm, PyPI, signed installers, Docker Compose, GHCR |

## Three modes

| Mode | Store | Sync | Use when |
|---|---|---|---|
| `local` | Markdown + SQLite FTS5 | none | One machine, offline, scratch brain |
| `git` | Markdown + SQLite FTS5 | `gitstore`: fetch, rebase, commit, optional push | Several machines or people, reviewable history (recommended default) |
| `server` | HTTP API + Postgres | API + SSE | A team wants one shared service and central accounts |

The same use cases run in all three; only adapters change. Server mode is single-node.

## Install

| Channel | Command |
|---|---|
| npm (Node.js 18+) | `npm i -g lv24-brain` |
| PyPI launcher (needs Node.js 18+) | `pip install lv24-brain` |
| Signed binary, macOS/Linux | `./installers/unix/install.sh --mode local --yes` |
| Signed binary, Windows | `.\installers\windows\install.ps1 -Mode local` |

The PyPI package is a thin launcher: it runs `npx --yes lv24-brain@<version>`, installs only
`lv24-brain` (never `brain`), and does not bundle Node.js. npm, the signed installers and GitHub
Releases are the canonical distribution. See [docs/install.md](docs/install.md) and
[docs/pypi.md](docs/pypi.md).

## Quick start

```sh
brain init --mode local --data-dir ~/brain
brain config use ~/brain/brain.yaml
brain project set myapp --path ~/repo/myapp
brain agents sync --hooks
cd ~/repo/myapp && brain open
```

That is the whole loop: agents read `brain open` (or `brain context "<task>"`) at session start and
leave a handoff at the end. Next: [quickstart](docs/quickstart.md) ·
[git mode across machines](docs/quickstart.md#2-git-mode) · [server mode](docs/deploy.md).

## See it work

```text
$ brain context "add retry to sync push"

# myapp — context
goal: ship offline sync for the mobile client
next_action: add retry with backoff to `sync push`

## Latest handoff
- 2026-11-08 summary: queue drains on reconnect; push still fails on 502
  next: add retry with backoff to `sync push`

## Relevant records
- [decision] Retry only idempotent pushes (accepted)
- [note] 502s come from the proxy restart window, ~20 s

## Failed attempts matching the task
- [attempt] Infinite retry loop — hammered the API during the outage
```

One call returns state, the last handoff, matching decisions and notes, and approaches that already
failed. Raw connector text stays out unless `--raw` is passed and an explicit grant exists.

## Architecture

- **Hexagonal core.** `internal/domain` ← `internal/ports` ← `internal/app`; adapters implement
  ports and are selected by `internal/bootstrap` per mode.
- **Files are the source of truth** in local/git modes. Markdown + YAML frontmatter under
  `projects/<id>/`; the SQLite FTS5 index is derived and rebuildable.
- **Git is the sync protocol.** Deterministic merge drivers for state, notes, registry and raw items
  preserve both sides in committed conflict artifacts; sync never force-pushes.
- **The server is optional.** One Go binary, Postgres schema 10, JWT/API tokens/OIDC, roles,
  per-project ACL, audit log and SSE revisions.
- **The desktop app talks to a frozen local contract.** `localapi-v1` is loopback-only, bearer-token
  authenticated, exact-Host checked and additive.

Read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the front door and
[docs/architecture-deep.md](docs/architecture-deep.md) for the deep dive. Diagram sources and SVGs live in
[docs/architecture/](docs/architecture/).

## Repository map

| Path | Purpose |
|---|---|
| `cmd/brain/` | CLI entry point and command dispatch |
| `cmd/brain-server/` | HTTP server entry point |
| `internal/domain/`, `app/`, `ports/`, `adapters/`, `bootstrap/` | Hexagonal core and composition root |
| `internal/mcpserver/`, `adapters/localapi/`, `daemon/` | Agent, desktop and local control surfaces |
| `internal/mergedriver/`, `adapters/gitstore/` | Git sync and merge drivers |
| `deploy/` | Dockerfile, Compose, GoReleaser, environment template |
| `installers/`, `npm/`, `python/` | Distribution channels |
| `docs/`, `docs/architecture/` | Documentation and diagrams |

## Documentation

| Doc | What it covers |
|---|---|
| [Documentation site](https://brain.lv24.by) | Guides, reference and FAQ in English, Russian and Chinese |
| [Architecture front door](docs/ARCHITECTURE.md) | Modes, components, storage, deployment, invariants |
| [Deep architecture](docs/architecture-deep.md) | Detailed implementation and known gaps |
| [Agent entry](AGENTS.md) | Five-minute read path, verified commands, invariants |
| [MCP server](docs/mcp.md) | 18 tools, resources, refusal codes, pagination |
| [CLI](docs/cli.md) | Command groups, exit codes, the generated CLI reference |
| [Status](docs/status.md) | Capability, known gaps, roadmap, all release notes |
| [Quickstart](docs/quickstart.md) · [Install](docs/install.md) · [Update](docs/update.md) | Running and upgrading |
| [Server deploy](docs/deploy.md) · [Security](docs/security.md) · [Runbook](docs/runbook.md) | Operations |
| [Search](docs/search.md) · [Context budget](docs/context-budget.md) · [Review](docs/review.md) | Retrieval and curation |
| [Relations](docs/relations.md) · [Limits](docs/limits.md) · [Schema compatibility](docs/schema-compat.md) | Data model details |
| [Release 0.28.0](docs/release-0.28.0.md) | Latest notes; the full `docs/release-*` set is indexed in [status](docs/status.md#release-history) |

## For AI agents

Read in this order:

1. [AGENTS.md](AGENTS.md) — start here, commands, invariants, gotchas.
2. [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — components, flows, storage, failure modes.
3. [docs/README.md](docs/README.md) — document map by audience.
4. [docs/mcp.md](docs/mcp.md) if you talk MCP; [docs/cli.md](docs/cli.md) if you shell out.
5. Code entry points: `cmd/brain/main.go`, `internal/bootstrap/bootstrap.go`, then the touched
   adapter or use case.

Hard invariants: dependencies point inward; MCP stdout is protocol-only; the public repo never holds
brain data or secrets; writes are atomic and the index is derived; sync never force-pushes; the
local API is loopback-only and additive. Do not run `brain` against the public repository as a data
directory.

## License and cost

Apache-2.0. See [LICENSE](LICENSE).

Designed for zero paid hosting and CI: GitHub Actions on the free allowance, GitHub Releases and
self-hosted Docker Compose. No paid runners, storage add-ons or marketplace services are required.
