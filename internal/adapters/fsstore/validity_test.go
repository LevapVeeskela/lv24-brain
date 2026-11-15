package fsstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestLegacyDecisionDefaultsToCreatedAt(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	dir := filepath.Join(store.root, projectsDir, project.ID, decisionsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := decisionFrontmatter{
		Frontmatter: markdown.Frontmatter{
			ProjectID: project.ID, Kind: "decision", UpdatedAt: testClock, SchemaVersion: SchemaVersion,
		},
		ID: "decision-legacy", Title: "Legacy decision", Status: domain.DecisionAccepted, CreatedAt: testClock,
	}
	raw, err := markdown.Render(legacy, "# Legacy decision\n\nStatus: accepted\n\n## Context\n\nlegacy text\n")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-11-05-legacy-decision.md")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	decision, err := readDecisionFile(path)
	if err != nil {
		t.Fatalf("read legacy decision: %v", err)
	}
	if !decision.Validity.ValidFrom.Equal(testClock) || !decision.Validity.ValidTo.IsZero() || decision.Validity.SupersededBy != "" {
		t.Fatalf("legacy validity = %+v, want valid_from=created_at and current", decision.Validity)
	}
}

func TestDefaultRecordWritesNoValidityFields(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	ctx := context.Background()
	if _, err := store.AppendDecision(ctx, domain.Decision{
		ProjectID: project.ID, Title: "Clean decision", Status: domain.DecisionAccepted,
		CreatedAt: testClock, UpdatedAt: testClock,
	}); err != nil {
		t.Fatal(err)
	}
	files := globFiles(t, filepath.Join(store.root, projectsDir, project.ID, decisionsDir, "*.md"))
	if len(files) != 1 {
		t.Fatalf("decision files = %v", files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"valid_from", "valid_to", "superseded_by", "supersedes", "confidence"} {
		if strings.Contains(string(raw), key+":") {
			t.Errorf("default record wrote %q:\n%s", key, raw)
		}
	}
}

func TestValidityRoundTripDecision(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	ctx := context.Background()
	created := testClock
	validFrom := created.Add(-48 * time.Hour)
	validTo := created.Add(24 * time.Hour)
	if _, err := store.AppendDecision(ctx, domain.Decision{
		ProjectID: project.ID, Title: "Use SQLite", Status: domain.DecisionAccepted,
		Context: "local index", CreatedAt: created, UpdatedAt: created,
		Validity: domain.Validity{
			ValidFrom: validFrom, ValidTo: validTo,
			SupersededBy: "decision-2026-11-06-new", Supersedes: "decision-2026-11-01-old",
			Confidence: 0.75,
		},
	}); err != nil {
		t.Fatal(err)
	}
	decisions, err := store.ListDecisions(ctx, project.ID)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("decisions = %+v, %v", decisions, err)
	}
	got := decisions[0].Validity
	if !got.ValidFrom.Equal(validFrom) || !got.ValidTo.Equal(validTo) ||
		got.SupersededBy != "decision-2026-11-06-new" || got.Supersedes != "decision-2026-11-01-old" || got.Confidence != 0.75 {
		t.Fatalf("validity = %+v", got)
	}
	if decisions[0].Status != domain.DecisionAccepted {
		t.Fatalf("status = %q", decisions[0].Status)
	}
}

func TestSetRecordValidityPreservesBody(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	ctx := context.Background()
	if _, err := store.AppendDecision(ctx, domain.Decision{
		ProjectID: project.ID, Title: "Use SQLite", Status: domain.DecisionAccepted,
		Context: "local index", Consequences: []string{"derived index"}, CreatedAt: testClock, UpdatedAt: testClock,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := store.ListDecisions(ctx, project.ID)
	if err != nil || len(before) != 1 {
		t.Fatalf("decisions = %+v, %v", before, err)
	}
	supersededAt := testClock.Add(time.Hour)
	validity := domain.Validity{ValidFrom: testClock, ValidTo: supersededAt, SupersededBy: "decision-2026-11-06-new"}
	if err := store.SetRecordValidity(ctx, project.ID, ports.RecordDecision, before[0].ID, validity); err != nil {
		t.Fatalf("SetRecordValidity: %v", err)
	}
	after, err := store.ListDecisions(ctx, project.ID)
	if err != nil || len(after) != 1 {
		t.Fatalf("decisions after = %+v, %v", after, err)
	}
	if !after[0].Validity.ValidTo.Equal(supersededAt) || after[0].Validity.SupersededBy != "decision-2026-11-06-new" {
		t.Fatalf("validity after = %+v", after[0].Validity)
	}
	if after[0].Context != "local index" || len(after[0].Consequences) != 1 {
		t.Fatalf("decision text changed: %+v", after[0])
	}
	if after[0].Status != domain.DecisionAccepted || !after[0].CreatedAt.Equal(testClock) {
		t.Fatalf("decision identity changed: %+v", after[0])
	}
}

func TestNoteValidityRoundTrip(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	ctx := context.Background()
	if _, err := store.AppendNote(ctx, domain.Note{
		ProjectID: project.ID, Title: "First note", Body: "hello world", CreatedAt: testClock, UpdatedAt: testClock,
	}); err != nil {
		t.Fatal(err)
	}
	notes, err := store.ListNotes(ctx, project.ID)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes = %+v, %v", notes, err)
	}
	id := notes[0].ID
	if !notes[0].Validity.ValidFrom.Equal(testClock) || !notes[0].Validity.ValidTo.IsZero() {
		t.Fatalf("default note validity = %+v", notes[0].Validity)
	}
	validTo := testClock.Add(2 * time.Hour)
	if err := store.SetRecordValidity(ctx, project.ID, ports.RecordNote, id, domain.Validity{
		ValidFrom: testClock, ValidTo: validTo, SupersededBy: "note-2026-11-06-new",
	}); err != nil {
		t.Fatalf("SetRecordValidity note: %v", err)
	}
	reloaded, err := store.ListNotes(ctx, project.ID)
	if err != nil || len(reloaded) != 1 {
		t.Fatalf("notes after = %+v, %v", reloaded, err)
	}
	if reloaded[0].Body != "hello world" || reloaded[0].Title != "First note" {
		t.Fatalf("note text changed: %+v", reloaded[0])
	}
	if !reloaded[0].Validity.ValidTo.Equal(validTo) || reloaded[0].Validity.SupersededBy != "note-2026-11-06-new" {
		t.Fatalf("note validity after = %+v", reloaded[0].Validity)
	}
}

func TestAttemptValidityFrontmatter(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	ctx := context.Background()
	validFrom := testClock.Add(-time.Hour)
	if _, err := store.AppendAttempt(ctx, domain.Attempt{
		ProjectID: project.ID, Title: "Cache warmup", Hypothesis: "warmer is faster",
		CreatedAt: testClock, UpdatedAt: testClock,
		Validity: domain.Validity{ValidFrom: validFrom, Confidence: 0.4},
	}); err != nil {
		t.Fatal(err)
	}
	attempts, err := store.ListAttempts(ctx, project.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %+v, %v", attempts, err)
	}
	if !attempts[0].Validity.ValidFrom.Equal(validFrom) || attempts[0].Validity.Confidence != 0.4 {
		t.Fatalf("attempt validity = %+v", attempts[0].Validity)
	}
}

func TestUnparseableValidityIsRejected(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	dir := filepath.Join(store.root, projectsDir, project.ID, decisionsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	frontmatter := decisionFrontmatter{
		Frontmatter: markdown.Frontmatter{
			ProjectID: project.ID, Kind: "decision", UpdatedAt: testClock, SchemaVersion: SchemaVersion,
		},
		validityFrontmatter: validityFrontmatter{ValidTo: "not a timestamp"},
		ID:                  "decision-broken", Title: "Broken", Status: domain.DecisionAccepted, CreatedAt: testClock,
	}
	raw, err := markdown.Render(frontmatter, "# Broken\n\nStatus: accepted\n")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-11-05-broken.md")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readDecisionFile(path); err == nil {
		t.Fatal("unparseable valid_to accepted")
	}
	// The listing skips the file and reports it, so nothing silently looks current.
	decisions, err := store.ListDecisions(context.Background(), project.ID)
	if err != nil || len(decisions) != 0 {
		t.Fatalf("listing = %+v, %v; want the broken file skipped", decisions, err)
	}
	problems, err := store.Problems(context.Background(), project.ID)
	if err != nil || len(problems) != 1 {
		t.Fatalf("problems = %+v, %v", problems, err)
	}
}
