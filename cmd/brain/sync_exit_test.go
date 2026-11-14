package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type cliSync struct {
	status  ports.SyncStatus
	pull    ports.SyncResult
	pullErr error
}

func (s *cliSync) Status(context.Context) (ports.SyncStatus, error) {
	return s.status, nil
}

func (s *cliSync) Pull(context.Context, ports.SyncOptions) (ports.SyncResult, error) {
	return s.pull, s.pullErr
}

func (s *cliSync) Commit(context.Context, string) (domain.SyncRevision, error) {
	return domain.SyncRevision{}, nil
}

func (s *cliSync) Push(context.Context) (ports.SyncResult, error) {
	return ports.SyncResult{}, nil
}

// conflictTestError wraps the domain sentinel like gitstore.ConflictError.
type conflictTestError struct{ paths string }

func (e conflictTestError) Error() string {
	return "gitstore: pull stopped on a conflict in " + e.paths
}
func (e conflictTestError) Unwrap() error { return domain.ErrConflict }

func syncTestConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "brain.yaml")
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.DataDir = t.TempDir()
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	return path
}

func useSync(t *testing.T, syncProvider ports.SyncProvider) {
	t.Helper()
	previous := buildRuntime
	buildRuntime = func(cfg config.BrainConfig) (*bootstrap.Runtime, error) {
		return &bootstrap.Runtime{App: app.New(cfg, app.Dependencies{Sync: syncProvider})}, nil
	}
	t.Cleanup(func() { buildRuntime = previous })
}

func TestSyncConflictExitCodeIsThree(t *testing.T) {
	useSync(t, &cliSync{
		status:  ports.SyncStatus{Provider: "git", Branch: "main"},
		pull:    ports.SyncResult{Conflicts: []string{"projects/demo/state.md"}},
		pullErr: conflictTestError{paths: "projects/demo/state.md"},
	})
	code, stdout, stderr := runWithInputForTest(t, "", "sync", "--config", syncTestConfig(t))
	if code != 3 {
		t.Fatalf("exit code = %d, want 3 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "conflict: projects/demo/state.md") {
		t.Fatalf("stdout does not name the conflict:\n%s", stdout)
	}
	if !strings.Contains(stderr, "resolve the conflicting files") {
		t.Fatalf("stderr lacks the recovery hint:\n%s", stderr)
	}
}

func TestSyncStatusJSONReportsUnmergedAndLastSynced(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	useSync(t, &cliSync{status: ports.SyncStatus{
		Provider:   "git",
		Branch:     "main",
		Ahead:      2,
		Behind:     1,
		Dirty:      true,
		Conflicts:  []string{"projects/demo/registry.json"},
		LastSynced: domain.SyncRevision{Kind: domain.SyncKindGit, Value: "abc123", At: at},
	}})
	code, stdout, stderr := runWithInputForTest(t, "", "sync", "--status", "--json", "--config", syncTestConfig(t))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	var envelope struct {
		Schema int      `json:"schema"`
		Data   jsonSync `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if envelope.Schema != 1 {
		t.Fatalf("schema = %d", envelope.Schema)
	}
	data := envelope.Data
	if data.Provider != "git" || data.Branch != "main" || data.Ahead != 2 || data.Behind != 1 || !data.Dirty || !data.Unmerged {
		t.Fatalf("status payload = %+v", data)
	}
	if len(data.Conflicts) != 1 || data.Conflicts[0] != "projects/demo/registry.json" {
		t.Fatalf("conflicts = %v", data.Conflicts)
	}
	if data.LastSynced == nil || data.LastSynced.Value != "abc123" || !data.LastSynced.At.Equal(at) {
		t.Fatalf("last synced = %+v", data.LastSynced)
	}
}

func TestSyncConflictCheckUsesDomainSentinel(t *testing.T) {
	if !errors.Is(conflictTestError{paths: "x"}, domain.ErrConflict) {
		t.Fatal("the conflict test error does not wrap domain.ErrConflict")
	}
}
