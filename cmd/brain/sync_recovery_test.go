package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// recoveringSync models a checkout left by a killed run: Status reports an
// unresolved path until recovery repairs the interrupted rebase. It proves the
// CLI calls recovery before the conflict pre-check.
type recoveringSync struct {
	recovered     bool
	recoveryCalls int
	conflictPaths []string
	statusCalls   int
}

func (s *recoveringSync) Status(context.Context) (ports.SyncStatus, error) {
	s.statusCalls++
	status := ports.SyncStatus{Provider: "git", Branch: "main"}
	if !s.recovered {
		status.Conflicts = append([]string{}, s.conflictPaths...)
	}
	return status, nil
}
func (s *recoveringSync) RecoverInterruptedSync(context.Context) (bool, error) {
	s.recoveryCalls++
	s.recovered = true
	return true, nil
}
func (s *recoveringSync) Pull(context.Context, ports.SyncOptions) (ports.SyncResult, error) {
	return ports.SyncResult{Recovered: true}, nil
}
func (s *recoveringSync) Commit(context.Context, string) (domain.SyncRevision, error) {
	return domain.SyncRevision{}, nil
}
func (s *recoveringSync) Push(context.Context) (ports.SyncResult, error) {
	return ports.SyncResult{}, nil
}

func TestSyncCLIRecoversBeforeConflictCheck(t *testing.T) {
	sync := &recoveringSync{conflictPaths: []string{"projects/demo/decisions/crash.md"}}
	useSync(t, sync)
	code, stdout, stderr := runWithInputForTest(t, "", "sync", "--config", syncTestConfig(t), "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if sync.recoveryCalls != 1 {
		t.Fatalf("recovery calls = %d, want 1", sync.recoveryCalls)
	}
	if sync.statusCalls == 0 {
		t.Fatal("Status was never called")
	}
	var envelope struct {
		Data jsonSync `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if !envelope.Data.Recovered || len(envelope.Data.Conflicts) != 0 || envelope.Data.Unmerged {
		t.Fatalf("sync payload = %+v", envelope.Data)
	}
}

type recordSync struct{ records []string }

func (s *recordSync) Status(context.Context) (ports.SyncStatus, error) {
	return ports.SyncStatus{Provider: "git", Branch: "main", Dirty: true, ConflictRecords: s.records}, nil
}
func (s *recordSync) Pull(context.Context, ports.SyncOptions) (ports.SyncResult, error) {
	return ports.SyncResult{}, nil
}
func (s *recordSync) Commit(context.Context, string) (domain.SyncRevision, error) {
	return domain.SyncRevision{}, nil
}
func (s *recordSync) Push(context.Context) (ports.SyncResult, error) {
	return ports.SyncResult{}, nil
}

func TestSyncStatusJSONListsConflictRecords(t *testing.T) {
	record := "projects/demo/state-conflicts/20261117T060000Z-goal.md"
	useSync(t, &recordSync{records: []string{record}})
	code, stdout, stderr := runWithInputForTest(t, "", "sync", "--status", "--json", "--config", syncTestConfig(t))
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	var envelope struct {
		Data jsonSync `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	data := envelope.Data
	if !data.Dirty {
		t.Fatal("dirty = false")
	}
	if len(data.Conflicts) != 1 || data.Conflicts[0] != record {
		t.Fatalf("conflicts = %v, want [%s]", data.Conflicts, record)
	}
	if len(data.ConflictRecords) != 1 || data.ConflictRecords[0] != record {
		t.Fatalf("conflict_records = %v", data.ConflictRecords)
	}
	if data.Unmerged {
		t.Fatal("unmerged = true for an informational conflict record")
	}
	if !strings.Contains(stdout, record) {
		t.Fatal("record missing from stdout")
	}
}
