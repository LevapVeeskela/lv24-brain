package connectors

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type fakeConnector struct {
	name  string
	run   domain.ConnectorRun
	err   error
	calls int
}

func (f *fakeConnector) Name() string { return f.name }

func (f *fakeConnector) Run(context.Context, ports.ConnectorOptions) (domain.ConnectorRun, error) {
	f.calls++
	return f.run, f.err
}

type fakeRunStore struct {
	appended []domain.ConnectorRun
	err      error
	calls    int
}

func (f *fakeRunStore) AppendConnectorRun(_ context.Context, run domain.ConnectorRun) error {
	f.calls++
	f.appended = append(f.appended, run)
	return f.err
}

func (f *fakeRunStore) ListConnectorRuns(context.Context, string, int) ([]domain.ConnectorRun, error) {
	return f.appended, nil
}

type ctxCheckStore struct {
	calls       int
	err         error
	hasDeadline bool
}

func (s *ctxCheckStore) AppendConnectorRun(ctx context.Context, _ domain.ConnectorRun) error {
	s.calls++
	s.err = ctx.Err()
	_, s.hasDeadline = ctx.Deadline()
	return nil
}

func (s *ctxCheckStore) ListConnectorRuns(context.Context, string, int) ([]domain.ConnectorRun, error) {
	return nil, nil
}

func TestRecordingPersistsRunWithCanceledContext(t *testing.T) {
	run := domain.ConnectorRun{ID: "run-canceled", Source: "github", Status: domain.ConnectorFailed, Error: "canceled"}
	inner := &fakeConnector{name: "github", run: run, err: context.Canceled}
	store := &ctxCheckStore{}
	recorder := Recording{Inner: inner, Runs: store}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := recorder.Run(ctx, ports.ConnectorOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if got.ID != run.ID {
		t.Fatalf("run result lost: %+v", got)
	}
	if store.calls != 1 {
		t.Fatalf("store calls = %d", store.calls)
	}
	if store.err != nil {
		t.Fatalf("persistence context canceled: %v", store.err)
	}
	if !store.hasDeadline {
		t.Fatal("persistence context has no deadline")
	}
}

func TestRecordingPersistsSuccessfulRun(t *testing.T) {
	run := domain.ConnectorRun{ID: "run-1", Source: "github", Status: domain.ConnectorSuccess}
	inner := &fakeConnector{name: "github", run: run}
	store := &fakeRunStore{}
	recorder := Recording{Inner: inner, Runs: store}

	got, err := recorder.Run(context.Background(), ports.ConnectorOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.ID != run.ID || inner.calls != 1 {
		t.Fatalf("run = %+v, calls = %d", got, inner.calls)
	}
	if store.calls != 1 || len(store.appended) != 1 || store.appended[0].ID != run.ID {
		t.Fatalf("store = %+v", store)
	}
	if recorder.Name() != "github" {
		t.Fatalf("Name = %q", recorder.Name())
	}
}

func TestRecordingPersistsFailedRunAndReturnsError(t *testing.T) {
	boom := errors.New("network down")
	run := domain.ConnectorRun{ID: "run-2", Source: "gitlab", Status: domain.ConnectorFailed, Error: "network down"}
	inner := &fakeConnector{name: "gitlab", run: run, err: boom}
	store := &fakeRunStore{}
	recorder := Recording{Inner: inner, Runs: store}

	got, err := recorder.Run(context.Background(), ports.ConnectorOptions{})
	if !errors.Is(err, boom) {
		t.Fatalf("Run error = %v", err)
	}
	if got.ID != run.ID || store.calls != 1 {
		t.Fatalf("run = %+v, store calls = %d", got, store.calls)
	}
}

func TestRecordingReportsPersistenceFailure(t *testing.T) {
	persistErr := errors.New("disk full")
	inner := &fakeConnector{
		name: "imap",
		run:  domain.ConnectorRun{ID: "run-3", Source: "imap", Status: domain.ConnectorSuccess},
	}
	store := &fakeRunStore{err: persistErr}
	recorder := Recording{Inner: inner, Runs: store}

	got, err := recorder.Run(context.Background(), ports.ConnectorOptions{})
	if !errors.Is(err, persistErr) {
		t.Fatalf("Run error = %v, want persistence error", err)
	}
	if !strings.Contains(err.Error(), "record connector run") {
		t.Fatalf("error lacks context: %v", err)
	}
	if got.ID != "run-3" {
		t.Fatalf("run result lost: %+v", got)
	}
}

func TestRecordingJoinsRunAndPersistenceErrors(t *testing.T) {
	runErr := errors.New("source failed")
	persistErr := errors.New("disk full")
	inner := &fakeConnector{
		name: "github",
		run:  domain.ConnectorRun{ID: "run-4", Source: "github", Status: domain.ConnectorFailed},
		err:  runErr,
	}
	recorder := Recording{Inner: inner, Runs: &fakeRunStore{err: persistErr}}

	got, err := recorder.Run(context.Background(), ports.ConnectorOptions{})
	if !errors.Is(err, runErr) || !errors.Is(err, persistErr) {
		t.Fatalf("joined error = %v", err)
	}
	if got.ID != "run-4" {
		t.Fatalf("run = %+v", got)
	}
}

func TestRecordingPassThroughWithoutStore(t *testing.T) {
	run := domain.ConnectorRun{ID: "run-5", Source: "github", Status: domain.ConnectorSuccess}
	recorder := Recording{Inner: &fakeConnector{name: "github", run: run}}
	got, err := recorder.Run(context.Background(), ports.ConnectorOptions{})
	if err != nil || got.ID != run.ID {
		t.Fatalf("Run = (%+v, %v)", got, err)
	}
}

func TestRecordingSkipsEmptyRun(t *testing.T) {
	store := &fakeRunStore{}
	recorder := Recording{Inner: &fakeConnector{name: "github"}, Runs: store}
	if _, err := recorder.Run(context.Background(), ports.ConnectorOptions{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if store.calls != 0 {
		t.Fatalf("empty run persisted: %d", store.calls)
	}
}

func TestRecordingRequiresInner(t *testing.T) {
	if _, err := (Recording{}).Run(context.Background(), ports.ConnectorOptions{}); err == nil {
		t.Fatal("expected error without inner connector")
	}
}
