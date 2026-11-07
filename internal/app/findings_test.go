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

type reviewFixture struct {
	app   *app.App
	clock *time.Time
}

func newReviewFixture(t *testing.T) reviewFixture {
	t.Helper()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.User = "alice"
	a := app.New(cfg, app.Dependencies{Store: store, Journal: journal.New(dataDir), Now: func() time.Time { return clock }})
	if _, err := a.Note(context.Background(), app.NoteInput{ProjectID: "p", Body: "seed", CreateProject: true}); err != nil {
		t.Fatal(err)
	}
	return reviewFixture{app: a, clock: &clock}
}

func (f reviewFixture) tick() { *f.clock = f.clock.Add(time.Minute) }

func kindsOf(findings []app.Finding) map[app.FindingKind]int {
	kinds := map[app.FindingKind]int{}
	for _, finding := range findings {
		kinds[finding.Kind]++
	}
	return kinds
}

func TestFindingsFixNextActionAndAnsweredQuestion(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t)
	next := "write the parser"
	questions := []string{"Which storage engine should the index use?", "Who owns the release key?"}
	if _, err := f.app.SetState(ctx, app.StateInput{ProjectID: "p", NextAction: &next, OpenQuestions: &questions}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if _, err := f.app.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "parser done", NextAction: "ship the release"}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if _, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Index storage engine: SQLite FTS5"}); err != nil {
		t.Fatal(err)
	}

	findings, err := f.app.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	kinds := kindsOf(findings)
	if kinds[app.FindingNextBehindHandoff] != 1 || kinds[app.FindingQuestionAnswered] != 1 || len(findings) != 2 {
		t.Fatalf("findings = %+v", findings)
	}

	f.tick()
	result, err := f.app.FixFindings(ctx, "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 2 || result.State.NextAction != "ship the release" ||
		len(result.State.OpenQuestions) != 1 || result.State.OpenQuestions[0] != "Who owns the release key?" {
		t.Fatalf("fix result = %+v", result)
	}
	if after, err := f.app.Findings(ctx, "p"); err != nil || len(after) != 0 {
		t.Fatalf("findings after fix = %+v, %v", after, err)
	}
	recent, err := f.app.Catchup(ctx, app.CatchupOptions{ProjectID: "p", Since: f.clock.Add(-time.Second)})
	if err != nil || len(recent.Notes) != 1 || !strings.Contains(recent.Notes[0].Body, "Which storage engine") {
		t.Fatalf("removed question not kept in a note: %+v %v", recent.Notes, err)
	}
}

func TestFindingsFixKindFilter(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t)
	next := "old"
	questions := []string{"storage engine for the index"}
	if _, err := f.app.SetState(ctx, app.StateInput{ProjectID: "p", NextAction: &next, OpenQuestions: &questions}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if _, err := f.app.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "s", NextAction: "new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Index storage engine is SQLite"}); err != nil {
		t.Fatal(err)
	}
	kinds, err := app.ParseFindingKinds("question-answered")
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.app.FixFindings(ctx, "p", kinds)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || result.State.NextAction != "old" || len(result.State.OpenQuestions) != 0 {
		t.Fatalf("filtered fix = %+v", result)
	}
	if _, err := app.ParseFindingKinds("next-behind-handoff, bogus"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestFindingsReportOnlyHeuristics(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t)
	next := "migrate the journal to union merge"
	fresh := f.clock.Add(time.Hour)
	if _, err := f.app.SetState(ctx, app.StateInput{ProjectID: "p", NextAction: &next, FreshUntil: &fresh}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Sync journals with git union merge"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		f.tick()
		subject := "chore: tidy"
		if i == 9 {
			subject = "Journal: union merge migration"
		}
		if _, err := f.app.RecordCommit(ctx, app.CommitInput{ProjectID: "p", Head: "c" + string(rune('0'+i)), Subject: subject}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.app.Attempt(ctx, app.AttemptInput{ProjectID: "p", Title: "Union merge for journals", Hypothesis: "git union merge keeps both sides", Result: "duplicated lines"}); err != nil {
		t.Fatal(err)
	}
	*f.clock = f.clock.Add(2 * time.Hour)

	findings, err := f.app.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	kinds := kindsOf(findings)
	for _, kind := range []app.FindingKind{app.FindingStateExpired, app.FindingStateBehindCommits, app.FindingNextMaybeDone, app.FindingDecisionContradicted} {
		if kinds[kind] != 1 {
			t.Fatalf("missing %s in %+v", kind, findings)
		}
	}
	for _, finding := range findings {
		if finding.Fixable {
			t.Fatalf("report-only finding marked fixable: %+v", finding)
		}
		if finding.Kind == app.FindingNextMaybeDone && !strings.Contains(finding.Evidence, "c9") {
			t.Fatalf("next-maybe-done evidence = %q", finding.Evidence)
		}
	}
	result, err := f.app.FixFindings(ctx, "p", nil)
	if err != nil || len(result.Applied) != 0 {
		t.Fatalf("report-only fix = %+v, %v", result, err)
	}
}
