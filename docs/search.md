# Search and progressive disclosure

`brain search` answers with a ranked page of previews. `brain get` and
`brain timeline` are the second and third steps: read one record, see what came
before and after it. The three commands share one rule: never spend tokens on
text the model did not ask for.

This page documents search v2 (WB-1.2) and progressive disclosure (WB-1.4).
Server mode keeps its own Postgres ranking, so it omits `match_reasons` and
refuses the local ranking controls (`--match-mode`, `--relaxed`, `--min-score`,
`--explain`, `--full`) instead of silently ignoring them. Previews, pagination,
`brain get` and `brain timeline` work in both modes.

## Composite score

Every hit gets a weighted sum over five signals, and the reported score maps
that sum into `[0, 1)`:

```
total = 0.55 * bm25
      + 0.20 * recency
      + 0.15 * importance(kind)
      + 0.05 * pin
      + 0.05 * link

score = total / (total + 1)
```

The map is strictly increasing, so it never changes the order: it only makes
`--min-score` comparable across queries (`score 0.5` is `total = 1`, `0.75` is
`total = 3`).

| Signal | Weight | Definition |
| --- | --- | --- |
| `bm25` | 0.55 | FTS5 BM25 with field weights `title=3.0`, `body=1.0`, sign flipped and floored at 0. Unbounded above: a strong lexical match can own most of the score. |
| `recency` | 0.20 | `0.5 ^ (age_days / 30)`. A record 30 days old scores half of one written now; a future timestamp counts as now. |
| `importance` | 0.15 | `decision=1.0`, `attempt=0.8`, `handoff=0.7`, `note=0.5`, anything else (`state`, `dump`, `item`) `=0.3`. |
| `pin` | 0.05 | `max(accepted decision, author confidence)`: an accepted decision scores 1.0; otherwise the record's `confidence` in `[0, 1]`; otherwise 0. |
| `link` | 0.05 | 1.0 when the record's text references another record with `[[id]]`, else 0. This is the link-graph signal search owns; 1-hop expansion of a result set belongs to the context packer. |

Ties are broken by `updated_at DESC`, then `id ASC`. The order is total and
deterministic, which is what makes paging safe.

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
| `pin` | the record is an accepted decision or has a stated confidence. |
| `link` | the record references another record with `[[id]]`. |

`--explain` additionally returns the weighted contribution of every signal.
Contributions are the same terms divided by `total + 1`, so they add up to the
score exactly; each contribution's share of the score is the signal's share of
`total`. An unexplained ranking position can therefore be read off the numbers
instead of guessed. Reasons and contributions describe the score, they never
adjust it.

## Flags

| Flag | Effect |
| --- | --- |
| `--match-mode all` | every term must match (AND); no automatic widening. |
| `--match-mode any` | any term may match (OR). |
| `--relaxed` | with `--match-mode all`, retry the same terms joined with OR when the strict pass finds nothing; the response then reports `relaxed: true`. |
| `--min-score <0..1>` | drop hits below the composite score. |
| `--explain` | add per-signal contributions (JSON `explain`, text `score`/`reasons` lines). |
| `--full` | return each hit's whole body alongside its preview (JSON `body`). |
| `--page-size <n>` | hits per page; defaults to `--limit`. |
| `--cursor <cursor>` | next page of the same query. |
| `--json` | envelope with `relaxed`, `page`, `page_size`, `has_more`, `next_cursor`, `hits`. |

Without `--match-mode`, search keeps its historical behaviour: a strict AND pass
first, then the same terms joined with OR when the strict pass is empty. The
fallback is now reported (`relaxed: true`, plus a stderr line in text mode)
instead of being invisible.

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

Tokens are estimated at four characters per token. The estimate is
deterministic and dependency-free; it is a limit to respect, not a billing
figure. `brain eval` reports `tokens_per_answer` for the retrieval answer, so a
change to previews or expansion shows up as a tokens delta next to recall.

## Compatibility

- Every pre-0.16 flag keeps its meaning: `--limit`, `--kind`, `--dumps`,
  `--since`, `--as-of`, `--include-superseded`, `--all-time`, `--json`.
- `brain search` text output is unchanged: `kind/project/title` and an indented
  preview; `--explain` and `--full` add lines, they never replace the first two.
- JSON is additive: `relaxed`, `page`, `page_size`, `has_more`, `next_cursor`,
  `match_reasons`, `explain`, `body`. `score` changed meaning from the raw BM25
  rank to the composite score in `[0, 1]`.
- `brain get` prints the text fields of a record; `brain record show` keeps
  printing every structured field, links and history. Use `get` when tokens
  matter, `record show` when structure matters.
- An index built before 0.16 is rebuilt automatically on the next search: the
  ranking inputs (decision status, author confidence) are projection data, and
  Markdown stays the source of truth.
