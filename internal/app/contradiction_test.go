package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/journal"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
)

func newContradictionFixture(t *testing.T) (*app.App, *time.Time) {
	t.Helper()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.User = "alice"
	a := app.New(cfg, app.Dependencies{Store: store, Journal: journal.New(dataDir), Now: func() time.Time { return clock }})
	return a, &clock
}

func suspectedContradictions(findings []app.Finding) []app.Finding {
	var out []app.Finding
	for _, finding := range findings {
		if finding.Kind == app.FindingSuspectedContradiction {
			out = append(out, finding)
		}
	}
	return out
}

// TestFindingsSurfaceSuspectedContradiction proves the kind is registered in
// the review pipeline: two decisions written by the normal write path are
// flagged when their numbers disagree, with both ids and a report-only shape.
func TestFindingsSurfaceSuspectedContradiction(t *testing.T) {
	ctx := context.Background()
	a, clock := newContradictionFixture(t)

	first, err := a.Decision(ctx, app.DecisionInput{
		ProjectID: "p", CreateProject: true,
		Title: "Retention window is 90 days", Context: "Artifacts live 90 days.",
	})
	if err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Minute)
	second, err := a.Decision(ctx, app.DecisionInput{
		ProjectID: "p",
		Title:     "Retention window is 180 days", Context: "Artifacts live 180 days.",
	})
	if err != nil {
		t.Fatal(err)
	}

	findings, err := a.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	got := suspectedContradictions(findings)
	if len(got) != 1 {
		t.Fatalf("suspected contradictions = %d %+v, want 1", len(got), got)
	}
	finding := got[0]
	if finding.Severity != app.SeverityWarn {
		t.Fatalf("severity = %q, want warn", finding.Severity)
	}
	if finding.Fixable {
		t.Fatalf("suspected contradiction is fixable: %+v", finding)
	}
	for _, id := range []string{first.Decision.ID, second.Decision.ID} {
		if !strings.Contains(finding.Message, id) {
			t.Fatalf("message omits %q: %q", id, finding.Message)
		}
	}
	if !strings.Contains(finding.Message, "numeric") || !strings.Contains(finding.Message, "90 days") || !strings.Contains(finding.Message, "180 days") {
		t.Fatalf("message lacks the numeric reason: %q", finding.Message)
	}
	if !strings.Contains(finding.Next, "open both records") || !strings.Contains(finding.Confirm, "true now") {
		t.Fatalf("next/confirm do not ask for a decision: %+v", finding)
	}
	if _, err := app.ParseFindingKinds("suspected-contradiction"); err != nil {
		t.Fatalf("kind is not registered: %v", err)
	}
}

// TestFindingsHideSuspectedContradictionWhenSuperseded proves the default
// review keeps current beliefs only: a properly superseded pair is gone.
func TestFindingsHideSuspectedContradictionWhenSuperseded(t *testing.T) {
	ctx := context.Background()
	a, clock := newContradictionFixture(t)

	first, err := a.Decision(ctx, app.DecisionInput{
		ProjectID: "p", CreateProject: true,
		Title: "Retention window is 90 days", Context: "Artifacts live 90 days.",
	})
	if err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Minute)
	if _, err := a.Decision(ctx, app.DecisionInput{
		ProjectID: "p", Supersedes: first.Decision.ID,
		Title: "Retention window is 180 days", Context: "Artifacts live 180 days.",
	}); err != nil {
		t.Fatal(err)
	}

	findings, err := a.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if got := suspectedContradictions(findings); len(got) != 0 {
		t.Fatalf("superseded pair still reported: %+v", got)
	}
}
