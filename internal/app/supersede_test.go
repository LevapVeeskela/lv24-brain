package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/journal"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// validityFixture is a local brain with a real SQLite projection, an activity
// journal (the supersede case store) and a clock the test controls, so validity
// intervals and review cases are deterministic.
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
	a := app.New(cfg, app.Dependencies{Store: store, Index: index, Journal: journal.New(dataDir), Now: func() time.Time { return clock }})
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
	if result.Old.SupersededBy != "" || !result.Old.ValidTo.IsZero() {
		t.Fatalf("the predecessor must stay valid until approval: %+v", result.Old)
	}
	records, err := f.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	wantsIDs(t, "current records before approval", listedIDs(records), attempt.Attempt.ID, note.Note.ID)
	if result.Case == nil || result.Case.Status != app.SupersedeProposed {
		t.Fatalf("case = %+v, want a proposed case", result.Case)
	}
	if _, err := f.app.ReviewSupersede(ctx, app.SupersedeReviewInput{
		ProjectID: "p", OldID: note.Note.ID, Action: app.SupersedeAccept,
	}); err != nil {
		t.Fatal(err)
	}
	approved, err := f.app.GetRecord(ctx, "p", note.Note.ID)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Validity().SupersededBy != attempt.Attempt.ID {
		t.Fatalf("approved links = %+v", approved.Validity())
	}
	records, err = f.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p"})
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
		t.Fatalf("second claimant error = %v, want ErrValidation", err)
	}
	if _, err := f.app.ReviewSupersede(ctx, app.SupersedeReviewInput{
		ProjectID: "p", OldID: first, Action: app.SupersedeReject,
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("rejection without a reason error = %v, want ErrValidation", err)
	}
	if _, err := f.app.ReviewSupersede(ctx, app.SupersedeReviewInput{
		ProjectID: "p", OldID: first, Action: app.SupersedeAccept,
	}); err != nil {
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
	if _, err := f.app.ReviewSupersede(ctx, app.SupersedeReviewInput{
		ProjectID: "p", OldID: second.Handoff.ID, Action: app.SupersedeAccept,
	}); err != nil {
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

// TestSupersedePredecessorVisibleUntilApproval is LIFE-1: a supersede writes a
// proposed case, the predecessor stays valid and searchable, and only the
// approval hides it.
func TestSupersedePredecessorVisibleUntilApproval(t *testing.T) {
	ctx := context.Background()
	f := newValidityFixture(t)
	old := f.decision(t, "Storage: use JSON files", "file storage decision", "")
	f.tick(time.Hour)
	replacement := f.decision(t, "Storage: use SQLite", "sqlite storage decision", "")

	result, err := f.app.SupersedeWithReason(ctx, "p", old, replacement, "sqlite removes the rewrite loop")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Case == nil {
		t.Fatalf("staged supersede = %+v, want a proposed case", result)
	}
	if result.Case.Status != app.SupersedeProposed || result.Case.OldID != old ||
		result.Case.NewID != replacement || result.Case.Reason != "sqlite removes the rewrite loop" {
		t.Fatalf("case = %+v", result.Case)
	}
	if result.Old.SupersededBy != "" || !result.Old.ValidTo.IsZero() {
		t.Fatalf("predecessor ended before approval: %+v", result.Old)
	}
	if result.New.Supersedes != old {
		t.Fatalf("successor does not name the predecessor: %+v", result.New)
	}

	// Before approval: the predecessor is current and searchable.
	listed, err := f.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	wantsIDs(t, "records before approval", listedIDs(listed), replacement, old)
	hits, err := f.app.SearchWith(ctx, app.SearchOptions{Text: "json", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != old {
		t.Fatalf("predecessor not searchable before approval: %+v", hits)
	}

	// The case is what the review queue owes the owner.
	cases, err := f.app.SupersedeCases(ctx, "p", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].Status != app.SupersedeProposed {
		t.Fatalf("pending cases = %+v", cases)
	}

	approved, err := f.app.ReviewSupersede(ctx, app.SupersedeReviewInput{
		ProjectID: "p", OldID: old, NewID: replacement, Action: app.SupersedeAccept, Reason: "owner approved",
	})
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != app.SupersedeAccepted || approved.ExpectedHash == "" {
		t.Fatalf("accepted case = %+v", approved)
	}
	ended, err := f.app.GetRecord(ctx, "p", old)
	if err != nil {
		t.Fatal(err)
	}
	if ended.Validity().SupersededBy != replacement || ended.Validity().ValidTo.IsZero() {
		t.Fatalf("predecessor after approval = %+v", ended.Validity())
	}
	if ended.Decision == nil || ended.Decision.Status != domain.DecisionSuperseded {
		t.Fatalf("decision status after approval = %+v", ended.Decision)
	}

	// After approval: hidden by default, still queryable as history.
	listed, err = f.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	wantsIDs(t, "records after approval", listedIDs(listed), replacement)
	hits, err = f.app.SearchWith(ctx, app.SearchOptions{Text: "json", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("superseded predecessor still in default search: %+v", hits)
	}
	hits, err = f.app.SearchWith(ctx, app.SearchOptions{Text: "json", ProjectID: "p", IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != old {
		t.Fatalf("archive search = %+v, want the predecessor", hits)
	}
	archived, err := f.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p", IncludeInvalidated: true})
	if err != nil {
		t.Fatal(err)
	}
	wantsIDs(t, "include_invalidated records", listedIDs(archived), replacement, old)

	archive, err := f.app.Archive(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(archive) != 1 || archive[0].ID != old || archive[0].SupersededBy != replacement ||
		archive[0].Reason != "sqlite removes the rewrite loop" || archive[0].InvalidatedAt.IsZero() {
		t.Fatalf("archive = %+v", archive)
	}
}

// TestSupersedeRejectionLeavesPredecessor is LIFE-1 for the refusal: nothing is
// restored because nothing was changed, and the case keeps the written reason.
func TestSupersedeRejectionLeavesPredecessor(t *testing.T) {
	ctx := context.Background()
	f := newValidityFixture(t)
	old := f.decision(t, "Release: ship Friday", "release decision", "")
	f.tick(time.Hour)
	replacement := f.decision(t, "Release: ship Monday", "release decision", "")
	before, err := f.app.GetRecord(ctx, "p", old)
	if err != nil {
		t.Fatal(err)
	}
	beforeValidity := before.Validity()

	if _, err := f.app.SupersedeWithReason(ctx, "p", old, replacement, "monday is safer"); err != nil {
		t.Fatal(err)
	}
	c, err := f.app.ReviewSupersede(ctx, app.SupersedeReviewInput{
		ProjectID: "p", OldID: old, Action: app.SupersedeReject, Reason: "friday still works",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != app.SupersedeRejected || c.DecisionNote != "friday still works" {
		t.Fatalf("rejected case = %+v", c)
	}
	after, err := f.app.GetRecord(ctx, "p", old)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Validity().ValidFrom.Equal(beforeValidity.ValidFrom) ||
		!after.Validity().ValidTo.Equal(beforeValidity.ValidTo) ||
		after.Validity().SupersededBy != beforeValidity.SupersededBy ||
		after.Decision.Status != before.Decision.Status {
		t.Fatalf("rejection changed the predecessor: %+v", after.Validity())
	}
	listed, err := f.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	wantsIDs(t, "records after rejection", listedIDs(listed), replacement, old)
	archive, err := f.app.Archive(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(archive) != 0 {
		t.Fatalf("a rejected case must not reach the archive: %+v", archive)
	}
	cases, err := f.app.SupersedeCases(ctx, "p", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].Status != app.SupersedeRejected || cases[0].DecisionNote != "friday still works" {
		t.Fatalf("case history = %+v", cases)
	}
}

// TestSupersedeRollbackRestoresExactTuple is LIFE-1 rollback: the previous
// valid_from/valid_to/superseded_by tuple comes back field for field.
func TestSupersedeRollbackRestoresExactTuple(t *testing.T) {
	ctx := context.Background()
	f := newValidityFixture(t)
	old := f.decision(t, "Cache: use Redis", "cache decision", "")
	f.tick(time.Hour)
	replacement := f.decision(t, "Cache: use in-memory", "cache decision", "")
	before, err := f.app.GetRecord(ctx, "p", old)
	if err != nil {
		t.Fatal(err)
	}
	previous := before.Validity()

	if _, err := f.app.Supersede(ctx, "p", old, replacement); err != nil {
		t.Fatal(err)
	}
	approved, err := f.app.ReviewSupersede(ctx, app.SupersedeReviewInput{
		ProjectID: "p", OldID: old, Action: app.SupersedeAccept,
	})
	if err != nil {
		t.Fatal(err)
	}
	if approved.Previous.ValidFrom.IsZero() || approved.Applied.ValidTo.IsZero() {
		t.Fatalf("the case did not record both tuples: %+v", approved)
	}
	applied, err := f.app.GetRecord(ctx, "p", old)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Validity().SupersededBy != replacement || applied.Validity().ValidTo.IsZero() {
		t.Fatalf("approval did not end the predecessor: %+v", applied.Validity())
	}

	rolled, err := f.app.ReviewSupersede(ctx, app.SupersedeReviewInput{
		ProjectID: "p", OldID: old, Action: app.SupersedeRollback, Reason: "false alarm",
		ExpectedHash: approved.ExpectedHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Status != app.SupersedeRolledBack {
		t.Fatalf("rolled back case = %+v", rolled)
	}
	restored, err := f.app.GetRecord(ctx, "p", old)
	if err != nil {
		t.Fatal(err)
	}
	got := restored.Validity()
	if !got.ValidFrom.Equal(previous.ValidFrom) || !got.ValidTo.Equal(previous.ValidTo) ||
		got.SupersededBy != previous.SupersededBy {
		t.Fatalf("restored tuple = %+v, want %+v", got, previous)
	}
	if !got.ValidTo.IsZero() {
		t.Fatalf("the restore kept a valid_to: %+v", got)
	}
	if restored.Decision == nil || restored.Decision.Status != before.Decision.Status {
		t.Fatalf("status not restored: %+v", restored.Decision)
	}
	successor, err := f.app.GetRecord(ctx, "p", replacement)
	if err != nil {
		t.Fatal(err)
	}
	if successor.Validity().Supersedes != "" {
		t.Fatalf("the successor still claims the restored record: %+v", successor.Validity())
	}
	archive, err := f.app.Archive(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(archive) != 0 {
		t.Fatalf("a rolled back supersede must leave the archive: %+v", archive)
	}
}

// TestSupersedeRollbackRefusesStaleRecord is the expected-hash guard: a record
// edited after the approval is never restored over.
func TestSupersedeRollbackRefusesStaleRecord(t *testing.T) {
	ctx := context.Background()
	f := newValidityFixture(t)
	old := f.decision(t, "Retries: three attempts", "retry decision", "")
	f.tick(time.Hour)
	replacement := f.decision(t, "Retries: five attempts", "retry decision", "")
	if _, err := f.app.Supersede(ctx, "p", old, replacement); err != nil {
		t.Fatal(err)
	}
	approved, err := f.app.ReviewSupersede(ctx, app.SupersedeReviewInput{
		ProjectID: "p", OldID: old, Action: app.SupersedeAccept,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(time.Minute)
	contextText := "retry decision, edited after approval"
	if _, err := f.app.EditRecord(ctx, app.RecordEdit{ProjectID: "p", ID: old, Body: &contextText}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.ReviewSupersede(ctx, app.SupersedeReviewInput{
		ProjectID: "p", OldID: old, Action: app.SupersedeRollback,
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale rollback error = %v, want ErrConflict", err)
	}
	unchanged, err := f.app.GetRecord(ctx, "p", old)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Validity().SupersededBy != replacement || unchanged.Validity().ValidTo.IsZero() {
		t.Fatalf("a refused rollback wrote the predecessor: %+v", unchanged.Validity())
	}
	if _, err := f.app.ReviewSupersede(ctx, app.SupersedeReviewInput{
		ProjectID: "p", OldID: old, Action: app.SupersedeRollback, ExpectedHash: approved.ExpectedHash,
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("explicit stale hash error = %v, want ErrConflict", err)
	}
}

// TestArchiveHidesInvalidatedRecordUntilFlag is LIFE-2: an explicit
// invalidation keeps the file, hides it from default queries and shows it with
// the archive flag.
func TestArchiveHidesInvalidatedRecordUntilFlag(t *testing.T) {
	ctx := context.Background()
	f := newValidityFixture(t)
	if _, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Seed", Body: "seed", CreateProject: true}); err != nil {
		t.Fatal(err)
	}
	obsolete, err := f.app.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Quartz rollout sketch", Body: "quartzmarker obsolete plan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.InvalidateRecord(ctx, "p", obsolete.Note.ID, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty reason error = %v, want ErrValidation", err)
	}
	result, err := f.app.InvalidateRecord(ctx, "p", obsolete.Note.ID, "the quartz plan was dropped")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Reason != "the quartz plan was dropped" || result.InvalidatedAt.IsZero() {
		t.Fatalf("invalidate = %+v", result)
	}
	if result.Record.ValidTo.IsZero() {
		t.Fatalf("invalidate did not end the record: %+v", result.Record)
	}
	again, err := f.app.InvalidateRecord(ctx, "p", obsolete.Note.ID, "the quartz plan was dropped")
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed || !again.InvalidatedAt.Equal(result.InvalidatedAt) {
		t.Fatalf("repeat invalidate = %+v, want an idempotent no-op", again)
	}
	// A same-tick end is one nanosecond after valid_from; let the clock pass
	// it so "now" is inside the ended interval.
	f.tick(time.Second)

	hits, err := f.app.SearchWith(ctx, app.SearchOptions{Text: "quartzmarker", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("invalidated note in default search: %+v", hits)
	}
	hits, err = f.app.SearchWith(ctx, app.SearchOptions{Text: "quartzmarker", ProjectID: "p", IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != obsolete.Note.ID {
		t.Fatalf("archive search = %+v, want the invalidated note", hits)
	}
	listed, err := f.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range listed {
		if record.ID == obsolete.Note.ID {
			t.Fatalf("invalidated note in the default listing: %+v", record)
		}
	}
	archived, err := f.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p", IncludeInvalidated: true})
	if err != nil {
		t.Fatal(err)
	}
	if !containsSummary(archived, obsolete.Note.ID) {
		t.Fatalf("archive listing omits the invalidated note: %+v", archived)
	}

	archive, err := f.app.Archive(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(archive) != 1 || archive[0].ID != obsolete.Note.ID || archive[0].Kind != ports.RecordNote ||
		archive[0].Reason != "the quartz plan was dropped" || !archive[0].InvalidatedAt.Equal(result.InvalidatedAt) ||
		archive[0].SupersededBy != "" {
		t.Fatalf("archive = %+v", archive)
	}
	// The file itself is untouched: the record is still readable by id.
	kept, err := f.app.GetRecord(ctx, "p", obsolete.Note.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Note == nil || !strings.Contains(kept.Note.Body, "quartzmarker obsolete plan") {
		t.Fatalf("invalidation rewrote the record: %+v", kept.Note)
	}
}

// TestDecisionStatusSettlesSupersedeCase checks the existing review action:
// accepting or rejecting the successor decision accepts or rejects its case.
func TestDecisionStatusSettlesSupersedeCase(t *testing.T) {
	ctx := context.Background()
	f := newValidityFixture(t)
	old := f.decision(t, "Limits: one writer", "writer decision", "")
	f.tick(time.Hour)
	accepted := f.decision(t, "Limits: one writer per project", "writer decision", "")
	if _, err := f.app.Supersede(ctx, "p", old, accepted); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.SetDecisionStatus(ctx, "p", accepted, domain.DecisionAccepted); err != nil {
		t.Fatal(err)
	}
	ended, err := f.app.GetRecord(ctx, "p", old)
	if err != nil {
		t.Fatal(err)
	}
	if ended.Validity().SupersededBy != accepted {
		t.Fatalf("accepting the successor did not approve the case: %+v", ended.Validity())
	}
	approved, err := f.app.SupersedeCases(ctx, "p", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(approved) != 1 || approved[0].Status != app.SupersedeAccepted ||
		approved[0].DecisionNote != "successor decision accepted" {
		t.Fatalf("case after status accept = %+v", approved)
	}

	f.tick(time.Hour)
	oldTwo := f.decision(t, "Limits: two writers", "writer decision two", "")
	f.tick(time.Hour)
	rejected := f.decision(t, "Limits: many writers", "writer decision three", "")
	if _, err := f.app.Supersede(ctx, "p", oldTwo, rejected); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.SetDecisionStatus(ctx, "p", rejected, domain.DecisionRejected); err != nil {
		t.Fatal(err)
	}
	untouched, err := f.app.GetRecord(ctx, "p", oldTwo)
	if err != nil {
		t.Fatal(err)
	}
	if untouched.Validity().SupersededBy != "" || !untouched.Validity().ValidTo.IsZero() {
		t.Fatalf("rejecting the successor changed the predecessor: %+v", untouched.Validity())
	}
	cases, err := f.app.SupersedeCases(ctx, "p", true)
	if err != nil {
		t.Fatal(err)
	}
	var rejectedCase *app.SupersedeCase
	for i := range cases {
		if cases[i].OldID == oldTwo {
			rejectedCase = &cases[i]
		}
	}
	if rejectedCase == nil || rejectedCase.Status != app.SupersedeRejected ||
		rejectedCase.DecisionNote != "successor decision rejected" {
		t.Fatalf("case after status reject = %+v", cases)
	}
}

// TestReviewReportsSupersedeCases checks that the staged cases reach the review
// report with both ends and the reason.
func TestReviewReportsSupersedeCases(t *testing.T) {
	ctx := context.Background()
	f := newValidityFixture(t)
	old := f.decision(t, "Index: one shard", "index decision", "")
	f.tick(time.Minute)
	replacement := f.decision(t, "Index: two shards", "index decision", "")
	if _, err := f.app.SupersedeWithReason(ctx, "p", old, replacement, "the second shard is read-only"); err != nil {
		t.Fatal(err)
	}
	review, err := f.app.Review(ctx, app.ReviewOptions{ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Supersede cases (1 proposed)", old, replacement, "the second shard is read-only"} {
		if !strings.Contains(review.Summary, want) {
			t.Fatalf("review summary omits %q:\n%s", want, review.Summary)
		}
	}
	found := false
	for _, item := range review.Items {
		if strings.Contains(item, old) && strings.Contains(item, replacement) {
			found = true
		}
	}
	if !found {
		t.Fatalf("review items omit the case: %+v", review.Items)
	}
}

// containsSummary reports whether a listing holds the id.
func containsSummary(records []app.RecordSummary, id string) bool {
	for _, record := range records {
		if record.ID == id {
			return true
		}
	}
	return false
}
