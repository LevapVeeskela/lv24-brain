package app_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/textbudget"
)

// smallNoteFixture builds count notes that each match "retmarker" with a small
// body, so a generous budget packs many records and a tight one drops them.
func smallNoteFixture(t *testing.T, count int) contextFixture {
	t.Helper()
	f := newContextFixture(t, true)
	for i := 0; i < count; i++ {
		f.note(t, fmt.Sprintf("retmarker note %02d", i))
	}
	return f
}

// TestContextBudgetDroppedCountsAndNoOverflow checks the layered packer at the
// budgets the release note promises: the rendered answer never exceeds the
// budget and every layer that left records out says how many.
func TestContextBudgetDroppedCountsAndNoOverflow(t *testing.T) {
	for _, budget := range []int{200, 500, 4000} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			// Arrange
			ctx := context.Background()
			f := newPackFixture(t)

			// Act
			result, err := f.app.Context(ctx, app.ContextOptions{
				ProjectID: "p", Query: "packtoken", TokenBudget: budget,
			})

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if result.Pack.Budget != budget {
				t.Fatalf("budget = %d, want %d", result.Pack.Budget, budget)
			}
			if got := textbudget.Tokens(result.Summary); got > budget {
				t.Fatalf("summary costs %d tokens, budget %d:\n%s", got, budget, result.Summary)
			}
			if result.Pack.TotalTokens > budget {
				t.Fatalf("pack total = %d, budget %d", result.Pack.TotalTokens, budget)
			}
			for _, layer := range []string{
				app.ContextLayerQuery, app.ContextLayerDecisions, app.ContextLayerAttempts, app.ContextLayerLinked,
			} {
				dropped := 0
				for _, truncation := range result.Pack.Truncated {
					if truncation.Section == layer && truncation.ID != "core" && truncation.ID != "summary" {
						dropped++
					}
				}
				line := fmt.Sprintf("- … %d more dropped", dropped)
				if dropped > 0 && !strings.Contains(result.Summary, line) {
					t.Fatalf("%s dropped %d records but the summary lacks %q:\n%s", layer, dropped, line, result.Summary)
				}
				if dropped == 0 && strings.Contains(result.Summary, "- … 0 more dropped") {
					t.Fatalf("%s printed a zero dropped line:\n%s", layer, result.Summary)
				}
			}
			// A layer never both packs and truncates the same record.
			packed := map[string]bool{}
			for _, section := range result.Pack.Sections {
				for _, record := range section.Records {
					packed[record.ID] = true
				}
			}
			for _, truncation := range result.Pack.Truncated {
				if packed[truncation.ID] && truncation.ID != "core" && truncation.ID != "summary" {
					t.Fatalf("record %s is both packed and truncated", truncation.ID)
				}
			}
		})
	}
}

// TestContextBudgetReturnsMoreThanEightRecords guards the removed fixed record
// ceiling: a generous budget packs every matching record, not the first eight.
func TestContextBudgetReturnsMoreThanEightRecords(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := smallNoteFixture(t, 12)

	// Act
	result, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "retmarker", TokenBudget: 6000,
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) <= 8 {
		t.Fatalf("records = %d, want more than the old ceiling of 8", len(result.Records))
	}
	if strings.Contains(result.Summary, "more dropped") {
		t.Fatalf("a 6000-token answer dropped records:\n%s", result.Summary)
	}
}

// TestContextCyrillicBudgetStaysWithinBudget checks the two-characters-per-token
// estimate: a Cyrillic answer must respect the same budget as a Latin one.
func TestContextCyrillicBudgetStaysWithinBudget(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newContextFixture(t, true)
	for i := 0; i < 12; i++ {
		f.decision(t, fmt.Sprintf("Контекст решение %02d", i),
			strings.Repeat(fmt.Sprintf("контекст бюджет запись %02d ", i), 8))
	}

	// Act
	result, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "контекст", TokenBudget: 500,
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := textbudget.Tokens(result.Summary); got > 500 {
		t.Fatalf("Cyrillic summary costs %d tokens, budget 500:\n%s", got, result.Summary)
	}
	if !strings.Contains(result.Summary, "Контекст") {
		t.Fatalf("summary lacks the Cyrillic records:\n%s", result.Summary)
	}
	if len(result.Pack.Truncated) == 0 {
		t.Fatal("a 500-token budget dropped nothing; the fixture is too small to prove the cap")
	}
}

// TestContextAsOfLabelsStateAsCurrent checks that an as-of answer reports the
// records of that instant while telling the reader the state and findings are
// not historical.
func TestContextAsOfLabelsStateAsCurrent(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newContextFixture(t, true)
	old := f.decision(t, "As-of decision asofmarker", "asofmarker first context")
	f.advance(2 * time.Hour)
	current := f.decision(t, "As-of replacement asofmarker", "asofmarker second context")
	if _, err := f.app.Supersede(ctx, "p", old, current); err != nil {
		t.Fatal(err)
	}
	asOf := f.clock.Add(-time.Hour)

	// Act
	result, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "asofmarker", AsOf: asOf,
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	got := ids(result.Records)
	if len(got) != 1 || got[0] != old {
		t.Fatalf("as-of records = %v, want only %s", got, old)
	}
	for _, want := range []string{"as_of: ", "state_as_of: current"} {
		if !strings.Contains(result.Summary, want) {
			t.Fatalf("summary lacks %q:\n%s", want, result.Summary)
		}
	}
	if !result.AsOf.Equal(asOf) {
		t.Fatalf("result.AsOf = %v, want %v", result.AsOf, asOf)
	}
}

// TestContextSinceIgnoresMetadataAndTracksText is the since contract: a
// metadata-only write moves updated_at but not the content time, so it must not
// make the record recent; a body edit does.
func TestContextSinceIgnoresMetadataAndTracksText(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newContextFixture(t, true)
	old := f.decision(t, "Since old decision sincewindow", "sincewindow superseded body")
	f.advance(time.Minute)
	newer := f.decision(t, "Since new decision sincewindow", "sincewindow current body")
	window := f.clock.Add(time.Hour).Format(time.RFC3339)
	f.advance(2 * time.Hour)
	// Supersede marks validity: it rewrites the frontmatter and moves
	// updated_at, which is exactly the metadata write a since window must
	// ignore.
	if _, err := f.app.Supersede(ctx, "p", old, newer); err != nil {
		t.Fatal(err)
	}

	// Act: the metadata write is outside the window.
	metadata, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "sincewindow", Since: window})
	if err != nil {
		t.Fatal(err)
	}
	// A body edit is content and must appear.
	if _, err := f.app.EditRecord(ctx, app.RecordEdit{
		ProjectID: "p", ID: newer, Body: strPtr("sincewindow edited body"),
	}); err != nil {
		t.Fatal(err)
	}
	edited, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "sincewindow", Since: window})
	if err != nil {
		t.Fatal(err)
	}
	sinceTime := mustParseSince(t, window)
	listed, err := f.app.ListRecords(ctx, app.ListRecordsOptions{ProjectID: "p", Since: sinceTime})
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	for _, id := range ids(metadata.Records) {
		if id == newer {
			t.Fatalf("metadata write made %s recent: %v", newer, ids(metadata.Records))
		}
	}
	if got := ids(edited.Records); len(got) != 1 || got[0] != newer {
		t.Fatalf("after the body edit records = %v, want only %s", got, newer)
	}
	listedIDs := make([]string, 0, len(listed))
	for _, summary := range listed {
		listedIDs = append(listedIDs, summary.ID)
	}
	if strings.Join(listedIDs, ",") != strings.Join(ids(edited.Records), ",") {
		t.Fatalf("list since = %v, context since = %v; the surfaces must agree", listedIDs, ids(edited.Records))
	}
}

// TestContextSinceIgnoresReview checks the review path: reviewing a record
// clears its quarantine but is not a text change.
func TestContextSinceIgnoresReview(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newContextFixture(t, true)
	unreviewed, err := f.app.Note(ctx, app.NoteInput{
		ProjectID: "p", Title: "Review since reviewmarker", Body: "reviewmarker body", CreateProject: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := f.app.GetRecord(ctx, "p", unreviewed.Note.ID)
	if err != nil {
		t.Fatal(err)
	}
	window := (*f.clock).Add(time.Hour).Format(time.RFC3339)
	f.advance(2 * time.Hour)

	// Act
	reviewed, err := f.app.ReviewRecord(ctx, app.ReviewRecordInput{
		ProjectID: "p", ID: unreviewed.Note.ID, Trust: "verified",
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if reviewed.ContentUpdatedAt.IsZero() || !reviewed.ContentUpdatedAt.Equal(stored.Summary().ContentUpdatedAt) {
		t.Fatalf("review moved content time: %v -> %v", stored.Summary().ContentUpdatedAt, reviewed.ContentUpdatedAt)
	}
	result, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "reviewmarker", Since: window, IncludeUnreviewed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(result.Records); len(got) != 0 {
		t.Fatalf("review made the record recent: %v", got)
	}
}

func strPtr(value string) *string { return &value }

func mustParseSince(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return at
}
