package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// RecordEdit changes the text of one stored record. Only title and body (plus
// tags on a note) are editable; ID, kind, project, provenance, validity,
// relations, review state and topic classification always come from the stored
// record. The decision status and supersede links stay separate commands.
type RecordEdit struct {
	ProjectID string
	ID        string
	// Title, Body and Tags are nil when the caller leaves that field alone.
	// For a note Body is its text; for a decision it is the context; for an
	// attempt it is the result; for a handoff it is the summary. A handoff has
	// no title, so setting Title on one is a validation error.
	Title *string
	Body  *string
	Tags  *[]string
	// ExpectedUpdatedAt is the revision the caller read. When non-zero the
	// edit is a compare-and-swap and a stale value returns a
	// domain.RevisionConflictError without writing.
	ExpectedUpdatedAt time.Time
	// AllowDuplicate accepts a body that already exists in another live
	// record of the same project and kind.
	AllowDuplicate bool
	// DryRun validates and returns the projected record without writing.
	DryRun bool
}

// RecordEditResult is the outcome of RecordEdit. Changed is false for an
// idempotent edit that matched the stored text exactly; DryRun reports that no
// file or index row was written.
type RecordEditResult struct {
	Record  Record
	Changed bool
	DryRun  bool
}

// EditRecord applies one text edit to a live record. The store rewrites the
// record in place under its file lock and a compare-and-swap on updated_at, so
// two editors cannot silently overwrite each other. The previous body is kept
// in the record's bounded history; provenance is preserved, so editing an
// unreviewed record never marks it reviewed.
func (a *App) EditRecord(ctx context.Context, edit RecordEdit) (RecordEditResult, error) {
	if err := a.requireStore(); err != nil {
		return RecordEditResult{}, err
	}
	edit.ID = strings.TrimSpace(edit.ID)
	if err := domain.ValidateRequired("record.id", edit.ID); err != nil {
		return RecordEditResult{}, err
	}
	if edit.Title == nil && edit.Body == nil && edit.Tags == nil {
		return RecordEditResult{}, domain.NewValidationError("record", "nothing to change; pass title, body or tags")
	}
	record, err := a.GetRecord(ctx, edit.ProjectID, edit.ID)
	if err != nil {
		return RecordEditResult{}, err
	}
	summary := record.Summary()
	if !edit.ExpectedUpdatedAt.IsZero() && !edit.ExpectedUpdatedAt.UTC().Equal(summary.UpdatedAt.UTC()) {
		return RecordEditResult{}, domain.NewRevisionConflictError(summary.Kind, summary.ID, summary.UpdatedAt)
	}
	next, textChanged, changed, err := applyRecordEdit(record, edit)
	if err != nil {
		return RecordEditResult{}, err
	}
	if !changed {
		return RecordEditResult{Record: record}, nil
	}
	if textChanged {
		next = withRecordHistory(next, historyWithBody(record, a.now()))
	}
	if err := validateEditedRecord(next); err != nil {
		return RecordEditResult{}, err
	}
	if err := a.enforceUpdateLimits(summary.ProjectID, summary.Kind, summary.ID, recordWriteContent(next)); err != nil {
		return RecordEditResult{}, err
	}
	if textChanged && !edit.AllowDuplicate {
		duplicate, found, err := a.exactDuplicateOfEdited(ctx, next, summary.ID)
		if err != nil {
			return RecordEditResult{}, err
		}
		if found {
			return RecordEditResult{}, &domain.DuplicateError{Kind: summary.Kind, ID: duplicate, ProjectID: summary.ProjectID}
		}
	}
	if edit.DryRun {
		return RecordEditResult{Record: next, Changed: true, DryRun: true}, nil
	}
	updater, ok := a.deps.Store.(ports.RecordTextUpdater)
	if !ok {
		return RecordEditResult{}, fmt.Errorf("%w: editing records is not supported by this store", domain.ErrUnsupported)
	}
	if err := updater.UpdateRecordText(ctx, ports.RecordTextUpdate{
		Note: next.Note, Decision: next.Decision, Attempt: next.Attempt, Handoff: next.Handoff,
		ExpectedUpdatedAt: edit.ExpectedUpdatedAt,
	}); err != nil {
		return RecordEditResult{}, err
	}
	updated, err := a.GetRecord(ctx, summary.ProjectID, summary.ID)
	if err != nil {
		return RecordEditResult{}, fmt.Errorf("%s %s was edited but could not be read back: %w", summary.Kind, summary.ID, err)
	}
	result := RecordEditResult{Record: updated, Changed: true}
	if err := a.afterWrite(ctx, JournalEdit, summary.ProjectID, summary.ID,
		updated.Summary().Title, recordDocument(updated)); err != nil {
		return result, err
	}
	return result, nil
}

// applyRecordEdit builds the edited record. It reports whether title or body
// changed (the duplicate check is about text) and whether anything changed at
// all (a tags-only update is still a write).
func applyRecordEdit(record Record, edit RecordEdit) (Record, bool, bool, error) {
	next := copyRecord(record)
	if edit.Tags != nil && record.Kind != ports.RecordNote {
		return Record{}, false, false, domain.NewValidationError("tags", "only a note has tags")
	}
	if edit.Title != nil {
		if record.Kind == ports.RecordHandoff {
			return Record{}, false, false, domain.NewValidationError("title",
				"a handoff has no title; edit its summary with body")
		}
		title := strings.TrimSpace(*edit.Title)
		switch {
		case next.Note != nil:
			next.Note.Title = title
		case next.Decision != nil:
			next.Decision.Title = title
		case next.Attempt != nil:
			next.Attempt.Title = title
		}
	}
	if edit.Body != nil {
		switch {
		case next.Note != nil:
			next.Note.Body = *edit.Body
		case next.Decision != nil:
			next.Decision.Context = *edit.Body
		case next.Attempt != nil:
			next.Attempt.Result = *edit.Body
		case next.Handoff != nil:
			next.Handoff.Summary = *edit.Body
		}
	}
	if edit.Tags != nil && next.Note != nil {
		next.Note.Tags = normalizeRecordTags(*edit.Tags)
	}
	beforeTitle, beforeBody, beforeTags := recordEditableText(record)
	afterTitle, afterBody, afterTags := recordEditableText(next)
	return next, beforeTitle != afterTitle || beforeBody != afterBody,
		beforeTitle != afterTitle || beforeBody != afterBody || !slices.Equal(beforeTags, afterTags), nil
}

// validateEditedRecord runs the domain validation of the edited kind.
func validateEditedRecord(record Record) error {
	switch {
	case record.Note != nil:
		return record.Note.Validate()
	case record.Decision != nil:
		return record.Decision.Validate()
	case record.Attempt != nil:
		return record.Attempt.Validate()
	case record.Handoff != nil:
		return record.Handoff.Validate()
	default:
		return domain.NewValidationError("record", "has no content")
	}
}

// recordEditableText returns the fields an edit can change, in the same order
// for a stored and an edited record.
func recordEditableText(record Record) (title, body string, tags []string) {
	switch {
	case record.Note != nil:
		return record.Note.Title, record.Note.Body, record.Note.Tags
	case record.Decision != nil:
		return record.Decision.Title, record.Decision.Context, nil
	case record.Attempt != nil:
		return record.Attempt.Title, record.Attempt.Result, nil
	case record.Handoff != nil:
		return "", record.Handoff.Summary, nil
	default:
		return "", "", nil
	}
}

// copyRecord copies the record and its text slices, so an edit never mutates
// the value a caller already read.
func copyRecord(record Record) Record {
	next := Record{Kind: record.Kind}
	switch {
	case record.Note != nil:
		value := *record.Note
		value.Tags = append([]string(nil), value.Tags...)
		value.History = append([]domain.HistoryEntry(nil), value.History...)
		next.Note = &value
	case record.Decision != nil:
		value := *record.Decision
		value.History = append([]domain.HistoryEntry(nil), value.History...)
		next.Decision = &value
	case record.Attempt != nil:
		value := *record.Attempt
		value.History = append([]domain.HistoryEntry(nil), value.History...)
		next.Attempt = &value
	case record.Handoff != nil:
		value := *record.Handoff
		value.History = append([]domain.HistoryEntry(nil), value.History...)
		next.Handoff = &value
	}
	return next
}

// withRecordHistory replaces the bounded topic history of a record.
func withRecordHistory(record Record, history []domain.HistoryEntry) Record {
	switch {
	case record.Note != nil:
		record.Note.History = domain.BoundedHistory(history)
	case record.Decision != nil:
		record.Decision.History = domain.BoundedHistory(history)
	case record.Attempt != nil:
		record.Attempt.History = domain.BoundedHistory(history)
	case record.Handoff != nil:
		record.Handoff.History = domain.BoundedHistory(history)
	}
	return record
}

// normalizeRecordTags trims tags, drops empty entries and removes repeats while
// keeping the author's order.
func normalizeRecordTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || slices.Contains(out, tag) {
			continue
		}
		out = append(out, tag)
	}
	return out
}

// exactDuplicateOfEdited returns the id of another live record of the same
// project and kind whose normalized text equals the edited record's. The
// edited record itself is never a candidate.
func (a *App) exactDuplicateOfEdited(ctx context.Context, record Record, editedID string) (string, bool, error) {
	summary := record.Summary()
	switch summary.Kind {
	case ports.RecordNote:
		notes, err := a.listNotes(ctx, summary.ProjectID)
		if err != nil {
			return "", false, err
		}
		candidates := noteDuplicateCandidates(withoutNote(notes, editedID))
		duplicate, ok := findExactDuplicate(candidates, recordDedupText(record.Note.Title, record.Note.Body))
		return duplicate.id, ok, nil
	case ports.RecordDecision:
		decisions, err := a.listDecisions(ctx, summary.ProjectID)
		if err != nil {
			return "", false, err
		}
		candidates := decisionDuplicateCandidates(withoutDecision(decisions, editedID))
		duplicate, ok := findExactDuplicate(candidates, decisionDedupText(*record.Decision))
		return duplicate.id, ok, nil
	case ports.RecordAttempt:
		attempts, err := a.listAttempts(ctx, summary.ProjectID)
		if err != nil {
			return "", false, err
		}
		candidates := attemptDuplicateCandidates(withoutAttempt(attempts, editedID))
		duplicate, ok := findExactDuplicate(candidates, attemptDedupText(*record.Attempt))
		return duplicate.id, ok, nil
	case ports.RecordHandoff:
		handoffs, err := a.listHandoffs(ctx, summary.ProjectID)
		if err != nil {
			return "", false, err
		}
		candidates := handoffDuplicateCandidates(withoutHandoff(handoffs, editedID))
		duplicate, ok := findExactDuplicate(candidates, handoffDedupText(*record.Handoff))
		return duplicate.id, ok, nil
	default:
		return "", false, nil
	}
}

func withoutNote(records []domain.Note, id string) []domain.Note {
	out := make([]domain.Note, 0, len(records))
	for _, record := range records {
		if record.ID != id {
			out = append(out, record)
		}
	}
	return out
}

func withoutDecision(records []domain.Decision, id string) []domain.Decision {
	out := make([]domain.Decision, 0, len(records))
	for _, record := range records {
		if record.ID != id {
			out = append(out, record)
		}
	}
	return out
}

func withoutAttempt(records []domain.Attempt, id string) []domain.Attempt {
	out := make([]domain.Attempt, 0, len(records))
	for _, record := range records {
		if record.ID != id {
			out = append(out, record)
		}
	}
	return out
}

func withoutHandoff(records []domain.Handoff, id string) []domain.Handoff {
	out := make([]domain.Handoff, 0, len(records))
	for _, record := range records {
		if record.ID != id {
			out = append(out, record)
		}
	}
	return out
}
