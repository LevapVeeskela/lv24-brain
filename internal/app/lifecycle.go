package app

import (
	"context"
	"errors"

	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Journal events of the record lifecycle.
const (
	JournalStatus  = "status"
	JournalTrash   = "trash"
	JournalRestore = "restore"
)

// RecordSummary is one record in a listing.
type RecordSummary struct {
	Kind      string
	ID        string
	ProjectID string
	Title     string
	// Status is set for decisions only.
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
	// ContentUpdatedAt is the last title or body change; a --since window
	// compares it, so a metadata write does not re-report the record.
	ContentUpdatedAt time.Time
	// Validity is the record's temporal validity and supersede links.
	ValidFrom    time.Time
	ValidTo      time.Time
	SupersededBy string
	Supersedes   string
	Confidence   float64
	// Subtype, Scope, TopicKey and Occurrences are the engram-compatible
	// classification of the record.
	Subtype     string
	Scope       string
	TopicKey    string
	Occurrences int
	// Provenance is the authorship, review state and trust of the record.
	Provenance domain.Provenance
	// Tags are the note tags. They are empty for every other kind.
	Tags []string
	// Permalink is the canonical path the record was written with, when any.
	Permalink string
	// Pinned is the explicit pin flag set by the owner.
	Pinned bool
	// ReviewAfter and ReviewSource hold the effective review date: the
	// record's frontmatter date ("frontmatter") or its kind default counted
	// from creation ("default"). Both are empty when the kind has no review
	// window. ListRecords fills them because the window comes from config.
	ReviewAfter  time.Time
	ReviewSource string
}

// Provenance returns the provenance of whichever record is set.
func (r Record) Provenance() domain.Provenance {
	switch {
	case r.Note != nil:
		return r.Note.Provenance
	case r.Decision != nil:
		return r.Decision.Provenance
	case r.Attempt != nil:
		return r.Attempt.Provenance
	case r.Handoff != nil:
		return r.Handoff.Provenance
	case r.Idea != nil:
		return r.Idea.Provenance
	}
	return domain.Provenance{}
}

// Permalink returns the canonical permalink of whichever record is set. An
// idea has none: its globally unique id is the stable reference.
func (r Record) Permalink() string {
	switch {
	case r.Note != nil:
		return r.Note.Permalink
	case r.Decision != nil:
		return r.Decision.Permalink
	case r.Attempt != nil:
		return r.Attempt.Permalink
	case r.Handoff != nil:
		return r.Handoff.Permalink
	}
	return ""
}

func (r Record) Pinned() bool {
	switch {
	case r.Note != nil:
		return r.Note.Pinned
	case r.Decision != nil:
		return r.Decision.Pinned
	case r.Attempt != nil:
		return r.Attempt.Pinned
	case r.Handoff != nil:
		return r.Handoff.Pinned
	}
	return false
}

// ListRecordsOptions filters ListRecords.
type ListRecordsOptions struct {
	ProjectID string
	// Kinds keeps only these record kinds; empty lists every kind.
	Kinds []string
	// Since keeps records whose content changed at or after it. A metadata-only
	// write (review, snooze, validity) does not make a record recent.
	Since time.Time
	// Status keeps decisions with this status (and drops other kinds).
	Status string
	// Subtype, Scope and TopicKey keep only records with that classification.
	// Empty keeps every value.
	Subtype  string
	Scope    string
	TopicKey string
	// Limit bounds the result; <= 0 uses 50.
	Limit int
	// AsOf lists the records valid at that instant instead of now.
	AsOf time.Time
	// IncludeSuperseded lists records whose validity ended too. An AsOf
	// instant selects the point in time and takes precedence.
	IncludeSuperseded bool
	// IncludeInvalidated is the archive spelling of IncludeSuperseded: it
	// keeps records ended by an approved supersede or an explicit
	// invalidation. Either flag opens the archive.
	IncludeInvalidated bool
}

// Record is one record of any kind; exactly one pointer is set.
type Record struct {
	Kind     string
	Note     *domain.Note
	Decision *domain.Decision
	Attempt  *domain.Attempt
	Handoff  *domain.Handoff
	// Idea is set only by the idea read path (`brain idea ...` and a
	// `brain record show` of an idea id). Knowledge retrieval never loads
	// ideas, so a Record returned by the context builder cannot be an idea.
	Idea *domain.Idea
}

// Summary returns the listing form of a record.
func (r Record) Summary() RecordSummary {
	var (
		summary RecordSummary
		valid   domain.Validity
	)
	switch {
	case r.Note != nil:
		summary = RecordSummary{Kind: ports.RecordNote, ID: r.Note.ID, ProjectID: r.Note.ProjectID,
			Title: SummarizeLine(valueOr(r.Note.Title, r.Note.Body), 100), CreatedAt: r.Note.CreatedAt, UpdatedAt: r.Note.UpdatedAt}
		valid = r.Note.Validity
	case r.Decision != nil:
		summary = RecordSummary{Kind: ports.RecordDecision, ID: r.Decision.ID, ProjectID: r.Decision.ProjectID,
			Title: SummarizeLine(r.Decision.Title, 100), Status: string(r.Decision.Status), CreatedAt: r.Decision.CreatedAt, UpdatedAt: r.Decision.UpdatedAt}
		valid = r.Decision.Validity
	case r.Attempt != nil:
		summary = RecordSummary{Kind: ports.RecordAttempt, ID: r.Attempt.ID, ProjectID: r.Attempt.ProjectID,
			Title: SummarizeLine(r.Attempt.Title, 100), CreatedAt: r.Attempt.CreatedAt, UpdatedAt: r.Attempt.UpdatedAt}
		valid = r.Attempt.Validity
	case r.Handoff != nil:
		summary = RecordSummary{Kind: ports.RecordHandoff, ID: r.Handoff.ID, ProjectID: r.Handoff.ProjectID,
			Title: SummarizeLine(r.Handoff.Summary, 100), CreatedAt: r.Handoff.CreatedAt,
			UpdatedAt: updatedOrCreated(r.Handoff.UpdatedAt, r.Handoff.CreatedAt)}
		valid = r.Handoff.Validity
	case r.Idea != nil:
		summary = RecordSummary{Kind: ports.RecordIdea, ID: r.Idea.ID, ProjectID: r.Idea.ProjectID,
			Title: SummarizeLine(r.Idea.Title, 100), Status: string(r.Idea.Status),
			CreatedAt: r.Idea.CreatedAt, UpdatedAt: updatedOrCreated(r.Idea.UpdatedAt, r.Idea.CreatedAt)}
		valid = r.Idea.Validity
	}
	summary.ValidFrom = valid.EffectiveFrom(summary.CreatedAt)
	summary.ValidTo = valid.ValidTo
	summary.SupersededBy = valid.SupersededBy
	summary.Supersedes = valid.Supersedes
	summary.Confidence = valid.Confidence
	summary.ContentUpdatedAt = contentUpdatedOr(recordContentUpdatedAt(r), summary.UpdatedAt)
	summary.Subtype, summary.Scope, summary.TopicKey, summary.Occurrences = taxonomyOf(r)
	summary.Provenance = r.Provenance()
	summary.Permalink = r.Permalink()
	summary.Pinned = r.Pinned()
	if r.Note != nil {
		summary.Tags = append([]string(nil), r.Note.Tags...)
	}
	return summary
}

// recordContentUpdatedAt returns the content edit time of whichever record is
// set, zero when the stored file did not carry one.
func recordContentUpdatedAt(record Record) time.Time {
	switch {
	case record.Note != nil:
		return record.Note.ContentUpdatedAt
	case record.Decision != nil:
		return record.Decision.ContentUpdatedAt
	case record.Attempt != nil:
		return record.Attempt.ContentUpdatedAt
	case record.Handoff != nil:
		return record.Handoff.ContentUpdatedAt
	case record.Idea != nil:
		// Ideas do not write content_updated_at: updated_at is the revision
		// of the whole record.
		return record.Idea.UpdatedAt
	}
	return time.Time{}
}

// taxonomyOf returns the classification of whichever record is set.
func taxonomyOf(record Record) (string, string, string, int) {
	switch {
	case record.Note != nil:
		taxonomy := record.Note.Taxonomy
		return taxonomy.Subtype, string(taxonomy.Scope.Normalized()), taxonomy.TopicKey, taxonomy.NormalizedOccurrences()
	case record.Decision != nil:
		taxonomy := record.Decision.Taxonomy
		return taxonomy.Subtype, string(taxonomy.Scope.Normalized()), taxonomy.TopicKey, taxonomy.NormalizedOccurrences()
	case record.Attempt != nil:
		taxonomy := record.Attempt.Taxonomy
		return taxonomy.Subtype, string(taxonomy.Scope.Normalized()), taxonomy.TopicKey, taxonomy.NormalizedOccurrences()
	case record.Handoff != nil:
		taxonomy := record.Handoff.Taxonomy
		return taxonomy.Subtype, string(taxonomy.Scope.Normalized()), taxonomy.TopicKey, taxonomy.NormalizedOccurrences()
	case record.Idea != nil:
		taxonomy := record.Idea.Taxonomy
		return taxonomy.Subtype, string(taxonomy.Scope.Normalized()), taxonomy.TopicKey, taxonomy.NormalizedOccurrences()
	}
	return "", "", "", 1
}

// Validity returns the validity of whichever record is set.
func (r Record) Validity() domain.Validity {
	switch {
	case r.Note != nil:
		return r.Note.Validity
	case r.Decision != nil:
		return r.Decision.Validity
	case r.Attempt != nil:
		return r.Attempt.Validity
	case r.Handoff != nil:
		return r.Handoff.Validity
	case r.Idea != nil:
		return r.Idea.Validity
	}
	return domain.Validity{}
}

// SetValidity updates the in-memory record; it does not write.
func (r Record) SetValidity(validity domain.Validity) Record {
	switch {
	case r.Note != nil:
		r.Note.Validity = validity
	case r.Decision != nil:
		r.Decision.Validity = validity
	case r.Attempt != nil:
		r.Attempt.Validity = validity
	case r.Handoff != nil:
		r.Handoff.Validity = validity
	case r.Idea != nil:
		r.Idea.Validity = validity
	}
	return r
}

// projectRecords loads every live record of a project.
func (a *App) projectRecords(ctx context.Context, projectID string) ([]Record, error) {
	var records []Record
	notes, err := a.listNotes(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range notes {
		records = append(records, Record{Kind: ports.RecordNote, Note: &notes[i]})
	}
	decisions, err := a.listDecisions(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range decisions {
		records = append(records, Record{Kind: ports.RecordDecision, Decision: &decisions[i]})
	}
	attempts, err := a.listAttempts(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range attempts {
		records = append(records, Record{Kind: ports.RecordAttempt, Attempt: &attempts[i]})
	}
	handoffs, err := a.listHandoffs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range handoffs {
		records = append(records, Record{Kind: ports.RecordHandoff, Handoff: &handoffs[i]})
	}
	return records, nil
}

// ListRecords lists a project's records newest first.
func (a *App) ListRecords(ctx context.Context, opts ListRecordsOptions) ([]RecordSummary, error) {
	if err := a.requireStore(); err != nil {
		return nil, err
	}
	project, err := a.ensureProject(ctx, opts.ProjectID, false)
	if err != nil {
		return nil, err
	}
	for _, kind := range opts.Kinds {
		if err := validateRecordKind(kind); err != nil {
			return nil, err
		}
	}
	subtype, scope, topic, err := validateClassificationFilters(opts.Subtype, opts.Scope, opts.TopicKey)
	if err != nil {
		return nil, err
	}
	opts.Subtype, opts.Scope, opts.TopicKey = subtype, scope, topic
	records, err := a.projectRecords(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	if kindsAllowIdeas(opts.Kinds) {
		ideas, ideaErr := a.projectIdeas(ctx, project.ID)
		if ideaErr != nil {
			return nil, ideaErr
		}
		for i := range ideas {
			records = append(records, Record{Kind: ports.RecordIdea, Idea: &ideas[i]})
		}
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	filter := archiveFilter(opts.AsOf, opts.IncludeSuperseded || opts.IncludeInvalidated)
	now := a.now()
	var out []RecordSummary
	for _, record := range records {
		summary := a.Summarize(record)
		if len(opts.Kinds) > 0 && !containsString(opts.Kinds, summary.Kind) {
			continue
		}
		if opts.Status != "" && (summary.Kind != ports.RecordDecision || !strings.EqualFold(summary.Status, opts.Status)) {
			continue
		}
		if opts.Subtype != "" && summary.Subtype != strings.TrimSpace(opts.Subtype) {
			continue
		}
		if opts.Scope != "" && summary.Scope != string(domain.RecordScope(opts.Scope).Normalized()) {
			continue
		}
		if opts.TopicKey != "" && summary.TopicKey != strings.TrimSpace(opts.TopicKey) {
			continue
		}
		validity := domain.Validity{ValidFrom: summary.ValidFrom, ValidTo: summary.ValidTo}
		if !filter.Allows(validity, summary.CreatedAt, now) {
			continue
		}
		if !withinSince(summary.ContentUpdatedAt, summary.UpdatedAt, summary.CreatedAt, opts.Since) {
			continue
		}
		out = append(out, summary)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// RecordsByIDs loads the records with these ids with one listing of the
// project, keyed by id. It is the batch form of GetRecord: a caller that needs
// several records of one project (the context link layer) must not pay one full
// listing per record. Unknown ids and ids of another kind are absent from the
// result, exactly like a failed GetRecord.
func (a *App) RecordsByIDs(ctx context.Context, projectID string, ids []string) (map[string]Record, error) {
	if err := a.requireStore(); err != nil {
		return nil, err
	}
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return map[string]Record{}, nil
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return nil, err
	}
	records, err := a.projectRecords(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Record, len(wanted))
	for _, record := range records {
		id := record.Summary().ID
		if _, ok := wanted[id]; ok {
			out[id] = record
		}
	}
	return out, nil
}

// GetRecord finds one live record of a project by ID.
func (a *App) GetRecord(ctx context.Context, projectID, id string) (Record, error) {
	if err := a.requireStore(); err != nil {
		return Record{}, err
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return Record{}, err
	}
	id = strings.TrimSpace(id)
	records, err := a.projectRecords(ctx, project.ID)
	if err != nil {
		return Record{}, err
	}
	for _, record := range records {
		if record.Summary().ID == id {
			return record, nil
		}
	}
	if strings.HasPrefix(id, ports.RecordIdea+"-") {
		// Ideas are not part of projectRecords (knowledge retrieval must stay
		// free of them), but `brain record show <idea-id>` reads one.
		idea, ideaErr := a.GetIdea(ctx, id)
		if ideaErr == nil && idea.ProjectID == project.ID {
			return Record{Kind: ports.RecordIdea, Idea: &idea}, nil
		}
		if ideaErr != nil && !errors.Is(ideaErr, domain.ErrNotFound) && !errors.Is(ideaErr, domain.ErrUnsupported) {
			return Record{}, ideaErr
		}
	}
	return Record{}, fmt.Errorf("record %q in project %s: %w", id, project.ID, domain.ErrNotFound)
}

func (a *App) recordEditor() (ports.RecordEditor, error) {
	editor, ok := a.deps.Store.(ports.RecordEditor)
	if !ok {
		return nil, fmt.Errorf("%w: changing records is not supported by this store yet", domain.ErrUnsupported)
	}
	return editor, nil
}

// SetDecisionStatus changes a decision's status (proposed, accepted,
// rejected, superseded). Accepting or rejecting a decision also settles the
// supersede case that names it as the successor: acceptance ends the
// predecessor at the approved tuple, rejection refuses the case and leaves the
// predecessor untouched.
func (a *App) SetDecisionStatus(ctx context.Context, projectID, id string, status domain.DecisionStatus) (domain.Decision, error) {
	decision, err := a.updateDecision(ctx, projectID, id, func(decision *domain.Decision) error {
		if !status.Valid() {
			return domain.NewValidationError("status", fmt.Sprintf("unknown decision status %q", status))
		}
		decision.Status = status
		return nil
	})
	if err != nil {
		return domain.Decision{}, err
	}
	if err := a.settleSupersedeCases(ctx, decision.ProjectID, decision.ID, status); err != nil {
		return decision, err
	}
	return decision, nil
}

// SetRecordPinned sets or clears the pinned flag of one record of any kind.
func (a *App) SetRecordPinned(ctx context.Context, projectID, id string, pinned bool) (Record, error) {
	if err := a.requireStore(); err != nil {
		return Record{}, err
	}
	pinner, ok := a.deps.Store.(ports.RecordPinner)
	if !ok {
		return Record{}, fmt.Errorf("%w: pinning records is not supported by this store", domain.ErrUnsupported)
	}
	record, err := a.GetRecord(ctx, projectID, id)
	if err != nil {
		return Record{}, err
	}
	if record.Idea != nil {
		return Record{}, domain.NewValidationError("id",
			fmt.Sprintf("idea %s is not pinned; ideas have no pin flag", record.Idea.ID))
	}
	summary := record.Summary()
	if _, err := pinner.SetRecordPinned(ctx, summary.ProjectID, summary.Kind, summary.ID, pinned); err != nil {
		return Record{}, err
	}
	switch {
	case record.Note != nil:
		record.Note.Pinned = pinned
	case record.Decision != nil:
		record.Decision.Pinned = pinned
	case record.Attempt != nil:
		record.Attempt.Pinned = pinned
	case record.Handoff != nil:
		record.Handoff.Pinned = pinned
	}
	switch {
	case record.Note != nil:
		if err := a.upsertIndex(ctx, noteDocument(*record.Note)); err != nil {
			return record, fmt.Errorf("record %s was pinned but the search index update failed: %w", record.Summary().ID, err)
		}
	case record.Decision != nil:
		if err := a.upsertIndex(ctx, decisionDocument(*record.Decision)); err != nil {
			return record, fmt.Errorf("record %s was pinned but the search index update failed: %w", record.Summary().ID, err)
		}
	case record.Attempt != nil:
		if err := a.upsertIndex(ctx, attemptDocument(*record.Attempt)); err != nil {
			return record, fmt.Errorf("record %s was pinned but the search index update failed: %w", record.Summary().ID, err)
		}
	case record.Handoff != nil:
		if err := a.upsertIndex(ctx, handoffDocument(*record.Handoff)); err != nil {
			return record, fmt.Errorf("record %s was pinned but the search index update failed: %w", record.Summary().ID, err)
		}
	}
	return record, nil
}

// PinRecord is a convenience wrapper.
func (a *App) PinRecord(ctx context.Context, projectID, id string) (Record, error) {
	return a.SetRecordPinned(ctx, projectID, id, true)
}

// UnpinRecord is a convenience wrapper.
func (a *App) UnpinRecord(ctx context.Context, projectID, id string) (Record, error) {
	return a.SetRecordPinned(ctx, projectID, id, false)
}

// Supersede is implemented in supersede.go: it links any two records of a
// project and keeps both on disk.

func (a *App) updateDecision(ctx context.Context, projectID, id string, change func(*domain.Decision) error) (domain.Decision, error) {
	// Server clients can update decisions but have no trash, so this needs
	// only the narrower interface.
	editor, ok := a.deps.Store.(ports.DecisionUpdater)
	if !ok {
		return domain.Decision{}, fmt.Errorf("%w: changing decisions is not supported by this store", domain.ErrUnsupported)
	}
	record, err := a.GetRecord(ctx, projectID, id)
	if err != nil {
		return domain.Decision{}, err
	}
	if record.Decision == nil {
		return domain.Decision{}, domain.NewValidationError("id", fmt.Sprintf("%q is a %s, not a decision", id, record.Kind))
	}
	decision := *record.Decision
	if err := change(&decision); err != nil {
		return domain.Decision{}, err
	}
	if err := a.enforceUpdateLimits(decision.ProjectID, ports.RecordDecision, decision.ID, decisionWriteContent(decision)); err != nil {
		return domain.Decision{}, err
	}
	decision.UpdatedAt = a.now().UTC()
	if err := editor.UpdateDecision(ctx, decision); err != nil {
		return domain.Decision{}, err
	}
	journalErr := a.journalWrite(ctx, JournalStatus, decision.ProjectID, decision.ID, string(decision.Status)+": "+SummarizeLine(decision.Title, 70))
	if err := a.upsertIndex(ctx, decisionDocument(decision)); err != nil {
		return decision, fmt.Errorf("decision %s was updated but the search index update failed: %w", decision.ID, err)
	}
	if journalErr != nil {
		return decision, fmt.Errorf("decision %s was updated but the journal append failed: %w", decision.ID, journalErr)
	}
	return decision, nil
}

// TrashRecord moves a record to its project's trash (reversible with
// RestoreRecord) and drops it from the search index.
func (a *App) TrashRecord(ctx context.Context, projectID, id string) (Record, error) {
	editor, err := a.recordEditor()
	if err != nil {
		return Record{}, err
	}
	record, err := a.GetRecord(ctx, projectID, id)
	if err != nil {
		return Record{}, err
	}
	if record.Idea != nil {
		return Record{}, ideaLifecycleError("trash")
	}
	summary := record.Summary()
	if err := editor.TrashRecord(ctx, summary.ProjectID, summary.Kind, summary.ID); err != nil {
		if summary.Kind == ports.RecordNote && errors.Is(err, domain.ErrNotFound) {
			// Listed but not addressable: a hand-written section without an
			// id marker (its id is derived from the file date).
			return Record{}, domain.NewValidationError("id", fmt.Sprintf("note %s is a hand-written section without an id marker; edit projects/%s/notes/ by hand", summary.ID, summary.ProjectID))
		}
		return Record{}, err
	}
	journalErr := a.journalWrite(ctx, JournalTrash, summary.ProjectID, summary.ID, summary.Kind+": "+summary.Title)
	if a.deps.Index != nil {
		if err := a.deps.Index.Delete(ctx, indexKey(summary.ProjectID, summary.ID)); err != nil {
			a.markIndexDirty()
			return record, fmt.Errorf("%s %s was trashed but the search index update failed: %w", summary.Kind, summary.ID, err)
		}
	}
	if journalErr != nil {
		return record, fmt.Errorf("%s %s was trashed but the journal append failed: %w", summary.Kind, summary.ID, journalErr)
	}
	return record, nil
}

// RestoreRecord brings a trashed record back.
func (a *App) RestoreRecord(ctx context.Context, projectID, id string) (Record, error) {
	editor, err := a.recordEditor()
	if err != nil {
		return Record{}, err
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return Record{}, err
	}
	trashed, err := editor.ListTrash(ctx, project.ID)
	if err != nil {
		return Record{}, err
	}
	id = strings.TrimSpace(id)
	if domain.IsRecordID(ports.RecordIdea, id) {
		return Record{}, ideaLifecycleError("restore")
	}
	kind := ""
	for _, item := range trashed {
		if item.ID == id {
			kind = item.Kind
			break
		}
	}
	if kind == "" {
		return Record{}, fmt.Errorf("trashed record %q in project %s: %w", id, project.ID, domain.ErrNotFound)
	}
	if err := editor.RestoreRecord(ctx, project.ID, kind, id); err != nil {
		return Record{}, err
	}
	record, err := a.GetRecord(ctx, project.ID, id)
	if err != nil {
		return Record{}, err
	}
	summary := record.Summary()
	journalErr := a.journalWrite(ctx, JournalRestore, project.ID, id, kind+": "+summary.Title)
	if err := a.upsertIndex(ctx, recordDocument(record)); err != nil {
		return record, fmt.Errorf("%s %s was restored but the search index update failed: %w", kind, id, err)
	}
	if journalErr != nil {
		return record, fmt.Errorf("%s %s was restored but the journal append failed: %w", kind, id, journalErr)
	}
	return record, nil
}

// ListTrash lists a project's trashed records.
func (a *App) ListTrash(ctx context.Context, projectID string) ([]ports.TrashedRecord, error) {
	editor, err := a.recordEditor()
	if err != nil {
		return nil, err
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return nil, err
	}
	return editor.ListTrash(ctx, project.ID)
}

func recordDocument(record Record) ports.Document {
	switch {
	case record.Note != nil:
		return noteDocument(*record.Note)
	case record.Decision != nil:
		return decisionDocument(*record.Decision)
	case record.Attempt != nil:
		return attemptDocument(*record.Attempt)
	case record.Handoff != nil:
		return handoffDocument(*record.Handoff)
	default:
		// The ideas index projection lands with WS3. In 0.28.0 no write path
		// reaches this case with an idea: trash, restore, pin and edit refuse
		// ideas, and AddIdea/UpdateIdea use their own hook.
		return ports.Document{}
	}
}

// kindsAllowIdeas reports whether a record listing asked for ideas. An empty
// kind list means every kind, including idea.
func kindsAllowIdeas(kinds []string) bool {
	return len(kinds) == 0 || containsString(kinds, ports.RecordIdea)
}

// ideaLifecycleError refuses an idea in a generic record lifecycle command.
// Only `brain idea rm|restore` may move one.
func ideaLifecycleError(action string) error {
	return domain.NewValidationError("kind",
		fmt.Sprintf("an idea is not handled by brain record %s; use brain idea %s", action, action))
}

func validateRecordKind(kind string) error {
	switch kind {
	case ports.RecordNote, ports.RecordDecision, ports.RecordAttempt, ports.RecordHandoff, ports.RecordIdea:
		return nil
	}
	return domain.NewValidationError("kind", fmt.Sprintf("unknown record kind %q (note, decision, attempt, handoff, idea)", kind))
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// KnowledgeTier classifies what a project actually contains, so an agent can
// tell "nothing is known here" from "the project is known but still empty".
type KnowledgeTier string

const (
	// TierAbsent means the id is not registered here: there are no records and
	// no state to report.
	TierAbsent KnowledgeTier = "ABSENT"
	// TierEmpty means the project is registered but has no records, no state
	// content and no handoff.
	TierEmpty KnowledgeTier = "EMPTY"
	// TierPopulated means at least one record, the state or a handoff exists.
	TierPopulated KnowledgeTier = "POPULATED"
)

// ProjectKnowledge reports what is stored for one project. The counts are the
// evidence behind the tier; the tier is never inferred from the search index,
// which may be stale or missing.
type ProjectKnowledge struct {
	ProjectID string
	Tier      KnowledgeTier
	Notes     int
	Decisions int
	Attempts  int
	Handoffs  int
	// HasState reports state content (a goal, a next action or a list), not
	// just the state file a newly registered project gets.
	HasState bool
}

// Records returns the number of live records of every kind.
func (k ProjectKnowledge) Records() int {
	return k.Notes + k.Decisions + k.Attempts + k.Handoffs
}

// Knowledge reports the tier of a project. An id that is not registered is
// ABSENT, not an error: `brain status` must be able to say that nothing is
// known for a directory without failing.
func (a *App) Knowledge(ctx context.Context, projectID string) (ProjectKnowledge, error) {
	if err := a.requireStore(); err != nil {
		return ProjectKnowledge{}, err
	}
	id := strings.TrimSpace(projectID)
	if id == "" {
		return ProjectKnowledge{}, domain.NewValidationError("project", "project id is required")
	}
	project, ok, err := a.LookupProject(ctx, id)
	if err != nil {
		return ProjectKnowledge{}, err
	}
	if !ok {
		return ProjectKnowledge{ProjectID: id, Tier: TierAbsent}, nil
	}
	notes, err := a.listNotes(ctx, project.ID)
	if err != nil {
		return ProjectKnowledge{}, err
	}
	decisions, err := a.listDecisions(ctx, project.ID)
	if err != nil {
		return ProjectKnowledge{}, err
	}
	attempts, err := a.listAttempts(ctx, project.ID)
	if err != nil {
		return ProjectKnowledge{}, err
	}
	handoffs, err := a.listHandoffs(ctx, project.ID)
	if err != nil {
		return ProjectKnowledge{}, err
	}
	state, err := a.loadState(ctx, project.ID)
	if err != nil {
		return ProjectKnowledge{}, err
	}
	knowledge := ProjectKnowledge{
		ProjectID: project.ID,
		Notes:     len(notes),
		Decisions: len(decisions),
		Attempts:  len(attempts),
		Handoffs:  len(handoffs),
		HasState:  stateHasContent(state),
	}
	if knowledge.Records() > 0 || knowledge.HasState {
		knowledge.Tier = TierPopulated
	} else {
		knowledge.Tier = TierEmpty
	}
	return knowledge, nil
}

// stateHasContent reports whether the state carries a working picture, as
// opposed to the empty state file a newly registered project gets.
func stateHasContent(state domain.State) bool {
	return strings.TrimSpace(state.Goal) != "" ||
		strings.TrimSpace(state.NextAction) != "" ||
		len(state.Active) > 0 || len(state.Blocked) > 0 || len(state.OpenQuestions) > 0
}

// isRecordWriteEvent reports the creation events that retries may repeat.
func isRecordWriteEvent(event string) bool {
	return event == JournalNote || event == JournalDecision || event == JournalAttempt || event == JournalHandoff
}
