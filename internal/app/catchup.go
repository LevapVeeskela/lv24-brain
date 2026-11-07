package app

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// DefaultCatchupWindow is used when a project has no handoff yet.
const DefaultCatchupWindow = 7 * 24 * time.Hour

// CatchupOptions selects the project and the point in time to catch up from.
type CatchupOptions struct {
	ProjectID string
	// Since overrides the baseline; zero means "since the latest handoff".
	Since time.Time
	// Limit bounds each list; <= 0 uses 20.
	Limit int
}

// CatchupResult is everything that happened in a project after a baseline:
// the records other sessions and machines wrote, state changes and commits.
type CatchupResult struct {
	Project domain.Project
	State   domain.State
	Since   time.Time
	// SinceSource is "handoff", "explicit" or "default".
	SinceSource string
	// Baseline is the handoff the window starts at, when SinceSource is
	// "handoff".
	Baseline     *domain.Handoff
	Notes        []domain.Note
	Decisions    []domain.Decision
	Attempts     []domain.Attempt
	Handoffs     []domain.Handoff
	StateChanged bool
	// Journal holds commits and other activity lines, newest first.
	Journal []ports.JournalEntry
}

// Empty reports whether nothing happened since the baseline.
func (r CatchupResult) Empty() bool {
	return len(r.Notes) == 0 && len(r.Decisions) == 0 && len(r.Attempts) == 0 &&
		len(r.Handoffs) == 0 && !r.StateChanged && len(r.Journal) == 0
}

// Catchup reports what changed in a project since its latest handoff (or an
// explicit time), so a session starts from the delta instead of re-reading
// everything or asking the owner.
func (a *App) Catchup(ctx context.Context, opts CatchupOptions) (CatchupResult, error) {
	if err := a.requireStore(); err != nil {
		return CatchupResult{}, err
	}
	project, err := a.ensureProject(ctx, opts.ProjectID, false)
	if err != nil {
		return CatchupResult{}, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 20
	}
	result := CatchupResult{Project: project}
	if state, err := a.deps.Store.GetState(ctx, project.ID); err == nil {
		result.State = state
	} else if !errors.Is(err, domain.ErrNotFound) {
		return CatchupResult{}, err
	}

	handoffs, err := a.listHandoffs(ctx, project.ID)
	if err != nil {
		return CatchupResult{}, err
	}
	sort.Slice(handoffs, func(i, j int) bool { return handoffs[i].CreatedAt.After(handoffs[j].CreatedAt) })
	switch {
	case !opts.Since.IsZero():
		result.Since, result.SinceSource = opts.Since.UTC(), "explicit"
	case len(handoffs) > 0:
		latest := handoffs[0]
		result.Since, result.SinceSource, result.Baseline = latest.CreatedAt.UTC(), "handoff", &latest
	default:
		result.Since, result.SinceSource = a.now().Add(-DefaultCatchupWindow).UTC(), "default"
	}
	after := func(t time.Time) bool { return t.After(result.Since) }

	notes, err := a.listNotes(ctx, project.ID)
	if err != nil {
		return CatchupResult{}, err
	}
	for _, note := range notes {
		if after(note.CreatedAt) {
			result.Notes = append(result.Notes, note)
		}
	}
	decisions, err := a.listDecisions(ctx, project.ID)
	if err != nil {
		return CatchupResult{}, err
	}
	for _, decision := range decisions {
		if after(decision.CreatedAt) || after(decision.UpdatedAt) {
			result.Decisions = append(result.Decisions, decision)
		}
	}
	attempts, err := a.listAttempts(ctx, project.ID)
	if err != nil {
		return CatchupResult{}, err
	}
	for _, attempt := range attempts {
		if after(attempt.CreatedAt) {
			result.Attempts = append(result.Attempts, attempt)
		}
	}
	for _, handoff := range handoffs {
		if after(handoff.CreatedAt) {
			result.Handoffs = append(result.Handoffs, handoff)
		}
	}
	result.StateChanged = after(result.State.UpdatedAt)

	if a.deps.Journal != nil {
		entries, err := a.deps.Journal.Read(ctx, ports.JournalQuery{Project: project.ID, Since: result.Since.Add(time.Second), Limit: limit * 4})
		if err != nil {
			return CatchupResult{}, err
		}
		// Record events are already listed above; keep commits and other
		// activity (state changes, connector syncs).
		for _, entry := range entries {
			switch entry.Event {
			case JournalNote, JournalDecision, JournalAttempt, JournalHandoff:
				continue
			}
			result.Journal = append(result.Journal, entry)
		}
	}

	result.Notes = newestNotes(result.Notes, limit)
	result.Decisions = newestDecisions(result.Decisions, limit)
	result.Attempts = newestAttempts(result.Attempts, limit)
	if len(result.Handoffs) > limit {
		result.Handoffs = result.Handoffs[:limit]
	}
	if len(result.Journal) > limit {
		result.Journal = result.Journal[:limit]
	}
	return result, nil
}

// RecentRecords returns the newest decisions, attempts and handoffs of a
// project (for machine-readable open output).
func (a *App) RecentRecords(ctx context.Context, projectID string, limit int) ([]domain.Decision, []domain.Attempt, []domain.Handoff, error) {
	if err := a.requireStore(); err != nil {
		return nil, nil, nil, err
	}
	if limit <= 0 {
		limit = 5
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return nil, nil, nil, err
	}
	decisions, err := a.listDecisions(ctx, project.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	attempts, err := a.listAttempts(ctx, project.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	handoffs, err := a.listHandoffs(ctx, project.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	sort.Slice(handoffs, func(i, j int) bool { return handoffs[i].CreatedAt.After(handoffs[j].CreatedAt) })
	if len(handoffs) > limit {
		handoffs = handoffs[:limit]
	}
	return newestDecisions(decisions, limit), newestAttempts(attempts, limit), handoffs, nil
}

func newestNotes(notes []domain.Note, limit int) []domain.Note {
	sort.Slice(notes, func(i, j int) bool { return notes[i].CreatedAt.After(notes[j].CreatedAt) })
	if len(notes) > limit {
		notes = notes[:limit]
	}
	return notes
}

func newestDecisions(decisions []domain.Decision, limit int) []domain.Decision {
	sort.Slice(decisions, func(i, j int) bool { return decisions[i].UpdatedAt.After(decisions[j].UpdatedAt) })
	if len(decisions) > limit {
		decisions = decisions[:limit]
	}
	return decisions
}

func newestAttempts(attempts []domain.Attempt, limit int) []domain.Attempt {
	sort.Slice(attempts, func(i, j int) bool { return attempts[i].CreatedAt.After(attempts[j].CreatedAt) })
	if len(attempts) > limit {
		attempts = attempts[:limit]
	}
	return attempts
}

// SummarizeLine shortens free text to one line for listings.
func SummarizeLine(text string, limit int) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(text), "\n", 2)[0])
	if runes := []rune(line); len(runes) > limit {
		return strings.TrimSpace(string(runes[:limit])) + "…"
	}
	return line
}
