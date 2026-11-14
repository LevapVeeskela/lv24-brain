package app_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// blockingSync holds the first Pull until the test releases it, so a second
// sync can be shown to wait on the data-directory lock instead of interleaving.
type blockingSync struct {
	mu          sync.Mutex
	statusCalls int
	started     chan struct{}
	release     chan struct{}
	once        sync.Once
}

func newBlockingSync() *blockingSync {
	return &blockingSync{started: make(chan struct{}), release: make(chan struct{})}
}

func (s *blockingSync) Status(context.Context) (ports.SyncStatus, error) {
	s.mu.Lock()
	s.statusCalls++
	s.mu.Unlock()
	return ports.SyncStatus{Provider: "git", Branch: "main"}, nil
}

func (s *blockingSync) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusCalls
}

func (s *blockingSync) Pull(context.Context, ports.SyncOptions) (ports.SyncResult, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return ports.SyncResult{Revision: domain.SyncRevision{Kind: domain.SyncKindGit, Value: "abc"}}, nil
}

func (s *blockingSync) Commit(context.Context, string) (domain.SyncRevision, error) {
	return domain.SyncRevision{Kind: domain.SyncKindGit, Value: "abc"}, nil
}

func (s *blockingSync) Push(context.Context) (ports.SyncResult, error) {
	return ports.SyncResult{}, nil
}

func TestConcurrentSyncsSerialize(t *testing.T) {
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	syncProvider := newBlockingSync()
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.DataDir = dataDir
	useCase := app.New(cfg, app.Dependencies{Store: store, Sync: syncProvider})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	firstDone := make(chan error, 1)
	go func() {
		_, err := useCase.Sync(ctx, app.SyncOptions{})
		firstDone <- err
	}()
	select {
	case <-syncProvider.started:
	case <-ctx.Done():
		t.Fatal("the first sync never started")
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := useCase.Sync(ctx, app.SyncOptions{})
		secondDone <- err
	}()

	// The second run must not even read Status while the first holds the lock.
	time.Sleep(150 * time.Millisecond)
	if calls := syncProvider.calls(); calls != 1 {
		t.Fatalf("status calls while the first sync is running = %d, want 1", calls)
	}
	select {
	case err := <-secondDone:
		t.Fatalf("the second sync finished before the first was released: %v", err)
	default:
	}

	close(syncProvider.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if calls := syncProvider.calls(); calls < 2 {
		t.Fatalf("status calls after both syncs = %d, want at least 2", calls)
	}
}
