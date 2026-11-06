package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	defaultConnectorTimeout = 5 * time.Minute
	connectorRunPersistTime = 5 * time.Second
)

// connectorRunner is the brain-server implementation of
// httpapi.ConnectorRunner. It builds a read-only connector from the process
// environment/OS secret store, runs it with a bounded context, and persists the
// run record through the HTTP store (PostgreSQL or memory).
type connectorRunner struct {
	store       httpapi.Store
	secretStore ports.SecretStore
	timeout     time.Duration
	logger      *slog.Logger
}

var _ httpapi.ConnectorRunner = (*connectorRunner)(nil)

func newConnectorRunner(store httpapi.Store, secretStore ports.SecretStore, timeout time.Duration, logger *slog.Logger) *connectorRunner {
	if timeout <= 0 {
		timeout = defaultConnectorTimeout
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &connectorRunner{store: store, secretStore: secretStore, timeout: timeout, logger: logger}
}

// runConnector executes one connector, converting a connector panic into a
// failed run record so the attempt is still persisted and auditable instead of
// only being swallowed (or killing the HTTP connection) by net/http.
func (r *connectorRunner) runConnector(ctx context.Context, connector ports.Connector, source string, opts ports.ConnectorOptions) (run domain.ConnectorRun, runErr error) {
	defer func() {
		if rec := recover(); rec != nil {
			runErr = fmt.Errorf("connector panicked: %v", rec)
			run = connectors.FinishRun(connectors.NewRun(source, time.Now()), nil, runErr, time.Now())
		}
	}()
	return connector.Run(ctx, opts)
}

// Run executes one connector. A failed/partial connector result is returned as
// a completed run record (status and sanitized error on the record); only build
// and persistence failures are returned as errors.
func (r *connectorRunner) Run(ctx context.Context, req httpapi.ConnectorRunRequest) (domain.ConnectorRun, error) {
	if r == nil || r.store == nil {
		return domain.ConnectorRun{}, errors.New("server: connector runner has no store")
	}
	if r.secretStore == nil {
		return domain.ConnectorRun{}, errors.New("server: connector runner has no secret store")
	}
	name := strings.ToLower(strings.TrimSpace(req.Name))
	connector, err := connectorbuild.Build(connectorbuild.Options{
		Name:     name,
		Projects: req.Projects,
		Limit:    req.Limit,
		Secrets:  r.secretStore,
		Sink:     connectors.DiscardSink{},
	})
	if err != nil {
		return domain.ConnectorRun{}, err
	}

	runCtx := ctx
	cancel := func() {}
	if r.timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, r.timeout)
	}
	defer cancel()

	started := time.Now()
	opts := ports.ConnectorOptions{Projects: req.Projects, Since: req.Since, Limit: req.Limit}
	run, runErr := r.runConnector(runCtx, connector, name, opts)
	if run.ID == "" {
		if runErr != nil {
			return run, runErr
		}
		return run, errors.New("server: connector returned an empty run id")
	}

	// Persist even when the connector failed, matching local mode: the record
	// carries the sanitized error and counts. The write is detached from the
	// request context so a client disconnect does not lose the record.
	persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(runCtx), connectorRunPersistTime)
	defer cancelPersist()
	if err := (httpapi.RunStore{Store: r.store}).AppendConnectorRun(persistCtx, run); err != nil {
		return run, fmt.Errorf("server: record connector run: %w", err)
	}

	r.logger.Info("connector run finished",
		"source", run.Source,
		"status", run.Status,
		"counts", len(run.Counts),
		"duration_ms", time.Since(started).Milliseconds(),
	)
	return run, nil
}
