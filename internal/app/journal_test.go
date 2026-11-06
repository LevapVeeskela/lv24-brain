package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/journal"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestWritesAndCommitsAreJournaledPerUser(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.DataDir = dataDir
	cfg.User = "alice"
	a := app.New(cfg, app.Dependencies{Store: store, Journal: journal.New(dataDir)})

	note, err := a.Note(ctx, app.NoteInput{ProjectID: "demo", Title: "Hello", Body: "first note"})
	if err != nil {
		t.Fatalf("Note: %v", err)
	}
	decision, err := a.Decision(ctx, app.DecisionInput{ProjectID: "demo", Title: "Use Go"})
	if err != nil {
		t.Fatalf("Decision: %v", err)
	}
	if _, err := a.RecordCommit(ctx, app.CommitInput{ProjectID: "demo", Branch: "main", Head: "abc1234", Subject: "init"}); err != nil {
		t.Fatalf("RecordCommit: %v", err)
	}

	entries, err := a.ReadJournal(ctx, ports.JournalQuery{Project: "demo"})
	if err != nil || len(entries) != 3 {
		t.Fatalf("journal = %+v, %v", entries, err)
	}
	byEvent := map[string]ports.JournalEntry{}
	for _, entry := range entries {
		if entry.User != "alice" {
			t.Fatalf("entry user = %q, want alice", entry.User)
		}
		byEvent[entry.Event] = entry
	}
	if byEvent[app.JournalNote].ID != note.Note.ID || byEvent[app.JournalDecision].ID != decision.Decision.ID {
		t.Fatalf("record events = %+v", byEvent)
	}
	if commit := byEvent[app.JournalCommit]; commit.Fields["head"] != "abc1234" || commit.Fields["branch"] != "main" {
		t.Fatalf("commit event = %+v", commit)
	}
	if _, err := a.RecordCommit(ctx, app.CommitInput{ProjectID: "demo"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("commit without head error = %v", err)
	}
}

func TestJournalIsUnsupportedInServerMode(t *testing.T) {
	cfg := config.Default()
	cfg.Mode = config.ModeServer
	a := app.New(cfg, app.Dependencies{})
	if _, err := a.ReadJournal(context.Background(), ports.JournalQuery{}); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("server-mode journal error = %v", err)
	}
}
