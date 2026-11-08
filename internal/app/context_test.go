package app_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// contextIndex is an in-memory index: a hit is a document of an allowed kind
// whose title or body contains the query, ranked by the number of matches.
// duplicate returns every hit twice, like an index with stale rows.
type contextIndex struct {
	docs      map[string]ports.Document
	duplicate bool
	queries   []ports.Query
}

func newContextIndex() *contextIndex { return &contextIndex{docs: map[string]ports.Document{}} }

func (c *contextIndex) Upsert(_ context.Context, doc ports.Document) error {
	c.docs[doc.ID] = doc
	return nil
}

func (c *contextIndex) Delete(_ context.Context, id string) error {
	delete(c.docs, id)
	return nil
}

func (c *contextIndex) Search(_ context.Context, query ports.Query) ([]ports.Hit, error) {
	c.queries = append(c.queries, query)
	needle := strings.ToLower(query.Text)
	type scored struct {
		hit   ports.Hit
		count int
	}
	var found []scored
	for key, doc := range c.docs {
		count := strings.Count(strings.ToLower(doc.Title+" "+doc.Body), needle)
		if count == 0 || !query.AllowsKind(doc.Kind) || (query.ProjectID != "" && doc.ProjectID != query.ProjectID) {
			continue
		}
		_, id, _ := strings.Cut(key, "/")
		found = append(found, scored{ports.Hit{ID: id, ProjectID: doc.ProjectID, Kind: doc.Kind, Title: doc.Title, Snippet: doc.Body}, count})
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].count != found[j].count {
			return found[i].count > found[j].count
		}
		return found[i].hit.ID < found[j].hit.ID
	})
	hits := make([]ports.Hit, 0, len(found))
	for _, item := range found {
		hits = append(hits, item.hit)
		if c.duplicate {
			hits = append(hits, item.hit)
		}
	}
	return hits, nil
}

type contextFixture struct {
	app   *app.App
	store ports.Store
	index *contextIndex
	clock *time.Time
}

func newContextFixture(t *testing.T, withIndex bool) contextFixture {
	t.Helper()
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	deps := app.Dependencies{Store: store, Now: func() time.Time { return clock }}
	index := newContextIndex()
	if withIndex {
		deps.Index = index
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	return contextFixture{app: app.New(cfg, deps), store: store, index: index, clock: &clock}
}

func (f contextFixture) advance(d time.Duration) { *f.clock = f.clock.Add(d) }

func (f contextFixture) decision(t *testing.T, title, contextText string) string {
	t.Helper()
	f.advance(time.Minute)
	result, err := f.app.Decision(context.Background(), app.DecisionInput{ProjectID: "p", Title: title, Context: contextText, CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	return result.Decision.ID
}

func (f contextFixture) note(t *testing.T, body string) string {
	t.Helper()
	f.advance(time.Minute)
	result, err := f.app.Note(context.Background(), app.NoteInput{ProjectID: "p", Body: body, CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	return result.Note.ID
}

func (f contextFixture) attempt(t *testing.T, title, reason string) string {
	t.Helper()
	f.advance(time.Minute)
	result, err := f.app.Attempt(context.Background(), app.AttemptInput{ProjectID: "p", Title: title, Reason: reason, CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	return result.Attempt.ID
}

func ids(records []app.ContextRecord) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, record.ID)
	}
	return out
}

func TestContextRanksRelevantRecordsFirst(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newContextFixture(t, true)
	best := f.decision(t, "Cache layer in SQLite", "the cache keeps cache entries for an hour")
	weaker := f.note(t, "cache warmup idea")
	unrelated := f.note(t, "release checklist for the installer")
	failed := f.attempt(t, "Redis cache", "too slow on Windows")
	_ = f.index.Upsert(ctx, ports.Document{ID: "dump:mail/1.md", ProjectID: "p", Kind: app.KindDump, Title: "cache cache cache", Body: "cache"})

	// Act
	result, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "cache"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	got := ids(result.Records)
	if len(got) == 0 || got[0] != best {
		t.Fatalf("records = %v, want %s first", got, best)
	}
	for _, id := range got {
		if id == unrelated || strings.HasPrefix(id, "dump:") {
			t.Fatalf("records = %v include %s", got, id)
		}
	}
	if !strings.Contains(strings.Join(got, " "), weaker) || !strings.Contains(strings.Join(got, " "), failed) {
		t.Fatalf("records = %v, want %s and %s", got, weaker, failed)
	}
	if !strings.Contains(result.Summary, "Cache layer in SQLite") || !strings.Contains(result.Summary, "## Relevant records") {
		t.Fatalf("summary:\n%s", result.Summary)
	}
	for _, query := range f.index.queries {
		if len(query.Kinds) == 0 || query.ProjectID != "p" {
			t.Fatalf("search query = %+v, want project and kinds set", query)
		}
	}
}

func TestContextDedupsRecordsAcrossSections(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newContextFixture(t, true)
	f.index.duplicate = true
	f.decision(t, "Retry sync push", "retry three times")
	f.attempt(t, "Retry forever", "retry loops hung the hook")

	// Act
	result, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "retry"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, id := range append(ids(result.Records), ids(result.FailedAttempts)...) {
		if seen[id] {
			t.Fatalf("record %s listed twice: records=%v failed=%v", id, ids(result.Records), ids(result.FailedAttempts))
		}
		seen[id] = true
	}
	if len(seen) != 2 {
		t.Fatalf("records = %v, want both records once", seen)
	}
}

func TestContextKindsFilterKeepsFailedAttempts(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newContextFixture(t, true)
	decision := f.decision(t, "Index with FTS5", "index rebuilds are cheap")
	f.note(t, "index note")
	failed := f.attempt(t, "Index with bleve", "index too large")

	// Act
	result, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "index", Kinds: []string{"Decision"}})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(result.Records); len(got) != 1 || got[0] != decision {
		t.Fatalf("records = %v, want only %s", got, decision)
	}
	if got := ids(result.FailedAttempts); len(got) != 1 || got[0] != failed {
		t.Fatalf("failed attempts = %v, want %s", got, failed)
	}
}

func TestContextSinceDropsOlderRecords(t *testing.T) {
	for _, since := range []string{"3d", "2026-11-05"} {
		t.Run(since, func(t *testing.T) {
			// Arrange
			ctx := context.Background()
			f := newContextFixture(t, true)
			old := f.decision(t, "Deploy with rsync", "deploy by hand")
			if _, err := f.app.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "old deploy work"}); err != nil {
				t.Fatal(err)
			}
			f.advance(10 * 24 * time.Hour)
			recent := f.decision(t, "Deploy with the release workflow", "deploy from CI")

			// Act
			result, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "deploy", Since: since})

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(result.Records); len(got) != 1 || got[0] != recent {
				t.Fatalf("records = %v, want only %s (not %s)", got, recent, old)
			}
			if result.Handoff != nil || result.Since.IsZero() {
				t.Fatalf("handoff = %+v since = %v, want the old handoff dropped", result.Handoff, result.Since)
			}
		})
	}
}

func TestContextBudgetTruncatesSummary(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newContextFixture(t, true)
	for i := 0; i < 6; i++ {
		f.decision(t, "Budget decision with a long title", strings.Repeat("budget context ", 20))
	}

	// Act
	result, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "budget", Budget: 300})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Summary, "(truncated)") || len([]rune(result.Summary)) > 300 {
		t.Fatalf("summary (%d runes) not truncated to 300:\n%s", len([]rune(result.Summary)), result.Summary)
	}
	if result.Budget != 300 || len(result.Records) == 0 {
		t.Fatalf("budget = %d records = %d", result.Budget, len(result.Records))
	}
}

func TestContextWithoutStateHandoffOrMatches(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newContextFixture(t, true)
	f.note(t, "unrelated text")

	// Act
	result, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "kubernetes"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if result.Handoff != nil || len(result.Records) != 0 || len(result.FailedAttempts) != 0 || len(result.Skipped) != 0 {
		t.Fatalf("result = %+v", result)
	}
	for _, want := range []string{"goal: not set", "## Latest handoff\n- none recorded", "- none matched the task", "task: kubernetes"} {
		if !strings.Contains(result.Summary, want) {
			t.Fatalf("summary lacks %q:\n%s", want, result.Summary)
		}
	}
}

func TestContextIncludesStateHandoffAndFindings(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newContextFixture(t, true)
	f.note(t, "seed")
	next := "write the parser"
	if _, err := f.app.SetState(ctx, app.StateInput{ProjectID: "p", NextAction: &next}); err != nil {
		t.Fatal(err)
	}
	f.advance(time.Minute)
	if _, err := f.app.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "parser done", NextAction: "ship the release"}); err != nil {
		t.Fatal(err)
	}

	// Act
	result, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "parser"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if result.Handoff == nil || result.Handoff.NextAction != "ship the release" {
		t.Fatalf("handoff = %+v", result.Handoff)
	}
	if len(result.Findings) == 0 || result.Findings[0].Kind != app.FindingNextBehindHandoff {
		t.Fatalf("findings = %+v", result.Findings)
	}
	for _, want := range []string{"next_action: write the parser", "[next-behind-handoff]", "next: ship the release"} {
		if !strings.Contains(result.Summary, want) {
			t.Fatalf("summary lacks %q:\n%s", want, result.Summary)
		}
	}
}

func TestContextDropsHitsOfRemovedRecords(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newContextFixture(t, true)
	kept := f.decision(t, "Logging with slog", "logging is structured")
	_ = f.index.Upsert(ctx, ports.Document{ID: "p/decision-p-ghost", ProjectID: "p", Kind: "decision", Title: "logging logging logging"})

	// Act
	result, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "logging"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(result.Records); len(got) != 1 || got[0] != kept {
		t.Fatalf("records = %v, want only %s", got, kept)
	}
}

func TestContextSkipsSearchWhenUnavailable(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newContextFixture(t, false)
	f.decision(t, "Cache layer", "cache")

	// Act
	result, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "cache"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 1 || !strings.Contains(result.Skipped[0], "relevant records") {
		t.Fatalf("skipped = %v", result.Skipped)
	}
	if !strings.Contains(result.Summary, "skipped: relevant records") {
		t.Fatalf("summary:\n%s", result.Summary)
	}
}

func TestContextServerModeSearchesThroughSearcher(t *testing.T) {
	// Arrange: the file store stands in for the server client.
	ctx := context.Background()
	local := newContextFixture(t, false)
	id := local.decision(t, "Tokens expire after 30 days", "token lifetime")
	searcher := &fakeSearcher{hits: []ports.Hit{{ID: id, ProjectID: "p", Kind: "decision", Title: "Tokens expire after 30 days"}}}
	cfg := config.Default()
	cfg.Mode = config.ModeServer
	cfg.ServerURL = "https://brain.example.test"
	server := app.New(cfg, app.Dependencies{Store: local.store, Searcher: searcher, Now: func() time.Time { return *local.clock }})

	// Act
	result, err := server.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "token"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if searcher.calls != 2 || searcher.query.ProjectID != "p" {
		t.Fatalf("searcher calls = %d query = %+v", searcher.calls, searcher.query)
	}
	if got := ids(result.Records); len(got) != 1 || got[0] != id || result.Mode != config.ModeServer {
		t.Fatalf("records = %v mode = %s", got, result.Mode)
	}
}

func TestContextValidatesInput(t *testing.T) {
	f := newContextFixture(t, true)
	f.note(t, "seed")
	for name, opts := range map[string]app.ContextOptions{
		"empty query":  {ProjectID: "p", Query: "  "},
		"unknown kind": {ProjectID: "p", Query: "x", Kinds: []string{"handoff"}},
		"bad since":    {ProjectID: "p", Query: "x", Since: "yesterday"},
	} {
		t.Run(name, func(t *testing.T) {
			// Act
			_, err := f.app.Context(context.Background(), opts)

			// Assert
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("error = %v, want validation", err)
			}
		})
	}
}
