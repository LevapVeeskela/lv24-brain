package fsstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var reviewClock = time.Date(2027, 2, 5, 0, 0, 0, 0, time.UTC)

// writeDecisionWithReview writes one decision through the public store path.
func writeDecisionWithReview(t *testing.T, store *Store, projectID, id string, schedule domain.ReviewSchedule) string {
	t.Helper()
	decision := domain.Decision{
		ID: id, ProjectID: projectID, Title: "Keep the wire format", Status: domain.DecisionAccepted,
		Context: "the wire format is versioned", ReviewSchedule: schedule,
		CreatedAt: testClock, UpdatedAt: testClock,
	}
	if _, err := store.AppendDecision(context.Background(), decision); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	dir := filepath.Join(store.root, projectsDir, projectID, decisionsDir)
	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("decision files = %v, %v", paths, err)
	}
	return paths[0]
}

func TestAppendDecisionWritesReviewAfterFrontmatter(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	path := writeDecisionWithReview(t, store, project.ID, "decision-review-roundtrip",
		domain.ReviewSchedule{ReviewAfter: reviewClock, ReviewedAt: testClock})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "review_after:") || !strings.Contains(string(raw), "2027-02-05T00:00:00Z") {
		t.Fatalf("frontmatter has no review_after:\n%s", raw)
	}
	decision, err := readDecisionFile(path)
	if err != nil {
		t.Fatalf("readDecisionFile: %v", err)
	}
	if !decision.ReviewAfter.Equal(reviewClock) || !decision.ReviewedAt.Equal(testClock) {
		t.Fatalf("review schedule = %+v", decision.ReviewSchedule)
	}
}

func TestSetReviewAfterMovesAndClears(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	path := writeDecisionWithReview(t, store, project.ID, "decision-review-move", domain.ReviewSchedule{})

	changed, err := store.SetReviewAfter(context.Background(), project.ID, ports.RecordDecision, "decision-review-move", reviewClock)
	if err != nil || !changed {
		t.Fatalf("SetReviewAfter = %v, %v, want a change", changed, err)
	}
	decision, err := readDecisionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.ReviewAfter.Equal(reviewClock) || decision.ReviewedAt.IsZero() {
		t.Fatalf("after move: %+v", decision.ReviewSchedule)
	}

	// A repeat of the same instant is an idempotent no-op.
	changed, err = store.SetReviewAfter(context.Background(), project.ID, ports.RecordDecision, "decision-review-move", reviewClock)
	if err != nil || changed {
		t.Fatalf("repeat SetReviewAfter = %v, %v, want no change", changed, err)
	}

	changed, err = store.SetReviewAfter(context.Background(), project.ID, ports.RecordDecision, "decision-review-move", time.Time{})
	if err != nil || !changed {
		t.Fatalf("clear SetReviewAfter = %v, %v, want a change", changed, err)
	}
	decision, err = readDecisionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.ReviewAfter.IsZero() {
		t.Fatalf("review_after = %s after clear", decision.ReviewAfter)
	}
}

func TestSetReviewAfterRewritesOnlyTheNoteMarker(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	note := domain.Note{
		ID: "note-review-marker", ProjectID: project.ID, Title: "Marker note", Body: "body stays byte for byte",
		CreatedAt: testClock, UpdatedAt: testClock,
	}
	if _, err := store.AppendNote(context.Background(), note); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	changed, err := store.SetReviewAfter(context.Background(), project.ID, ports.RecordNote, note.ID, reviewClock)
	if err != nil || !changed {
		t.Fatalf("SetReviewAfter(note) = %v, %v", changed, err)
	}
	notes, err := store.ListNotes(context.Background(), project.ID)
	if err != nil || len(notes) != 1 {
		t.Fatalf("ListNotes = %+v, %v", notes, err)
	}
	if !notes[0].ReviewAfter.Equal(reviewClock) {
		t.Fatalf("note review_after = %s", notes[0].ReviewAfter)
	}
	if notes[0].Body != note.Body {
		t.Fatalf("note body changed: %q", notes[0].Body)
	}
}

func TestSetReviewAfterUnknownRecordIsNotFound(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	_, err := store.SetReviewAfter(context.Background(), project.ID, ports.RecordAttempt, "attempt-missing", reviewClock)
	if err == nil || !strings.Contains(err.Error(), domain.ErrNotFound.Error()) {
		t.Fatalf("SetReviewAfter(missing) error = %v, want not found", err)
	}
}
