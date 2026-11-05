package httpapi

import (
	"context"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// RunStore adapts the HTTP API Store to the connector run store contract used
// by connector wrappers and the CLI. Server mode has no Markdown sink, so run
// records live in the API store instead of connectors/runs.
type RunStore struct {
	Store Store
}

var _ ports.ConnectorRunStore = RunStore{}

// AppendConnectorRun creates a run record and, when the run already carries a
// terminal status, finishes it in the same call so local and server modes store
// the same final record.
func (s RunStore) AppendConnectorRun(ctx context.Context, run domain.ConnectorRun) error {
	if s.Store == nil {
		return nil
	}
	record, _, err := s.Store.CreateConnectorRun(ctx, run)
	if err != nil {
		return err
	}
	if run.Status == domain.ConnectorRunning || run.FinishedAt == nil {
		return nil
	}
	_, _, err = s.Store.FinishConnectorRun(ctx, record.ID, run.Status, run.Counts, run.Error, 0)
	return err
}

// ListConnectorRuns returns server run records newest first, mapped to the
// domain type. An empty source means every source.
func (s RunStore) ListConnectorRuns(ctx context.Context, source string, limit int) ([]domain.ConnectorRun, error) {
	if s.Store == nil {
		return nil, nil
	}
	records, err := s.Store.ListConnectorRuns(ctx, ConnectorRunFilter{
		Source: strings.TrimSpace(source),
		Limit:  limit,
	})
	if err != nil {
		return nil, err
	}
	runs := make([]domain.ConnectorRun, 0, len(records))
	for _, record := range records {
		runs = append(runs, record.ConnectorRun)
	}
	return runs, nil
}
