package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cliEvalFixture = `version: 1
project: demo
records:
  - id: decision-2026-01-01-alpha
    kind: decision
    title: "Alpha widget rollout"
    status: accepted
    created_at: 2026-01-01T00:00:00Z
    context: "The alpha widget deploy pipeline."
tasks:
  - id: t-alpha
    query: "%s"
    expected: [decision-2026-01-01-alpha]
`

type cliEvalReport struct {
	Fixtures string `json:"fixtures"`
	Report   struct {
		TasksRun int `json:"tasks_run"`
		Metrics  struct {
			RecallAt5 float64 `json:"recall_at_5"`
		} `json:"metrics"`
	} `json:"report"`
	Baseline *struct {
		OK bool `json:"ok"`
	} `json:"baseline"`
}

func TestEvalJSONReportsMetrics(t *testing.T) {
	dir := writeEvalFixture(t, "alpha widget")
	code, stdout, stderr := runForTest(t, "eval", "--fixtures", dir, "--json")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	var envelope struct {
		Schema int           `json:"schema"`
		Data   cliEvalReport `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if envelope.Schema != 1 || envelope.Data.Report.TasksRun != 1 {
		t.Fatalf("envelope = %+v", envelope)
	}
	if envelope.Data.Report.Metrics.RecallAt5 != 1 {
		t.Fatalf("recall@5 = %v, want 1", envelope.Data.Report.Metrics.RecallAt5)
	}
	if envelope.Data.Baseline != nil {
		t.Fatalf("baseline = %+v, want none", envelope.Data.Baseline)
	}
}

func TestEvalLimitRunsAPrefix(t *testing.T) {
	dir := writeEvalFixture(t, "alpha widget")
	code, _, stderr := runForTest(t, "eval", "--fixtures", dir, "--limit", "1", "--json")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
}

func TestEvalUsageErrors(t *testing.T) {
	dir := writeEvalFixture(t, "alpha widget")
	baseline := filepath.Join(t.TempDir(), "baseline.json")
	cases := [][]string{
		{"eval", "--fixtures", dir, "--limit", "-1"},
		{"eval", "--fixtures", dir, "--baseline", baseline, "--write-baseline", baseline},
		{"eval", "--fixtures", dir, "--limit", "1", "--baseline", baseline},
		{"eval", "--fixtures", dir, "unexpected"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			code, _, stderr := runForTest(t, args...)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr)
			}
		})
	}
	if code, _, _ := runForTest(t, "eval", "--fixtures", filepath.Join(t.TempDir(), "missing")); code != 1 {
		t.Fatalf("missing fixtures exit code = %d, want 1", code)
	}
}

func TestEvalBaselineRegressionExitsOne(t *testing.T) {
	dir := writeEvalFixture(t, "quantum entanglement")
	baseline := filepath.Join(t.TempDir(), "baseline.json")
	raw := `{"version":1,"task_count":1,"metrics":{"recall_at_5":1,"precision_at_5":0.2,"mrr":1,"superseded_leak_rate":0,"tokens_per_answer":5,"p50_latency_ms":1,"p95_latency_ms":2}}`
	if err := os.WriteFile(baseline, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runForTest(t, "eval", "--fixtures", dir, "--baseline", baseline)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 for a regressed recall (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "regression recall@5") {
		t.Fatalf("stderr does not name the regressed metric: %s", stderr)
	}
}

func TestEvalImprovedBaselinePasses(t *testing.T) {
	dir := writeEvalFixture(t, "alpha widget")
	baseline := filepath.Join(t.TempDir(), "baseline.json")
	raw := `{"version":1,"task_count":1,"metrics":{"recall_at_5":0,"precision_at_5":0,"mrr":0,"superseded_leak_rate":0,"tokens_per_answer":20,"p50_latency_ms":0,"p95_latency_ms":0}}`
	if err := os.WriteFile(baseline, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runForTest(t, "eval", "--fixtures", dir, "--baseline", baseline, "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 for an improved run (stderr: %s)", code, stderr)
	}
}

func TestEvalWritesBaseline(t *testing.T) {
	dir := writeEvalFixture(t, "alpha widget")
	baseline := filepath.Join(t.TempDir(), "nested", "baseline.json")
	code, stdout, stderr := runForTest(t, "eval", "--fixtures", dir, "--write-baseline", baseline)
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "baseline written") {
		t.Fatalf("stderr = %q, want a baseline notice", stderr)
	}
	if !strings.Contains(stdout, "recall@5") {
		t.Fatalf("stdout does not contain the metrics table:\n%s", stdout)
	}
	raw, err := os.ReadFile(baseline)
	if err != nil {
		t.Fatalf("baseline file: %v", err)
	}
	if !strings.Contains(string(raw), `"task_count": 1`) {
		t.Fatalf("baseline content = %s", raw)
	}
}

func writeEvalFixture(t *testing.T, query string) string {
	t.Helper()
	dir := t.TempDir()
	content := strings.Replace(cliEvalFixture, "%s", query, 1)
	if err := os.WriteFile(filepath.Join(dir, "demo.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir
}
