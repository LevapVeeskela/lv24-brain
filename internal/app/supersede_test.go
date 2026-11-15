package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// validityFixture is a local brain with a real SQLite projection and a clock
// the test controls, so validity intervals are deterministic.
type validityFixture struct {
	app   *app.App
	store *fsstore.Store
	clock *time.Time
}

func newValidityFixture(t *testing.T) validityFixture {
	t.Helper()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	index, err := sqliteindex.Open(filepath.Join(dataDir, "index", "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	a := app.New(cfg, app.Dependencies{Store: store, Index: index, Now: func() time.Time { return clock }})
	t.Cleanup(func() { _ = index.Close() })
	return validityFixture{app: a, store: store, clock: &clock}
}

func (f validityFixture) tick(d time.Duration) { *f.clock = f.clock.Add(d) }

func (f validityFixture) decision(t *testing.T, title, contextText, supersedes string) string {
	t.Helper()
	result, err := f.app.Decision(context.Background(), app.DecisionInput{
		ProjectID: "p", Title: title, Context: contextText, Status: domain.DecisionAccepted,
		CreateProject: true, Supersedes: supersedes,
	})
	if err != nil {
		t.Fatalf("Decision(%q): %v", title, err)
	}
	return result.Decision.ID
}

func listedIDs(records []app.RecordSummary) []string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	return ids
}

func wantsIDs(t *testing.T, name string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
	}
}

func TestSupersedeThreeGenerations(t *testing.T) {
	ctx := context.Background()
	f := newValidityFixture(t)
	first := f.decision(t, "Storage: use JSON files", "file storage", "")
	firstAt := *f.clock
	f.tick(time.Hour)
	second := f.decision(t, "Storage: use SQLite", "sqlite storage", first)
	secondAt := *f.clock
	f.tick(time.Hour)
	third := f.decision(t, "Storage: use Postgres", "postgres storage", second)
	thirdAt := *f.clock

	current, err := f.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p", Kinds: []string{ports.RecordDecision}})
	if err != nil {
		t.Fatal(err)
	}
	wantsIDs(t, "current decisions", listedIDs(current), third)

	all, err := f.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p", IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	wantsIDs(t, "all decisions", listedIDs(all), third, second, first)

	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"at first valid_from", firstAt, first},
		{"just before second", secondAt.Add(-time.Nanosecond), first},
		{"at second valid_from", secondAt, second},
		{"just before third", thirdAt.Add(-time.Nanosecond), second},
		{"at third valid_from", thirdAt, third},
		{"after third", thirdAt.Add(24 * time.Hour), third},
		{"before first", firstAt.Add(-time.Nanosecond), ""},
	}
	for _, tc := range cases {
		records, err := f.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p", AsOf: tc.at})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tc.want == "" {
			if len(records) != 0 {
				t.Fatalf("%s: records = %v, want none", tc.name, listedIDs(records))
			}
			continue
		}
		wantsIDs(t, tc.name, listedIDs(records), tc.want)
	}

	chain, err := f.app.SupersedeChain(ctx, "p", second)
	if err != nil {
		t.Fatal(err)
	}
	wantsIDs(t, "chain", listedIDs(chain), first, second, third)

	hits, err := f.app.SearchWith(ctx, app.SearchOptions{Text: "storage", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	var searchIDs []string
	for _, hit := range hits {
		searchIDs = append(searchIDs, hit.ID)
	}
	wantsIDs(t, "default search", searchIDs, third)

	hits, err = f.app.SearchWith(ctx, app.SearchOptions{Text: "storage", ProjectID: "p", AsOf: secondAt})
	if err != nil {
		t.Fatal(err)
	}
	searchIDs = searchIDs[:0]
	for _, hit := range hits {
		searchIDs = append(searchIDs, hit.ID)
	}
	wantsIDs(t, "as-of search", searchIDs, second)

	hits, err = f.app.SearchWith(ctx, app.SearchOptions{Text: "storage", ProjectID: "p", IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("all-time search = %d hits, want 3", len(hits))
	}

	result, err := f.app.Supersede(ctx, "p", first, second)
	if err != nil || result.Changed {
		t.Fatalf("repeat supersede = %+v, %v; want idempotent no-op", result, err)
	}
	if _, err := f.app.Supersede(ctx, "p", third, first); err == nil {
		t.Fatal("supersede cycle accepted")
	}
}

func TestSupersedeAcrossKinds(t *testing.T) {
	ctx := context.Background()
	f := newValidityFixture(t)
	note, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Storage sketch", Body: "json files", CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(time.Minute)
	attempt, err := f.app.Attempt(ctx, app.AttemptInput{ProjectID: "p", Title: "Storage spike", Hypothesis: "sqlite is faster"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.app.Supersede(ctx, "p", note.Note.ID, attempt.Attempt.ID)
	if err != nil || !result.Changed {
		t.Fatalf("cross-kind supersede = %+v, %v", result, err)
	}
	if result.Old.SupersededBy != attempt.Attempt.ID || result.New.Supersedes != note.Note.ID {
		t.Fatalf("links = %+v", result)
	}
	records, err := f.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	wantsIDs(t, "current records", listedIDs(records), attempt.Attempt.ID)
}

func TestSupersedeValidation(t *testing.T) {
	ctx := context.Background()
	f := newValidityFixture(t)
	first := f.decision(t, "Storage: use JSON files", "file storage", "")
	f.tick(time.Minute)
	second := f.decision(t, "Storage: use SQLite", "sqlite storage", "")
	f.tick(time.Minute)
	third := f.decision(t, "Storage: use Postgres", "postgres storage", "")
	if _, err := f.app.Supersede(ctx, "p", first, first); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("self supersede error = %v, want ErrValidation", err)
	}
	if _, err := f.app.Supersede(ctx, "p", first, "bogus"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad reference error = %v, want ErrValidation", err)
	}
	if _, err := f.app.Supersede(ctx, "p", "decision-2026-11-01-missing", second); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown old error = %v, want ErrNotFound", err)
	}
	if _, err := f.app.Supersede(ctx, "p", first, second); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Supersede(ctx, "p", first, third); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("conflicting chain error = %v, want ErrValidation", err)
	}
	if _, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Bad link", Supersedes: "not-an-id"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad --supersedes error = %v, want ErrValidation", err)
	}
	if _, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Late replacement", Supersedes: first}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("superseding an already superseded record error = %v, want ErrValidation", err)
	}
}

func TestRepairWriteWithSupersededBy(t *testing.T) {
	ctx := context.Background()
	f := newValidityFixture(t)
	current := f.decision(t, "Rollout v2", "roll out the new path", "")
	f.tick(time.Minute)
	repaired, err := f.app.Decision(ctx, app.DecisionInput{
		ProjectID: "p", Title: "Rollout v1", Context: "legacy path",
		Status: domain.DecisionAccepted, SupersededBy: current, Confidence: 0.5,
	})
	if err != nil {
		t.Fatalf("repair write: %v", err)
	}
	if repaired.Decision.Status != domain.DecisionSuperseded || repaired.Decision.Validity.SupersededBy != current {
		t.Fatalf("repaired decision = %+v", repaired.Decision)
	}
	if !repaired.Decision.Validity.ValidTo.After(repaired.Decision.Validity.ValidFrom) {
		t.Fatalf("repair interval is empty: %+v", repaired.Decision.Validity)
	}
	records, err := f.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p", Kinds: []string{ports.RecordDecision}})
	if err != nil {
		t.Fatal(err)
	}
	wantsIDs(t, "current decisions", listedIDs(records), current)
	all, err := f.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p", IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all records = %+v", all)
	}
	for _, record := range all {
		if record.ID == repaired.Decision.ID && record.Confidence != 0.5 {
			t.Fatalf("repaired confidence = %v", record.Confidence)
		}
	}
}

func TestReadSurfacesExcludeSuperseded(t *testing.T) {
	ctx := context.Background()
	f := newValidityFixture(t)
	old := f.decision(t, "Cache: use Redis", "warm cache for retries", "")
	f.tick(time.Hour)
	newer := f.decision(t, "Cache: use in-memory", "warm cache for retries", old)

	taskContext, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "warm cache retries"})
	if err != nil {
		t.Fatal(err)
	}
	if len(taskContext.Records) != 1 || taskContext.Records[0].ID != newer {
		t.Fatalf("context records = %+v, want only %s", taskContext.Records, newer)
	}
	if strings.Contains(taskContext.Summary, "Cache: use Redis") {
		t.Fatalf("context summary leaked the superseded record:\n%s", taskContext.Summary)
	}

	historical, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "warm cache retries", AsOf: *f.clock})
	if err != nil {
		t.Fatal(err)
	}
	if len(historical.Records) != 1 || historical.Records[0].ID != newer {
		t.Fatalf("as-of now records = %+v", historical.Records)
	}

	bootstrap, err := f.app.Bootstrap(ctx, app.BootstrapOptions{ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bootstrap.Summary, "Cache: use Redis") || !strings.Contains(bootstrap.Summary, "Cache: use in-memory") {
		t.Fatalf("bootstrap summary =\n%s", bootstrap.Summary)
	}

	allTime, err := f.app.Bootstrap(ctx, app.BootstrapOptions{ProjectID: "p", IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(allTime.Summary, "Cache: use Redis") {
		t.Fatalf("--include-superseded bootstrap omitted history:\n%s", allTime.Summary)
	}

	catchup, err := f.app.Catchup(ctx, app.CatchupOptions{ProjectID: "p", Since: (*f.clock).Add(-48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	wantsIDs(t, "catchup decisions", listedIDs(mapDecisions(catchup.Decisions)), newer)
}

func mapDecisions(decisions []domain.Decision) []app.RecordSummary {
	out := make([]app.RecordSummary, 0, len(decisions))
	for i := range decisions {
		out = append(out, app.Record{Decision: &decisions[i]}.Summary())
	}
	return out
}

func TestSupersededHandoffIsNotLatest(t *testing.T) {
	ctx := context.Background()
	f := newValidityFixture(t)
	first, err := f.app.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "first session", CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(time.Hour)
	second, err := f.app.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "second session"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Supersede(ctx, "p", second.Handoff.ID, first.Handoff.ID); err != nil {
		t.Fatal(err)
	}
	// A record created and superseded in the same clock tick ends one
	// nanosecond after valid_from; let the clock pass it.
	f.tick(time.Second)
	result, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Handoff == nil || result.Handoff.ID != first.Handoff.ID {
		t.Fatalf("latest handoff = %+v, want the current one %s", result.Handoff, first.Handoff.ID)
	}
}

func TestReviewReportsSupersessionGapAndStaleRecord(t *testing.T) {
	ctx := context.Background()
	f := newValidityFixture(t)
	if _, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Seed", Body: "seed", CreateProject: true}); err != nil {
		t.Fatal(err)
	}
	f.tick(time.Minute)
	f.decision(t, "Index storage engine: SQLite full text", "fts5 index", "")
	f.tick(time.Minute)
	f.decision(t, "Index storage engine: SQLite full text search", "fts5 index again", "")

	f.tick(time.Minute)
	expiredAt := (*f.clock).Add(time.Hour)
	if _, err := f.store.AppendDecision(ctx, domain.Decision{
		ProjectID: "p", Title: "Old rollout plan", Status: domain.DecisionAccepted,
		Context: "ship friday", CreatedAt: *f.clock, UpdatedAt: *f.clock,
		Validity: domain.Validity{ValidFrom: *f.clock, ValidTo: expiredAt},
	}); err != nil {
		t.Fatal(err)
	}
	f.tick(2 * time.Hour)

	findings, err := f.app.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	kinds := kindsOf(findings)
	if kinds[app.FindingSupersessionGap] == 0 {
		t.Fatalf("no supersession-gap finding: %+v", findings)
	}
	if kinds[app.FindingStaleSuperseded] != 1 {
		t.Fatalf("stale-superseded findings = %d: %+v", kinds[app.FindingStaleSuperseded], findings)
	}
}

func TestDoctorReportsInvalidValidity(t *testing.T) {
	ctx := context.Background()
	f := newValidityFixture(t)
	if _, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Seed", Body: "seed", CreateProject: true}); err != nil {
		t.Fatal(err)
	}
	valid, err := f.store.AppendDecision(ctx, domain.Decision{
		ProjectID: "p", Title: "Valid plan", Status: domain.DecisionProposed,
		CreatedAt: *f.clock, UpdatedAt: *f.clock,
		Validity: domain.Validity{ValidFrom: *f.clock, ValidTo: (*f.clock).Add(time.Hour), SupersededBy: "decision-2026-11-01-replacement"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The replacement must exist for the link to be considered healthy.
	f.tick(time.Minute)
	if _, err := f.store.AppendDecision(ctx, domain.Decision{
		ID: "decision-2026-11-01-replacement", ProjectID: "p", Title: "Replacement", Status: domain.DecisionAccepted,
		CreatedAt: *f.clock, UpdatedAt: *f.clock, Validity: domain.Validity{ValidFrom: *f.clock},
	}); err != nil {
		t.Fatal(err)
	}
	report, err := f.app.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	check := doctorCheck(t, report, "validity")
	if check.Status == "" || check.Code != "" {
		t.Fatalf("validity check = %+v", check)
	}
	_ = valid

	if _, err := f.store.AppendAttempt(ctx, domain.Attempt{
		ProjectID: "p", Title: "Broken link", CreatedAt: *f.clock, UpdatedAt: *f.clock,
		Validity: domain.Validity{ValidFrom: *f.clock, ValidTo: (*f.clock).Add(time.Hour), SupersededBy: "attempt-2026-11-01-missing"},
	}); err != nil {
		t.Fatal(err)
	}
	report, err = f.app.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	check = doctorCheck(t, report, "validity")
	if check.Code != "invalid-validity" || !strings.Contains(check.Message, "attempt-2026-11-01-missing") {
		t.Fatalf("validity check = %+v", check)
	}
}
