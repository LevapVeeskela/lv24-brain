package sqliteindex

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	contentionHelperEnv = "LV24_SQLITEINDEX_CONTENTION_HELPER"
	contentionDBEnv     = "LV24_SQLITEINDEX_CONTENTION_DB"
	contentionWorkerEnv = "LV24_SQLITEINDEX_CONTENTION_WORKER"
	contentionGateEnv   = "LV24_SQLITEINDEX_CONTENTION_GATE"

	contentionWorkers   = 3
	contentionDocsEach  = 150
	contentionGateLimit = 30 * time.Second
)

// TestConcurrentOpenFreshIndex starts three processes that open the same
// brand-new index at the same moment. The WAL switch takes an exclusive lock
// that busy_timeout does not cover, so before the retry this failed with
// "database is locked (5) (SQLITE_BUSY)".
func TestConcurrentOpenFreshIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns three child processes; skipped with -short")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.db")
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("database %q must not exist before the test (stat error: %v)", dbPath, err)
	}
	gatePath := filepath.Join(dir, "start")

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test executable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	outputs := make([]bytes.Buffer, contentionWorkers)
	commands := make([]*exec.Cmd, contentionWorkers)
	t.Cleanup(func() {
		cancel()
		for _, cmd := range commands {
			if cmd != nil && cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	})
	for worker := 0; worker < contentionWorkers; worker++ {
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestConcurrentOpenHelperProcess$", "-test.count=1")
		cmd.Env = append(os.Environ(),
			contentionHelperEnv+"=1",
			contentionDBEnv+"="+dbPath,
			contentionWorkerEnv+"="+strconv.Itoa(worker),
			contentionGateEnv+"="+gatePath,
		)
		cmd.Stdout = &outputs[worker]
		cmd.Stderr = &outputs[worker]
		if err := cmd.Start(); err != nil {
			t.Fatalf("start worker %d: %v", worker, err)
		}
		commands[worker] = cmd
	}

	waitForWorkers(t, dir, gatePath)
	for worker, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Errorf("worker %d failed: %v\n%s", worker, err, outputs[worker].String())
		}
	}
	if t.Failed() {
		return
	}

	index, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open after the workers finished: %v", err)
	}
	defer func() {
		if err := index.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	ids, err := index.IDs(context.Background())
	if err != nil {
		t.Fatalf("IDs: %v", err)
	}
	if want := contentionWorkers * contentionDocsEach; len(ids) != want {
		t.Fatalf("documents = %d, want %d", len(ids), want)
	}

	var mode string
	if err := index.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if strings.ToLower(mode) != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

// waitForWorkers releases the start gate once every child process has reached
// it, so all three Open calls overlap.
func waitForWorkers(t *testing.T, dir, gatePath string) {
	t.Helper()
	deadline := time.Now().Add(contentionGateLimit)
	for {
		ready := 0
		for worker := 0; worker < contentionWorkers; worker++ {
			if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("ready-%d", worker))); err == nil {
				ready++
			}
		}
		if ready == contentionWorkers {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d workers reached the start gate", ready, contentionWorkers)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := os.WriteFile(gatePath, []byte("go"), 0o600); err != nil {
		t.Fatalf("release start gate: %v", err)
	}
}

// TestConcurrentOpenHelperProcess is the child half of
// TestConcurrentOpenFreshIndex. It is a test so the test binary can re-exec
// itself without a separate helper program.
func TestConcurrentOpenHelperProcess(t *testing.T) {
	if os.Getenv(contentionHelperEnv) != "1" {
		t.Skip("helper process for TestConcurrentOpenFreshIndex")
	}
	dbPath := os.Getenv(contentionDBEnv)
	worker := os.Getenv(contentionWorkerEnv)
	gatePath := os.Getenv(contentionGateEnv)
	if dbPath == "" || worker == "" || gatePath == "" {
		t.Fatal("helper environment is incomplete")
	}

	readyPath := filepath.Join(filepath.Dir(dbPath), "ready-"+worker)
	if err := os.WriteFile(readyPath, []byte("ready"), 0o600); err != nil {
		t.Fatalf("worker %s: write ready marker: %v", worker, err)
	}
	deadline := time.Now().Add(contentionGateLimit)
	for {
		if _, err := os.Stat(gatePath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker %s: timed out waiting for the start gate", worker)
		}
		runtime.Gosched()
	}

	index, err := Open(dbPath)
	if err != nil {
		t.Fatalf("worker %s: Open: %v", worker, err)
	}
	defer func() {
		if err := index.Close(); err != nil {
			t.Errorf("worker %s: Close: %v", worker, err)
		}
	}()

	ctx := context.Background()
	for n := 0; n < contentionDocsEach; n++ {
		doc := ports.Document{
			ID:        fmt.Sprintf("%s-%03d", worker, n),
			ProjectID: "contention",
			Kind:      "note",
			Title:     fmt.Sprintf("Worker %s document %d", worker, n),
			Body:      fmt.Sprintf("concurrent sqlite open probe worker %s document %d", worker, n),
			UpdatedAt: baseTime.Add(time.Duration(n) * time.Second),
		}
		if err := index.Upsert(ctx, doc); err != nil {
			t.Fatalf("worker %s: Upsert %d: %v", worker, n, err)
		}
		hits, err := index.Search(ctx, ports.Query{Text: "probe", Limit: 5})
		if err != nil {
			t.Fatalf("worker %s: Search %d: %v", worker, n, err)
		}
		if len(hits) == 0 {
			t.Fatalf("worker %s: Search %d returned no hits", worker, n)
		}
	}
}
