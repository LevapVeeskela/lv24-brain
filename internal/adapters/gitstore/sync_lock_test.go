package gitstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// TestPublishWaitsForTheSyncLockWithoutStealingIt holds the data store's sync
// lock and proves a publish neither proceeds nor deletes the live lock. After
// the release the publish runs and leaves no lock behind.
func TestPublishWaitsForTheSyncLockWithoutStealingIt(t *testing.T) {
	requireGit(t)
	repo := initRepo(t, t.TempDir(), false)
	writeFile(t, repo.dir, "connectors/work/items/old.md", "shared")
	repo.run("add", ".")
	repo.run("commit", "-qm", "seed")
	provider := newTestProvider(t, repo.dir)

	store, err := fsstore.New(repo.dir)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	release, err := store.LockSync(context.Background())
	if err != nil {
		t.Fatalf("LockSync: %v", err)
	}
	lockPath := filepath.Join(repo.dir, "sync.lock")
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read the held sync lock: %v", err)
	}
	var owner struct {
		PID   int    `json:"pid"`
		Host  string `json:"host"`
		TTL   int64  `json:"ttl"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &owner); err != nil {
		t.Fatalf("decode the held sync lock: %v", err)
	}
	if owner.PID != os.Getpid() || owner.Token == "" || owner.TTL <= 0 || owner.Host == "" {
		t.Fatalf("sync lock is not owner-aware: %+v", owner)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = provider.PrepareConnectionPublish(ctx, "work", domain.PublishLocalOnly)
	if err == nil {
		t.Fatal("publish proceeded while the sync lock was held")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("publish error = %v, want the context deadline", err)
	}
	current, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("publish deleted the live sync lock: %v", err)
	}
	if string(current) != string(raw) {
		t.Fatalf("publish replaced the live sync lock:\n%s", current)
	}

	release()
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("release left the sync lock: %v", err)
	}
	if err := provider.PrepareConnectionPublish(context.Background(), "work", domain.PublishLocalOnly); err != nil {
		t.Fatalf("publish after release: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("publish left the sync lock: %v", err)
	}
}
