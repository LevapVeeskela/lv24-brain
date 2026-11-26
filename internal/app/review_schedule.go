package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// reviewOverdueErrorAfter is how long a record must be past its review date
// before the aggregated finding escalates from warn to error.
const reviewOverdueErrorAfter = 30 * 24 * time.Hour

// DueReview is one record, project state or proposal that review lists as due.
type DueReview struct {
	// Kind is note, decision, attempt, handoff, state or proposal.
	Kind string
	// ID is the record id, the project id for state, or the proposal id.
	ID string
	// Title is the human-readable title.
	Title string
	// ReviewAfter is when it became due.
	ReviewAfter time.Time
	// Overdue is how long the review date has passed.
	Overdue time.Duration
	// Source is "frontmatter" when the record carries review_after,
	// "default" when the per-kind default was applied to its creation time,
	// and "proposal" for a pending candidate.
	Source string
}

// reviewWindow returns the effective review window of one record kind.
func (a *App) reviewWindow(kind string) time.Duration {
	windows := a.cfg.EffectiveReviewWindows()
	switch kind {
	case ports.RecordDecision:
		return windows.Decision
	case ports.RecordAttempt:
		return windows.Attempt
	case ports.RecordNote:
		return windows.Note
	case ports.RecordHandoff:
		return windows.Handoff
	case "state":
		return windows.State
	case "proposal":
		return windows.Proposal
	default:
		return 0
	}
}

// newReviewSchedule is the review schedule a fresh record is written with: the
// kind's window counted from now. A zero window leaves the record unscheduled.
func (a *App) newReviewSchedule(kind string, now time.Time) domain.ReviewSchedule {
	window := a.reviewWindow(kind)
	if window <= 0 || now.IsZero() {
		return domain.ReviewSchedule{}
	}
	return domain.ReviewSchedule{ReviewAfter: now.UTC().Add(window).Truncate(time.Second)}
}

// newWriteSchedule is the review schedule of a record that is being written. A
// repair write (superseded_by set) is history by definition and gets none.
func (a *App) newWriteSchedule(kind string, validity domain.Validity, now time.Time) domain.ReviewSchedule {
	if validity.SupersededBy != "" {
		return domain.ReviewSchedule{}
	}
	return a.newReviewSchedule(kind, now)
}

// reviewAfter returns the effective review date of a record: its frontmatter
// date when it has one, otherwise the kind's default counted from creation.
func (a *App) reviewAfter(kind string, schedule domain.ReviewSchedule, createdAt time.Time) (time.Time, string) {
	if !schedule.ReviewAfter.IsZero() {
		return schedule.ReviewAfter.UTC(), "frontmatter"
	}
	window := a.reviewWindow(kind)
	if window <= 0 || createdAt.IsZero() {
		return time.Time{}, ""
	}
	return createdAt.UTC().Add(window).Truncate(time.Second), "default"
}

// ReviewSchedule returns the review dates of whichever record is set.
func (r Record) ReviewSchedule() domain.ReviewSchedule {
	switch {
	case r.Note != nil:
		return r.Note.ReviewSchedule
	case r.Decision != nil:
		return r.Decision.ReviewSchedule
	case r.Attempt != nil:
		return r.Attempt.ReviewSchedule
	case r.Handoff != nil:
		return r.Handoff.ReviewSchedule
	case r.Idea != nil:
		return r.Idea.ReviewSchedule
	}
	return domain.ReviewSchedule{}
}

// dueRecords lists the current, reviewed records whose review date has passed,
// oldest due date first. Quarantined records stay in the quarantine queue: the
// same `brain record review` call clears them.
func (a *App) dueRecords(in reviewInputs, now time.Time) []DueReview {
	var due []DueReview
	add := func(kind string, schedule domain.ReviewSchedule, createdAt time.Time, id, title string) {
		after, source := a.reviewAfter(kind, schedule, createdAt)
		if after.IsZero() || after.After(now) {
			return
		}
		due = append(due, DueReview{Kind: kind, ID: id, Title: title, ReviewAfter: after, Overdue: now.Sub(after), Source: source})
	}
	for _, note := range in.notes {
		if note.Quarantined() || !note.Validity.IsCurrent() {
			continue
		}
		add(ports.RecordNote, note.ReviewSchedule, note.CreatedAt, note.ID, SummarizeLine(valueOr(note.Title, note.Body), 100))
	}
	for _, decision := range in.decisions {
		if decision.Quarantined() || !decision.Validity.IsCurrent() ||
			decision.Status == domain.DecisionRejected || decision.Status == domain.DecisionSuperseded {
			continue
		}
		add(ports.RecordDecision, decision.ReviewSchedule, decision.CreatedAt, decision.ID, SummarizeLine(decision.Title, 100))
	}
	for _, attempt := range in.attempts {
		if attempt.Quarantined() || !attempt.Validity.IsCurrent() {
			continue
		}
		add(ports.RecordAttempt, attempt.ReviewSchedule, attempt.CreatedAt, attempt.ID, SummarizeLine(attempt.Title, 100))
	}
	for _, handoff := range in.handoffs {
		if handoff.Quarantined() || !handoff.Validity.IsCurrent() {
			continue
		}
		add(ports.RecordHandoff, handoff.ReviewSchedule, handoff.CreatedAt, handoff.ID, SummarizeLine(handoff.Summary, 100))
	}
	sort.SliceStable(due, func(i, j int) bool {
		if !due[i].ReviewAfter.Equal(due[j].ReviewAfter) {
			return due[i].ReviewAfter.Before(due[j].ReviewAfter)
		}
		return due[i].ID < due[j].ID
	})
	return due
}

// dueState lists the project state when it is older than the state window.
// The freshness window (state.fresh_until) is the explicit override; this check
// catches a state nobody has touched while the project moved on.
func (a *App) dueState(in reviewInputs, now time.Time) []DueReview {
	if in.state.UpdatedAt.IsZero() {
		return nil
	}
	window := a.reviewWindow("state")
	if window <= 0 {
		return nil
	}
	after := in.state.UpdatedAt.UTC().Add(window).Truncate(time.Second)
	if after.After(now) {
		return nil
	}
	return []DueReview{{
		Kind: "state", ID: in.state.ProjectID, Title: "project state",
		ReviewAfter: after, Overdue: now.Sub(after), Source: "default",
	}}
}

// dueProposals lists pending candidates whose source has not been refreshed
// within the proposal window. A stale status counts as due immediately.
func (a *App) dueProposals(ctx context.Context, projectID string, now time.Time) ([]DueReview, error) {
	if a.deps.Proposals == nil {
		return nil, nil
	}
	proposals, err := a.listPendingProposals(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list proposals: %w", err)
	}
	window := a.reviewWindow("proposal")
	if window <= 0 {
		return nil, nil
	}
	var due []DueReview
	for _, proposal := range proposals {
		if proposal.IsSnoozed(now) {
			continue
		}
		updated := proposal.UpdatedAt
		if updated.IsZero() {
			updated = proposal.CreatedAt
		}
		after := updated.UTC().Add(window).Truncate(time.Second)
		if proposal.StatusOrPending() == domain.ProposalStale {
			after = now
		}
		if after.After(now) {
			continue
		}
		due = append(due, DueReview{
			Kind: "proposal", ID: proposal.ID, Title: SummarizeLine(proposal.Title, 100),
			ReviewAfter: after, Overdue: now.Sub(after), Source: "proposal",
		})
	}
	return due, nil
}

// DueReviews lists everything review lists as due for one project: records past
// their review date, an old state, and pending proposals that were not
// refreshed. The list is ordered by due date, oldest first.
func (a *App) DueReviews(ctx context.Context, projectID string) ([]DueReview, error) {
	if err := a.requireStore(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	in, err := a.reviewInputs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	now := a.now()
	due := a.dueRecords(in, now)
	due = append(due, a.dueState(in, now)...)
	proposals, err := a.dueProposals(ctx, in.state.ProjectID, now)
	if err != nil {
		return nil, err
	}
	due = append(due, proposals...)
	sort.SliceStable(due, func(i, j int) bool {
		if !due[i].ReviewAfter.Equal(due[j].ReviewAfter) {
			return due[i].ReviewAfter.Before(due[j].ReviewAfter)
		}
		return due[i].ID < due[j].ID
	})
	return due, nil
}

// reviewOverdueFindings aggregates the overdue records of one project into one
// finding, like the quarantine queue: a long list is a queue, not a finding per
// record. The severity escalates with the oldest overdue date.
func (a *App) reviewOverdueFindings(in reviewInputs, now time.Time) []Finding {
	due := a.dueRecords(in, now)
	if len(due) == 0 {
		return nil
	}
	oldest := due[0]
	ids := make([]string, 0, len(due))
	for _, review := range due {
		ids = append(ids, review.ID)
	}
	severity := SeverityWarn
	if oldest.Overdue >= reviewOverdueErrorAfter {
		severity = SeverityError
	}
	days := int(oldest.Overdue.Hours() / 24)
	subject := fmt.Sprintf("%d records are", len(due))
	if len(due) == 1 {
		subject = "1 record is"
	}
	finding := newFinding(FindingReviewOverdue,
		fmt.Sprintf("%s past their review date; the oldest (%s %s) is %d days overdue: %s",
			subject, oldest.Kind, oldest.ID, days, quarantineIDSample(ids)),
		oldest.ID).withSeverity(severity)
	return []Finding{finding}
}

// doctorReviews reports how many records are past their review date. It only
// reports: moving a review date or superseding a record is a decision.
func (a *App) doctorReviews(ctx context.Context) DoctorCheck {
	if a.deps.Store == nil || a.cfg.Mode == config.ModeServer {
		return DoctorCheck{Name: "reviews", Status: doctorOK, Message: "not checked in this mode"}
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return DoctorCheck{Name: "reviews", Status: doctorFail, Message: err.Error()}
	}
	var counts []string
	total := 0
	for _, project := range projects {
		due, err := a.DueReviews(ctx, project.ID)
		if err != nil {
			return DoctorCheck{Name: "reviews", Status: doctorFail, Message: project.ID + ": " + err.Error()}
		}
		if len(due) > 0 {
			counts = append(counts, fmt.Sprintf("%s %d", project.ID, len(due)))
			total += len(due)
		}
	}
	if total == 0 {
		return DoctorCheck{Name: "reviews", Status: doctorOK, Message: fmt.Sprintf("no records past their review date (%d projects)", len(projects))}
	}
	return DoctorCheck{
		Name:    "reviews",
		Status:  doctorWarn,
		Code:    string(FindingReviewOverdue),
		Message: fmt.Sprintf("%d records past their review date (%s); report only: see brain review --due", total, strings.Join(counts, ", ")),
	}
}
