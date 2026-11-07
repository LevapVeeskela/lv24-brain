package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Managed block markers. A block is every line from a start marker through
// the next end marker; text outside the block is never modified.
const (
	agentsStartPrefix  = "<!-- LV24_SHARED_START"
	agentsEndMarker    = "<!-- LV24_SHARED_END -->"
	agentsUTF8BOM      = "\xEF\xBB\xBF"
	agentsSourceSuffix = "global/AGENTS.md"
)

// Outcome statuses for one target file.
const (
	agentsUpdated   = "updated"
	agentsCreated   = "created"
	agentsUnchanged = "unchanged"
	agentsSkipped   = "skipped"
	agentsConflict  = "conflict"
)

// agentsKind selects what the managed block holds.
type agentsKind int

const (
	// agentsCopy places the canonical Markdown content inside the block.
	agentsCopy agentsKind = iota
	// agentsImport places a single "@<path>" import line inside the block.
	agentsImport
)

// agentsTarget is one file that receives the managed block.
type agentsTarget struct {
	path string
	// dir must exist for a missing file to be created; otherwise the target
	// is skipped. For an existing file it is not consulted.
	dir  string
	kind agentsKind
	// atTop inserts the block at the top of the file (after a BOM). When
	// false the block is appended at the end.
	atTop bool
}

// agentsSource carries the canonical instructions in the forms the targets need.
type agentsSource struct {
	content    string // canonical text, LF endings, no trailing newline
	importLine string // "@<absolute canonical path, forward slashes>"
	startLine  string // start marker, data_dir rendered with forward slashes
}

// block renders the managed block using the given line ending.
func (s agentsSource) block(kind agentsKind, eol string) string {
	body := s.importLine
	if kind == agentsCopy {
		body = strings.ReplaceAll(s.content, "\n", eol)
	}
	return s.startLine + eol + body + eol + agentsEndMarker + eol
}

// agentsOutcome is the planned result for one target before any write.
type agentsOutcome struct {
	status string
	reason string
	data   []byte // full new file content for updated and created
	perm   os.FileMode
}

// runAgents implements "brain agents sync". args excludes the "agents" word.
func runAgents(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printAgentsUsage(stderr)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		printAgentsUsage(stdout)
		return 0
	case "sync":
		return runAgentsSync(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "brain agents: unknown subcommand %q\n\n", args[0])
		printAgentsUsage(stderr)
		return 2
	}
}

func printAgentsUsage(w io.Writer) {
	fmt.Fprint(w, `Keep shared agent instructions identical on every machine.

Usage:
  brain agents sync [--check] [--hooks] [--source <file>] [--root <dir>] [--home <dir>] [--config <brain.yaml>]

The canonical file is <data_dir>/global/AGENTS.md unless --source is given.
Each target receives a managed block between LV24_SHARED markers:
  <root>/AGENTS.md                      canonical content
  <home>/.config/opencode/AGENTS.md     canonical content (if the directory exists)
  <home>/.codex/AGENTS.md               canonical content (if the directory exists)
  <home>/.claude/CLAUDE.md              @import of the canonical file (if the directory exists)

--hooks also installs brain session hooks (off by default; the nightly job does not pass it):
  <home>/.claude/settings.json          SessionStart and SessionEnd hook groups
  <home>/.codex/hooks.json              SessionStart, Stop and SessionEnd hook groups
  <home>/.config/opencode/plugins/      brain-session-end.js, or brain-handoff.js
Other settings and hooks are kept; a one-time <file>.pre-brain-hooks backup is written
before the first change. Codex keeps new hooks disabled until you approve them in Codex;
brain never edits ~/.codex/config.toml.

--check writes nothing and exits 1 if any file would change.
`)
}

func runAgentsSync(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("agents sync", stderr)
	check := fs.Bool("check", false, "write nothing; exit 1 if any file would change")
	source := fs.String("source", "", "canonical instructions file (default: <data_dir>/global/AGENTS.md)")
	root := fs.String("root", "", "workspace root for <root>/AGENTS.md (default: parent of data_dir)")
	home := fs.String("home", "", "home directory for agent config dirs (default: user home)")
	hooks := fs.Bool("hooks", false, "also install brain session hooks for Claude Code, Codex and OpenCode")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "brain agents sync: unexpected arguments: %s\n", strings.Join(positional, " "))
		return 2
	}

	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "agents", err)
	}
	if strings.TrimSpace(cfg.DataDir) == "" {
		return reportError(stderr, "agents", fmt.Errorf("data_dir is not set in %s", *cfgPath))
	}
	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return reportError(stderr, "agents", err)
	}

	sourcePath := *source
	if sourcePath == "" {
		sourcePath = filepath.Join(dataDir, "global", "AGENTS.md")
	}
	sourcePath, err = filepath.Abs(sourcePath)
	if err != nil {
		return reportError(stderr, "agents", err)
	}
	// A new instance has no shared instructions yet; --hooks still installs
	// the session hooks, which do not depend on them.
	_, statErr := os.Stat(sourcePath)
	skipInstructions := *hooks && *source == "" && errors.Is(statErr, os.ErrNotExist)
	content := ""
	if skipInstructions {
		fmt.Fprintf(stdout, "skipped (no shared instructions; create %s to sync them) agent instructions\n", sourcePath)
	} else if content, err = agentsLoadCanonical(sourcePath); err != nil {
		return reportError(stderr, "agents", err)
	}

	rootDir := *root
	if rootDir == "" {
		rootDir = filepath.Dir(dataDir)
	}
	rootDir, err = filepath.Abs(rootDir)
	if err != nil {
		return reportError(stderr, "agents", err)
	}
	homeDir := *home
	if homeDir == "" {
		homeDir, err = os.UserHomeDir()
		if err != nil {
			return reportError(stderr, "agents", err)
		}
	}
	homeDir, err = filepath.Abs(homeDir)
	if err != nil {
		return reportError(stderr, "agents", err)
	}

	src := agentsSource{
		content:    content,
		importLine: "@" + filepath.ToSlash(sourcePath),
		startLine: fmt.Sprintf(`%s: managed by "brain agents sync"; edit %s/%s instead -->`,
			agentsStartPrefix, strings.TrimRight(filepath.ToSlash(dataDir), "/"), agentsSourceSuffix),
	}

	exit := 0
	targets := agentsTargets(rootDir, homeDir)
	if skipInstructions {
		targets = nil
	}
	for _, target := range targets {
		out, err := agentsPlan(target, src)
		if err != nil {
			fmt.Fprintf(stderr, "brain agents: %s: %v\n", target.path, err)
			exit = 1
			continue
		}
		switch out.status {
		case agentsSkipped:
			fmt.Fprintf(stdout, "skipped (%s) %s\n", out.reason, target.path)
			continue
		case agentsUnchanged:
			fmt.Fprintf(stdout, "%s %s\n", agentsUnchanged, target.path)
			continue
		case agentsConflict:
			exit = 1
			fmt.Fprintf(stdout, "%s %s\n", agentsConflict, target.path)
			fmt.Fprintf(stderr, "brain agents: %s: %s; keep exactly one start and one end marker, then rerun\n",
				target.path, out.reason)
			continue
		}
		if *check {
			exit = 1
			verb := "update"
			if out.status == agentsCreated {
				verb = "create"
			}
			fmt.Fprintf(stdout, "would %s %s\n", verb, target.path)
			continue
		}
		if err := agentsWriteFile(target.path, out.data, out.perm); err != nil {
			fmt.Fprintf(stderr, "brain agents: %s: %v\n", target.path, err)
			exit = 1
			continue
		}
		fmt.Fprintf(stdout, "%s %s\n", out.status, target.path)
	}

	if *hooks {
		exe, err := os.Executable()
		if err != nil {
			return reportError(stderr, "agents", err)
		}
		cfgAbs, err := filepath.Abs(*cfgPath)
		if err != nil {
			return reportError(stderr, "agents", err)
		}
		if syncAgentHooks(homeDir, exe, cfgAbs, *check, stdout, stderr) != 0 {
			exit = 1
		}
	}
	return exit
}

// agentsTargets lists the four managed files for a root and home directory.
func agentsTargets(root, home string) []agentsTarget {
	opencode := filepath.Join(home, ".config", "opencode")
	codex := filepath.Join(home, ".codex")
	claude := filepath.Join(home, ".claude")
	return []agentsTarget{
		{path: filepath.Join(root, "AGENTS.md"), dir: root, kind: agentsCopy, atTop: true},
		{path: filepath.Join(opencode, "AGENTS.md"), dir: opencode, kind: agentsCopy, atTop: true},
		{path: filepath.Join(codex, "AGENTS.md"), dir: codex, kind: agentsCopy, atTop: true},
		{path: filepath.Join(claude, "CLAUDE.md"), dir: claude, kind: agentsImport, atTop: false},
	}
}

// agentsLoadCanonical reads the canonical instructions and normalises them
// to LF with no trailing newline. It refuses empty files and files that
// already contain managed-block markers, which would corrupt the targets.
func agentsLoadCanonical(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("canonical instructions not found: %s (create it or pass --source)", path)
	}
	if err != nil {
		return "", err
	}
	text := strings.TrimPrefix(string(raw), agentsUTF8BOM)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("canonical instructions are empty: %s", path)
	}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, agentsStartPrefix) || trimmed == agentsEndMarker {
			return "", fmt.Errorf("canonical instructions must not contain managed-block markers: %s", path)
		}
	}
	return text, nil
}

// agentsPlan computes the result for one target without writing anything.
func agentsPlan(target agentsTarget, src agentsSource) (agentsOutcome, error) {
	data, err := os.ReadFile(target.path)
	if errors.Is(err, os.ErrNotExist) {
		info, statErr := os.Stat(target.dir)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
			return agentsOutcome{status: agentsSkipped, reason: target.dir + " not found"}, nil
		case statErr != nil:
			return agentsOutcome{}, statErr
		case !info.IsDir():
			return agentsOutcome{status: agentsSkipped, reason: target.dir + " is not a directory"}, nil
		}
		return agentsOutcome{
			status: agentsCreated,
			data:   []byte(src.block(target.kind, "\n")),
			perm:   0o644,
		}, nil
	}
	if err != nil {
		return agentsOutcome{}, err
	}
	info, err := os.Stat(target.path)
	if err != nil {
		return agentsOutcome{}, err
	}

	original := string(data)
	text := original
	bom := ""
	if strings.HasPrefix(text, agentsUTF8BOM) {
		bom = agentsUTF8BOM
		text = text[len(bom):]
	}
	eol := "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
	}
	block := src.block(target.kind, eol)

	starts, ends := agentsScan(text)
	var next string
	switch {
	case len(starts) == 0 && len(ends) == 0:
		next = agentsInsert(text, block, eol, target.atTop)
	case len(starts) == 1 && len(ends) == 1 && starts[0] < ends[0][0]:
		next = text[:starts[0]] + block + text[ends[0][1]:]
	default:
		return agentsOutcome{
			status: agentsConflict,
			reason: fmt.Sprintf("found %d start and %d end markers", len(starts), len(ends)),
		}, nil
	}

	final := bom + next
	if final == original {
		return agentsOutcome{status: agentsUnchanged}, nil
	}
	return agentsOutcome{status: agentsUpdated, data: []byte(final), perm: info.Mode().Perm()}, nil
}

// agentsInsert adds a block to a file that has none. At the top it is
// followed by one blank line; at the end it is preceded by one blank line.
func agentsInsert(text, block, eol string, atTop bool) string {
	switch {
	case text == "":
		return block
	case atTop:
		return block + eol + text
	default:
		body := text
		if !strings.HasSuffix(body, "\n") {
			body += eol
		}
		return body + eol + block
	}
}

// agentsScan returns the line-start offset of every start marker and the
// [line start, line end] offsets of every end marker.
func agentsScan(text string) (starts []int, ends [][2]int) {
	offset := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, agentsStartPrefix):
			starts = append(starts, offset)
		case trimmed == agentsEndMarker:
			ends = append(ends, [2]int{offset, offset + len(line)})
		}
		offset += len(line)
	}
	return starts, ends
}

// agentsWriteFile replaces path atomically: it writes a temp file in the same
// directory, syncs it, sets the mode, and renames it over the target.
func agentsWriteFile(path string, data []byte, perm os.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
