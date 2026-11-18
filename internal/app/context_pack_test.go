package app_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// newPackFixture builds a project with a state, a handoff, large decisions,
// attempts and notes matching "packtoken", so every layer has material to
// include or drop.
func newPackFixture(t *testing.T) contextFixture {
	t.Helper()
	f := newContextFixture(t, true)
	ctx := context.Background()
	// The first write registers the project before state can be set.
	f.decision(t, "Pack seed decision packtoken", "seed context packtoken")
	goal := "Pack the context"
	next := "Ship the packer"
	active := []string{"layer budgets", "truncation rules"}
	questions := []string{"which cap is right?"}
	if _, err := f.app.SetState(ctx, app.StateInput{
		ProjectID: "p", Goal: &goal, NextAction: &next, Active: &active, OpenQuestions: &questions,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Handoff(ctx, app.HandoffInput{
		ProjectID: "p", Summary: "Packed context handoff packtoken", NextAction: "Review the budget table",
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		f.decision(t, fmt.Sprintf("Pack decision %02d packtoken", i),
			strings.Repeat(fmt.Sprintf("decision %02d packtoken context ", i), 12))
	}
	for i := 0; i < 10; i++ {
		f.attempt(t, fmt.Sprintf("Pack attempt %02d packtoken", i),
			strings.Repeat(fmt.Sprintf("attempt %02d packtoken reason ", i), 10))
	}
	for i := 0; i < 6; i++ {
		f.note(t, strings.Repeat(fmt.Sprintf("note %02d packtoken ", i), 8))
	}
	return f
}

func packSection(t *testing.T, pack app.ContextPack, name string) app.ContextSection {
	t.Helper()
	for _, section := range pack.Sections {
		if section.Name == name {
			return section
		}
	}
	return app.ContextSection{}
}

func TestContextPackHonoursBudgets(t *testing.T) {
	for _, budget := range []int{2000, 6000, 12000} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			// Arrange
			ctx := context.Background()
			f := newPackFixture(t)

			// Act
			result, err := f.app.Context(ctx, app.ContextOptions{
				ProjectID: "p", Query: "packtoken", TokenBudget: budget,
			})

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if result.Pack.Budget != budget || result.Pack.TotalTokens > budget {
				t.Fatalf("budget = %d total = %d, want at most %d", result.Pack.Budget, result.Pack.TotalTokens, budget)
			}
			core := packSection(t, result.Pack, app.ContextLayerCore)
			if len(core.Records) == 0 || core.Records[0].ID != "state" {
				t.Fatalf("core records = %+v, want the state first", core.Records)
			}
			for _, want := range []string{"goal: Pack the context", "next: Review the budget table", "open_questions:"} {
				if !strings.Contains(result.Summary, want) {
					t.Fatalf("summary lacks %q:\n%s", want, result.Summary)
				}
			}
			sum := 0
			for _, section := range result.Pack.Sections {
				sum += section.Tokens
				if section.Name == app.ContextLayerCore {
					continue
				}
				for _, record := range section.Records {
					if record.Tokens <= 0 || record.Path == "" || record.Title == "" || record.ID == "" {
						t.Fatalf("%s record = %+v, want id, title, path and tokens", section.Name, record)
					}
					// A record is never split: its title and full preview must
					// be in the rendered answer.
					if !strings.Contains(result.Summary, record.Title) || !strings.Contains(result.Summary, record.Detail) {
						t.Fatalf("%s record %s was split:\n%s", section.Name, record.ID, result.Summary)
					}
				}
			}
			if sum != result.Pack.TotalTokens {
				t.Fatalf("section sum = %d, total = %d", sum, result.Pack.TotalTokens)
			}
			if budget == 2000 {
				if len(result.Pack.Truncated) == 0 {
					t.Fatal("a 2000-token budget dropped nothing")
				}
				for _, truncation := range result.Pack.Truncated {
					if truncation.ID == "" || truncation.Section == "" || truncation.Reason == "" {
						t.Fatalf("truncation = %+v, want id, section and reason", truncation)
					}
					for _, section := range result.Pack.Sections {
						for _, record := range section.Records {
							if record.ID == truncation.ID {
								t.Fatalf("record %s is both packed and truncated", truncation.ID)
							}
						}
					}
				}
			}
			if budget >= 6000 && len(result.Records) == 0 {
				t.Fatalf("budget %d packed no query hits", budget)
			}
		})
	}
}

func TestContextPackIsDeterministic(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newPackFixture(t)
	opts := app.ContextOptions{ProjectID: "p", Query: "packtoken", TokenBudget: 6000}

	// Act
	first, err := f.app.Context(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.app.Context(ctx, opts)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Pack, second.Pack) {
		t.Fatalf("packs differ:\n%+v\n%+v", first.Pack, second.Pack)
	}
	if first.Summary != second.Summary {
		t.Fatalf("summaries differ:\n%s\n%s", first.Summary, second.Summary)
	}
}

func TestContextPackLinkedLayerNeedsRoom(t *testing.T) {
	// Arrange: the decision links to a note that the query itself does not
	// match, so the note can only arrive through the linked layer.
	ctx := context.Background()
	f := newContextFixture(t, true)
	target := f.note(t, "linked target marker only")
	f.decision(t, "Linked root decision linkedroot", "the root is linked: [["+target+"]]")

	// Act: a large budget has room for the 1-hop layer.
	large, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "linkedroot", TokenBudget: domain.MaxContextBudgetTokens,
	})
	if err != nil {
		t.Fatal(err)
	}
	small, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "linkedroot", TokenBudget: 150,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	linked := packSection(t, large.Pack, app.ContextLayerLinked)
	if len(linked.Records) != 1 || linked.Records[0].ID != target {
		t.Fatalf("linked records = %+v, want %s", linked.Records, target)
	}
	if strings.Contains(small.Summary, "linked target marker") {
		t.Fatalf("a 150-token answer included the linked layer:\n%s", small.Summary)
	}
}

func TestContextPackQueryHitsRespectValidityAndQuarantine(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newContextFixture(t, true)
	old := f.decision(t, "Filter decision old filtermarker", "filtermarker old body")
	current := f.decision(t, "Filter decision current filtermarker", "filtermarker current body")
	if _, err := f.app.Supersede(ctx, "p", old, current); err != nil {
		t.Fatal(err)
	}
	quarantined, err := f.app.Note(ctx, app.NoteInput{
		ProjectID: "p", Title: "Filter quarantined filtermarker", Body: "filtermarker quarantined body",
		Provenance: domain.Provenance{Origin: domain.OriginAgent},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	def, err := f.app.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "filtermarker", TokenBudget: 12000})
	if err != nil {
		t.Fatal(err)
	}
	included, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "filtermarker", TokenBudget: 12000, IncludeUnreviewed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	everything, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "filtermarker", TokenBudget: 12000, IncludeSuperseded: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	for _, section := range def.Pack.Sections {
		for _, record := range section.Records {
			if record.ID == old || record.ID == quarantined.Note.ID {
				t.Fatalf("%s leaked %s into the default context", section.Name, record.ID)
			}
			if record.ID == current && strings.Contains(record.Detail, "old body") {
				t.Fatalf("current decision carries the old body: %+v", record)
			}
		}
	}
	found := false
	for _, record := range included.Records {
		if record.ID == quarantined.Note.ID && record.Quarantined {
			found = true
		}
	}
	if !found {
		t.Fatalf("include-unreviewed records = %+v, want %s", included.Records, quarantined.Note.ID)
	}
	superseded := false
	for _, section := range everything.Pack.Sections {
		for _, record := range section.Records {
			if record.ID == old {
				superseded = true
			}
		}
	}
	if !superseded {
		t.Fatalf("include-superseded did not restore %s", old)
	}
}

func TestContextPackConfigBudget(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newPackFixture(t)
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.Context.Budget = 900
	runtime := app.New(cfg, app.Dependencies{
		Store: f.store, Index: f.index, Now: func() time.Time { return *f.clock },
	})

	// Act
	result, err := runtime.Context(ctx, app.ContextOptions{ProjectID: "p", Query: "packtoken"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if result.Pack.Budget != 900 || result.Pack.TotalTokens > 900 {
		t.Fatalf("budget = %d total = %d, want the configured 900", result.Pack.Budget, result.Pack.TotalTokens)
	}
}

func TestContextRejectsOversizedTokenBudget(t *testing.T) {
	f := newContextFixture(t, true)
	f.note(t, "seed")
	if _, err := f.app.Context(context.Background(), app.ContextOptions{
		ProjectID: "p", Query: "seed", TokenBudget: domain.MaxContextBudgetTokens + 1,
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
}

func TestBootstrapPackedBudget(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newPackFixture(t)

	// Act
	result, err := f.app.Bootstrap(ctx, app.BootstrapOptions{ProjectID: "p", TokenBudget: 2000})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if result.Pack.Budget != 2000 || result.Pack.TotalTokens > 2000 {
		t.Fatalf("budget = %d total = %d", result.Pack.Budget, result.Pack.TotalTokens)
	}
	if strings.Contains(result.Summary, "# p — context") || !strings.Contains(result.Summary, "# p — bootstrap") {
		t.Fatalf("summary header:\n%s", result.Summary)
	}
	for _, want := range []string{"goal: Pack the context", "## Latest handoff", "## Recent decisions", "## Recent attempts"} {
		if !strings.Contains(result.Summary, want) {
			t.Fatalf("summary lacks %q:\n%s", want, result.Summary)
		}
	}
}

// --- RET-5: path/branch relevance ------------------------------------------

// initPathRepo creates a throwaway checkout on a feature branch with one
// commit. The index is never written by the tests: they only add untracked
// files, which is what `git ls-files --others` reports.
func initPathRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_DATE=2026-11-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-11-01T00:00:00Z")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "brain-test@example.test")
	run("config", "user.name", "Brain Test")
	run("checkout", "-b", "feat/context-ranker")
	writeRepoFile(t, dir, "README.md", "init\n")
	run("add", "README.md")
	run("commit", "-m", "init")
	return dir
}

func writeRepoFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newPathFixture(t *testing.T, repo string) contextFixture {
	t.Helper()
	f := newContextFixture(t, true)
	paths := []string{repo}
	if _, err := f.app.SaveProject(context.Background(), app.ProjectInput{ID: "p", Paths: &paths}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f contextFixture) decisionWithPaths(t *testing.T, title, contextText string, paths []string) string {
	t.Helper()
	f.advance(time.Minute)
	result, err := f.app.Decision(context.Background(), app.DecisionInput{
		ProjectID: "p", Title: title, Context: contextText, Paths: paths, CreateProject: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Decision.ID
}

func recordIndex(records []app.ContextRecord, id string) int {
	for i, record := range records {
		if record.ID == id {
			return i
		}
	}
	return -1
}

// TestContextPathSignalImprovesTenQueryPositions is the RET-5 acceptance
// fixture: ten queries whose expected record touches a changed file moves in
// front of its no-path competitor with ranking.paths on. With the switch off
// the same ten rankings are byte-identical to a run without the feature.
func TestContextPathSignalImprovesTenQueryPositions(t *testing.T) {
	// Arrange
	ctx := context.Background()
	repo := initPathRepo(t)
	f := newPathFixture(t, repo)
	const queries = 10
	tokens := make([]string, queries)
	expected := make([]string, queries)
	competitors := make([]string, queries)
	for i := 0; i < queries; i++ {
		tokens[i] = fmt.Sprintf("pathsignal%02d", i)
		competitors[i] = f.decision(t, "Competitor "+tokens[i], tokens[i]+" shared text")
		expected[i] = f.decisionWithPaths(t,
			"Expected "+tokens[i], tokens[i]+" shared text",
			[]string{fmt.Sprintf("src/changed%02d.go", i)})
		writeRepoFile(t, repo, fmt.Sprintf("src/changed%02d.go", i), "package src\n")
	}
	// Fillers match every query once, so each query has the same 15 hits and
	// the expected/competitor pair is adjacent at the top.
	fillerBody := strings.Join(tokens, " ")
	for i := 0; i < 13; i++ {
		f.decision(t, fmt.Sprintf("Filler %02d", i), fillerBody)
	}
	offScores := make([][]float64, queries)
	offIDs := make([][]string, queries)
	offSummaries := make([]string, queries)

	// Act: the switch off keeps the default order and never runs the signal.
	t.Setenv(app.RankingPathsEnv, "")
	for i := 0; i < queries; i++ {
		result, err := f.app.Context(ctx, app.ContextOptions{
			ProjectID: "p", Query: tokens[i], TokenBudget: domain.MaxContextBudgetTokens,
		})
		if err != nil {
			t.Fatal(err)
		}
		competitorAt, expectedAt := recordIndex(result.Records, competitors[i]), recordIndex(result.Records, expected[i])
		if competitorAt < 0 || expectedAt < 0 || competitorAt >= expectedAt {
			t.Fatalf("off order %v: competitor at %d, expected at %d", ids(result.Records), competitorAt, expectedAt)
		}
		if result.Records[expectedAt].PathSignal != 0 || result.Pack.PathSignal {
			t.Fatalf("off query %s carried a path signal", tokens[i])
		}
		if strings.Contains(result.Pack.ExplainText(), "paths:") {
			t.Fatalf("off explain carried a paths line:\n%s", result.Pack.ExplainText())
		}
		offIDs[i] = ids(result.Records)
		offScores[i] = scores(result.Records)
		offSummaries[i] = result.Summary
	}

	// Act: the switch on moves the expected record in front of the competitor.
	t.Setenv(app.RankingPathsEnv, "1")
	for i := 0; i < queries; i++ {
		result, err := f.app.Context(ctx, app.ContextOptions{
			ProjectID: "p", Query: tokens[i], TokenBudget: domain.MaxContextBudgetTokens,
		})
		if err != nil {
			t.Fatal(err)
		}
		competitorAt, expectedAt := recordIndex(result.Records, competitors[i]), recordIndex(result.Records, expected[i])
		if expectedAt < 0 || competitorAt < 0 || expectedAt >= competitorAt {
			t.Fatalf("on order %v: expected at %d, competitor at %d", ids(result.Records), expectedAt, competitorAt)
		}
		bonus := result.Records[expectedAt].PathSignal
		if bonus != app.ContextPathSignalCap {
			t.Fatalf("exact file bonus = %.3f, want %.3f", bonus, app.ContextPathSignalCap)
		}
		if result.Records[competitorAt].PathSignal != 0 {
			t.Fatalf("competitor without paths earned %.3f", result.Records[competitorAt].PathSignal)
		}
		for _, record := range result.Records {
			if record.PathSignal > app.ContextPathSignalCap {
				t.Fatalf("%s bonus = %.3f, over the cap %.3f", record.ID, record.PathSignal, app.ContextPathSignalCap)
			}
		}
		if !result.Pack.PathSignal || result.Pack.PathChangedFiles < queries {
			t.Fatalf("pack path report = %+v", result.Pack)
		}
		explain := result.Pack.ExplainText()
		if !strings.Contains(explain, "paths: on") || !strings.Contains(explain, expected[i]+" +0.050") {
			t.Fatalf("explain lacks the path contribution:\n%s", explain)
		}
	}

	// Assert: turning the switch off again reproduces the default answer. One
	// query is enough here; the ten-query comparison above already ran the
	// whole fixture with the switch on.
	t.Setenv(app.RankingPathsEnv, "")
	result, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: tokens[0], TokenBudget: domain.MaxContextBudgetTokens,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(result.Records); !reflect.DeepEqual(got, offIDs[0]) {
		t.Fatalf("off/on/off order %v, want %v", got, offIDs[0])
	}
	if got := scores(result.Records); !reflect.DeepEqual(got, offScores[0]) {
		t.Fatalf("off/on/off scores %v, want %v", got, offScores[0])
	}
	if result.Summary != offSummaries[0] {
		t.Fatalf("off/on/off summary changed:\n%s\n%s", offSummaries[0], result.Summary)
	}
}

func scores(records []app.ContextRecord) []float64 {
	out := make([]float64, 0, len(records))
	for _, record := range records {
		out = append(out, record.Score)
	}
	return out
}

// TestContextPathSignalTiersAndCap checks the strength order exact file >
// directory prefix > branch-name token and that every tier stays under the cap.
func TestContextPathSignalTiersAndCap(t *testing.T) {
	// Arrange
	ctx := context.Background()
	repo := initPathRepo(t)
	f := newPathFixture(t, repo)
	writeRepoFile(t, repo, "internal/app/changed.go", "package app\n")
	query := "tiersignal"
	exact := f.decisionWithPaths(t, "Exact tiersignal", query, []string{"internal/app/changed.go"})
	dir := f.decisionWithPaths(t, "Dir tiersignal", query, []string{"internal/app"})
	branch := f.decisionWithPaths(t, "Branch tiersignal", query, []string{"src/context_feature.go"})

	// Act
	t.Setenv(app.RankingPathsEnv, "1")
	result, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: query, TokenBudget: domain.MaxContextBudgetTokens,
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]app.ContextRecord{}
	for _, record := range result.Records {
		byID[record.ID] = record
		if record.PathSignal > app.ContextPathSignalCap {
			t.Fatalf("%s bonus = %.3f, over the cap %.3f", record.ID, record.PathSignal, app.ContextPathSignalCap)
		}
	}
	if got := byID[exact].PathSignal; got != app.ContextPathSignalCap {
		t.Fatalf("exact bonus = %.3f, want %.3f", got, app.ContextPathSignalCap)
	}
	if got, want := byID[dir].PathSignal, 0.03; got != want {
		t.Fatalf("dir bonus = %.3f, want %.3f", got, want)
	}
	if got, want := byID[branch].PathSignal, 0.013; got != want {
		t.Fatalf("branch bonus = %.3f, want %.3f", got, want)
	}
	if byID[exact].PathSignal <= byID[dir].PathSignal || byID[dir].PathSignal <= byID[branch].PathSignal {
		t.Fatalf("tier order wrong: exact=%.3f dir=%.3f branch=%.3f",
			byID[exact].PathSignal, byID[dir].PathSignal, byID[branch].PathSignal)
	}
	for id, want := range map[string]string{exact: "exact ", dir: "dir ", branch: "branch "} {
		if !strings.HasPrefix(byID[id].PathSignalMatch, want) {
			t.Fatalf("%s match = %q, want %q prefix", id, byID[id].PathSignalMatch, want)
		}
	}
}

// TestContextPathSignalDegradesSilently keeps the read-only promise: a
// checkout that is not a repository, or a machine without git, ranks exactly
// as the flagless engine and reports no skipped section.
func TestContextPathSignalDegradesSilently(t *testing.T) {
	for _, tc := range []struct {
		name       string
		isolateGit bool
	}{
		{"not a repository", false},
		{"git missing", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			ctx := context.Background()
			f := newContextFixture(t, true)
			dir := t.TempDir()
			paths := []string{dir}
			if _, err := f.app.SaveProject(ctx, app.ProjectInput{ID: "p", Paths: &paths}); err != nil {
				t.Fatal(err)
			}
			id := f.decisionWithPaths(t, "Degraded pathsignal", "pathsignal degraded text", []string{"src/degrade.go"})
			t.Setenv(app.RankingPathsEnv, "1")
			if tc.isolateGit {
				t.Setenv("PATH", "")
			}

			// Act
			result, err := f.app.Context(ctx, app.ContextOptions{
				ProjectID: "p", Query: "pathsignal", TokenBudget: domain.MaxContextBudgetTokens,
			})

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Records) != 1 || result.Records[0].ID != id {
				t.Fatalf("records = %v, want %s", ids(result.Records), id)
			}
			if result.Records[0].PathSignal != 0 || result.Pack.PathSignal {
				t.Fatalf("a checkout without git still earned a signal: %+v", result.Records[0])
			}
			if len(result.Skipped) != 0 || strings.Contains(result.Pack.ExplainText(), "paths:") {
				t.Fatalf("degraded signal was reported: skipped=%v explain=%s", result.Skipped, result.Pack.ExplainText())
			}
		})
	}
}
