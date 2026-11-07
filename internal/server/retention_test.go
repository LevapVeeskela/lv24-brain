package server

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/postgres"
)

func TestRetentionJobNilIsNoOp(t *testing.T) {
	var job *retentionJob
	done := make(chan struct{})
	go func() {
		job.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("nil retention job must return immediately (memory mode)")
	}
}

func TestRetentionJobStopsOnCancel(t *testing.T) {
	job := &retentionJob{interval: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		job.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retention job must stop when its context is cancelled")
	}
}

func TestRetentionJobRunOnceAgainstPostgres(t *testing.T) {
	baseURL := os.Getenv(postgres.TestDatabaseURLEnv)
	if baseURL == "" {
		t.Skipf("%s is not set; skipping PostgreSQL integration test", postgres.TestDatabaseURLEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pg, err := postgres.Open(ctx, isolatedDatabaseURL(t, baseURL), postgres.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	if _, err := pg.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var logs bytes.Buffer
	job := newRetentionJob(pg, DefaultConfig(), slog.New(slog.NewTextHandler(&logs, nil)))
	job.runOnce(ctx)
	if !strings.Contains(logs.String(), "retention completed") {
		t.Fatalf("runOnce logs = %q, want a completed record", logs.String())
	}
}
