package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// The prepare*Topic helpers implement the read-modify-write half of a topic
// upsert. They return handled=false with the topic lock still held when the
// topic does not exist yet, so the caller's create runs inside the same lock
// and two concurrent writers cannot both create the record. The caller must
// defer release whenever it is non-nil.

func (a *App) prepareTopicNote(ctx context.Context, projectID string, input NoteInput, taxonomy domain.Taxonomy) (NoteResult, bool, func(), error) {
	if !topicUpsertEligible(taxonomy.TopicKey, input.ID, input.Supersedes, input.SupersededBy) {
		return NoteResult{}, false, nil, nil
	}
	release, err := a.lockTopic(ctx, projectID, ports.RecordNote, taxonomy.TopicKey)
	if err != nil {
		return NoteResult{}, false, nil, err
	}
	record, found, err := a.findTopicRecord(ctx, projectID, ports.RecordNote, taxonomy.TopicKey)
	if err != nil || !found {
		return NoteResult{}, false, release, err
	}
	replacer, ok := a.deps.Store.(ports.TopicReplacer)
	if !ok {
		return NoteResult{}, false, release, fmt.Errorf("%w: topic upsert is not supported by this store", domain.ErrUnsupported)
	}
	existing := *record.Note
	if topicBodyEqual(existing.Body, input.Body) {
		return NoteResult{Note: existing, Already: true}, true, release, nil
	}
	next := existing
	if title := strings.TrimSpace(input.Title); title != "" {
		next.Title = title
	}
	next.Body = input.Body
	if len(input.Tags) > 0 {
		next.Tags = input.Tags
	}
	next.Taxonomy = mergeTopicTaxonomy(existing.Taxonomy, taxonomy,
		strings.TrimSpace(string(input.Scope)) != "", existing.Taxonomy.NormalizedOccurrences()+1)
	next.History = historyWithBody(record, a.now())
	next.UpdatedAt = a.now()
	if err := a.enforceUpdateLimits(projectID, ports.RecordNote, next.ID, noteWriteContent(next)); err != nil {
		return NoteResult{}, false, release, err
	}
	updated, err := replacer.ReplaceNote(ctx, next)
	if err != nil {
		return NoteResult{}, false, release, err
	}
	result := NoteResult{Note: updated}
	return result, true, release, a.afterWrite(ctx, JournalNote, updated.ProjectID, updated.ID, updated.Title, noteDocument(updated))
}

func (a *App) prepareTopicDecision(ctx context.Context, projectID string, input DecisionInput, taxonomy domain.Taxonomy) (DecisionResult, bool, func(), error) {
	if !topicUpsertEligible(taxonomy.TopicKey, input.ID, input.Supersedes, input.SupersededBy) {
		return DecisionResult{}, false, nil, nil
	}
	release, err := a.lockTopic(ctx, projectID, ports.RecordDecision, taxonomy.TopicKey)
	if err != nil {
		return DecisionResult{}, false, nil, err
	}
	record, found, err := a.findTopicRecord(ctx, projectID, ports.RecordDecision, taxonomy.TopicKey)
	if err != nil || !found {
		return DecisionResult{}, false, release, err
	}
	replacer, ok := a.deps.Store.(ports.TopicReplacer)
	if !ok {
		return DecisionResult{}, false, release, fmt.Errorf("%w: topic upsert is not supported by this store", domain.ErrUnsupported)
	}
	existing := *record.Decision
	if topicBodyEqual(decisionTopicBody(existing), decisionTopicBody(domain.Decision{
		Title: input.Title, Context: input.Context, Alternatives: input.Alternatives,
		Consequences: input.Consequences, Evidence: input.Evidence, Paths: input.Paths,
	})) {
		return DecisionResult{Decision: existing, Already: true}, true, release, nil
	}
	next := existing
	if title := strings.TrimSpace(input.Title); title != "" {
		next.Title = title
	}
	// Status is lifecycle, not text: a topic update keeps it and the explicit
	// `brain record status` command changes it.
	next.Context = input.Context
	next.Alternatives = input.Alternatives
	next.Consequences = input.Consequences
	next.Evidence = input.Evidence
	if len(input.Paths) > 0 {
		next.Paths = input.Paths
	}
	next.Taxonomy = mergeTopicTaxonomy(existing.Taxonomy, taxonomy,
		strings.TrimSpace(string(input.Scope)) != "", existing.Taxonomy.NormalizedOccurrences()+1)
	next.History = historyWithBody(record, a.now())
	next.UpdatedAt = a.now()
	if err := a.enforceUpdateLimits(projectID, ports.RecordDecision, next.ID, decisionWriteContent(next)); err != nil {
		return DecisionResult{}, false, release, err
	}
	updated, err := replacer.ReplaceDecision(ctx, next)
	if err != nil {
		return DecisionResult{}, false, release, err
	}
	result := DecisionResult{Decision: updated}
	return result, true, release, a.afterWrite(ctx, JournalDecision, updated.ProjectID, updated.ID, updated.Title, decisionDocument(updated))
}

func (a *App) prepareTopicAttempt(ctx context.Context, projectID string, input AttemptInput, taxonomy domain.Taxonomy) (AttemptResult, bool, func(), error) {
	if !topicUpsertEligible(taxonomy.TopicKey, input.ID, input.Supersedes, input.SupersededBy) {
		return AttemptResult{}, false, nil, nil
	}
	release, err := a.lockTopic(ctx, projectID, ports.RecordAttempt, taxonomy.TopicKey)
	if err != nil {
		return AttemptResult{}, false, nil, err
	}
	record, found, err := a.findTopicRecord(ctx, projectID, ports.RecordAttempt, taxonomy.TopicKey)
	if err != nil || !found {
		return AttemptResult{}, false, release, err
	}
	replacer, ok := a.deps.Store.(ports.TopicReplacer)
	if !ok {
		return AttemptResult{}, false, release, fmt.Errorf("%w: topic upsert is not supported by this store", domain.ErrUnsupported)
	}
	existing := *record.Attempt
	if topicBodyEqual(attemptTopicBody(existing), attemptTopicBody(domain.Attempt{
		Hypothesis: input.Hypothesis, Result: input.Result, Reason: input.Reason,
	})) {
		return AttemptResult{Attempt: existing, Already: true}, true, release, nil
	}
	next := existing
	if title := strings.TrimSpace(input.Title); title != "" {
		next.Title = title
	}
	next.Hypothesis = input.Hypothesis
	next.Result = input.Result
	next.Reason = input.Reason
	next.Taxonomy = mergeTopicTaxonomy(existing.Taxonomy, taxonomy,
		strings.TrimSpace(string(input.Scope)) != "", existing.Taxonomy.NormalizedOccurrences()+1)
	next.History = historyWithBody(record, a.now())
	next.UpdatedAt = a.now()
	if err := a.enforceUpdateLimits(projectID, ports.RecordAttempt, next.ID, attemptWriteContent(next)); err != nil {
		return AttemptResult{}, false, release, err
	}
	updated, err := replacer.ReplaceAttempt(ctx, next)
	if err != nil {
		return AttemptResult{}, false, release, err
	}
	result := AttemptResult{Attempt: updated}
	return result, true, release, a.afterWrite(ctx, JournalAttempt, updated.ProjectID, updated.ID, updated.Title, attemptDocument(updated))
}

func (a *App) prepareTopicHandoff(ctx context.Context, projectID string, input HandoffInput, taxonomy domain.Taxonomy) (HandoffResult, bool, func(), error) {
	if !topicUpsertEligible(taxonomy.TopicKey, "", input.Supersedes, input.SupersededBy) {
		return HandoffResult{}, false, nil, nil
	}
	release, err := a.lockTopic(ctx, projectID, ports.RecordHandoff, taxonomy.TopicKey)
	if err != nil {
		return HandoffResult{}, false, nil, err
	}
	record, found, err := a.findTopicRecord(ctx, projectID, ports.RecordHandoff, taxonomy.TopicKey)
	if err != nil || !found {
		return HandoffResult{}, false, release, err
	}
	replacer, ok := a.deps.Store.(ports.TopicReplacer)
	if !ok {
		return HandoffResult{}, false, release, fmt.Errorf("%w: topic upsert is not supported by this store", domain.ErrUnsupported)
	}
	existing := *record.Handoff
	if topicBodyEqual(handoffTopicBody(existing), handoffTopicBody(domain.Handoff{
		Summary: input.Summary, GitContext: input.GitContext, NextAction: input.NextAction,
	})) {
		return HandoffResult{Handoff: existing, Already: true}, true, release, nil
	}
	next := existing
	next.Summary = input.Summary
	next.GitContext = input.GitContext
	next.NextAction = input.NextAction
	next.Taxonomy = mergeTopicTaxonomy(existing.Taxonomy, taxonomy,
		strings.TrimSpace(string(input.Scope)) != "", existing.Taxonomy.NormalizedOccurrences()+1)
	next.History = historyWithBody(record, a.now())
	if err := a.enforceUpdateLimits(projectID, ports.RecordHandoff, next.ID, handoffWriteContent(next)); err != nil {
		return HandoffResult{}, false, release, err
	}
	updated, err := replacer.ReplaceHandoff(ctx, next)
	if err != nil {
		return HandoffResult{}, false, release, err
	}
	result := HandoffResult{Handoff: updated}
	return result, true, release, a.afterWrite(ctx, JournalHandoff, updated.ProjectID, updated.ID, updated.Summary, handoffDocument(updated))
}

// topicUpsertEligible reports whether a write is the topic-upsert form: a
// topic key with no pinned id and no supersede link. An explicit id or a
// supersede chain is a normal create, so history is never rewritten silently.
func topicUpsertEligible(topicKey, id, supersedes, supersededBy string) bool {
	return strings.TrimSpace(topicKey) != "" &&
		strings.TrimSpace(id) == "" &&
		strings.TrimSpace(supersedes) == "" &&
		strings.TrimSpace(supersededBy) == ""
}

func decisionTopicBody(decision domain.Decision) string {
	return joinText(decision.Context, strings.Join(decision.Alternatives, "\n"),
		strings.Join(decision.Consequences, "\n"), strings.Join(decision.Evidence, "\n"),
		strings.Join(decision.Paths, "\n"))
}

func attemptTopicBody(attempt domain.Attempt) string {
	return joinText(attempt.Hypothesis, attempt.Result, attempt.Reason)
}

func handoffTopicBody(handoff domain.Handoff) string {
	return joinText(handoff.Summary, handoff.GitContext, handoff.NextAction)
}
