package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// Baseline is the committed metrics snapshot a run is compared against. It
// deliberately stores no paths and no per-task detail so it stays stable in
// review.
type Baseline struct {
	Version       int     `json:"version"`
	FixtureDigest string  `json:"fixture_digest,omitempty"`
	TaskCount     int     `json:"task_count"`
	Metrics       Metrics `json:"metrics"`
}

// NewBaseline captures a report as a baseline.
func NewBaseline(report Report) Baseline {
	return Baseline{
		Version: ReportVersion, FixtureDigest: report.FixtureDigest,
		TaskCount: report.TaskCount, Metrics: report.Metrics,
	}
}

// SaveBaseline writes report as an indented JSON baseline file.
func SaveBaseline(path string, report Report) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("eval: create baseline directory: %w", err)
		}
	}
	raw, err := json.MarshalIndent(NewBaseline(report), "", "  ")
	if err != nil {
		return fmt.Errorf("eval: encode baseline: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("eval: write baseline: %w", err)
	}
	return nil
}

// LoadBaseline reads and sanity-checks a baseline file.
func LoadBaseline(path string) (Baseline, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Baseline{}, fmt.Errorf("eval: read baseline: %w", err)
	}
	var baseline Baseline
	if err := json.Unmarshal(raw, &baseline); err != nil {
		return Baseline{}, fmt.Errorf("eval: decode baseline %s: %w", path, err)
	}
	if baseline.Version != ReportVersion {
		return Baseline{}, fmt.Errorf("eval: baseline version = %d, want %d", baseline.Version, ReportVersion)
	}
	if baseline.TaskCount <= 0 {
		return Baseline{}, errors.New("eval: baseline has no task count")
	}
	if err := validateMetrics(baseline.Metrics); err != nil {
		return Baseline{}, fmt.Errorf("eval: baseline %s: %w", path, err)
	}
	return baseline, nil
}

func validateMetrics(metrics Metrics) error {
	for name, value := range map[string]float64{
		"recall_at_5": metrics.RecallAt5, "precision_at_5": metrics.PrecisionAt5,
		"mrr": metrics.MRR, "superseded_leak_rate": metrics.SupersededLeakRate,
		"tokens_per_answer": metrics.TokensPerAnswer,
		"p50_latency_ms":    metrics.P50LatencyMS, "p95_latency_ms": metrics.P95LatencyMS,
	} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("%s is not a finite number", name)
		}
		if value < 0 {
			return fmt.Errorf("%s is negative", name)
		}
	}
	for name, value := range map[string]float64{
		"recall_at_5": metrics.RecallAt5, "precision_at_5": metrics.PrecisionAt5,
		"mrr": metrics.MRR, "superseded_leak_rate": metrics.SupersededLeakRate,
	} {
		if value > 1 {
			return fmt.Errorf("%s = %v is above 1", name, value)
		}
	}
	if metrics.SupersededTasks < 0 || metrics.LeakedTasks < 0 ||
		metrics.LeakedTasks > metrics.SupersededTasks {
		return errors.New("superseded leak counts are inconsistent")
	}
	return nil
}

// Direction says which way a metric is allowed to move.
const (
	HigherIsBetter = "higher_is_better"
	LowerIsBetter  = "lower_is_better"
)

// Tolerances are the allowed worsening of each metric against a baseline.
// Rates are absolute; tokens are relative; latency is relative plus a floor so
// a fast run does not create an impossible baseline.
type Tolerances struct {
	Rate           float64
	Tokens         float64
	LatencyFactor  float64
	LatencyFloorMS float64
}

// DefaultTolerances is the committed policy: a two-point rate drop is noise, a
// superseded leak may never rise, and latency may double plus 5 ms.
func DefaultTolerances() Tolerances {
	return Tolerances{Rate: 0.02, Tokens: 0.10, LatencyFactor: 2, LatencyFloorMS: 5}
}

// Check is one metric compared with its baseline.
type Check struct {
	Metric    string  `json:"metric"`
	Direction string  `json:"direction"`
	Baseline  float64 `json:"baseline"`
	Current   float64 `json:"current"`
	// Allowed is the permitted worsening in the metric's own units.
	Allowed float64 `json:"allowed"`
	OK      bool    `json:"ok"`
}

// Comparison is the baseline gate result.
type Comparison struct {
	OK bool `json:"ok"`
	// FixtureDigestMismatch reports that the baseline was produced from
	// different fixture bytes. The gate still runs; the caller warns.
	FixtureDigestMismatch bool    `json:"fixture_digest_mismatch"`
	Checks                []Check `json:"checks"`
}

// Compare applies the tolerances. A superseded leak has zero tolerance: it is
// a correctness regression, not a quality trade-off.
func Compare(report Report, baseline Baseline, tol Tolerances) Comparison {
	comparison := Comparison{
		OK:                    true,
		FixtureDigestMismatch: baseline.FixtureDigest != "" && report.FixtureDigest != "" && baseline.FixtureDigest != report.FixtureDigest,
	}
	current := report.Metrics
	add := func(check Check) {
		if !check.OK {
			comparison.OK = false
		}
		comparison.Checks = append(comparison.Checks, check)
	}
	add(higher("recall@5", baseline.Metrics.RecallAt5, current.RecallAt5, tol.Rate))
	add(higher("precision@5", baseline.Metrics.PrecisionAt5, current.PrecisionAt5, tol.Rate))
	add(higher("mrr", baseline.Metrics.MRR, current.MRR, tol.Rate))
	add(lower("superseded_leak_rate", baseline.Metrics.SupersededLeakRate, current.SupersededLeakRate, 0))
	add(lower("tokens_per_answer", baseline.Metrics.TokensPerAnswer, current.TokensPerAnswer, baseline.Metrics.TokensPerAnswer*tol.Tokens))
	add(lower("p95_latency_ms", baseline.Metrics.P95LatencyMS, current.P95LatencyMS,
		baseline.Metrics.P95LatencyMS*(tol.LatencyFactor-1)+tol.LatencyFloorMS))
	return comparison
}

func higher(metric string, baseline, current, allowed float64) Check {
	return Check{
		Metric: metric, Direction: HigherIsBetter, Baseline: baseline,
		Current: current, Allowed: allowed, OK: current >= baseline-allowed,
	}
}

func lower(metric string, baseline, current, allowed float64) Check {
	return Check{
		Metric: metric, Direction: LowerIsBetter, Baseline: baseline,
		Current: current, Allowed: allowed, OK: current <= baseline+allowed,
	}
}
