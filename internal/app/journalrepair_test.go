package app_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/journal"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func newJournalApp(t *testing.T) (*app.App, string) {
	t.Helper()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.User = "alice"
	return app.New(cfg, app.Dependencies{Store: store, Journal: journal.New(dataDir)}), dataDir
}

func TestRetriedWriteIsJournaledOnce(t *testing.T) {
	ctx := context.Background()
	a, _ := newJournalApp(t)
	for range 2 {
		if _, err := a.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Same", Body: "same body", CreateProject: true}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := a.ReadJournal(ctx, ports.JournalQuery{Event: app.JournalNote})
	if err != nil || len(entries) != 1 {
		t.Fatalf("journal note lines = %+v, %v", entries, err)
	}
}

// The journal adapter returns 50 lines unless told otherwise; gap detection
// must see older lines too, or repair would duplicate them.
func TestJournalGapsSeesMoreThanTheDefaultReadLimit(t *testing.T) {
	ctx := context.Background()
	a, _ := newJournalApp(t)
	for i := range 60 {
		if _, err := a.Note(ctx, app.NoteInput{ProjectID: "p", Title: "n", Body: fmt.Sprintf("body %d", i), CreateProject: true}); err != nil {
			t.Fatal(err)
		}
	}
	gaps, err := a.JournalGaps(ctx, time.Now().Add(-time.Hour))
	if err != nil || len(gaps) != 0 {
		t.Fatalf("gaps with a complete 60-line journal = %d, %v", len(gaps), err)
	}
}

func TestRepairJournalFillsMissingLines(t *testing.T) {
	ctx := context.Background()
	a, dataDir := newJournalApp(t)
	result, err := a.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Lost line", Body: "x", CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a journal append that failed after the record was stored.
	if err := os.Remove(filepath.Join(dataDir, "journal", "alice.jsonl")); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-time.Hour)
	gaps, err := a.JournalGaps(ctx, since)
	if err != nil || len(gaps) != 1 || gaps[0].ID != result.Note.ID {
		t.Fatalf("gaps = %+v, %v", gaps, err)
	}
	repaired, err := a.RepairJournal(ctx, since)
	if err != nil || len(repaired) != 1 {
		t.Fatalf("repaired = %+v, %v", repaired, err)
	}
	entries, err := a.ReadJournal(ctx, ports.JournalQuery{Event: app.JournalNote})
	if err != nil || len(entries) != 1 || entries[0].ID != result.Note.ID || entries[0].Fields["repaired"] != "true" {
		t.Fatalf("journal after repair = %+v, %v", entries, err)
	}
	if again, err := a.JournalGaps(ctx, since); err != nil || len(again) != 0 {
		t.Fatalf("gaps after repair = %+v, %v", again, err)
	}
}
