package eval

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const (
	// ReportVersion is the shape of a report and baseline file.
	ReportVersion = 1
	// K is the ranking cutoff for recall, precision and leak checks.
	K = 5
)

// RunOptions controls one evaluation run.
type RunOptions struct {
	// Limit runs only the first N tasks in fixture order; zero runs all.
	Limit int
}

// Metrics are the aggregated scores of a run. Latency is the only field that
// is not reproducible: it measures the machine, not the ranking.
type Metrics struct {
	RecallAt5       float64 `json:"recall_at_5"`
	PrecisionAt5    float64 `json:"precision_at_5"`
	MRR             float64 `json:"mrr"`
	TokensPerAnswer float64 `json:"tokens_per_answer"`
	P50LatencyMS    float64 `json:"p50_latency_ms"`
	P95LatencyMS    float64 `json:"p95_latency_ms"`
	// SupersededTasks counts tasks that declare forbidden ids; LeakedTasks
	// counts those that returned at least one forbidden id.
	SupersededTasks    int     `json:"superseded_tasks"`
	LeakedTasks        int     `json:"superseded_leak_tasks"`
	SupersededLeakRate float64 `json:"superseded_leak_rate"`
}

// TaskResult is the scored outcome of one query.
type TaskResult struct {
	ID        string   `json:"id"`
	Category  string   `json:"category"`
	Query     string   `json:"query"`
	Expected  []string `json:"expected"`
	Forbidden []string `json:"forbidden,omitempty"`
	// Hits are the top-K record ids in rank order, without the project prefix.
	Hits []string `json:"hits"`
	// FirstExpectedRank is 1-based, or 0 when no expected record was found.
	FirstExpectedRank int      `json:"first_expected_rank"`
	Leaked            []string `json:"leaked,omitempty"`
	Tokens            int      `json:"tokens"`
	LatencyMS         float64  `json:"latency_ms"`
}

// CategoryRow is one compact table row.
type CategoryRow struct {
	Category string  `json:"category"`
	Tasks    int     `json:"tasks"`
	Metrics  Metrics `json:"metrics"`
}

// Report is the complete run result.
type Report struct {
	Version       int           `json:"version"`
	FixtureDigest string        `json:"fixture_digest"`
	Records       int           `json:"records"`
	TaskCount     int           `json:"task_count"`
	TasksRun      int           `json:"tasks_run"`
	K             int           `json:"k"`
	Metrics       Metrics       `json:"metrics"`
	Categories    []CategoryRow `json:"categories"`
	Tasks         []TaskResult  `json:"tasks"`
}

// Run builds a temporary brain from the corpus, reindexes it through the public
// app API and scores every selected task. The temporary directory is removed
// before Run returns.
func Run(ctx context.Context, corpus Corpus, opts RunOptions) (Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := corpus.Validate(); err != nil {
		return Report{}, err
	}
	if opts.Limit < 0 {
		return Report{}, fmt.Errorf("eval: limit must not be negative")
	}
	selected, err := selectTasks(corpus, opts.Limit)
	if err != nil {
		return Report{}, err
	}
	recordCount, taskCount := 0, 0
	for index := range corpus.Fixtures {
		recordCount += len(corpus.Fixtures[index].Records)
		taskCount += len(corpus.Fixtures[index].Tasks)
	}

	dataDir, err := os.MkdirTemp("", "brain-eval-")
	if err != nil {
		return Report{}, fmt.Errorf("eval: create data dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dataDir) }()

	store, err := fsstore.New(dataDir)
	if err != nil {
		return Report{}, fmt.Errorf("eval: open store: %w", err)
	}
	want := map[string]int{}
	for index := range corpus.Fixtures {
		fixture := &corpus.Fixtures[index]
		project := strings.TrimSpace(fixture.Project)
		if err := store.PutProject(ctx, domain.Project{
			ID: project, Name: project, Kind: domain.ProjectKindOther,
		}); err != nil {
			return Report{}, fmt.Errorf("eval: register project %s: %w", project, err)
		}
		for _, record := range fixture.Records {
			if err := appendRecord(ctx, store, project, record); err != nil {
				return Report{}, fmt.Errorf("eval: project %s: %w", project, err)
			}
			want[record.Kind]++
		}
	}

	index, err := sqliteindex.Open(filepath.Join(dataDir, "index", "brain.db"))
	if err != nil {
		return Report{}, fmt.Errorf("eval: open index: %w", err)
	}
	defer func() { _ = index.Close() }()

	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = dataDir
	useCase := app.New(cfg, app.Dependencies{Store: store, Index: index})
	indexed, err := useCase.Reindex(ctx, app.IndexOptions{})
	if err != nil {
		return Report{}, fmt.Errorf("eval: reindex: %w", err)
	}
	if err := checkIndexed(indexed, want); err != nil {
		return Report{}, err
	}

	// One warm-up query keeps the first measured task from paying the SQLite
	// page-cache cost alone.
	if len(selected) > 0 {
		_, _, _ = useCase.SearchWithNotice(ctx, app.SearchOptions{
			Text: selected[0].Task.Query, ProjectID: selected[0].Project, Limit: K,
		})
	}

	report := Report{
		Version: ReportVersion, FixtureDigest: corpus.Digest,
		Records: recordCount, TaskCount: taskCount,
		TasksRun: len(selected), K: K,
		Tasks: make([]TaskResult, 0, len(selected)),
	}
	for _, choice := range selected {
		result, err := runTask(ctx, useCase, choice)
		if err != nil {
			return Report{}, err
		}
		report.Tasks = append(report.Tasks, result)
	}
	report.Metrics = summarize(report.Tasks)
	report.Categories = categorize(report.Tasks)
	return report, nil
}

type taskChoice struct {
	Project string
	Task    Task
}

func selectTasks(corpus Corpus, limit int) ([]taskChoice, error) {
	selected := make([]taskChoice, 0, corpus.TaskCount)
	for index := range corpus.Fixtures {
		fixture := &corpus.Fixtures[index]
		for _, task := range fixture.Tasks {
			selected = append(selected, taskChoice{Project: strings.TrimSpace(fixture.Project), Task: task})
			if limit > 0 && len(selected) == limit {
				return selected, nil
			}
		}
	}
	return selected, nil
}

func runTask(ctx context.Context, useCase *app.App, choice taskChoice) (TaskResult, error) {
	task := choice.Task
	asOf, err := task.AsOfTime()
	if err != nil {
		return TaskResult{}, fmt.Errorf("eval: task %q: %w", task.ID, err)
	}
	started := time.Now()
	hits, _, err := useCase.SearchWithNotice(ctx, app.SearchOptions{
		Text: task.Query, ProjectID: choice.Project, Limit: K, AsOf: asOf,
	})
	elapsed := time.Since(started)
	if err != nil {
		return TaskResult{}, fmt.Errorf("eval: task %q: %w", task.ID, err)
	}
	result := TaskResult{
		ID: task.ID, Category: task.CategoryName(), Query: task.Query,
		Expected: nonNil(task.Expected), Forbidden: nonNil(task.Forbidden),
		Hits:      make([]string, 0, len(hits)),
		LatencyMS: float64(elapsed) / float64(time.Millisecond),
	}
	for _, hit := range hits {
		id := trimProjectPrefix(hit.ID, choice.Project)
		result.Hits = append(result.Hits, id)
		result.Tokens += estimateTokens(hit.Title) + estimateTokens(hit.Snippet)
	}
	for index, id := range result.Hits {
		if contains(task.Expected, id) {
			result.FirstExpectedRank = index + 1
			break
		}
	}
	for _, id := range result.Hits {
		if contains(task.Forbidden, id) {
			result.Leaked = append(result.Leaked, id)
		}
	}
	return result, nil
}

func appendRecord(ctx context.Context, store *fsstore.Store, project string, record Record) error {
	createdAt, err := record.CreatedTime()
	if err != nil {
		return fmt.Errorf("record %s: %w", record.ID, err)
	}
	validity, err := record.Validity()
	if err != nil {
		return fmt.Errorf("record %s: %w", record.ID, err)
	}
	switch record.Kind {
	case KindDecision:
		_, err = store.AppendDecision(ctx, domain.Decision{
			ID: record.ID, ProjectID: project, Title: record.Title,
			Status: record.DecisionStatus(), Context: record.Context,
			Alternatives: record.Alternatives, Consequences: record.Consequences,
			Evidence: record.Evidence, Paths: record.Paths,
			Validity: validity, CreatedAt: createdAt,
		})
	case KindAttempt:
		_, err = store.AppendAttempt(ctx, domain.Attempt{
			ID: record.ID, ProjectID: project, Title: record.Title,
			Hypothesis: record.Hypothesis, Result: record.Result,
			Reason: record.Reason, Validity: validity, CreatedAt: createdAt,
		})
	case KindNote:
		_, err = store.AppendNote(ctx, domain.Note{
			ID: record.ID, ProjectID: project, Title: record.Title,
			Body: record.Body, Tags: record.Tags,
			Validity: validity, CreatedAt: createdAt,
		})
	default:
		return fmt.Errorf("record %s: unknown kind %q", record.ID, record.Kind)
	}
	if err != nil {
		return fmt.Errorf("record %s: %w", record.ID, err)
	}
	return nil
}

// checkIndexed fails when a fixture record did not reach the index. A silent
// skip would turn a broken fixture into a permanent, unexplained miss.
func checkIndexed(indexed app.IndexResult, want map[string]int) error {
	got := map[string]int{
		KindDecision: indexed.Decisions,
		KindAttempt:  indexed.Attempts,
		KindNote:     indexed.Notes,
	}
	for kind, count := range want {
		if got[kind] != count {
			return fmt.Errorf("eval: indexed %d %s records, fixtures have %d", got[kind], kind, count)
		}
	}
	return nil
}

func summarize(results []TaskResult) Metrics {
	metrics := Metrics{}
	if len(results) == 0 {
		return metrics
	}
	latencies := make([]float64, 0, len(results))
	totalTokens := 0
	for _, result := range results {
		latencies = append(latencies, result.LatencyMS)
		totalTokens += result.Tokens
		if result.FirstExpectedRank > 0 {
			metrics.RecallAt5++
			metrics.MRR += 1 / float64(result.FirstExpectedRank)
		}
		found := 0
		for _, id := range result.Expected {
			if contains(result.Hits, id) {
				found++
			}
		}
		metrics.PrecisionAt5 += float64(found) / float64(K)
		if len(result.Forbidden) > 0 {
			metrics.SupersededTasks++
			if len(result.Leaked) > 0 {
				metrics.LeakedTasks++
			}
		}
	}
	tasks := float64(len(results))
	metrics.RecallAt5 /= tasks
	metrics.PrecisionAt5 /= tasks
	metrics.MRR /= tasks
	metrics.TokensPerAnswer = float64(totalTokens) / tasks
	if metrics.SupersededTasks > 0 {
		metrics.SupersededLeakRate = float64(metrics.LeakedTasks) / float64(metrics.SupersededTasks)
	}
	sort.Float64s(latencies)
	metrics.P50LatencyMS = percentile(latencies, 50)
	metrics.P95LatencyMS = percentile(latencies, 95)
	return metrics
}

func categorize(results []TaskResult) []CategoryRow {
	groups := make(map[string][]TaskResult)
	names := make([]string, 0, 4)
	for _, result := range results {
		if _, seen := groups[result.Category]; !seen {
			names = append(names, result.Category)
		}
		groups[result.Category] = append(groups[result.Category], result)
	}
	sort.Strings(names)
	rows := make([]CategoryRow, 0, len(names))
	for _, name := range names {
		group := groups[name]
		rows = append(rows, CategoryRow{Category: name, Tasks: len(group), Metrics: summarize(group)})
	}
	return rows
}

// percentile uses the nearest-rank method on a sorted slice.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// estimateTokens approximates a tokenizer with the usual bytes/4 rule.
func estimateTokens(text string) int {
	return (len(text) + 3) / 4
}

func trimProjectPrefix(id, project string) string {
	prefix := project + "/"
	return strings.TrimPrefix(id, prefix)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
