package connectors

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// runRecordTimeout bounds the detached write of one run record.
const runRecordTimeout = 5 * time.Second

// Recording decorates a connector with run-record persistence. A connector run
// is stored even when the connector fails: the record carries the sanitized
// error. Persistence errors never hide the original run result.
type Recording struct {
	Inner ports.Connector
	Runs  ports.ConnectorRunStore
}

var _ ports.Connector = Recording{}

// Name forwards the wrapped connector name.
func (r Recording) Name() string {
	if r.Inner == nil {
		return ""
	}
	return r.Inner.Name()
}

// Run executes the wrapped connector and appends its run record. The record is
// written with a detached, bounded context so a canceled or timed-out run is
// still persisted.
func (r Recording) Run(ctx context.Context, opts ports.ConnectorOptions) (domain.ConnectorRun, error) {
	if r.Inner == nil {
		return domain.ConnectorRun{}, errors.New("connectors: recording wrapper has no connector")
	}
	run, runErr := r.Inner.Run(ctx, opts)
	if r.Runs == nil || run.ID == "" || run.Source == "" {
		return run, runErr
	}
	if ctx == nil {
		ctx = context.Background()
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runRecordTimeout)
	defer cancel()
	persistErr := r.Runs.AppendConnectorRun(persistCtx, run)
	if persistErr == nil {
		return run, runErr
	}
	wrapped := fmt.Errorf("record connector run: %w", persistErr)
	if runErr != nil {
		return run, errors.Join(runErr, wrapped)
	}
	return run, wrapped
}
