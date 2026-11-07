package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// flakyIndex fails Upsert while failing is set and remembers indexed IDs.
type flakyIndex struct {
	mu      sync.Mutex
	failing bool
	docs    map[string]ports.Document
}

func (f *flakyIndex) Upsert(_ context.Context, doc ports.Document) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing {
		return errors.New("database is locked")
	}
	if f.docs == nil {
		f.docs = map[string]ports.Document{}
	}
	f.docs[doc.ID] = doc
	return nil
}

func (f *flakyIndex) Delete(context.Context, string) error { return nil }

func (f *flakyIndex) Search(_ context.Context, query ports.Query) ([]ports.Hit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var hits []ports.Hit
	for _, doc := range f.docs {
		if strings.Contains(doc.Title+" "+doc.Body, query.Text) {
			hits = append(hits, ports.Hit{ID: doc.ID, Title: doc.Title})
		}
	}
	return hits, nil
}

func TestFailedIndexUpdateIsRepairedByTheNextSearch(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	index := &flakyIndex{}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = dataDir
	a := app.New(cfg, app.Dependencies{Store: store, Index: index})

	index.failing = true
	if _, err := a.Note(ctx, app.NoteInput{ProjectID: "p", Title: "Searchable", Body: "kiwi", CreateProject: true}); err == nil {
		t.Fatal("index failure was not reported")
	}
	marker := filepath.Join(dataDir, "index", "dirty")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("dirty marker missing after a failed index update: %v", err)
	}
	report, err := a.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range report.Checks {
		if check.Name == "index" && check.Status != "warn" {
			t.Fatalf("doctor index check = %+v, want warn", check)
		}
	}

	index.failing = false
	hits, err := a.Search(ctx, "kiwi", "", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("search after repair = %+v, %v", hits, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dirty marker not cleared: %v", err)
	}
}
