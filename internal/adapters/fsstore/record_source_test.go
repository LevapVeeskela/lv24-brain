package fsstore

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func testRecordSource(projectID string) *domain.SourceRef {
	return &domain.SourceRef{
		ProjectID:    projectID,
		ConnectionID: "gh-acme",
		ItemKey:      "gh-acme/" + strings.Repeat("a", 40),
		ExternalID:   "acme/widgets#issue-7",
		URL:          "https://github.test/acme/widgets/issues/7",
		ObservedAt:   testClock.Add(-2 * time.Hour),
		ProposalID:   domain.ProposalID(projectID, "gh-acme", "acme/widgets#issue-7"),
		Confidence:   0.82,
	}
}

func TestDecisionSourceRoundTripAndUpdatePreservesIt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	source := testRecordSource(project.ID)
	decision := domain.Decision{
		ProjectID: project.ID,
		Title:     "Promoted retry budget",
		Status:    domain.DecisionAccepted,
		Context:   "The issue was closed with a decision.",
		Source:    source,
		CreatedAt: testClock.Add(-time.Hour),
	}
	if _, err := store.AppendDecision(ctx, decision); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	files := globFiles(t, filepath.Join(store.Root(), "projects", project.ID, "decisions", "*.md"))
	if len(files) != 1 {
		t.Fatalf("decision files = %v", files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read decision: %v", err)
	}
	for _, want := range []string{"source:", "proposal_id:", "connection_id:", "observed_at:"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("decision frontmatter lacks %q:\n%s", want, raw)
		}
	}
	got, err := readDecisionFile(files[0])
	if err != nil {
		t.Fatalf("readDecisionFile: %v", err)
	}
	if !reflect.DeepEqual(got.Source, source) {
		t.Fatalf("source = %+v, want %+v", got.Source, source)
	}

	// UpdateDecision rewrites the file. Provenance must survive even when the
	// caller passes a decision without a source (an older writer).
	update := got
	update.Source = nil
	update.Context = "Updated context."
	if err := store.UpdateDecision(ctx, update); err != nil {
		t.Fatalf("UpdateDecision: %v", err)
	}
	updated, err := readDecisionFile(files[0])
	if err != nil {
		t.Fatalf("read updated decision: %v", err)
	}
	if !reflect.DeepEqual(updated.Source, source) {
		t.Fatalf("UpdateDecision lost the source: %+v", updated.Source)
	}
	if updated.Context != "Updated context." {
		t.Fatalf("context = %q", updated.Context)
	}
}

func TestAttemptSourceRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	source := testRecordSource(project.ID)
	attempt := domain.Attempt{
		ProjectID: project.ID,
		Title:     "Try a bounded retry queue",
		Result:    "Worked.",
		Source:    source,
	}
	if _, err := store.AppendAttempt(ctx, attempt); err != nil {
		t.Fatalf("AppendAttempt: %v", err)
	}
	attempts, err := store.ListAttempts(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %+v", attempts)
	}
	if !reflect.DeepEqual(attempts[0].Source, source) {
		t.Fatalf("source = %+v, want %+v", attempts[0].Source, source)
	}
}

func TestNoteSourceRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	source := testRecordSource(project.ID)
	note := domain.Note{
		ProjectID: project.ID,
		Title:     "Promoted note",
		Body:      "Body of the promoted note.",
		Source:    source,
		CreatedAt: testClock.Add(-time.Hour),
	}
	if _, err := store.AppendNote(ctx, note); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	notes, err := store.ListNotes(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListNotes: %v", err)
	}
	if len(notes) != 1 || !reflect.DeepEqual(notes[0].Source, source) {
		t.Fatalf("notes = %+v", notes)
	}
	files := globFiles(t, filepath.Join(store.Root(), "projects", project.ID, "notes", "*.md"))
	if len(files) != 1 {
		t.Fatalf("note files = %v", files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read notes: %v", err)
	}
	if !strings.Contains(string(raw), `"source"`) || !strings.Contains(string(raw), `"proposalId"`) {
		t.Fatalf("note marker lacks source:\n%s", raw)
	}
}

func TestRecordsWithoutSourceStayNil(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	if _, err := store.AppendDecision(ctx, domain.Decision{ProjectID: project.ID, Title: "Legacy decision"}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	if _, err := store.AppendAttempt(ctx, domain.Attempt{ProjectID: project.ID, Title: "Legacy attempt"}); err != nil {
		t.Fatalf("AppendAttempt: %v", err)
	}
	if _, err := store.AppendNote(ctx, domain.Note{ProjectID: project.ID, Title: "Legacy note", Body: "Body."}); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	decisions, err := store.ListDecisions(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	attempts, err := store.ListAttempts(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	notes, err := store.ListNotes(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListNotes: %v", err)
	}
	if decisions[0].Source != nil || attempts[0].Source != nil || notes[0].Source != nil {
		t.Fatalf("legacy records gained a source: %+v %+v %+v", decisions[0].Source, attempts[0].Source, notes[0].Source)
	}
	for _, pattern := range []string{
		filepath.Join(store.Root(), "projects", project.ID, "decisions", "*.md"),
		filepath.Join(store.Root(), "projects", project.ID, "attempts", "*.md"),
	} {
		files := globFiles(t, pattern)
		raw, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatalf("read %s: %v", files[0], err)
		}
		if strings.Contains(string(raw), "source:") {
			t.Fatalf("legacy record encoded an empty source:\n%s", raw)
		}
	}
}
