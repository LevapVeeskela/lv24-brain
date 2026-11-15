package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// TestFindingsAlwaysExplainThemselves runs the real producers and checks the
// review contract on every finding they return: a valid severity and a why,
// next and confirm a reader can act on.
func TestFindingsAlwaysExplainThemselves(t *testing.T) {
	// Arrange: one record per producer that can fire in a single project.
	ctx := context.Background()
	f := newReviewFixture(t)
	next := "write the parser"
	questions := []string{"Which storage engine should the index use?"}
	fresh := f.clock.Add(-time.Hour)
	if _, err := f.app.SetState(ctx, app.StateInput{ProjectID: "p", NextAction: &next, OpenQuestions: &questions, FreshUntil: &fresh}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if _, err := f.app.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "parser done", NextAction: "ship the release"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Index storage engine: SQLite FTS5"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Note(ctx, app.NoteInput{
		ProjectID: "p", Title: "Connector note", Body: "unreviewed external text",
		Provenance: domain.Provenance{Origin: domain.OriginConnector, Reviewed: false},
	}); err != nil {
		t.Fatal(err)
	}
	body := "the index is rebuilt on every sync and the old index is removed afterwards by the worker"
	if _, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Index rebuild", Body: body}); err != nil {
		t.Fatal(err)
	}
	f.pastRetryWindow()
	if _, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Index rebuild", Body: "- " + body + "!", AllowDuplicate: true}); err != nil {
		t.Fatal(err)
	}
	*f.clock = f.clock.Add(200 * 24 * time.Hour)

	// Act
	findings, err := f.app.Findings(ctx, "p")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 {
		t.Fatal("fixture produced no findings")
	}
	seen := map[app.FindingKind]bool{}
	for _, finding := range findings {
		if !finding.Severity.Valid() {
			t.Fatalf("finding %s has severity %q", finding.Kind, finding.Severity)
		}
		if strings.TrimSpace(finding.Why) == "" || strings.TrimSpace(finding.Next) == "" {
			t.Fatalf("finding %s explains nothing: %+v", finding.Kind, finding)
		}
		if finding.Message == "" {
			t.Fatalf("finding %s has no message", finding.Kind)
		}
		seen[finding.Kind] = true
	}
	for _, kind := range []app.FindingKind{
		app.FindingQuarantined, app.FindingStateExpired, app.FindingNextBehindHandoff,
		app.FindingQuestionAnswered, app.FindingNearDuplicate, app.FindingReviewOverdue,
	} {
		if !seen[kind] {
			t.Fatalf("fixture did not trigger %s: %+v", kind, findings)
		}
	}
}

// TestQuarantinedFindingStaysFirst keeps the documented order: a short context
// never truncates the queue that says external text awaits review.
func TestQuarantinedFindingStaysFirst(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t)
	if _, err := f.app.Note(ctx, app.NoteInput{
		ProjectID: "p", Title: "Imported", Body: "imported external text",
		Provenance: domain.Provenance{Origin: domain.OriginImport, Reviewed: false},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "A decision"}); err != nil {
		t.Fatal(err)
	}
	*f.clock = f.clock.Add(200 * 24 * time.Hour)
	findings, err := f.app.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 || findings[0].Kind != app.FindingQuarantined {
		t.Fatalf("first finding = %+v, want quarantined", findings)
	}
}
