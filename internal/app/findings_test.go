package app_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/journal"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

type reviewFixture struct {
	app   *app.App
	clock *time.Time
}

func newReviewFixture(t *testing.T) reviewFixture {
	t.Helper()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.User = "alice"
	a := app.New(cfg, app.Dependencies{Store: store, Journal: journal.New(dataDir), Now: func() time.Time { return clock }})
	if _, err := a.Note(context.Background(), app.NoteInput{ProjectID: "p", Body: "seed", CreateProject: true}); err != nil {
		t.Fatal(err)
	}
	return reviewFixture{app: a, clock: &clock}
}

func (f reviewFixture) tick() { *f.clock = f.clock.Add(time.Minute) }

func kindsOf(findings []app.Finding) map[app.FindingKind]int {
	kinds := map[app.FindingKind]int{}
	for _, finding := range findings {
		kinds[finding.Kind]++
	}
	return kinds
}

func TestFindingsFixNextActionAndAnsweredQuestion(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t)
	next := "write the parser"
	questions := []string{"Which storage engine should the index use?", "Who owns the release key?"}
	if _, err := f.app.SetState(ctx, app.StateInput{ProjectID: "p", NextAction: &next, OpenQuestions: &questions}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if _, err := f.app.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "parser done", NextAction: "ship the release"}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if _, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Index storage engine: SQLite FTS5"}); err != nil {
		t.Fatal(err)
	}

	findings, err := f.app.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	kinds := kindsOf(findings)
	if kinds[app.FindingNextBehindHandoff] != 1 || kinds[app.FindingQuestionAnswered] != 1 || len(findings) != 2 {
		t.Fatalf("findings = %+v", findings)
	}

	f.tick()
	result, err := f.app.FixFindings(ctx, "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 2 || result.State.NextAction != "ship the release" ||
		len(result.State.OpenQuestions) != 1 || result.State.OpenQuestions[0] != "Who owns the release key?" {
		t.Fatalf("fix result = %+v", result)
	}
	if after, err := f.app.Findings(ctx, "p"); err != nil || len(after) != 0 {
		t.Fatalf("findings after fix = %+v, %v", after, err)
	}
	recent, err := f.app.Catchup(ctx, app.CatchupOptions{ProjectID: "p", Since: f.clock.Add(-time.Second)})
	if err != nil || len(recent.Notes) != 1 || !strings.Contains(recent.Notes[0].Body, "Which storage engine") {
		t.Fatalf("removed question not kept in a note: %+v %v", recent.Notes, err)
	}
}

func TestFindingsFixKindFilter(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t)
	next := "old"
	questions := []string{"storage engine for the index"}
	if _, err := f.app.SetState(ctx, app.StateInput{ProjectID: "p", NextAction: &next, OpenQuestions: &questions}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if _, err := f.app.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "s", NextAction: "new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Index storage engine is SQLite"}); err != nil {
		t.Fatal(err)
	}
	kinds, err := app.ParseFindingKinds("question-answered")
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.app.FixFindings(ctx, "p", kinds)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || result.State.NextAction != "old" || len(result.State.OpenQuestions) != 0 {
		t.Fatalf("filtered fix = %+v", result)
	}
	if _, err := app.ParseFindingKinds("next-behind-handoff, bogus"); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestFindingsReportOnlyHeuristics(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t)
	next := "migrate the journal to union merge"
	fresh := f.clock.Add(time.Hour)
	if _, err := f.app.SetState(ctx, app.StateInput{ProjectID: "p", NextAction: &next, FreshUntil: &fresh}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Sync journals with git union merge"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		f.tick()
		subject := "chore: tidy"
		if i == 9 {
			subject = "Journal: union merge migration"
		}
		if _, err := f.app.RecordCommit(ctx, app.CommitInput{ProjectID: "p", Head: "c" + string(rune('0'+i)), Subject: subject}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.app.Attempt(ctx, app.AttemptInput{ProjectID: "p", Title: "Union merge for journals", Hypothesis: "git union merge keeps both sides", Result: "duplicated lines"}); err != nil {
		t.Fatal(err)
	}
	*f.clock = f.clock.Add(2 * time.Hour)

	findings, err := f.app.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	kinds := kindsOf(findings)
	for _, kind := range []app.FindingKind{app.FindingStateExpired, app.FindingStateBehindCommits, app.FindingNextMaybeDone, app.FindingDecisionContradicted} {
		if kinds[kind] != 1 {
			t.Fatalf("missing %s in %+v", kind, findings)
		}
	}
	for _, finding := range findings {
		if finding.Fixable {
			t.Fatalf("report-only finding marked fixable: %+v", finding)
		}
		if finding.Kind == app.FindingNextMaybeDone && !strings.Contains(finding.Evidence, "c9") {
			t.Fatalf("next-maybe-done evidence = %q", finding.Evidence)
		}
	}
	result, err := f.app.FixFindings(ctx, "p", nil)
	if err != nil || len(result.Applied) != 0 {
		t.Fatalf("report-only fix = %+v, %v", result, err)
	}
}

// Two shared words are not enough to call an open question answered.
func TestFindingsKeepWeaklyMatchedQuestions(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t)
	questions := []string{"Redis or Postgres for the session cache?"}
	if _, err := f.app.SetState(ctx, app.StateInput{ProjectID: "p", OpenQuestions: &questions}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if _, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Redis for rate limiting; Postgres stays primary"}); err != nil {
		t.Fatal(err)
	}
	findings, err := f.app.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if kindsOf(findings)[app.FindingQuestionAnswered] != 0 {
		t.Fatalf("weak match reported as answered: %+v", findings)
	}
}

// codeDecisionFixture saves a project checkout at dir (empty: none) and
// records an accepted decision that governs internal/wire/.
func codeDecisionFixture(t *testing.T, f reviewFixture, dir string) {
	t.Helper()
	ctx := context.Background()
	if dir != "" {
		if _, err := f.app.SaveProject(ctx, app.ProjectInput{ID: "p", Paths: &[]string{dir}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Keep the wire format", Status: domain.DecisionAccepted, Paths: []string{"internal/wire/"}}); err != nil {
		t.Fatal(err)
	}
}

func codeFindings(findings []app.Finding) []app.Finding {
	var out []app.Finding
	for _, finding := range findings {
		if finding.Kind == app.FindingDecisionCodeChanged {
			out = append(out, finding)
		}
	}
	return out
}

func TestFindingsDecisionCodeChangedWithFakeGit(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t)
	dir := t.TempDir()
	codeDecisionFixture(t, f, dir)
	var gotDir string
	var gotPaths []string
	restore := app.SetCommitsTouchingForTest(func(_ context.Context, d string, since time.Time, paths []string) ([]string, error) {
		gotDir, gotPaths = d, paths
		if since.IsZero() {
			t.Error("since is zero")
		}
		return []string{"abc1234", "def5678"}, nil
	})
	defer restore()

	findings, err := f.app.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	got := codeFindings(findings)
	if len(got) != 1 {
		t.Fatalf("decision-code-changed findings = %+v", got)
	}
	if got[0].Fixable || got[0].Evidence != "abc1234" || !strings.Contains(got[0].Message, "2 commits changed them") || !strings.Contains(got[0].Message, "internal/wire/") {
		t.Fatalf("finding = %+v", got[0])
	}
	if gotDir != dir || len(gotPaths) != 1 || gotPaths[0] != "internal/wire/" {
		t.Fatalf("git queried with dir=%q paths=%q", gotDir, gotPaths)
	}
}

func TestFindingsDecisionCodeUnchanged(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t)
	codeDecisionFixture(t, f, t.TempDir())
	restore := app.SetCommitsTouchingForTest(func(context.Context, string, time.Time, []string) ([]string, error) { return nil, nil })
	defer restore()

	findings, err := f.app.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if got := codeFindings(findings); len(got) != 0 {
		t.Fatalf("unexpected findings: %+v", got)
	}
}

func TestFindingsDecisionCodeNoCheckout(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t)
	codeDecisionFixture(t, f, "")
	calls := 0
	restore := app.SetCommitsTouchingForTest(func(context.Context, string, time.Time, []string) ([]string, error) {
		calls++
		return []string{"abc1234"}, nil
	})
	defer restore()

	findings, err := f.app.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || len(codeFindings(findings)) != 0 {
		t.Fatalf("git called %d times, findings %+v; want no checkout, no git", calls, findings)
	}
}

func TestFindingsDecisionCodeMissingDirSkipped(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t)
	codeDecisionFixture(t, f, filepath.Join(t.TempDir(), "absent"))
	calls := 0
	restore := app.SetCommitsTouchingForTest(func(context.Context, string, time.Time, []string) ([]string, error) {
		calls++
		return []string{"abc1234"}, nil
	})
	defer restore()

	if _, err := f.app.Findings(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("git called %d times for a checkout that does not exist here", calls)
	}
}

func TestFindingsDecisionCodeRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	f := newReviewFixture(t)
	repo := t.TempDir()
	// The store stamps UpdatedAt with the real clock, so the commits are dated
	// around it: one before the decision, one unrelated and one governed after.
	now := time.Now().UTC()
	gitRun(t, repo, now.Add(-2*time.Hour).Format(time.RFC3339), "init", "-q")
	gitWrite(t, repo, "internal/wire/codec.go", "v1\n")
	gitWrite(t, repo, "README.md", "readme\n")
	gitRun(t, repo, now.Add(-2*time.Hour).Format(time.RFC3339), "add", ".")
	gitRun(t, repo, now.Add(-2*time.Hour).Format(time.RFC3339), "commit", "-q", "-m", "initial")

	codeDecisionFixture(t, f, repo)

	gitWrite(t, repo, "README.md", "readme v2\n")
	gitRun(t, repo, now.Add(time.Hour).Format(time.RFC3339), "commit", "-qam", "docs only")
	gitWrite(t, repo, "internal/wire/codec.go", "v2\n")
	gitRun(t, repo, now.Add(2*time.Hour).Format(time.RFC3339), "commit", "-qam", "change codec")

	findings, err := f.app.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	got := codeFindings(findings)
	if len(got) != 1 || !strings.Contains(got[0].Message, "1 commits changed them since") || got[0].Evidence == "" {
		t.Fatalf("real git findings = %+v", got)
	}
}

// gitRun runs git in dir with a fixed author and committer date so the
// --since filter is deterministic.
func gitRun(t *testing.T, dir, date string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.invalid"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitWrite(t *testing.T, repo, rel, content string) {
	t.Helper()
	path := filepath.Join(repo, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
