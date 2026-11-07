package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const agentsTestBOM = "\xEF\xBB\xBF"

// agentsFixture is a temp brain plus workspace root and home directory.
// Root is the parent of the data dir, as the command derives it by default.
type agentsFixture struct {
	dataDir   string
	cfgPath   string
	root      string
	home      string
	canonical string
	rootFile  string
	opencode  string
	codex     string
	claude    string
}

func newAgentsFixture(t *testing.T) agentsFixture {
	t.Helper()
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}
	home := filepath.Join(base, "home")
	f := agentsFixture{
		dataDir:   dataDir,
		cfgPath:   filepath.Join(dataDir, "brain.yaml"),
		root:      base,
		home:      home,
		canonical: filepath.Join(dataDir, "global", "AGENTS.md"),
		rootFile:  filepath.Join(base, "AGENTS.md"),
		opencode:  filepath.Join(home, ".config", "opencode", "AGENTS.md"),
		codex:     filepath.Join(home, ".codex", "AGENTS.md"),
		claude:    filepath.Join(home, ".claude", "CLAUDE.md"),
	}
	agentsWrite(t, f.canonical, "# Shared\nRule one.\n")
	return f
}

func (f agentsFixture) args(extra ...string) []string {
	return append([]string{"sync", "--config", f.cfgPath, "--root", f.root, "--home", f.home}, extra...)
}

func runAgentsForTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runAgents(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func agentsWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func agentsRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// agentsStartLine is the expected start marker for the fixture data dir.
func (f agentsFixture) startLine() string {
	return `<!-- LV24_SHARED_START: managed by "brain agents sync"; edit ` +
		filepath.ToSlash(f.dataDir) + `/global/AGENTS.md instead -->`
}

// copyBlock is the expected managed block for a canonical-content target.
func (f agentsFixture) copyBlock(body, eol string) string {
	return f.startLine() + eol + strings.ReplaceAll(body, "\n", eol) + eol +
		"<!-- LV24_SHARED_END -->" + eol
}

// importBlock is the expected managed block for the CLAUDE.md import.
func (f agentsFixture) importBlock() string {
	return f.startLine() + "\n@" + filepath.ToSlash(f.canonical) + "\n<!-- LV24_SHARED_END -->\n"
}

// seedAgentsTargets creates the home config dirs and seeds the existing files
// used by the first-sync test: a BOM AGENTS.md, a CRLF AGENTS.md, an empty
// codex dir, and an LF CLAUDE.md.
func seedAgentsTargets(t *testing.T, f agentsFixture) {
	t.Helper()
	agentsWrite(t, f.rootFile, agentsTestBOM+"before\n")
	agentsWrite(t, f.opencode, "before\r\nafter\r\n")
	if err := os.MkdirAll(filepath.Dir(f.codex), 0o755); err != nil {
		t.Fatal(err)
	}
	agentsWrite(t, f.claude, "before\nafter\n")
}

func TestAgentsSyncCreatesAndInsertsBlocks(t *testing.T) {
	f := newAgentsFixture(t)
	seedAgentsTargets(t, f)

	code, stdout, stderr := runAgentsForTest(t, f.args()...)
	if code != 0 {
		t.Fatalf("sync exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{
		"updated " + f.rootFile + "\n",
		"updated " + f.opencode + "\n",
		"created " + f.codex + "\n",
		"updated " + f.claude + "\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q; got:\n%s", want, stdout)
		}
	}

	// BOM stays first, block goes right after it, then one blank line.
	wantRoot := agentsTestBOM + f.copyBlock("# Shared\nRule one.", "\n") + "\n" + "before\n"
	if got := agentsRead(t, f.rootFile); got != wantRoot {
		t.Errorf("root AGENTS.md:\n got %q\nwant %q", got, wantRoot)
	}

	// CRLF file gets CRLF block lines; the outside text is untouched.
	wantOpencode := f.copyBlock("# Shared\nRule one.", "\r\n") + "\r\n" + "before\r\nafter\r\n"
	if got := agentsRead(t, f.opencode); got != wantOpencode {
		t.Errorf("opencode AGENTS.md:\n got %q\nwant %q", got, wantOpencode)
	}

	// New file: block only, LF endings.
	if got, want := agentsRead(t, f.codex), f.copyBlock("# Shared\nRule one.", "\n"); got != want {
		t.Errorf("codex AGENTS.md:\n got %q\nwant %q", got, want)
	}

	// CLAUDE.md: only the @import line inside the block, appended at the end.
	wantClaude := "before\nafter\n" + "\n" + f.importBlock()
	if got := agentsRead(t, f.claude); got != wantClaude {
		t.Errorf("CLAUDE.md:\n got %q\nwant %q", got, wantClaude)
	}
	if strings.Contains(agentsRead(t, f.claude), "Rule one.") {
		t.Error("CLAUDE.md must hold an @import, not a copy of the canonical content")
	}
}

func TestAgentsSyncSecondRunUnchanged(t *testing.T) {
	f := newAgentsFixture(t)
	seedAgentsTargets(t, f)
	if code, _, stderr := runAgentsForTest(t, f.args()...); code != 0 {
		t.Fatalf("first sync exit = %d (stderr: %s)", code, stderr)
	}
	before := []string{agentsRead(t, f.rootFile), agentsRead(t, f.opencode), agentsRead(t, f.codex), agentsRead(t, f.claude)}

	code, stdout, stderr := runAgentsForTest(t, f.args()...)
	if code != 0 {
		t.Fatalf("second sync exit = %d (stderr: %s)", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 output lines, got %d:\n%s", len(lines), stdout)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "unchanged ") {
			t.Errorf("want unchanged, got %q", line)
		}
	}
	after := []string{agentsRead(t, f.rootFile), agentsRead(t, f.opencode), agentsRead(t, f.codex), agentsRead(t, f.claude)}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("target %d changed on an in-sync run", i)
		}
	}
}

func TestAgentsCheckThenUpdate(t *testing.T) {
	f := newAgentsFixture(t)
	seedAgentsTargets(t, f)
	if code, _, stderr := runAgentsForTest(t, f.args()...); code != 0 {
		t.Fatalf("first sync exit = %d (stderr: %s)", code, stderr)
	}
	before := agentsRead(t, f.codex)
	agentsWrite(t, f.canonical, "# Shared\nRule two.\n")

	code, stdout, _ := runAgentsForTest(t, f.args("--check")...)
	if code != 1 {
		t.Fatalf("--check exit = %d, want 1", code)
	}
	if !strings.Contains(stdout, "would update "+f.codex+"\n") {
		t.Errorf("--check output missing would update line:\n%s", stdout)
	}
	if got := agentsRead(t, f.codex); got != before {
		t.Error("--check must not write files")
	}

	code, stdout, stderr := runAgentsForTest(t, f.args()...)
	if code != 0 {
		t.Fatalf("sync exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "updated "+f.codex+"\n") {
		t.Errorf("sync output missing updated line:\n%s", stdout)
	}
	if got, want := agentsRead(t, f.codex), f.copyBlock("# Shared\nRule two.", "\n"); got != want {
		t.Errorf("codex after update:\n got %q\nwant %q", got, want)
	}
	if code, _, _ := runAgentsForTest(t, f.args("--check")...); code != 0 {
		t.Errorf("--check after sync exit = %d, want 0", code)
	}
}

func TestAgentsReplacesStaleBlockAndKeepsOutsideText(t *testing.T) {
	f := newAgentsFixture(t)
	stale := "<!-- LV24_SHARED_START: managed by \"brain agents sync\"; edit C:/old/global/AGENTS.md instead -->\n" +
		"stale\n<!-- LV24_SHARED_END -->\n"
	agentsWrite(t, f.codex, "head\n"+stale+"tail\n")

	if code, _, stderr := runAgentsForTest(t, f.args()...); code != 0 {
		t.Fatalf("sync exit = %d (stderr: %s)", code, stderr)
	}
	want := "head\n" + f.copyBlock("# Shared\nRule one.", "\n") + "tail\n"
	if got := agentsRead(t, f.codex); got != want {
		t.Errorf("codex:\n got %q\nwant %q", got, want)
	}
}

func TestAgentsConflictLeavesFileUntouched(t *testing.T) {
	cases := map[string]func(block string) string{
		"two start markers": func(block string) string { return block + "mid\n" + block },
		"start without end": func(string) string {
			return "<!-- LV24_SHARED_START: old -->\nstale\n"
		},
		"end without start": func(string) string {
			return "stale\n<!-- LV24_SHARED_END -->\n"
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAgentsFixture(t)
			seedAgentsTargets(t, f)
			content := build(f.copyBlock("old", "\n"))
			agentsWrite(t, f.codex, content)

			code, stdout, stderr := runAgentsForTest(t, f.args()...)
			if code != 1 {
				t.Fatalf("exit = %d, want 1 (stderr: %s)", code, stderr)
			}
			if !strings.Contains(stdout, "conflict "+f.codex+"\n") {
				t.Errorf("stdout missing conflict line:\n%s", stdout)
			}
			if got := agentsRead(t, f.codex); got != content {
				t.Errorf("conflicting file was modified:\n got %q\nwant %q", got, content)
			}
		})
	}
}

func TestAgentsMissingSourceFails(t *testing.T) {
	f := newAgentsFixture(t)
	if err := os.Remove(f.canonical); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runAgentsForTest(t, f.args()...)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "canonical instructions not found") {
		t.Errorf("stderr = %q, want a missing-source message", stderr)
	}
	if _, err := os.Stat(f.rootFile); !os.IsNotExist(err) {
		t.Error("no target may be written when the source is missing")
	}
}

func TestAgentsSkipsOptionalTargetsWithoutDirs(t *testing.T) {
	f := newAgentsFixture(t)
	code, stdout, stderr := runAgentsForTest(t, f.args()...)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "created "+f.rootFile+"\n") {
		t.Errorf("root AGENTS.md should be created:\n%s", stdout)
	}
	if got := strings.Count(stdout, "skipped ("); got != 3 {
		t.Errorf("skipped lines = %d, want 3:\n%s", got, stdout)
	}
	for _, p := range []string{f.opencode, f.codex, f.claude} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s must not be created when its directory is absent", p)
		}
	}
}

func TestAgentsSyncHooksFlagIsOptIn(t *testing.T) {
	f := newAgentsFixture(t)
	settings := filepath.Join(f.home, ".claude", "settings.json")
	agentsWrite(t, settings, "{}\n")

	code, stdout, stderr := runAgentsForTest(t, f.args()...)
	if code != 0 {
		t.Fatalf("sync exit = %d (stderr: %s)", code, stderr)
	}
	if strings.Contains(stdout, "settings.json") || agentsRead(t, settings) != "{}\n" {
		t.Errorf("without --hooks no settings file may be touched:\n%s", stdout)
	}

	code, stdout, stderr = runAgentsForTest(t, f.args("--hooks")...)
	if code != 0 {
		t.Fatalf("sync --hooks exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "updated "+settings+"\n") {
		t.Errorf("--hooks should update settings.json:\n%s", stdout)
	}
	if !strings.Contains(agentsRead(t, settings), " session start --hook claude --config ") {
		t.Error("settings.json lacks the brain SessionStart hook")
	}
}

func TestAgentsUsageAndUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runAgents(nil, &stdout, &stderr); code != 2 {
		t.Errorf("no args exit = %d, want 2", code)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runAgents([]string{"bogus"}, &stdout, &stderr); code != 2 {
		t.Errorf("unknown subcommand exit = %d, want 2", code)
	}
}
