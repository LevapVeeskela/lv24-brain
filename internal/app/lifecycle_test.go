package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestRecordLifecycle(t *testing.T) {
	ctx := context.Background()
	a, _ := newJournalApp(t)
	old, err := a.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Use JSON files", CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := a.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Use SQLite"})
	if err != nil {
		t.Fatal(err)
	}
	note, err := a.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Scratch", Body: "temporary"})
	if err != nil {
		t.Fatal(err)
	}

	listed, err := a.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p", Kinds: []string{ports.RecordDecision}})
	if err != nil || len(listed) != 2 || listed[0].Status != string(domain.DecisionProposed) {
		t.Fatalf("decisions = %+v, %v", listed, err)
	}
	if _, err := a.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p", Kinds: []string{"bogus"}}); err == nil {
		t.Fatal("unknown kind accepted")
	}

	accepted, err := a.SetDecisionStatus(ctx, "p", replacement.Decision.ID, domain.DecisionAccepted)
	if err != nil || accepted.Status != domain.DecisionAccepted {
		t.Fatalf("accept = %+v, %v", accepted, err)
	}
	superseded, err := a.Supersede(ctx, "p", old.Decision.ID, replacement.Decision.ID)
	if err != nil || superseded.Status != domain.DecisionSuperseded || len(superseded.Evidence) != 1 {
		t.Fatalf("supersede = %+v, %v", superseded, err)
	}
	reread, err := a.GetRecord(ctx, "p", old.Decision.ID)
	if err != nil || reread.Decision == nil || reread.Decision.Status != domain.DecisionSuperseded {
		t.Fatalf("reread = %+v, %v", reread, err)
	}
	if _, err := a.Supersede(ctx, "p", old.Decision.ID, note.Note.ID); err == nil {
		t.Fatal("superseded by a note")
	}
	accepts, err := a.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p", Status: "accepted"})
	if err != nil || len(accepts) != 1 || accepts[0].ID != replacement.Decision.ID {
		t.Fatalf("accepted decisions = %+v, %v", accepts, err)
	}

	if _, err := a.TrashRecord(ctx, "p", note.Note.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetRecord(ctx, "p", note.Note.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("trashed note still live: %v", err)
	}
	trash, err := a.ListTrash(ctx, "p")
	if err != nil || len(trash) != 1 || trash[0].ID != note.Note.ID {
		t.Fatalf("trash = %+v, %v", trash, err)
	}
	restored, err := a.RestoreRecord(ctx, "p", note.Note.ID)
	if err != nil || restored.Note == nil || restored.Note.Body != "temporary" {
		t.Fatalf("restore = %+v, %v", restored, err)
	}

	events := map[string]int{}
	entries, err := a.ReadJournal(ctx, ports.JournalQuery{Project: "p", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		events[entry.Event]++
	}
	if events[app.JournalStatus] != 2 || events[app.JournalTrash] != 1 || events[app.JournalRestore] != 1 {
		t.Fatalf("journal events = %v", events)
	}
}
