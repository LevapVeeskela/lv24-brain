//go:build realcorpus

// Real-corpus retrieval gate. Unlike the authored fixtures, this test reads a
// live brain data directory and scores the same search path the CLI uses.
//
// The data directory is never modified: the Markdown store is opened for
// reading only and every index write goes to a temporary directory, which is
// also the app data dir so even a dirty-index repair cannot touch the source.
//
// Run:
//
//	BRAIN_DATA_DIR=/path/to/lv24-brain-data go test -tags realcorpus ./eval \
//	    -run TestRealQueries -count=1
package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
)

const (
	realQueryFile    = "realqueries.json"
	realQueryCount   = 30
	realQueryPage    = 50
	realQueryTop5Min = 0.80
)

// realQuery is one labelled query against the live corpus. The fixture keeps
// record ids and query text only; notes explain why the label is unambiguous.
type realQuery struct {
	Project    string `json:"project"`
	Query      string `json:"query"`
	ExpectedID string `json:"expected_id"`
	Note       string `json:"note"`
}

type realQueryFixture struct {
	Version int         `json:"version"`
	Source  string      `json:"source"`
	Queries []realQuery `json:"queries"`
}

type realOutcome struct {
	Query  realQuery
	Rank   int // 1-based within the fetched page; 0 when absent.
	MRR    float64
	InTop5 bool
}

// TestRealQueries scores realqueries.json against BRAIN_DATA_DIR. It skips when
// the variable is unset, so the default test run stays hermetic.
func TestRealQueries(t *testing.T) {
	dataDir := strings.TrimSpace(os.Getenv("BRAIN_DATA_DIR"))
	if dataDir == "" {
		t.Skip("BRAIN_DATA_DIR is not set; skipping the real-corpus gate")
	}
	if info, err := os.Stat(filepath.Join(dataDir, "projects")); err != nil || !info.IsDir() {
		t.Fatalf("BRAIN_DATA_DIR %q has no projects directory: %v", dataDir, err)
	}
	fixture, err := loadRealQueries(filepath.Join(".", realQueryFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.validate(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	// The temp dir is both the index location and the app data dir. Index
	// writes, the dirty marker and reindex bookkeeping therefore never land in
	// BRAIN_DATA_DIR, while the Markdown store reads the live records.
	tmp := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatalf("open live store: %v", err)
	}
	index, err := sqliteindex.Open(filepath.Join(tmp, "index", "brain.db"))
	if err != nil {
		t.Fatalf("open temp index: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = tmp
	useCase := app.New(cfg, app.Dependencies{Store: store, Index: index})

	indexed, err := useCase.Reindex(ctx, app.IndexOptions{})
	if err != nil {
		t.Fatalf("reindex live corpus: %v", err)
	}
	ids, err := index.IDs(ctx)
	if err != nil {
		t.Fatalf("list indexed ids: %v", err)
	}
	present := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		present[id] = struct{}{}
	}
	for _, query := range fixture.Queries {
		// Index ids carry the project prefix; hits expose the bare id.
		if _, ok := present[query.Project+"/"+query.ExpectedID]; !ok {
			t.Fatalf("expected id %q of query %q is not in the live index", query.ExpectedID, query.Query)
		}
	}

	outcomes := make([]realOutcome, 0, len(fixture.Queries))
	for _, query := range fixture.Queries {
		hits, _, err := useCase.SearchWithNotice(ctx, app.SearchOptions{
			Text:      query.Query,
			ProjectID: query.Project,
			Limit:     realQueryPage,
		})
		if err != nil {
			t.Fatalf("search %q: %v", query.Query, err)
		}
		outcome := realOutcome{Query: query}
		for position, hit := range hits {
			if hit.ProjectID != query.Project {
				continue
			}
			if trimProjectPrefix(hit.ID, query.Project) == query.ExpectedID {
				outcome.Rank = position + 1
				outcome.MRR = 1 / float64(position+1)
				outcome.InTop5 = position < 5
				break
			}
		}
		outcomes = append(outcomes, outcome)
	}

	report := summarizeReal(outcomes)
	fmt.Printf("real corpus: %d queries over %d projects (%d notes, %d decisions, %d attempts, %d handoffs; %d documents)\n",
		len(outcomes), indexed.Projects, indexed.Notes, indexed.Decisions,
		indexed.Attempts, indexed.Handoffs, indexed.Documents)
	fmt.Printf("top-1 %.3f  top-5 %.3f  MRR %.3f  page=%d\n",
		report.top1, report.top5, report.mrr, realQueryPage)
	printRealKinds(summarizeRealByKind(outcomes), []string{"decision", "attempt", "note", "handoff"})
	printRealWorst(outcomes, 5)

	if report.top5 < realQueryTop5Min {
		t.Errorf("top-5 %.3f is below the %.2f gate", report.top5, realQueryTop5Min)
	}
}

type realSummary struct {
	top1, top5, mrr float64
	top1Count       int
	top5Count       int
}

func summarizeReal(outcomes []realOutcome) realSummary {
	summary := realSummary{}
	if len(outcomes) == 0 {
		return summary
	}
	for _, outcome := range outcomes {
		summary.mrr += outcome.MRR
		if outcome.Rank == 1 {
			summary.top1Count++
		}
		if outcome.InTop5 {
			summary.top5Count++
		}
	}
	total := float64(len(outcomes))
	summary.top1 = float64(summary.top1Count) / total
	summary.top5 = float64(summary.top5Count) / total
	summary.mrr /= total
	return summary
}

// summarizeRealByKind groups the outcomes by the kind encoded in the expected
// id, so the fixture needs no duplicated metadata.
func summarizeRealByKind(outcomes []realOutcome) map[string]realSummary {
	groups := make(map[string][]realOutcome, 4)
	for _, outcome := range outcomes {
		kind := realKind(outcome.Query.ExpectedID)
		groups[kind] = append(groups[kind], outcome)
	}
	report := make(map[string]realSummary, len(groups))
	for kind, group := range groups {
		report[kind] = summarizeReal(group)
	}
	return report
}

// printRealKinds reports retrieval per record kind. The kind is the id prefix,
// so the fixture does not need to duplicate metadata the id already carries.
func printRealKinds(report map[string]realSummary, order []string) {
	fmt.Println("by kind:")
	for _, kind := range order {
		summary := report[kind]
		fmt.Printf("  %-8s top-1 %.2f  top-5 %.2f  MRR %.3f\n",
			kind, summary.top1, summary.top5, summary.mrr)
	}
}

// printRealWorst lists the queries that missed the top hit: absent results
// first, then the deepest ranks. Perfect hits are not filler.
func printRealWorst(outcomes []realOutcome, count int) {
	worst := make([]realOutcome, 0, len(outcomes))
	for _, outcome := range outcomes {
		if outcome.Rank != 1 {
			worst = append(worst, outcome)
		}
	}
	sort.SliceStable(worst, func(left, right int) bool {
		// An absent hit is worse than any rank; the rest go deepest first.
		if (worst[left].Rank == 0) != (worst[right].Rank == 0) {
			return worst[left].Rank == 0
		}
		return worst[left].Rank > worst[right].Rank
	})
	if count > len(worst) {
		count = len(worst)
	}
	fmt.Printf("missed top-1: %d (showing up to %d)\n", len(worst), count)
	if count == 0 {
		fmt.Println("  none")
		return
	}
	for _, outcome := range worst[:count] {
		rank := "not in page"
		if outcome.Rank > 0 {
			rank = fmt.Sprintf("rank %d", outcome.Rank)
		}
		fmt.Printf("  %-12s %s :: %q\n", rank, outcome.Query.Project, outcome.Query.Query)
	}
}

func loadRealQueries(path string) (realQueryFixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return realQueryFixture{}, fmt.Errorf("read %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var fixture realQueryFixture
	if err := decoder.Decode(&fixture); err != nil {
		return realQueryFixture{}, fmt.Errorf("decode %s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return realQueryFixture{}, fmt.Errorf("%s must contain exactly one JSON document", path)
	}
	return fixture, nil
}

func (f realQueryFixture) validate() error {
	if f.Version != 1 {
		return fmt.Errorf("realqueries: version = %d, want 1", f.Version)
	}
	if len(f.Queries) != realQueryCount {
		return fmt.Errorf("realqueries: %d queries, want %d; update the constant with the fixture", len(f.Queries), realQueryCount)
	}
	queries := make(map[string]string, len(f.Queries))
	labels := make(map[string]string, len(f.Queries))
	for index, query := range f.Queries {
		where := fmt.Sprintf("query %d", index+1)
		project := strings.TrimSpace(query.Project)
		text := strings.TrimSpace(query.Query)
		expected := strings.TrimSpace(query.ExpectedID)
		switch {
		case project == "":
			return fmt.Errorf("realqueries: %s: project is required", where)
		case text == "":
			return fmt.Errorf("realqueries: %s: query is required", where)
		case expected == "":
			return fmt.Errorf("realqueries: %s: expected_id is required", where)
		case strings.TrimSpace(query.Note) == "":
			return fmt.Errorf("realqueries: %s: note is required", where)
		case text != query.Query:
			return fmt.Errorf("realqueries: %s: query has surrounding whitespace", where)
		case expected != query.ExpectedID:
			return fmt.Errorf("realqueries: %s: expected_id has surrounding whitespace", where)
		}
		key := project + "\x00" + text
		if previous, duplicate := queries[key]; duplicate {
			return fmt.Errorf("realqueries: %s repeats the query of %s", where, previous)
		}
		queries[key] = where
		if previous, duplicate := labels[expected]; duplicate {
			return fmt.Errorf("realqueries: %s repeats the expected id of %s", where, previous)
		}
		labels[expected] = where
		if kind := realKind(expected); kind == "" {
			return fmt.Errorf("realqueries: %s: expected_id %q has no known record kind", where, expected)
		}
	}
	return nil
}

// realKind returns the record kind encoded in an id, or an empty string when
// the id is not one of the four curated kinds the fixture may label.
func realKind(id string) string {
	for _, kind := range []string{"decision", "attempt", "note", "handoff"} {
		if strings.HasPrefix(id, kind+"-") {
			return kind
		}
	}
	return ""
}
