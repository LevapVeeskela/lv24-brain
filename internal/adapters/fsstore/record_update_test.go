package fsstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func setStoreClock(store *Store, at time.Time) {
	store.now = func() time.Time { return at }
}

func TestUpdateRecordTextKeepsIdentityForEveryKind(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	created := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	review := domain.ReviewSchedule{ReviewAfter: created.Add(30 * 24 * time.Hour)}
	validity := domain.Validity{ValidFrom: created, Confidence: 0.6}
	relations := domain.RelationList{{
		Target: "decision-target", Relation: domain.RelationRelatesTo,
		Status: domain.RelationAccepted, VerdictBy: domain.VerdictByHuman,
	}}
	provenance := domain.Provenance{Origin: domain.OriginImport, Reviewed: false, Trust: domain.TrustStated}

	t.Run("note", func(t *testing.T) {
		original := domain.Note{
			ProjectID: project.ID, ID: "note-edit", Title: "Note", Body: "old text",
			Tags: []string{"a"}, Provenance: provenance, Validity: validity,
			Relations: relations, ReviewSchedule: review, CreatedAt: created,
		}
		if _, err := store.AppendNote(ctx, original); err != nil {
			t.Fatal(err)
		}
		before := onlyNote(t, store, project.ID, original.ID)
		pathBefore := onlyFile(t, filepath.Join(store.Root(), "projects", project.ID, notesDir))

		next := before
		next.Title, next.Body, next.Tags = "Note edited", "new text", []string{"b"}
		setStoreClock(store, testClock.Add(time.Minute))
		if err := store.UpdateRecordText(ctx, ports.RecordTextUpdate{Note: &next, ExpectedUpdatedAt: before.UpdatedAt}); err != nil {
			t.Fatalf("UpdateRecordText: %v", err)
		}
		after := onlyNote(t, store, project.ID, original.ID)
		if after.Title != "Note edited" || after.Body != "new text" || len(after.Tags) != 1 || after.Tags[0] != "b" {
			t.Fatalf("text = %+v", after)
		}
		if after.ID != before.ID || !after.CreatedAt.Equal(before.CreatedAt) ||
			after.Provenance != before.Provenance || after.Provenance.Reviewed ||
			after.Provenance.Origin != domain.OriginImport ||
			after.Validity.Confidence != 0.6 ||
			len(after.Relations) != 1 || !after.ReviewSchedule.ReviewAfter.Equal(review.ReviewAfter) {
			t.Fatalf("metadata changed: %+v", after)
		}
		if pathAfter := onlyFile(t, filepath.Join(store.Root(), "projects", project.ID, notesDir)); pathAfter != pathBefore {
			t.Fatalf("path changed: %q -> %q", pathBefore, pathAfter)
		}
		if !after.UpdatedAt.Equal(testClock.Add(time.Minute)) {
			t.Fatalf("updated_at = %v", after.UpdatedAt)
		}

		stale := before.UpdatedAt
		if err := store.UpdateRecordText(ctx, ports.RecordTextUpdate{Note: &after, ExpectedUpdatedAt: stale}); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("stale note edit = %v, want ErrConflict", err)
		}
	})

	t.Run("decision", func(t *testing.T) {
		original := editTestDecision(project.ID, "dec-edit", "Decision")
		original.Provenance = provenance
		original.Validity = validity
		original.Relations = relations
		original.ReviewSchedule = review
		if _, err := store.AppendDecision(ctx, original); err != nil {
			t.Fatal(err)
		}
		before := onlyDecision(t, store, project.ID, original.ID)
		pathBefore := onlyFile(t, filepath.Join(store.Root(), "projects", project.ID, decisionsDir))

		next := before
		next.Title, next.Context = "Decision edited", "new context"
		setStoreClock(store, testClock.Add(2*time.Minute))
		if err := store.UpdateRecordText(ctx, ports.RecordTextUpdate{Decision: &next, ExpectedUpdatedAt: before.UpdatedAt}); err != nil {
			t.Fatalf("UpdateRecordText: %v", err)
		}
		after := onlyDecision(t, store, project.ID, original.ID)
		if after.Title != "Decision edited" || after.Context != "new context" || after.Status != domain.DecisionProposed {
			t.Fatalf("text/status = %+v", after)
		}
		if after.ID != before.ID || !after.CreatedAt.Equal(before.CreatedAt) ||
			after.Provenance != before.Provenance || after.Validity.Confidence != 0.6 ||
			len(after.Relations) != 1 || !after.ReviewSchedule.ReviewAfter.Equal(review.ReviewAfter) {
			t.Fatalf("metadata changed: %+v", after)
		}
		if pathAfter := onlyFile(t, filepath.Join(store.Root(), "projects", project.ID, decisionsDir)); pathAfter != pathBefore {
			t.Fatalf("path changed: %q -> %q", pathBefore, pathAfter)
		}
	})

	t.Run("attempt", func(t *testing.T) {
		original := domain.Attempt{
			ProjectID: project.ID, ID: "att-edit", Title: "Attempt",
			Hypothesis: "h", Result: "r", Reason: "why",
			Provenance: provenance, Validity: validity, Relations: relations,
			ReviewSchedule: review, CreatedAt: created,
		}
		if _, err := store.AppendAttempt(ctx, original); err != nil {
			t.Fatal(err)
		}
		before := onlyAttempt(t, store, project.ID, original.ID)
		next := before
		next.Result = "new result"
		setStoreClock(store, testClock.Add(3*time.Minute))
		if err := store.UpdateRecordText(ctx, ports.RecordTextUpdate{Attempt: &next, ExpectedUpdatedAt: before.UpdatedAt}); err != nil {
			t.Fatalf("UpdateRecordText: %v", err)
		}
		after := onlyAttempt(t, store, project.ID, original.ID)
		if after.Result != "new result" || after.Hypothesis != "h" || after.Reason != "why" {
			t.Fatalf("text = %+v", after)
		}
		if after.Provenance != before.Provenance || after.Validity.Confidence != 0.6 || len(after.Relations) != 1 {
			t.Fatalf("metadata changed: %+v", after)
		}
	})

	t.Run("handoff", func(t *testing.T) {
		original := domain.Handoff{
			ProjectID: project.ID, ID: "hand-edit", Summary: "old summary",
			NextAction: "next", GitContext: "main",
			Provenance: provenance, Validity: validity, Relations: relations,
			ReviewSchedule: review, CreatedAt: created,
		}
		if _, err := store.AppendHandoff(ctx, original); err != nil {
			t.Fatal(err)
		}
		before := onlyHandoff(t, store, project.ID, original.ID)
		next := before
		next.Summary = "new summary"
		setStoreClock(store, testClock.Add(4*time.Minute))
		if err := store.UpdateRecordText(ctx, ports.RecordTextUpdate{Handoff: &next, ExpectedUpdatedAt: before.UpdatedAt}); err != nil {
			t.Fatalf("UpdateRecordText: %v", err)
		}
		after := onlyHandoff(t, store, project.ID, original.ID)
		if after.Summary != "new summary" || after.NextAction != "next" || after.GitContext != "main" {
			t.Fatalf("text = %+v", after)
		}
		if after.Provenance != before.Provenance || after.Validity.Confidence != 0.6 || len(after.Relations) != 1 {
			t.Fatalf("metadata changed: %+v", after)
		}
		if !after.UpdatedAt.Equal(testClock.Add(4 * time.Minute)) {
			t.Fatalf("handoff updated_at = %v", after.UpdatedAt)
		}
	})
}

// onlyNote returns one stored note after an edit.
func onlyNote(t *testing.T, store *Store, projectID, id string) domain.Note {
	t.Helper()
	notes, err := store.ListNotes(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, note := range notes {
		if note.ID == id {
			return note
		}
	}
	t.Fatalf("note %q not found in %+v", id, notes)
	return domain.Note{}
}

func onlyDecision(t *testing.T, store *Store, projectID, id string) domain.Decision {
	t.Helper()
	decisions, err := store.ListDecisions(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range decisions {
		if decision.ID == id {
			return decision
		}
	}
	t.Fatalf("decision %q not found", id)
	return domain.Decision{}
}

func onlyAttempt(t *testing.T, store *Store, projectID, id string) domain.Attempt {
	t.Helper()
	attempts, err := store.ListAttempts(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range attempts {
		if attempt.ID == id {
			return attempt
		}
	}
	t.Fatalf("attempt %q not found", id)
	return domain.Attempt{}
}

func onlyHandoff(t *testing.T, store *Store, projectID, id string) domain.Handoff {
	t.Helper()
	handoffs, err := store.ListHandoffs(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, handoff := range handoffs {
		if handoff.ID == id {
			return handoff
		}
	}
	t.Fatalf("handoff %q not found", id)
	return domain.Handoff{}
}

func TestUpdateRecordTextRejectsAmbiguousUpdate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.UpdateRecordText(ctx, ports.RecordTextUpdate{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty update = %v, want validation", err)
	}
	note := domain.Note{ProjectID: "p", ID: "n", Title: "t", Body: "b"}
	decision := domain.Decision{ProjectID: "p", ID: "d", Title: "t"}
	if err := store.UpdateRecordText(ctx, ports.RecordTextUpdate{Note: &note, Decision: &decision}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("two records = %v, want validation", err)
	}
}
