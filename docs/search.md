# Search and progressive disclosure

`brain search` answers with a ranked page of previews. `brain get` and
`brain timeline` are the second and third steps: read one record, see what came
before and after it. The three commands share one rule: never spend tokens on
text the model did not ask for.

This page documents search v2 (WB-1.2), the 0.27 support gate and progressive
disclosure (WB-1.4). Server mode keeps its own Postgres ranking, so it omits
`match_reasons`, does not evaluate support, and refuses the local ranking
controls (`--match-mode`, `--relaxed`, `--min-score`, `--explain`, `--full`)
instead of silently ignoring them. Previews, pagination, `brain get` and
`brain timeline` work in both modes.

## Composite score

Every signal is normalized to `[0, 1]` and weighted, so the score is a plain
weighted sum in `[0, 1]`:

```text
score = 0.55 * bm25_norm
      + 0.20 * recency
      + 0.15 * pin
      + 0.05 * importance(kind)
      + 0.05 * trust
```

The weights are shares: a signal's contribution is exactly its share of the
score, and `--min-score` compares the number the caller sees. There is no
`total / (total + 1)` remap any more.

| Signal | Weight | Definition |
| --- | --- | --- |
| `bm25_norm` | 0.55 | FTS5 BM25 with field weights `title=3.0`, `body=1.0`, sign flipped and clamped at 0, then normalized **per query**: min-max over the matched set with three or more matches, top-anchored with one or two, `1.0` for a flat set. |
| `recency` | 0.20 | `0.5 ^ (age_days / 30)`. A record 30 days old scores half of one written now; a future timestamp counts as now. |
| `pin` | 0.15 | `max(pinned, accepted decision, author confidence)`: an explicitly pinned record or an accepted decision scores 1.0; otherwise the record's `confidence` in `[0, 1]`; otherwise 0. |
| `importance` | 0.05 | `decision=1.0`, `attempt=0.8`, `handoff=0.7`, `note=0.5`, anything else (`state`, `dump`, `item`) `=0.3`. |
| `trust` | 0.05 | Provenance trust: verified 1.0, stated 0.6, inferred 0.3, additive and never a filter. The index projection stores `origin`/`trust` (schema 9), so the term reorders records; an older index row without provenance scores neutral (1.0). |

Relevance used to be measured on the raw BM25 scale, which is negative, corpus
dependent and unbounded. Normalizing per query fixes that: a change to the corpus
no longer changes what the 0.55 weight means, and a strong match still outranks a
merely fresh one. Min-max can only scale the matched set, so a single match is
never penalized for being alone.

Ties in the strict pass break on lexical strength (`bm`), then `updated_at DESC`,
then `id ASC`. A page merged from the strict and OR passes orders each group by
score then id, so strict hits stay above OR-only hits. The order is total and
deterministic, which is what makes paging safe.

The `[[id]]` link bonus is retired: its 0.05 share moved to trust. The `c_link`
column stays in the API shape as `0.0` and never adds a `match_reasons` entry.

The weights live in one place in the index adapter (`rank.go`) as named
constants. There is no configuration surface yet: a weight is changed in code,
reviewed, and measured with `brain eval` (see `docs/eval.md`). The ranking was
tuned against the eval baseline: recall, precision and MRR may not regress, so a
weight change that "feels" better but loses a labelled hit fails the gate.

### `match_reasons` and `--explain`

Each hit carries `match_reasons`: only the signals that fired.

| Reason | Fires when |
| --- | --- |
| `bm25` | the document matched at least one query term (always for a hit). |
| `recency` | the record is younger than one half-life (30 days). |
| `importance` | the kind's importance is `>= 0.7` (decision, attempt). |
| `pin` | the record is explicitly pinned, an accepted decision, or has a stated confidence. |

`--explain` additionally returns the weighted contribution of every signal.
Contributions are the weighted terms themselves, so they add up to the score
exactly; each contribution's share of the score is the signal's weight times its
strength. An unexplained ranking position can therefore be read off the numbers
instead of guessed. Reasons and contributions describe the score, they never
adjust it.

## Support tiers

A ranked page is not automatically an answer. Every search response carries a
support verdict, and every hit carries its own tier:

| Tier | Meaning |
| --- | --- |
| `supported` | at least one hit contains every rare query term and enough of the rest. |
| `related` | the page overlaps the topic, but no hit answers it. The records stay visible so the caller can see they are nearby. |
| `none` | nothing answered the query. `hits` is empty and `support.reason` says why; the matched records move to `excluded[]` with reason `lower-rank`. |

The gate is computed from document frequency inside the project. Subject terms
are the query terms after stop words are removed.

1. A term is **rare** when at most 20 % of the project's documents contain it.
   The limit is floored at one document, so a term that occurs once is still
   treated as the term the query is about.
2. A hit is **supported** when it contains every rare term and at least 50 % of
   the remaining common terms, rounded up (`commonNeeded = ceil(0.5 * common)`).
3. A hit from the strict all-terms pass always qualifies: the pass already
   required every term. An OR-only hit must pass the test on its own.
4. If any hit passes, the tier is `supported`; passing hits carry `supported`,
   the others `related`.
5. If no hit passes, the page is still `related` when the page covers a rare
   term and at least one hit covers two query terms. One shared word is a
   coincidence, not overlap.
6. Below that, the tier is `none`, `hits` is emptied, and every matched record is
   reported in `excluded[]` as `lower-rank`. The reason names the rule, for
   example: `no record contains every rare query term (document frequency <=
   20%) and at least 50% of the remaining subject terms`.

A query with no subject terms after stop words cannot be supported: an empty
page is `none` (`the query has no searchable subject terms`), and a non-empty
page is `related` (`the query has no subject terms; the page is a lexical match
only`).

An explicit `--relaxed`, or `match_mode: any` over MCP, tells the engine the
caller accepts partial matches. The page is then `related` with the reason
`partial matching was requested explicitly`, even when a hit matched every
term: the caller asked for recall, and the tier says the page is nearby rather
than an answer. The hits stay in `hits`.

The gate never changes the score or the order. It reads the whole record text,
not the preview, and mirrors the index's prefix and stem matching, so `signed`
covers `sign` and `releases` covers `release`. When the index cannot report
document frequency, `support.reason` says support was not evaluated and the
hits keep no tier. Server mode has its own Postgres ranking and does not
evaluate support: `hits[].tier` is absent, and `support` is present but empty
(`tier: ""` with zero `best` and `threshold`).

```json
{
  "support": {"tier": "related", "best": 0.66, "threshold": 0.5,
              "reason": "the page only matched the relaxed pass; no record contains every rare query term"},
  "hits": [{"id": "note-2026-11-18-cache", "tier": "related"}],
  "excluded": [{"id": "note-2026-11-17-cache", "reason": "duplicate",
                "kind": "note", "title": "Cache warmup", "score": 0.71}]
}
```

`support.best` is the largest share of query terms any matched record contained;
`support.threshold` is the 0.5 subject share a supported hit needs. In text mode
the verdict is on stderr (`brain search: related only — …` or
`brain search: no supported matches — …`), and withheld records are summarized
as `brain search: excluded N (duplicate 1, lower-rank 2)`.

## Duplicates and stubs

Two projection rules keep the page honest without touching Markdown.

**Duplicates.** The index stores a `content_hash` of the normalized kind, title
and body (lowercase, whitespace collapsed). Within a page, search keeps one row
per hash inside one project: the **oldest** record wins (creation time, then id),
takes the best rank its group reached, and every other copy is listed once in
`excluded[]` with reason `duplicate`. The same record written on two machines is
one answer; the same sentence in two different projects is not a duplicate.
Nothing is deleted or merged.

**Stubs.** A record is a stub when its title is only a generated label
(`<kind>-<date>-<slug>-<id>`) or its whole body is `nothing new`,
`session ended`, `no changes` or `no updates` (case-insensitive, surrounding
punctuation ignored). Stubs stay in the index and in counts, but search hides
them by default, so a session-closing line cannot outrank a real record.
`--include-stubs` brings them back. Over MCP, `brain_search` has no switch and
always hides them; `brain context` has no override either, and review's
supersession, contradiction and near-duplicate checks skip stubs. The flag is
derived, never stored in Markdown, and reverses itself when the file changes.

## Flags

| Flag | Effect |
| --- | --- |
| `--match-mode all` | every term must match (AND). The OR fallback is **off** unless `--relaxed` is also given. |
| `--match-mode any` | any term may match (OR); no fallback, and the page is reported as `related`. |
| `--relaxed` | allow the OR fallback when the strict pass finds fewer than five hits; the pages are merged and the response reports `relaxed: true`. It also marks the page `related` (the caller accepts partial matches). |
| `--include-stubs` | keep low-signal stub records that search hides by default. |
| `--min-score <0..1>` | drop hits below the composite score. |
| `--explain` | add per-signal contributions (JSON `explain`, text `score`/`reasons` lines). |
| `--full` | return each hit's whole body alongside its preview (JSON `body`). |
| `--page-size <n>` | hits per page; defaults to `--limit`. |
| `--cursor <cursor>` | next page of the same query. |
| `--json` | envelope with `relaxed`, `page`, `page_size`, `has_more`, `next_cursor`, `hits`, `support`, `excluded`. |

Without `--match-mode`, search runs a strict AND pass first, then the same terms
joined with OR when the strict pass found fewer than five hits. The two pages are
merged: strict hits hold the top, OR-only hits follow, duplicates are dropped by
project and id, and the response reports `relaxed: true` (plus a stderr line in
text mode) when the OR page added anything. The passes normalize relevance over
different match sets, so an OR-only score never outranks a strict hit.

`--match-mode all` makes the mode explicit: only the strict pass runs, unless
`--relaxed` is also given. `--match-mode any` sends one OR pass. In both explicit
modes the support gate still runs: a strict page can be `supported`, an OR page
is `related`, and `none` says the corpus does not answer the query.

A cursor is an opaque, query-bound offset. Replaying it on another query is a
usage error (exit 2). Paging is bounded by the 200-result index window, the same
window `brain_search` uses over MCP; both surfaces use `page`, `page_size`,
`has_more` and `next_cursor`, so an agent that learned one can use the other.

```sh
brain search --json --match-mode all --min-score 0.45 --page-size 5 "merge driver"
# data.next_cursor = "eyJ2IjoxLCJ0b29sIjoi..."
brain search --json --page-size 5 --cursor "eyJ2IjoxLCJ0b29sIjoi..." "merge driver"
```

## Progressive workflow

```sh
brain search "supersede chain" --json        # id, kind, title, preview, score, reasons
brain get decision-2026-11-14-supersede --max-tokens 400
brain timeline decision-2026-11-14-supersede --window 3
```

1. **Search** returns at most `PreviewRunes` (160) characters per hit. Enough to
   judge relevance, far too little to answer from.
2. **Get** returns one record's text under a token budget, with a truncation
   marker and `truncated: true` in JSON. `--max-tokens 0` returns the whole
   record; the default is 600.
3. **Timeline** returns up to `--window` records (default 3) of the same project
   and kind on each side of the anchor, oldest first, with `>` marking the
   anchor. It is a historical view: records whose validity ended are included,
   so a superseded decision still shows where it sat.

Over MCP, `brain_search` follows the same rule:

| Argument | Effect |
| --- | --- |
| `compact` (default `true`) | hits carry ids, kind, title, preview, score and reasons; no bodies. |
| `expand_ids: [...]` | return those records in full in the same response; ids must be on the page of the same call. |
| `expand: true` | expand every hit on the page. |
| `compact: false` | same as `expand: true`, kept for parity with the CLI wording. |
| `max_tokens` | budget per expanded record; default 400, maximum 4000. |

At most 20 records are expanded per call. A host that asks for more is refused
with a hint, because that is a context-packing decision, not a search decision.

## Token budget

| Budget | Default | Bound |
| --- | --- | --- |
| search preview per hit | 160 runes | fixed by `PreviewRunes` |
| `brain get --max-tokens` | 600 tokens | `0` = unlimited |
| MCP expanded record | 400 tokens | `1..4000` |

Tokens are estimated from the script of each rune: Latin text packs about four
characters per token, Cyrillic, Greek, CJK and the other dense scripts about
two. The estimate is deterministic and dependency-free; it is a limit to
respect, not a billing figure. `brain eval` reports `tokens_per_answer` for the
retrieval answer, so a change to previews or expansion shows up as a tokens
delta next to recall.

`--since` keeps documents whose **content** changed in the window. The
frontmatter carries `content_updated_at`, which moves only on a title or body
change: a review, a snooze, a validity change or a merge bookkeeping write
rewrites the file without making the record recent. `brain search`,
`brain context` and `brain record list` share the rule, so one window returns
the same records on every surface.

## Real-corpus retrieval gate

The authored fixtures in `eval/fixtures` are deterministic and small. A second
gate scores 30 hand-written queries against a live data directory through the
same search use case as `brain search`:

```sh
BRAIN_DATA_DIR=/path/to/lv24-brain-data go test -tags realcorpus ./eval \
    -run TestRealQueries -count=1
```

The Markdown store is read-only and the index is built in a temporary
directory, so the data directory is never modified. The test prints top-1,
top-5, MRR and the queries that missed the top hit (add `-v`, because `go test`
hides passing-test output), and fails only when top-5 drops below 0.80. The
labels live in `eval/realqueries.json`; each query names one record a human
would pick, so the fixture can be reviewed without the live corpus.
`docs/eval.md` covers the authored fixtures and `brain eval`.

## Compatibility

- Every pre-0.16 flag keeps its meaning: `--limit`, `--kind`, `--dumps`,
  `--since`, `--as-of`, `--include-superseded`, `--all-time`, `--json`.
- `brain search` text output is unchanged: `kind/project/title` and an indented
  preview; `--explain` and `--full` add lines, they never replace the first two.
- JSON is additive: `relaxed`, `page`, `page_size`, `has_more`, `next_cursor`,
  `match_reasons`, `explain`, `body`; 0.27 adds `support`
  (`tier`, `best`, `threshold`, `reason`), `hits[].tier` and `excluded[]`
  (`id`, `kind`, `title`, `score`, `reason`; MCP also carries `project_id`).
- `score` changed meaning in 0.16 from the raw BM25 rank to the composite score
  in `[0, 1]`.
- `brain get` prints the text fields of a record; `brain record show` keeps
  printing every structured field, links and history. Use `get` when tokens
  matter, `record show` when structure matters.
- An index built before 0.16 is rebuilt automatically on the next search: the
  ranking inputs (decision status, author confidence) are projection data, and
  Markdown stays the source of truth. The 0.27 index (schema 10) rebuilds once
  the same way; see `docs/release-0.27.0.md`.
