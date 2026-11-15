package eval

import (
	"context"
	"testing"
)

// TestCommittedFixtures runs the corpus that ships with the repository. It
// guards the hard invariants (no forbidden leak, every task labelled) and keeps
// the committed baseline in sync with the fixture digest. Latency is copied
// from the baseline before comparing because it is machine-relative and the
// gate is the non-blocking CI job documented in docs/eval.md.
func TestCommittedFixtures(t *testing.T) {
	corpus, err := LoadDir("fixtures")
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	report, err := Run(context.Background(), corpus, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.TasksRun != report.TaskCount || report.TaskCount < 50 {
		t.Fatalf("committed corpus has %d tasks (%d run), want at least 50 all run", report.TaskCount, report.TasksRun)
	}
	if report.Metrics.SupersededTasks < 10 || report.Metrics.LeakedTasks != 0 || report.Metrics.SupersededLeakRate != 0 {
		t.Fatalf("leak metrics = %+v, want at least 10 forbidden tasks and zero leaks", report.Metrics)
	}
	if report.Metrics.RecallAt5 < 0.9 {
		t.Fatalf("recall@5 = %v; the authored corpus must stay useful", report.Metrics.RecallAt5)
	}
	categories := make(map[string]bool, len(report.Categories))
	for _, row := range report.Categories {
		categories[row.Category] = true
	}
	for _, want := range []string{"identifier", "morphology", "paraphrase", "staleness", "as_of", "temporal"} {
		if !categories[want] {
			t.Fatalf("category %q is missing from the committed corpus", want)
		}
	}

	baseline, err := LoadBaseline("baseline.json")
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}
	if baseline.TaskCount != report.TaskCount {
		t.Fatalf("baseline has %d tasks, fixtures have %d; regenerate with --write-baseline", baseline.TaskCount, report.TaskCount)
	}
	comparable := report
	comparable.Metrics.P50LatencyMS = baseline.Metrics.P50LatencyMS
	comparable.Metrics.P95LatencyMS = baseline.Metrics.P95LatencyMS
	comparison := Compare(comparable, baseline, DefaultTolerances())
	if comparison.FixtureDigestMismatch {
		t.Fatal("baseline fixture digest differs; regenerate it with --write-baseline after reviewing the label change")
	}
	if !comparison.OK {
		for _, check := range comparison.Checks {
			if !check.OK {
				t.Errorf("regressed %s: baseline %v, current %v", check.Metric, check.Baseline, check.Current)
			}
		}
		t.Fatal("committed fixtures regress against baseline.json")
	}
}
