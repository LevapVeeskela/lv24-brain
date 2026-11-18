# Idea scan: 8 agent-memory MCP servers (2026-11)

Method: 8 repositories cloned to `%TEMP%\lv24-ideas`, one read-only agent per
project, same rubric (architecture, memory model, retrieval, multi-session,
ingestion, unique features, weaknesses, adoption fit, license). No code was run.

## The field

| Project | License | Shape | Verdict |
|---|---|---|---|
| omega-memory | Apache-2.0 | Python, SQLite+sqlite-vec+ONNX | Most complete single-machine memory; heavy |
| hindsight | MIT | Python, Postgres+pgvector, LLM extraction | Strong evidence model; enterprise-scale |
| superlocalmemory | **AGPL-3.0** | Python, SQLite+vec, 283k LOC | Rich lifecycle; ideas only, no code |
| dakera-mcp | MIT (client) | Rust bridge to a closed server | Good tool diet/lifecycle; engine unauditable |
| nucleus-mcp | MIT (open-core) | Python, `.brain/` JSONL+SQLite | Honest-unknown UX; sprawl, flags off |
| novyx-mcp | MIT | Python, SQLite | 102 tools, 45 cloud-only; one good idea |
| cortex-memory | MIT | TypeScript, `.cortex/` files | 1 commit, 0 tests; mostly vaporware |
| claude-code-organizer | MIT | Node, config control plane | Not memory; strong safety patterns |

**Pattern:** almost every competitor is DB-canonical, single-machine, with
embeddings and/or an LLM in the write path, tool sprawl (58-106 tools), and a
sync story that is either cloud-only or absent. Our git-canonical Markdown,
FTS5, provenance/review, 17 tools and 3-machine merge are a different (and
better-fitting) architecture, not a weaker one. Nothing here justifies replacing
it.

## What we should adopt (ideas only, clean-room)

Ranked by value to our actual audit gaps; effort S/M/L; source in brackets.

### 0.27.0 — Honest retrieval
1. **Per-record support gate + tri-state answerability** [superlocalmemory]:
   require all rare query terms and >=50% subject-term coverage in one record;
   return `supported | related | none` with `best`/`threshold`. Kills the
   "relaxed results are still presented as answers" problem (audit F2/F3) and
   gives an honest no-answer without a model. M.
2. **Write-time `similar[]` hint** [novyx]: after note/decision, BM25 top-3 with
   ids+scores in MCP `structuredContent` and CLI `--similar` (not default
   stdout). The writer still has the context to set `--supersedes`. S.
3. **Index generation watermark** [superlocalmemory]: one monotonic
   `index_generation` (HEAD sha + dirty + schema) in index meta; status says
   "stale at <sha>" and caches key on it. Fixes the graph/index "complete but
   old" lie (audit F1). S/M.
4. **Content-hash dedup at index** [nucleus]: normalized hash + first-occurrence
   provenance; replayed notes and identical handoffs stop eating top-5 slots
   across the three machines. S.
5. **Per-record exclusion reasons** [claude-code-organizer]: in search/context
   JSON and MCP `truncated[]`, say why (stale/superseded/quarantined/
   over-budget/lower-rank/other-owner) and list near-misses. Fixes the silent
   budget drops (audit F5). M.
6. **Stub/noise classifier** [superlocalmemory]: label+ID records and "session
   ended / nothing new" handoffs get `stub: true`; they stay in lists/counts but
   leave search/context/findings. Directly attacks the 87% diagnostic noise. S.
7. **ABSENT / EMPTY / POPULATED status** [nucleus]: `brain open`/`brain status`
   distinguish "no knowledge", "empty project" and "wrong checkout". S.
8. **Path/branch relevance signal** [cortex]: overlap between the task's changed
   files/branch and `Decision.Paths`, unioned with BM25. We already store the
   columns and have an eval. M.

### 0.27.1 — Belief lifecycle
9. **Correction successor as a review case** [superlocalmemory + hindsight]:
   supersede writes a successor plus a `proposed` case; the predecessor stays
   valid/searchable until approval; rollback restores the exact validity tuple.
   Replaces immediate supersede + separate review bookkeeping. M.
10. **Invalidated archive** [hindsight]: `invalidated_at` + reason; retired
    records stay queryable in an archive zone instead of being edited away. M.
11. **Write-time contradiction gate** [omega]: Go heuristics (negation,
    antonyms, value change, temporal override) gated by FTS overlap; lands as
    "suspected contradiction" in review/quarantine, never as an auto-edit. M.
12. **Session-scoped batch review** [novyx]: group pending proposals by session;
    accept/reject/merge as one unit with one digest line. At 3 machines the
    per-record queue is the backlog. S.
13. **Proposal merge recommendation** [novyx]: nearest record, changed fields,
    label new|duplicate|supersede|conflict, would-be commit summary. Pairs with
    the desktop merge UI. M.
14. **Spaced-repetition review cadence** [dakera]: `review_after/interval`;
    confirmation extends, contradiction shortens; no deletion. S/M.

### 0.28 — Entities and capture
15. **Entity layer from explicit edges** [hindsight/novyx/dakera]: entities and
    triples materialized from `[[links]]`, `--relation` and supersede chains as
    a rebuildable index projection; `brain graph <entity>` + 1-hop expansion
    behind a budget flag. No LLM extraction. M.
16. **PreCompact capture** [cortex]: on the host's PreCompact event, write a
    deterministic proposal with state/goal/next and explicit `Decision:` lines.
    Long sessions that compact and never end cleanly currently lose the thread.
    S/M.
17. **Capture-time redaction** [omega]: the same credential redaction for hook
    and journal writers that `record edit` already uses. S.
18. **Reflection as proposal-only consolidation** [dakera/cortex]: summarize 2+
    records into a cited proposal; never delete or rewrite sources; lands via
    `brain promote`. M.

## What we explicitly skip

- Embeddings, vector stores, ONNX models, cross-encoder/LLM rerank — no measured
  gain over our trust-aware BM25, and they break the single-binary/offline model.
- LLM extraction/consolidation in the write path — nondeterministic, costs
  tokens, and Hindsight's own eval admits alternating destructive edits.
- Cloud sync, relay, Firestore, federation, multi-tenant schemas — our problem
  is solved by git; every DB-canonical project here either lacks sync or bolts
  it on.
- Decay/TTL hard-delete and in-place "evolution" — incompatible with audit and
  three-machine merge. Lifecycle stays record-based (supersede/archive/review).
- Tool sprawl (58-106 tools), health scores, namespaces/quotas, IDE installers,
  auto-widening project filters, hash-chained audit (git history is stronger).
- AGPL code from superlocalmemory — ideas only, no translation, own names and
  constants, cite the arXiv papers for the math we reuse (Ebbinghaus, Beta
  priors, RRF-style fusion).

## Why not just "keep developing"

The scan confirms the architecture is right; it also shows where we are behind:
not in storage or sync, but in **answerability** (honest "no"), **evidence and
lifecycle of a belief** (supersede, archive, contradiction), and **noise
control** (stub records, exclusion reasons). Those are exactly the audit's P1s.
So: keep developing, but adopt these eight ideas as 0.27 scope instead of
copying any product.

## Acceptance criteria for 0.27

1. Support gate: on 20 absent-topic queries, `none` in >= 90%; on 30 real
   queries, top-5 stays >= 0.95.
2. Similar hint: >= 70% of seeded near-duplicate writes surface the original in
   top-3.
3. Noise: review findings fall from 461 to < 100 on the current corpus without
   losing the true positives in the fixture.
4. Index: status reports `stale at <old sha>` instead of `complete`; no cache
   survives a generation bump.
5. Dedup: replayed notes no longer occupy top-5 in the real-corpus eval.
6. Supersede: predecessor stays searchable until approval; rollback restores
   `valid_from/valid_to/superseded_by` exactly.
7. Entity graph: 1-hop expansion improves top-5 on 10 relational queries,
   otherwise the feature stays behind a flag.
