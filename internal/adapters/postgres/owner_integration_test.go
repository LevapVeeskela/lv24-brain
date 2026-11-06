package postgres

import (
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// TestKnowledgeRecordsKeepOwner checks that created_by is written on insert,
// returned by Get/List, and never changed by an update made by another user.
func TestKnowledgeRecordsKeepOwner(t *testing.T) {
	store := newTestStore(t)
	base := testContext(t)
	creator := ports.WithActorID(WithActor(base, "creator"), "user_1")
	other := ports.WithActorID(WithActor(base, "other"), "user_2")
	project := mustProject(t, creator, store, "owner_p")

	note, _, err := store.CreateNote(creator, domain.Note{ProjectID: project.ID, Body: "Body"})
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if note.CreatedBy != "user_1" {
		t.Fatalf("CreateNote CreatedBy = %q, want user_1", note.CreatedBy)
	}
	note.Body = "Edited"
	updatedNote, _, err := store.UpdateNote(other, note.Note, note.Revision)
	if err != nil {
		t.Fatalf("UpdateNote: %v", err)
	}
	if updatedNote.CreatedBy != "user_1" {
		t.Fatalf("UpdateNote CreatedBy = %q, want user_1", updatedNote.CreatedBy)
	}
	gotNote, err := store.GetNote(other, note.ID)
	if err != nil || gotNote.CreatedBy != "user_1" {
		t.Fatalf("GetNote = (%+v, %v), want CreatedBy user_1", gotNote, err)
	}
	notes, err := store.ListNotes(other, ListOptions{ProjectID: project.ID})
	if err != nil || len(notes) != 1 || notes[0].CreatedBy != "user_1" {
		t.Fatalf("ListNotes = (%+v, %v), want one note owned by user_1", notes, err)
	}

	decision, _, err := store.CreateDecision(creator, domain.Decision{ProjectID: project.ID, Title: "Decide"})
	if err != nil {
		t.Fatalf("CreateDecision: %v", err)
	}
	if decision.CreatedBy != "user_1" {
		t.Fatalf("CreateDecision CreatedBy = %q, want user_1", decision.CreatedBy)
	}
	updatedDecision, _, err := store.UpdateDecision(other, domain.Decision{
		ID: decision.ID, ProjectID: project.ID, Title: "Decided", Status: domain.DecisionAccepted,
	}, decision.Revision)
	if err != nil {
		t.Fatalf("UpdateDecision: %v", err)
	}
	if updatedDecision.CreatedBy != "user_1" {
		t.Fatalf("UpdateDecision CreatedBy = %q, want user_1", updatedDecision.CreatedBy)
	}
	gotDecision, err := store.GetDecision(other, decision.ID)
	if err != nil || gotDecision.CreatedBy != "user_1" {
		t.Fatalf("GetDecision = (%+v, %v), want CreatedBy user_1", gotDecision, err)
	}

	attempt, _, err := store.CreateAttempt(creator, domain.Attempt{ProjectID: project.ID, Title: "Try"})
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	if attempt.CreatedBy != "user_1" {
		t.Fatalf("CreateAttempt CreatedBy = %q, want user_1", attempt.CreatedBy)
	}
	gotAttempt, err := store.GetAttempt(other, attempt.ID)
	if err != nil || gotAttempt.CreatedBy != "user_1" {
		t.Fatalf("GetAttempt = (%+v, %v), want CreatedBy user_1", gotAttempt, err)
	}

	handoff, _, err := store.CreateHandoff(creator, domain.Handoff{ProjectID: project.ID, Summary: "Done"})
	if err != nil {
		t.Fatalf("CreateHandoff: %v", err)
	}
	if handoff.CreatedBy != "user_1" {
		t.Fatalf("CreateHandoff CreatedBy = %q, want user_1", handoff.CreatedBy)
	}
	gotHandoff, err := store.GetHandoff(other, handoff.ID)
	if err != nil || gotHandoff.CreatedBy != "user_1" {
		t.Fatalf("GetHandoff = (%+v, %v), want CreatedBy user_1", gotHandoff, err)
	}
}
