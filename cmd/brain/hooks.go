package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	hookName       = "post-commit"
	hookMarker     = "# lv24-brain post-commit hook"
	hookBackupBase = "post-commit.lv24-brain-backup"
	hooksUsage     = "usage: brain hooks install [--repo <path>] [--force] [--config <brain.yaml>]\n" +
		"       brain hooks uninstall [--repo <path>]\n" +
		"       brain hooks status [--repo <path>]\n"
)

// hookState classifies the post-commit hook file of a repository.
type hookState string

const (
	hookAbsent    hookState = "absent"
	hookInstalled hookState = "installed"
	hookLegacy    hookState = "legacy"
	hookForeign   hookState = "foreign"
)

// runHooks manages the lv24-brain git post-commit hook.
func runHooks(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, hooksUsage)
		return 2
	}
	switch args[0] {
	case "install":
		return runHooksInstall(args[1:], stdout, stderr)
	case "uninstall":
		return runHooksUninstall(args[1:], stdout, stderr)
	case "status":
		return runHooksStatus(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "brain hooks: unknown subcommand %q\n", args[0])
		fmt.Fprint(stderr, hooksUsage)
		return 2
	}
}

func runHooksInstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("hooks install", stderr)
	repo := fs.String("repo", ".", "git repository (default: current directory)")
	force := fs.Bool("force", false, "back up a foreign post-commit hook and replace it")
	cfgPath := fs.String("config", "", "brain.yaml recorded in the hook (default: $BRAIN_CONFIG, else omitted)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain hooks install: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	_, hooksDir, err := hooksLocate(*repo)
	if err != nil {
		fmt.Fprintf(stderr, "brain hooks install: %v\n", err)
		return 1
	}
	script, err := hooksScript(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "brain hooks install: %v\n", err)
		return 1
	}
	hookPath := filepath.Join(hooksDir, hookName)
	state, _, err := hooksReadState(hookPath)
	if err != nil {
		fmt.Fprintf(stderr, "brain hooks install: %v\n", err)
		return 1
	}
	action := "installed"
	var backup string
	switch state {
	case hookInstalled:
		action = "updated"
	case hookLegacy:
		action = "replaced legacy hook"
	case hookForeign:
		if !*force {
			fmt.Fprintf(stderr, "brain hooks install: %s exists and was not written by lv24-brain; rerun with --force to back it up and replace it\n", hookPath)
			return 1
		}
		backup, err = hooksBackup(hooksDir, hookPath)
		if err != nil {
			fmt.Fprintf(stderr, "brain hooks install: %v\n", err)
			return 1
		}
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "brain hooks install: %v\n", err)
		return 1
	}
	if err := hooksWrite(hookPath, script); err != nil {
		fmt.Fprintf(stderr, "brain hooks install: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: %s\n", action, hookPath)
	if backup != "" {
		fmt.Fprintf(stdout, "backed up foreign hook to: %s\n", backup)
	}
	return 0
}

func runHooksUninstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("hooks uninstall", stderr)
	repo := fs.String("repo", ".", "git repository (default: current directory)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain hooks uninstall: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	_, hooksDir, err := hooksLocate(*repo)
	if err != nil {
		fmt.Fprintf(stderr, "brain hooks uninstall: %v\n", err)
		return 1
	}
	hookPath := filepath.Join(hooksDir, hookName)
	state, _, err := hooksReadState(hookPath)
	if err != nil {
		fmt.Fprintf(stderr, "brain hooks uninstall: %v\n", err)
		return 1
	}
	switch state {
	case hookAbsent:
		fmt.Fprintf(stdout, "nothing to uninstall: %s\n", hookPath)
		return 0
	case hookForeign:
		fmt.Fprintf(stderr, "brain hooks uninstall: refusing to remove %s: it was not written by lv24-brain\n", hookPath)
		return 1
	}
	if err := os.Remove(hookPath); err != nil {
		fmt.Fprintf(stderr, "brain hooks uninstall: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "removed: %s\n", hookPath)
	if idx := hooksLatestBackupIndex(hooksDir); idx >= 0 {
		src := hooksBackupName(hooksDir, idx)
		if err := os.Rename(src, hookPath); err != nil {
			fmt.Fprintf(stderr, "brain hooks uninstall: restore %s: %v\n", src, err)
			return 1
		}
		fmt.Fprintf(stdout, "restored backup: %s\n", src)
	}
	return 0
}

func runHooksStatus(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("hooks status", stderr)
	repo := fs.String("repo", ".", "git repository (default: current directory)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain hooks status: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	_, hooksDir, err := hooksLocate(*repo)
	if err != nil {
		fmt.Fprintf(stderr, "brain hooks status: %v\n", err)
		return 1
	}
	hookPath := filepath.Join(hooksDir, hookName)
	state, _, err := hooksReadState(hookPath)
	if err != nil {
		fmt.Fprintf(stderr, "brain hooks status: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s %s\n", state, hookPath)
	return 0
}

// hooksLocate returns the repository toplevel and its hooks directory, as
// resolved by git (handles worktrees and core.hooksPath).
func hooksLocate(repo string) (string, string, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", "", err
	}
	top, err := hooksGit(abs, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", fmt.Errorf("%s is not a git repository: %w", repo, err)
	}
	top = filepath.FromSlash(top)
	dir, err := hooksGit(top, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return "", "", err
	}
	dir = filepath.FromSlash(dir)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(top, dir)
	}
	return top, dir, nil
}

func hooksGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// hooksScript renders the post-commit hook. configPath is the --config value;
// when empty, $BRAIN_CONFIG is used; when both are empty the option is omitted.
func hooksScript(configPath string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve brain executable: %w", err)
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	cfg := strings.TrimSpace(configPath)
	if cfg == "" {
		cfg = strings.TrimSpace(os.Getenv("BRAIN_CONFIG"))
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(hookMarker + " (managed by `brain hooks install`; do not edit)\n")
	b.WriteString(`"` + hooksShellEscape(filepath.ToSlash(exe)) + `" event commit --quiet --repo "$(git rev-parse --show-toplevel)"`)
	if cfg != "" {
		abs, err := filepath.Abs(cfg)
		if err != nil {
			return "", fmt.Errorf("resolve config path: %w", err)
		}
		b.WriteString(` --config "` + hooksShellEscape(filepath.ToSlash(abs)) + `"`)
	}
	b.WriteString(" >/dev/null 2>&1 || true\n")
	return b.String(), nil
}

var hooksShellReplacer = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")

// hooksShellEscape escapes a value for use inside sh double quotes.
func hooksShellEscape(s string) string {
	return hooksShellReplacer.Replace(s)
}

// hooksClassify decides which kind of hook the file content represents.
func hooksClassify(content string) hookState {
	if strings.Contains(content, hookMarker) {
		return hookInstalled
	}
	if strings.Contains(content, "brain.ps1") && strings.Contains(content, "event commit") {
		return hookLegacy
	}
	return hookForeign
}

// hooksReadState returns the state of the hook file and its content.
func hooksReadState(path string) (hookState, string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return hookAbsent, "", nil
	}
	if err != nil {
		return "", "", err
	}
	content := string(data)
	return hooksClassify(content), content, nil
}

func hooksWrite(path, script string) error {
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}

// hooksBackupName returns the backup path for index i: 0 is the bare name,
// n > 0 gets the numeric suffix ".n".
func hooksBackupName(dir string, i int) string {
	name := filepath.Join(dir, hookBackupBase)
	if i > 0 {
		name += "." + strconv.Itoa(i)
	}
	return name
}

// hooksLatestBackupIndex returns the highest existing backup index, or -1.
func hooksLatestBackupIndex(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return -1
	}
	latest := -1
	for _, entry := range entries {
		name := entry.Name()
		idx := -1
		switch {
		case name == hookBackupBase:
			idx = 0
		case strings.HasPrefix(name, hookBackupBase+"."):
			if n, err := strconv.Atoi(strings.TrimPrefix(name, hookBackupBase+".")); err == nil && n > 0 {
				idx = n
			}
		}
		if idx > latest {
			latest = idx
		}
	}
	return latest
}

// hooksBackup moves an existing foreign hook to the next free backup name.
// Existing backups are never overwritten.
func hooksBackup(dir, hookPath string) (string, error) {
	idx := hooksLatestBackupIndex(dir) + 1
	dst := hooksBackupName(dir, idx)
	for {
		if _, err := os.Lstat(dst); os.IsNotExist(err) {
			break
		}
		idx++
		dst = hooksBackupName(dir, idx)
	}
	if err := os.Rename(hookPath, dst); err != nil {
		return "", fmt.Errorf("back up %s: %w", hookPath, err)
	}
	return dst, nil
}
