package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	hookTestExe = "C:/tools/brain/brain.exe"
	hookTestCfg = "C:/data/brain.yaml"
)

// agentHookFixture is an empty temp home with the agent directories created on demand.
type agentHookFixture struct {
	home     string
	claude   string
	codex    string
	opencode string
}

func newAgentHookFixture(t *testing.T) agentHookFixture {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	return agentHookFixture{
		home:     home,
		claude:   filepath.Join(home, ".claude", "settings.json"),
		codex:    filepath.Join(home, ".codex", "hooks.json"),
		opencode: filepath.Join(home, ".config", "opencode"),
	}
}

func (f agentHookFixture) installAll(t *testing.T) {
	t.Helper()
	for _, dir := range []string{filepath.Dir(f.claude), filepath.Dir(f.codex), f.opencode} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// dataDir sits outside home so a trust file never changes the home layout
// assertions.
func (f agentHookFixture) dataDir() string {
	return filepath.Join(filepath.Dir(f.home), "brain")
}

func runAgentHooksForTest(t *testing.T, home, exe, cfg string, check bool) (int, string, string) {
	t.Helper()
	return runAgentHooksHostsForTest(t, home, exe, cfg, check)
}

func runAgentHooksHostsForTest(t *testing.T, home, exe, cfg string, check bool, hosts ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := syncAgentHooks(agentHookSyncOptions{
		home:    home,
		exe:     exe,
		cfg:     cfg,
		dataDir: filepath.Join(filepath.Dir(home), "brain"),
		check:   check,
		hosts:   hosts,
	}, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// writeHookStatusConfig creates a minimal brain.yaml for agent hook status.
func writeHookStatusConfig(t *testing.T, f agentHookFixture) string {
	t.Helper()
	dataDir := f.dataDir()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	content := "mode: local\ndata_dir: " + filepath.ToSlash(dataDir) + "\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// agentHookCommands returns the commands under hooks.<event> for a JSON file.
func agentHookCommands(t *testing.T, path, event string) []string {
	t.Helper()
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(agentsRead(t, path)), &doc); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	var cmds []string
	for _, g := range doc.Hooks[event] {
		for _, h := range g.Hooks {
			cmds = append(cmds, h.Command)
		}
	}
	return cmds
}

func TestAgentHooksFreshInstallPreservesForeignHooks(t *testing.T) {
	f := newAgentHookFixture(t)
	const original = `{
  "model": "opus",
  "hooks": {
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "orca-guard"}]}],
    "SessionStart": [{"hooks": [{"type": "command", "command": "orca-start", "timeout": 5}]}]
  },
  "env": {"A": "1"}
}
`
	agentsWrite(t, f.claude, original)

	code, stdout, stderr := runAgentHooksForTest(t, f.home, hookTestExe, hookTestCfg, false)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "updated "+f.claude+"\n") {
		t.Errorf("stdout missing updated line:\n%s", stdout)
	}

	got := agentsRead(t, f.claude)
	starts := agentHookCommands(t, f.claude, "SessionStart")
	wantStart := []string{"orca-start", `"C:/tools/brain/brain.exe" session start --hook claude --config C:/data/brain.yaml`}
	if strings.Join(starts, "|") != strings.Join(wantStart, "|") {
		t.Errorf("SessionStart commands = %q, want %q", starts, wantStart)
	}
	ends := agentHookCommands(t, f.claude, "SessionEnd")
	if len(ends) != 1 || ends[0] != `"C:/tools/brain/brain.exe" session end --hook claude --config C:/data/brain.yaml` {
		t.Errorf("SessionEnd commands = %q", ends)
	}
	if agentHookCommands(t, f.claude, "PreToolUse")[0] != "orca-guard" {
		t.Error("foreign PreToolUse hook was not preserved")
	}

	// Key order: top-level keys and hook events keep their original order, new event goes last.
	order := []int{
		strings.Index(got, `"model"`), strings.Index(got, `"hooks"`), strings.Index(got, `"env"`),
	}
	if !(order[0] < order[1] && order[1] < order[2]) {
		t.Errorf("top-level key order changed:\n%s", got)
	}
	pre := strings.Index(got, `"PreToolUse"`)
	start := strings.Index(got, `"SessionStart"`)
	end := strings.Index(got, `"SessionEnd"`)
	if !(pre < start && start < end) {
		t.Errorf("hook event order wrong:\n%s", got)
	}
	if !strings.Contains(got, `"env": {`) || !strings.HasSuffix(got, "}\n") {
		t.Errorf("formatting not 2-space indented with final newline:\n%s", got)
	}

	backup := f.claude + agentHookBackupSuffix
	if agentsRead(t, backup) != original {
		t.Error("backup must hold the original settings.json bytes")
	}
}

func TestAgentHooksSecondRunUnchanged(t *testing.T) {
	f := newAgentHookFixture(t)
	f.installAll(t)
	agentsWrite(t, f.claude, `{"model": "opus"}`)

	if code, _, stderr := runAgentHooksForTest(t, f.home, hookTestExe, hookTestCfg, false); code != 0 {
		t.Fatalf("first run exit = %d (stderr: %s)", code, stderr)
	}
	before := []string{agentsRead(t, f.claude), agentsRead(t, f.codex), agentsRead(t, f.opencode+"/plugins/brain-handoff.js")}

	code, stdout, stderr := runAgentHooksForTest(t, f.home, hookTestExe, hookTestCfg, false)
	if code != 0 {
		t.Fatalf("second run exit = %d (stderr: %s)", code, stderr)
	}
	if strings.Contains(stdout, "updated") || strings.Contains(stdout, "skipped") {
		t.Errorf("second run should report only unchanged:\n%s", stdout)
	}
	if got := strings.Count(stdout, "unchanged "); got != 3 {
		t.Errorf("unchanged lines = %d, want 3:\n%s", got, stdout)
	}
	after := []string{agentsRead(t, f.claude), agentsRead(t, f.codex), agentsRead(t, f.opencode+"/plugins/brain-handoff.js")}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("file %d changed on an in-sync run", i)
		}
	}
}

func TestAgentHooksCodexCreatesFileAndKeepsForeignGroups(t *testing.T) {
	f := newAgentHookFixture(t)
	f.installAll(t)
	agentsWrite(t, filepath.Join(f.home, ".codex", "config.toml"), "trusted = true\n")
	agentsWrite(t, f.codex, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"orca-stop","timeout":3}]}]}}`)

	code, stdout, stderr := runAgentHooksForTest(t, f.home, hookTestExe, hookTestCfg, false)
	if code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, codexHooksNote) {
		t.Errorf("stdout missing codex trust note:\n%s", stdout)
	}

	stops := agentHookCommands(t, f.codex, "Stop")
	if len(stops) != 2 || stops[0] != "orca-stop" ||
		stops[1] != `"C:/tools/brain/brain.exe" session end --hook codex --config C:/data/brain.yaml` {
		t.Errorf("Stop commands = %q", stops)
	}
	if got := agentHookCommands(t, f.codex, "SessionStart"); len(got) != 1 ||
		got[0] != `"C:/tools/brain/brain.exe" session start --hook codex --config C:/data/brain.yaml` {
		t.Errorf("SessionStart commands = %q", got)
	}
	if got := agentHookCommands(t, f.codex, "SessionEnd"); len(got) != 1 ||
		got[0] != `"C:/tools/brain/brain.exe" session end --hook codex --config C:/data/brain.yaml` {
		t.Errorf("SessionEnd commands = %q", got)
	}
	if agentsRead(t, filepath.Join(f.home, ".codex", "config.toml")) != "trusted = true\n" {
		t.Error("config.toml must never be edited")
	}

	// A missing hooks.json is created when the codex directory exists.
	f2 := newAgentHookFixture(t)
	f2.installAll(t)
	if code, _, _ := runAgentHooksForTest(t, f2.home, hookTestExe, hookTestCfg, false); code != 0 {
		t.Fatalf("fresh codex exit = %d", code)
	}
	if _, err := os.Stat(f2.codex); err != nil {
		t.Errorf("hooks.json not created: %v", err)
	}
}

func TestAgentHooksUpdatesPathInPlaceAndBacksUpOnce(t *testing.T) {
	f := newAgentHookFixture(t)
	agentsWrite(t, f.claude, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"orca-start"}]}]}}`)
	original := agentsRead(t, f.claude)

	if code, _, stderr := runAgentHooksForTest(t, f.home, "C:/old/brain.exe", hookTestCfg, false); code != 0 {
		t.Fatalf("first run exit = %d (stderr: %s)", code, stderr)
	}
	code, stdout, stderr := runAgentHooksForTest(t, f.home, hookTestExe, "C:/new/brain.yaml", false)
	if code != 0 {
		t.Fatalf("path update exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "updated "+f.claude) {
		t.Errorf("path change should report updated:\n%s", stdout)
	}

	starts := agentHookCommands(t, f.claude, "SessionStart")
	if len(starts) != 2 {
		t.Fatalf("SessionStart commands = %q, want the foreign one plus one brain hook", starts)
	}
	if starts[0] != "orca-start" {
		t.Errorf("foreign hook moved: %q", starts)
	}
	if starts[1] != `"C:/tools/brain/brain.exe" session start --hook claude --config C:/new/brain.yaml` {
		t.Errorf("brain hook not updated in place: %q", starts[1])
	}
	if agentsRead(t, f.claude+agentHookBackupSuffix) != original {
		t.Error("backup was overwritten; it must hold the first pre-brain content only")
	}
}

func TestAgentHooksCheckExitCodes(t *testing.T) {
	f := newAgentHookFixture(t)
	f.installAll(t)
	agentsWrite(t, f.claude, `{}`)
	before := agentsRead(t, f.claude)

	code, stdout, _ := runAgentHooksForTest(t, f.home, hookTestExe, hookTestCfg, true)
	if code != 1 {
		t.Fatalf("--check exit = %d, want 1 when changes are pending", code)
	}
	if !strings.Contains(stdout, "would update "+f.claude+"\n") || !strings.Contains(stdout, "would create "+f.codex+"\n") {
		t.Errorf("--check output:\n%s", stdout)
	}
	if agentsRead(t, f.claude) != before {
		t.Error("--check must not write")
	}
	if _, err := os.Stat(f.codex); !os.IsNotExist(err) {
		t.Error("--check must not create hooks.json")
	}
	if _, err := os.Stat(f.claude + agentHookBackupSuffix); !os.IsNotExist(err) {
		t.Error("--check must not write a backup")
	}

	if code, _, stderr := runAgentHooksForTest(t, f.home, hookTestExe, hookTestCfg, false); code != 0 {
		t.Fatalf("sync exit = %d (stderr: %s)", code, stderr)
	}
	if code, stdout, _ := runAgentHooksForTest(t, f.home, hookTestExe, hookTestCfg, true); code != 0 {
		t.Errorf("--check after sync exit = %d, want 0:\n%s", code, stdout)
	}
}

func TestAgentHooksSkipsMissingAgents(t *testing.T) {
	f := newAgentHookFixture(t)
	code, stdout, stderr := runAgentHooksForTest(t, f.home, hookTestExe, hookTestCfg, false)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, reason := range []string{"Claude Code not installed", "Codex not installed", "OpenCode not installed"} {
		if !strings.Contains(stdout, "skipped ("+reason+")") {
			t.Errorf("stdout missing skip reason %q:\n%s", reason, stdout)
		}
	}
	if _, err := os.Stat(f.home); !os.IsNotExist(err) {
		t.Error("no agent directory may be created when the agent is absent")
	}
}

func TestAgentHooksRejectsMalformedSettings(t *testing.T) {
	f := newAgentHookFixture(t)
	agentsWrite(t, f.claude, `{"hooks": [1,`)
	code, _, stderr := runAgentHooksForTest(t, f.home, hookTestExe, hookTestCfg, false)
	if code != 1 || !strings.Contains(stderr, "parse JSON") {
		t.Fatalf("exit = %d, stderr = %q; want 1 with a parse error", code, stderr)
	}
	if agentsRead(t, f.claude) != `{"hooks": [1,` {
		t.Error("malformed settings.json must not be modified")
	}
}

func TestOpenCodePluginPlaceholdersAndPreferredName(t *testing.T) {
	f := newAgentHookFixture(t)
	f.installAll(t)
	plugins := filepath.Join(f.opencode, "plugins")

	if code, _, stderr := runAgentHooksForTest(t, f.home, "C:\\tools\\brain.exe", hookTestCfg, false); code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	handoff := filepath.Join(plugins, "brain-handoff.js")
	got := agentsRead(t, handoff)
	if strings.Contains(got, "__BRAIN_BIN__") || strings.Contains(got, "__BRAIN_CONFIG__") {
		t.Error("placeholders were not substituted")
	}
	if !strings.Contains(got, `const BRAIN_BIN = process.env.BRAIN_BIN || "C:/tools/brain.exe";`) {
		t.Errorf("BRAIN_BIN line not substituted with forward slashes")
	}
	if !strings.Contains(got, `"C:/data/brain.yaml"`) {
		t.Error("BRAIN_CONFIG not substituted")
	}

	// An existing brain-session-end.js is preferred and brain-handoff.js is left alone.
	agentsWrite(t, filepath.Join(plugins, "brain-session-end.js"), "// old\n")
	if code, _, _ := runAgentHooksForTest(t, f.home, "C:\\tools\\brain.exe", hookTestCfg, false); code != 0 {
		t.Fatal("second run failed")
	}
	if !strings.Contains(agentsRead(t, filepath.Join(plugins, "brain-session-end.js")), "BRAIN_BIN") {
		t.Error("brain-session-end.js was not updated")
	}
	if agentsRead(t, handoff) != got {
		t.Error("brain-handoff.js must not change when brain-session-end.js is used")
	}
}

func TestRenderOpenCodePluginEscapesForJSString(t *testing.T) {
	got := renderOpenCodePlugin(`a="__BRAIN_BIN__" b="__BRAIN_CONFIG__"`, `C:\x"y`, `C:\cfg`)
	want := `a="C:\\x\"y" b="C:\\cfg"`
	if got != want {
		t.Errorf("render = %s\nwant    %s", got, want)
	}
}

func TestJSONObjectRoundTripKeepsValues(t *testing.T) {
	in := `{"n": 1.50, "s": "a<b&\u00e9", "t": true, "z": null, "arr": [], "obj": {}, "nest": [{"k": [1]}]}`
	root, err := parseJSONObject([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out := string(encodeJSONObject(root))
	var a, b any
	if err := json.Unmarshal([]byte(in), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		t.Fatalf("re-encoded JSON invalid: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"n": 1.50`) || !strings.Contains(out, `"s": "a<b&é"`) {
		t.Errorf("numbers or HTML characters changed:\n%s", out)
	}
	if strings.Index(out, `"n"`) > strings.Index(out, `"nest"`) {
		t.Error("key order changed")
	}
}

// HOK-1: a missing ~/.claude/settings.json is created when the directory
// exists, and --check names created, installed and not installed with paths.
func TestAgentHooksCreatesClaudeSettingsWhenDirExists(t *testing.T) {
	f := newAgentHookFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.claude), 0o755); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runAgentHooksForTest(t, f.home, hookTestExe, hookTestCfg, true)
	if code != 1 {
		t.Fatalf("--check exit = %d, want 1 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "would create "+f.claude+"\n") {
		t.Errorf("--check must report the created settings.json path:\n%s", stdout)
	}
	if _, err := os.Stat(f.claude); !os.IsNotExist(err) {
		t.Fatal("--check created settings.json")
	}

	code, stdout, stderr = runAgentHooksForTest(t, f.home, hookTestExe, hookTestCfg, false)
	if code != 0 {
		t.Fatalf("sync exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "updated "+f.claude+"\n") {
		t.Errorf("sync did not report settings.json:\n%s", stdout)
	}
	if got := agentHookCommands(t, f.claude, "SessionStart"); len(got) != 1 ||
		got[0] != `"C:/tools/brain/brain.exe" session start --hook claude --config C:/data/brain.yaml` {
		t.Errorf("SessionStart commands = %q", got)
	}

	code, stdout, _ = runAgentHooksForTest(t, f.home, hookTestExe, hookTestCfg, true)
	if code != 0 {
		t.Fatalf("--check after install exit = %d:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "installed "+f.claude+"\n") {
		t.Errorf("--check must report installed with the path:\n%s", stdout)
	}
	if !strings.Contains(stdout, "not installed (Codex not installed) "+f.codex+"\n") ||
		!strings.Contains(stdout, "not installed (OpenCode not installed) "+f.opencode) {
		t.Errorf("--check must report not installed with paths:\n%s", stdout)
	}
}

// An explicitly selected host is installed even when its config directory is
// absent, and the other hosts are reported as not selected.
func TestAgentHooksSelectedHostCreatesMissingDir(t *testing.T) {
	f := newAgentHookFixture(t)

	code, stdout, stderr := runAgentHooksHostsForTest(t, f.home, hookTestExe, hookTestCfg, false, "claude")
	if code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "updated "+f.claude+"\n") ||
		!strings.Contains(stdout, "skipped (not selected: codex) "+f.codex+"\n") {
		t.Errorf("selected host output:\n%s", stdout)
	}
	if _, err := os.Stat(f.claude); err != nil {
		t.Fatalf("settings.json not created: %v", err)
	}
	if _, err := os.Stat(f.codex); !os.IsNotExist(err) {
		t.Error("an unselected host must not be installed")
	}
}

// HOK-5: a moved executable changes the Codex command, so the old approval is
// stale and the owner must be told to re-approve.
func TestCodexTrustDriftReportsChangedExe(t *testing.T) {
	f := newAgentHookFixture(t)
	f.installAll(t)

	if code, _, stderr := runAgentHooksForTest(t, f.home, hookTestExe, hookTestCfg, false); code != 0 {
		t.Fatalf("first sync exit = %d (stderr: %s)", code, stderr)
	}
	code, stdout, stderr := runAgentHooksForTest(t, f.home, "C:/moved/brain.exe", hookTestCfg, false)
	if code != 0 {
		t.Fatalf("second sync exit = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "updated "+f.codex+"\n") {
		t.Errorf("changed exe must update hooks.json:\n%s", stdout)
	}
	for _, want := range []string{"trust hash", "re-approve", "sha256:"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("drift warning missing %q:\n%s", want, stderr)
		}
	}
	state := loadAgentHookTrust(f.dataDir())
	desired := ""
	for _, target := range agentHookTargets(f.home, "C:/moved/brain.exe", hookTestCfg) {
		if target.host == "codex" {
			desired = agentHookTrustHash(target, "C:/moved/brain.exe", hookTestCfg)
		}
	}
	if desired == "" || state.Hosts["codex"].Hash != desired {
		t.Errorf("the new Codex trust hash was not recorded: state=%q desired=%q", state.Hosts["codex"].Hash, desired)
	}
}

// HOK-3: hooks status reports installed, the Codex trust hash, last run and
// last error per host.
func TestHooksStatusAgentsReportsHosts(t *testing.T) {
	t.Setenv("BRAIN_HOOK_OFF", "")
	f := newAgentHookFixture(t)
	f.installAll(t)
	cfgPath := writeHookStatusConfig(t, f)
	if code, _, stderr := runAgentHooksForTest(t, f.home, hookTestExe, cfgPath, false); code != 0 {
		t.Fatalf("sync exit = %d (stderr: %s)", code, stderr)
	}
	appendHookLog(f.dataDir(), hookLogEntry{
		Time: time.Now().UTC(), Host: "claude", Event: "session.end", Exit: 0,
	})
	appendHookLog(f.dataDir(), hookLogEntry{
		Time: time.Now().UTC(), Host: "claude", Event: "session.start", Exit: 0,
		Error: "brain session start: token=supersecret boom",
	})

	var stdout, stderr bytes.Buffer
	code := runHooksStatus([]string{"--agents", "--home", f.home, "--config", cfgPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("hooks status --agents exit = %d (stderr: %s)", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"installed", "trust approved sha256:", "last run", "last error: brain session start: token=<redacted> boom",
		f.claude, f.codex, f.opencode,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "supersecret") {
		t.Error("status leaked a secret from the hook log")
	}

	stdout.Reset()
	if code := runHooksStatus([]string{"--agents", "--json", "--home", f.home, "--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("hooks status --agents --json exit = %d", code)
	}
	var envelope struct {
		Data agentHooksStatusReport `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode status JSON: %v\n%s", err, stdout.String())
	}
	if len(envelope.Data.Hosts) != 3 {
		t.Fatalf("hosts = %+v", envelope.Data.Hosts)
	}
	for _, host := range envelope.Data.Hosts {
		if !host.Installed {
			t.Errorf("%s is not installed: %s", host.Host, host.Detail)
		}
		if host.Host == "codex" && (host.Trusted == nil || !*host.Trusted || host.Trust != "approved") {
			t.Errorf("codex trust = %+v", host)
		}
		if host.Host == "claude" && (host.LastRun == nil || host.LastError == "") {
			t.Errorf("claude run log missing: %+v", host)
		}
	}
}

// An externally edited hooks.json is trust drift, and status says how to fix it.
func TestHooksStatusAgentsReportsTrustDrift(t *testing.T) {
	f := newAgentHookFixture(t)
	f.installAll(t)
	cfgPath := writeHookStatusConfig(t, f)
	if code, _, stderr := runAgentHooksForTest(t, f.home, hookTestExe, cfgPath, false); code != 0 {
		t.Fatalf("sync exit = %d (stderr: %s)", code, stderr)
	}
	agentsWrite(t, f.codex, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"\"D:/other/brain.exe\" session start --hook codex --config C:/data/brain.yaml"}]}]}}`)

	var stdout, stderr bytes.Buffer
	if code := runHooksStatus([]string{"--agents", "--home", f.home, "--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr.String())
	}
	for _, want := range []string{"trust drift", "re-approve", f.codex} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("status missing %q:\n%s", want, stdout.String())
		}
	}
}

// HOK-2: the liveness log is one JSON line per hook run, redacts secrets and
// is disabled by BRAIN_HOOK_OFF.
func TestHookLogAppendsRedactsAndHonorsKillSwitch(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("BRAIN_HOOK_OFF", "")
	appendHookLog(dataDir, hookLogEntry{
		Time: time.Now().UTC(), Host: "codex", Event: "session.end",
		DurationMS: 12, Exit: 0, Error: "brain session end: password=hunter2 failed",
	})
	entries := readHookLog(dataDir)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Host != "codex" || entries[0].Event != "session.end" ||
		entries[0].DurationMS != 12 || entries[0].Exit != 0 {
		t.Errorf("entry = %+v", entries[0])
	}
	if strings.Contains(entries[0].Error, "hunter2") || !strings.Contains(entries[0].Error, "<redacted>") {
		t.Errorf("secret not redacted: %q", entries[0].Error)
	}
	raw, err := os.ReadFile(hookLogPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(raw), "\n"); lines != 1 {
		t.Errorf("log lines = %d, want 1 per invocation", lines)
	}

	t.Setenv("BRAIN_HOOK_OFF", "1")
	appendHookLog(dataDir, hookLogEntry{Host: "claude", Event: "session.end"})
	if entries := readHookLog(dataDir); len(entries) != 1 {
		t.Errorf("BRAIN_HOOK_OFF=1 still appended: %+v", entries)
	}
}

func TestRedactHookErrorKeepsFirstLine(t *testing.T) {
	got := redactHookError("first glpat-abcdefgh12345678 line\nsecond line")
	if strings.Contains(got, "glpat-abcdefgh12345678") || strings.Contains(got, "second line") {
		t.Errorf("redact = %q", got)
	}
	if !strings.HasPrefix(got, "first ") {
		t.Errorf("redact dropped the first line: %q", got)
	}
}
