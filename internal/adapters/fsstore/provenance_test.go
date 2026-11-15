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

// TestLegacyFrontmatterDefaultsToHumanReviewed reads a record written before
// provenance existed: absent keys must behave as human, reviewed, verified.
func TestLegacyFrontmatterDefaultsToHumanReviewed(t *testing.T) {
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
	path := filepath.Join(dir, "2026-11-01-legacy-decision.md")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	decision, err := readDecisionFile(path)
	if err != nil {
		t.Fatalf("read legacy decision: %v", err)
	}
	if decision.Origin != domain.OriginHuman || !decision.Reviewed || decision.Trust != domain.TrustVerified {
		t.Fatalf("legacy provenance = %+v, want human/reviewed/verified", decision.Provenance)
	}
	if decision.Quarantined() || decision.EffectiveTrust() != domain.TrustVerified {
		t.Fatalf("legacy decision is untrusted: %+v", decision.Provenance)
	}

	// The same holds for a note marker without provenance keys.
	notesDirPath := filepath.Join(store.root, projectsDir, project.ID, notesDir)
	if err := os.MkdirAll(notesDirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	notesFile := noteFileFrontmatter{
		Frontmatter: markdown.Frontmatter{
			ProjectID: project.ID, Kind: "note", UpdatedAt: testClock, SchemaVersion: SchemaVersion,
		},
		Date: "2026-11-01",
	}
	notesRaw, err := markdown.Render(notesFile, "<!-- brain:note {\"id\":\"note-legacy\",\"title\":\"Old note\",\"created_at\":\"2026-11-01T00:00:00Z\"} -->\n## Old note\n\nlegacy body\n\n")
	if err != nil {
		t.Fatal(err)
	}
	notesPath := filepath.Join(notesDirPath, "2026-11-01.md")
	if err := os.WriteFile(notesPath, notesRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, notes, err := readNotesFile(notesPath)
	if err != nil {
		t.Fatalf("read legacy notes: %v", err)
	}
	if len(notes) != 1 || notes[0].Origin != domain.OriginHuman || !notes[0].Reviewed || notes[0].Trust != domain.TrustVerified {
		t.Fatalf("legacy note provenance = %+v", notes)
	}
}

func TestRecordProvenanceRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	project := seedProject(t, store)
	provenance := domain.Provenance{Origin: domain.OriginAgent, Reviewed: false, Trust: domain.TrustStated}
	if _, err := store.AppendDecision(ctx, domain.Decision{
		ProjectID: project.ID, Title: "Agent claim", Status: domain.DecisionProposed,
		Context: "agent text", Provenance: provenance, CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	decisions, err := store.ListDecisions(ctx, project.ID)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("ListDecisions = %+v, %v", decisions, err)
	}
	got := decisions[0].Provenance
	if got != provenance || !decisions[0].Quarantined() {
		t.Fatalf("decision provenance = %+v, want %+v", got, provenance)
	}
	path, err := findRecordPath(filepath.Join(store.root, projectsDir, project.ID, decisionsDir), ports.RecordDecision, decisions[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"origin: agent", "reviewed: false", "trust: stated"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("decision file misses %q:\n%s", want, raw)
		}
	}

	// A human note keeps its file clean: origin is explicit, reviewed and
	// verified are omitted because they are the defaults.
	if _, err := store.AppendNote(ctx, domain.Note{
		ProjectID: project.ID, Title: "Human note", Body: "owner text", CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	notes, err := store.ListNotes(ctx, project.ID)
	if err != nil || len(notes) != 1 {
		t.Fatalf("ListNotes = %+v, %v", notes, err)
	}
	if notes[0].Origin != domain.OriginHuman || !notes[0].Reviewed || notes[0].Trust != domain.TrustVerified {
		t.Fatalf("human note provenance = %+v", notes[0].Provenance)
	}
	notesPath, err := findNoteFile(filepath.Join(store.root, projectsDir, project.ID, notesDir), notes[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	notesRaw, err := os.ReadFile(notesPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(notesRaw), `"origin":"human"`) {
		t.Fatalf("human note marker has no origin:\n%s", notesRaw)
	}
	if strings.Contains(string(notesRaw), `"reviewed"`) || strings.Contains(string(notesRaw), `"trust"`) {
		t.Fatalf("human note marker repeats defaults:\n%s", notesRaw)
	}

	// Attempts and handoffs share the same frontmatter plumbing.
	if _, err := store.AppendAttempt(ctx, domain.Attempt{
		ProjectID: project.ID, Title: "Agent attempt", Reason: "why", Provenance: provenance, CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendAttempt: %v", err)
	}
	attempts, err := store.ListAttempts(ctx, project.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Provenance != provenance {
		t.Fatalf("attempt provenance = %+v, %v", attempts, err)
	}
	if _, err := store.AppendHandoff(ctx, domain.Handoff{
		ProjectID: project.ID, Summary: "agent handoff", Provenance: provenance, CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendHandoff: %v", err)
	}
	handoffs, err := store.ListHandoffs(ctx, project.ID)
	if err != nil || len(handoffs) != 1 || handoffs[0].Provenance != provenance {
		t.Fatalf("handoff provenance = %+v, %v", handoffs, err)
	}
}

func TestUnknownProvenanceIsPreservedOnRead(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	dir := filepath.Join(store.root, projectsDir, project.ID, decisionsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	reviewed := true
	frontmatter := decisionFrontmatter{
		Frontmatter: markdown.Frontmatter{
			ProjectID: project.ID, Kind: "decision", UpdatedAt: testClock, SchemaVersion: SchemaVersion,
		},
		provenanceFrontmatter: provenanceFrontmatter{Origin: "robot", Reviewed: &reviewed, Trust: "guessed"},
		ID:                    "decision-unknown",
		Title:                 "Unknown origin",
		Status:                domain.DecisionAccepted,
		CreatedAt:             testClock,
	}
	raw, err := markdown.Render(frontmatter, "# Unknown origin\n\nStatus: accepted\n\n## Context\n\nx\n")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-11-02-unknown-origin.md")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	decision, err := readDecisionFile(path)
	if err != nil {
		t.Fatalf("read unknown provenance: %v", err)
	}
	if decision.Origin != "robot" || decision.Trust != "guessed" || !decision.Reviewed {
		t.Fatalf("unknown provenance was not preserved: %+v", decision.Provenance)
	}
	if !decision.Quarantined() {
		t.Fatal("unknown origin is not quarantined")
	}
	if decision.EffectiveTrust() != domain.TrustInferred {
		t.Fatalf("unknown trust = %q, want inferred", decision.EffectiveTrust())
	}
}

func TestReviewRecordClearsQuarantineAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	project := seedProject(t, store)
	agent := domain.Provenance{Origin: domain.OriginAgent, Reviewed: false}
	if _, err := store.AppendNote(ctx, domain.Note{
		ProjectID: project.ID, Title: "Agent note", Body: "agent body", Provenance: agent, CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	notes, err := store.ListNotes(ctx, project.ID)
	if err != nil || len(notes) != 1 || !notes[0].Quarantined() {
		t.Fatalf("notes before review = %+v, %v", notes, err)
	}
	changed, err := store.ReviewRecord(ctx, project.ID, ports.RecordNote, notes[0].ID, domain.TrustStated)
	if err != nil || !changed {
		t.Fatalf("ReviewRecord(note) = %v, %v, want changed", changed, err)
	}
	if changed, err := store.ReviewRecord(ctx, project.ID, ports.RecordNote, notes[0].ID, domain.TrustStated); err != nil || changed {
		t.Fatalf("second ReviewRecord(note) = %v, %v, want an idempotent no-op", changed, err)
	}
	notes, err = store.ListNotes(ctx, project.ID)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes after review = %+v, %v", notes, err)
	}
	if notes[0].Quarantined() || !notes[0].Reviewed || notes[0].Trust != domain.TrustStated {
		t.Fatalf("note after review = %+v", notes[0].Provenance)
	}
	if notes[0].Body != "agent body" {
		t.Fatalf("note body changed by review: %q", notes[0].Body)
	}

	if _, err := store.AppendDecision(ctx, domain.Decision{
		ProjectID: project.ID, Title: "Agent decision", Status: domain.DecisionProposed,
		Context: "ctx", Provenance: agent, CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	decisions, err := store.ListDecisions(ctx, project.ID)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("ListDecisions = %+v, %v", decisions, err)
	}
	if changed, err := store.ReviewRecord(ctx, project.ID, ports.RecordDecision, decisions[0].ID, ""); err != nil || !changed {
		t.Fatalf("ReviewRecord(decision) = %v, %v, want changed", changed, err)
	}
	decisions, err = store.ListDecisions(ctx, project.ID)
	if err != nil || len(decisions) != 1 || decisions[0].Quarantined() || !decisions[0].Reviewed {
		t.Fatalf("decision after review = %+v, %v", decisions, err)
	}
	if !decisions[0].UpdatedAt.Equal(testClock) {
		t.Fatalf("review changed updated_at: %v", decisions[0].UpdatedAt)
	}
}

func TestReviewRecordRejectsUnknownTrust(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	project := seedProject(t, store)
	if _, err := store.ReviewRecord(ctx, project.ID, ports.RecordDecision, "decision-x", domain.Trust("guessed")); err == nil {
		t.Fatal("unknown trust accepted")
	}
}

func TestNoteReviewKeepsSiblingEntries(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	project := seedProject(t, store)
	day := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	if _, err := store.AppendNote(ctx, domain.Note{
		ProjectID: project.ID, Title: "Quarantined", Body: "first", CreatedAt: day,
		Provenance: domain.Provenance{Origin: domain.OriginConnector},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendNote(ctx, domain.Note{
		ProjectID: project.ID, Title: "Trusted", Body: "second", CreatedAt: day.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	notes, err := store.ListNotes(ctx, project.ID)
	if err != nil || len(notes) != 2 {
		t.Fatalf("notes = %+v, %v", notes, err)
	}
	var quarantineID string
	for _, note := range notes {
		if note.Quarantined() {
			quarantineID = note.ID
		}
	}
	if quarantineID == "" {
		t.Fatal("no quarantined note")
	}
	if changed, err := store.ReviewRecord(ctx, project.ID, ports.RecordNote, quarantineID, ""); err != nil || !changed {
		t.Fatalf("ReviewRecord = %v, %v", changed, err)
	}
	after, err := store.ListNotes(ctx, project.ID)
	if err != nil || len(after) != 2 {
		t.Fatalf("notes after review = %+v, %v", after, err)
	}
	bodies := map[string]string{}
	for _, note := range after {
		bodies[note.Title] = note.Body
		if note.Quarantined() {
			t.Fatalf("still quarantined: %+v", note.Provenance)
		}
	}
	if bodies["Quarantined"] != "first" || bodies["Trusted"] != "second" {
		t.Fatalf("review lost sibling entries: %+v", after)
	}
}
