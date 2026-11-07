package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/journal"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestDigestCoversWindowAndNeedsYou(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.User = "alice"
	a := app.New(cfg, app.Dependencies{Store: store, Journal: journal.New(dataDir), Now: now})

	// Long before the window: a proposed decision that still needs the owner,
	// and a quiet project with one old note.
	if _, err := a.Decision(ctx, app.DecisionInput{ProjectID: "alpha", Title: "Adopt queue", Status: domain.DecisionProposed, CreateProject: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Note(ctx, app.NoteInput{ProjectID: "quiet", Body: "old note", CreateProject: true}); err != nil {
		t.Fatal(err)
	}

	clock = time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	since := clock
	clock = clock.Add(time.Hour)
	if _, err := a.Note(ctx, app.NoteInput{ProjectID: "alpha", Body: "window note"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Decision(ctx, app.DecisionInput{ProjectID: "alpha", Title: "Use SQLite", Status: domain.DecisionAccepted}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	if _, err := a.Handoff(ctx, app.HandoffInput{ProjectID: "alpha", Summary: "Shipped parser", NextAction: "Write tests"}); err != nil {
		t.Fatal(err)
	}
	goal := "ship digest"
	openQuestions := []string{"Which DB?"}
	blocked := []string{"waiting on API key"}
	clock = clock.Add(time.Minute)
	if _, err := a.SetState(ctx, app.StateInput{ProjectID: "alpha", Goal: &goal, OpenQuestions: &openQuestions, Blocked: &blocked}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	if _, err := a.Attempt(ctx, app.AttemptInput{ProjectID: "beta", Title: "Try cache", CreateProject: true}); err != nil {
		t.Fatal(err)
	}

	digest, err := a.Digest(ctx, app.DigestOptions{Since: since})
	if err != nil {
		t.Fatal(err)
	}
	if len(digest.Projects) != 2 {
		t.Fatalf("projects = %+v, want alpha and beta only", digest.Projects)
	}
	alpha, beta := digest.Projects[0], digest.Projects[1]
	if alpha.Project.ID != "alpha" || beta.Project.ID != "beta" {
		t.Fatalf("order = %s, %s; want alpha (more activity) first", alpha.Project.ID, beta.Project.ID)
	}
	if alpha.LatestHandoff == nil || alpha.LatestHandoff.Summary != "Shipped parser" {
		t.Fatalf("latest handoff = %+v", alpha.LatestHandoff)
	}
	if len(alpha.Activity.Notes) != 1 || len(alpha.Activity.Handoffs) != 1 || !alpha.Activity.StateChanged || !hasDecision(alpha.Activity.Decisions, "Use SQLite") {
		t.Fatalf("alpha activity = %+v", alpha.Activity)
	}
	if len(alpha.ProposedDecisions) != 1 || alpha.ProposedDecisions[0].Title != "Adopt queue" {
		t.Fatalf("proposed decisions outside the window must still be listed: %+v", alpha.ProposedDecisions)
	}
	if len(alpha.OpenQuestions) != 1 || alpha.OpenQuestions[0] != "Which DB?" || len(alpha.Blocked) != 1 {
		t.Fatalf("open questions = %v, blocked = %v", alpha.OpenQuestions, alpha.Blocked)
	}
	if len(beta.Activity.Attempts) != 1 {
		t.Fatalf("beta attempts = %+v", beta.Activity.Attempts)
	}

	scoped, err := a.Digest(ctx, app.DigestOptions{ProjectID: "alpha", Since: since})
	if err != nil || len(scoped.Projects) != 1 || scoped.Projects[0].Project.ID != "alpha" {
		t.Fatalf("scoped digest = %+v, %v", scoped, err)
	}
	if _, err := a.Digest(ctx, app.DigestOptions{ProjectID: "missing", Since: since}); err == nil {
		t.Fatal("digest of an unknown project succeeded")
	}
}

func TestDigestDefaultsToTheLast24Hours(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.User = "alice"
	a := app.New(cfg, app.Dependencies{Store: store, Journal: journal.New(dataDir), Now: func() time.Time { return clock }})
	if _, err := a.Note(ctx, app.NoteInput{ProjectID: "alpha", Body: "old", CreateProject: true}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(48 * time.Hour)

	digest, err := a.Digest(ctx, app.DigestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !digest.Since.Equal(clock.Add(-app.DefaultDigestWindow)) || len(digest.Projects) != 0 {
		t.Fatalf("digest = %+v", digest)
	}
}

// hasDecision reports whether a decision with title is in the list. The fake
// clock does not stamp the proposed decision's UpdatedAt, so it may also
// appear there; the assertion only requires the new decision.
func hasDecision(decisions []domain.Decision, title string) bool {
	for _, decision := range decisions {
		if decision.Title == title {
			return true
		}
	}
	return false
}
