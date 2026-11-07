package fsstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListSkipsDamagedRecordsAndProblemsReportsThem(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	ctx := context.Background()
	created := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

	if _, err := store.AppendNote(ctx, domain.Note{
		ProjectID: project.ID,
		Title:     "Valid note",
		Body:      "still listed",
		CreatedAt: created,
	}); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	if _, err := store.AppendDecision(ctx, domain.Decision{
		ProjectID: project.ID,
		Title:     "Valid decision",
		Context:   "context",
		CreatedAt: created,
	}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}

	decisionsDir := filepath.Join(store.Root(), "projects", project.ID, "decisions")
	emptyPath := filepath.Join(decisionsDir, "2026-11-05-empty.md")
	brokenPath := filepath.Join(decisionsDir, "2026-11-05-broken.md")
	writeTestFile(t, emptyPath, "")
	writeTestFile(t, brokenPath, "---\nkind: decision\nid: [unterminated\n---\n\n# Broken\n")
	// A lock file next to the daily notes file is not a record.
	writeTestFile(t, filepath.Join(store.Root(), "projects", project.ID, "notes", "2026-11-05.md.lock"), "pid 1\n")

	notes, err := store.ListNotes(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListNotes with damaged siblings: %v", err)
	}
	if len(notes) != 1 || notes[0].Title != "Valid note" {
		t.Fatalf("notes = %+v, want the valid note only", notes)
	}
	decisions, err := store.ListDecisions(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListDecisions with damaged siblings: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Title != "Valid decision" {
		t.Fatalf("decisions = %+v, want the valid decision only", decisions)
	}

	problems, err := store.Problems(ctx, project.ID)
	if err != nil {
		t.Fatalf("Problems: %v", err)
	}
	if len(problems) != 2 {
		t.Fatalf("problems = %+v, want 2", problems)
	}
	gotPaths := map[string]Problem{}
	for _, problem := range problems {
		gotPaths[problem.Path] = problem
		if problem.Kind != "decision" {
			t.Errorf("problem kind = %q, want decision", problem.Kind)
		}
		if problem.Reason == "" {
			t.Errorf("problem %s has no reason", problem.Path)
		}
	}
	if _, ok := gotPaths[emptyPath]; !ok {
		t.Errorf("empty decision file not reported: %+v", problems)
	}
	if _, ok := gotPaths[brokenPath]; !ok {
		t.Errorf("broken decision file not reported: %+v", problems)
	}

	// Reading the broken file as a single record still fails.
	if _, err := readDecisionFile(brokenPath); err == nil {
		t.Fatal("readDecisionFile on malformed frontmatter returned no error")
	}
}

func TestListReportsOldEmptyFileAsOrphanedReservation(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	attemptsDir := filepath.Join(store.Root(), "projects", project.ID, "attempts")
	orphan := filepath.Join(attemptsDir, "2026-11-05-orphan.md")
	writeTestFile(t, orphan, "")
	old := time.Now().Add(-2 * orphanReservationAge)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	attempts, err := store.ListAttempts(context.Background(), project.ID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 0 {
		t.Fatalf("attempts = %+v, want none", attempts)
	}
	problems, err := store.Problems(context.Background(), project.ID)
	if err != nil {
		t.Fatalf("Problems: %v", err)
	}
	if len(problems) != 1 || problems[0].Path != orphan {
		t.Fatalf("problems = %+v, want the orphan", problems)
	}
	if !strings.Contains(problems[0].Reason, "orphaned reservation") {
		t.Fatalf("reason = %q, want orphaned reservation", problems[0].Reason)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("orphan was deleted: %v", err)
	}
}

func TestProblemsEmptyForHealthyProject(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	problems, err := store.Problems(context.Background(), project.ID)
	if err != nil {
		t.Fatalf("Problems: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("problems = %+v, want none", problems)
	}
}

func TestProblemsRejectsMissingProject(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Problems(context.Background(), "missing"); err == nil {
		t.Fatal("Problems for an unknown project returned no error")
	}
}
