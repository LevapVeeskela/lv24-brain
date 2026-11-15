# Context budget

`brain context` and the `brain_context` MCP tool return one curated answer for
a task. The answer is packed in ordered layers under a single token budget, so
a large brain cannot flood an agent prompt and a small one still gets the state
first.

## Layers

Layers are filled in order. Each has its own cap; the later layers also respect
the room the earlier ones left in the total budget.

| # | Layer | Contents | Cap (tokens) |
|---|---|---|---|
| 1 | `core` | project state, next action, open questions, latest handoff, review findings | 1 200 |
| 2 | `decisions` | current and pinned (accepted) decisions not already in the query layer | 1 500 |
| 3 | `attempts` | recent attempts and their outcome not already in the query layer | 1 000 |
| 4 | `query` | search-v2 hits for the task, plus failed attempts matching it | 1 500 |
| 5 | `linked` | 1-hop `[[...]]` neighbours of the packed records, same project only | 800 |

The default total is **6 000 tokens**. The maximum a caller may request is
**32 000**.

The core layer is never dropped: if it alone exceeds 1 200 tokens, its prose is
truncated with a `(truncated)` marker and `truncated[]` gets an entry with
`id: "core"`. Everything else follows one rule: **a record is never split**.

## Truncation policy

- A record that does not fit the remaining room in its layer is skipped whole
  and listed in `truncated[]` as `{id, section, reason}`.
- Because candidates are ordered best first, the tail of a layer is what gets
  dropped: lowest score first, then oldest update, then id. A very large
  high-score record can be skipped while smaller lower-score records still fit;
  the reason names the token need and the room left.
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

1. layer order (`core`, `decisions`, `attempts`, `query`, `linked`);
2. within a layer, score descending;
3. then `updated_at` descending;
4. then id ascending.

Two runs over the same store and clock produce the same sections, tokens and
summary.

## Token counting

Tokens come from `internal/textbudget`, the shared four-characters-per-token
estimate. There is no model-specific tokenizer dependency: a budget is a limit
to respect, not a billing figure. The rendered Markdown line of each record is
what gets counted, so the budget and the printed answer cannot drift apart.

## Surfaces

### CLI

```sh
brain context "add retry to sync push" --budget 6000
brain context "add retry to sync push" --budget 6000 --explain
brain context "add retry to sync push" --json
```

- `--budget` is the total token budget. `0` selects `context.budget` from
  `brain.yaml`, then the 6 000 default.
- `--explain` prints the per-layer table (`core: 240/1200 tokens, 3 records`,
  `total: 1200/6000 tokens, 4 dropped`). With `--json` the same lines are in
  `explain[]`.
- `--json` returns `budget`, `total_tokens`, `sections[]`
  (`name`, `cap`, `tokens`, `records[]` with `id`, `kind`, `title`, `path`,
  `tokens`, `score`), `truncated[]`, and `quarantined_count`.
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
`total_tokens` and `quarantined_count` fields as `--json`. A bad budget is a
validation refusal, not a silent clamp.

## Tuning

| Goal | Change |
|---|---|
| A model with a small window | lower `--budget`; core and decisions fill first, so the state is still there |
| More task-specific hits | raise the total and keep the caps; or narrow the task text |
| Less background noise | pass `--kind decision` (or `note`/`attempt`) to skip the other background layers |
| A larger curated baseline | raise `context.budget`; 12 000–16 000 is a comfortable ceiling for one task |
| Debugging a drop | run `--explain` and read `truncated[]`; the reason names the layer and token shortfall |

Raising the total does not raise the per-layer caps. Layers below their cap
(`attempts` and `linked` in a small project) leave the rest of the budget
unused on purpose: the answer is curated, not filled.
