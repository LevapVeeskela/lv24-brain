package app_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

type engramFixture struct {
	app   *app.App
	store *fsstore.Store
	index *sqliteindex.Index
	clock time.Time
}

func newEngramFixture(t *testing.T) *engramFixture {
	t.Helper()
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	index, err := sqliteindex.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("sqliteindex.Open: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	clock := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	cfg := config.Default()
	deps := app.Dependencies{Store: store, Index: index, Now: func() time.Time { return clock }}
	return &engramFixture{app: app.New(cfg, deps), store: store, index: index, clock: clock}
}

func (f *engramFixture) note(t *testing.T, body, topic, subtype string, scope domain.RecordScope) app.NoteResult {
	t.Helper()
	f.clock = f.clock.Add(time.Minute)
	result, err := f.app.Note(context.Background(), app.NoteInput{
		ProjectID: "demo", CreateProject: true, Title: "Topic note", Body: body,
		Topic: topic, Subtype: subtype, Scope: scope,
	})
	if err != nil {
		t.Fatalf("Note(%q): %v", body, err)
	}
	return result
}

func TestTopicUpsertNote(t *testing.T) {
	fixture := newEngramFixture(t)
	ctx := context.Background()
	first := fixture.note(t, "first body", "architecture/auth", "architecture", domain.ScopeProject)
	if first.Already {
		t.Fatal("first write reported unchanged")
	}
	if first.Note.Occurrences != 1 || first.Note.TopicKey != "architecture/auth" || first.Note.Subtype != "architecture" {
		t.Fatalf("first taxonomy = %+v", first.Note.Taxonomy)
	}

	second := fixture.note(t, "second body", "architecture/auth", "", "")
	if second.Note.ID != first.Note.ID {
		t.Fatalf("topic update created a new record: %s -> %s", first.Note.ID, second.Note.ID)
	}
	if second.Note.Occurrences != 2 {
		t.Fatalf("occurrences = %d, want 2", second.Note.Occurrences)
	}
	if second.Note.Subtype != "architecture" {
		t.Fatalf("stored subtype was lost: %q", second.Note.Subtype)
	}
	if second.Note.Body != "second body" {
		t.Fatalf("body = %q", second.Note.Body)
	}
	if len(second.Note.History) != 1 || second.Note.History[0].Body != "first body" {
		t.Fatalf("history = %+v", second.Note.History)
	}
	if !second.Note.CreatedAt.Equal(first.Note.CreatedAt) {
		t.Fatalf("created_at changed: %v -> %v", first.Note.CreatedAt, second.Note.CreatedAt)
	}

	third := fixture.note(t, "second body", "architecture/auth", "", "")
	if !third.Already {
		t.Fatal("identical body was not reported unchanged")
	}
	if third.Note.Occurrences != 2 {
		t.Fatalf("idempotent write changed occurrences: %d", third.Note.Occurrences)
	}
	records, err := fixture.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "demo"})
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want one topic record", len(records))
	}
	if records[0].Occurrences != 2 || records[0].TopicKey != "architecture/auth" {
		t.Fatalf("summary = %+v", records[0])
	}
}

func TestTopicUpsertDecisionAttemptHandoff(t *testing.T) {
	fixture := newEngramFixture(t)
	ctx := context.Background()
	if _, err := fixture.app.Decision(ctx, app.DecisionInput{
		ProjectID: "demo", CreateProject: true, Title: "Storage", Status: domain.DecisionAccepted,
		Context: "first context", Topic: "architecture/storage", Subtype: "architecture",
	}); err != nil {
		t.Fatalf("Decision: %v", err)
	}
	second, err := fixture.app.Decision(ctx, app.DecisionInput{
		ProjectID: "demo", Title: "Storage", Status: domain.DecisionAccepted,
		Context: "second context", Topic: "architecture/storage",
	})
	if err != nil {
		t.Fatalf("Decision update: %v", err)
	}
	if second.Decision.Occurrences != 2 || second.Decision.Context != "second context" {
		t.Fatalf("decision = %+v", second.Decision)
	}
	if len(second.Decision.History) != 1 || second.Decision.History[0].Body == "" {
		t.Fatalf("decision history = %+v", second.Decision.History)
	}

	if _, err := fixture.app.Attempt(ctx, app.AttemptInput{
		ProjectID: "demo", Title: "Cache", Hypothesis: "warm", Result: "same", Reason: "warm",
		Topic: "bug/cache",
	}); err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	attempt, err := fixture.app.Attempt(ctx, app.AttemptInput{
		ProjectID: "demo", Title: "Cache", Hypothesis: "cold", Result: "slower", Reason: "cold",
		Topic: "bug/cache",
	})
	if err != nil {
		t.Fatalf("Attempt update: %v", err)
	}
	if attempt.Attempt.Occurrences != 2 || attempt.Attempt.Hypothesis != "cold" {
		t.Fatalf("attempt = %+v", attempt.Attempt)
	}

	if _, err := fixture.app.Handoff(ctx, app.HandoffInput{
		ProjectID: "demo", Summary: "first handoff", NextAction: "next", Topic: "session/summary",
	}); err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	handoff, err := fixture.app.Handoff(ctx, app.HandoffInput{
		ProjectID: "demo", Summary: "second handoff", NextAction: "next", Topic: "session/summary",
	})
	if err != nil {
		t.Fatalf("Handoff update: %v", err)
	}
	if handoff.Handoff.Occurrences != 2 || handoff.Handoff.Summary != "second handoff" {
		t.Fatalf("handoff = %+v", handoff.Handoff)
	}
	if len(handoff.Handoff.History) != 1 {
		t.Fatalf("handoff history = %+v", handoff.Handoff.History)
	}
}

func TestTopicHistoryIsBounded(t *testing.T) {
	fixture := newEngramFixture(t)
	for i := 0; i < domain.MaxTopicHistoryEntries+5; i++ {
		fixture.note(t, "body "+time.Duration(i).String(), "topic/bound", "discovery", domain.ScopeProject)
	}
	records, err := fixture.app.ListRecords(context.Background(), app.ListRecordsOptions{ProjectID: "demo"})
	if err != nil || len(records) != 1 {
		t.Fatalf("ListRecords = %d, %v", len(records), err)
	}
	record, err := fixture.app.GetRecord(context.Background(), "demo", records[0].ID)
	if err != nil || record.Note == nil {
		t.Fatalf("GetRecord: %+v, %v", record, err)
	}
	if len(record.Note.History) != domain.MaxTopicHistoryEntries {
		t.Fatalf("history = %d entries, want %d", len(record.Note.History), domain.MaxTopicHistoryEntries)
	}
	if record.Note.Occurrences != domain.MaxTopicHistoryEntries+5 {
		t.Fatalf("occurrences = %d", record.Note.Occurrences)
	}
	// Newest first: the last replaced body is body 23ns, the previous 22ns.
	if record.Note.History[0].Body != "body 23ns" || record.Note.History[1].Body != "body 22ns" {
		t.Fatalf("history order = %q, %q", record.Note.History[0].Body, record.Note.History[1].Body)
	}
}

func TestConcurrentTopicUpsertsCreateOneRecord(t *testing.T) {
	fixture := newEngramFixture(t)
	if _, err := fixture.app.Note(context.Background(), app.NoteInput{
		ProjectID: "demo", CreateProject: true, Title: "Seed", Body: "seed", Topic: "architecture/race",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var wait sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := fixture.app.Note(context.Background(), app.NoteInput{
				ProjectID: "demo", Title: "Race", Body: "writer " + time.Duration(index).String(), Topic: "architecture/race",
			})
			errs <- err
		}(i)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent upsert: %v", err)
		}
	}
	records, err := fixture.app.ListRecords(context.Background(), app.ListRecordsOptions{ProjectID: "demo"})
	if err != nil || len(records) != 1 {
		t.Fatalf("ListRecords = %d, %v", len(records), err)
	}
	if records[0].Occurrences != 9 {
		t.Fatalf("occurrences = %d, want 9", records[0].Occurrences)
	}
}

func TestClassificationFilters(t *testing.T) {
	fixture := newEngramFixture(t)
	ctx := context.Background()
	first := fixture.note(t, "authentication model alpha", "architecture/auth", "architecture", domain.ScopeProject)
	fixture.note(t, "authentication model beta", "bug/auth", "bugfix", domain.ScopePersonal)
	fixture.note(t, "authentication model gamma", "", "discovery", domain.ScopeGlobal)

	records, err := fixture.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "demo", Subtype: "bugfix"})
	if err != nil || len(records) != 1 || records[0].ID == first.Note.ID {
		t.Fatalf("subtype filter = %+v, %v", records, err)
	}
	records, err = fixture.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "demo", Scope: "personal"})
	if err != nil || len(records) != 1 || records[0].Subtype != "bugfix" {
		t.Fatalf("scope filter = %+v, %v", records, err)
	}
	records, err = fixture.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "demo", TopicKey: "architecture/auth"})
	if err != nil || len(records) != 1 || records[0].ID != first.Note.ID {
		t.Fatalf("topic filter = %+v, %v", records, err)
	}
	if _, err := fixture.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "demo", Scope: "team"}); err == nil {
		t.Fatal("unknown scope filter accepted")
	}

	hits, err := fixture.app.SearchWith(ctx, app.SearchOptions{Text: "authentication", ProjectID: "demo", Subtype: "architecture"})
	if err != nil || len(hits) != 1 || hits[0].ID != first.Note.ID {
		t.Fatalf("search subtype = %+v, %v", hits, err)
	}
	hits, err = fixture.app.SearchWith(ctx, app.SearchOptions{Text: "authentication", ProjectID: "demo", Scope: "personal"})
	if err != nil || len(hits) != 1 || hits[0].Subtype != "bugfix" {
		t.Fatalf("search scope = %+v, %v", hits, err)
	}
	hits, err = fixture.app.SearchWith(ctx, app.SearchOptions{Text: "authentication", ProjectID: "demo", TopicKey: "architecture/auth"})
	if err != nil || len(hits) != 1 || hits[0].ID != first.Note.ID {
		t.Fatalf("search topic = %+v, %v", hits, err)
	}
	all, err := fixture.app.SearchWith(ctx, app.SearchOptions{Text: "authentication", ProjectID: "demo"})
	if err != nil || len(all) != 3 {
		t.Fatalf("default search = %d, %v", len(all), err)
	}
}

func TestContextFiltersByClassification(t *testing.T) {
	fixture := newEngramFixture(t)
	ctx := context.Background()
	if _, err := fixture.app.Note(ctx, app.NoteInput{
		ProjectID: "demo", CreateProject: true, Title: "Architecture", Body: "authentication token architecture",
		Subtype: "architecture", Topic: "architecture/auth",
	}); err != nil {
		t.Fatalf("Note architecture: %v", err)
	}
	bug, err := fixture.app.Note(ctx, app.NoteInput{
		ProjectID: "demo", Title: "Bugfix", Body: "authentication token bug",
		Subtype: "bugfix", Topic: "bug/auth",
	})
	if err != nil {
		t.Fatalf("Note bugfix: %v", err)
	}
	result, err := fixture.app.Context(ctx, app.ContextOptions{
		ProjectID: "demo", Query: "authentication token", Subtype: "bugfix",
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if len(result.Records) != 1 || result.Records[0].ID != bug.Note.ID {
		t.Fatalf("filtered context records = %+v", result.Records)
	}
}

func TestDecisionStatusChangeKeepsTopicHistory(t *testing.T) {
	fixture := newEngramFixture(t)
	ctx := context.Background()
	if _, err := fixture.app.Decision(ctx, app.DecisionInput{
		ProjectID: "demo", CreateProject: true, Title: "Auth", Status: domain.DecisionProposed,
		Context: "first context", Paths: []string{"docs/design.md"}, Topic: "architecture/auth",
	}); err != nil {
		t.Fatalf("first decision: %v", err)
	}
	second, err := fixture.app.Decision(ctx, app.DecisionInput{
		ProjectID: "demo", Title: "Auth", Status: domain.DecisionProposed,
		Context: "second context", Topic: "architecture/auth",
	})
	if err != nil {
		t.Fatalf("second decision: %v", err)
	}
	if len(second.Decision.History) != 1 {
		t.Fatalf("history before status change = %+v", second.Decision.History)
	}
	if _, err := fixture.app.SetDecisionStatus(ctx, "demo", second.Decision.ID, domain.DecisionAccepted); err != nil {
		t.Fatalf("SetDecisionStatus: %v", err)
	}
	record, err := fixture.app.GetRecord(ctx, "demo", second.Decision.ID)
	if err != nil || record.Decision == nil {
		t.Fatalf("GetRecord: %+v, %v", record, err)
	}
	if len(record.Decision.History) != 1 {
		t.Fatalf("status change rewrote history: %+v", record.Decision.History)
	}
	if strings.Contains(record.Decision.Context, "## History") {
		t.Fatalf("status change folded history into context: %q", record.Decision.Context)
	}
	if len(record.Decision.Paths) != 1 || record.Decision.Paths[0] != "docs/design.md" {
		t.Fatalf("status change dropped paths: %+v", record.Decision.Paths)
	}
	if record.Decision.Status != domain.DecisionAccepted {
		t.Fatalf("status = %q", record.Decision.Status)
	}
	// An upsert must not downgrade an accepted decision even when the CLI
	// default status is proposed.
	third, err := fixture.app.Decision(ctx, app.DecisionInput{
		ProjectID: "demo", Title: "Auth", Status: domain.DecisionProposed,
		Context: "third context", Topic: "architecture/auth",
	})
	if err != nil {
		t.Fatalf("third decision: %v", err)
	}
	if third.Decision.Status != domain.DecisionAccepted {
		t.Fatalf("topic upsert downgraded status to %q", third.Decision.Status)
	}
}
