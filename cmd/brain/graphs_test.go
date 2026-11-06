package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// graphsFake records external tool calls and replaces graphsRun and
// graphsLookPath. Git calls are delegated to the real runner so changed-only
// checks see a real repository.
type graphsFake struct {
	calls   []string
	missing map[string]bool
	failOn  map[string]bool
}

func stubGraphs(t *testing.T, missing []string, failOn []string) *graphsFake {
	t.Helper()
	fake := &graphsFake{missing: map[string]bool{}, failOn: map[string]bool{}}
	for _, name := range missing {
		fake.missing[name] = true
	}
	for _, name := range failOn {
		fake.failOn[name] = true
	}
	origRun, origLook, origLog := graphsRun, graphsLookPath, graphsLogPath
	t.Cleanup(func() {
		graphsRun, graphsLookPath, graphsLogPath = origRun, origLook, origLog
	})
	logPath := filepath.Join(t.TempDir(), "graphs.log")
	graphsLogPath = func() (string, error) { return logPath, nil }
	graphsLookPath = func(name string) (string, error) {
		if fake.missing[name] {
			return "", exec.ErrNotFound
		}
		return "/fake/bin/" + name, nil
	}
	graphsRun = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "git" {
			return execGraphsCommand(ctx, dir, name, args...)
		}
		fake.calls = append(fake.calls, graphsCallString(name, dir, args...))
		if fake.failOn[name] {
			// A real non-zero exit so the status is reported as "exit N".
			return execGraphsCommand(ctx, t.TempDir(), "git", "rev-parse", "--verify", "no-such-ref")
		}
		return []byte("done\n"), nil
	}
	return fake
}

// toolCalls returns the recorded calls excluding git queries.
func (f *graphsFake) toolCalls() []string {
	return append([]string(nil), f.calls...)
}

func graphsCallString(name, dir string, args ...string) string {
	return name + " " + strings.Join(args, " ") + " @" + filepath.Clean(dir)
}

func assertGraphsCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	g := append([]string(nil), got...)
	w := make([]string, 0, len(want))
	for _, c := range want {
		w = append(w, c)
	}
	slices.Sort(g)
	slices.Sort(w)
	if !slices.Equal(g, w) {
		t.Fatalf("tool calls mismatch\n got: %q\nwant: %q", g, w)
	}
}

func setupGraphsBrain(t *testing.T) string {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}
	return filepath.Join(dataDir, "brain.yaml")
}

func registerGraphsProject(t *testing.T, cfgPath, id string, paths ...string) {
	t.Helper()
	args := []string{"project", "set", id}
	for _, p := range paths {
		args = append(args, "--path", p)
	}
	args = append(args, "--config", cfgPath)
	if code, _, stderr := runForTest(t, args...); code != 0 {
		t.Fatalf("project set %s exit code = %d (stderr: %s)", id, code, stderr)
	}
}

func mkGraphsDirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeGraphsFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGraphsUpdatesGraphifyAndCodegraphEntries(t *testing.T) {
	cfgPath := setupGraphsBrain(t)
	root := t.TempDir()
	alpha := filepath.Join(root, "alpha")
	writeGraphsFile(t, filepath.Join(alpha, "graphify-out", "graph.json"))
	mkGraphsDirs(t, filepath.Join(alpha, ".codegraph"), filepath.Join(alpha, "sub", ".codegraph"))
	registerGraphsProject(t, cfgPath, "alpha", alpha)
	fake := stubGraphs(t, nil, nil)

	var stdout, stderr bytes.Buffer
	if code := runGraphs([]string{"--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0 (stdout: %s stderr: %s)", code, stdout.String(), stderr.String())
	}
	assertGraphsCalls(t, fake.toolCalls(),
		graphsCallString("graphify", alpha, "update", alpha),
		graphsCallString("codegraph", alpha, "sync", alpha),
		graphsCallString("codegraph", filepath.Join(alpha, "sub"), "sync", filepath.Join(alpha, "sub")),
	)
	if !strings.Contains(stdout.String(), "[alpha] refreshing...") {
		t.Fatalf("stdout missing header:\n%s", stdout.String())
	}
}

func TestGraphsSkipsGraphifyWithoutGraphJSON(t *testing.T) {
	cfgPath := setupGraphsBrain(t)
	beta := filepath.Join(t.TempDir(), "beta")
	mkGraphsDirs(t, filepath.Join(beta, "graphify-out"))
	registerGraphsProject(t, cfgPath, "beta", beta)
	fake := stubGraphs(t, nil, nil)

	var stdout, stderr bytes.Buffer
	if code := runGraphs([]string{"--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr.String())
	}
	assertGraphsCalls(t, fake.toolCalls())
}

func TestGraphsProjectFilter(t *testing.T) {
	cfgPath := setupGraphsBrain(t)
	root := t.TempDir()
	alpha := filepath.Join(root, "alpha")
	beta := filepath.Join(root, "beta")
	mkGraphsDirs(t, filepath.Join(alpha, ".codegraph"), filepath.Join(beta, ".codegraph"))
	registerGraphsProject(t, cfgPath, "alpha", alpha)
	registerGraphsProject(t, cfgPath, "beta", beta)
	fake := stubGraphs(t, nil, nil)

	var stdout, stderr bytes.Buffer
	if code := runGraphs([]string{"beta", "--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr.String())
	}
	assertGraphsCalls(t, fake.toolCalls(), graphsCallString("codegraph", beta, "sync", beta))

	stdout.Reset()
	stderr.Reset()
	if code := runGraphs([]string{"missing", "--config", cfgPath}, &stdout, &stderr); code != 1 {
		t.Fatalf("unknown project exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `project "missing" not found`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestGraphsInitCreatesMissingIndexes(t *testing.T) {
	cfgPath := setupGraphsBrain(t)
	root := t.TempDir()
	gamma := filepath.Join(root, "gamma")
	delta := filepath.Join(root, "delta")
	mkGraphsDirs(t, gamma, delta)
	writeGraphsFile(t, filepath.Join(delta, "graphify-out", "graph.json"))
	registerGraphsProject(t, cfgPath, "delta", delta)
	registerGraphsProject(t, cfgPath, "gamma", gamma)

	fake := stubGraphs(t, nil, nil)
	var stdout, stderr bytes.Buffer
	if code := runGraphs([]string{"--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("without --init exit = %d (stderr: %s)", code, stderr.String())
	}
	assertGraphsCalls(t, fake.toolCalls(), graphsCallString("graphify", delta, "update", delta))
	if !strings.Contains(stdout.String(), "no graph index found") {
		t.Fatalf("stdout missing hint:\n%s", stdout.String())
	}

	fake.calls = nil
	stdout.Reset()
	stderr.Reset()
	if code := runGraphs([]string{"--init", "--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("--init exit = %d (stderr: %s)", code, stderr.String())
	}
	assertGraphsCalls(t, fake.toolCalls(),
		graphsCallString("graphify", delta, "update", delta),
		graphsCallString("codegraph", delta, "init", delta),
		graphsCallString("graphify", gamma, "extract", gamma, "--code-only"),
		graphsCallString("codegraph", gamma, "init", gamma),
	)
}

func TestGraphsDryRunRunsNothing(t *testing.T) {
	cfgPath := setupGraphsBrain(t)
	alpha := filepath.Join(t.TempDir(), "alpha")
	writeGraphsFile(t, filepath.Join(alpha, "graphify-out", "graph.json"))
	mkGraphsDirs(t, filepath.Join(alpha, ".codegraph"))
	registerGraphsProject(t, cfgPath, "alpha", alpha)
	fake := stubGraphs(t, nil, nil)

	var stdout, stderr bytes.Buffer
	if code := runGraphs([]string{"--dry-run", "--init", "--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr.String())
	}
	if n := len(fake.toolCalls()); n != 0 {
		t.Fatalf("dry-run executed %d commands: %q", n, fake.calls)
	}
	if !strings.Contains(stdout.String(), "would run: graphify update") {
		t.Fatalf("stdout missing planned command:\n%s", stdout.String())
	}
}

func TestGraphsMissingToolSkippedOnce(t *testing.T) {
	cfgPath := setupGraphsBrain(t)
	root := t.TempDir()
	alpha := filepath.Join(root, "alpha")
	writeGraphsFile(t, filepath.Join(alpha, "graphify-out", "graph.json"))
	mkGraphsDirs(t, filepath.Join(alpha, ".codegraph"), filepath.Join(alpha, "sub", ".codegraph"))
	registerGraphsProject(t, cfgPath, "alpha", alpha)
	fake := stubGraphs(t, []string{"codegraph"}, nil)

	var stdout, stderr bytes.Buffer
	if code := runGraphs([]string{"--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	assertGraphsCalls(t, fake.toolCalls(), graphsCallString("graphify", alpha, "update", alpha))
	if n := strings.Count(stdout.String(), "codegraph not installed; skipped"); n != 1 {
		t.Fatalf("notice printed %d times, want 1:\n%s", n, stdout.String())
	}
}

func TestGraphsFailureDoesNotStopRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cfgPath := setupGraphsBrain(t)
	alpha := filepath.Join(t.TempDir(), "alpha")
	writeGraphsFile(t, filepath.Join(alpha, "graphify-out", "graph.json"))
	mkGraphsDirs(t, filepath.Join(alpha, ".codegraph"))
	registerGraphsProject(t, cfgPath, "alpha", alpha)
	fake := stubGraphs(t, nil, []string{"graphify"})

	var stdout, stderr bytes.Buffer
	if code := runGraphs([]string{"--config", cfgPath}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	assertGraphsCalls(t, fake.toolCalls(),
		graphsCallString("graphify", alpha, "update", alpha),
		graphsCallString("codegraph", alpha, "sync", alpha),
	)
	if !strings.Contains(stdout.String(), "graphify update "+alpha+": exit ") {
		t.Fatalf("stdout missing exit status:\n%s", stdout.String())
	}
}

func TestGraphsWritesLogOutsideStdout(t *testing.T) {
	cfgPath := setupGraphsBrain(t)
	alpha := filepath.Join(t.TempDir(), "alpha")
	mkGraphsDirs(t, filepath.Join(alpha, ".codegraph"))
	registerGraphsProject(t, cfgPath, "alpha", alpha)
	stubGraphs(t, nil, nil)
	logPath, err := graphsLogPath()
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runGraphs([]string{"--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "done") {
		t.Fatalf("tool output leaked to stdout:\n%s", stdout.String())
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("log not written: %v", err)
	}
	for _, want := range []string{"[alpha] codegraph sync", "): ok", "done"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("log missing %q:\n%s", want, data)
		}
	}
}

func TestGraphsChangedOnlyUsesGitHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	cfgPath := setupGraphsBrain(t)
	repo := filepath.Join(t.TempDir(), "repo")
	graphJSON := filepath.Join(repo, "graphify-out", "graph.json")
	writeGraphsFile(t, graphJSON)
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")
	registerGraphsProject(t, cfgPath, "repo", repo)
	fake := stubGraphs(t, nil, nil)

	// graph.json newer than HEAD and a clean tree: skipped.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(graphJSON, future, future); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runGraphs([]string{"--changed-only", "--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "[skip] repo - unchanged") {
		t.Fatalf("expected skip:\n%s", stdout.String())
	}
	assertGraphsCalls(t, fake.toolCalls())

	// graph.json older than HEAD: refreshed.
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(graphJSON, past, past); err != nil {
		t.Fatal(err)
	}
	fake.calls = nil
	stdout.Reset()
	if code := runGraphs([]string{"--changed-only", "--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr.String())
	}
	assertGraphsCalls(t, fake.toolCalls(), graphsCallString("graphify", repo, "update", repo))

	// Newer graph.json but a dirty tree: refreshed.
	if err := os.Chtimes(graphJSON, future, future); err != nil {
		t.Fatal(err)
	}
	writeGraphsFile(t, filepath.Join(repo, "untracked.txt"))
	fake.calls = nil
	stdout.Reset()
	if code := runGraphs([]string{"--changed-only", "--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr.String())
	}
	assertGraphsCalls(t, fake.toolCalls(), graphsCallString("graphify", repo, "update", repo))
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

var _ = errors.New
