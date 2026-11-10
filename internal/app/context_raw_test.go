package app_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type fakeContextGrants struct {
	grants domain.ContextGrants
	err    error
}

func (f fakeContextGrants) ContextGrants(context.Context) (domain.ContextGrants, error) {
	return f.grants, f.err
}

type rawContextFixture struct {
	app    *app.App
	store  *fsstore.Store
	index  *contextIndex
	items  *fsstore.ItemStore
	grants *fakeContextGrants
	clock  time.Time
}

func newRawContextFixture(t *testing.T) *rawContextFixture {
	t.Helper()
	return newRawContextFixtureWithGrants(t, &fakeContextGrants{grants: domain.ContextGrants{
		SchemaVersion: 1,
		Grants: []domain.ContextGrant{{
			Project: "p", Connection: "tracker", Kinds: []domain.ItemKind{domain.ItemIssue},
			MaxAgeDays: 30, MaxExcerpts: 5,
		}},
	}})
}

func newRawContextFixtureWithGrants(t *testing.T, grants ports.ContextGrantStore) *rawContextFixture {
	t.Helper()
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	index := newContextIndex()
	deps := app.Dependencies{
		Store: store, Index: index, Items: store.Items(),
		ContextGrants: grants, Now: func() time.Time { return clock },
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	application := app.New(cfg, deps)
	if _, err := application.Decision(context.Background(), app.DecisionInput{
		ProjectID: "p", Title: "seed", Context: "seed", CreateProject: true,
	}); err != nil {
		t.Fatal(err)
	}
	fixture := &rawContextFixture{app: application, store: store, index: index, items: store.Items(), clock: clock}
	if fake, ok := grants.(*fakeContextGrants); ok {
		fixture.grants = fake
	}
	return fixture
}

func (f *rawContextFixture) item(connectionID, externalID, title, body string) domain.Item {
	item := rawItem(connectionID, externalID, title, body)
	item.ProjectID = "p"
	item.UpdatedAt = f.clock.Add(-time.Hour)
	item.ObservedAt = item.UpdatedAt
	return item.WithHash()
}

func (f *rawContextFixture) addItem(t *testing.T, item domain.Item) {
	t.Helper()
	item = item.WithHash()
	if _, err := f.items.Upsert(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	f.addIndexHit(t, item, item.ProjectID)
}

func (f *rawContextFixture) addIndexHit(t *testing.T, item domain.Item, documentProject string) {
	t.Helper()
	err := f.index.Upsert(context.Background(), ports.Document{
		ID: documentProject + "/item:" + item.Key(), ProjectID: documentProject,
		Kind: app.KindItem, Title: item.Title, Body: item.Body, UpdatedAt: item.UpdatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *rawContextFixture) note(t *testing.T, body string) string {
	t.Helper()
	result, err := f.app.Note(context.Background(), app.NoteInput{ProjectID: "p", Body: body, CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	note := result.Note
	if err := f.index.Upsert(context.Background(), ports.Document{
		ID: "p/" + note.ID, ProjectID: "p", Kind: app.ContextKindNote,
		Title: note.Title, Body: note.Body, UpdatedAt: note.CreatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	return note.ID
}

func hasSkipped(skipped []string, needle string) bool {
	for _, item := range skipped {
		if strings.Contains(item, needle) {
			return true
		}
	}
	return false
}

func TestContextRawExcerptsRequireGrant(t *testing.T) {
	// Arrange
	f := newRawContextFixture(t)
	f.grants.grants = domain.ContextGrants{SchemaVersion: 1}
	f.addItem(t, f.item("tracker", "ISSUE-1", "Wombat parser", "wombat body"))

	// Act
	result, err := f.app.Context(context.Background(), app.ContextOptions{ProjectID: "p", Query: "wombat", Raw: true})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RawExcerpts) != 0 {
		t.Fatalf("excerpts = %+v, want default-deny", result.RawExcerpts)
	}
	if !hasSkipped(result.Skipped, "no grant for project") {
		t.Fatalf("skipped = %+v", result.Skipped)
	}
}

func TestContextRawExcerptsAreGrantedCitedAndRedacted(t *testing.T) {
	// Arrange
	f := newRawContextFixture(t)
	token := "ghp_" + strings.Repeat("a", 36)
	item := f.item("tracker", "ISSUE-1", "Wombat parser", "wombat parser token "+token+" \x1b[31mred\x1b[0m")
	f.addItem(t, item)

	// Act
	result, err := f.app.Context(context.Background(), app.ContextOptions{ProjectID: "p", Query: "wombat", Raw: true})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RawExcerpts) != 1 {
		t.Fatalf("excerpts = %+v", result.RawExcerpts)
	}
	excerpt := result.RawExcerpts[0]
	if excerpt.ConnectionID != "tracker" || excerpt.ProjectID != "p" || excerpt.ExternalID != "ISSUE-1" ||
		excerpt.Kind != string(domain.ItemIssue) || excerpt.ItemKey != item.Key() {
		t.Fatalf("excerpt = %+v", excerpt)
	}
	if !strings.Contains(excerpt.Citation, "tracker") || !strings.Contains(excerpt.Citation, "ISSUE-1") ||
		!strings.Contains(excerpt.Citation, item.URL) {
		t.Fatalf("citation = %q", excerpt.Citation)
	}
	if strings.Contains(excerpt.Body, "ghp_") || !strings.Contains(excerpt.Body, "[redacted]") {
		t.Fatalf("body was not redacted: %q", excerpt.Body)
	}
	if strings.ContainsRune(excerpt.Body, '\x1b') || strings.ContainsRune(result.Summary, '\x1b') {
		t.Fatalf("control character survived: %q", excerpt.Body)
	}
	for _, want := range []string{"## Raw excerpts (untrusted external text", "citation: tracker", "[issue] Wombat parser"} {
		if !strings.Contains(result.Summary, want) {
			t.Fatalf("summary lacks %q:\n%s", want, result.Summary)
		}
	}
}

func TestContextRawExcerptsRequireExactProject(t *testing.T) {
	// Arrange
	f := newRawContextFixture(t)
	item := f.item("tracker", "ISSUE-1", "Wombat parser", "wombat body")
	item.ProjectID = "other"
	item = item.WithHash()
	if _, err := f.items.Upsert(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	// A stale index row claims the item belongs to p; the item itself does not.
	f.addIndexHit(t, item, "p")

	// Act
	result, err := f.app.Context(context.Background(), app.ContextOptions{ProjectID: "p", Query: "wombat", Raw: true})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RawExcerpts) != 0 {
		t.Fatalf("cross-project excerpt leaked: %+v", result.RawExcerpts)
	}
	if !hasSkipped(result.Skipped, "no granted items matched") {
		t.Fatalf("skipped = %+v", result.Skipped)
	}
}

func TestContextRawExcerptsSkipWhenCuratedRich(t *testing.T) {
	// Arrange
	f := newRawContextFixture(t)
	for i := 0; i < 5; i++ {
		f.note(t, fmt.Sprintf("wombat note %d", i))
	}
	f.addItem(t, f.item("tracker", "ISSUE-1", "Wombat parser", "wombat body"))

	// Act
	result, err := f.app.Context(context.Background(), app.ContextOptions{ProjectID: "p", Query: "wombat", Raw: true})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) < 5 {
		t.Fatalf("curated records = %+v, fixture is not rich", result.Records)
	}
	if len(result.RawExcerpts) != 0 {
		t.Fatalf("excerpts added to rich context: %+v", result.RawExcerpts)
	}
}

func TestContextRawExcerptsRespectKindAndAge(t *testing.T) {
	// Arrange
	f := newRawContextFixture(t)
	item := f.item("tracker", "ISSUE-1", "Wombat parser", "wombat body")
	item.UpdatedAt = f.clock.Add(-48 * time.Hour)
	item.ObservedAt = item.UpdatedAt
	f.addItem(t, item)
	ctx := context.Background()

	// Act / assert: kind mismatch.
	f.grants.grants.Grants[0].Kinds = []domain.ItemKind{domain.ItemCommit}
	result, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "wombat", Raw: true})
	if err != nil || len(result.RawExcerpts) != 0 {
		t.Fatalf("kind filter: excerpts=%+v err=%v", result.RawExcerpts, err)
	}

	// Act / assert: age filter.
	f.grants.grants.Grants[0].Kinds = []domain.ItemKind{domain.ItemIssue}
	f.grants.grants.Grants[0].MaxAgeDays = 1
	result, err = f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "wombat", Raw: true})
	if err != nil || len(result.RawExcerpts) != 0 {
		t.Fatalf("age filter: excerpts=%+v err=%v", result.RawExcerpts, err)
	}

	// Act / assert: the same item passes a wider window.
	f.grants.grants.Grants[0].MaxAgeDays = 30
	result, err = f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "wombat", Raw: true})
	if err != nil || len(result.RawExcerpts) != 1 {
		t.Fatalf("granted: excerpts=%+v err=%v", result.RawExcerpts, err)
	}
}

func TestContextRawExcerptsCapAndTruncate(t *testing.T) {
	// Arrange
	f := newRawContextFixture(t)
	for i := 0; i < 8; i++ {
		f.addItem(t, f.item("tracker", fmt.Sprintf("ISSUE-%d", i), fmt.Sprintf("Wombat %d", i), strings.Repeat("x", 900)))
	}

	// Act
	result, err := f.app.Context(context.Background(), app.ContextOptions{ProjectID: "p", Query: "wombat", Raw: true})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RawExcerpts) == 0 || len(result.RawExcerpts) > domain.MaxContextExcerpts {
		t.Fatalf("excerpt count = %d", len(result.RawExcerpts))
	}
	total := 0
	for _, excerpt := range result.RawExcerpts {
		runes := len([]rune(excerpt.Body))
		if runes > domain.MaxContextExcerptRunes {
			t.Fatalf("excerpt body = %d runes", runes)
		}
		if !excerpt.Truncated {
			t.Fatal("900-rune body was not marked truncated")
		}
		total += runes
	}
	if total > domain.MaxContextExcerptTotalRunes {
		t.Fatalf("total excerpt runes = %d", total)
	}
}

func TestContextRawExcerptsRequireRawFlag(t *testing.T) {
	// Arrange
	f := newRawContextFixture(t)
	f.addItem(t, f.item("tracker", "ISSUE-1", "Wombat parser", "wombat body"))

	// Act
	result, err := f.app.Context(context.Background(), app.ContextOptions{ProjectID: "p", Query: "wombat"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RawExcerpts) != 0 {
		t.Fatalf("raw excerpts without --raw: %+v", result.RawExcerpts)
	}
}

func TestContextRawExcerptsGrantLoadError(t *testing.T) {
	// Arrange
	f := newRawContextFixture(t)
	f.grants.err = errors.New("boom")

	// Act
	_, err := f.app.Context(context.Background(), app.ContextOptions{ProjectID: "p", Query: "wombat", Raw: true})

	// Assert
	if err == nil || !strings.Contains(err.Error(), "read context grants") {
		t.Fatalf("err = %v", err)
	}
}

func TestContextRawExcerptsWithoutGrantStore(t *testing.T) {
	// Arrange
	f := newRawContextFixtureWithGrants(t, nil)
	f.addItem(t, f.item("tracker", "ISSUE-1", "Wombat parser", "wombat body"))

	// Act
	result, err := f.app.Context(context.Background(), app.ContextOptions{ProjectID: "p", Query: "wombat", Raw: true})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RawExcerpts) != 0 || !hasSkipped(result.Skipped, "not available") {
		t.Fatalf("excerpts=%+v skipped=%+v", result.RawExcerpts, result.Skipped)
	}
}
