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

func TestAppendNoteQuarantinesAnUnparseableDailyFile(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	created := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	if _, err := store.AppendNote(ctx, domain.Note{ProjectID: project.ID, Title: "Kept", Body: "x", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(store.root, "projects", project.ID, notesDir)
	daily := filepath.Join(dir, "2026-11-05.md")
	raw, err := os.ReadFile(daily)
	if err != nil {
		t.Fatal(err)
	}
	// A hand edit broke the YAML frontmatter: the whole file no longer parses.
	damaged := strings.Replace(string(raw), "kind: note", "kind: [note", 1)
	if damaged == string(raw) {
		t.Fatalf("fixture did not change the frontmatter:\n%s", raw)
	}
	if err := os.WriteFile(daily, []byte(damaged), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := store.AppendNote(ctx, domain.Note{ProjectID: project.ID, Title: "Today", Body: "y", CreatedAt: created.Add(time.Hour)}); err != nil {
		t.Fatalf("AppendNote after damage: %v", err)
	}
	notes, err := store.ListNotes(ctx, project.ID)
	if err != nil || len(notes) != 1 || notes[0].Title != "Today" {
		t.Fatalf("notes = %+v, %v", notes, err)
	}
	aside, _ := filepath.Glob(filepath.Join(dir, "2026-11-05"+quarantineInfix+"*"))
	if len(aside) != 1 {
		t.Fatalf("quarantined files = %v", aside)
	}
	if kept, _ := os.ReadFile(aside[0]); string(kept) != damaged {
		t.Fatal("quarantined file content changed")
	}
	problems, err := store.Problems(ctx, project.ID)
	if err != nil || len(problems) != 1 || problems[0].Path != aside[0] || !strings.HasPrefix(problems[0].Reason, "quarantined") {
		t.Fatalf("problems = %+v, %v", problems, err)
	}
}
