package app

import (
	"context"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// journalDedupeWindow bounds the lookup that keeps a retried write from
// logging the same record twice.
const journalDedupeWindow = 24 * time.Hour

// DefaultJournalRepairWindow is how far back journal repair and the doctor
// check look for records without a journal line.
const DefaultJournalRepairWindow = 30 * 24 * time.Hour

// journalScanLimit is the largest journal read (the adapter's maximum); an
// unbounded read would otherwise return only the newest 50 lines.
const journalScanLimit = 1000

// journaled reports whether any user's journal already has this record event.
func (a *App) journaled(ctx context.Context, event, projectID, id string) bool {
	entries, err := a.deps.Journal.Read(ctx, ports.JournalQuery{
		Project: projectID, Event: event, Since: a.now().Add(-journalDedupeWindow), Limit: journalScanLimit,
	})
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.ID == id {
			return true
		}
	}
	return false
}

// JournalGap is a record written after since that no journal mentions.
type JournalGap struct {
	Event     string
	ProjectID string
	ID        string
	Title     string
	CreatedAt time.Time
}

// JournalGaps lists records created after since that have no journal line in
// any user's journal (a journal append failed after the record was stored).
// A project whose journal read reaches journalScanLimit is skipped: its lines
// cannot all be seen, and reporting them would make repair write duplicates.
func (a *App) JournalGaps(ctx context.Context, since time.Time) ([]JournalGap, error) {
	if err := a.requireStore(); err != nil || a.deps.Journal == nil {
		return nil, err
	}
	records, err := a.recordsSince(ctx, since)
	if err != nil {
		return nil, err
	}
	logged := map[string]map[string]struct{}{}
	var gaps []JournalGap
	for _, record := range records {
		ids, seen := logged[record.ProjectID]
		if !seen {
			if ids, err = a.journaledIDs(ctx, record.ProjectID, since); err != nil {
				return nil, err
			}
			logged[record.ProjectID] = ids
		}
		if ids == nil {
			continue
		}
		if _, ok := ids[record.Event+"\x00"+record.ID]; !ok {
			gaps = append(gaps, record)
		}
	}
	return gaps, nil
}

// journaledIDs returns event+id keys journaled for a project after since,
// or nil when the read was truncated.
func (a *App) journaledIDs(ctx context.Context, projectID string, since time.Time) (map[string]struct{}, error) {
	entries, err := a.deps.Journal.Read(ctx, ports.JournalQuery{Project: projectID, Since: since.Add(-time.Hour), Limit: journalScanLimit})
	if err != nil {
		return nil, err
	}
	if len(entries) >= journalScanLimit {
		return nil, nil
	}
	ids := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.ID != "" {
			ids[entry.Event+"\x00"+entry.ID] = struct{}{}
		}
	}
	return ids, nil
}

// RepairJournal appends a line for every gap, timed at the record's creation
// and marked repaired, and returns the repaired gaps.
func (a *App) RepairJournal(ctx context.Context, since time.Time) ([]JournalGap, error) {
	gaps, err := a.JournalGaps(ctx, since)
	if err != nil {
		return nil, err
	}
	for i, gap := range gaps {
		if err := a.deps.Journal.Append(ctx, ports.JournalEntry{
			Time: gap.CreatedAt, Event: gap.Event, User: a.JournalUser(), Project: gap.ProjectID,
			ID: gap.ID, Title: gap.Title, Fields: map[string]string{"repaired": "true"},
		}); err != nil {
			return gaps[:i], err
		}
	}
	return gaps, nil
}

// recordsSince lists notes, decisions, attempts and handoffs of every project
// created after since.
func (a *App) recordsSince(ctx context.Context, since time.Time) ([]JournalGap, error) {
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var out []JournalGap
	add := func(event, projectID, id, title string, created time.Time) {
		if created.After(since) {
			out = append(out, JournalGap{Event: event, ProjectID: projectID, ID: id, Title: SummarizeLine(title, 80), CreatedAt: created})
		}
	}
	for _, project := range projects {
		notes, err := a.listNotes(ctx, project.ID)
		if err != nil {
			return nil, err
		}
		for _, note := range notes {
			add(JournalNote, project.ID, note.ID, valueOr(note.Title, note.Body), note.CreatedAt)
		}
		decisions, err := a.listDecisions(ctx, project.ID)
		if err != nil {
			return nil, err
		}
		for _, decision := range decisions {
			add(JournalDecision, project.ID, decision.ID, decision.Title, decision.CreatedAt)
		}
		attempts, err := a.listAttempts(ctx, project.ID)
		if err != nil {
			return nil, err
		}
		for _, attempt := range attempts {
			add(JournalAttempt, project.ID, attempt.ID, attempt.Title, attempt.CreatedAt)
		}
		handoffs, err := a.listHandoffs(ctx, project.ID)
		if err != nil {
			return nil, err
		}
		for _, handoff := range handoffs {
			add(JournalHandoff, project.ID, handoff.ID, handoff.Summary, handoff.CreatedAt)
		}
	}
	return out, nil
}
