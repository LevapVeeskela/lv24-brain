package fsstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func editTestDecision(projectID, id, title string) domain.Decision {
	return domain.Decision{
		ID:           id,
		ProjectID:    projectID,
		Title:        title,
		Status:       domain.DecisionProposed,
		Context:      "Context for " + title,
		Alternatives: []string{"Option A"},
		Consequences: []string{"Consequence"},
		Evidence:     []string{"first evidence"},
		CreatedAt:    time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC),
	}
}

func liveRecordIDs(t *testing.T, store *Store, projectID, kind string) []string {
	t.Helper()
	ctx := context.Background()
	ids := []string{}
	switch kind {
	case ports.RecordNote:
		notes, err := store.ListNotes(ctx, projectID)
		if err != nil {
			t.Fatalf("ListNotes: %v", err)
		}
		for _, note := range notes {
			ids = append(ids, note.ID)
		}
	case ports.RecordDecision:
		decisions, err := store.ListDecisions(ctx, projectID)
		if err != nil {
			t.Fatalf("ListDecisions: %v", err)
		}
		for _, decision := range decisions {
			ids = append(ids, decision.ID)
		}
	case ports.RecordAttempt:
		attempts, err := store.ListAttempts(ctx, projectID)
		if err != nil {
			t.Fatalf("ListAttempts: %v", err)
		}
		for _, attempt := range attempts {
			ids = append(ids, attempt.ID)
		}
	case ports.RecordHandoff:
		handoffs, err := store.ListHandoffs(ctx, projectID)
		if err != nil {
			t.Fatalf("ListHandoffs: %v", err)
		}
		for _, handoff := range handoffs {
			ids = append(ids, handoff.ID)
		}
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	return ids
}

func onlyFile(t *testing.T, dir string) string {
	t.Helper()
	files := globFiles(t, filepath.Join(dir, "*.md"))
	if len(files) != 1 {
		t.Fatalf("files in %s = %v, want exactly one", dir, files)
	}
	return files[0]
}

func TestUpdateDecisionRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	original := editTestDecision(project.ID, "dec-update", "Pick storage")
	if _, err := store.AppendDecision(ctx, original); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	dir := filepath.Join(store.Root(), "projects", project.ID, "decisions")
	pathBefore := onlyFile(t, dir)
	before, err := readDecisionFile(pathBefore)
	if err != nil {
		t.Fatalf("readDecisionFile: %v", err)
	}

	updated := before
	updated.Status = domain.DecisionAccepted
	updated.Evidence = append(append([]string{}, before.Evidence...), "second evidence")
	if err := store.UpdateDecision(ctx, updated); err != nil {
		t.Fatalf("UpdateDecision: %v", err)
	}

	if pathAfter := onlyFile(t, dir); pathAfter != pathBefore {
		t.Fatalf("file path changed: %q -> %q", pathBefore, pathAfter)
	}
	decisions, err := store.ListDecisions(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("decisions = %d, want 1", len(decisions))
	}
	got := decisions[0]
	if got.ID != before.ID || !got.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("ID or created_at changed: %+v", got)
	}
	if got.Status != domain.DecisionAccepted {
		t.Fatalf("status = %q, want accepted", got.Status)
	}
	if !reflect.DeepEqual(got.Evidence, updated.Evidence) {
		t.Fatalf("evidence = %#v, want %#v", got.Evidence, updated.Evidence)
	}
	if got.Context != before.Context || got.Title != before.Title {
		t.Fatalf("unchanged fields differ: %+v", got)
	}
	raw, err := os.ReadFile(onlyFile(t, dir))
	if err != nil {
		t.Fatalf("read decision file: %v", err)
	}
	if !strings.Contains(string(raw), "Status: accepted") {
		t.Fatalf("body status line not updated:\n%s", raw)
	}
}

func TestUpdateDecisionNotFound(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	err := store.UpdateDecision(context.Background(), editTestDecision(project.ID, "missing", "Nope"))
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("UpdateDecision error = %v, want ErrNotFound", err)
	}
}

func TestTrashAndRestoreFileRecords(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		kind   string
		create func(t *testing.T, store *Store, projectID string)
		dir    string
	}{
		{
			name: "decision",
			kind: ports.RecordDecision,
			create: func(t *testing.T, store *Store, projectID string) {
				if _, err := store.AppendDecision(ctx, editTestDecision(projectID, "dec-trash", "Trash decision")); err != nil {
					t.Fatalf("AppendDecision: %v", err)
				}
			},
			dir: decisionsDir,
		},
		{
			name: "attempt",
			kind: ports.RecordAttempt,
			create: func(t *testing.T, store *Store, projectID string) {
				attempt := domain.Attempt{
					ID:         "att-trash",
					ProjectID:  projectID,
					Title:      "Trash attempt",
					Hypothesis: "It might work",
					Result:     "It did not",
					Reason:     "Wrong layer",
					CreatedAt:  time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC),
				}
				if _, err := store.AppendAttempt(ctx, attempt); err != nil {
					t.Fatalf("AppendAttempt: %v", err)
				}
			},
			dir: attemptsDir,
		},
		{
			name: "handoff",
			kind: ports.RecordHandoff,
			create: func(t *testing.T, store *Store, projectID string) {
				handoff := domain.Handoff{
					ID:         "handoff-trash",
					ProjectID:  projectID,
					Summary:    "Session summary",
					GitContext: "main clean",
					NextAction: "Continue",
					CreatedAt:  time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC),
				}
				if _, err := store.AppendHandoff(ctx, handoff); err != nil {
					t.Fatalf("AppendHandoff: %v", err)
				}
			},
			dir: handoffsDir,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			project := seedProject(t, store)
			tc.create(t, store, project.ID)
			liveDir := filepath.Join(store.Root(), "projects", project.ID, tc.dir)
			trashKindDir := filepath.Join(store.Root(), "projects", project.ID, trashDir, tc.dir)
			ids := liveRecordIDs(t, store, project.ID, tc.kind)
			if len(ids) != 1 {
				t.Fatalf("live IDs = %v, want one", ids)
			}
			id := ids[0]
			livePath := onlyFile(t, liveDir)
			contentBefore, err := os.ReadFile(livePath)
			if err != nil {
				t.Fatalf("read live file: %v", err)
			}

			if err := store.TrashRecord(ctx, project.ID, tc.kind, id); err != nil {
				t.Fatalf("TrashRecord: %v", err)
			}
			if got := liveRecordIDs(t, store, project.ID, tc.kind); len(got) != 0 {
				t.Fatalf("live IDs after trash = %v, want none", got)
			}
			trashed, err := store.ListTrash(ctx, project.ID)
			if err != nil {
				t.Fatalf("ListTrash: %v", err)
			}
			if len(trashed) != 1 || trashed[0].Kind != tc.kind || trashed[0].ID != id || trashed[0].Title == "" {
				t.Fatalf("ListTrash = %+v", trashed)
			}
			if got := onlyFile(t, trashKindDir); filepath.Base(got) != filepath.Base(livePath) {
				t.Fatalf("trash file name = %q, want %q", filepath.Base(got), filepath.Base(livePath))
			}
			problems, err := store.Problems(ctx, project.ID)
			if err != nil {
				t.Fatalf("Problems: %v", err)
			}
			if len(problems) != 0 {
				t.Fatalf("Problems after trash = %+v, want none", problems)
			}

			if err := store.RestoreRecord(ctx, project.ID, tc.kind, id); err != nil {
				t.Fatalf("RestoreRecord: %v", err)
			}
			if got := liveRecordIDs(t, store, project.ID, tc.kind); !reflect.DeepEqual(got, []string{id}) {
				t.Fatalf("live IDs after restore = %v, want [%s]", got, id)
			}
			restoredPath := onlyFile(t, liveDir)
			contentAfter, err := os.ReadFile(restoredPath)
			if err != nil {
				t.Fatalf("read restored file: %v", err)
			}
			if string(contentAfter) != string(contentBefore) {
				t.Fatalf("restored content differs:\n%s\n---\n%s", contentBefore, contentAfter)
			}
			if got, err := store.ListTrash(ctx, project.ID); err != nil || len(got) != 0 {
				t.Fatalf("ListTrash after restore = %+v, %v", got, err)
			}
		})
	}
}

func TestTrashedNameCollisionGetsSuffix(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	if _, err := store.AppendDecision(ctx, editTestDecision(project.ID, "dec-a", "Same title")); err != nil {
		t.Fatalf("AppendDecision a: %v", err)
	}
	if err := store.TrashRecord(ctx, project.ID, ports.RecordDecision, "dec-a"); err != nil {
		t.Fatalf("TrashRecord a: %v", err)
	}
	if _, err := store.AppendDecision(ctx, editTestDecision(project.ID, "dec-b", "Same title")); err != nil {
		t.Fatalf("AppendDecision b: %v", err)
	}
	if err := store.TrashRecord(ctx, project.ID, ports.RecordDecision, "dec-b"); err != nil {
		t.Fatalf("TrashRecord b: %v", err)
	}
	trashDecisions := filepath.Join(store.Root(), "projects", project.ID, trashDir, decisionsDir)
	names := []string{}
	for _, path := range globFiles(t, filepath.Join(trashDecisions, "*.md")) {
		names = append(names, filepath.Base(path))
	}
	want := []string{"2026-11-05-same-title-2.md", "2026-11-05-same-title.md"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("trash names = %v, want %v", names, want)
	}
}

func TestTrashNoteKeepsOtherNoteInSameFile(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	first := domain.Note{
		ID: "note-first", ProjectID: project.ID, Title: "First", Body: "First body.\n\nSecond paragraph.",
		Tags: []string{"keep"}, CreatedAt: time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC),
	}
	second := domain.Note{
		ID: "note-second", ProjectID: project.ID, Title: "Second", Body: "Second body.",
		CreatedAt: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC),
	}
	for _, note := range []domain.Note{first, second} {
		if _, err := store.AppendNote(ctx, note); err != nil {
			t.Fatalf("AppendNote %s: %v", note.ID, err)
		}
	}

	if err := store.TrashRecord(ctx, project.ID, ports.RecordNote, "note-first"); err != nil {
		t.Fatalf("TrashRecord: %v", err)
	}
	notes, err := store.ListNotes(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListNotes: %v", err)
	}
	if len(notes) != 1 || notes[0].ID != "note-second" || notes[0].Body != second.Body || notes[0].Title != second.Title {
		t.Fatalf("notes after trash = %+v, want only intact note-second", notes)
	}
	trashed, err := store.ListTrash(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListTrash: %v", err)
	}
	if len(trashed) != 1 || trashed[0].Kind != ports.RecordNote || trashed[0].ID != "note-first" {
		t.Fatalf("ListTrash = %+v", trashed)
	}

	if err := store.RestoreRecord(ctx, project.ID, ports.RecordNote, "note-first"); err != nil {
		t.Fatalf("RestoreRecord: %v", err)
	}
	notes, err = store.ListNotes(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListNotes after restore: %v", err)
	}
	byID := map[string]domain.Note{}
	for _, note := range notes {
		byID[note.ID] = note
	}
	if len(byID) != 2 {
		t.Fatalf("notes after restore = %+v, want 2", notes)
	}
	restored := byID["note-first"]
	if restored.Body != first.Body || restored.Title != first.Title || !restored.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("restored note = %+v, want %+v", restored, first)
	}
	if byID["note-second"].Body != second.Body {
		t.Fatalf("second note changed: %+v", byID["note-second"])
	}
}

func TestRestoreConflicts(t *testing.T) {
	ctx := context.Background()

	t.Run("live record with same ID", func(t *testing.T) {
		store := newTestStore(t)
		project := seedProject(t, store)
		if _, err := store.AppendDecision(ctx, editTestDecision(project.ID, "dec-same", "Old title")); err != nil {
			t.Fatalf("AppendDecision: %v", err)
		}
		if err := store.TrashRecord(ctx, project.ID, ports.RecordDecision, "dec-same"); err != nil {
			t.Fatalf("TrashRecord: %v", err)
		}
		if _, err := store.AppendDecision(ctx, editTestDecision(project.ID, "dec-same", "New title")); err != nil {
			t.Fatalf("AppendDecision new: %v", err)
		}
		err := store.RestoreRecord(ctx, project.ID, ports.RecordDecision, "dec-same")
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("RestoreRecord error = %v, want ErrConflict", err)
		}
	})

	t.Run("live file name taken", func(t *testing.T) {
		store := newTestStore(t)
		project := seedProject(t, store)
		if _, err := store.AppendDecision(ctx, editTestDecision(project.ID, "dec-old", "Shared name")); err != nil {
			t.Fatalf("AppendDecision old: %v", err)
		}
		if err := store.TrashRecord(ctx, project.ID, ports.RecordDecision, "dec-old"); err != nil {
			t.Fatalf("TrashRecord: %v", err)
		}
		if _, err := store.AppendDecision(ctx, editTestDecision(project.ID, "dec-other", "Shared name")); err != nil {
			t.Fatalf("AppendDecision other: %v", err)
		}
		err := store.RestoreRecord(ctx, project.ID, ports.RecordDecision, "dec-old")
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("RestoreRecord error = %v, want ErrConflict", err)
		}
		if got := liveRecordIDs(t, store, project.ID, ports.RecordDecision); !reflect.DeepEqual(got, []string{"dec-other"}) {
			t.Fatalf("live IDs = %v, want only dec-other", got)
		}
	})

	t.Run("note ID already live", func(t *testing.T) {
		store := newTestStore(t)
		project := seedProject(t, store)
		note := domain.Note{ID: "note-dup", ProjectID: project.ID, Title: "Dup", Body: "body", CreatedAt: testClock}
		if _, err := store.AppendNote(ctx, note); err != nil {
			t.Fatalf("AppendNote: %v", err)
		}
		if err := store.TrashRecord(ctx, project.ID, ports.RecordNote, "note-dup"); err != nil {
			t.Fatalf("TrashRecord: %v", err)
		}
		if _, err := store.AppendNote(ctx, note); err != nil {
			t.Fatalf("AppendNote again: %v", err)
		}
		err := store.RestoreRecord(ctx, project.ID, ports.RecordNote, "note-dup")
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("RestoreRecord error = %v, want ErrConflict", err)
		}
	})
}

func TestTrashAndRestoreNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)

	for _, kind := range []string{ports.RecordNote, ports.RecordDecision, ports.RecordAttempt, ports.RecordHandoff} {
		if err := store.TrashRecord(ctx, project.ID, kind, "missing"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("TrashRecord %s error = %v, want ErrNotFound", kind, err)
		}
		if err := store.RestoreRecord(ctx, project.ID, kind, "missing"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("RestoreRecord %s error = %v, want ErrNotFound", kind, err)
		}
	}
}

func TestTrashUnknownKindIsValidationError(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)

	if err := store.TrashRecord(ctx, project.ID, "state", "x"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("TrashRecord error = %v, want ErrValidation", err)
	}
	if err := store.RestoreRecord(ctx, project.ID, "state", "x"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("RestoreRecord error = %v, want ErrValidation", err)
	}
}

func TestListTrashEmptyProject(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	trashed, err := store.ListTrash(context.Background(), project.ID)
	if err != nil {
		t.Fatalf("ListTrash: %v", err)
	}
	if trashed == nil || len(trashed) != 0 {
		t.Fatalf("ListTrash = %#v, want empty non-nil slice", trashed)
	}
}
