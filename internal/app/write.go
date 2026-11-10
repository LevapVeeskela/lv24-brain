package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Note appends a dated note and indexes it for search.
func (a *App) Note(ctx context.Context, input NoteInput) (NoteResult, error) {
	input.redact()
	if err := a.requireStore(); err != nil {
		return NoteResult{}, err
	}
	project, err := a.ensureProject(ctx, input.ProjectID, input.CreateProject)
	if err != nil {
		return NoteResult{}, err
	}
	now := a.now()
	note := domain.Note{
		ProjectID: project.ID,
		Title:     fallbackTitle(input.Title, input.Body),
		Body:      input.Body,
		Tags:      input.Tags,
		Source:    input.Source,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := note.Validate(); err != nil {
		return NoteResult{}, err
	}
	existing, err := a.listNotes(ctx, project.ID)
	if err != nil {
		return NoteResult{}, err
	}
	for _, prior := range existing {
		if prior.Title == note.Title && prior.Body == note.Body && sameRecordSource(prior.Source, note.Source) && a.isRetry(prior.CreatedAt) {
			return NoteResult{Note: prior}, a.afterWrite(ctx, JournalNote, prior.ProjectID, prior.ID, prior.Title, noteDocument(prior))
		}
	}
	used := stringSet(noteIDs(existing))
	base := input.ID
	if base == "" {
		base = recordID("note", project.ID, now)
	}
	for attempt := 1; attempt <= maxRecordIDAttempts; attempt++ {
		note.ID = availableID(base, used)
		revision, appendErr := a.deps.Store.AppendNote(ctx, note)
		if appendErr == nil {
			return a.finishNote(ctx, note, revision)
		}
		if !errors.Is(appendErr, domain.ErrConflict) {
			return NoteResult{}, appendErr
		}
		used[note.ID] = struct{}{}
	}
	return NoteResult{}, fmt.Errorf("note: %w: no free record id", domain.ErrConflict)
}

func (a *App) finishNote(ctx context.Context, note domain.Note, revision domain.SyncRevision) (NoteResult, error) {
	result := NoteResult{Note: note, Revision: revision}
	return result, a.afterWrite(ctx, JournalNote, note.ProjectID, note.ID, note.Title, noteDocument(note))
}

// Decision appends a decision record and indexes it for search.
func (a *App) Decision(ctx context.Context, input DecisionInput) (DecisionResult, error) {
	input.redact()
	paths, err := domain.NormalizePaths(input.Paths)
	if err != nil {
		return DecisionResult{}, err
	}
	if err := a.requireStore(); err != nil {
		return DecisionResult{}, err
	}
	project, err := a.ensureProject(ctx, input.ProjectID, input.CreateProject)
	if err != nil {
		return DecisionResult{}, err
	}
	status := input.Status
	if status == "" {
		status = domain.DecisionProposed
	}
	now := a.now()
	decision := domain.Decision{
		ProjectID:    project.ID,
		Title:        strings.TrimSpace(input.Title),
		Status:       status,
		Context:      input.Context,
		Alternatives: input.Alternatives,
		Consequences: input.Consequences,
		Evidence:     input.Evidence,
		Paths:        paths,
		Source:       input.Source,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := decision.Validate(); err != nil {
		return DecisionResult{}, err
	}
	existing, err := a.listDecisions(ctx, project.ID)
	if err != nil {
		return DecisionResult{}, err
	}
	for _, prior := range existing {
		if prior.Title == decision.Title && prior.Context == decision.Context && sameRecordSource(prior.Source, decision.Source) && a.isRetry(prior.CreatedAt) {
			return DecisionResult{Decision: prior}, a.afterWrite(ctx, JournalDecision, prior.ProjectID, prior.ID, prior.Title, decisionDocument(prior))
		}
	}
	decision.ID = availableID(baseRecordID(input.ID, "decision", project.ID, now), stringSet(decisionIDs(existing)))
	revision, err := a.deps.Store.AppendDecision(ctx, decision)
	if err != nil {
		return DecisionResult{}, err
	}
	result := DecisionResult{Decision: decision, Revision: revision}
	return result, a.afterWrite(ctx, JournalDecision, decision.ProjectID, decision.ID, decision.Title, decisionDocument(decision))
}

// Attempt appends an attempt record and indexes it for search.
func (a *App) Attempt(ctx context.Context, input AttemptInput) (AttemptResult, error) {
	input.redact()
	if err := a.requireStore(); err != nil {
		return AttemptResult{}, err
	}
	project, err := a.ensureProject(ctx, input.ProjectID, input.CreateProject)
	if err != nil {
		return AttemptResult{}, err
	}
	now := a.now()
	attempt := domain.Attempt{
		ProjectID:  project.ID,
		Title:      strings.TrimSpace(input.Title),
		Hypothesis: input.Hypothesis,
		Result:     input.Result,
		Reason:     input.Reason,
		Source:     input.Source,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := attempt.Validate(); err != nil {
		return AttemptResult{}, err
	}
	existing, err := a.listAttempts(ctx, project.ID)
	if err != nil {
		return AttemptResult{}, err
	}
	for _, prior := range existing {
		if prior.Title == attempt.Title && prior.Hypothesis == attempt.Hypothesis && prior.Result == attempt.Result &&
			prior.Reason == attempt.Reason && sameRecordSource(prior.Source, attempt.Source) && a.isRetry(prior.CreatedAt) {
			return AttemptResult{Attempt: prior}, a.afterWrite(ctx, JournalAttempt, prior.ProjectID, prior.ID, prior.Title, attemptDocument(prior))
		}
	}
	attempt.ID = availableID(baseRecordID(input.ID, "attempt", project.ID, now), stringSet(attemptIDs(existing)))
	revision, err := a.deps.Store.AppendAttempt(ctx, attempt)
	if err != nil {
		return AttemptResult{}, err
	}
	result := AttemptResult{Attempt: attempt, Revision: revision}
	return result, a.afterWrite(ctx, JournalAttempt, attempt.ProjectID, attempt.ID, attempt.Title, attemptDocument(attempt))
}

// Handoff appends a session handoff and indexes it for search.
func (a *App) Handoff(ctx context.Context, input HandoffInput) (HandoffResult, error) {
	input.redact()
	if err := a.requireStore(); err != nil {
		return HandoffResult{}, err
	}
	project, err := a.ensureProject(ctx, input.ProjectID, input.CreateProject)
	if err != nil {
		return HandoffResult{}, err
	}
	now := a.now()
	handoff := domain.Handoff{
		ProjectID:  project.ID,
		Summary:    input.Summary,
		GitContext: input.GitContext,
		NextAction: input.NextAction,
		CreatedAt:  now,
	}
	if err := handoff.Validate(); err != nil {
		return HandoffResult{}, err
	}
	existing, err := a.listHandoffs(ctx, project.ID)
	if err != nil {
		return HandoffResult{}, err
	}
	for _, prior := range existing {
		if prior.Summary == handoff.Summary && prior.NextAction == handoff.NextAction && a.isRetry(prior.CreatedAt) {
			return HandoffResult{Handoff: prior}, a.afterWrite(ctx, JournalHandoff, prior.ProjectID, prior.ID, prior.Summary, handoffDocument(prior))
		}
	}
	handoff.ID = availableID(recordID("handoff", project.ID, now), stringSet(handoffIDs(existing)))
	revision, err := a.deps.Store.AppendHandoff(ctx, handoff)
	if err != nil {
		return HandoffResult{}, err
	}
	result := HandoffResult{Handoff: handoff, Revision: revision}
	return result, a.afterWrite(ctx, JournalHandoff, handoff.ProjectID, handoff.ID, handoff.Summary, handoffDocument(handoff))
}

// baseRecordID uses an explicit id when the caller pinned one (promotion uses
// a deterministic id) and otherwise the dated default.
func baseRecordID(id, prefix, projectID string, now time.Time) string {
	if id != "" {
		return id
	}
	return recordID(prefix, projectID, now)
}

// sameRecordSource scopes the retry heuristic. A plain user write has no
// source and keeps the old title/body behavior; a promoted write only matches
// a prior record from the same proposal, so two proposals with identical
// titles never collapse into one record.
func sameRecordSource(previous, next *domain.SourceRef) bool {
	if next == nil {
		return true
	}
	return previous != nil && previous.ProposalID == next.ProposalID
}

// retryWindow is how long an identical record counts as a retry of the same
// write (for example after "written but the index update failed") instead of
// a new record.
const retryWindow = 10 * time.Minute

// isRetry reports whether a record created at createdAt is recent enough that
// an identical write is a retry and must not create a duplicate.
func (a *App) isRetry(createdAt time.Time) bool {
	age := a.now().Sub(createdAt)
	return age >= -time.Minute && age < retryWindow
}
