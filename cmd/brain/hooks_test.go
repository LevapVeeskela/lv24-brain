package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// hooksTestRepo creates a fresh git repository and isolates git and brain
// from the user's configuration. It skips the test when git is missing.
func hooksTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("BRAIN_CONFIG", "")
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

func hooksTestPath(dir string) string {
	return filepath.Join(dir, ".git", "hooks", "post-commit")
}

func hooksTestRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runHooks(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func hooksTestRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestHooksShellEscape(t *testing.T) {
	in := "a\"b\\c$d`e"
	want := "a\\\"b\\\\c\\$d\\`e"
	if got := hooksShellEscape(in); got != want {
		t.Fatalf("hooksShellEscape(%q) = %q, want %q", in, got, want)
	}
}

func TestHooksInstallContent(t *testing.T) {
	dir := hooksTestRepo(t)
	cfg := filepath.Join(t.TempDir(), "my $cfg dir", "brain.yaml")

	code, stdout, stderr := hooksTestRun(t, "install", "--repo", dir, "--config", cfg)
	if code != 0 {
		t.Fatalf("install exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "installed") {
		t.Fatalf("install stdout = %q, want 'installed'", stdout)
	}
	hook := hooksTestPath(dir)
	content := hooksTestRead(t, hook)
	lines := strings.Split(content, "\n")
	if lines[0] != "#!/bin/sh" {
		t.Fatalf("first line = %q", lines[0])
	}
	if !strings.Contains(content, hookMarker) {
		t.Fatalf("hook lacks marker:\n%s", content)
	}
	if !strings.Contains(content, "event commit") {
		t.Fatalf("hook lacks 'event commit':\n%s", content)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wantExe := hooksShellEscape(filepath.ToSlash(exe))
	wantCfg := strings.ReplaceAll(filepath.ToSlash(cfg), "$", `\$`)
	wantLine := `"` + wantExe + `" event commit --quiet --repo "$(git rev-parse --show-toplevel)" --config "` + wantCfg + `" >/dev/null 2>&1 || true`
	found := false
	for _, line := range lines {
		if line == wantLine {
			found = true
		}
	}
	if !found {
		t.Fatalf("hook missing line\nwant: %s\ngot:\n%s", wantLine, content)
	}
	if strings.Contains(content, "\r") {
		t.Fatal("hook must use LF line endings")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(hook)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o755 {
			t.Fatalf("hook mode = %o, want 0755", mode)
		}
	}
}

func TestHooksInstallWithoutConfigOmitsOption(t *testing.T) {
	dir := hooksTestRepo(t)
	if code, _, stderr := hooksTestRun(t, "install", "--repo", dir); code != 0 {
		t.Fatalf("install exit = %d, stderr = %s", code, stderr)
	}
	content := hooksTestRead(t, hooksTestPath(dir))
	if strings.Contains(content, "--config") {
		t.Fatalf("hook must not contain --config without a known config:\n%s", content)
	}
}

func TestHooksInstallIdempotent(t *testing.T) {
	dir := hooksTestRepo(t)
	if code, _, stderr := hooksTestRun(t, "install", "--repo", dir); code != 0 {
		t.Fatalf("first install exit = %d, stderr = %s", code, stderr)
	}
	first := hooksTestRead(t, hooksTestPath(dir))
	code, stdout, stderr := hooksTestRun(t, "install", "--repo", dir)
	if code != 0 {
		t.Fatalf("second install exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "updated") {
		t.Fatalf("second install stdout = %q, want 'updated'", stdout)
	}
	if second := hooksTestRead(t, hooksTestPath(dir)); second != first {
		t.Fatalf("install is not idempotent:\n%s\n---\n%s", first, second)
	}
}

func TestHooksForeignHookRefusedAndReplacedWithForce(t *testing.T) {
	dir := hooksTestRepo(t)
	hook := hooksTestPath(dir)
	foreign := "#!/bin/sh\necho mine\n"
	if err := os.WriteFile(hook, []byte(foreign), 0o755); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := hooksTestRun(t, "install", "--repo", dir)
	if code != 1 {
		t.Fatalf("install without --force exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "--force") {
		t.Fatalf("stderr = %q, want mention of --force", stderr)
	}
	if got := hooksTestRead(t, hook); got != foreign {
		t.Fatal("foreign hook was modified without --force")
	}

	code, stdout, stderr := hooksTestRun(t, "install", "--repo", dir, "--force")
	if code != 0 {
		t.Fatalf("install --force exit = %d, stderr = %s", code, stderr)
	}
	backup := filepath.Join(dir, ".git", "hooks", "post-commit.lv24-brain-backup")
	if !strings.Contains(stdout, "backed up") {
		t.Fatalf("stdout = %q, want backup report", stdout)
	}
	if got := hooksTestRead(t, backup); got != foreign {
		t.Fatalf("backup content = %q, want foreign hook", got)
	}
	if !strings.Contains(hooksTestRead(t, hook), hookMarker) {
		t.Fatal("hook was not replaced by lv24-brain hook")
	}

	// A second foreign hook must not overwrite the first backup.
	foreign2 := "#!/bin/sh\necho second\n"
	if err := os.WriteFile(hook, []byte(foreign2), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := hooksTestRun(t, "install", "--repo", dir, "--force"); code != 0 {
		t.Fatalf("second --force exit = %d, stderr = %s", code, stderr)
	}
	if got := hooksTestRead(t, backup); got != foreign {
		t.Fatal("existing backup was overwritten")
	}
	backup2 := backup + ".1"
	if got := hooksTestRead(t, backup2); got != foreign2 {
		t.Fatalf("numbered backup content = %q, want second foreign hook", got)
	}
}

func TestHooksUninstallRestoresBackup(t *testing.T) {
	dir := hooksTestRepo(t)
	hook := hooksTestPath(dir)
	foreign := "#!/bin/sh\necho mine\n"
	if err := os.WriteFile(hook, []byte(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := hooksTestRun(t, "install", "--repo", dir, "--force"); code != 0 {
		t.Fatalf("install --force exit = %d, stderr = %s", code, stderr)
	}
	code, stdout, stderr := hooksTestRun(t, "uninstall", "--repo", dir)
	if code != 0 {
		t.Fatalf("uninstall exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "restored backup") {
		t.Fatalf("uninstall stdout = %q, want restore report", stdout)
	}
	if got := hooksTestRead(t, hook); got != foreign {
		t.Fatalf("restored hook = %q, want foreign hook", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "hooks", "post-commit.lv24-brain-backup")); !os.IsNotExist(err) {
		t.Fatalf("backup should be consumed by restore, stat err = %v", err)
	}
}

func TestHooksUninstallRefusesForeignAndRemovesOwn(t *testing.T) {
	dir := hooksTestRepo(t)
	hook := hooksTestPath(dir)
	foreign := "#!/bin/sh\necho mine\n"
	if err := os.WriteFile(hook, []byte(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := hooksTestRun(t, "uninstall", "--repo", dir); code != 1 {
		t.Fatalf("uninstall foreign exit = %d, want 1", code)
	}
	if got := hooksTestRead(t, hook); got != foreign {
		t.Fatal("foreign hook was removed")
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := hooksTestRun(t, "install", "--repo", dir); code != 0 {
		t.Fatalf("install exit = %d, stderr = %s", code, stderr)
	}
	if code, _, stderr := hooksTestRun(t, "uninstall", "--repo", dir); code != 0 {
		t.Fatalf("uninstall exit = %d, stderr = %s", code, stderr)
	}
	if _, err := os.Stat(hook); !os.IsNotExist(err) {
		t.Fatalf("own hook still present, stat err = %v", err)
	}
}

func TestHooksLegacyReplacedWithoutForce(t *testing.T) {
	dir := hooksTestRepo(t)
	hook := hooksTestPath(dir)
	legacy := "#!/bin/sh\npowershell -File brain.ps1 event commit\n"
	if err := os.WriteFile(hook, []byte(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := hooksTestRun(t, "install", "--repo", dir)
	if code != 0 {
		t.Fatalf("install over legacy exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "replaced legacy hook") {
		t.Fatalf("stdout = %q, want 'replaced legacy hook'", stdout)
	}
	if !strings.Contains(hooksTestRead(t, hook), hookMarker) {
		t.Fatal("legacy hook was not replaced")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "hooks", "post-commit.lv24-brain-backup")); !os.IsNotExist(err) {
		t.Fatalf("legacy replacement must not create a backup, stat err = %v", err)
	}
}

func TestHooksStatus(t *testing.T) {
	dir := hooksTestRepo(t)
	hook := hooksTestPath(dir)
	cases := []struct {
		name  string
		setup func(t *testing.T)
		want  string
	}{
		{"absent", func(t *testing.T) {}, "absent"},
		{"installed", func(t *testing.T) {
			if code, _, stderr := hooksTestRun(t, "install", "--repo", dir); code != 0 {
				t.Fatalf("install exit = %d, stderr = %s", code, stderr)
			}
		}, "installed"},
		{"foreign", func(t *testing.T) {
			if err := os.WriteFile(hook, []byte("#!/bin/sh\necho x\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "foreign"},
		{"legacy", func(t *testing.T) {
			if err := os.WriteFile(hook, []byte("brain.ps1 event commit\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "legacy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			code, stdout, stderr := hooksTestRun(t, "status", "--repo", dir)
			if code != 0 {
				t.Fatalf("status exit = %d, stderr = %s", code, stderr)
			}
			if !strings.HasPrefix(stdout, tc.want+" ") {
				t.Fatalf("status stdout = %q, want prefix %q", stdout, tc.want+" ")
			}
			if !strings.Contains(stdout, "post-commit") {
				t.Fatalf("status stdout = %q, want hook path", stdout)
			}
		})
	}
}

func TestHooksUsageErrors(t *testing.T) {
	if code, _, _ := hooksTestRun(t); code != 2 {
		t.Fatalf("no args exit = %d, want 2", code)
	}
	if code, _, _ := hooksTestRun(t, "bogus"); code != 2 {
		t.Fatalf("unknown subcommand exit = %d, want 2", code)
	}
	if code, _, _ := hooksTestRun(t, "status", "extra"); code != 2 {
		t.Fatalf("unexpected argument exit = %d, want 2", code)
	}
}
