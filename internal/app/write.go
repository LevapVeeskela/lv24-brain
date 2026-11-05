package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Note appends a dated note and indexes it for search.
func (a *App) Note(ctx context.Context, input NoteInput) (NoteResult, error) {
	if err := a.requireStore(); err != nil {
		return NoteResult{}, err
	}
	project, err := a.ensureProject(ctx, input.ProjectID)
	if err != nil {
		return NoteResult{}, err
	}
	now := a.now()
	note := domain.Note{
		ProjectID: project.ID,
		Title:     fallbackTitle(input.Title, input.Body),
		Body:      input.Body,
		Tags:      input.Tags,
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
	used := stringSet(noteIDs(existing))
	base := recordID("note", project.ID, now)
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
	if err := a.upsertIndex(ctx, noteDocument(note)); err != nil {
		return result, fmt.Errorf("note %s was written but the search index update failed: %w", note.ID, err)
	}
	return result, nil
}

// Decision appends a decision record and indexes it for search.
func (a *App) Decision(ctx context.Context, input DecisionInput) (DecisionResult, error) {
	if err := a.requireStore(); err != nil {
		return DecisionResult{}, err
	}
	project, err := a.ensureProject(ctx, input.ProjectID)
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
	decision.ID = availableID(recordID("decision", project.ID, now), stringSet(decisionIDs(existing)))
	revision, err := a.deps.Store.AppendDecision(ctx, decision)
	if err != nil {
		return DecisionResult{}, err
	}
	result := DecisionResult{Decision: decision, Revision: revision}
	if err := a.upsertIndex(ctx, decisionDocument(decision)); err != nil {
		return result, fmt.Errorf("decision %s was written but the search index update failed: %w", decision.ID, err)
	}
	return result, nil
}

// Attempt appends an attempt record and indexes it for search.
func (a *App) Attempt(ctx context.Context, input AttemptInput) (AttemptResult, error) {
	if err := a.requireStore(); err != nil {
		return AttemptResult{}, err
	}
	project, err := a.ensureProject(ctx, input.ProjectID)
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
	attempt.ID = availableID(recordID("attempt", project.ID, now), stringSet(attemptIDs(existing)))
	revision, err := a.deps.Store.AppendAttempt(ctx, attempt)
	if err != nil {
		return AttemptResult{}, err
	}
	result := AttemptResult{Attempt: attempt, Revision: revision}
	if err := a.upsertIndex(ctx, attemptDocument(attempt)); err != nil {
		return result, fmt.Errorf("attempt %s was written but the search index update failed: %w", attempt.ID, err)
	}
	return result, nil
}

// Handoff appends a session handoff and indexes it for search.
func (a *App) Handoff(ctx context.Context, input HandoffInput) (HandoffResult, error) {
	if err := a.requireStore(); err != nil {
		return HandoffResult{}, err
	}
	project, err := a.ensureProject(ctx, input.ProjectID)
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
	handoff.ID = availableID(recordID("handoff", project.ID, now), stringSet(handoffIDs(existing)))
	revision, err := a.deps.Store.AppendHandoff(ctx, handoff)
	if err != nil {
		return HandoffResult{}, err
	}
	result := HandoffResult{Handoff: handoff, Revision: revision}
	if err := a.upsertIndex(ctx, handoffDocument(handoff)); err != nil {
		return result, fmt.Errorf("handoff %s was written but the search index update failed: %w", handoff.ID, err)
	}
	return result, nil
}
