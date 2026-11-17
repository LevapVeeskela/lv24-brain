package mergedriver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MergeItems merges two content-addressed raw-zone item files. A shared item
// is immutable and its path is derived from its hash, so two hosts that reach
// the same path are looking at the same item. The only field that legitimately
// differs per host is the observation time; normalizeItem removes it, and in
// the common case both sides then render byte-identically.
//
// If the normalized bytes still differ we keep ours at the canonical path and
// write theirs to a deterministic sibling path derived from its content hash.
// Both sides survive under distinct paths, the result never contains conflict
// markers, and the rebase is not blocked.
func MergeItems(base, ours, theirs []byte, opts Options) (Result, error) {
	if err := checkInputs(base, ours, theirs); err != nil {
		return Result{}, err
	}
	if shortcut, ok := unchangedSide(ours, theirs, base); ok {
		return Result{Merged: normalizeItem(shortcut)}, nil
	}
	oursNormalized := normalizeItem(ours)
	theirsNormalized := normalizeItem(theirs)
	if bytes.Equal(oursNormalized, theirsNormalized) {
		return Result{Merged: theirsNormalized}, nil
	}

	incoming, err := writeIncomingItem(theirsNormalized, opts)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Merged: oursNormalized,
		Conflicts: []Conflict{{
			Kind:   "item",
			Field:  "content",
			Path:   opts.Path,
			Ours:   string(oursNormalized),
			Theirs: string(theirsNormalized) + "\n\nIncoming item kept at: " + incoming,
		}},
	}, nil
}

// normalizeItem strips the host observation time from an item's YAML
// frontmatter and fixes line endings, so the same item rendered on two hosts
// has the same bytes. Values are double-quoted by the store, so dropping the
// whole line is safe and cannot break the frontmatter delimiter.
func normalizeItem(raw []byte) []byte {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.TrimPrefix(text, "\ufeff")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		out := []string{strings.TrimRight(lines[0], " \t")}
		closed := false
		for i := 1; i < len(lines); i++ {
			line := lines[i]
			if strings.TrimSpace(line) == "---" {
				out = append(out, "---")
				for _, body := range lines[i+1:] {
					out = append(out, strings.TrimRight(body, " \t"))
				}
				closed = true
				break
			}
			if strings.HasPrefix(strings.TrimSpace(line), "observed_at:") {
				continue
			}
			out = append(out, strings.TrimRight(line, " \t"))
		}
		if closed {
			return []byte(strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n")
		}
	}
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return []byte(strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n")
}

// writeIncomingItem stores the divergent incoming item under a deterministic
// sibling path. The suffix is the first 12 hex digits of the content hash, so
// the name is stable across hosts and two different items never overwrite one
// another.
func writeIncomingItem(theirs []byte, opts Options) (string, error) {
	dir := opts.OutputDir
	if dir == "" {
		dir = "."
	}
	rel := filepath.FromSlash(opts.Path)
	if strings.TrimSpace(rel) != "" {
		dir = filepath.Join(dir, filepath.Dir(rel))
	}
	stem := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	if stem == "" || stem == "." || stem == string(filepath.Separator) {
		stem = "item"
	}
	sum := sha256.Sum256(theirs)
	name := fmt.Sprintf("%s-%s.md", stem, hex.EncodeToString(sum[:])[:12])
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mergedriver: create item merge directory %s: %w", dir, err)
	}
	if err := os.WriteFile(path, theirs, 0o644); err != nil {
		return "", fmt.Errorf("mergedriver: write incoming item %s: %w", path, err)
	}
	return filepath.ToSlash(path), nil
}
