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

func TestDecisionRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	created := time.Date(2026, 11, 5, 9, 15, 0, 0, time.UTC)
	decision := domain.Decision{
		ProjectID:    project.ID,
		Title:        "Use PostgreSQL",
		Status:       domain.DecisionAccepted,
		Context:      "Server mode needs relational storage.\n\n## Notes\n\nKeep it single-node.",
		Alternatives: []string{"SQLite\nfor local mode", "Files only"},
		Consequences: []string{"migrations required"},
		Evidence:     []string{"docs/product-design.md"},
		Permalink:    "/projects/lv24-brain/decisions/use-postgresql",
		CreatedAt:    created,
	}
	if _, err := store.AppendDecision(ctx, decision); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}

	dir := filepath.Join(store.Root(), "projects", project.ID, "decisions")
	files := globFiles(t, filepath.Join(dir, "*.md"))
	if len(files) != 1 {
		t.Fatalf("decision files = %v", files)
	}
	if name := filepath.Base(files[0]); name != "2026-11-05-use-postgresql.md" {
		t.Fatalf("decision file name = %q", name)
	}
	got, err := readDecisionFile(files[0])
	if err != nil {
		t.Fatalf("readDecisionFile: %v", err)
	}
	if got.ID != "decision-2026-11-05-use-postgresql" {
		t.Fatalf("decision ID = %q", got.ID)
	}
	if got.ProjectID != decision.ProjectID || got.Title != decision.Title || got.Status != decision.Status {
		t.Fatalf("decision identity mismatch: %+v", got)
	}
	if got.Context != decision.Context {
		t.Fatalf("context = %q, want %q", got.Context, decision.Context)
	}
	if !reflect.DeepEqual(got.Alternatives, decision.Alternatives) {
		t.Fatalf("alternatives = %#v, want %#v", got.Alternatives, decision.Alternatives)
	}
	if !reflect.DeepEqual(got.Consequences, decision.Consequences) {
		t.Fatalf("consequences = %#v, want %#v", got.Consequences, decision.Consequences)
	}
	if !reflect.DeepEqual(got.Evidence, decision.Evidence) {
		t.Fatalf("evidence = %#v, want %#v", got.Evidence, decision.Evidence)
	}
	if got.Permalink != decision.Permalink {
		t.Fatalf("permalink = %q", got.Permalink)
	}
	if !got.CreatedAt.Equal(created) || !got.UpdatedAt.Equal(testClock) {
		t.Fatalf("decision times = created %v updated %v", got.CreatedAt, got.UpdatedAt)
	}

	second := decision
	second.Title = "Use PostgreSQL"
	second.CreatedAt = created
	if _, err := store.AppendDecision(ctx, second); err != nil {
		t.Fatalf("AppendDecision second: %v", err)
	}
	if files := globFiles(t, filepath.Join(dir, "*.md")); len(files) != 2 {
		t.Fatalf("second append created %d files", len(files))
	}
	secondFile := filepath.Join(dir, "2026-11-05-use-postgresql-2.md")
	secondRecord, err := readDecisionFile(secondFile)
	if err != nil {
		t.Fatalf("read second decision: %v", err)
	}
	if secondRecord.ID != "decision-2026-11-05-use-postgresql-2" {
		t.Fatalf("second decision ID = %q", secondRecord.ID)
	}

	provided := decision
	provided.ID = "custom-decision"
	provided.Title = "Provided identifier"
	provided.Status = ""
	if _, err := store.AppendDecision(ctx, provided); err != nil {
		t.Fatalf("AppendDecision provided ID: %v", err)
	}
	providedRecord, err := readDecisionFile(filepath.Join(dir, "2026-11-05-provided-identifier.md"))
	if err != nil {
		t.Fatalf("read provided decision: %v", err)
	}
	if providedRecord.ID != "custom-decision" {
		t.Fatalf("provided ID = %q", providedRecord.ID)
	}
	if providedRecord.Status != domain.DecisionProposed {
		t.Fatalf("default status = %q", providedRecord.Status)
	}

	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read decision raw: %v", err)
	}
	var frontmatter decisionFrontmatter
	body, err := markdown.Parse(raw, &frontmatter)
	if err != nil {
		t.Fatalf("parse decision: %v", err)
	}
	if frontmatter.Kind != "decision" || frontmatter.SchemaVersion != SchemaVersion {
		t.Fatalf("decision frontmatter = %+v", frontmatter)
	}
	if !strings.Contains(body, "## Notes") {
		t.Fatalf("decision body lost its embedded heading:\n%s", body)
	}
}

func TestAttemptRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	created := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	attempt := domain.Attempt{
		ProjectID:  project.ID,
		Title:      "Cache warmup",
		Hypothesis: "Preloading removes cold-start latency.",
		Result:     "No measurable change.",
		Reason:     "The index was already in the OS page cache.",
		CreatedAt:  created,
	}
	if _, err := store.AppendAttempt(ctx, attempt); err != nil {
		t.Fatalf("AppendAttempt: %v", err)
	}
	path := filepath.Join(store.Root(), "projects", project.ID, "attempts", "2026-11-05-cache-warmup.md")
	got, err := readAttemptFile(path)
	if err != nil {
		t.Fatalf("readAttemptFile: %v", err)
	}
	if got.ID != "attempt-2026-11-05-cache-warmup" {
		t.Fatalf("attempt ID = %q", got.ID)
	}
	if got.ProjectID != attempt.ProjectID || got.Title != attempt.Title {
		t.Fatalf("attempt identity = %+v", got)
	}
	if got.Hypothesis != attempt.Hypothesis || got.Result != attempt.Result || got.Reason != attempt.Reason {
		t.Fatalf("attempt content = %+v", got)
	}
	if !got.CreatedAt.Equal(created) || !got.UpdatedAt.Equal(testClock) {
		t.Fatalf("attempt times = created %v updated %v", got.CreatedAt, got.UpdatedAt)
	}
}

func TestHandoffRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	created := time.Date(2026, 11, 5, 10, 15, 0, 0, time.UTC)
	handoff := domain.Handoff{
		ProjectID:  project.ID,
		Summary:    "Filesystem store implemented.",
		GitContext: "develop @ abc1234",
		NextAction: "wire the CLI",
		CreatedAt:  created,
	}
	if _, err := store.AppendHandoff(ctx, handoff); err != nil {
		t.Fatalf("AppendHandoff: %v", err)
	}
	path := filepath.Join(store.Root(), "projects", project.ID, "handoffs", "2026-11-05-101500.md")
	got, err := readHandoffFile(path)
	if err != nil {
		t.Fatalf("readHandoffFile: %v", err)
	}
	if got.ID != "handoff-2026-11-05-101500" {
		t.Fatalf("handoff ID = %q", got.ID)
	}
	if got.ProjectID != handoff.ProjectID || got.Summary != handoff.Summary {
		t.Fatalf("handoff identity = %+v", got)
	}
	if got.GitContext != handoff.GitContext || got.NextAction != handoff.NextAction {
		t.Fatalf("handoff content = %+v", got)
	}
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("handoff CreatedAt = %v, want %v", got.CreatedAt, created)
	}

	if _, err := store.AppendHandoff(ctx, handoff); err != nil {
		t.Fatalf("AppendHandoff second: %v", err)
	}
	second, err := readHandoffFile(filepath.Join(store.Root(), "projects", project.ID, "handoffs", "2026-11-05-101500-2.md"))
	if err != nil {
		t.Fatalf("read second handoff: %v", err)
	}
	if second.ID != "handoff-2026-11-05-101500-2" {
		t.Fatalf("second handoff ID = %q", second.ID)
	}
}

func TestNoteDailyFileRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	first := domain.Note{
		ProjectID: project.ID,
		Title:     "Session notes",
		Body:      "First paragraph.\n\n## Inline heading\n\nMore text.",
		Tags:      []string{"ux", "brain"},
		Permalink: "/notes/session",
		CreatedAt: time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC),
	}
	if _, err := store.AppendNote(ctx, first); err != nil {
		t.Fatalf("AppendNote first: %v", err)
	}
	second := domain.Note{
		ProjectID: project.ID,
		Title:     "Follow-up",
		Body:      "Second body.",
		CreatedAt: time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC),
	}
	if _, err := store.AppendNote(ctx, second); err != nil {
		t.Fatalf("AppendNote second: %v", err)
	}

	dir := filepath.Join(store.Root(), "projects", project.ID, "notes")
	files := globFiles(t, filepath.Join(dir, "*.md"))
	if len(files) != 1 || filepath.Base(files[0]) != "2026-11-05.md" {
		t.Fatalf("daily note files = %v", files)
	}
	frontmatter, notes, err := readNotesFile(files[0])
	if err != nil {
		t.Fatalf("readNotesFile: %v", err)
	}
	if frontmatter.Kind != "note" || frontmatter.SchemaVersion != SchemaVersion {
		t.Fatalf("notes frontmatter = %+v", frontmatter)
	}
	if len(notes) != 2 {
		t.Fatalf("notes = %d: %+v", len(notes), notes)
	}
	if notes[0].ID != "note-2026-11-05-session-notes" || notes[1].ID != "note-2026-11-05-follow-up" {
		t.Fatalf("note IDs = %q, %q", notes[0].ID, notes[1].ID)
	}
	if notes[0].Title != first.Title || notes[0].Body != first.Body {
		t.Fatalf("first note round trip = %+v", notes[0])
	}
	if !reflect.DeepEqual(notes[0].Tags, first.Tags) || notes[0].Permalink != first.Permalink {
		t.Fatalf("first note metadata = %+v", notes[0])
	}
	if !notes[0].CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("first note CreatedAt = %v", notes[0].CreatedAt)
	}
	if !notes[0].UpdatedAt.Equal(testClock) {
		t.Fatalf("first note UpdatedAt = %v", notes[0].UpdatedAt)
	}

	third := domain.Note{
		ProjectID: project.ID,
		ID:        "custom-note-id",
		Title:     "Custom",
		Body:      "Provided identifier.",
		CreatedAt: time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC),
	}
	if _, err := store.AppendNote(ctx, third); err != nil {
		t.Fatalf("AppendNote custom: %v", err)
	}
	if _, duplicateErr := store.AppendNote(ctx, third); duplicateErr == nil {
		t.Fatal("duplicate note id should fail")
	}
	_, notes, err = readNotesFile(files[0])
	if err != nil {
		t.Fatalf("readNotesFile after custom: %v", err)
	}
	if len(notes) != 3 || notes[2].ID != "custom-note-id" {
		t.Fatalf("custom note = %+v", notes)
	}

	nextDay := domain.Note{
		ProjectID: project.ID,
		Title:     "Next day",
		Body:      "New file.",
		CreatedAt: time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC),
	}
	if _, err := store.AppendNote(ctx, nextDay); err != nil {
		t.Fatalf("AppendNote next day: %v", err)
	}
	nextDayFiles := globFiles(t, filepath.Join(dir, "2026-11-06.md"))
	if len(nextDayFiles) != 1 {
		t.Fatalf("next day note file missing: %v", nextDayFiles)
	}
}

func TestEmptyOptionalTextFieldsRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)

	if _, err := store.PutState(ctx, domain.State{ProjectID: project.ID}); err != nil {
		t.Fatalf("PutState: %v", err)
	}
	state, err := store.GetState(ctx, project.ID)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if state.Goal != "" || state.NextAction != "" {
		t.Fatalf("empty state text came back non-empty: %+v", state)
	}
	if len(state.Active) != 0 || len(state.Blocked) != 0 || len(state.OpenQuestions) != 0 {
		t.Fatalf("empty state lists came back non-empty: %+v", state)
	}

	decisionPath, err := store.AppendDecision(ctx, domain.Decision{
		ProjectID: project.ID,
		Title:     "Empty context",
		CreatedAt: testClock,
	})
	if err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	if decisionPath.Value == "" {
		t.Fatal("AppendDecision returned no revision")
	}
	decision, err := readDecisionFile(filepath.Join(store.Root(), "projects", project.ID, "decisions", "2026-11-05-empty-context.md"))
	if err != nil {
		t.Fatalf("readDecisionFile: %v", err)
	}
	if decision.Context != "" || len(decision.Alternatives) != 0 || len(decision.Consequences) != 0 || len(decision.Evidence) != 0 {
		t.Fatalf("empty decision fields came back non-empty: %+v", decision)
	}

	if _, err := store.AppendAttempt(ctx, domain.Attempt{
		ProjectID: project.ID,
		Title:     "Empty attempt",
		CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendAttempt: %v", err)
	}
	attempt, err := readAttemptFile(filepath.Join(store.Root(), "projects", project.ID, "attempts", "2026-11-05-empty-attempt.md"))
	if err != nil {
		t.Fatalf("readAttemptFile: %v", err)
	}
	if attempt.Hypothesis != "" || attempt.Result != "" || attempt.Reason != "" {
		t.Fatalf("empty attempt fields came back non-empty: %+v", attempt)
	}

	if _, err := store.AppendHandoff(ctx, domain.Handoff{
		ProjectID: project.ID,
		Summary:   "Summary only",
		CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendHandoff: %v", err)
	}
	handoff, err := readHandoffFile(filepath.Join(store.Root(), "projects", project.ID, "handoffs", "2026-11-05-103000.md"))
	if err != nil {
		t.Fatalf("readHandoffFile: %v", err)
	}
	if handoff.GitContext != "" || handoff.NextAction != "" {
		t.Fatalf("empty handoff fields came back non-empty: %+v", handoff)
	}
}
