# Documentation map

Start with [../README.md](../README.md) for the product and [ARCHITECTURE.md](ARCHITECTURE.md) for
the system front door. The public documentation site is <https://brain.lv24.by> (English, Russian,
Chinese). This page indexes the repository docs by audience.

## Everyone

| Doc | What it covers |
|---|---|
| [../README.md](../README.md) | Product, three modes, install, five-command quickstart, status |
| [ARCHITECTURE.md](ARCHITECTURE.md) | Front door: context, components, flows, storage, deployment, invariants |
| [architecture-deep.md](architecture-deep.md) | Deep implementation detail and the full known-gaps table |
| [status.md](status.md) | Current release, shipped capability, gaps, roadmap, all release notes |
| [architecture/README.md](architecture/README.md) | Diagram sources, SVGs and the generated index |

## Users

| Doc | What it covers |
|---|---|
| [quickstart.md](quickstart.md) | First run in local and git modes with expected output |
| [cli.md](cli.md) | Command groups, configuration resolution, exit codes, agent surfaces |
| [install.md](install.md) | npm, PyPI, signed installers and release assets |
| [update.md](update.md) | `brain update`, signed manifests, rollback and channels |
| [pypi.md](pypi.md) | The thin PyPI launcher and its release runbook |
| [migration.md](migration.md) | Importing a legacy Company Brain `.brain` directory |
| [import-engram.md](import-engram.md) | Importing engram memories into brain records |
| [exchange.md](exchange.md) | LV24 Exchange Format (LvEF v1) for portable knowledge |
| [mcp.md](mcp.md) | MCP contract: 18 tools, resources, refusal codes, pagination |
| [search.md](search.md) | Search, ranking, support tiers and progressive disclosure |
| [context-budget.md](context-budget.md) | Layered context packing and token budgets |
| [review.md](review.md) | Review findings, review dates and deterministic duplicates |
| [relations.md](relations.md) | Typed relations and the two-phase merge |
| [limits.md](limits.md) | Write-time limits and protected paths |
| [schema-compat.md](schema-compat.md) | Data schema compatibility and engram fields |
| [code-graph.md](code-graph.md) | The code graph adapter used by `brain code` |

## Operators

| Doc | What it covers |
|---|---|
| [deploy.md](deploy.md) | Single-node server: Docker Compose, Postgres, environment, health |
| [security.md](security.md) | Threat model, secret handling, redaction, roles and privacy |
| [runbook.md](runbook.md) | Day-two team operations, backup and recovery |
| [localapi-v1.md](localapi-v1.md) | Frozen loopback contract used by the desktop app |
| [publishing-plan.md](publishing-plan.md) | npm, PyPI and release distribution plan |
| [audit-2026-11-17.md](audit-2026-11-17.md) | Point-in-time audit of engine 0.24.0 and desktop 1.6.1 (Russian) |

## Agents

| Doc | What it covers |
|---|---|
| [../AGENTS.md](../AGENTS.md) | Five-minute agent entry: commands, invariants, gotchas, brain project id |
| [ARCHITECTURE.md](ARCHITECTURE.md) | The system model an agent needs before changing code |
| [mcp.md](mcp.md) | Tool names, actions, budgets and the `memory://` resources |
| [cli.md](cli.md) | The command surface and exit codes |
| [localapi-v1.md](localapi-v1.md) | Desktop wire contract; additive changes only |
| [status.md](status.md) | What is shipped and which gaps are intentional |

## Contributors

| Doc | What it covers |
|---|---|
| [product-design.md](product-design.md) | Approved product design and non-goals |
| [architecture-deep.md](architecture-deep.md) | Module layout, adapters, storage and server internals |
| [eval.md](eval.md) | Offline retrieval evaluation fixtures, runner and baseline |
| [spec-0.25-second-brain.md](spec-0.25-second-brain.md) | Second-brain correctness spec (Russian) |
| [spec-0.27-honest-retrieval.md](spec-0.27-honest-retrieval.md) | Honest retrieval and belief lifecycle spec (Russian) |
| [spec-0.28-ideas.md](spec-0.28-ideas.md) | Ideas/TODO layer spec; engine 0.28.0 and desktop 1.8.0 |
| [ideas-contract.md](ideas-contract.md) | Frozen 0.28 storage, CLI, MCP and local-API contract |
| [plans/2026-11-27-ideas.md](plans/2026-11-27-ideas.md) | 0.28 implementation plan and workstreams |
| [idea-scan-2026-11.md](idea-scan-2026-11.md) | Survey of eight agent-memory MCP servers |

## Release notes

Every release from 0.15 to 0.28.0 has a note under `docs/release-*.md`. The indexed table is in
[status.md](status.md#release-history); the latest is
[release-0.28.0.md](release-0.28.0.md).

## Conventions

- `README.md` is the landing page; status and version live in [status.md](status.md); commands live
  in [cli.md](cli.md) and [quickstart.md](quickstart.md); deep design lives in
  [architecture-deep.md](architecture-deep.md).
- Diagrams are generated, not hand-written: edit `architecture/NN-*.mmd` and re-run
  `lv24-diagram render <file> --saved "<title>"`.
- New files are in English. Existing Russian specs and audits are preserved as written.
- `ARCHITECTURE.md` is the front door. The original deep file is `architecture-deep.md`; it was
  moved from `architecture.md` because Windows cannot host two paths that differ only by case.
