package app_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// newPackFixture builds a project with a state, a handoff, large decisions,
// attempts and notes matching "packtoken", so every layer has material to
// include or drop.
func newPackFixture(t *testing.T) contextFixture {
	t.Helper()
	f := newContextFixture(t, true)
	ctx := context.Background()
	// The first write registers the project before state can be set.
	f.decision(t, "Pack seed decision packtoken", "seed context packtoken")
	goal := "Pack the context"
	next := "Ship the packer"
	active := []string{"layer budgets", "truncation rules"}
	questions := []string{"which cap is right?"}
	if _, err := f.app.SetState(ctx, app.StateInput{
		ProjectID: "p", Goal: &goal, NextAction: &next, Active: &active, OpenQuestions: &questions,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Handoff(ctx, app.HandoffInput{
		ProjectID: "p", Summary: "Packed context handoff packtoken", NextAction: "Review the budget table",
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		f.decision(t, fmt.Sprintf("Pack decision %02d packtoken", i),
			strings.Repeat(fmt.Sprintf("decision %02d packtoken context ", i), 12))
	}
	for i := 0; i < 10; i++ {
		f.attempt(t, fmt.Sprintf("Pack attempt %02d packtoken", i),
			strings.Repeat(fmt.Sprintf("attempt %02d packtoken reason ", i), 10))
	}
	for i := 0; i < 6; i++ {
		f.note(t, strings.Repeat(fmt.Sprintf("note %02d packtoken ", i), 8))
	}
	return f
}

func packSection(t *testing.T, pack app.ContextPack, name string) app.ContextSection {
	t.Helper()
	for _, section := range pack.Sections {
		if section.Name == name {
			return section
		}
	}
	return app.ContextSection{}
}

func TestContextPackHonoursBudgets(t *testing.T) {
	for _, budget := range []int{2000, 6000, 12000} {
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
			if result.Pack.Budget != budget || result.Pack.TotalTokens > budget {
				t.Fatalf("budget = %d total = %d, want at most %d", result.Pack.Budget, result.Pack.TotalTokens, budget)
			}
			core := packSection(t, result.Pack, app.ContextLayerCore)
			if len(core.Records) == 0 || core.Records[0].ID != "state" {
				t.Fatalf("core records = %+v, want the state first", core.Records)
			}
			for _, want := range []string{"goal: Pack the context", "next: Review the budget table", "open_questions:"} {
				if !strings.Contains(result.Summary, want) {
					t.Fatalf("summary lacks %q:\n%s", want, result.Summary)
				}
			}
			sum := 0
			for _, section := range result.Pack.Sections {
				sum += section.Tokens
				if section.Name == app.ContextLayerCore {
					continue
				}
				for _, record := range section.Records {
					if record.Tokens <= 0 || record.Path == "" || record.Title == "" || record.ID == "" {
						t.Fatalf("%s record = %+v, want id, title, path and tokens", section.Name, record)
					}
					// A record is never split: its title and full preview must
					// be in the rendered answer.
					if !strings.Contains(result.Summary, record.Title) || !strings.Contains(result.Summary, record.Detail) {
						t.Fatalf("%s record %s was split:\n%s", section.Name, record.ID, result.Summary)
					}
				}
			}
			if sum != result.Pack.TotalTokens {
				t.Fatalf("section sum = %d, total = %d", sum, result.Pack.TotalTokens)
			}
			if budget == 2000 {
				if len(result.Pack.Truncated) == 0 {
					t.Fatal("a 2000-token budget dropped nothing")
				}
				for _, truncation := range result.Pack.Truncated {
					if truncation.ID == "" || truncation.Section == "" || truncation.Reason == "" {
						t.Fatalf("truncation = %+v, want id, section and reason", truncation)
					}
					for _, section := range result.Pack.Sections {
						for _, record := range section.Records {
							if record.ID == truncation.ID {
								t.Fatalf("record %s is both packed and truncated", truncation.ID)
							}
						}
					}
				}
			}
			if budget >= 6000 && len(result.Records) == 0 {
				t.Fatalf("budget %d packed no query hits", budget)
			}
		})
	}
}

func TestContextPackIsDeterministic(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newPackFixture(t)
	opts := app.ContextOptions{ProjectID: "p", Query: "packtoken", TokenBudget: 6000}

	// Act
	first, err := f.app.Context(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.app.Context(ctx, opts)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Pack, second.Pack) {
		t.Fatalf("packs differ:\n%+v\n%+v", first.Pack, second.Pack)
	}
	if first.Summary != second.Summary {
		t.Fatalf("summaries differ:\n%s\n%s", first.Summary, second.Summary)
	}
}

func TestContextPackLinkedLayerNeedsRoom(t *testing.T) {
	// Arrange: the decision links to a note that the query itself does not
	// match, so the note can only arrive through the linked layer.
	ctx := context.Background()
	f := newContextFixture(t, true)
	target := f.note(t, "linked target marker only")
	f.decision(t, "Linked root decision linkedroot", "the root is linked: [["+target+"]]")

	// Act: a large budget has room for the 1-hop layer.
	large, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "linkedroot", TokenBudget: domain.MaxContextBudgetTokens,
	})
	if err != nil {
		t.Fatal(err)
	}
	small, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "linkedroot", TokenBudget: 150,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	linked := packSection(t, large.Pack, app.ContextLayerLinked)
	if len(linked.Records) != 1 || linked.Records[0].ID != target {
		t.Fatalf("linked records = %+v, want %s", linked.Records, target)
	}
	if strings.Contains(small.Summary, "linked target marker") {
		t.Fatalf("a 150-token answer included the linked layer:\n%s", small.Summary)
	}
}

func TestContextPackQueryHitsRespectValidityAndQuarantine(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newContextFixture(t, true)
	old := f.decision(t, "Filter decision old filtermarker", "filtermarker old body")
	current := f.decision(t, "Filter decision current filtermarker", "filtermarker current body")
	if _, err := f.app.Supersede(ctx, "p", old, current); err != nil {
		t.Fatal(err)
	}
	quarantined, err := f.app.Note(ctx, app.NoteInput{
		ProjectID: "p", Title: "Filter quarantined filtermarker", Body: "filtermarker quarantined body",
		Provenance: domain.Provenance{Origin: domain.OriginAgent},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	def, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "filtermarker", TokenBudget: 12000})
	if err != nil {
		t.Fatal(err)
	}
	included, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "filtermarker", TokenBudget: 12000, IncludeUnreviewed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	everything, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "filtermarker", TokenBudget: 12000, IncludeSuperseded: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	for _, section := range def.Pack.Sections {
		for _, record := range section.Records {
			if record.ID == old || record.ID == quarantined.Note.ID {
				t.Fatalf("%s leaked %s into the default context", section.Name, record.ID)
			}
			if record.ID == current && strings.Contains(record.Detail, "old body") {
				t.Fatalf("current decision carries the old body: %+v", record)
			}
		}
	}
	found := false
	for _, record := range included.Records {
		if record.ID == quarantined.Note.ID && record.Quarantined {
			found = true
		}
	}
	if !found {
		t.Fatalf("include-unreviewed records = %+v, want %s", included.Records, quarantined.Note.ID)
	}
	superseded := false
	for _, section := range everything.Pack.Sections {
		for _, record := range section.Records {
			if record.ID == old {
				superseded = true
			}
		}
	}
	if !superseded {
		t.Fatalf("include-superseded did not restore %s", old)
	}
}

func TestContextPackConfigBudget(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newPackFixture(t)
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.Context.Budget = 900
	runtime := app.New(cfg, app.Dependencies{
		Store: f.store, Index: f.index, Now: func() time.Time { return *f.clock },
	})

	// Act
	result, err := runtime.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "packtoken"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if result.Pack.Budget != 900 || result.Pack.TotalTokens > 900 {
		t.Fatalf("budget = %d total = %d, want the configured 900", result.Pack.Budget, result.Pack.TotalTokens)
	}
}

func TestContextRejectsOversizedTokenBudget(t *testing.T) {
	f := newContextFixture(t, true)
	f.note(t, "seed")
	if _, err := f.app.Context(context.Background(), app.ContextOptions{
		ProjectID: "p", Query: "seed", TokenBudget: domain.MaxContextBudgetTokens + 1,
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
}

func TestBootstrapPackedBudget(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newPackFixture(t)

	// Act
	result, err := f.app.Bootstrap(ctx, app.BootstrapOptions{ProjectID: "p", TokenBudget: 2000})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if result.Pack.Budget != 2000 || result.Pack.TotalTokens > 2000 {
		t.Fatalf("budget = %d total = %d", result.Pack.Budget, result.Pack.TotalTokens)
	}
	if strings.Contains(result.Summary, "# p — context") || !strings.Contains(result.Summary, "# p — bootstrap") {
		t.Fatalf("summary header:\n%s", result.Summary)
	}
	for _, want := range []string{"goal: Pack the context", "## Latest handoff", "## Recent decisions", "## Recent attempts"} {
		if !strings.Contains(result.Summary, want) {
			t.Fatalf("summary lacks %q:\n%s", want, result.Summary)
		}
	}
}
