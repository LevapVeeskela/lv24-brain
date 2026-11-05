# LV24 Brain

Shared project memory with a local-first core: notes, decisions, attempts,
handoffs, and state for teams of humans and agents. Apache-2.0.

> **Status: foundation scaffold (WP0).** This repository currently ships the Go
> module layout, domain types, ports, configuration, CLI/server skeletons, and
> templates. `brain version`, `brain help`, and `brain status` work; storage,
> search, sync, connectors, server auth, and auto-update land in later work
> packages and return a typed "not implemented" error today.

Product design (source of truth): [`docs/product-design.md`](docs/product-design.md).
in the LV24 workspace.

- Public engine: this repository (`LevapVeeskela/lv24-brain`).
- Private data instance: `LevapVeeskela/lv24-brain-data` (never this repo).

## Requirements

- Go 1.27 or newer.
- Git for `git` mode.

## Build

```sh
go build ./cmd/brain
go build ./cmd/brain-server
go test ./...
go vet ./...
```

The quickstarts below show the target workflows. In the current scaffold,
`init`, `note`, `decision`, `attempt`, `handoff`, `bootstrap`, `review`, `sync`,
`doctor`, and `update` return a typed `not implemented` error until their work
package lands; `version`, `help`, and `status` work today.

## Quickstart: local mode

Single user, filesystem data, no sync.

```sh
brain init --mode local --data-dir ~/brain
brain status --config ~/brain/brain.yaml
```

## Quickstart: git mode

Team mode: a private data repository is the shared brain.

```sh
git clone git@github.com:LevapVeeskela/lv24-brain-data.git ~/brain-data
brain init --mode git --data-dir ~/brain-data --config ~/brain-data/brain.yaml
brain sync --status --config ~/brain-data/brain.yaml
```

`brain sync` performs pull/rebase, optional commit, and push. Secrets, indexes,
and backups never enter the data repository.

## Quickstart: server mode

Central server with Postgres, OIDC, JWT/API tokens, and roles
`admin|member|viewer`.

```sh
brain-server --addr 127.0.0.1:8080
brain init --mode server --server-url https://brain.example.com --config brain.yaml
```

The scaffold server exposes `GET /health`, `GET /ready`, and `GET /version`.
There is no database or authentication yet.

## Repository layout

```text
cmd/brain/          CLI
cmd/brain-server/   HTTP server
internal/domain/    entities and validation errors
internal/app/       use cases (skeletons)
internal/ports/     Store, Index, Sync, Secrets, Connectors, Auth, Update
internal/config/    brain.yaml load/save/validate
internal/server/    shared /health, /ready, /version handler
internal/version/   build identity
templates/          state, decision, attempt, handoff, AGENTS.md
docs/               architecture and security
```

Adapters (`fsstore`, `gitstore`, `sqliteindex`, `postgres`, `httpapi`, `oidc`,
connectors) arrive in later work packages.

## Documentation

- [Architecture](docs/architecture.md)
- [Security and privacy](docs/security.md)

## License

Apache-2.0. See [LICENSE](LICENSE).
