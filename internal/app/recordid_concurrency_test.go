package app_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// memoryStore records records in memory. It exists so the concurrency test
// exercises the app's id allocation and the real search index without paying
// fsstore's whole-tree revision hash on every one of a thousand writes. File
// naming under concurrency is covered by the fsstore package tests.
type memoryStore struct {
	mu        sync.Mutex
	project   domain.Project
	notes     []domain.Note
	decisions []domain.Decision
	attempts  []domain.Attempt
	handoffs  []domain.Handoff
}

func newMemoryStore(projectID string) *memoryStore {
	return &memoryStore{project: domain.Project{ID: projectID, Name: projectID, Kind: domain.ProjectKindOther}}
}

func (s *memoryStore) GetProject(_ context.Context, id string) (domain.Project, error) {
	if id != s.project.ID {
		return domain.Project{}, domain.ErrNotFound
	}
	return s.project, nil
}

func (s *memoryStore) PutProject(_ context.Context, project domain.Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.project = project
	return nil
}

func (s *memoryStore) ListProjects(context.Context) ([]domain.Project, error) {
	return []domain.Project{s.project}, nil
}

func (s *memoryStore) GetState(context.Context, string) (domain.State, error) {
	return domain.State{}, domain.ErrNotFound
}

func (s *memoryStore) PutState(context.Context, domain.State) (domain.SyncRevision, error) {
	return domain.SyncRevision{Kind: domain.SyncKindGit, Value: "memory"}, nil
}

func (s *memoryStore) CurrentRevision(context.Context) (domain.SyncRevision, error) {
	return domain.SyncRevision{Kind: domain.SyncKindGit, Value: "memory"}, nil
}

func (s *memoryStore) AppendNote(_ context.Context, note domain.Note) (domain.SyncRevision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.notes {
		if existing.ID == note.ID {
			return domain.SyncRevision{}, domain.NewTargetExistsError(ports.RecordNote, note.ID, note.ID)
		}
	}
	s.notes = append(s.notes, note)
	return domain.SyncRevision{Kind: domain.SyncKindGit, Value: fmt.Sprintf("rev-%d", len(s.notes))}, nil
}

func (s *memoryStore) AppendDecision(_ context.Context, decision domain.Decision) (domain.SyncRevision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.decisions {
		if existing.ID == decision.ID {
			return domain.SyncRevision{}, domain.NewTargetExistsError(ports.RecordDecision, decision.ID, decision.ID)
		}
	}
	s.decisions = append(s.decisions, decision)
	return domain.SyncRevision{Kind: domain.SyncKindGit, Value: fmt.Sprintf("rev-%d", len(s.decisions))}, nil
}

func (s *memoryStore) AppendAttempt(_ context.Context, attempt domain.Attempt) (domain.SyncRevision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.attempts {
		if existing.ID == attempt.ID {
			return domain.SyncRevision{}, domain.NewTargetExistsError(ports.RecordAttempt, attempt.ID, attempt.ID)
		}
	}
	s.attempts = append(s.attempts, attempt)
	return domain.SyncRevision{Kind: domain.SyncKindGit, Value: fmt.Sprintf("rev-%d", len(s.attempts))}, nil
}

func (s *memoryStore) AppendHandoff(_ context.Context, handoff domain.Handoff) (domain.SyncRevision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.handoffs {
		if existing.ID == handoff.ID {
			return domain.SyncRevision{}, domain.NewTargetExistsError(ports.RecordHandoff, handoff.ID, handoff.ID)
		}
	}
	s.handoffs = append(s.handoffs, handoff)
	return domain.SyncRevision{Kind: domain.SyncKindGit, Value: fmt.Sprintf("rev-%d", len(s.handoffs))}, nil
}

func (s *memoryStore) ListNotes(context.Context, string) ([]domain.Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Note(nil), s.notes...), nil
}

func (s *memoryStore) ListDecisions(context.Context, string) ([]domain.Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Decision(nil), s.decisions...), nil
}

func (s *memoryStore) ListAttempts(context.Context, string) ([]domain.Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Attempt(nil), s.attempts...), nil
}

func (s *memoryStore) ListHandoffs(context.Context, string) ([]domain.Handoff, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Handoff(nil), s.handoffs...), nil
}

// TestConcurrentDecisionWritesHaveUniqueIDs writes 1000 decisions with one
// injected clock second: every id and index row must be distinct.
func TestConcurrentDecisionWritesHaveUniqueIDs(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore("p")
	index, err := sqliteindex.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("sqliteindex.Open: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })

	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	application := app.New(cfg, app.Dependencies{
		Store: store,
		Index: index,
		Now:   func() time.Time { return clock },
	})

	const writers = 1000
	ids := make([]string, writers)
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := application.Decision(ctx, app.DecisionInput{
				ProjectID: "p",
				Title:     fmt.Sprintf("Concurrent decision %04d", i),
				Context:   fmt.Sprintf("writer %d", i),
			})
			if err != nil {
				errs[i] = err
				return
			}
			ids[i] = result.Decision.ID
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	seen := make(map[string]struct{}, writers)
	for i, id := range ids {
		if !domain.IsRecordID("decision", id) {
			t.Fatalf("decision %d id = %q, want <kind>-<date>-<slug>-<ULID>", i, id)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate record id %q after %d writes", id, i)
		}
		seen[id] = struct{}{}
	}
	if len(store.decisions) != writers {
		t.Fatalf("stored decisions = %d, want %d", len(store.decisions), writers)
	}
	indexed, err := index.IDs(ctx)
	if err != nil {
		t.Fatalf("Index.IDs: %v", err)
	}
	if len(indexed) != writers {
		t.Fatalf("index rows = %d, want %d", len(indexed), writers)
	}
	indexedOnce := make(map[string]struct{}, len(indexed))
	for _, key := range indexed {
		if _, duplicate := indexedOnce[key]; duplicate {
			t.Fatalf("duplicate index row %q", key)
		}
		indexedOnce[key] = struct{}{}
	}
}
