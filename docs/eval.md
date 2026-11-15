# Retrieval evaluation harness

`brain eval` scores the retrieval path against authored golden fixtures. It is
the quality gate for `search v2`, temporal validity and context packing
(WB-1.9). It runs fully offline: a temporary brain data directory, the local
SQLite index, no network, no model and no external service.

```sh
go run ./cmd/brain eval --fixtures eval/fixtures
go run ./cmd/brain eval --fixtures eval/fixtures --json
go run ./cmd/brain eval --fixtures eval/fixtures --baseline eval/baseline.json
```

The command exits 0 when the run matches its baseline (or when no baseline is
given), 1 on a metric regression or a fixture error, and 2 on a usage error.

## Metrics

Every task is a query plus labelled record ids. The harness takes the top 5
search hits and computes:

| Metric | Definition |
| --- | --- |
| `recall@5` | Fraction of tasks where at least one `expected` id is in the top 5. |
| `precision@5` | Mean of `|expected ∩ top5| / 5`. The denominator is always 5, so with one expected id a perfect single hit scores 0.2. This measures noise in the answer, not recall. |
| `mrr` | Mean reciprocal rank of the first `expected` id; a miss contributes 0. |
| `superseded_leak_rate` | Fraction of tasks that declare `forbidden` ids where any forbidden id appears in the top 5. On a current-state task the forbidden record is superseded; on an `as_of` task it did not exist yet. Target: **0**. With a zero baseline any increase fails the gate. |
| `tokens_per_answer` | Mean estimated tokens of the answer pack: title plus snippet for each of the top 5 hits, `ceil(bytes/4)`. It measures the retrieval answer only; the full context pack (state, handoff, findings) has its own budget. |
| `p95_latency_ms` | Nearest-rank p95 of the per-task search call after one warm-up. The only non-deterministic metric; p50 is reported too. |

`brain eval` prints the overall metrics and a compact table per fixture
`category`. Every ranking-derived number is deterministic: two runs of the same
fixtures produce the same hits, ranks and metrics. Only latency varies.

## Fixture format

One YAML file is one synthetic project: its records and its tasks. All
`*.yaml`/`*.yml` files in the fixture directory are loaded in file-name order.

```yaml
version: 1
project: lv24-brain
notes: "What this corpus covers."

records:
  - id: decision-2026-10-14-fts5-search
    kind: decision              # decision | attempt | note
    title: "Keep SQLite FTS5 as the only retrieval backend"
    status: accepted            # proposed | accepted | rejected | superseded
    created_at: 2026-10-14T09:20:00Z
    context: |
      In October 2026 the decision was to keep FTS5 bm25 with prefix word
      forms. The adapter lives in internal/adapters/sqliteindex.
    alternatives: ["Local embeddings"]
    consequences: ["Golden fixtures gate search v2"]
    evidence: ["search_recall_test.go"]
    paths: ["internal/adapters/sqliteindex"]
    # Temporal fields (all optional):
    valid_from: 2026-10-14T09:20:00Z
    valid_to: 2027-01-01T00:00:00Z
    superseded_by: decision-2027-01-01-newer-id
    supersedes: decision-2026-01-01-older-id
    confidence: 0.8

tasks:
  - id: q-fts5
    query: "sqliteindex FTS5 bm25"
    category: identifier        # free label; default "retrieval"
    expected: [decision-2026-10-14-fts5-search]
    forbidden: [decision-2026-01-01-older-id]
    as_of: 2026-11-01T00:00:00Z # optional point-in-time query
    notes: "Why this label is correct."
```

Rules enforced by the loader:

- `version` must be `1`; unknown YAML keys are rejected (typos cannot drop a
  label silently); a file holds exactly one YAML document.
- A project appears in one file. Record ids and task ids are unique.
- `created_at` and any `as_of`, `valid_from`, `valid_to` are RFC 3339.
- `expected` is non-empty and every id must exist in the same project.
- `forbidden` ids must exist, must not overlap `expected`, and mean "must not
  rank". Use them for superseded records on current-state tasks and for
  not-yet-valid records on `as_of` tasks.
- Decision paths must be repo-relative (same rule as `brain decision`).
- A `superseded_by`/`supersedes` reference must name a record in the project.
- At least one record and one task per file.

`valid_from`, `valid_to`, `superseded_by` and `supersedes` map to the temporal
validity fields. A record with `valid_to` set is excluded from default (current)
queries and included only in an `as_of` window that contains it.

## Adding a task

1. Pick a category: `identifier`, `paths`, `morphology`, `paraphrase`,
   `temporal`, `natural`, `staleness`, `as_of`.
2. If the corpus has no governing record, add a small one (one to three
   sentences) with the terms a user would actually type.
3. Label `expected` with the record that **should** answer the query, not the
   record that currently ranks first. Keep a known miss when search lacks the
   synonym: that is the measurement, and rewriting the record to make the test
   pass destroys it. Note the gap in `notes`.
4. For a current-state question, add the obsolete decision to `forbidden`. For
   a historical question, add `as_of` and put records that were not valid yet
   in `forbidden`.
5. Run `go run ./cmd/brain eval --fixtures eval/fixtures`. Inspect a surprising
   result with `--json` (each task reports its top-5 ids and first expected
   rank).
6. Regenerate the baseline only when the change is intentional, and review the
   diff:

   ```sh
   go run ./cmd/brain eval --fixtures eval/fixtures --write-baseline eval/baseline.json
   ```

The committed corpus currently has 52 records and 63 tasks: 50 retrieval
(identifier, path, morphology, paraphrase, natural, temporal), 10 staleness and
3 `as_of` history questions.

The committed corpus lives in `eval/fixtures/`:

| File | Project | Focus |
| --- | --- | --- |
| `engine.yaml` | `lv24-brain` | identifiers, paths, Russian morphology, paraphrase, temporal language, known synonym gaps |
| `ops.yaml` | `brain-server` | server operations, deployments, migrations, Russian notes |
| `staleness.yaml` | `lv24-brain-stale` | 10 superseded decisions and 3 `as_of` history questions |

## Baselines

`eval/baseline.json` is the committed snapshot. `--baseline` compares the run
with these tolerances:

| Metric | Allowed worsening |
| --- | --- |
| `recall@5`, `precision@5`, `mrr` | 0.02 absolute |
| `superseded_leak_rate` | 0 (any increase fails) |
| `tokens_per_answer` | 10% |
| `p95_latency_ms` | 2× plus 5 ms |

A failed check prints the baseline, current and allowed values and exits 1.
An improvement always passes. The baseline stores a SHA-256 digest of the
fixture files; if the digest differs, the gate still runs and prints a warning,
so a label change cannot silently reuse an old baseline. Latency is
machine-relative: regenerate the baseline after a deliberate change and treat
small latency moves as noise.

Baseline files are stable JSON (`version`, `fixture_digest`, `task_count`,
`metrics`) and belong in review like code.

## Temporal and staleness tasks

Current-state tasks already exercise validity: records with `valid_to` and
`superseded_by` are filtered by the default search, so a stale decision cannot
rank and the leak rate stays 0. Historical questions use `as_of`:

```yaml
  - id: st-asof-sync-june
    query: "how does sync pull remote changes"
    category: as_of
    as_of: 2026-06-01T00:00:00Z
    expected: [decision-2026-03-02-cron-sync]     # current then
    forbidden: [decision-2026-09-12-bounded-rebase] # did not exist yet
```

When adding a temporal record, set `valid_to` on the old record and
`superseded_by` to the new id, plus `supersedes` on the new record; then add
both the current-state task and the `as_of` task. The harness validates the
cross-references and passes `as_of` to `app.SearchOptions.AsOf`, so it tests the
product, not fixture-side filtering.

## Licensing and scope

The fixtures are authored for this repository and covered by the repository
licence (Apache-2.0). No text or labels are copied from LoCoMo (CC BY-NC 4.0),
MemBench or MemoryAgentBench (no licence file), BEAM or τ-bench. Those are
deliberately not used in CI.

LongMemEval (MIT) is the only external benchmark considered, and only as a
future adapter: ingest each history as note records in a throwaway project, map
evidence session ids to `expected`, run retrieval-only, and score recall@10,
knowledge-update correctness (latest fact in the top 5, superseded fact absent
or marked) and abstention for `_abs` questions. No adapter or dataset is
shipped in this release; the fixture interface above is the contract it would
target.

## CI

`go test ./eval ./cmd/brain -count=1` is deterministic and suitable as a
blocking job. The metric run is documented as **non-blocking** because latency
is machine-relative and a quality baseline should inform, not block, unrelated
changes. This snippet is documentation only; WB-1.9 does not add a workflow or
any paid CI service. If added to the existing `ci.yml`, it uses the same free
GitHub-hosted runner minutes as the other jobs:

```yaml
  eval:
    if: github.event_name == 'push'
    continue-on-error: true
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go test ./eval ./cmd/brain -count=1
      - run: go run ./cmd/brain eval --fixtures eval/fixtures --baseline eval/baseline.json --json
```

Do not make the metric job required until the baseline has been stable across
two machines for a week.
