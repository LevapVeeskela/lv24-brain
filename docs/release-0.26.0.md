# Release 0.26.0 — ranking, budgets, time and identity

Status: release candidate on `main`.

0.26 completes the items 0.25 deferred. Search ranks a normalized relevance
instead of raw BM25, the context packer spends a proportional budget, `--since`
compares text changes, and connector accounts move out of the synced catalogue.
The data schema stays 1; the local search index moves to version 8 and rebuilds
itself once.

## Ranking v2

- Every signal is normalized to `[0, 1]` before it is weighted, so the weights
  are shares and the score is a plain weighted sum in `[0, 1]`. The old
  `total / (total + 1)` map is gone; `--min-score` now filters exactly the score
  the caller sees.
- Lexical relevance is normalized **per query**. With three or more matches the
  scale is min-max over the matched set: the weakest match scores 0, the
  strongest 1.0, every match keeps its relative position. With one or two
  matches there is no distribution to scale, so relevance is top-anchored (the
  best match scores 1.0, a second keeps its ratio to it). A flat set scores 1.0
  instead of dividing by zero.
- Weights: `bm25` 0.55, `recency` 0.20, `pin` 0.15, `importance` 0.05, `trust`
  0.05. `pin` grew from 0.05 to 0.15 and `importance` shrank from 0.15 to 0.05:
  an explicit pin or an accepted decision says more about what to read first
  than the record kind does.
- Trust is an **additive** term, never a filter. Its 0.05 share is reserved for
  the provenance levels (verified 1.0, stated 0.6, inferred 0.3). It shares the
  `importance` column of `--explain` (the API keeps five slots), so the five
  contributions still add up to the score. The index projection does not store
  provenance yet, so the term is neutral today and auto-captured records are not
  buried.
- The `[[id]]` link bonus is retired; its 0.05 share went to trust. `c_link`
  stays in the payload as `0.0` so the response shape does not change.
- Ordering of the strict pass is `score DESC, bm DESC, updated_at DESC, id ASC`.
  The extra `bm` tie-break keeps records with the same composite score in
  lexical order. A merged page orders each group by score, then id, so strict
  hits stay above OR-only hits.

## Strict hits above the OR fallback

- A strict AND pass that finds fewer than **five** hits now retries the same
  terms joined with OR, and the two pages are **merged** instead of replaced.
- Strict hits always hold the top of the page; records only the relaxed pass
  found follow. The two passes normalize relevance over different match sets, so
  a raw score from the OR page can never outrank a record that matched every
  term.
- Records are deduped by project and id; a hit in both pages keeps its strict
  position and score. `relaxed: true` is reported when the OR page added
  anything. An explicit `--match-mode any` does not run the fallback.

## Context budget v2

- Layer caps are **shares of the total budget**, not fixed token numbers:
  `core` 25 %, `query` 35 %, `decisions` 20 %, `attempts` 12 %, `linked` 8 %.
  The same packer behaves for a 500-token and a 32 000-token answer.
- Caps are **fungible**: a layer may borrow the part of an earlier share that
  was left unused, and a layer that overspends pays the pool back (never below
  zero).
- The packer order follows priority: state and handoff (`core`), then the
  records that match the task (`query`), then the curated background
  (`decisions`, `attempts`) and the 1-hop neighbours (`linked`).
- The eight-record ceiling on relevant records is gone. The budget decides how
  many records fit, so a large budget returns every matching record.
- Every layer that left records out ends with `- … N more dropped`; a layer
  that packed nothing still prints its heading and that line when the budget
  dropped its candidates. `--explain` prints `layer: used/cap tokens, N
  records` and `total: used/budget tokens, D dropped`.
- The rendered answer is measured after packing: if it would still exceed the
  budget (marker lines, an empty-section placeholder), the lowest-priority
  packed records are dropped until it fits. The budget is a hard limit.
- MCP and `brain context --json` cap `truncated[]` at **20** entries and add
  `total_truncated`, so a tight budget on a large project reports an honest
  count instead of a truncated list pretending to be complete.
- Token estimates are script-aware: Latin text packs about four characters per
  token, Cyrillic, Greek, Hebrew, Arabic, CJK, Hangul, Thai and Devanagari about
  two. A pure-Latin text keeps the historical estimate. Truncation finds the
  longest fitting prefix by binary search instead of a fixed runes-per-token
  factor, and the marker is now part of the budget.

## `content_updated_at` and temporal reads

- Notes, decisions, attempts and handoffs carry `content_updated_at` in
  frontmatter (JSON `contentUpdatedAt`). It moves only when the title, body or
  tags change (the handoff summary, next action or git context).
- Review, snooze, validity changes, supersede links and merge bookkeeping leave
  the content time alone: the file is rewritten (and `updated_at` moves with
  validity and supersede writes), but the text did not change.
- `--since` compares the content time on every surface: `brain search`,
  `brain context`, `brain record list` and the latest-handoff selection. A
  review pass no longer re-reports the whole corpus. Files written before the
  field existed fall back to `updated_at`, the conservative answer.
- The local index schema is **8** (a `content_updated_at` column). A mismatch
  recreates the index and marks it dirty; the next search reindexes once from
  Markdown, so no manual step is required.
- `--as-of` selects records and the handoff valid at that instant, and the
  answer labels the limit of that: state and review findings have no history.
  `brain context` prints `state_as_of: current (state and review findings
  describe the current data)` and JSON returns `state_as_of: "current"`. A
  past-instant answer never pretends the state was historical.

## Connector identity migration

- `brain connection migrate-identity [--check|--apply] [--json]` moves raw
  account labels and connector-declared identity settings (currently the IMAP
  `username`) from `connectors/connections.json` into the secret store as
  `conn/<id>/account` and `conn/<id>/<field>`. The catalogue keeps only a
  marker: `{"$keychain":"account","fingerprint":"<16 hex>"}`.
- The fingerprint is a truncated SHA-256 of the trimmed, lower-cased value. It
  keeps `Connection.Identity()` unique per account without publishing the
  address, is identical on every machine, and makes a legacy raw value compare
  equal to its migrated form — so `add` and the legacy importer cannot create a
  duplicate mailbox across the migration boundary.
- The default is a dry run; `--check` and `--apply` cannot be combined. Apply
  writes the secret, reads it back, and only then rewrites the catalogue, so a
  failed step never leaves a marker without a secret. Re-running is idempotent.
  Statuses: `would-move`, `moved`, `already-migrated`, `missing-secret`,
  `unreadable-secret`, `mismatched-secret`.
- Identity is resolved **lazily**, only when a connector, an explicit check or
  the list view needs it. A locked or missing keychain never blocks
  `connection list`, `add` or `migrate`; `list` and `migrate` print the
  placeholder `<keychain>` and one warning instead of failing.
- `brain connection add` now protects an identity at creation, and
  `connection secret set <id> account` repairs one. A repair value must match
  the stored marker fingerprint; changing the account is a migration, not a
  secret set.
- A marker without a usable secret fails the run with a message naming the
  credential key and the repair command; `connection list` reports the same
  problem as a warning.

### Fleet order

Update **every machine to 0.26.0 before applying the migration anywhere**. A
pre-0.26 binary has no marker concept: it reads `{"$keychain":…}` as a literal
account string, so its identity comparison, `connection list` display and any
new `connection add` would be wrong, and a second machine still on 0.25 could
import the legacy account back as a duplicate. Migrate after the whole fleet
reports 0.26.0:

```sh
brain --version                  # on every machine: 0.26.0
brain connection migrate-identity --check
brain connection migrate-identity --apply
brain sync --push
```

The raw value is a valid fallback forever, so a machine that has not migrated
keeps working; only the catalogue grows a marker while another machine still
holds the secret. That asymmetry is why the migration is opt-in and ordered.

## Diagnostics gating

- Release notes are a series, not a supersession chain. Two titles that name
  different dotted versions are never a supersession gap and never a
  near-duplicate: `Release notes 0.6.0` and `0.6.0 shipped` can meet, `0.25.0`
  and `0.26.0` cannot.
- Supersession-gap detection now compares the significant title words after
  dropping release boilerplate (`release`, `notes`, `changelog`, `version`,
  `shipped`, `published`, `announcement`, `draft`, `final`, `news`) and requires
  90 % coverage of the shorter title, so a shared template is not a lineage.
- The near-duplicate gate requires body overlap of at least 0.5 in addition to
  the existing shingle/simhash checks: titles and weekly-report templates
  collide too easily on their own.
- `decision-contradicted` requires an attempt that **records** a failed result.
  An attempt with a neutral or empty result no longer contradicts a later
  decision; failure words and phrases (including Russian forms) are matched in
  the result, falling back to the reason.

## Server review and provenance

- `POST /v1/projects/{project}/records/{kind}/{id}/review` and the project-less
  `POST /v1/records/{kind}/{id}/review` update `origin`, `reviewed` and `trust`
  without rewriting the body.
- The body carries the `revision` the caller read; zero means the revision this
  request read, and the write is still a compare-and-swap, never
  last-write-wins. `reviewed: false` is refused, a stale revision is `409`, a
  missing record is `404`, an unknown origin is `400` until the caller passes a
  valid one, and a record without a revision is `409`. The write bumps the
  revision and records an audit event.
- The HTTP client implements `ports.RecordReviewer`, so `brain record review`
  works against a server. `--review-after`/`--clear-review-after` remain
  local/git only (`project_state` has no provenance columns yet, so a server
  state write stores no provenance).
- Server search filters quarantined records in SQL before ranking and `LIMIT`,
  and the project listings drop them unless `include_unreviewed=true`. Reading
  one record by id remains an explicit request and still returns it. Server
  search has no opt-in for quarantined hits yet.

## Local API additions

The frozen `localapi-v1` contract grows the routes Desktop 1.7.0 drives and the
two-phase merge routes ([localapi-v1.md](localapi-v1.md)):

- `POST /v1/merge/plan`, `POST /v1/merge/apply`, `POST /v1/merge/rollback` —
  two-phase merge with a fingerprint guard; drift is `409 merge_stale` and is
  never overwritten.
- `POST /v1/digest` — dry run by default, `{"write":true}` saves
  `digests/YYYY-MM-DD.md`.
- `POST /v1/projects/adopt` and `POST /v1/projects/{id}/paths` — stamp path
  ownership and register a checkout path; re-adding a path is a no-op.
- `GET /v1/journal` and `GET /v1/hooks` — the project journal page and one
  `HookStatus` per agent host (`claude`, `codex`, `opencode`).

## Privacy

- GitHub is now marked `personalData` like GitLab, IMAP, Slack, Notion and MCP:
  commit authors and issue bodies routinely carry e-mail addresses, so the
  ingest policy replaces every address with `[email:<12 hex>]` in every text
  field it stores, for local-only and shared connections alike. The placeholder
  is stable per connection (the same sender stays searchable) and not joinable
  across connections.
- `scripts/scrub-raw-pii.py` is a one-off, reproducible scrub of e-mail
  addresses already stored in raw zones (`items`, `runs`, legacy `mail/`,
  `github/`, `gitlab/` dumps). It writes the same placeholder the ingest policy
  writes, is idempotent (existing `[email:…]` tokens are left untouched),
  rewrites only matched bytes, and recomputes the item content hash with the
  engine's byte-exact encoding, so a scrubbed item looks exactly like a fresh
  ingest. A dry run prints per-zone counts and up to three example paths; only
  `--apply` writes. `go run scripts/scrub-raw-pii-hash-check.go <data-dir>`
  verifies every item hash afterwards with the engine's own code.

```sh
python scripts/scrub-raw-pii.py <data-dir>                    # dry run
python scripts/scrub-raw-pii.py <data-dir> --apply
go run scripts/scrub-raw-pii-hash-check.go <data-dir>
```

## Desktop 1.7.0

Desktop **1.7.0** bundles engine **0.26.0**. It adds the morning digest with a
dry-run preview before writing, project path adoption and add-path, agent hook
status, and a journal panel on the activity page. See the docs site for the
screen-by-screen description.

## Upgrade

1. **Update every machine first** — `brain update --yes` or rebuild and replace
   the binary; confirm `brain --version` on each. Do not run the identity
   migration until the whole fleet is on 0.26.0.
2. `brain doctor` — expect no new failures. `brain connection list` warns about
   an identity marker whose secret is missing or unreadable, with the repair
   command.
3. `brain index` — the first search after the upgrade reindexes once for schema
   8; an explicit `brain index` makes it visible.
4. `brain connection migrate-identity --check`, then `--apply`, then
   `brain sync --push`.
5. Optional: `python scripts/scrub-raw-pii.py <data-dir>` then `--apply` if the
   raw zones predate e-mail hashing. Commit the result with the usual sync.

A saved `--min-score` threshold is not comparable across the upgrade: 0.25
divided a weighted sum by `total + 1`, 0.26 reports the weighted sum itself, so
re-check the value against a known query. The same applies to a checked-in
`brain eval` baseline: regenerate it with `--write-baseline` after reviewing the
report.

The data schema stays 1, records remain readable in both directions, and no
manual migration is required beyond the fleet order for identity.
