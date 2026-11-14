package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/mergedriver"
)

// runMergeDriver is the hidden command git invokes for the drivers registered
// in .gitattributes. The argument layout follows the git merge driver
// convention: <driver> %O %A %B %L %P. Driver arguments after %B (%L and %P)
// are optional so a hand-written config also works.
func runMergeDriver(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printMergeDriverUsage(stderr)
		return 2
	}
	driver := strings.ToLower(strings.TrimSpace(args[0]))
	if driver == "help" || driver == "-h" || driver == "--help" {
		printMergeDriverUsage(stdout)
		return 0
	}
	switch driver {
	case "state", "notes", "registry":
	default:
		fmt.Fprintf(stderr, "brain merge-driver: unknown driver %q (want state, notes, or registry)\n", args[0])
		return 2
	}
	if len(args) < 4 {
		printMergeDriverUsage(stderr)
		return 2
	}
	basePath, oursPath, theirsPath := args[1], args[2], args[3]
	relativePath := ""
	if len(args) >= 6 {
		relativePath = args[5]
	}

	base, err := readMergeInput(basePath)
	if err != nil {
		fmt.Fprintf(stderr, "brain merge-driver: %v\n", err)
		return 1
	}
	ours, err := readMergeInput(oursPath)
	if err != nil {
		fmt.Fprintf(stderr, "brain merge-driver: %v\n", err)
		return 1
	}
	theirs, err := readMergeInput(theirsPath)
	if err != nil {
		fmt.Fprintf(stderr, "brain merge-driver: %v\n", err)
		return 1
	}

	now := time.Now().UTC()
	options := mergedriver.Options{Path: filepath.ToSlash(relativePath), Now: now}
	var result mergedriver.Result
	switch driver {
	case "state":
		result, err = mergedriver.MergeState(base, ours, theirs, options)
	case "notes":
		result, err = mergedriver.MergeNotes(base, ours, theirs, options)
	case "registry":
		result, err = mergedriver.MergeRegistry(base, ours, theirs, options)
	}
	if err != nil {
		// Exit 1 makes git fall back to a normal content conflict, which is
		// safer than writing a half-merged file.
		fmt.Fprintf(stderr, "brain merge-driver %s: %v\n", driver, err)
		return 1
	}
	if err := os.WriteFile(oursPath, result.Merged, 0o644); err != nil {
		fmt.Fprintf(stderr, "brain merge-driver: write %s: %v\n", oursPath, err)
		return 1
	}

	conflictDir := mergeConflictDir(driver, relativePath, oursPath)
	for _, conflict := range result.Conflicts {
		path, writeErr := mergedriver.WriteConflict(conflictDir, conflict, now)
		if writeErr != nil {
			fmt.Fprintf(stderr, "brain merge-driver: %v\n", writeErr)
			return 1
		}
		fmt.Fprintf(stderr, "brain merge-driver: %s conflict recorded: %s\n", driver, path)
	}
	return 0
}

func readMergeInput(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, mergedriver.MaxInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > mergedriver.MaxInputBytes {
		return nil, fmt.Errorf("read %s: %w", path, mergedriver.ErrInputTooLarge)
	}
	return data, nil
}

// mergeConflictDir places the conflict record next to the merged file. %P is
// the repository-relative path and git runs the driver in the work tree root,
// so its directory is stable even when %A is a temporary file.
func mergeConflictDir(driver, relativePath, oursPath string) string {
	dir := filepath.Dir(oursPath)
	if strings.TrimSpace(relativePath) != "" && !filepath.IsAbs(relativePath) {
		dir = filepath.Dir(filepath.FromSlash(relativePath))
	}
	switch driver {
	case "state":
		return filepath.Join(dir, "state-conflicts")
	case "registry":
		return filepath.Join(dir, "registry-conflicts")
	default:
		return filepath.Join(dir, "merge-conflicts")
	}
}

func printMergeDriverUsage(w io.Writer) {
	fmt.Fprint(w, `Usage (git merge driver, hidden):
  brain merge-driver state    <base> <ours> <theirs> [<marker-size> <path>]
  brain merge-driver notes    <base> <ours> <theirs> [<marker-size> <path>]
  brain merge-driver registry <base> <ours> <theirs> [<marker-size> <path>]

The command merges the three inputs field by field and rewrites the second
argument (ours). A field both sides changed differently keeps the current value
and is reported in a conflict file next to it. Drivers are registered per clone
by brain init and brain doctor --fix.
`)
}
