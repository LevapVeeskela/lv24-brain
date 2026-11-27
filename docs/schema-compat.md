# Schema compatibility and engram fields

This page documents the additive record fields introduced for engram
compatibility, the topic-upsert rules, the scope values and the opt-in prompt
zone. The data schema stays **`schema_version: 1`**: every field below is
optional, omitted when it has its default value, and a file written by this
engine is still valid for an older one that ignores unknown keys.

See also: [architecture](architecture-deep.md), [security](security.md),
[runbook](runbook.md).

## Record fields

`Note`, `Decision`, `Attempt` and `Handoff` share the same optional
classification.

| Frontmatter key | Type | Default | Meaning |
|---|---|---|---|
| `subtype` | string, ≤120 characters, one line | empty | Free-form record subtype. The engram observation type maps here: `architecture`, `bugfix`, `discovery`, `session_summary`, `feature`, `policy`, ... |
| `scope` | `project` \| `personal` \| `global` | `project` | Where the record applies. The default is omitted from the file, so an old record is project-scoped. |
| `topic_key` | string, ≤120 characters, lowercase | empty | Stable identifier of an evolving topic. Empty means the record is append-only. Normalized: trimmed, lowercased, runs of spaces and `_` become `-`; letters, digits and `- . / :` are allowed. |
| `occurrences` | integer ≥1 | `1` | How many topic writes the record represents. Written only when greater than one and only changed by a topic upsert. |
| `source.external_id` | string | empty | External identity of an imported record (for example `engram:<sync_id>`). `source` already existed on notes, decisions and attempts; handoffs now carry it too. |

Note frontmatter is stored in the JSON marker line of the daily notes file;
decisions, attempts and handoffs store the same keys in YAML frontmatter. The
keys and their meaning are identical in both encodings, except that the note
marker keeps the existing camelCase spelling of `source` (`externalId`) that
notes have always used.

A record written before these fields existed has no keys, reads with the
documented defaults, and is never rewritten just because it was read.

## Topic upsert

A write with a topic key updates one record instead of appending a copy:

```bash
brain note --project lv24-brain --topic architecture/auth-model --body "..."
brain decision --project lv24 --topic architecture/storage --title "Use SQLite" ...
```

Rules:

1. The upsert form is a write with `--topic` and **no** explicit id and no
   `--supersedes`/`--superseded-by`. An explicit id or a supersede link is a
   normal create, so history is never rewritten silently.
2. Lookup is by `(project, kind, topic_key)`. The oldest matching record is the
   one updated; a legacy duplicate stays untouched and is reported by review
   and dedup like any other duplicate.
3. The stored current body is prepended to a `## History` section, newest
   first. History is bounded to **20 entries**; older entries are dropped.
   The current body is never history.
4. `occurrences` is incremented, `updated_at` moves to now, and `created_at`,
   the record id and provenance (`origin`, `reviewed`, `trust`, `source`) are
   preserved.
5. Validity, supersede links, typed relations and the decision status are
   preserved: an upsert changes text and classification only. Use
   `brain record status` to change a decision's lifecycle.
6. An identical body (ignoring surrounding whitespace) is a no-op: the stored
   record is returned and the CLI prints `(unchanged)`. Nothing is written.
7. Concurrent upserts of the same topic are serialized per
   `(project, kind, topic_key)` by the store lock, so two agents cannot both
   create the first record.

An upsert that finds no record creates one with `occurrences: 1` under the
same lock.

## Filters

`brain search`, `brain record list` and `brain context` accept:

| Flag | Values | Effect |
|---|---|---|
| `--subtype` | free text | keep records with exactly this subtype |
| `--scope` | `project`, `personal`, `global` | keep records with this effective scope |
| `--topic` | topic key | keep records with this normalized topic key |

Omitting a flag keeps the previous behaviour: no classification filter is
applied and every scope is searchable. The search index stores the fields as
derived columns (schema version 6) with indexes on `(project_id, subtype)`,
`(project_id, scope)` and `(project_id, kind, topic_key)`. Search ranking,
the context packer, review, dedup and relation logic are unchanged.

JSON search hits and record listings include `subtype`, `scope`, `topic_key`
and `occurrences` when set.

## Scopes

* `project` — the default; the record belongs to its project.
* `personal` — a record about the person, stored in the project it was written
  in.
* `global` — a rule that applies beyond one project.

Scope is a classification, not an access-control boundary. A scope filter
narrows a result set; it never grants or denies access to a project.

## Prompt zone (opt-in, local-only)

Prompts are the most sensitive text the tool can see, so they are disabled by
default.

```yaml
prompts:
  enabled: true
  retention_days: 30   # omit for 30; 0 keeps forever
```

```bash
brain prompt save --project lv24-brain --text "..." [--session <id>]
brain prompt list [--project lv24-brain] [--limit 20] [--json]
```

Privacy guarantees:

1. Prompts live in `projects/<id>/prompts/YYYY-MM-DD.md` with
   `visibility: local` in the frontmatter.
2. A `projects/<id>/prompts/.gitignore` containing `*` is written and
   repaired on every save, so `git add --all` cannot publish a prompt.
3. `CurrentRevision` skips the prompt directory: saving a prompt does not
   change the data revision and cannot make a sync commit it.
4. Prompts are not records: they are not listed by record commands, not
   indexed by the search index, and never returned by `brain search`,
   `brain context`, `brain bootstrap` or MCP tools.
5. `brain prompt save` and `brain prompt list` refuse with a validation error
   while `prompts.enabled` is false.
6. `brain doctor` reports whether the zone is enabled, that it is local-only,
   the retention window and how many prompts are older than it. Retention is a
   report: nothing is deleted automatically.

The prompt zone exists only in local and git mode. Server mode has no prompt
store, so the commands report that the feature is unavailable.
