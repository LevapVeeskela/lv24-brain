package httpapi

import (
	"context"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// TestMemoryStoreRecordsOwner checks that every knowledge kind stamps the
// creator's user ID on create and keeps it immutable across updates.
func TestMemoryStoreRecordsOwner(t *testing.T) {
	store := NewMemoryStore()
	creator := ports.WithActorID(context.Background(), "user_1")
	other := ports.WithActorID(context.Background(), "user_2")

	if _, _, err := store.CreateProject(creator, domain.Project{ID: "p1", Name: "Project p1", Kind: domain.ProjectKindApp}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	t.Run("note", func(t *testing.T) {
		created, _, err := store.CreateNote(creator, domain.Note{ID: "note_owner", ProjectID: "p1", Body: "body"})
		if err != nil {
			t.Fatalf("CreateNote: %v", err)
		}
		if created.CreatedBy != "user_1" {
			t.Fatalf("CreateNote CreatedBy = %q, want user_1", created.CreatedBy)
		}
		got, err := store.GetNote(creator, "note_owner")
		if err != nil || got.CreatedBy != "user_1" {
			t.Fatalf("GetNote = (%+v, %v), want CreatedBy user_1", got, err)
		}
		updated, _, err := store.UpdateNote(other, domain.Note{ID: "note_owner", ProjectID: "p1", Body: "edited"}, 0)
		if err != nil {
			t.Fatalf("UpdateNote: %v", err)
		}
		if updated.CreatedBy != "user_1" {
			t.Fatalf("UpdateNote CreatedBy = %q, want user_1", updated.CreatedBy)
		}
		got, err = store.GetNote(other, "note_owner")
		if err != nil || got.CreatedBy != "user_1" {
			t.Fatalf("GetNote after update = (%+v, %v), want CreatedBy user_1", got, err)
		}
	})

	t.Run("decision", func(t *testing.T) {
		created, _, err := store.CreateDecision(creator, domain.Decision{ID: "decision_owner", ProjectID: "p1", Title: "Decide"})
		if err != nil {
			t.Fatalf("CreateDecision: %v", err)
		}
		if created.CreatedBy != "user_1" {
			t.Fatalf("CreateDecision CreatedBy = %q, want user_1", created.CreatedBy)
		}
		updated, _, err := store.UpdateDecision(other, domain.Decision{ID: "decision_owner", ProjectID: "p1", Title: "Decided", Status: domain.DecisionAccepted}, 0)
		if err != nil {
			t.Fatalf("UpdateDecision: %v", err)
		}
		if updated.CreatedBy != "user_1" {
			t.Fatalf("UpdateDecision CreatedBy = %q, want user_1", updated.CreatedBy)
		}
		got, err := store.GetDecision(other, "decision_owner")
		if err != nil || got.CreatedBy != "user_1" {
			t.Fatalf("GetDecision after update = (%+v, %v), want CreatedBy user_1", got, err)
		}
	})

	t.Run("attempt", func(t *testing.T) {
		created, _, err := store.CreateAttempt(creator, domain.Attempt{ID: "attempt_owner", ProjectID: "p1", Title: "Try"})
		if err != nil {
			t.Fatalf("CreateAttempt: %v", err)
		}
		if created.CreatedBy != "user_1" {
			t.Fatalf("CreateAttempt CreatedBy = %q, want user_1", created.CreatedBy)
		}
		updated, _, err := store.UpdateAttempt(other, domain.Attempt{ID: "attempt_owner", ProjectID: "p1", Title: "Tried"}, 0)
		if err != nil {
			t.Fatalf("UpdateAttempt: %v", err)
		}
		if updated.CreatedBy != "user_1" {
			t.Fatalf("UpdateAttempt CreatedBy = %q, want user_1", updated.CreatedBy)
		}
		got, err := store.GetAttempt(other, "attempt_owner")
		if err != nil || got.CreatedBy != "user_1" {
			t.Fatalf("GetAttempt after update = (%+v, %v), want CreatedBy user_1", got, err)
		}
	})

	t.Run("handoff", func(t *testing.T) {
		created, _, err := store.CreateHandoff(creator, domain.Handoff{ID: "handoff_owner", ProjectID: "p1", Summary: "Done"})
		if err != nil {
			t.Fatalf("CreateHandoff: %v", err)
		}
		if created.CreatedBy != "user_1" {
			t.Fatalf("CreateHandoff CreatedBy = %q, want user_1", created.CreatedBy)
		}
		got, err := store.GetHandoff(other, "handoff_owner")
		if err != nil || got.CreatedBy != "user_1" {
			t.Fatalf("GetHandoff = (%+v, %v), want CreatedBy user_1", got, err)
		}
	})
}
