package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func recordFiles(t *testing.T, dataDir, projectID, kindDir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dataDir, "projects", projectID, kindDir, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestEditRecordUpdatesTextSearchAndKeepsIdentity(t *testing.T) {
	fixture := newMemoryAPIFixture(t)
	ctx := context.Background()
	target, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "demo", Title: "Relation target", Body: "target text"})
	if err != nil {
		t.Fatal(err)
	}
	created, err := fixture.app.Note(ctx, app.NoteInput{
		ProjectID: "demo", Title: "Cache key", Body: "old body",
		Tags:       []string{"cache"},
		Confidence: 0.7,
		Relations: []domain.Relation{{
			Target: target.Note.ID, Relation: domain.RelationRelatesTo, Status: domain.RelationAccepted,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := fixture.app.GetRecord(ctx, "demo", created.Note.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeSummary := before.Summary()
	pathBefore := recordFiles(t, fixture.dataDir, "demo", "notes")
	if len(pathBefore) != 1 {
		t.Fatalf("files before = %v", pathBefore)
	}

	title, body, tags := "Cache key fixed", "zebracache new body", []string{"cache", "search"}
	edited, err := fixture.app.EditRecord(ctx, app.RecordEdit{
		ProjectID: "demo", ID: created.Note.ID,
		Title: &title, Body: &body, Tags: &tags,
	})
	if err != nil {
		t.Fatalf("EditRecord: %v", err)
	}
	if !edited.Changed || edited.DryRun || edited.Record.Note == nil {
		t.Fatalf("edit result = %+v", edited)
	}
	note := edited.Record.Note
	if note.ID != created.Note.ID || !note.CreatedAt.Equal(created.Note.CreatedAt) {
		t.Fatalf("identity changed: %+v", note)
	}
	if note.Title != title || note.Body != body || len(note.Tags) != 2 {
		t.Fatalf("text = %+v", note)
	}
	if note.Provenance != created.Note.Provenance || note.Validity.Confidence != 0.7 ||
		len(note.Relations) != 1 || note.Relations[0].Target != target.Note.ID ||
		!note.ReviewSchedule.ReviewAfter.Equal(created.Note.ReviewSchedule.ReviewAfter) {
		t.Fatalf("metadata changed: %+v", note)
	}
	if len(note.History) != 1 || note.History[0].Body != "old body" {
		t.Fatalf("history = %+v, want the old body", note.History)
	}
	if !note.UpdatedAt.After(beforeSummary.UpdatedAt) {
		t.Fatalf("updated_at did not move: %v -> %v", beforeSummary.UpdatedAt, note.UpdatedAt)
	}
	if pathAfter := recordFiles(t, fixture.dataDir, "demo", "notes"); len(pathAfter) != 1 || pathAfter[0] != pathBefore[0] {
		t.Fatalf("file path changed: %v -> %v", pathBefore, pathAfter)
	}

	report, err := fixture.app.SearchWithReport(ctx, app.SearchOptions{Text: "zebracache", ProjectID: "demo", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Hits) != 1 || report.Hits[0].ID != created.Note.ID || report.Hits[0].Title != title {
		t.Fatalf("search after edit = %+v", report.Hits)
	}
}

func TestEditRecordCASConflictAndDryRun(t *testing.T) {
	fixture := newMemoryAPIFixture(t)
	ctx := context.Background()
	created, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "demo", Title: "Old", Body: "old"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := fixture.app.GetRecord(ctx, "demo", created.Note.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale := before.Summary().UpdatedAt

	dryTitle, dryBody := "Dry", "dry body"
	dry, err := fixture.app.EditRecord(ctx, app.RecordEdit{
		ProjectID: "demo", ID: created.Note.ID, Title: &dryTitle, Body: &dryBody,
		ExpectedUpdatedAt: stale, DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !dry.DryRun || !dry.Changed || dry.Record.Note.Title != dryTitle {
		t.Fatalf("dry run result = %+v", dry)
	}
	stored, err := fixture.app.GetRecord(ctx, "demo", created.Note.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Note.Title != "Old" {
		t.Fatalf("dry run wrote: %+v", stored.Note)
	}

	title, body := "New", "new"
	if _, err := fixture.app.EditRecord(ctx, app.RecordEdit{
		ProjectID: "demo", ID: created.Note.ID, Title: &title, Body: &body,
		ExpectedUpdatedAt: stale,
	}); err != nil {
		t.Fatalf("edit with current revision: %v", err)
	}
	_, err = fixture.app.EditRecord(ctx, app.RecordEdit{
		ProjectID: "demo", ID: created.Note.ID, Title: &title,
		ExpectedUpdatedAt: stale,
	})
	var conflict *domain.RevisionConflictError
	if !errors.As(err, &conflict) || conflict.CurrentUpdatedAt.IsZero() {
		t.Fatalf("stale edit = %v, want RevisionConflictError", err)
	}
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("conflict does not unwrap to ErrConflict: %v", err)
	}
}

func TestEditRecordDuplicatePolicyAndQuarantine(t *testing.T) {
	fixture := newMemoryAPIFixture(t)
	ctx := context.Background()
	first, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "demo", Title: "Claim", Body: "same claim"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.app.Note(ctx, app.NoteInput{
		ProjectID: "demo", Title: "Second", Body: "other",
		Provenance: domain.Provenance{Origin: domain.OriginImport, Reviewed: false, Trust: domain.TrustInferred},
	})
	if err != nil {
		t.Fatal(err)
	}
	title, body := first.Note.Title, first.Note.Body
	_, err = fixture.app.EditRecord(ctx, app.RecordEdit{
		ProjectID: "demo", ID: second.Note.ID, Title: &title, Body: &body,
	})
	var duplicate *domain.DuplicateError
	if !errors.As(err, &duplicate) || duplicate.ID != first.Note.ID {
		t.Fatalf("duplicate edit = %v, want DuplicateError naming %s", err, first.Note.ID)
	}
	if !errors.Is(err, domain.ErrDuplicate) {
		t.Fatalf("duplicate does not unwrap to ErrDuplicate: %v", err)
	}

	quarantine, err := fixture.app.ListQuarantined(ctx, "demo", 10)
	if err != nil || len(quarantine) != 1 || quarantine[0].ID != second.Note.ID {
		t.Fatalf("quarantine before allowed edit = %+v %v", quarantine, err)
	}
	edited, err := fixture.app.EditRecord(ctx, app.RecordEdit{
		ProjectID: "demo", ID: second.Note.ID, Title: &title, Body: &body,
		AllowDuplicate: true,
	})
	if err != nil {
		t.Fatalf("allowed duplicate edit: %v", err)
	}
	if edited.Record.Note.Provenance.Reviewed || edited.Record.Note.Provenance.Origin != domain.OriginImport {
		t.Fatalf("edit changed provenance: %+v", edited.Record.Note.Provenance)
	}
	quarantine, err = fixture.app.ListQuarantined(ctx, "demo", 10)
	if err != nil || len(quarantine) != 1 || quarantine[0].ID != second.Note.ID {
		t.Fatalf("edit cleared quarantine: %+v %v", quarantine, err)
	}
}

func TestEditRecordEnforcesLimitsAndProtection(t *testing.T) {
	ctx := context.Background()
	t.Run("body limit", func(t *testing.T) {
		fixture := newLimitsFixture(t, config.LimitsConfig{MaxBodyBytes: 8})
		created, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "n", Body: "small"})
		if err != nil {
			t.Fatal(err)
		}
		body := "0123456789"
		_, err = fixture.app.EditRecord(ctx, app.RecordEdit{ProjectID: "p", ID: created.Note.ID, Body: &body})
		limitErr := limitError(t, err)
		if limitErr.Field != "note.body" || limitErr.Limit != 8 || limitErr.Actual != 10 {
			t.Fatalf("limit error = %+v", limitErr)
		}
		stored, err := fixture.app.GetRecord(ctx, "p", created.Note.ID)
		if err != nil || stored.Note.Body != "small" {
			t.Fatalf("refused edit wrote: %+v %v", stored.Note, err)
		}
	})
	t.Run("protected project", func(t *testing.T) {
		fixture := newLimitsFixture(t, config.LimitsConfig{ProtectedProjects: []string{"p"}})
		const noteID = "note-protected"
		if _, err := fixture.store.AppendNote(ctx, domain.Note{
			ProjectID: "p", ID: noteID, Title: "Protected", Body: "text",
			CreatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
		body := "changed"
		_, err := fixture.app.EditRecord(ctx, app.RecordEdit{ProjectID: "p", ID: noteID, Body: &body})
		protectedErr := protectedError(t, err)
		if protectedErr.ProjectID != "p" {
			t.Fatalf("protected error = %+v", protectedErr)
		}
	})
	t.Run("protected glob", func(t *testing.T) {
		fixture := newLimitsFixture(t, config.LimitsConfig{ProtectedGlobs: []string{"projects/p/notes/*.md"}})
		const noteID = "note-glob"
		if _, err := fixture.store.AppendNote(ctx, domain.Note{
			ProjectID: "p", ID: noteID, Title: "Protected", Body: "text",
			CreatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
		body := "changed"
		_, err := fixture.app.EditRecord(ctx, app.RecordEdit{ProjectID: "p", ID: noteID, Body: &body})
		protectedErr := protectedError(t, err)
		if protectedErr.Path != "projects/p/notes/note-glob.md" {
			t.Fatalf("protected error = %+v", protectedErr)
		}
	})
}

func TestEditRecordDecisionAttemptHandoffAndLifecycle(t *testing.T) {
	fixture := newMemoryAPIFixture(t)
	ctx := context.Background()

	decision, err := fixture.app.Decision(ctx, app.DecisionInput{
		ProjectID: "demo", Title: "Use SQLite", Status: domain.DecisionAccepted,
		Context: "old context", Alternatives: []string{"JSON files"}, Confidence: 0.8,
	})
	if err != nil {
		t.Fatal(err)
	}
	title, contextBody := "Use SQLite FTS5", "new context"
	editedDecision, err := fixture.app.EditRecord(ctx, app.RecordEdit{
		ProjectID: "demo", ID: decision.Decision.ID, Title: &title, Body: &contextBody,
	})
	if err != nil {
		t.Fatalf("decision edit: %v", err)
	}
	storedDecision := editedDecision.Record.Decision
	if storedDecision.Status != domain.DecisionAccepted || storedDecision.Context != contextBody ||
		len(storedDecision.Alternatives) != 1 || storedDecision.Validity.Confidence != 0.8 {
		t.Fatalf("decision after edit = %+v", storedDecision)
	}

	attempt, err := fixture.app.Attempt(ctx, app.AttemptInput{
		ProjectID: "demo", Title: "Retry", Hypothesis: "h", Result: "r", Reason: "why",
	})
	if err != nil {
		t.Fatal(err)
	}
	attemptBody := "new result"
	editedAttempt, err := fixture.app.EditRecord(ctx, app.RecordEdit{
		ProjectID: "demo", ID: attempt.Attempt.ID, Body: &attemptBody,
	})
	if err != nil {
		t.Fatalf("attempt edit: %v", err)
	}
	if editedAttempt.Record.Attempt.Result != attemptBody ||
		editedAttempt.Record.Attempt.Hypothesis != "h" || editedAttempt.Record.Attempt.Reason != "why" {
		t.Fatalf("attempt after edit = %+v", editedAttempt.Record.Attempt)
	}

	handoff, err := fixture.app.Handoff(ctx, app.HandoffInput{
		ProjectID: "demo", Summary: "old summary", NextAction: "next", GitContext: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	summary := "new summary"
	editedHandoff, err := fixture.app.EditRecord(ctx, app.RecordEdit{
		ProjectID: "demo", ID: handoff.Handoff.ID, Body: &summary,
	})
	if err != nil {
		t.Fatalf("handoff edit: %v", err)
	}
	if editedHandoff.Record.Handoff.Summary != summary || editedHandoff.Record.Handoff.NextAction != "next" {
		t.Fatalf("handoff after edit = %+v", editedHandoff.Record.Handoff)
	}
	if !editedHandoff.Record.Summary().UpdatedAt.After(handoff.Handoff.CreatedAt) {
		t.Fatalf("handoff updated_at did not move: %+v", editedHandoff.Record.Summary())
	}
	handoffTitle := "not allowed"
	_, err = fixture.app.EditRecord(ctx, app.RecordEdit{ProjectID: "demo", ID: handoff.Handoff.ID, Title: &handoffTitle})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("handoff title edit = %v, want validation", err)
	}

	note, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "demo", Title: "Trash me", Body: "before trash"})
	if err != nil {
		t.Fatal(err)
	}
	noteTitle := "Trash me edited"
	if _, err := fixture.app.EditRecord(ctx, app.RecordEdit{ProjectID: "demo", ID: note.Note.ID, Title: &noteTitle}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.TrashRecord(ctx, "demo", note.Note.ID); err != nil {
		t.Fatal(err)
	}
	restored, err := fixture.app.RestoreRecord(ctx, "demo", note.Note.ID)
	if err != nil {
		t.Fatalf("restore after edit: %v", err)
	}
	if restored.Note.Title != noteTitle || !strings.Contains(restored.Note.Body, "before trash") {
		t.Fatalf("restored record = %+v", restored.Note)
	}
}
