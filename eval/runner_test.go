package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"testing"
)

func TestRunComputesKnownMetrics(t *testing.T) {
	corpus := memoryCorpus(
		[]Record{
			{ID: "decision-alpha", Kind: KindDecision, Title: "Alpha deploy pipeline", CreatedAt: "2026-01-01T00:00:00Z", Context: "alpha deploy pipeline retry"},
			{ID: "decision-beta", Kind: KindDecision, Title: "Beta sqlite index tuning", CreatedAt: "2026-01-02T00:00:00Z", Context: "beta sqlite index tuning"},
			{ID: "note-gamma", Kind: KindNote, Title: "Gamma desktop theme", CreatedAt: "2026-01-03T00:00:00Z", Body: "gamma desktop theme colors"},
		},
		[]Task{
			{ID: "t-alpha", Query: "alpha deploy pipeline", Expected: []string{"decision-alpha"}},
			{ID: "t-beta", Query: "sqlite index tuning", Expected: []string{"decision-beta"}},
			{ID: "t-miss", Query: "quantum entanglement", Expected: []string{"decision-alpha"}},
		},
	)
	report, err := Run(context.Background(), corpus, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.TasksRun != 3 || report.TaskCount != 3 || report.Records != 3 {
		t.Fatalf("report counts = %+v", report)
	}
	assertClose(t, "recall@5", report.Metrics.RecallAt5, 2.0/3)
	assertClose(t, "precision@5", report.Metrics.PrecisionAt5, 2.0/15)
	assertClose(t, "MRR", report.Metrics.MRR, 2.0/3)
	assertClose(t, "leak rate", report.Metrics.SupersededLeakRate, 0)
	if report.Metrics.TokensPerAnswer <= 0 {
		t.Fatalf("tokens/answer = %v, want a positive answer pack", report.Metrics.TokensPerAnswer)
	}
	if report.Metrics.P95LatencyMS < 0 {
		t.Fatalf("p95 latency = %v", report.Metrics.P95LatencyMS)
	}
	if len(report.Categories) != 1 || report.Categories[0].Category != DefaultCategory {
		t.Fatalf("categories = %+v", report.Categories)
	}
	miss := report.Tasks[2]
	if miss.FirstExpectedRank != 0 || len(miss.Hits) != 0 {
		t.Fatalf("miss task = %+v, want no hits", miss)
	}
}

func TestRunCountsForbiddenHitsAsLeak(t *testing.T) {
	corpus := memoryCorpus(
		[]Record{
			{ID: "decision-current", Kind: KindDecision, Title: "Widget rollout current", CreatedAt: "2026-02-01T00:00:00Z", Context: "widget rollout plan"},
			{ID: "decision-obsolete", Kind: KindDecision, Title: "Widget rollout legacy", CreatedAt: "2026-01-01T00:00:00Z", Context: "widget rollout plan legacy"},
		},
		[]Task{{
			ID: "t-current", Query: "widget rollout", Expected: []string{"decision-current"},
			Forbidden: []string{"decision-obsolete"},
		}},
	)
	report, err := Run(context.Background(), corpus, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Metrics.SupersededTasks != 1 || report.Metrics.LeakedTasks != 1 {
		t.Fatalf("leak counts = %d/%d, want 1/1", report.Metrics.LeakedTasks, report.Metrics.SupersededTasks)
	}
	assertClose(t, "leak rate", report.Metrics.SupersededLeakRate, 1)
	if !contains(report.Tasks[0].Leaked, "decision-obsolete") {
		t.Fatalf("leaked = %v, want decision-obsolete", report.Tasks[0].Leaked)
	}
}

// TestTemporalCurrentAndAsOf checks the wiring between fixtures and the
// validity-aware search: a superseded record must not appear for current state
// and must appear for an as-of instant inside its validity window.
func TestTemporalCurrentAndAsOf(t *testing.T) {
	corpus := memoryCorpus(
		[]Record{
			{
				ID: "decision-old", Kind: KindDecision, Title: "Sync cron pull", Status: "superseded",
				CreatedAt: "2026-01-01T00:00:00Z", ValidTo: "2026-10-01T00:00:00Z",
				SupersededBy: "decision-new", Context: "sync pull cron loop",
			},
			{
				ID: "decision-new", Kind: KindDecision, Title: "Sync bounded rebase pull",
				CreatedAt: "2026-09-01T00:00:00Z", Supersedes: "decision-old",
				Context: "sync pull bounded rebase",
			},
		},
		[]Task{
			{ID: "t-current", Query: "sync pull", Expected: []string{"decision-new"}, Forbidden: []string{"decision-old"}},
			{ID: "t-asof", Query: "sync pull", AsOf: "2026-06-01T00:00:00Z", Expected: []string{"decision-old"}, Forbidden: []string{"decision-new"}},
		},
	)
	report, err := Run(context.Background(), corpus, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	current, asOf := report.Tasks[0], report.Tasks[1]
	if current.FirstExpectedRank != 1 || len(current.Leaked) != 0 {
		t.Fatalf("current state = %+v, want the new record only", current)
	}
	if asOf.FirstExpectedRank != 1 || len(asOf.Leaked) != 0 {
		t.Fatalf("as-of state = %+v, want the old record only", asOf)
	}
	assertClose(t, "leak rate", report.Metrics.SupersededLeakRate, 0)
}

func TestRunIsDeterministicExceptLatency(t *testing.T) {
	corpus := memoryCorpus(
		[]Record{
			{ID: "decision-alpha", Kind: KindDecision, Title: "Alpha deploy pipeline", CreatedAt: "2026-01-01T00:00:00Z", Context: "alpha deploy pipeline"},
			{ID: "note-beta", Kind: KindNote, Title: "Beta notes", CreatedAt: "2026-01-02T00:00:00Z", Body: "beta notes"},
		},
		[]Task{
			{ID: "t-alpha", Query: "alpha deploy", Expected: []string{"decision-alpha"}},
			{ID: "t-beta", Query: "beta notes", Expected: []string{"note-beta"}},
		},
	)
	first, err := Run(context.Background(), corpus, RunOptions{})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	second, err := Run(context.Background(), corpus, RunOptions{})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !bytes.Equal(reportWithoutLatency(t, first), reportWithoutLatency(t, second)) {
		t.Fatal("two runs of the same fixtures differ in ranking-derived metrics")
	}
}

func TestRunLimitRunsAPrefix(t *testing.T) {
	corpus := memoryCorpus(
		[]Record{{ID: "decision-alpha", Kind: KindDecision, Title: "Alpha", CreatedAt: "2026-01-01T00:00:00Z"}},
		[]Task{
			{ID: "t-one", Query: "alpha", Expected: []string{"decision-alpha"}},
			{ID: "t-two", Query: "alpha", Expected: []string{"decision-alpha"}},
		},
	)
	report, err := Run(context.Background(), corpus, RunOptions{Limit: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.TasksRun != 1 || report.TaskCount != 2 || len(report.Tasks) != 1 {
		t.Fatalf("limited report = %+v", report)
	}
	if _, err := Run(context.Background(), corpus, RunOptions{Limit: -1}); err == nil {
		t.Fatal("Run accepted a negative limit")
	}
}

func memoryCorpus(records []Record, tasks []Task) Corpus {
	return Corpus{
		Fixtures:  []Fixture{{Version: FixtureVersion, Project: "demo", Records: records, Tasks: tasks}},
		TaskCount: len(tasks), RecordCount: len(records),
	}
}

func reportWithoutLatency(t *testing.T, report Report) []byte {
	t.Helper()
	report.Metrics.P50LatencyMS = 0
	report.Metrics.P95LatencyMS = 0
	for index := range report.Tasks {
		report.Tasks[index].LatencyMS = 0
	}
	for index := range report.Categories {
		report.Categories[index].Metrics.P50LatencyMS = 0
		report.Categories[index].Metrics.P95LatencyMS = 0
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	return raw
}

func assertClose(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}
