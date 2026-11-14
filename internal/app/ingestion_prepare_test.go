package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// funcCheckpointStore delegates the checkpoint load to a test function so a
// case can block on the run context or fail with a chosen error.
type funcCheckpointStore struct {
	list func(ctx context.Context) ([]domain.Checkpoint, error)
}

func (s *funcCheckpointStore) Get(context.Context, string, string) (domain.Checkpoint, error) {
	panic("unused")
}

func (s *funcCheckpointStore) List(ctx context.Context, _ string) ([]domain.Checkpoint, error) {
	return s.list(ctx)
}

func (s *funcCheckpointStore) Put(context.Context, string, domain.Checkpoint) error { panic("unused") }

func (s *funcCheckpointStore) Delete(context.Context, string, string) error { panic("unused") }

type recordingRunStore struct{ runs []domain.ConnectorRun }

func (s *recordingRunStore) AppendConnectorRun(_ context.Context, run domain.ConnectorRun) error {
	s.runs = append(s.runs, run)
	return nil
}

func (s *recordingRunStore) ListConnectorRuns(context.Context, string, int) ([]domain.ConnectorRun, error) {
	return append([]domain.ConnectorRun(nil), s.runs...), nil
}

// A budget that ends while preparation loads checkpoints must truncate the run
// instead of reporting a failed connector: the source was never read, and the
// record is persisted like any other deadline run.
func TestIngestionBudgetEndingDuringPrepareTruncatesRun(t *testing.T) {
	connection := domain.Connection{ID: "gh-work", Provider: "sample"}
	items := &recordingItemStore{}

	cases := []struct {
		name   string
		status domain.ConnectorStatus
		reason domain.TruncationReason
		ctx    func() (context.Context, context.CancelFunc)
	}{
		{
			name:   "deadline",
			status: domain.ConnectorSuccess,
			reason: domain.TruncatedDeadline,
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 20*time.Millisecond)
			},
		},
		{
			name:   "canceled",
			status: domain.ConnectorPartial,
			reason: domain.TruncatedCanceled,
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source := &sequenceSource{descriptor: ingestionDescriptor(false)}
			ctx, cancel := testCase.ctx()
			defer cancel()
			checkpoints := &funcCheckpointStore{list: func(ctx context.Context) ([]domain.Checkpoint, error) {
				if testCase.reason == domain.TruncatedCanceled {
					cancel()
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			application := ingestionFixture(t, connection, source, items, checkpoints, nil)
			runs := &recordingRunStore{}
			application.Runs = runs

			result, err := application.RunConnection(ctx, RunConnectionOptions{ConnectionID: connection.ID})
			if err != nil {
				t.Fatalf("preparation budget returned error: %v", err)
			}
			if result.Run.Status != testCase.status || result.Run.Error != "" {
				t.Fatalf("run = %+v", result.Run)
			}
			if result.Run.Stats == nil || result.Run.Stats.Truncated != testCase.reason {
				t.Fatalf("run stats = %+v", result.Run.Stats)
			}
			if err := result.Run.Stats.Validate(); err != nil {
				t.Fatalf("run stats invalid: %v", err)
			}
			if source.reads != 0 {
				t.Fatalf("source read %d times after a preparation budget end", source.reads)
			}
			if len(runs.runs) != 1 || runs.runs[0].ID != result.Run.ID || runs.runs[0].Status != testCase.status {
				t.Fatalf("persisted runs = %+v", runs.runs)
			}
			if runs.runs[0].Stats == nil || runs.runs[0].Stats.Truncated != testCase.reason {
				t.Fatalf("persisted stats = %+v", runs.runs[0].Stats)
			}
		})
	}
}

// A preparation error that is not a context error still fails the run: a
// canceled run context must not hide a real checkpoint failure.
func TestIngestionPrepareFailureIsNotMaskedByCanceledContext(t *testing.T) {
	connection := domain.Connection{ID: "gh-work", Provider: "sample"}
	cases := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{name: "live context", ctx: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		}},
		{name: "canceled context", ctx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, func() {}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source := &sequenceSource{descriptor: ingestionDescriptor(false)}
			ctx, cancel := testCase.ctx()
			defer cancel()
			checkpoints := &funcCheckpointStore{list: func(context.Context) ([]domain.Checkpoint, error) {
				return nil, errors.New("checkpoint disk full")
			}}
			application := ingestionFixture(t, connection, source, &recordingItemStore{}, checkpoints, nil)

			result, err := application.RunConnection(ctx, RunConnectionOptions{ConnectionID: connection.ID})
			if err == nil || !strings.Contains(err.Error(), "checkpoint disk full") {
				t.Fatalf("error = %v", err)
			}
			if result.Run.Status != domain.ConnectorFailed || result.Run.Stats == nil || result.Run.Stats.Truncated != "" {
				t.Fatalf("run = %+v", result.Run)
			}
			if !strings.Contains(result.Run.Error, "checkpoint disk full") {
				t.Fatalf("run error = %q", result.Run.Error)
			}
			if source.reads != 0 {
				t.Fatalf("source read %d times after a failed preparation", source.reads)
			}
		})
	}
}
