package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func baselineMetrics() Metrics {
	return Metrics{
		RecallAt5: 0.8, PrecisionAt5: 0.3, MRR: 0.7,
		SupersededTasks: 10, LeakedTasks: 0, SupersededLeakRate: 0,
		TokensPerAnswer: 100, P50LatencyMS: 4, P95LatencyMS: 10,
	}
}

func TestCompareTolerances(t *testing.T) {
	tolerance := DefaultTolerances()
	cases := map[string]struct {
		mutate func(*Metrics)
		wantOK bool
		failed string
	}{
		"unchanged": {wantOK: true},
		"recall drop within two points": {
			mutate: func(m *Metrics) { m.RecallAt5 = 0.78 }, wantOK: true,
		},
		"recall drop beyond two points": {
			mutate: func(m *Metrics) { m.RecallAt5 = 0.77 }, wantOK: false, failed: "recall@5",
		},
		"recall improvement": {
			mutate: func(m *Metrics) { m.RecallAt5 = 0.9 }, wantOK: true,
		},
		"mrr drop": {
			mutate: func(m *Metrics) { m.MRR = 0.6 }, wantOK: false, failed: "mrr",
		},
		"tokens at the tolerance edge": {
			mutate: func(m *Metrics) { m.TokensPerAnswer = 110 }, wantOK: true,
		},
		"tokens beyond tolerance": {
			mutate: func(m *Metrics) { m.TokensPerAnswer = 111 }, wantOK: false, failed: "tokens_per_answer",
		},
		"latency within the doubled budget": {
			mutate: func(m *Metrics) { m.P95LatencyMS = 25 }, wantOK: true,
		},
		"latency beyond the doubled budget": {
			mutate: func(m *Metrics) { m.P95LatencyMS = 26 }, wantOK: false, failed: "p95_latency_ms",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			baseline := Baseline{Version: ReportVersion, FixtureDigest: "digest", TaskCount: 10, Metrics: baselineMetrics()}
			current := baselineMetrics()
			if tc.mutate != nil {
				tc.mutate(&current)
			}
			report := Report{Version: ReportVersion, FixtureDigest: "digest", TaskCount: 10, Metrics: current}
			comparison := Compare(report, baseline, tolerance)
			if comparison.OK != tc.wantOK {
				t.Fatalf("OK = %v, want %v; checks = %+v", comparison.OK, tc.wantOK, comparison.Checks)
			}
			if tc.failed != "" {
				if !hasFailedCheck(comparison, tc.failed) {
					t.Fatalf("failed checks = %+v, want %s", comparison.Checks, tc.failed)
				}
			}
		})
	}
}

func TestCompareLeakHasZeroTolerance(t *testing.T) {
	baseline := Baseline{Version: ReportVersion, TaskCount: 10, Metrics: baselineMetrics()}
	current := baselineMetrics()
	current.LeakedTasks = 1
	current.SupersededLeakRate = 0.1
	comparison := Compare(Report{Version: ReportVersion, TaskCount: 10, Metrics: current}, baseline, DefaultTolerances())
	if comparison.OK {
		t.Fatal("a superseded leak increase passed the gate")
	}
	if !hasFailedCheck(comparison, "superseded_leak_rate") {
		t.Fatalf("checks = %+v", comparison.Checks)
	}
}

func TestCompareReportsFixtureDigestMismatch(t *testing.T) {
	baseline := Baseline{Version: ReportVersion, FixtureDigest: "old", TaskCount: 10, Metrics: baselineMetrics()}
	report := Report{Version: ReportVersion, FixtureDigest: "new", TaskCount: 10, Metrics: baselineMetrics()}
	comparison := Compare(report, baseline, DefaultTolerances())
	if !comparison.OK || !comparison.FixtureDigestMismatch {
		t.Fatalf("comparison = %+v, want OK with a digest warning", comparison)
	}
}

func TestSaveAndLoadBaselineRoundTrip(t *testing.T) {
	report := Report{
		Version: ReportVersion, FixtureDigest: "digest", TaskCount: 7,
		Records: 12, Metrics: baselineMetrics(),
	}
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := SaveBaseline(path, report); err != nil {
		t.Fatalf("SaveBaseline: %v", err)
	}
	baseline, err := LoadBaseline(path)
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}
	if baseline.TaskCount != 7 || baseline.FixtureDigest != "digest" || baseline.Metrics != report.Metrics {
		t.Fatalf("baseline = %+v", baseline)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"recall_at_5"`) {
		t.Fatalf("baseline JSON is not the report shape:\n%s", raw)
	}
}

func TestLoadBaselineRejectsInvalidFiles(t *testing.T) {
	valid := `{"version":1,"task_count":3,"metrics":{"recall_at_5":0.8,"precision_at_5":0.2,"mrr":0.7,"tokens_per_answer":20,"p50_latency_ms":1,"p95_latency_ms":2}}`
	cases := map[string]string{
		"bad json":                 `{`,
		"wrong version":            strings.Replace(valid, `"version":1`, `"version":9`, 1),
		"no task count":            strings.Replace(valid, `"task_count":3`, `"task_count":0`, 1),
		"recall above 1":           strings.Replace(valid, `"recall_at_5":0.8`, `"recall_at_5":1.5`, 1),
		"negative tokens":          strings.Replace(valid, `"tokens_per_answer":20`, `"tokens_per_answer":-1`, 1),
		"inconsistent leak counts": `{"version":1,"task_count":3,"metrics":{"superseded_tasks":1,"superseded_leak_tasks":2}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "baseline.json")
			if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadBaseline(path); err == nil {
				t.Fatal("LoadBaseline accepted an invalid file")
			}
		})
	}
	if _, err := LoadBaseline(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("LoadBaseline accepted a missing file")
	}
}

func hasFailedCheck(comparison Comparison, metric string) bool {
	for _, check := range comparison.Checks {
		if check.Metric == metric {
			return !check.OK
		}
	}
	return false
}
