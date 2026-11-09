package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	defaultConnectorTimeout = 5 * time.Minute
	connectorRunPersistTime = 5 * time.Second
)

// errConnectorPanicked marks a run that ended in a recovered connector panic.
// Unlike an ordinary connector failure it is a server bug, so Run persists the
// failed record and still returns the error (HTTP 500) to the caller.
var errConnectorPanicked = errors.New("connector panicked")

// connectorRunner is the brain-server implementation of
// httpapi.ConnectorRunner. It synthesizes an ephemeral connection from the
// process environment and the request, runs it through the item-level
// ingestion host (raw-zone items before checkpoints), and persists the run
// record through the HTTP store (PostgreSQL or memory).
type connectorRunner struct {
	store       httpapi.Store
	secretStore ports.SecretStore
	timeout     time.Duration
	logger      *slog.Logger
	// registry builds sources for ephemeral connections. Nil selects
	// connectorbuild.Builtin(); tests inject a fake native registry.
	registry *connectorbuild.Registry
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

// Run executes one connector through the ingestion host. A failed or partial
// source result is returned as a completed run record (status and sanitized
// error on the record); only panics, build/lock failures and persistence
// failures are returned as errors.
func (r *connectorRunner) Run(ctx context.Context, req httpapi.ConnectorRunRequest) (domain.ConnectorRun, error) {
	if r == nil || r.store == nil {
		return domain.ConnectorRun{}, errors.New("server: connector runner has no store")
	}
	if r.secretStore == nil {
		return domain.ConnectorRun{}, errors.New("server: connector runner has no secret store")
	}
	// Memory mode has no raw zone: fail as unsupported instead of reporting
	// success while discarding output.
	raw, ok := r.store.(rawStore)
	if !ok {
		return domain.ConnectorRun{}, fmt.Errorf("%w: server: connector runs need a durable raw zone", domain.ErrUnsupported)
	}
	if projectID := strings.TrimSpace(req.ProjectID); projectID != "" {
		if _, err := r.store.GetProject(ctx, projectID); err != nil {
			return domain.ConnectorRun{}, fmt.Errorf("server: unknown project %q: %w", projectID, err)
		}
	}
	conn, err := buildServerConnection(req)
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
	host := r.newIngestion(raw, conn)
	result, ingestErr, panicked := runIngestion(runCtx, host, conn, req)
	if panicked != nil {
		return r.persistPanic(runCtx, conn, started, panicked)
	}
	if result.Busy {
		return domain.ConnectorRun{}, fmt.Errorf("%w: connection %q is already running", domain.ErrConflict, conn.ID)
	}
	run := result.Run
	if run.ID == "" {
		if ingestErr != nil {
			return run, ingestErr
		}
		return run, errors.New("server: connector returned an empty run id")
	}

	// Persist even when the source failed, matching local mode: the record
	// carries the sanitized error and counts. The write is detached from the
	// request context so a client disconnect does not lose the record. The
	// ingestion error itself stays on the record; only persistence failures
	// are returned.
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

// runIngestion runs one ephemeral connection through the host, converting a
// source panic into a recovered value so the caller can persist a durable
// sanitized failed run instead of only being swallowed (or killing the HTTP
// connection) by net/http.
func runIngestion(ctx context.Context, host *app.Ingestion, conn domain.Connection, req httpapi.ConnectorRunRequest) (result app.RunConnectionResult, ingestErr error, panicked any) {
	defer func() {
		if rec := recover(); rec != nil {
			result = app.RunConnectionResult{}
			ingestErr = nil
			panicked = rec
		}
	}()
	result, ingestErr = host.RunConnection(ctx, app.RunConnectionOptions{
		ConnectionID: conn.ID,
		Since:        req.Since,
		Limit:        req.Limit,
	})
	return result, ingestErr, nil
}

// persistPanic stores a sanitized failed run for a recovered source panic and
// returns it with the panic error. A persistence failure surfaces as an error
// carrying the run; the panic detail stays on the record, redacted.
func (r *connectorRunner) persistPanic(ctx context.Context, conn domain.Connection, started time.Time, rec any) (domain.ConnectorRun, error) {
	redacted, _ := app.RedactIngest(strings.TrimSpace(fmt.Sprintf("%v", rec)))
	if redacted == "" {
		redacted = "connector panicked"
	}
	panicErr := fmt.Errorf("%w: %s", errConnectorPanicked, connectors.SanitizeText(redacted, 500))
	run := connectors.NewRun(conn.Provider, started)
	run.ConnectionID = conn.ID
	run = connectors.FinishRun(run, nil, panicErr, time.Now())

	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), connectorRunPersistTime)
	defer cancel()
	if err := (httpapi.RunStore{Store: r.store}).AppendConnectorRun(persistCtx, run); err != nil {
		return run, fmt.Errorf("server: record connector run: %w", err)
	}
	r.logger.Error("connector run panicked", "source", run.Source, "connection", conn.ID)
	return run, panicErr
}
