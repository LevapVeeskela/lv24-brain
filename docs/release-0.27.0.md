# Release 0.27.0 — honest retrieval and index integrity

Status: release candidate on `main`.

0.27 lets retrieval say "nothing here" without presenting noise as an answer,
and lets the index say whether it still describes the data directory. Markdown
and data schema 1 are unchanged; the local search index moves to schema 10 and
rebuilds itself once. Search and context responses are additive: existing keys
keep their meaning.

## Support gate: supported, related, none

A search page now carries a support verdict next to the hits.

| Tier | Meaning |
|---|---|
| `supported` | at least one hit contains every rare query term and enough of the rest |
| `related` | the page overlaps the topic but no hit answers it; a reader can see it is nearby |
| `none` | nothing answered the query: `hits` is empty and `support.reason` says why |

The rule uses document frequency inside the project. A query term is **rare**
when at most 20 % of the project's documents contain it (with a floor of one
document, so a term that occurs once is still the term the query is about). A
hit is supported when it contains **every** rare term and at least **50 %** of
the remaining common subject terms, rounded up. Question and function words are
stop words and are never required.

Specifics that matter when reading the output:

- The strict all-terms pass always qualifies: it already required every term.
  An OR-only hit must pass the gate on its own.
- A page with no supported hit stays visible as `related` only when the page
  covers a rare term and at least one hit covers two query terms. Below that it
  is not nearby text either, so the hits move to `excluded[]` with the
  `lower-rank` reason and the tier is `none`.
- An explicit `--relaxed`, or `match_mode: any` over MCP, is the caller asking
  for partial matches. The page is then `related` with the reason `partial
  matching was requested explicitly`; the hits stay in `hits` and are not
  hidden.
- Server mode keeps its Postgres ranking and does not evaluate support: `tier`
  is absent from its hits, and the `support` block is present but empty
  (`tier: ""`, zero `best` and `threshold`).
- The gate reads the record text, not the preview, and uses the same prefix and
  stem matching as the index. It never changes the score or the order.

The CLI reports the verdict on stderr (`brain search: related only — …`,
`brain search: no supported matches — …`), so a script that reads stdout is not
broken by a new line. JSON carries `support {tier, best, threshold, reason}` and
`hits[].tier`; `best` is the largest share of query terms any matched record
contained and `threshold` is the 0.5 subject share.

## Content-hash duplicate collapse

The index now stores a `content_hash`: lowercase kind, title and body, with
whitespace collapsed, hashed as `kind\ntitle\nbody`. The record id, project and
timestamps are not part of it, so the same record written on two clones hashes
the same.

Within a page, search keeps one row per hash inside one project. The **oldest**
record wins (creation time, then id), takes the best rank its group reached, and
every other copy is listed once in `excluded[]` with reason `duplicate`. Nothing
is deleted: the records stay in Markdown, in lists and in counts. Project scope
is part of the key because the same sentence can legitimately appear in two
projects.

## Stub records stay out of retrieval, not out of the brain

A record is a **stub** when its title is only a generated label
(`<kind>-<date>-<slug>-<id>`) or its whole body is one of `nothing new`,
`session ended`, `no changes`, `no updates` (case-insensitive, surrounding
punctuation ignored). The flag is derived at projection time and never written
to Markdown; lists and counts still include the record.

- `brain search` hides stubs by default; `--include-stubs` brings them back.
  MCP `brain_search` has no switch and always hides them.
- `brain context` uses the same search path, so a stub cannot consume budget.
- `brain review` skips stubs in its supersession, contradiction and
  near-duplicate checks.
- Neither surface has an override: `--include-stubs` is search-only, and MCP
  `brain_search` has no switch at all.
- The flag is reversible: changing the file changes the projection on the next
  write or reindex. No stub is deleted or rewritten by this release.

## Context: every drop has a reason, near misses keep their score

`brain context` / `brain_context` no longer leaves a silent gap. Every record
that did not make the answer is listed in `excluded[]` with a stable code:
`over-budget`, `lower-rank`, `superseded`, `stale`, `quarantined`,
`other-owner`, or the `other` fallback for an old truncation. The Markdown
summary prints the count and the distinct reasons per layer:

```text
- … 7 more dropped (over-budget, lower-rank)
```

The query layer additionally keeps up to three **near misses**: the records
that matched the task but ranked below the budget cut. They appear in
`near_misses[]` with `id`, `kind`, `reason` (`over-budget` or `lower-rank`) and
the positive score at the moment they were dropped, and the summary adds
`- … N lower-ranked omitted`. Near misses are JSON-only: the text answer prints
the count, not the ids. `excluded[]` is capped at 50 entries in a transport
answer while `excluded_total` keeps the full count.

## Index schema 10 and an index that admits when it is stale

Schema 10 adds the derived projection columns and a `meta` table:

- `documents.content_hash` and `documents.stub`, with an index on
  `(project_id, content_hash)`;
- `meta` keys `index_generation` (an order-independent digest of the indexed
  corpus), `indexed_from` (the data directory's short HEAD, with a `+dirty`
  suffix when the tree was dirty) and `schema_version`.

A version mismatch drops and recreates the derived tables in one transaction and
sets the reindex flag, so an old index rebuilds once. The generation is written
only after a completed reindex: an empty generation means no completed reindex
has been recorded yet, and a changed generation means the content changed.

`brain open` and `brain status` print the index state:

```text
index: complete at 1a2b3c4
index: stale at 1a2b3c4 (HEAD 9f8e7d6)
index: stale (never indexed)
```

`stale` is reported when the generation is missing, or when `indexed_from`
differs from the current HEAD or dirty state. There is no separate
`brain index --status` flag: the line lives in `brain open` and `brain status`,
and the same values feed `brain doctor`. A one-commit lag is normal (the next
write or `brain index` catches up); the new `index-generation` check warns only
when the data directory is dirty or the index is more than one commit behind,
and it never fails.

## Status tiers and the wrong-checkout hint

`brain open` and `brain status` now start with the resolved project and its tier;
`brain status` reports a directory with no resolvable project as `ABSENT`:

| Tier | Meaning |
|---|---|
| `ABSENT` | no project is resolved for the directory: no records, no state |
| `EMPTY` | the project is registered but has no records, no state content and no handoff |
| `POPULATED` | at least one record, or state content, or a handoff exists |

When the working directory is outside the resolved checkout, the output names
both sides instead of silently showing another project's context:

```text
cwd: C:\work\other (not inside the resolved checkout)
registered_paths: C:\work\demo
```

`brain status` reports `tier: ABSENT` when no project can be resolved for the
directory (there is no registered checkout and no single-project fallback); it
is a diagnostic and still exits 0. The tier is computed from stored records, not
from the search index, so a stale index cannot fake an empty project.
`brain status --json` keeps its configuration shape and does not carry the tier
line; the tier and index line are text surfaces.

## Write-time similar hint

After `brain note`, `brain decision` and `brain attempt`, the engine searches
for up to three existing records of the same project that resemble the new one.

- MCP returns `similar[]` with `{id, title, score, kind}` in
  `structuredContent`. It is advice only: the write is unchanged.
- The CLI prints the block only with `--similar`:
  `similar: <id> <score> <title>` per record, plus
  `hint: pass --supersedes <id> if this replaces <id>` when the top hit is a
  record of the same kind. No automatic supersede happens.
- The new record is excluded from its own list, and a search failure after a
  successful write is not an error: the list is simply empty.

## Upgrade

The data schema and Markdown format are unchanged, so the upgrade is a binary
update plus one index rebuild.

```sh
brain update --yes
brain doctor
brain index
brain open .
brain search "release" --json
```

The first command that opens the index rebuilds it once automatically; running
`brain index` up front makes the rebuild explicit and lets `index: complete at
<sha>` be checked immediately. `brain doctor` should report the
`index-generation` check as `ok` after that. Mixed versions are safe: Markdown
is unchanged, so a pre-0.27 binary can keep writing. That machine keeps its old
index and does not collapse duplicates or hide stubs until it is upgraded and
reindexed; update it when you want the same retrieval there.

## Compatibility

- Search JSON is additive: `support`, `hits[].tier` and `excluded[]` are new;
  `relaxed`, `page`, `page_size`, `has_more`, `next_cursor` keep their meaning.
- Context JSON is additive: `excluded`, `excluded_total` and `near_misses`.
- Text output of `brain search` is unchanged; the support verdict is on stderr.
  `brain context` adds the reason codes to the existing dropped line.
- `brain status --json` and `brain open --json` are unchanged.
- The Local API (`localapi-v1`) is unchanged; the new search and context fields
  are on the CLI and MCP surfaces.
- The support gate, duplicate collapse and stub filtering are local/git only.
  Server mode has no local index, no `tier` and no stub flag.
- An older binary that opens a schema-10 index recreates the derived index at
  its own version; alternating versions on one machine reindexes each time.
  Markdown and the data schema are untouched.
- No record is deleted or rewritten. Duplicates are collapsed in the response,
  stubs are hidden from retrieval, and both stay recoverable.
