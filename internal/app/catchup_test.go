package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/journal"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
)

func TestCatchupReportsOnlyWhatHappenedAfterTheLatestHandoff(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.User = "alice"
	a := app.New(cfg, app.Dependencies{Store: store, Journal: journal.New(dataDir), Now: now})
	tick := func() { clock = clock.Add(time.Minute) }

	if _, err := a.Note(ctx, app.NoteInput{ProjectID: "p", Body: "before the handoff", CreateProject: true}); err != nil {
		t.Fatal(err)
	}
	tick()
	if _, err := a.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "session one", NextAction: "do X"}); err != nil {
		t.Fatal(err)
	}
	// Nothing yet.
	if result, err := a.Catchup(ctx, app.CatchupOptions{ProjectID: "p"}); err != nil || !result.Empty() || result.SinceSource != "handoff" {
		t.Fatalf("right after handoff = %+v, %v", result, err)
	}

	tick()
	if _, err := a.Note(ctx, app.NoteInput{ProjectID: "p", Body: "another agent wrote this"}); err != nil {
		t.Fatal(err)
	}
	tick()
	if _, err := a.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Use SQLite"}); err != nil {
		t.Fatal(err)
	}
	tick()
	goal := "ship"
	if _, err := a.SetState(ctx, app.StateInput{ProjectID: "p", Goal: &goal}); err != nil {
		t.Fatal(err)
	}
	tick()
	if _, err := a.RecordCommit(ctx, app.CommitInput{ProjectID: "p", Head: "abc1234", Subject: "fix"}); err != nil {
		t.Fatal(err)
	}

	result, err := a.Catchup(ctx, app.CatchupOptions{ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Baseline == nil || result.Baseline.Summary != "session one" {
		t.Fatalf("baseline = %+v", result.Baseline)
	}
	if len(result.Notes) != 1 || result.Notes[0].Body != "another agent wrote this" {
		t.Fatalf("notes = %+v", result.Notes)
	}
	if len(result.Decisions) != 1 || !result.StateChanged || len(result.Handoffs) != 0 {
		t.Fatalf("decisions=%d stateChanged=%v handoffs=%d", len(result.Decisions), result.StateChanged, len(result.Handoffs))
	}
	events := map[string]int{}
	for _, entry := range result.Journal {
		events[entry.Event]++
	}
	if events[app.JournalCommit] != 1 || events[app.JournalState] != 1 || events[app.JournalNote] != 0 {
		t.Fatalf("journal events = %v", events)
	}

	// An explicit window overrides the handoff baseline.
	result, err = a.Catchup(ctx, app.CatchupOptions{ProjectID: "p", Since: clock.Add(-90 * time.Second)})
	if err != nil || result.SinceSource != "explicit" || len(result.Notes) != 0 || !result.StateChanged {
		t.Fatalf("explicit since = %+v, %v", result, err)
	}
	if _, err := a.Catchup(ctx, app.CatchupOptions{ProjectID: "missing"}); err == nil {
		t.Fatal("catchup of an unknown project succeeded")
	}
}
