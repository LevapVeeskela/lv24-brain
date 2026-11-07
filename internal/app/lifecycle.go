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
}

// ListRecordsOptions filters ListRecords.
type ListRecordsOptions struct {
	ProjectID string
	// Kinds keeps only these record kinds; empty lists every kind.
	Kinds []string
	// Since keeps records created or updated after it.
	Since time.Time
	// Status keeps decisions with this status (and drops other kinds).
	Status string
	// Limit bounds the result; <= 0 uses 50.
	Limit int
}

// Record is one record of any kind; exactly one pointer is set.
type Record struct {
	Kind     string
	Note     *domain.Note
	Decision *domain.Decision
	Attempt  *domain.Attempt
	Handoff  *domain.Handoff
}

// Summary returns the listing form of a record.
func (r Record) Summary() RecordSummary {
	switch {
	case r.Note != nil:
		return RecordSummary{Kind: ports.RecordNote, ID: r.Note.ID, ProjectID: r.Note.ProjectID,
			Title: SummarizeLine(valueOr(r.Note.Title, r.Note.Body), 100), CreatedAt: r.Note.CreatedAt, UpdatedAt: r.Note.UpdatedAt}
	case r.Decision != nil:
		return RecordSummary{Kind: ports.RecordDecision, ID: r.Decision.ID, ProjectID: r.Decision.ProjectID,
			Title: SummarizeLine(r.Decision.Title, 100), Status: string(r.Decision.Status), CreatedAt: r.Decision.CreatedAt, UpdatedAt: r.Decision.UpdatedAt}
	case r.Attempt != nil:
		return RecordSummary{Kind: ports.RecordAttempt, ID: r.Attempt.ID, ProjectID: r.Attempt.ProjectID,
			Title: SummarizeLine(r.Attempt.Title, 100), CreatedAt: r.Attempt.CreatedAt, UpdatedAt: r.Attempt.UpdatedAt}
	case r.Handoff != nil:
		return RecordSummary{Kind: ports.RecordHandoff, ID: r.Handoff.ID, ProjectID: r.Handoff.ProjectID,
			Title: SummarizeLine(r.Handoff.Summary, 100), CreatedAt: r.Handoff.CreatedAt, UpdatedAt: r.Handoff.CreatedAt}
	}
	return RecordSummary{}
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
	records, err := a.projectRecords(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	var out []RecordSummary
	for _, record := range records {
		summary := record.Summary()
		if len(opts.Kinds) > 0 && !containsString(opts.Kinds, summary.Kind) {
			continue
		}
		if opts.Status != "" && (summary.Kind != ports.RecordDecision || !strings.EqualFold(summary.Status, opts.Status)) {
			continue
		}
		if !opts.Since.IsZero() && !summary.CreatedAt.After(opts.Since) && !summary.UpdatedAt.After(opts.Since) {
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
// rejected, superseded).
func (a *App) SetDecisionStatus(ctx context.Context, projectID, id string, status domain.DecisionStatus) (domain.Decision, error) {
	return a.updateDecision(ctx, projectID, id, func(decision *domain.Decision) error {
		if !status.Valid() {
			return domain.NewValidationError("status", fmt.Sprintf("unknown decision status %q", status))
		}
		decision.Status = status
		return nil
	})
}

// Supersede marks decision oldID superseded by the live decision newID and
// records the link as evidence on the old one.
func (a *App) Supersede(ctx context.Context, projectID, oldID, newID string) (domain.Decision, error) {
	replacement, err := a.GetRecord(ctx, projectID, newID)
	if err != nil {
		return domain.Decision{}, err
	}
	if replacement.Decision == nil {
		return domain.Decision{}, domain.NewValidationError("by", fmt.Sprintf("%q is a %s, not a decision", newID, replacement.Kind))
	}
	if strings.TrimSpace(oldID) == strings.TrimSpace(newID) {
		return domain.Decision{}, domain.NewValidationError("by", "a decision cannot supersede itself")
	}
	return a.updateDecision(ctx, projectID, oldID, func(decision *domain.Decision) error {
		decision.Status = domain.DecisionSuperseded
		link := "superseded by " + replacement.Decision.ID
		if !containsString(decision.Evidence, link) {
			decision.Evidence = append(decision.Evidence, link)
		}
		return nil
	})
}

func (a *App) updateDecision(ctx context.Context, projectID, id string, change func(*domain.Decision) error) (domain.Decision, error) {
	editor, err := a.recordEditor()
	if err != nil {
		return domain.Decision{}, err
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
		if err := a.deps.Index.Delete(ctx, summary.ID); err != nil {
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
	default:
		return handoffDocument(*record.Handoff)
	}
}

func validateRecordKind(kind string) error {
	switch kind {
	case ports.RecordNote, ports.RecordDecision, ports.RecordAttempt, ports.RecordHandoff:
		return nil
	}
	return domain.NewValidationError("kind", fmt.Sprintf("unknown record kind %q (note, decision, attempt, handoff)", kind))
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// isRecordWriteEvent reports the creation events that retries may repeat.
func isRecordWriteEvent(event string) bool {
	return event == JournalNote || event == JournalDecision || event == JournalAttempt || event == JournalHandoff
}
