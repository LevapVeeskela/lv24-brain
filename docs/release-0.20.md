# Release 0.20.0 — state provenance, server review, pin/unpin, LvEF exchange, graph surprises

Status: implementation complete; not tagged. Engine commits: `faf9344`
(LvEF v1 export/import), `7a4f00d` (state provenance, server record review,
pin/unpin, graph surprises and PR deltas). Desktop 1.4.0: `1184a00`
(snoozed queue merge). Data revision `67c3c52`.

0.20 makes memory accountable and portable. Project state now carries
provenance and can be quarantined and reviewed, server mode can review a
record's provenance without rewriting its body, records can be pinned and
unpinned with a ranking boost, curated records can move between brain
instances through the deterministic LvEF v1 bundle, and the code graph
surfaces surprising cross-community edges. Desktop 1.4 merges the
All/Pending/Snoozed review queues.

## State provenance

`state.md` carries the same `origin` / `reviewed` / `trust` keys as curated
records:

- `brain state set` writes `origin: human, reviewed: true, trust: verified`;
  MCP `brain_state set` writes `origin: agent, trust: stated` and is reviewed
  unless `security.quarantine_agent` is set.
- A quarantined state is reported as `state_quarantined: true`; its text is
  replaced with a minimal placeholder in trusted context. Pass
  `--include-unreviewed` to view it.
- `brain state review` clears the quarantine, optionally with `--trust`.
- MCP `brain_state` gained `review`; agent writes carry `origin: agent`.
- A `state-quarantined` finding and a `state_quarantined` flag surface the
  queue in `brain review` and `brain open`.

## Server record review

Server mode can now review a record's provenance without rewriting its body:

- `POST /v1/projects/{project}/records/{kind}/{id}/review` and
  `/v1/records/{kind}/{id}/review`.
- Provenance-only update of `reviewed` and `trust`, optimistic concurrency
  through `expected_revision`, owner/member/admin access rules, a server
  revision bump and an audit event.
- Quarantined (unreviewed) records stay out of trusted context and server
  search until reviewed.
- Postgres migration `0010` adds the review columns; `httpclient` gained a
  `RecordReviewer`.
- Local/git review (`brain record review`, `brain_judge(action=review)`) is
  unchanged; server mode closes the gap noted in 0.15.

## Pin and unpin

Records can be marked important and stay boosted in ranking and context:

- `pinned` frontmatter on a record; pinned records are boosted in search
  ranking and in context packing.
- Index schema v7 stores the flag; an existing store must be rebuilt
  (`brain code index --rebuild` is not required for the memory index, but the
  memory index rebuild runs on upgrade where the schema moved).
- `brain_judge pin <id>` and `brain_judge unpin <id>` (MCP and CLI) set and
  clear the flag. Pinning is no longer refused over MCP.
- Consolidation remains refused over MCP and stays CLI-only.

## LvEF v1 exchange

Curated records move between brain instances through a deterministic bundle:

- `brain export --out <dir-or-zip>` writes `manifest.json`, `records.jsonl`,
  `projects.jsonl` and `tombstones.jsonl` (empty in v1). Records are state,
  note, decision, attempt and handoff only.
- `brain import --from <dir-or-zip>` is idempotent by content hash; identical
  content is `unchanged`, differing content is `conflict` and is never
  overwritten. Missing projects are created only with `--create-projects`.
- `--dry-run` previews. Zip containers use store/deflate only and are guarded
  against path traversal and zip bombs.
- `export -> import -> export` is byte-identical for the same data.
- MVP limits: no rollback, no tombstones beyond the empty file, no
  attachments, no proposal or raw-zone data, no server-side exchange route.

## Graph surprises and PR deltas

- `brain_graph(action=surprises)` returns dependency edges whose endpoints live
  in different communities, scored by confidence, degree and rarity. Capped at
  50.
- PR impact community deltas ride along with the existing `pr_impact` action.
- MCP tool list gains `surprises`; the `brain_graph` action set is now nine.

## Desktop 1.4.0

Shipped (`lv24-brain-desktop` commit `1184a00`, tag `v1.4.0`), bundles
engine 0.20.0:

- Full `include_snoozed` queue merge in the inbox: All / Pending / Snoozed
  tabs, with deduplication and compare-and-swap refresh.
- Snooze and unsnooze from the inbox, with snoozed badges and remaining time.
- 199 tests green (`npm run check`: typecheck, tests, production build).

## Migration notes

- Data schema stays `schema_version: 1`; provenance, pin and exchange are
  additive. Older engines keep reading the files.
- Memory search index moves to schema v7 for the `pinned` flag; rebuild it on
  upgrade (`brain index --rebuild` or the equivalent local command). No
  Markdown truth is rewritten.
- Postgres: migration `0010` adds the server record review columns. Startup
  refuses to run until a backup is taken and
  `BRAIN_DATABASE_BACKUP_CONFIRMED=1` is set; migrations are forward-only.
- LvEF is additive and optional: no migration, no overwrite. Existing
  exports from 0.19 do not exist; 0.20 is the first exchange release.
- MCP hosts must be restarted to see `surprises` and `brain_graph`'s nine
  actions. `brain_judge` now serves `pin` and `unpin`.
- `lv24-legacy` bridge profile remains deprecated and is removed in 0.19's
  successor line; the seven 0.16 names remain available only under it.

## Known gaps

- LvEF MVP: no rollback, no tombstones beyond the empty file, no
  attachments, no proposals or raw-zone data, no server route.
- Server record review edits provenance only; it does not edit bodies or
  other fields.
- Pinning boosts ranking and context but does not yet drive consolidation or
  automated eviction.
- Graph surprises are capped at 50 and are not cached separately from the
  partition.
- Desktop 1.4 requires engine 0.20 for the merged snoozed queue.
- RustDesk password rotation is still pending.

## Release checklist

- [ ] Review this note and `docs/release-0.20.md` style against
      `docs/release-0.19.md`.
- [ ] Bump engine and desktop versions to 0.20.0 / 1.4.0.
- [ ] Update docs site: version strings, footer, landing eyebrow, install and
      update references, new/updated pages.
- [ ] Run engine `go test ./...` and desktop `npm run check`.
- [ ] Run docs site `npm test`, `npm run lint`, `npm run docs:build`.
- [ ] Tag `v0.20.0` and `v1.4.0`, push, publish npm/PyPI/GHCR and the
      signed desktop release.
- [ ] Rotate the RustDesk password.