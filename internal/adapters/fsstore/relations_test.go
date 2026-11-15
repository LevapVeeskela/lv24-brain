package fsstore

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// relationFixture is a decision with one accepted and one pending relation.
func relationFixture() domain.RelationList {
	return domain.RelationList{
		{Target: "decision-b", Relation: domain.RelationRelatesTo, Reason: "same storage", Confidence: 0.9, Status: domain.RelationAccepted, JudgedAt: testClock, VerdictBy: domain.VerdictByHuman},
		{Target: "decision-c", Relation: domain.RelationRelatesTo, Reason: "similar text", Confidence: 0.72, Status: domain.RelationPending},
	}
}

// recordFilePath finds the file of a live record by its frontmatter id.
func recordFilePath(t *testing.T, store *Store, projectID, kind, id string) string {
	t.Helper()
	dir := filepath.Join(store.Root(), "projects", projectID, recordKindDirs[kind])
	path, err := findRecordPath(dir, kind, id)
	if err != nil {
		t.Fatalf("find %s %s: %v", kind, id, err)
	}
	return path
}

func TestRelationsRoundTripAcrossKinds(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)

	if _, err := store.AppendDecision(ctx, domain.Decision{
		ID: "decision-a", ProjectID: project.ID, Title: "Cache", Status: domain.DecisionAccepted,
		Relations: relationFixture(), CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	if _, err := store.AppendAttempt(ctx, domain.Attempt{
		ID: "attempt-a", ProjectID: project.ID, Title: "Try cache", Relations: relationFixture().Accepted(), CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendAttempt: %v", err)
	}
	if _, err := store.AppendHandoff(ctx, domain.Handoff{
		ID: "handoff-a", ProjectID: project.ID, Summary: "handoff", Relations: relationFixture().Accepted(), CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendHandoff: %v", err)
	}
	if _, err := store.AppendNote(ctx, domain.Note{
		ID: "note-a", ProjectID: project.ID, Title: "Note", Body: "body", Relations: relationFixture(), CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}

	decisions, err := store.ListDecisions(ctx, project.ID)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("ListDecisions = %+v, %v", decisions, err)
	}
	if !reflect.DeepEqual(decisions[0].Relations.Normalize(), relationFixture().Normalize()) {
		t.Fatalf("decision relations = %+v", decisions[0].Relations)
	}
	attempts, _ := store.ListAttempts(ctx, project.ID)
	if len(attempts) != 1 || len(attempts[0].Relations) != 1 {
		t.Fatalf("attempt relations = %+v", attempts)
	}
	handoffs, _ := store.ListHandoffs(ctx, project.ID)
	if len(handoffs) != 1 || len(handoffs[0].Relations) != 1 {
		t.Fatalf("handoff relations = %+v", handoffs)
	}
	notes, _ := store.ListNotes(ctx, project.ID)
	if len(notes) != 1 || !reflect.DeepEqual(notes[0].Relations.Normalize(), relationFixture().Normalize()) {
		t.Fatalf("note relations = %+v", notes)
	}
}

func TestRelationsAreOptionalInFrontmatter(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	if _, err := store.AppendDecision(ctx, domain.Decision{ID: "decision-a", ProjectID: project.ID, Title: "Plain", CreatedAt: testClock}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	if _, err := store.AppendNote(ctx, domain.Note{ID: "note-a", ProjectID: project.ID, Title: "Plain", Body: "text", CreatedAt: testClock}); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	decisionPath := recordFilePath(t, store, project.ID, "decision", "decision-a")
	raw, err := os.ReadFile(decisionPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "relations:") || strings.Contains(string(raw), "merged_from") {
		t.Fatalf("plain decision gained relation keys:\n%s", raw)
	}
	noteFiles := globFiles(t, filepath.Join(store.Root(), "projects", project.ID, "notes", "*.md"))
	noteRaw, err := os.ReadFile(noteFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(noteRaw), `"relations"`) || strings.Contains(string(noteRaw), `"mergedFrom"`) {
		t.Fatalf("plain note gained relation keys:\n%s", noteRaw)
	}
}

func TestSetRecordRelationsIsMinimalAndIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	if _, err := store.AppendDecision(ctx, domain.Decision{
		ID: "decision-a", ProjectID: project.ID, Title: "Cache", Context: "keep this text",
		Status: domain.DecisionAccepted, CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	path := recordFilePath(t, store, project.ID, "decision", "decision-a")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), "relations:") {
		t.Fatalf("new decision already has relations:\n%s", before)
	}
	changed, err := store.SetRecordRelations(ctx, project.ID, "decision", "decision-a", relationFixture())
	if err != nil || !changed {
		t.Fatalf("SetRecordRelations = %v, %v", changed, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "keep this text") {
		t.Fatalf("body text lost:\n%s", after)
	}
	if strings.Contains(string(after), "updated_at: 2026-11-05T11:30:00Z") {
		t.Fatalf("relation edit bumped updated_at:\n%s", after)
	}
	if !strings.Contains(string(after), "relation: relates_to") {
		t.Fatalf("relation not written:\n%s", after)
	}
	if !strings.Contains(string(after), "judged_at:") {
		t.Fatalf("judged_at not written:\n%s", after)
	}
	if !strings.Contains(string(after), "status: pending") {
		t.Fatalf("pending status not written:\n%s", after)
	}
	changed, err = store.SetRecordRelations(ctx, project.ID, "decision", "decision-a", relationFixture())
	if err != nil || changed {
		t.Fatalf("repeat = %v, %v; want no change", changed, err)
	}
	if again, _ := os.ReadFile(path); string(again) != string(after) {
		t.Fatal("repeat rewrote the file")
	}
	// Clearing the list removes the key.
	if changed, err := store.SetRecordRelations(ctx, project.ID, "decision", "decision-a", nil); err != nil || !changed {
		t.Fatalf("clear = %v, %v", changed, err)
	}
	cleared, _ := os.ReadFile(path)
	if strings.Contains(string(cleared), "relations:") {
		t.Fatalf("cleared relations still written:\n%s", cleared)
	}
}

func TestSetRecordRelationsOnNoteMarker(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	if _, err := store.AppendNote(ctx, domain.Note{ID: "note-a", ProjectID: project.ID, Title: "Note", Body: "body", CreatedAt: testClock}); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	changed, err := store.SetRecordRelations(ctx, project.ID, "note", "note-a", relationFixture())
	if err != nil || !changed {
		t.Fatalf("SetRecordRelations = %v, %v", changed, err)
	}
	notes, err := store.ListNotes(ctx, project.ID)
	if err != nil || len(notes) != 1 || len(notes[0].Relations) != 2 {
		t.Fatalf("notes = %+v, %v", notes, err)
	}
	if notes[0].Body != "body" {
		t.Fatalf("note body = %q", notes[0].Body)
	}
	changed, err = store.SetRecordRelations(ctx, project.ID, "note", "note-a", relationFixture())
	if err != nil || changed {
		t.Fatalf("repeat = %v, %v", changed, err)
	}
}

func TestUpdateDecisionKeepsRelationsAndMergeHistory(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	if _, err := store.AppendDecision(ctx, domain.Decision{
		ID: "decision-a", ProjectID: project.ID, Title: "Cache", Context: "old",
		Status: domain.DecisionAccepted, Relations: relationFixture(), CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	decision, err := readDecisionFile(recordFilePath(t, store, project.ID, "decision", "decision-a"))
	if err != nil {
		t.Fatal(err)
	}
	decision.Context = "new context"
	if err := store.UpdateDecision(ctx, decision); err != nil {
		t.Fatalf("UpdateDecision: %v", err)
	}
	stored, err := readDecisionFile(recordFilePath(t, store, project.ID, "decision", "decision-a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Relations) != 2 {
		t.Fatalf("relations lost: %+v", stored.Relations)
	}
	// Merge history is written only by the merger: an ordinary decision update
	// keeps whatever is stored, so an edit cannot fake a merge.
	if len(stored.MergeInfo.MergedFrom) != 0 {
		t.Fatalf("UpdateDecision wrote merge history: %+v", stored.MergeInfo)
	}
	// An update that carries no relations keeps the stored ones.
	stored.Relations = nil
	stored.Context = "third context"
	if err := store.UpdateDecision(ctx, stored); err != nil {
		t.Fatalf("UpdateDecision without relations: %v", err)
	}
	stored, err = readDecisionFile(recordFilePath(t, store, project.ID, "decision", "decision-a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Relations) != 2 {
		t.Fatalf("an update without relations dropped them: %+v", stored.Relations)
	}
}

func TestInvalidRelationFrontmatterIsReported(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	dir := filepath.Join(store.Root(), "projects", project.ID, "decisions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `---
project_id: ` + project.ID + `
kind: decision
schema_version: 1
id: decision-bad
title: Bad relation
created_at: 2026-11-05T10:30:00Z
relations:
  - target: decision-other
    relation: likes
---
# Bad relation

Status: accepted
`
	path := filepath.Join(dir, "decision-bad.md")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	decisions, err := store.ListDecisions(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 0 {
		t.Fatalf("record with an unknown verb listed: %+v", decisions)
	}
	problems, err := store.Problems(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0].Reason, "unknown relation") {
		t.Fatalf("problems = %+v", problems)
	}
}

func TestMergeInfoRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	if _, err := store.AppendDecision(ctx, domain.Decision{
		ID: "decision-a", ProjectID: project.ID, Title: "Survivor", Status: domain.DecisionAccepted,
		MergeInfo: domain.MergeInfo{MergedFrom: []string{"decision-b", "decision-c"}}, CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	decisions, err := store.ListDecisions(ctx, project.ID)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("decisions = %+v, %v", decisions, err)
	}
	if !reflect.DeepEqual(decisions[0].MergeInfo.MergedFrom, []string{"decision-b", "decision-c"}) {
		t.Fatalf("merged_from = %+v", decisions[0].MergeInfo)
	}
	raw, _ := os.ReadFile(recordFilePath(t, store, project.ID, "decision", "decision-a"))
	if !strings.Contains(string(raw), "merged_from:") {
		t.Fatalf("merged_from missing from frontmatter:\n%s", raw)
	}
}
