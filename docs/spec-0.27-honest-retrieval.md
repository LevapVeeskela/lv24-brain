# Спецификация: lv24-brain 0.27 — Honest Retrieval & Belief Lifecycle

Status: approved for implementation
Date: 2026-11-18
Related: `docs/audit-2026-11-17.md`, `docs/idea-scan-2026-11.md`,
`docs/spec-0.25-second-brain.md`

Scope: 0.27.0 (honest retrieval + index integrity), 0.27.1 (belief lifecycle),
0.28 (entities and capture). The architecture does not change: git-canonical
Markdown, SQLite FTS5 sidecar, deterministic core, single binary, 3 machines.

Non-goals: embeddings/vector stores, LLM extraction or rerank, cloud sync,
federation, decay/TTL deletion, in-place record mutation, tool sprawl.

---

## 1. Problem

The 0.26 audit found that retrieval cannot say "nothing found" honestly
(`OR` fallback presents partial matches as answers), context silently drops
records, the index claims `complete` while stale, and the review surface is
~87% noise. The idea scan confirmed these are the real gaps; storage and sync
are not.

## 2. Requirements

### WS1. Index integrity (0.27.0)

| ID | Requirement | Acceptance |
|---|---|---|
| IDX-1 | SQLite index schema v10 adds `content_hash TEXT` (normalized title+body) and `stub INTEGER DEFAULT 0`, plus a `meta` table with `index_generation`, `indexed_from`, `schema_version`. | Migration drops/recreates like v8/v9; old DB rebuilds once. |
| IDX-2 | `index_generation` changes when the index content changes (reindex after HEAD/dirty or record edits); status reports `stale at <sha>` when `indexed_from` != current HEAD (or dirty). | `brain index --status --json` (or `brain status`) returns `stale`, `indexed_from`, `head`. No cache survives a generation bump. |
| IDX-3 | `content_hash` is deterministic across machines: lowercase, collapse whitespace, strip record id/date markers, hash title+"\n"+body. | Same record on two clones yields the same hash. |
| IDX-4 | `stub` is derived at projection time: title matching `^(note|decision|attempt|handoff)-<date>-<id>` or body containing only "nothing new"/"session ended"/"no changes" patterns. Stubs stay in lists/counts but are excluded from search/context/findings by default. | Fixture: 5 stubs excluded, 5 real kept; `--include-stubs` shows them. |

### WS2. Retrieval honesty (0.27.0)

| ID | Requirement | Acceptance |
|---|---|---|
| RET-1 | Support gate: a result is `supported` when it contains all rare query terms (document frequency <= 20%) and >= 50% of subject terms; `related` when only the OR pass matched; `none` when nothing passes. | 20 absent-topic queries -> `none` >= 90%; 30 real queries keep top-5 >= 0.95. |
| RET-2 | Search response adds `tier` per hit and top-level `support {tier, best, threshold, reason}`; `none` returns an empty hit list with an explicit reason, not relaxed noise. | CLI/MCP JSON and text both carry it; no silent fallback. |
| RET-3 | Content-hash duplicates are collapsed in search/context: first occurrence wins (oldest `created_at`, then id), duplicates listed in `excluded[]` with reason `duplicate`. | Seeded duplicate does not occupy top-5; excluded entry present. |
| RET-4 | Every dropped/excluded record carries a reason: `stale`, `superseded`, `quarantined`, `duplicate`, `stub`, `over-budget`, `lower-rank`, `other-owner`. Context keeps near-misses (top 3 below the cut) with scores. | JSON `excluded[]`; Markdown prints `… N dropped (reason)`. |
| RET-5 | Path/branch relevance: when the caller supplies a task, score overlap between `git diff --name-only` (or the current branch) and `Decision.Paths`; additive, capped, visible in `--explain`. Gate on eval: enable only if top-5 improves. | Eval delta recorded; feature flag `ranking.paths`. |

### WS3. Write path (0.27.0)

| ID | Requirement | Acceptance |
|---|---|---|
| WRT-1 | After `note`/`decision`/`attempt`, compute BM25 top-3 similar records (excluding the new id) and return them as `similar[]` {id, title, score} in MCP `structuredContent`; CLI prints them only with `--similar`. | Near-duplicate write surfaces the original in top-3 >= 70% on the fixture; default stdout unchanged. |
| WRT-2 | Capture-time redaction: hook/journal writers use the same `RedactIngest` as `record edit`; no secret reaches a record or journal line. | Test with a bearer token in a hook payload. |
| WRT-3 | `brain note/decision` accepts `--supersedes <id>` (already) and the similar hint names it when the top hit is an older record of the same kind. | Hint text suggests the exact flag; no auto-supersede. |

### WS4. Status UX (0.27.0)

| ID | Requirement | Acceptance |
|---|---|---|
| UX-1 | `brain open`/`brain status` distinguish `ABSENT` (no records), `EMPTY` (project registered, no state/handoff), `POPULATED`; a wrong checkout says which path was resolved and why. | Three fixtures + one wrong-cwd case. |
| UX-2 | Index status line: `index: complete at <sha>` or `index: stale at <sha> (HEAD <sha>)`. | Stale after a commit without reindex. |
| UX-3 | `brain doctor` adds `index-generation` warning when stale > 1 commit; no failure by default. | Report-only. |

### WS5. Belief lifecycle (0.27.1)

| ID | Requirement | Acceptance |
|---|---|---|
| LIFE-1 | `supersede` writes a successor as a `proposed` case; the predecessor remains valid/searchable until approval; rollback restores the exact `valid_from/valid_to/superseded_by` tuple. | Predecessor found before approval, hidden after; rollback byte-equal tuple. |
| LIFE-2 | Invalidated archive: `invalidated_at` + reason; retired records stay queryable with `--archive`, never edited away. | Archive query returns them; default search hides. |
| LIFE-3 | Write-time contradiction gate: Go heuristics (negation, antonyms, value change, temporal override) gated by FTS overlap; lands as `suspected contradiction` in review/quarantine, never an auto-edit. | Fixture: 8 true pairs flagged, 8 near-miss pairs not. |
| LIFE-4 | Session-scoped batch review: group pending proposals by session; accept/reject/merge as one unit with one digest line. | 10 proposals in 2 sessions -> 2 actions. |
| LIFE-5 | Spaced-repetition review cadence: `review_after` interval extends on confirmation, shortens on contradiction; no deletion. | Two confirmations double the interval; contradiction halves it. |

### WS6. Entities and capture (0.28)

| ID | Requirement | Acceptance |
|---|---|---|
| ENT-1 | Entity layer from explicit edges only: materialize entities/triples from `[[links]]`, `--relation`, supersede chains into the index projection; `brain graph <entity>` + 1-hop expansion behind a budget flag. No LLM. | 10 relational queries improve top-5, otherwise flag stays off. |
| CAP-1 | PreCompact hook writes a deterministic proposal (state/goal/next + `Decision:` lines); no LLM. | Compaction mid-session leaves a proposal. |
| CAP-2 | Reflection as proposal-only consolidation: summarize 2+ records into a cited proposal; sources never deleted. | Proposal shows sources; approve creates a new record. |

## 3. Data and compatibility

- Index schema v10; one automatic rebuild. Markdown format unchanged in 0.27.0.
- Search/MCP responses are additive (`tier`, `support`, `excluded`, `similar`);
  existing keys stay. Local API golden operation set unchanged except additive
  fields.
- No record is deleted or rewritten by 0.27.0. Stub is a derived flag, reversible.
- AGPL project (superlocalmemory) contributes behaviour only; no code, names or
  constants are copied. Cite the arXiv papers for reused math.

## 4. Rollout

| Version | Content | Gate |
|---|---|---|
| 0.27.0 | WS1-WS4 | absent-query `none` >= 90%; top-5 >= 0.95; stubs excluded; stale status |
| 0.27.1 | WS5 | predecessor visible until approve; contradiction fixture 8/8; batch review |
| 0.28.0 | WS6 | entity uplift measured; pre-compact proposal |

## 5. Risks

| Risk | Mitigation |
|---|---|
| Support gate rejects useful partial matches | `related` tier stays visible; eval gate on the 30 real queries |
| Stub heuristic mislabels short but real notes | derived flag only, `--include-stubs`, never deletes |
| Content hash collides across kinds | hash includes kind; duplicates listed, not dropped silently |
| Path relevance overfits one repo | additive cap, flag, eval delta required |
| Index v10 rebuild cost | one-shot, same batch path as v9 (3-4 s on current corpus) |
