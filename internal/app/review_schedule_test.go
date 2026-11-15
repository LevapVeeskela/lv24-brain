package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/journal"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// newScheduleFixture builds a git-mode app with a controllable clock so review
// dates can be checked without waiting.
func newScheduleFixture(t *testing.T, cfg config.BrainConfig) reviewFixture {
	t.Helper()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	cfg.Mode = config.ModeGit
	cfg.User = "alice"
	a := app.New(cfg, app.Dependencies{Store: store, Journal: journal.New(dataDir), Now: func() time.Time { return clock }})
	return reviewFixture{app: a, clock: &clock}
}

func reviewAfterOf(t *testing.T, a *app.App, projectID, id string) time.Time {
	t.Helper()
	record, err := a.GetRecord(context.Background(), projectID, id)
	if err != nil {
		t.Fatalf("GetRecord(%s): %v", id, err)
	}
	return record.ReviewSchedule().ReviewAfter
}

func TestWriteSetsPerKindReviewDates(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(t, config.Default())
	note, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Body: "a note", CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "a decision", CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := f.app.Attempt(ctx, app.AttemptInput{ProjectID: "p", Title: "an attempt"})
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := f.app.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "a handoff"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		id   string
		want time.Duration
	}{
		{"note", note.Note.ID, domain.DefaultReviewNote},
		{"decision", decision.Decision.ID, domain.DefaultReviewDecision},
		{"attempt", attempt.Attempt.ID, domain.DefaultReviewAttempt},
		{"handoff", handoff.Handoff.ID, domain.DefaultReviewHandoff},
	}
	for _, test := range cases {
		got := reviewAfterOf(t, f.app, "p", test.id)
		if want := f.clock.Add(test.want); !got.Equal(want) {
			t.Fatalf("%s review date = %s, want %s", test.name, got, want)
		}
	}
}

func TestReviewWindowOverrideAppliesToNewRecords(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Review = config.ReviewConfig{Defaults: config.ReviewDefaults{Note: "1d"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	f := newScheduleFixture(t, cfg)
	note, err := f.app.Note(context.Background(), app.NoteInput{ProjectID: "p", Body: "a note", CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reviewAfterOf(t, f.app, "p", note.Note.ID), f.clock.Add(24*time.Hour); !got.Equal(want) {
		t.Fatalf("note review date = %s, want %s", got, want)
	}
}

func TestDueReviewsOrdersByAgeAndIncludesState(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(t, config.Default())
	note, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Body: "a note", CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "a decision", CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := f.app.Attempt(ctx, app.AttemptInput{ProjectID: "p", Title: "an attempt"})
	if err != nil {
		t.Fatal(err)
	}
	*f.clock = f.clock.Add(120 * 24 * time.Hour)

	due, err := f.app.DueReviews(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(due))
	for _, review := range due {
		kinds = append(kinds, review.Kind+":"+review.ID)
	}
	wantOrder := []string{"state:p", "attempt:" + attempt.Attempt.ID, "decision:" + decision.Decision.ID}
	if strings.Join(kinds, ",") != strings.Join(wantOrder, ",") {
		t.Fatalf("due order = %v, want %v", kinds, wantOrder)
	}
	for _, review := range due {
		if review.Overdue <= 0 {
			t.Fatalf("due record %+v has no overdue time", review)
		}
	}

	*f.clock = f.clock.Add(80 * 24 * time.Hour)
	due, err = f.app.DueReviews(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, review := range due {
		if review.ID == note.Note.ID {
			found = true
			if review.Source != "frontmatter" {
				t.Fatalf("note source = %q, want frontmatter", review.Source)
			}
		}
	}
	if !found {
		t.Fatalf("note %s not due after 200 days: %+v", note.Note.ID, due)
	}
}

func TestReviewRecordsMovesAndClearsReviewDate(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(t, config.Default())
	decision, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "a decision", Context: "why", CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	past := f.clock.Add(-24 * time.Hour)
	if _, err := f.app.ReviewRecords(ctx, app.ReviewRecordOptions{ProjectID: "p", ID: decision.Decision.ID, ReviewAfter: &past}); err != nil {
		t.Fatalf("ReviewRecords(review-after): %v", err)
	}
	if got := reviewAfterOf(t, f.app, "p", decision.Decision.ID); !got.Equal(past) {
		t.Fatalf("review date = %s, want %s", got, past)
	}
	due, err := f.app.DueReviews(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, review := range due {
		if review.ID == decision.Decision.ID {
			found = review.Source == "frontmatter" && review.Kind == "decision"
		}
	}
	if !found {
		t.Fatalf("decision not due after an explicit date: %+v", due)
	}

	if _, err := f.app.ReviewRecords(ctx, app.ReviewRecordOptions{ProjectID: "p", ID: decision.Decision.ID, ClearReviewAfter: true}); err != nil {
		t.Fatalf("ReviewRecords(clear): %v", err)
	}
	if got := reviewAfterOf(t, f.app, "p", decision.Decision.ID); !got.IsZero() {
		t.Fatalf("review date = %s after clear", got)
	}
	due, err = f.app.DueReviews(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	for _, review := range due {
		if review.ID == decision.Decision.ID && review.Source == "frontmatter" {
			t.Fatalf("cleared record still carries a frontmatter date: %+v", review)
		}
	}
}

func TestReviewRecordsScheduleRejectsAllAndBothFlags(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(t, config.Default())
	if _, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Body: "a note", CreateProject: true}); err != nil {
		t.Fatal(err)
	}
	past := f.clock.Add(-time.Hour)
	for _, opts := range []app.ReviewRecordOptions{
		{ProjectID: "p", All: true, ReviewAfter: &past},
		{ProjectID: "p", ID: "note-anything", ReviewAfter: &past, ClearReviewAfter: true},
	} {
		if _, err := f.app.ReviewRecords(ctx, opts); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("ReviewRecords(%+v) error = %v, want validation", opts, err)
		}
	}
}

func TestDueReviewsIncludesUnrefreshedProposals(t *testing.T) {
	// Arrange: a pending proposal is due when its source was not refreshed
	// within the proposal window.
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)
	fixture.add(t, promoteClosedIssue("issue-due", "Serialize writes"))
	fixture.digest(t)
	proposalID := domain.ProposalID("demo", "gh", "issue-due")

	due, err := fixture.app.DueReviews(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	for _, review := range due {
		if review.Kind == "proposal" {
			t.Fatalf("fresh proposal reported due: %+v", review)
		}
	}

	// Act
	fixture.clock = fixture.clock.Add(15 * 24 * time.Hour)
	due, err = fixture.app.DueReviews(ctx, "demo")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	for _, review := range due {
		if review.Kind != "proposal" {
			continue
		}
		if review.ID != proposalID || review.Source != "proposal" || review.Overdue <= 0 {
			t.Fatalf("proposal due entry = %+v", review)
		}
		return
	}
	t.Fatalf("proposal %s not due after 15 days: %+v", proposalID, due)
}

func TestReviewOverdueFindingEscalatesWithAge(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(t, config.Default())
	decision, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "a decision", CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	*f.clock = f.clock.Add(95 * 24 * time.Hour)
	findings, err := f.app.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	overdue := findingOfKind(findings, app.FindingReviewOverdue)
	if overdue == nil || overdue.Severity != app.SeverityWarn {
		t.Fatalf("overdue finding = %+v, want warn", overdue)
	}
	if !strings.Contains(overdue.Message, decision.Decision.ID) || !strings.Contains(overdue.Next, "brain review --due") {
		t.Fatalf("overdue finding = %+v", overdue)
	}

	*f.clock = f.clock.Add(90 * 24 * time.Hour)
	findings, err = f.app.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	overdue = findingOfKind(findings, app.FindingReviewOverdue)
	if overdue == nil || overdue.Severity != app.SeverityError {
		t.Fatalf("overdue finding = %+v, want error", overdue)
	}
}

func TestDoctorReportsOverdueReviews(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(t, config.Default())
	if _, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Body: "a note", CreateProject: true}); err != nil {
		t.Fatal(err)
	}
	*f.clock = f.clock.Add(200 * 24 * time.Hour)
	report, err := f.app.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range report.Checks {
		if check.Name != "reviews" {
			continue
		}
		if check.Status != "warn" || check.Code != string(app.FindingReviewOverdue) || !strings.Contains(check.Message, "(p ") {
			t.Fatalf("reviews check = %+v", check)
		}
		return
	}
	t.Fatalf("no reviews check in %+v", report.Checks)
}

func findingOfKind(findings []app.Finding, kind app.FindingKind) *app.Finding {
	for i := range findings {
		if findings[i].Kind == kind {
			return &findings[i]
		}
	}
	return nil
}
