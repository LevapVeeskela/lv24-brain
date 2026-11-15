package app

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// JournalRelation records a judged relation, so the activity journal shows who
// settled a pair and when.
const JournalRelation = "relation"

// Remaining journal events are declared in merge.go.

// Target states of a listed relation. A relation may point at a live record,
// at a tombstone (a merged, soft-trashed record) or at nothing.
const (
	RelationTargetFound     = "found"
	RelationTargetTombstone = "tombstone"
	RelationTargetMissing   = "missing"
)

// Relation listing caps.
const (
	// defaultRelationListLimit bounds one listing when the caller sets none.
	defaultRelationListLimit = 200
	// relationCandidateLimit is how many pending candidates one write may add.
	relationCandidateLimit = 3
	// relationCandidateThreshold is the shingle similarity at which a record
	// looks related enough to propose a pending link. It is looser than the
	// near-duplicate finding threshold because a candidate is advisory.
	relationCandidateThreshold = 0.6
	// relationCandidateAutoThreshold applies when either side is an automatic
	// session handoff: those are routine and share their wording.
	relationCandidateAutoThreshold = 0.9
)

// RelationView is one relation in a listing, with both ends resolved.
type RelationView struct {
	ProjectID string
	Source    RecordSummary
	TargetID  string
	// Target is the resolved live target; nil when it is a tombstone or
	// missing.
	Target *RecordSummary
	// TargetState is found, tombstone or missing.
	TargetState string
	// Relation is the stored verb, status, reason and verdict metadata.
	Relation domain.Relation
	// Direction is "out" for a relation the source carries and "in" for one
	// another record points at the requested target.
	Direction string
	// PendingSince is when the relation was proposed: the source record's last
	// write (a candidate is written with its record). Empty for a judged
	// relation.
	PendingSince time.Time
	// AgeDays is whole days since PendingSince; zero when judged or fresh.
	AgeDays int
}

// RelationListOptions selects a relation listing.
type RelationListOptions struct {
	ProjectID string
	// ID limits the listing to the relations of one source record.
	ID string
	// TargetID limits the listing to the relations that point at one record.
	TargetID string
	// Statuses keeps these statuses; empty keeps every status.
	Statuses []domain.RelationStatus
	// Kinds keeps these verbs; empty keeps every verb.
	Kinds []domain.RelationKind
	// Limit bounds the result; <= 0 uses defaultRelationListLimit.
	Limit int
}

// Relations lists the typed relations of a project. The Markdown frontmatter is
// authoritative; the derived index is used when it has rows and a records scan
// fills in for a project whose index was not built yet. Targets are annotated
// as live, tombstone or missing, so a dangling relation is visible instead of
// silently dropped.
func (a *App) Relations(ctx context.Context, opts RelationListOptions) ([]RelationView, error) {
	if err := a.requireStore(); err != nil {
		return nil, err
	}
	project, err := a.ensureProject(ctx, opts.ProjectID, false)
	if err != nil {
		return nil, err
	}
	for _, status := range opts.Statuses {
		if !status.Valid() {
			return nil, domain.NewValidationError("status", fmt.Sprintf("unknown relation status %q", status))
		}
	}
	for _, kind := range opts.Kinds {
		if !kind.Valid() {
			return nil, domain.NewValidationError("relation", fmt.Sprintf("unknown relation %q", kind))
		}
	}
	id := strings.TrimSpace(opts.ID)
	targetID := strings.TrimSpace(opts.TargetID)
	if opts.Limit < 0 {
		return nil, domain.NewValidationError("limit", "must not be negative")
	}
	if id != "" && targetID != "" {
		return nil, domain.NewValidationError("id", "pass either id or target-id, not both")
	}
	records, err := a.projectRecords(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	entries := a.indexedRelationEntries(ctx, project.ID, id, targetID)
	if len(entries) == 0 {
		entries = scanRelationEntries(records, id, targetID)
	}
	entries = filterRelationEntries(entries, opts.Statuses, opts.Kinds)
	byID := make(map[string]RecordSummary, len(records))
	for _, record := range records {
		summary := record.Summary()
		byID[summary.ID] = summary
	}
	tombstones := a.tombstoneIDs(ctx, project.ID)
	now := a.now()
	views := make([]RelationView, 0, len(entries))
	for _, entry := range entries {
		view := RelationView{
			ProjectID: project.ID, Source: entry.source, TargetID: entry.relation.Target, Relation: entry.relation,
		}
		switch {
		case id != "" && entry.source.ID == id:
			view.Direction = "out"
		case targetID != "" && entry.relation.Target == targetID:
			view.Direction = "in"
		default:
			view.Direction = "out"
		}
		if target, ok := byID[view.TargetID]; ok {
			copied := target
			view.Target = &copied
			view.TargetState = RelationTargetFound
		} else if _, merged := tombstones[view.TargetID]; merged {
			view.TargetState = RelationTargetTombstone
		} else {
			view.TargetState = RelationTargetMissing
		}
		if entry.relation.IsPending() {
			view.PendingSince = entry.source.UpdatedAt
			if view.PendingSince.IsZero() {
				view.PendingSince = entry.source.CreatedAt
			}
			if !view.PendingSince.IsZero() && now.After(view.PendingSince) {
				view.AgeDays = int(now.Sub(view.PendingSince).Hours() / 24)
			}
		}
		views = append(views, view)
	}
	sortRelationViews(views)
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultRelationListLimit
	}
	if len(views) > limit {
		views = views[:limit]
	}
	return views, nil
}

// relationEntry is one relation with the summary of the record that owns it.
type relationEntry struct {
	source   RecordSummary
	relation domain.Relation
}

// indexedRelationEntries reads the derived relation table. A missing index (or
// a project not indexed yet) returns nothing, and the caller scans records.
func (a *App) indexedRelationEntries(ctx context.Context, projectID, sourceID, targetID string) []relationEntry {
	index, ok := a.deps.Index.(ports.RelationIndex)
	if !ok {
		return nil
	}
	rows, err := index.Relations(ctx, ports.RelationQuery{ProjectID: projectID, SourceID: sourceID, TargetID: targetID})
	if err != nil {
		return nil
	}
	entries := make([]relationEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, relationEntry{
			source: RecordSummary{
				Kind: row.SourceKind, ID: row.SourceID, ProjectID: row.ProjectID, Title: row.SourceTitle,
			},
			relation: domain.Relation{
				Target: row.TargetID, Relation: domain.RelationKind(row.Relation),
				Reason: row.Reason, Confidence: row.Confidence,
				Status: domain.RelationStatus(row.Status), JudgedAt: row.JudgedAt,
				VerdictBy: domain.VerdictBy(row.VerdictBy),
			},
		})
	}
	return entries
}

// scanRelationEntries reads relations straight from the records. It is the
// fallback for a project whose derived index has no relation rows yet.
func scanRelationEntries(records []Record, sourceID, targetID string) []relationEntry {
	var entries []relationEntry
	for _, record := range records {
		summary := record.Summary()
		if sourceID != "" && summary.ID != sourceID {
			continue
		}
		for _, relation := range record.Relations() {
			normalized := relation.Normalize()
			if targetID != "" && normalized.Target != targetID {
				continue
			}
			entries = append(entries, relationEntry{source: summary, relation: normalized})
		}
	}
	return entries
}

func filterRelationEntries(entries []relationEntry, statuses []domain.RelationStatus, kinds []domain.RelationKind) []relationEntry {
	if len(statuses) == 0 && len(kinds) == 0 {
		return entries
	}
	statusSet := make(map[domain.RelationStatus]struct{}, len(statuses))
	for _, status := range statuses {
		statusSet[status] = struct{}{}
	}
	kindSet := make(map[domain.RelationKind]struct{}, len(kinds))
	for _, kind := range kinds {
		kindSet[kind] = struct{}{}
	}
	out := make([]relationEntry, 0, len(entries))
	for _, entry := range entries {
		if len(statusSet) > 0 {
			if _, ok := statusSet[entry.relation.Status]; !ok {
				continue
			}
		}
		if len(kindSet) > 0 {
			if _, ok := kindSet[entry.relation.Relation]; !ok {
				continue
			}
		}
		out = append(out, entry)
	}
	return out
}

// sortRelationViews orders pending relations first, then by source and target,
// so repeated listings and pages are deterministic.
func sortRelationViews(views []RelationView) {
	rank := func(status domain.RelationStatus) int {
		switch status {
		case domain.RelationPending:
			return 0
		case domain.RelationAccepted:
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(views, func(i, j int) bool {
		left, right := views[i], views[j]
		if rank(left.Relation.Status) != rank(right.Relation.Status) {
			return rank(left.Relation.Status) < rank(right.Relation.Status)
		}
		if left.Source.ID != right.Source.ID {
			return left.Source.ID < right.Source.ID
		}
		if left.TargetID != right.TargetID {
			return left.TargetID < right.TargetID
		}
		return left.Relation.Relation < right.Relation.Relation
	})
}

// tombstoneIDs lists the ids in a project's trash. A merged duplicate is a
// tombstone; a normally trashed record is indistinguishable here, and both are
// legitimate relation targets.
func (a *App) tombstoneIDs(ctx context.Context, projectID string) map[string]struct{} {
	editor, ok := a.deps.Store.(ports.RecordEditor)
	if !ok {
		return nil
	}
	trashed, err := editor.ListTrash(ctx, projectID)
	if err != nil {
		return nil
	}
	ids := make(map[string]struct{}, len(trashed))
	for _, item := range trashed {
		ids[item.ID] = struct{}{}
	}
	return ids
}

// JudgeOptions is one relation verdict.
type JudgeOptions struct {
	ProjectID string
	// From is the record that carries the relation.
	From string
	// To is the related record.
	To string
	// Verdict is the judged verb.
	Verdict domain.RelationKind
	// Reason is required: a verdict without a reason cannot be reviewed.
	Reason string
	// Confidence is the optional 0..1 judge confidence.
	Confidence float64
	// By names the judge; empty means human.
	By domain.VerdictBy
}

// JudgeResult reports the judged relation and whether anything changed.
type JudgeResult struct {
	Relation RelationView
	Changed  bool
}

// JudgeRelation records a verdict on the pair (from, to). It is the only way a
// relation changes status: the candidate row is replaced by one accepted
// verdict, the losing record is never touched, and the target's validity and
// status stay as they are. A supersede or depends_on verdict is refused when it
// would close a cycle.
func (a *App) JudgeRelation(ctx context.Context, opts JudgeOptions) (JudgeResult, error) {
	if err := a.requireStore(); err != nil {
		return JudgeResult{}, err
	}
	from := strings.TrimSpace(opts.From)
	to := strings.TrimSpace(opts.To)
	if err := domain.ValidateRecordReference("from", from); err != nil {
		return JudgeResult{}, err
	}
	if err := domain.ValidateRecordReference("to", to); err != nil {
		return JudgeResult{}, err
	}
	if from == to {
		return JudgeResult{}, domain.NewValidationError("to", "a record cannot be judged against itself")
	}
	verdict, err := domain.ParseRelationKind(string(opts.Verdict))
	if err != nil {
		return JudgeResult{}, err
	}
	reason := strings.TrimSpace(opts.Reason)
	if reason == "" {
		return JudgeResult{}, domain.NewValidationError("reason", "is required: a verdict must explain itself")
	}
	if math.IsNaN(opts.Confidence) || math.IsInf(opts.Confidence, 0) || opts.Confidence < 0 || opts.Confidence > 1 {
		return JudgeResult{}, domain.NewValidationError("confidence", "must be between 0 and 1")
	}
	by := opts.By
	if by == "" {
		by = domain.VerdictByHuman
	}
	if !by.Valid() {
		return JudgeResult{}, domain.NewValidationError("by", "must be human or agent")
	}
	setter, ok := a.deps.Store.(ports.RecordRelationSetter)
	if !ok {
		return JudgeResult{}, fmt.Errorf("%w: this store cannot change relations", domain.ErrUnsupported)
	}
	project, err := a.ensureProject(ctx, opts.ProjectID, false)
	if err != nil {
		return JudgeResult{}, err
	}
	records, err := a.projectRecords(ctx, project.ID)
	if err != nil {
		return JudgeResult{}, err
	}
	record, found := findRecordIn(records, from)
	if !found {
		return JudgeResult{}, fmt.Errorf("record %q in project %s: %w", from, project.ID, domain.ErrNotFound)
	}
	if _, found := findRecordIn(records, to); !found {
		if _, merged := a.tombstoneIDs(ctx, project.ID)[to]; !merged {
			return JudgeResult{}, fmt.Errorf("relation target %q in project %s: %w", to, project.ID, domain.ErrNotFound)
		}
	}
	judged := domain.Relation{
		Target: to, Relation: verdict, Reason: reason, Confidence: opts.Confidence,
		Status: domain.RelationAccepted, JudgedAt: a.now().UTC(), VerdictBy: by,
	}.Normalize()
	existing := record.Relations().ForTarget(to)
	if len(existing) == 1 && sameJudgedRelation(existing[0], judged) {
		view, err := a.relationView(ctx, project.ID, record.Summary(), judged)
		return JudgeResult{Relation: view, Changed: false}, err
	}
	relations := append(domain.RelationList(nil), record.Relations()...).Without(to)
	relations = append(relations, judged)
	if err := a.validateRelationCycles(ctx, records, record.Summary(), relations); err != nil {
		return JudgeResult{}, err
	}
	if err := judged.Validate("relation", from); err != nil {
		return JudgeResult{}, err
	}
	if err := relations.Validate("relations", from); err != nil {
		return JudgeResult{}, err
	}
	summary := record.Summary()
	changed, err := setter.SetRecordRelations(ctx, project.ID, summary.Kind, summary.ID, relations)
	if err != nil {
		return JudgeResult{}, err
	}
	updated := record.SetRelations(relations)
	indexErr := a.upsertIndex(ctx, recordDocument(updated))
	journalErr := a.journalWrite(ctx, JournalRelation, project.ID, summary.ID,
		fmt.Sprintf("%s %s: %s", summary.ID, verdict, SummarizeLine(reason, 70)))
	view, err := a.relationView(ctx, project.ID, summary, judged)
	if err != nil {
		return JudgeResult{Relation: view, Changed: changed}, err
	}
	if indexErr != nil {
		return JudgeResult{Relation: view, Changed: changed}, fmt.Errorf("%s was judged but the search index update failed: %w", summary.ID, indexErr)
	}
	if journalErr != nil {
		return JudgeResult{Relation: view, Changed: changed}, fmt.Errorf("%s was judged but the journal append failed: %w", summary.ID, journalErr)
	}
	return JudgeResult{Relation: view, Changed: changed}, nil
}

// sameJudgedRelation reports whether a stored relation is exactly the verdict
// being repeated, so judging twice writes nothing.
func sameJudgedRelation(stored, judged domain.Relation) bool {
	stored, judged = stored.Normalize(), judged.Normalize()
	return stored.Target == judged.Target &&
		stored.Relation == judged.Relation &&
		stored.Status == judged.Status &&
		stored.Reason == judged.Reason &&
		stored.Confidence == judged.Confidence &&
		stored.VerdictBy == judged.VerdictBy
}

// relationView builds the listing form of one relation of one record.
func (a *App) relationView(ctx context.Context, projectID string, source RecordSummary, relation domain.Relation) (RelationView, error) {
	view := RelationView{ProjectID: projectID, Source: source, TargetID: relation.Target, Relation: relation.Normalize()}
	if record, err := a.GetRecord(ctx, projectID, relation.Target); err == nil {
		target := record.Summary()
		view.Target = &target
		view.TargetState = RelationTargetFound
	} else if _, merged := a.tombstoneIDs(ctx, projectID)[relation.Target]; merged {
		view.TargetState = RelationTargetTombstone
	} else {
		view.TargetState = RelationTargetMissing
	}
	return view, nil
}

// findRecordIn returns the record with id.
func findRecordIn(records []Record, id string) (Record, bool) {
	for _, record := range records {
		if record.Summary().ID == id {
			return record, true
		}
	}
	return Record{}, false
}

// Relation accessors of a record.

// Relations returns the typed relation list of whichever record is set.
func (r Record) Relations() domain.RelationList {
	switch {
	case r.Note != nil:
		return r.Note.Relations
	case r.Decision != nil:
		return r.Decision.Relations
	case r.Attempt != nil:
		return r.Attempt.Relations
	case r.Handoff != nil:
		return r.Handoff.Relations
	}
	return nil
}

// MergeInfo returns the merge history of whichever record is set.
func (r Record) MergeInfo() domain.MergeInfo {
	switch {
	case r.Note != nil:
		return r.Note.MergeInfo
	case r.Decision != nil:
		return r.Decision.MergeInfo
	case r.Attempt != nil:
		return r.Attempt.MergeInfo
	case r.Handoff != nil:
		return r.Handoff.MergeInfo
	}
	return domain.MergeInfo{}
}

// SetRelations updates the in-memory record; it does not write.
func (r Record) SetRelations(relations domain.RelationList) Record {
	switch {
	case r.Note != nil:
		r.Note.Relations = relations
	case r.Decision != nil:
		r.Decision.Relations = relations
	case r.Attempt != nil:
		r.Attempt.Relations = relations
	case r.Handoff != nil:
		r.Handoff.Relations = relations
	}
	return r
}

// prepareRelations validates an explicit relation list at write time: every
// target must exist (or be a tombstone) and the entries must be coherent. It
// returns the normalized list. selfID is the pinned record id when the caller
// supplied one.
func (a *App) prepareRelations(ctx context.Context, projectID, selfID string, relations domain.RelationList) (domain.RelationList, error) {
	relations = relations.Normalize()
	if len(relations) == 0 {
		return nil, nil
	}
	records, err := a.projectRecords(ctx, projectID)
	if err != nil {
		return nil, err
	}
	tombstones := a.tombstoneIDs(ctx, projectID)
	for i, relation := range relations {
		if err := relation.Validate(fmt.Sprintf("relations[%d]", i), selfID); err != nil {
			return nil, err
		}
		if _, found := findRecordIn(records, relation.Target); found {
			continue
		}
		if _, merged := tombstones[relation.Target]; merged {
			continue
		}
		return nil, fmt.Errorf("relation target %q in project %s: %w", relation.Target, projectID, domain.ErrNotFound)
	}
	if err := relations.Validate("relations", selfID); err != nil {
		return nil, err
	}
	edges := relationGraph(records)
	if selfID != "" {
		edges[selfID] = relations
	}
	if err := domain.ValidateRelationGraph(edges); err != nil {
		return nil, err
	}
	return relations, nil
}

// validateRelationCycles re-checks the graph for one record's new list.
func (a *App) validateRelationCycles(ctx context.Context, records []Record, source RecordSummary, relations domain.RelationList) error {
	edges := relationGraph(records)
	edges[source.ID] = relations
	return domain.ValidateRelationGraph(edges)
}

// relationGraph maps every live record id to its relation list.
func relationGraph(records []Record) map[string]domain.RelationList {
	edges := make(map[string]domain.RelationList, len(records))
	for _, record := range records {
		edges[record.Summary().ID] = record.Relations()
	}
	return edges
}

// candidateRelations proposes pending relates_to links for a record that is
// about to be written. Candidates are computed before the write, so the record
// is published once with its pending relations and a save never pays for a
// second rewrite. A candidate is advisory: it is always pending, never an
// accepted verdict, and never merges anything.
func (a *App) candidateRelations(ctx context.Context, projectID string, record Record, existing domain.RelationList) domain.RelationList {
	candidates := a.relationCandidates(ctx, projectID, record)
	if len(candidates) == 0 {
		return nil
	}
	var out domain.RelationList
	for _, candidate := range candidates {
		if len(existing.ForTarget(candidate.id)) > 0 {
			continue
		}
		out = append(out, domain.Relation{
			Target: candidate.id, Relation: domain.RelationRelatesTo, Reason: candidate.reason,
			Confidence: candidate.score, Status: domain.RelationPending,
		})
	}
	return out
}

// relationCandidate is one proposed pending relation.
type relationCandidate struct {
	id     string
	score  float64
	reason string
}

// relationCandidates scores the newer record against the live records of the
// same kind and project and returns at most relationCandidateLimit of the most
// similar ones.
func (a *App) relationCandidates(ctx context.Context, projectID string, record Record) []relationCandidate {
	var prior []dupRecord
	var current dupRecord
	found := false
	switch {
	case record.Note != nil:
		notes, err := a.listNotes(ctx, projectID)
		if err != nil {
			return nil
		}
		prior = noteDupRecords(liveNotes(notes, record.Note.ID))
		current, found = relationDupRecord(record)
	case record.Decision != nil:
		decisions, err := a.listDecisions(ctx, projectID)
		if err != nil {
			return nil
		}
		prior = decisionDupRecords(liveDecisions(decisions, record.Decision.ID))
		current, found = relationDupRecord(record)
	case record.Attempt != nil:
		attempts, err := a.listAttempts(ctx, projectID)
		if err != nil {
			return nil
		}
		prior = attemptDupRecords(liveAttempts(attempts, record.Attempt.ID))
		current, found = relationDupRecord(record)
	case record.Handoff != nil:
		handoffs, err := a.listHandoffs(ctx, projectID)
		if err != nil {
			return nil
		}
		prior = handoffDupRecords(liveHandoffs(handoffs, record.Handoff.ID))
		current, found = relationDupRecord(record)
	}
	if !found {
		return nil
	}
	var candidates []relationCandidate
	for _, other := range prior {
		score, ok := relationSimilarity(current, other)
		if !ok {
			continue
		}
		candidates = append(candidates, relationCandidate{
			id: other.id, score: roundRelationScore(score), reason: relationCandidateReason(score),
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return candidates[i].id < candidates[j].id
	})
	if len(candidates) > relationCandidateLimit {
		candidates = candidates[:relationCandidateLimit]
	}
	return candidates
}

// relationDupRecord prepares one record for the similarity comparison, using the
// same text rules the near-duplicate detector uses.
func relationDupRecord(record Record) (dupRecord, bool) {
	switch {
	case record.Note != nil:
		records := noteDupRecords([]domain.Note{*record.Note})
		return records[0], true
	case record.Decision != nil:
		records := decisionDupRecords([]domain.Decision{*record.Decision})
		if len(records) == 1 {
			return records[0], true
		}
	case record.Attempt != nil:
		records := attemptDupRecords([]domain.Attempt{*record.Attempt})
		return records[0], true
	case record.Handoff != nil:
		records := handoffDupRecords([]domain.Handoff{*record.Handoff})
		return records[0], true
	}
	return dupRecord{}, false
}

// relationSimilarity scores two prepared records for candidate detection. It
// is looser than the near-duplicate threshold: a shingle overlap of
// relationCandidateThreshold is enough, and a close simhash sketch counts as a
// strong candidate.
func relationSimilarity(a, b dupRecord) (float64, bool) {
	if a.text == "" || b.text == "" || a.id == b.id {
		return 0, false
	}
	if a.fingerprint == b.fingerprint {
		return 1, true
	}
	score := 0.0
	if len(a.shingles) >= duplicateMinShingles && len(b.shingles) >= duplicateMinShingles {
		score = jaccard(a.shingles, b.shingles)
	}
	if a.title != "" && a.title == b.title && len(a.bodyShing) >= duplicateMinShingles && len(b.bodyShing) >= duplicateMinShingles {
		if bodyScore := jaccard(a.bodyShing, b.bodyShing); bodyScore > score {
			score = bodyScore
		}
	}
	if a.words >= domain.SimhashMinWords && b.words >= domain.SimhashMinWords {
		if domain.HammingDistance(a.simhash, b.simhash) <= domain.NearDuplicateHammingDistance && score < 0.9 {
			score = 0.9
		}
	}
	threshold := relationCandidateThreshold
	if a.auto || b.auto {
		threshold = relationCandidateAutoThreshold
	}
	return score, score >= threshold
}

func roundRelationScore(score float64) float64 {
	return math.Round(score*100) / 100
}

func relationCandidateReason(score float64) string {
	if score >= 0.999 {
		return "identical normalized text"
	}
	return "similar text (" + strconv.Itoa(int(math.Round(score*100))) + "% shingle overlap)"
}

// liveNotes drops the record being compared and records whose validity ended.
func liveNotes(notes []domain.Note, excludeID string) []domain.Note {
	out := make([]domain.Note, 0, len(notes))
	for _, note := range notes {
		if note.ID == excludeID || !note.Validity.IsCurrent() {
			continue
		}
		out = append(out, note)
	}
	return out
}

func liveDecisions(decisions []domain.Decision, excludeID string) []domain.Decision {
	out := make([]domain.Decision, 0, len(decisions))
	for _, decision := range decisions {
		if decision.ID == excludeID || !decision.Validity.IsCurrent() {
			continue
		}
		if decision.Status == domain.DecisionRejected || decision.Status == domain.DecisionSuperseded {
			continue
		}
		out = append(out, decision)
	}
	return out
}

func liveAttempts(attempts []domain.Attempt, excludeID string) []domain.Attempt {
	out := make([]domain.Attempt, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.ID == excludeID || !attempt.Validity.IsCurrent() {
			continue
		}
		out = append(out, attempt)
	}
	return out
}

func liveHandoffs(handoffs []domain.Handoff, excludeID string) []domain.Handoff {
	out := make([]domain.Handoff, 0, len(handoffs))
	for _, handoff := range handoffs {
		if handoff.ID == excludeID || !handoff.Validity.IsCurrent() {
			continue
		}
		out = append(out, handoff)
	}
	return out
}

// relationLinks projects the relation list for the search index.
func relationLinks(relations domain.RelationList) []ports.RelationLink {
	normalized := relations.Normalize()
	if len(normalized) == 0 {
		return nil
	}
	out := make([]ports.RelationLink, 0, len(normalized))
	for _, relation := range normalized {
		out = append(out, ports.RelationLink{
			Target: relation.Target, Relation: string(relation.Relation), Status: string(relation.Status),
			Confidence: relation.Confidence, Reason: relation.Reason,
			JudgedAt: relation.JudgedAt, VerdictBy: string(relation.VerdictBy),
		})
	}
	return out
}

// ParseRelationSpec parses the CLI form "<target>:<relation>[:<reason>]" into
// an explicit, accepted relation. The reason may contain colons; the target and
// verb never do.
func ParseRelationSpec(value string, by domain.VerdictBy, now time.Time) (domain.Relation, error) {
	parts := strings.SplitN(value, ":", 3)
	if len(parts) < 2 {
		return domain.Relation{}, domain.NewValidationError("relation",
			fmt.Sprintf("%q: want <target>:<relation>[:<reason>]", value))
	}
	target := strings.TrimSpace(parts[0])
	kind, err := domain.ParseRelationKind(parts[1])
	if err != nil {
		return domain.Relation{}, err
	}
	if err := domain.ValidateRecordReference("relation.target", target); err != nil {
		return domain.Relation{}, err
	}
	reason := ""
	if len(parts) == 3 {
		reason = strings.TrimSpace(parts[2])
	}
	return domain.Relation{
		Target: target, Relation: kind, Reason: reason,
		Status: domain.RelationAccepted, JudgedAt: now.UTC(), VerdictBy: by,
	}.Normalize(), nil
}

// ParseRelationSpecs parses a repeated --relation value.
func ParseRelationSpecs(values []string, by domain.VerdictBy, now time.Time) (domain.RelationList, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(domain.RelationList, 0, len(values))
	for _, value := range values {
		relation, err := ParseRelationSpec(value, by, now)
		if err != nil {
			return nil, err
		}
		out = out.With(relation)
	}
	return out, nil
}
