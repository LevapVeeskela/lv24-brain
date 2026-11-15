# Write-time limits and protected paths

`brain.yaml` can bound what one record write may contain and refuse writes to
projects or data-directory paths an agent must not rewrite. The limits are
enforced by the engine, at the app boundary, so the CLI, MCP, session hooks and
`brain promote` all obey the same rule. They are additive: a configuration
without a `limits` block keeps the defaults below.

## Configuration

```yaml
limits:
  max_body_bytes: 1048576      # 1 MiB, the default
  max_title_chars: 200         # the default
  max_metadata_bytes: 16384    # 16 KiB, the default
  protected_projects:          # default: empty
    - global
    - templates
  protected_globs:             # default: empty, slash-separated
    - projects/global/**
    - projects/templates/**
```

| Key | Default | Effective minimum | Counts |
|---|---|---|---|
| `max_body_bytes` | `1048576` (1 MiB) | 1 byte | Free text of the record, UTF-8 bytes |
| `max_title_chars` | `200` | 1 code point | The title in Unicode code points; a handoff has no title |
| `max_metadata_bytes` | `16384` (16 KiB) | 1 byte | Combined UTF-8 bytes of the `tags`, `evidence`, `alternatives` and `consequences` lists |
| `protected_projects` | empty | — | Project ids; a write to one is refused |
| `protected_globs` | empty | — | Data-directory paths; a matching write is refused |

Rules for zero and negative values:

- An omitted key and an explicit `0` both select the default. **Zero never
  disables a cap.**
- A negative size is a configuration error (`config.limits.*`) and `brain
  doctor` reports it.
- Tiny positive values are honoured, for example `max_body_bytes: 64`. That is
  useful in tests and on an agent sandbox, not for day-to-day work; `brain
  doctor` warns with `limits-small` below 1024 body bytes, 16 title characters
  or 128 metadata bytes.
- Protected entries must be non-empty; project entries must be ids (no path
  separators), and globs must be valid `path.Match` patterns.

The table below shows which field each cap covers. `doctor` and error messages
name the record field, for example `decision.context`.

| Kind | Body (`max_body_bytes`) | Title (`max_title_chars`) | Metadata (`max_metadata_bytes`) |
|---|---|---|---|
| note | `body` | `title` | `tags` |
| decision | `context` | `title` | `alternatives`, `consequences`, `evidence` |
| attempt | `hypothesis`, `result`, `reason` (combined) | `title` | — |
| handoff | `summary`, `next_action`, `git_context` (combined) | — (a handoff has no title) | — |

`paths` on a decision is not part of the metadata budget; it is validated by
the existing path rules. Connector raw-zone items keep their own item limits.

## Protected projects and globs

Protection refuses `brain note`, `brain decision`, `brain attempt`, `brain
handoff`, `brain session` writes and `brain promote` into a matching project or
path. It also refuses updates the app makes itself (`brain record status`,
supersede links), because a rewrite carries the whole record.

- `protected_projects` compares the canonical project id, case-insensitively.
  The check runs before `--create` can register the project, so a refused write
  leaves no project directory behind.
- `protected_globs` match the record path relative to the data directory, for
  example `projects/global/notes/2026-11-15-decision-….md`. A glob protects
  everything below a directory it matches, so `projects/global/**` covers the
  whole project and `projects/templates` covers its tree. Windows paths are
  matched with forward slashes. A glob that equals a project id (`templates`)
  also protects that project, so the compact `templates/` form works.
- The documented example `global/AGENTS.md` is a hand-edited workspace file,
  not a record; it never passes through the record write path. Use
  `projects/global/**` to protect the records of a project named `global`.
- The MCP server never overrides protection. Only an explicit CLI flag does:
  `--allow-protected` on `note`, `decision`, `attempt`, `handoff` and `promote`.
  Use it for an owner-approved repair, not for routine agent work.

## Errors

A refused write returns a typed error and nothing is written: no record file,
no journal line and no index row.

- Size: `domain.LimitError` unwraps to the existing validation error and carries
  `Field`, `ConfigKey` (`limits.max_body_bytes`, `limits.max_title_chars` or
  `limits.max_metadata_bytes`), `Limit`, `Actual` and `Unit` (`bytes` or
  `chars`). The message reads
  `write refused: note.body is 200 bytes, over limits.max_body_bytes (64 bytes)`.
  Over MCP this is an `INVALID_ARGUMENTS` refusal with the same text.
- Protection: `domain.ProtectedError` unwraps to the existing forbidden error and
  carries `Kind` (`project` or `glob`), `ConfigKey`
  (`limits.protected_projects` or `limits.protected_globs`), `Pattern`,
  `ProjectID` and `Path`. The CLI exits with code 1; MCP reports a failed tool
  call whose text names the config key. An agent cannot widen the rule; the
  owner edits `brain.yaml` or passes the CLI override.

## Legacy records and `brain doctor`

Data written before a limit existed is never migrated or rewritten
automatically. `brain doctor` reports it:

- `limits` — effective values, `ok`, or `warn` with code `limits-small` for a
  limit below the compatibility floor.
- `record-limits` — `warn` with code `record-limits` when one or more stored
  records exceed the current caps. The message lists up to three records with
  the field, the configured limit and the actual size.

Both are report-only. `brain doctor --fix` registers git merge drivers; it never
edits a record body and never clears a `record-limits` finding. To clear one,
either shrink the record by hand or raise the limit in `brain.yaml`.

A legacy oversized record can still be read, searched, listed, trashed and
restored. An app-level update (`brain record status`, supersede) rewrites the
whole file and is refused until the record fits the current caps; the error
names the field so the owner knows what to shorten.

## Optional pre-commit verification

The data repository is plain git, so a commit made outside `brain` can bypass
the app boundary. To catch that before it lands, add a local hook that runs the
same report:

```sh
#!/bin/sh
# <data-dir>/.git/hooks/pre-commit
brain doctor --json >/dev/null 2>&1 || true
```

`brain doctor` exits 0 while findings are warnings, so this hook is advisory.
For a hard gate, fail on the finding directly:

```sh
#!/bin/sh
# <data-dir>/.git/hooks/pre-commit
if brain doctor --json | grep -q '"code": "record-limits"'; then
  echo "brain: committed records exceed limits; see brain doctor" >&2
  exit 1
fi
```

The existing managed hook (`brain hooks install`) is a post-commit journal hook,
not a pre-commit gate; the engine does not install the snippet above
automatically. On a workspace with legacy records the hard gate blocks every
commit until they are shrunk, so enable it only when `brain doctor` reports
`record-limits: ok`.
