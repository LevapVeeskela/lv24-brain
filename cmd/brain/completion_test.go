package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// completionHelpExempt lists top-level commands whose subcommands are not
// checked against --help. Their --help does not print flags through a
// flag.FlagSet, and running it would act (config clear, secret list), so the
// table lists them without flags.
var completionHelpExempt = map[string]string{
	"config": "subcommands take no flags; `config clear --help` would clear the pointer",
	"secret": "subcommands take no flags; `secret list --help` would read the OS secret store",
}

// helpFlagLine matches the "  -name type" lines Go's flag package prints for
// each defined flag.
var helpFlagLine = regexp.MustCompile(`(?m)^  -([A-Za-z][A-Za-z0-9-]*)`)

func helpFlagNames(help string) map[string]bool {
	names := map[string]bool{}
	for _, m := range helpFlagLine.FindAllStringSubmatch(help, -1) {
		names[m[1]] = true
	}
	return names
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestCompletionTableMatchesHelp(t *testing.T) {
	for _, leaf := range completionLeaves() {
		path := leaf.key()
		if reason, ok := completionHelpExempt[leaf.path[0]]; ok {
			t.Run(path, func(t *testing.T) { t.Skip(reason) })
			continue
		}
		t.Run(path, func(t *testing.T) {
			args := append(append([]string{}, leaf.path...), "--help")
			code, _, stderr := runWithInputForTest(t, "", args...)
			if code != 0 {
				t.Fatalf("brain %s --help: exit %d, want 0", path, code)
			}
			printed := helpFlagNames(stderr)
			listed := map[string]bool{}
			for _, f := range leaf.flags {
				listed[f.name] = true
			}
			for _, name := range sortedKeys(listed) {
				if !printed[name] {
					t.Errorf("completion table lists --%s for %q, but --help does not print it", name, path)
				}
			}
			for _, name := range sortedKeys(printed) {
				if !listed[name] {
					t.Errorf("--help of %q prints -%s, but the completion table does not list it", path, name)
				}
			}
		})
	}
}

// usageCommandNames returns the first word of each line in the Commands block
// of the top-level usage text.
func usageCommandNames(usage string) []string {
	var names []string
	inCommands := false
	for _, line := range strings.Split(usage, "\n") {
		switch {
		case line == "Commands:":
			inCommands = true
		case line == "" && inCommands:
			return names
		case inCommands:
			if fields := strings.Fields(line); len(fields) > 0 {
				names = append(names, fields[0])
			}
		}
	}
	return names
}

func TestCompletionTableCoversUsageCommands(t *testing.T) {
	var usage bytes.Buffer
	printUsage(&usage)
	names := usageCommandNames(usage.String())
	if len(names) == 0 {
		t.Fatal("no commands parsed from printUsage")
	}
	known := map[string]bool{}
	for _, name := range topLevelNames() {
		known[name] = true
	}
	for _, name := range names {
		if !known[name] {
			t.Errorf("printUsage lists command %q, but the completion table does not", name)
		}
	}
}

func TestCompletionScriptsNameCommandsAndFlags(t *testing.T) {
	cases := []struct {
		shell string
		want  []string
	}{
		{"bash", []string{"complete -F _brain_complete brain", "project", "record", "--kind", "note decision attempt handoff", "proposed accepted rejected superseded", "powershell"}},
		{"zsh", []string{"#compdef brain", "_arguments", "'--kind=:kind:(note decision attempt handoff)'", "record status", "compdef _brain brain", "powershell"}},
		{"fish", []string{"complete -c brain", "-l kind", "__brain_at record list", "__brain_pos_record_status", "powershell"}},
		{"powershell", []string{"Register-ArgumentCompleter -Native -CommandName brain,brain.exe", "'--kind='", "'record status'", "'proposed', 'accepted'", "powershell"}},
	}
	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			code, out, errOut := runWithInputForTest(t, "", "completion", tc.shell)
			if code != 0 {
				t.Fatalf("completion %s: exit %d, stderr %q", tc.shell, code, errOut)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("completion %s output does not contain %q", tc.shell, want)
				}
			}
		})
	}
}

func TestCompletionBashSyntax(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not on PATH")
	}
	cmd := exec.Command(bash, "-n")
	cmd.Stdin = strings.NewReader(renderBash())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

func TestCompletionPowerShellParses(t *testing.T) {
	ps, err := exec.LookPath("powershell")
	if err != nil {
		t.Skip("powershell is not on PATH")
	}
	path := filepath.Join(t.TempDir(), "brain-completion.ps1")
	if err := os.WriteFile(path, []byte(renderPowerShell()), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "[scriptblock]::Create((Get-Content -Raw -LiteralPath '" + path + "')) | Out-Null"
	if out, err := exec.Command(ps, "-NoProfile", "-Command", script).CombinedOutput(); err != nil {
		t.Fatalf("powershell parse: %v\n%s", err, out)
	}
}

func TestCompletionCommandUsageAndExitCodes(t *testing.T) {
	if code, _, errOut := runWithInputForTest(t, "", "completion"); code != 2 || !strings.Contains(errOut, "missing shell") {
		t.Errorf("completion without shell: exit %d, stderr %q; want 2 with usage", code, errOut)
	}
	if code, _, errOut := runWithInputForTest(t, "", "completion", "tcsh"); code != 2 || !strings.Contains(errOut, `unknown shell "tcsh"`) {
		t.Errorf("completion tcsh: exit %d, stderr %q; want 2 naming the shell", code, errOut)
	}
	if code, _, errOut := runWithInputForTest(t, "", "completion", "bash", "zsh"); code != 2 {
		t.Errorf("completion bash zsh: exit %d, stderr %q; want 2", code, errOut)
	}
	for _, args := range [][]string{{"completion", "--help"}, {"completion", "bash", "--help"}} {
		code, out, _ := runWithInputForTest(t, "", args...)
		if code != 0 || !strings.Contains(out, "Usage: brain completion") {
			t.Errorf("%v: exit %d, stdout %q; want 0 with usage", args, code, out)
		}
	}
}
