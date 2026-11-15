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
)

func engramTaxonomy() domain.Taxonomy {
	return domain.Taxonomy{Subtype: "architecture", Scope: domain.ScopePersonal, TopicKey: "architecture/auth-model", Occurrences: 3}
}

func TestEngramFieldsRoundTrip(t *testing.T) {
	store := newTestStore(t)
	seedProject(t, store)
	ctx := context.Background()

	note := domain.Note{
		ProjectID: "lv24-brain", Title: "Auth model", Body: "current body",
		Taxonomy:     engramTaxonomy(),
		TopicHistory: domain.TopicHistory{History: []domain.HistoryEntry{{At: "2026-11-01T10:00:00Z", Body: "older body"}}},
	}
	if _, err := store.AppendNote(ctx, note); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	notes, err := store.ListNotes(ctx, "lv24-brain")
	if err != nil {
		t.Fatalf("ListNotes: %v", err)
	}
	if len(notes) != 1 {
		t.Fatalf("notes = %d", len(notes))
	}
	got := notes[0]
	if got.Subtype != "architecture" || got.Scope != domain.ScopePersonal || got.TopicKey != "architecture/auth-model" || got.Occurrences != 3 {
		t.Fatalf("note taxonomy = %+v", got.Taxonomy)
	}
	if len(got.History) != 1 || got.History[0].Body != "older body" || got.History[0].At != "2026-11-01T10:00:00Z" {
		t.Fatalf("note history = %+v", got.History)
	}
	if got.Body != "current body" {
		t.Fatalf("note body = %q", got.Body)
	}

	decision := domain.Decision{
		ProjectID: "lv24-brain", Title: "Use SQLite", Status: domain.DecisionAccepted,
		Context: "local index", Taxonomy: engramTaxonomy(),
	}
	if _, err := store.AppendDecision(ctx, decision); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	decisions, err := store.ListDecisions(ctx, "lv24-brain")
	if err != nil || len(decisions) != 1 {
		t.Fatalf("ListDecisions = %d, %v", len(decisions), err)
	}
	if decisions[0].TopicKey != "architecture/auth-model" || decisions[0].Occurrences != 3 || decisions[0].Scope != domain.ScopePersonal {
		t.Fatalf("decision taxonomy = %+v", decisions[0].Taxonomy)
	}

	attempt := domain.Attempt{
		ProjectID: "lv24-brain", Title: "Warm cache", Hypothesis: "faster", Result: "no change",
		Taxonomy: engramTaxonomy(),
	}
	if _, err := store.AppendAttempt(ctx, attempt); err != nil {
		t.Fatalf("AppendAttempt: %v", err)
	}
	attempts, err := store.ListAttempts(ctx, "lv24-brain")
	if err != nil || len(attempts) != 1 {
		t.Fatalf("ListAttempts = %d, %v", len(attempts), err)
	}
	if attempts[0].TopicKey != "architecture/auth-model" || attempts[0].Subtype != "architecture" {
		t.Fatalf("attempt taxonomy = %+v", attempts[0].Taxonomy)
	}

	handoff := domain.Handoff{
		ProjectID: "lv24-brain", Summary: "wired the fields", Taxonomy: engramTaxonomy(),
		Source: &domain.SourceRef{ExternalID: "engram:handoff-1"},
	}
	if _, err := store.AppendHandoff(ctx, handoff); err != nil {
		t.Fatalf("AppendHandoff: %v", err)
	}
	handoffs, err := store.ListHandoffs(ctx, "lv24-brain")
	if err != nil || len(handoffs) != 1 {
		t.Fatalf("ListHandoffs = %d, %v", len(handoffs), err)
	}
	if handoffs[0].TopicKey != "architecture/auth-model" || handoffs[0].Occurrences != 3 {
		t.Fatalf("handoff taxonomy = %+v", handoffs[0].Taxonomy)
	}
	if handoffs[0].Source == nil || handoffs[0].Source.ExternalID != "engram:handoff-1" {
		t.Fatalf("handoff source = %+v", handoffs[0].Source)
	}
}

func TestDefaultTaxonomyIsOmittedFromFrontmatter(t *testing.T) {
	store := newTestStore(t)
	seedProject(t, store)
	ctx := context.Background()
	if _, err := store.AppendDecision(ctx, domain.Decision{ProjectID: "lv24-brain", Title: "Plain", Context: "body"}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	paths := globFiles(t, filepath.Join(t.TempDir(), "unused"))
	_ = paths
	root := store.root
	raw, err := os.ReadFile(filepath.Join(root, "projects", "lv24-brain", "decisions", mustDecisionFile(t, store)))
	if err != nil {
		t.Fatalf("read decision: %v", err)
	}
	for _, key := range []string{"subtype:", "scope:", "topic_key:", "occurrences:"} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("default frontmatter contains %q:\n%s", key, raw)
		}
	}
	if !strings.Contains(string(raw), "schema_version: 1") {
		t.Fatalf("record schema_version was bumped:\n%s", raw)
	}
}

func mustDecisionFile(t *testing.T, store *Store) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(store.root, "projects", "lv24-brain", "decisions"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("decisions dir = %d entries, %v", len(entries), err)
	}
	return entries[0].Name()
}

func TestOldFilesWithoutEngramFieldsStayValid(t *testing.T) {
	store := newTestStore(t)
	seedProject(t, store)
	dir := filepath.Join(store.root, "projects", "lv24-brain")
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	noteBody := `<!-- brain:note {"id":"note-legacy","title":"Legacy","created_at":"2026-01-02T00:00:00Z"} -->` + "\n\n## Legacy\n\nold body\n"
	raw, err := markdown.Render(noteFileFrontmatter{
		Frontmatter: markdown.Frontmatter{ProjectID: "lv24-brain", Kind: "note", UpdatedAt: at, SchemaVersion: SchemaVersion},
		Date:        "2026-01-02",
	}, noteBody)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes", "2026-01-02.md"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	notes, err := store.ListNotes(context.Background(), "lv24-brain")
	if err != nil || len(notes) != 1 {
		t.Fatalf("ListNotes = %d, %v", len(notes), err)
	}
	if notes[0].ID != "note-legacy" || notes[0].Body != "old body" {
		t.Fatalf("legacy note = %+v", notes[0])
	}
	if notes[0].TopicKey != "" || notes[0].Occurrences != 0 || len(notes[0].History) != 0 || notes[0].Scope != "" {
		t.Fatalf("legacy taxonomy was invented: %+v", notes[0].Taxonomy)
	}

	decisionRaw, err := markdown.Render(decisionFrontmatter{
		Frontmatter: markdown.Frontmatter{ProjectID: "lv24-brain", Kind: "decision", UpdatedAt: at, SchemaVersion: SchemaVersion},
		ID:          "decision-legacy",
		CreatedAt:   at,
	}, "# Old decision\n\nStatus: accepted\n\n## Context\n\nold context\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "decisions"), 0o755); err != nil {
		t.Fatal(err)
	}
	decisionPath := filepath.Join(dir, "decisions", "2026-01-02-old.md")
	if err := os.WriteFile(decisionPath, decisionRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	decision, err := readDecisionFile(decisionPath)
	if err != nil {
		t.Fatalf("readDecisionFile: %v", err)
	}
	if decision.TopicKey != "" || decision.Occurrences != 0 || decision.Subtype != "" || decision.Scope != "" {
		t.Fatalf("legacy decision taxonomy was invented: %+v", decision.Taxonomy)
	}
}

func TestReplaceDecisionKeepsIdentityProvenanceAndHistory(t *testing.T) {
	store := newTestStore(t)
	seedProject(t, store)
	ctx := context.Background()
	created := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	source := &domain.SourceRef{ExternalID: "engram:sync-1"}
	original := domain.Decision{
		ProjectID: "lv24-brain", Title: "Auth", Status: domain.DecisionAccepted,
		Context: "old body", CreatedAt: created,
		Provenance: domain.Provenance{Origin: domain.OriginConnector, Reviewed: false, Trust: domain.TrustStated},
		Source:     source,
		Taxonomy:   engramTaxonomy(),
		Validity:   domain.Validity{ValidFrom: created, Supersedes: "decision-old"},
	}
	if _, err := store.AppendDecision(ctx, original); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	decisions, err := store.ListDecisions(ctx, "lv24-brain")
	if err != nil || len(decisions) != 1 {
		t.Fatalf("ListDecisions = %d, %v", len(decisions), err)
	}
	next := decisions[0]
	next.Context = "new body"
	next.History = domain.PrependedHistory(next.History,
		domain.HistoryEntry{At: created.Format(time.RFC3339), Body: "old body"})
	next.Taxonomy.Occurrences = 2
	updated, err := store.ReplaceDecision(ctx, next)
	if err != nil {
		t.Fatalf("ReplaceDecision: %v", err)
	}
	if updated.CreatedAt != created {
		t.Fatalf("created_at = %v, want %v", updated.CreatedAt, created)
	}
	if updated.Provenance != original.Provenance {
		t.Fatalf("provenance = %+v, want %+v", updated.Provenance, original.Provenance)
	}
	if updated.Source == nil || updated.Source.ExternalID != "engram:sync-1" {
		t.Fatalf("source dropped: %+v", updated.Source)
	}
	stored, err := store.ListDecisions(ctx, "lv24-brain")
	if err != nil || len(stored) != 1 {
		t.Fatalf("ListDecisions after replace = %d, %v", len(stored), err)
	}
	if stored[0].Context != "new body" {
		t.Fatalf("context = %q", stored[0].Context)
	}
	if len(stored[0].History) != 1 || stored[0].History[0].Body != "old body" {
		t.Fatalf("history = %+v", stored[0].History)
	}
	if stored[0].Occurrences != 2 || stored[0].TopicKey != "architecture/auth-model" {
		t.Fatalf("taxonomy = %+v", stored[0].Taxonomy)
	}
	if stored[0].Validity.Supersedes != "decision-old" {
		t.Fatalf("validity dropped: %+v", stored[0].Validity)
	}
}

func TestReplaceNoteKeepsEntryInPlace(t *testing.T) {
	store := newTestStore(t)
	seedProject(t, store)
	ctx := context.Background()
	base := store.nowUTC()
	for i, body := range []string{"keep me", "update me"} {
		note := domain.Note{ProjectID: "lv24-brain", Title: "Topic", Body: body,
			Taxonomy: domain.Taxonomy{Subtype: "discovery", TopicKey: "topic/a"}}
		note.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		if _, err := store.AppendNote(ctx, note); err != nil {
			t.Fatalf("AppendNote: %v", err)
		}
	}
	notes, err := store.ListNotes(ctx, "lv24-brain")
	if err != nil || len(notes) != 2 {
		t.Fatalf("ListNotes = %d, %v", len(notes), err)
	}
	var target domain.Note
	for _, note := range notes {
		if note.Body == "update me" {
			target = note
		}
	}
	target.Body = "updated"
	target.History = domain.PrependedHistory(target.History,
		domain.HistoryEntry{At: target.UpdatedAt.Format(time.RFC3339), Body: "update me"})
	if _, err := store.ReplaceNote(ctx, target); err != nil {
		t.Fatalf("ReplaceNote: %v", err)
	}
	stored, err := store.ListNotes(ctx, "lv24-brain")
	if err != nil || len(stored) != 2 {
		t.Fatalf("ListNotes after replace = %d, %v", len(stored), err)
	}
	bodies := map[string]bool{}
	for _, note := range stored {
		bodies[note.Body] = true
		if note.ID == target.ID {
			if len(note.History) != 1 || note.History[0].Body != "update me" {
				t.Fatalf("note history = %+v", note.History)
			}
			if note.CreatedAt != target.CreatedAt {
				t.Fatalf("created_at changed: %v -> %v", target.CreatedAt, note.CreatedAt)
			}
		}
	}
	if !bodies["keep me"] || !bodies["updated"] || bodies["update me"] {
		t.Fatalf("bodies = %v", bodies)
	}
}

func TestHistorySectionParseRoundTrip(t *testing.T) {
	history := []domain.HistoryEntry{
		{At: "2026-11-02T10:00:00Z", Body: "newest body\n\nwith a paragraph"},
		{At: "2026-11-01T10:00:00Z", Body: "older body"},
	}
	rendered := appendHistory("current", history)
	current, parsed := splitHistorySection(rendered)
	if current != "current" {
		t.Fatalf("current body = %q", current)
	}
	if len(parsed) != 2 {
		t.Fatalf("parsed history = %+v", parsed)
	}
	if parsed[0].Body != "newest body\n\nwith a paragraph" || parsed[0].At != "2026-11-02T10:00:00Z" {
		t.Fatalf("first entry = %+v", parsed[0])
	}
	if parsed[1].Body != "older body" || parsed[1].At != "2026-11-01T10:00:00Z" {
		t.Fatalf("second entry = %+v", parsed[1])
	}

	handWritten, parsed := splitHistorySection(appendHistory("current", []domain.HistoryEntry{{Body: "hand-written entry"}}))
	if handWritten != "current" || len(parsed) != 1 || parsed[0].At != "" || parsed[0].Body != "hand-written entry" {
		t.Fatalf("hand-written history = %q, %+v", handWritten, parsed)
	}
}
