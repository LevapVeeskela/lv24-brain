package app_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// benchmarkLinkedCorpus builds a project whose packed layers select many
// records: every decision matches the query, and each links to one of forty
// notes. The link layer is what used to cost one project listing per selected
// record, so the benchmark measures the batch lookup.
func benchmarkLinkedCorpus(b *testing.B) contextFixture {
	b.Helper()
	ctx := context.Background()
	store, err := fsstore.New(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	clock := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	index := newContextIndex()
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	f := contextFixture{
		app:   app.New(cfg, app.Dependencies{Store: store, Index: index, Now: func() time.Time { return clock }}),
		store: store, index: index, clock: &clock,
	}
	notes := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		f.advance(time.Minute)
		result, err := f.app.Note(ctx, app.NoteInput{
			ProjectID: "p", Body: fmt.Sprintf("linked note %02d batchlink", i), CreateProject: true,
		})
		if err != nil {
			b.Fatal(err)
		}
		notes = append(notes, result.Note.ID)
	}
	for i := 0; i < 40; i++ {
		f.advance(time.Minute)
		if _, err := f.app.Decision(ctx, app.DecisionInput{
			ProjectID: "p", Title: fmt.Sprintf("Linked root %02d batchlink", i),
			Context: fmt.Sprintf("root %02d batchlink body [[%s]]", i, notes[i]),
		}); err != nil {
			b.Fatal(err)
		}
	}
	return f
}

// BenchmarkContextLinkedLayer measures one packed context answer whose linked
// layer resolves the neighbours of every selected record.
func BenchmarkContextLinkedLayer(b *testing.B) {
	f := benchmarkLinkedCorpus(b)
	opts := app.ContextOptions{ProjectID: "p", Query: "batchlink", TokenBudget: domain.MaxContextBudgetTokens}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.app.Context(context.Background(), opts); err != nil {
			b.Fatal(err)
		}
	}
}
