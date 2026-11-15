package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/levapveeskela/lv24-brain/eval"
)

// registerEvalCompletion keeps the shell completion table in step with the
// command without editing completion.go (WB-1.9 scope).
func init() {
	completionTable = append(completionTable, cmdLeaf("eval",
		valueFlag("fixtures"), boolFlag("json"), valueFlag("limit"),
		valueFlag("baseline"), valueFlag("write-baseline")))
}

// jsonEval is the --json payload: the report first, then the optional gate.
type jsonEval struct {
	Fixtures     string           `json:"fixtures"`
	Report       eval.Report      `json:"report"`
	BaselinePath string           `json:"baseline_path,omitempty"`
	Baseline     *eval.Comparison `json:"baseline,omitempty"`
}

// runEval scores the retrieval fixtures and optionally gates against a
// baseline. A metric regression exits 1; a fixture or setup error also exits 1.
func runEval(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("eval", stderr)
	fixtures := fs.String("fixtures", "eval/fixtures", "directory of YAML retrieval fixtures")
	asJSON := fs.Bool("json", false, "print JSON")
	limit := fs.Int("limit", 0, "run only the first N tasks in fixture order")
	baselinePath := fs.String("baseline", "", "baseline JSON to compare against; exit 1 on regression")
	writeBaseline := fs.String("write-baseline", "", "write this run as a baseline JSON file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain eval: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if *limit < 0 {
		fmt.Fprintln(stderr, "brain eval: --limit must not be negative")
		return 2
	}
	if *baselinePath != "" && *writeBaseline != "" {
		fmt.Fprintln(stderr, "brain eval: --baseline and --write-baseline cannot be combined")
		return 2
	}
	if *limit > 0 && (*baselinePath != "" || *writeBaseline != "") {
		fmt.Fprintln(stderr, "brain eval: --limit cannot be combined with a baseline; a baseline must cover every task")
		return 2
	}

	corpus, err := eval.LoadDir(*fixtures)
	if err != nil {
		return reportError(stderr, "eval", err)
	}
	report, err := eval.Run(context.Background(), corpus, eval.RunOptions{Limit: *limit})
	if err != nil {
		return reportError(stderr, "eval", err)
	}

	var comparison *eval.Comparison
	if *baselinePath != "" {
		baseline, loadErr := eval.LoadBaseline(*baselinePath)
		if loadErr != nil {
			return reportError(stderr, "eval", loadErr)
		}
		gate := eval.Compare(report, baseline, eval.DefaultTolerances())
		comparison = &gate
	}
	if *writeBaseline != "" {
		if err := eval.SaveBaseline(*writeBaseline, report); err != nil {
			return reportError(stderr, "eval", err)
		}
		// Keep stdout machine-readable under --json.
		fmt.Fprintf(stderr, "brain eval: baseline written: %s\n", *writeBaseline)
	}

	if *asJSON {
		payload := jsonEval{Fixtures: *fixtures, Report: report}
		if comparison != nil {
			payload.BaselinePath = *baselinePath
			payload.Baseline = comparison
		}
		if code := writeJSONReport(stdout, stderr, "eval", payload); code != 0 {
			return code
		}
		if comparison == nil || comparison.OK {
			return 0
		}
		return evalRegression(stderr, *baselinePath, *comparison)
	}

	printEvalReport(stdout, report)
	if comparison == nil {
		return 0
	}
	printEvalComparison(stdout, *baselinePath, *comparison)
	if !comparison.OK {
		return evalRegression(stderr, *baselinePath, *comparison)
	}
	return 0
}

func printEvalReport(w io.Writer, report eval.Report) {
	metrics := report.Metrics
	fmt.Fprintf(w, "fixtures: %d records, %d tasks (%d run, top-%d)\n",
		report.Records, report.TaskCount, report.TasksRun, report.K)
	fmt.Fprintf(w, "  recall@%d          %.3f\n", report.K, metrics.RecallAt5)
	fmt.Fprintf(w, "  precision@%d       %.3f\n", report.K, metrics.PrecisionAt5)
	fmt.Fprintf(w, "  MRR                %.3f\n", metrics.MRR)
	fmt.Fprintf(w, "  superseded leak    %.3f (%d/%d tasks)\n",
		metrics.SupersededLeakRate, metrics.LeakedTasks, metrics.SupersededTasks)
	fmt.Fprintf(w, "  tokens/answer      %.1f\n", metrics.TokensPerAnswer)
	fmt.Fprintf(w, "  p95 latency        %.2f ms (p50 %.2f ms)\n", metrics.P95LatencyMS, metrics.P50LatencyMS)
	if len(report.Categories) == 0 {
		return
	}
	fmt.Fprintln(w)
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "category\ttasks\trecall@"+fmt.Sprint(report.K)+"\tMRR\tleak\ttokens")
	for _, row := range report.Categories {
		fmt.Fprintf(table, "%s\t%d\t%.3f\t%.3f\t%.3f\t%.1f\n",
			row.Category, row.Tasks, row.Metrics.RecallAt5, row.Metrics.MRR,
			row.Metrics.SupersededLeakRate, row.Metrics.TokensPerAnswer)
	}
	_ = table.Flush()
}

func printEvalComparison(w io.Writer, path string, comparison eval.Comparison) {
	fmt.Fprintf(w, "\nbaseline %s\n", path)
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "metric\tbaseline\tcurrent\tallowed\tstatus")
	for _, check := range comparison.Checks {
		status := "ok"
		if !check.OK {
			status = "REGRESSED"
		}
		fmt.Fprintf(table, "%s\t%.3f\t%.3f\t%.3f\t%s\n",
			check.Metric, check.Baseline, check.Current, check.Allowed, status)
	}
	_ = table.Flush()
	if comparison.FixtureDigestMismatch {
		fmt.Fprintln(w, "warning: baseline was produced from different fixtures")
	}
}

// evalRegression reports every failed check on stderr and returns exit 1.
func evalRegression(stderr io.Writer, path string, comparison eval.Comparison) int {
	failed := 0
	for _, check := range comparison.Checks {
		if check.OK {
			continue
		}
		failed++
		fmt.Fprintf(stderr, "brain eval: regression %s: baseline %.3f, current %.3f, allowed %.3f\n",
			check.Metric, check.Baseline, check.Current, check.Allowed)
	}
	fmt.Fprintf(stderr, "brain eval: %d metric(s) regressed against %s\n", failed, path)
	return 1
}
