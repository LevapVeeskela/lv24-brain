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
	"github.com/levapveeskela/lv24-brain/internal/markdown"
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

func writeDecisionWithSource(t *testing.T, store *Store, projectID string, source *sourceRefFrontmatter) {
	t.Helper()
	frontmatter := decisionFrontmatter{
		Frontmatter: markdown.Frontmatter{
			ProjectID:     projectID,
			Kind:          "decision",
			UpdatedAt:     testClock,
			SchemaVersion: SchemaVersion,
		},
		ID:        "decision-source-test",
		Title:     "Source test",
		Status:    domain.DecisionAccepted,
		Source:    source,
		CreatedAt: testClock,
	}
	raw, err := markdown.Render(frontmatter, "# Source test\n\nStatus: accepted\n\n## Context\n\ntext\n")
	if err != nil {
		t.Fatalf("render decision: %v", err)
	}
	dir := filepath.Join(store.Root(), "projects", projectID, "decisions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-11-01-source-test.md"), raw, 0o644); err != nil {
		t.Fatalf("write decision: %v", err)
	}
}

// TestInvalidRecordSourceIsSkipped rejects provenance that is malformed,
// points outside http(s) or names another project. A record with such a source
// must not be listed as curated; Problems reports the file.
func TestInvalidRecordSourceIsSkipped(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)

	otherProjectSource := &sourceRefFrontmatter{
		ProjectID:    "other",
		ConnectionID: "gh-acme",
		ExternalID:   "acme/widgets#issue-7",
	}
	writeDecisionWithSource(t, store, project.ID, otherProjectSource)

	if decisions, err := store.ListDecisions(ctx, project.ID); err != nil || len(decisions) != 0 {
		t.Fatalf("ListDecisions = %+v, %v; want empty", decisions, err)
	}
	problems, err := store.Problems(ctx, project.ID)
	if err != nil {
		t.Fatalf("Problems: %v", err)
	}
	if len(problems) != 1 || problems[0].Kind != "decision" || !strings.Contains(problems[0].Reason, "source.projectId") {
		t.Fatalf("problems = %+v, want one decision source problem", problems)
	}

	// A decision whose source URL is javascript: must not be curated either.
	writeDecisionWithSource(t, store, project.ID, &sourceRefFrontmatter{
		ConnectionID: "gh-acme",
		URL:          "javascript:alert(1)",
	})
	if decisions, err := store.ListDecisions(ctx, project.ID); err != nil || len(decisions) != 0 {
		t.Fatalf("ListDecisions with javascript source = %+v, %v; want empty", decisions, err)
	}

	// The same rule applies to attempts.
	attemptFrontmatter := attemptFrontmatter{
		Frontmatter: markdown.Frontmatter{
			ProjectID:     project.ID,
			Kind:          "attempt",
			UpdatedAt:     testClock,
			SchemaVersion: SchemaVersion,
		},
		ID:        "attempt-source-test",
		Title:     "Source test",
		Source:    &sourceRefFrontmatter{ConnectionID: "gh-acme", URL: "javascript:alert(1)"},
		CreatedAt: testClock,
	}
	attemptRaw, err := markdown.Render(attemptFrontmatter, "# Source test\n\n## Hypothesis\n\nh\n")
	if err != nil {
		t.Fatalf("render attempt: %v", err)
	}
	attemptsDir := filepath.Join(store.Root(), "projects", project.ID, "attempts")
	if err := os.MkdirAll(attemptsDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(attemptsDir, "2026-11-01-source-test.md"), attemptRaw, 0o644); err != nil {
		t.Fatalf("write attempt: %v", err)
	}
	if attempts, err := store.ListAttempts(ctx, project.ID); err != nil || len(attempts) != 0 {
		t.Fatalf("ListAttempts = %+v, %v; want empty", attempts, err)
	}

	// An empty source object is not provenance: the note file is reported, not
	// returned as a valid note.
	noteFrontmatter := noteFileFrontmatter{
		Frontmatter: markdown.Frontmatter{
			ProjectID:     project.ID,
			Kind:          "note",
			UpdatedAt:     testClock,
			SchemaVersion: SchemaVersion,
		},
		Date: "2026-11-02",
	}
	emptySource := &domain.SourceRef{}
	body := renderNoteEntry(noteMetadata{
		ID:        "note-empty-source",
		Title:     "Empty source",
		Source:    emptySource,
		CreatedAt: testClock,
	}, "Body of the note.")
	raw, err := markdown.Render(noteFrontmatter, body)
	if err != nil {
		t.Fatalf("render notes: %v", err)
	}
	notesDir := filepath.Join(store.Root(), "projects", project.ID, "notes")
	if err := os.MkdirAll(notesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(notesDir, "2026-11-02.md"), raw, 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	if notes, err := store.ListNotes(ctx, project.ID); err != nil || len(notes) != 0 {
		t.Fatalf("ListNotes = %+v, %v; want empty", notes, err)
	}

	problems, err = store.Problems(ctx, project.ID)
	if err != nil {
		t.Fatalf("Problems: %v", err)
	}
	counts := make(map[string]int, len(problems))
	for _, problem := range problems {
		counts[problem.Kind]++
	}
	if counts["decision"] != 1 || counts["attempt"] != 1 || counts["note"] != 1 {
		t.Fatalf("problem kinds = %v, want one decision, attempt and note", counts)
	}
}

// TestOptionalObservedAtIsTolerated: a space-separated observation time is
// informational and must not hide the decision that carries it.
func TestOptionalObservedAtIsTolerated(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	writeDecisionWithSource(t, store, project.ID, &sourceRefFrontmatter{
		ProjectID:    project.ID,
		ConnectionID: "gh-acme",
		ExternalID:   "acme/widgets#issue-7",
		ObservedAt:   "2026-11-05 09:30:00",
	})
	decisions, err := store.ListDecisions(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Source == nil {
		t.Fatalf("decisions = %+v, want one with a source", decisions)
	}
	want := time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC)
	if !decisions[0].Source.ObservedAt.Equal(want) {
		t.Fatalf("observed_at = %v, want %v", decisions[0].Source.ObservedAt, want)
	}

	writeDecisionWithSource(t, store, project.ID, &sourceRefFrontmatter{
		ProjectID:    project.ID,
		ConnectionID: "gh-acme",
		ExternalID:   "acme/widgets#issue-8",
		ObservedAt:   "not a timestamp",
	})
	// The helper overwrites the same file with an unparseable observation
	// time; the decision still reads and the optional time becomes zero.
	decisions, err = store.ListDecisions(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListDecisions with bad timestamp: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Source == nil || !decisions[0].Source.ObservedAt.IsZero() {
		t.Fatalf("decisions = %+v, want one with a zero observed_at", decisions)
	}
}
