package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestRunStoreAppendsWithTerminalStatusFinishes(t *testing.T) {
	store := NewMemoryStore()
	rs := RunStore{Store: store}
	// FinishedAt is deliberately unset: the adapter must still stamp the
	// terminal status through FinishConnectorRun.
	run := domain.ConnectorRun{
		ID:        "run-terminal",
		Source:    "github",
		Status:    domain.ConnectorFailed,
		StartedAt: time.Now().UTC(),
		Error:     "boom",
	}
	if err := rs.AppendConnectorRun(context.Background(), run); err != nil {
		t.Fatalf("AppendConnectorRun: %v", err)
	}
	record, err := store.GetConnectorRun(context.Background(), "run-terminal")
	if err != nil || record.Status != domain.ConnectorFailed || record.Error != "boom" || record.FinishedAt == nil {
		t.Fatalf("finished record = %+v, %v", record, err)
	}
	if record.Revision != 2 {
		t.Fatalf("revision = %d, want 2 (start + finish)", record.Revision)
	}
}

func TestRunStoreAppendsRunningStaysOpen(t *testing.T) {
	store := NewMemoryStore()
	rs := RunStore{Store: store}
	run := domain.ConnectorRun{
		ID:        "run-open",
		Source:    "imap",
		Status:    domain.ConnectorRunning,
		StartedAt: time.Now().UTC(),
	}
	if err := rs.AppendConnectorRun(context.Background(), run); err != nil {
		t.Fatalf("AppendConnectorRun: %v", err)
	}
	record, err := store.GetConnectorRun(context.Background(), "run-open")
	if err != nil || record.Status != domain.ConnectorRunning || record.FinishedAt != nil {
		t.Fatalf("open record = %+v, %v", record, err)
	}
	if record.Revision != 1 {
		t.Fatalf("revision = %d, want 1 (start only)", record.Revision)
	}
}

func TestRunStoreWithoutStoreIsNoop(t *testing.T) {
	rs := RunStore{}
	if err := rs.AppendConnectorRun(context.Background(), domain.ConnectorRun{}); err != nil {
		t.Fatalf("AppendConnectorRun: %v", err)
	}
}
