# Context budget

`brain context` and the `brain_context` MCP tool return one curated answer for
a task. The answer is packed in ordered layers under a single token budget, so
a large brain cannot flood an agent prompt and a small one still gets the state
first.

## Layers

Layers are filled in order: state and handoff first, then the records that match
the task, then the curated background. A layer's cap is a **share of the total
budget**, so the same packer behaves for 500 and 32 000 tokens; a layer may
borrow the part of an earlier share that was left unused, which makes the caps
fungible.

| # | Layer | Contents | Share of the budget |
|---|---|---|---|
| 1 | `core` | project state, next action, open questions, latest handoff, review findings | 25 % |
| 2 | `query` | search-v2 hits for the task, plus failed attempts matching it | 35 % |
| 3 | `decisions` | current and pinned (accepted) decisions not already in the query layer | 20 % |
| 4 | `attempts` | recent attempts and their outcome not already in the query layer | 12 % |
| 5 | `linked` | 1-hop `[[...]]` neighbours of the packed records, same project only | 8 % |

The default total is **6 000 tokens**. The maximum a caller may request is
**32 000**.

The core layer is never dropped: if it alone exceeds its share, its prose is
truncated with a `(truncated)` marker and `truncated[]` gets an entry with
`id: "core"`. Everything else follows one rule: **a record is never split**.
There is no fixed record count: the budget decides how many records of a layer
fit, so a large budget returns every matching record.

## Truncation policy

- A record that does not fit the remaining room in its layer is skipped whole
  and listed in `truncated[]` as `{id, section, reason}`.
- The Markdown summary ends every layer that left records out with
  `- … N more dropped`, so a reader can tell a short layer from a complete one.
  A layer that packed nothing still prints its heading and that line when the
  budget dropped its candidates.
- Because candidates are ordered best first, the tail of a layer is what gets
  dropped: lowest score first, then oldest update, then id. A very large
  high-score record can be skipped while smaller lower-score records still fit;
  the reason names the token need and the room left.
- The rendered answer is measured after packing: if it would still exceed the
  budget (a marker line, an empty-section placeholder), the lowest-priority
  packed records are dropped until it fits. The budget is a hard limit.
- Duplicates never appear twice: a decision or attempt selected for the query
  layer is not repeated in the background layers, and a linked record already
  present is skipped.
- The `linked` layer only exists when the earlier layers left room; its store
  lookup is skipped entirely when they did not.
- Raw-zone excerpts (`--raw`) are outside this accounting: they are opt-in,
  grant-gated and untrusted, and are added only when the curated context is
  thin.

## Ordering

The ordering is deterministic:

1. layer order (`core`, `query`, `decisions`, `attempts`, `linked`);
2. within a layer, score descending;
3. then `updated_at` descending;
4. then id ascending.

Two runs over the same store and clock produce the same sections, tokens and
summary.

## Token counting

Tokens come from `internal/textbudget`: Latin text packs about four characters
per token, while Cyrillic, Greek, CJK, Hangul and the other dense scripts pack
about two. The estimate is deliberate and dependency-free: a budget is a limit
to respect, not a billing figure. The rendered Markdown line of each record is
what gets counted, so the budget and the printed answer cannot drift apart.

## Since and as-of

`--since` compares the record's **content** change time, not its revision.
`content_updated_at` in the frontmatter moves only when the title, body or tags
change; review, snooze, supersede links, validity and merge bookkeeping rewrite
the file without changing the text, so they do not make a record recent. A
redaction that actually rewrites the text is a text change and does move it.
Records written before the field existed fall back to `updated_at`. Search,
`brain context` and `brain record list` use the same rule.

`--as-of` selects the records and the handoff valid at that instant. State and
review findings have no history: `state_as_of` is `"current"` in JSON and the
text summary prints one warning line, so a past-instant answer never pretends
the state was historical.


## Surfaces

### CLI

```sh
brain context "add retry to sync push" --budget 6000
brain context "add retry to sync push" --budget 6000 --explain
brain context "add retry to sync push" --json
```

- `--budget` is the total token budget. `0` selects `context.budget` from
  `brain.yaml`, then the 6 000 default.
- `--explain` prints the per-layer table (`core: 240/500 tokens, 3 records`,
  `total: 1200/6000 tokens, 4 dropped`). With `--json` the same lines are in
  `explain[]`.
- `--json` returns `budget`, `total_tokens`, `sections[]`
  (`name`, `cap`, `tokens`, `records[]` with `id`, `kind`, `title`, `path`,
  `tokens`, `score`), `truncated[]` (at most 20 entries), `total_truncated`,
  `state_as_of`, and `quarantined_count`.
- `--include-unreviewed`, `--as-of`, `--include-superseded`, `--kind`,
  `--since` and `--raw` keep their meaning. Quarantined and superseded records
  are filtered before packing, so a filtered hit cannot consume budget.

`brain open` uses the same packer for its full summary (state + handoff +
recent decisions + recent attempts). `brain open --brief` stays state-only.

### Configuration

```yaml
# brain.yaml
context:
  budget: 6000   # tokens; 0 or omitted selects the default
```

The value is validated: negative and above 32 000 are configuration errors.

### MCP

`brain_context` takes `budget` (default 6 000, max 32 000) and `explain`
(boolean). Its structured result carries the same `sections`, `truncated`,
`total_truncated`, `state_as_of`, `total_tokens` and `quarantined_count` fields
as `--json`; `truncated[]` is capped at 20 entries while `total_truncated` keeps
the full count. A bad budget is a validation refusal, not a silent clamp.

## Tuning

| Goal | Change |
|---|---|
| A model with a small window | lower `--budget`; core and query come first, so the state and the task hits are still there |
| More task-specific hits | raise the total: every layer's cap is a share of it, so all caps grow together |
| Less background noise | pass `--kind decision` (or `note`/`attempt`) to skip the other background layers |
| A larger curated baseline | raise `context.budget`; 12 000–16 000 is a comfortable ceiling for one task |
| Debugging a drop | run `--explain` and read `truncated[]`; the reason names the layer and token shortfall |

Because the caps are shares, raising the total raises every cap with it. A layer
that does not spend its share does not waste the tokens: the unused part is
borrowed by the next layer, and only what no layer packs stays free. A record
that is still dropped did not fit the room its layer had; the reason names the
need and the room left.
