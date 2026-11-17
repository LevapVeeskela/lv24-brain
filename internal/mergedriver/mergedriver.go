// Package mergedriver implements the deterministic three-way merges that the
// brain data repository registers as git merge drivers.
//
// The drivers keep single mutable files mergeable across machines: state.md is
// merged per field, notes files are merged as a union of dated entries, and
// registry.json is merged as a keyed project catalogue. A field both sides
// changed differently is never dropped silently: the driver keeps the current
// value in the file and writes a conflict record next to it.
package mergedriver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/markdown"
)

// MaxInputBytes bounds one driver input. A driver must never spend unbounded
// memory on a corrupted or binary file: git falls back to a normal conflict.
const MaxInputBytes = 8 << 20

// ErrInputTooLarge reports an input over MaxInputBytes.
var ErrInputTooLarge = errors.New("mergedriver: input exceeds 8 MiB")

// Options carries driver metadata.
type Options struct {
	// Path is the repository-relative path git passed as %P. It is recorded in
	// conflict files and used to place them next to the merged file.
	Path string
	// OutputDir is the base directory for files a driver writes next to the
	// merged file. Git runs drivers from the work tree root, so the CLI leaves
	// it empty; tests point it at a temporary directory.
	OutputDir string
	// Now stamps conflict file names. Zero uses time.Now.
	Now time.Time
}

// Conflict describes one field both sides changed differently. The merged file
// keeps Ours; the record carries all three values so nothing is lost. Machine
// names the host that recorded the conflict when the driver could read one.
type Conflict struct {
	Kind    string
	Field   string
	Base    string
	Ours    string
	Theirs  string
	Path    string
	Machine string
}

// Result is the output of one driver run.
type Result struct {
	Merged    []byte
	Conflicts []Conflict
}

// MergeState merges projects/<id>/state.md field by field. goal and
// next_action are three-way scalars; active, blocked and open questions are
// ordered-set unions. A both-changed scalar keeps ours and is reported in
// Conflict; the driver still exits success because the merged file is the
// deterministic result of the merge.
func MergeState(base, ours, theirs []byte, opts Options) (Result, error) {
	if err := checkInputs(base, ours, theirs); err != nil {
		return Result{}, err
	}
	if shortcut, ok := unchangedSide(ours, theirs, base); ok {
		return Result{Merged: shortcut}, nil
	}

	baseDoc, err := parseState(base)
	if err != nil {
		return Result{}, fmt.Errorf("mergedriver: parse base state: %w", err)
	}
	oursDoc, err := parseState(ours)
	if err != nil {
		return Result{}, fmt.Errorf("mergedriver: parse current state: %w", err)
	}
	theirsDoc, err := parseState(theirs)
	if err != nil {
		return Result{}, fmt.Errorf("mergedriver: parse incoming state: %w", err)
	}

	baseFields := stateFieldsOf(baseDoc)
	oursFields := stateFieldsOf(oursDoc)
	theirsFields := stateFieldsOf(theirsDoc)

	var conflicts []Conflict
	merged := oursFields
	merged.Goal, conflicts = mergeScalar("goal", baseFields.Goal, oursFields.Goal, theirsFields.Goal, opts, conflicts)
	merged.NextAction, conflicts = mergeScalar("next-action", baseFields.NextAction, oursFields.NextAction, theirsFields.NextAction, opts, conflicts)
	merged.Active = mergeItems(baseFields.Active, oursFields.Active, theirsFields.Active)
	merged.Blocked = mergeItems(baseFields.Blocked, oursFields.Blocked, theirsFields.Blocked)
	merged.OpenQuestions = mergeItems(baseFields.OpenQuestions, oursFields.OpenQuestions, theirsFields.OpenQuestions)

	extras, extraConflicts := mergeExtraSections(baseDoc, oursDoc, theirsDoc, opts)
	conflicts = append(conflicts, extraConflicts...)

	frontmatter := oursDoc.frontmatter
	if frontmatter == nil {
		frontmatter = map[string]any{}
	}
	if later := laterTime(frontmatterTime(oursDoc.frontmatter), frontmatterTime(theirsDoc.frontmatter)); !later.IsZero() {
		frontmatter["updated_at"] = later.UTC().Format(time.RFC3339Nano)
	}

	body, err := renderState(oursDoc.preamble, merged, extras)
	if err != nil {
		return Result{}, err
	}
	raw, err := markdown.Render(frontmatter, body)
	if err != nil {
		return Result{}, fmt.Errorf("mergedriver: render state: %w", err)
	}
	return Result{Merged: raw, Conflicts: conflicts}, nil
}

// MergeNotes merges a dated notes file as an ordered union of entries. An entry
// is a brain:note marker block, or a level-2 heading block in a legacy file.
// Entries are deduplicated by heading plus body hash and ordered by their
// marker created_at time.
func MergeNotes(base, ours, theirs []byte, opts Options) (Result, error) {
	if err := checkInputs(base, ours, theirs); err != nil {
		return Result{}, err
	}
	if shortcut, ok := unchangedSide(ours, theirs, base); ok {
		return Result{Merged: shortcut}, nil
	}

	baseDoc, err := parseNotes(base)
	if err != nil {
		return Result{}, fmt.Errorf("mergedriver: parse base notes: %w", err)
	}
	oursDoc, err := parseNotes(ours)
	if err != nil {
		return Result{}, fmt.Errorf("mergedriver: parse current notes: %w", err)
	}
	theirsDoc, err := parseNotes(theirs)
	if err != nil {
		return Result{}, fmt.Errorf("mergedriver: parse incoming notes: %w", err)
	}

	entries := mergeNoteEntries(baseDoc.entries, oursDoc.entries, theirsDoc.entries)
	frontmatter := oursDoc.frontmatter
	if frontmatter == nil {
		frontmatter = map[string]any{}
	}
	if later := laterTime(frontmatterTime(oursDoc.frontmatter), frontmatterTime(theirsDoc.frontmatter)); !later.IsZero() {
		frontmatter["updated_at"] = later.UTC().Format(time.RFC3339Nano)
	}

	raw, err := renderNotes(frontmatter, entries)
	if err != nil {
		return Result{}, err
	}
	return Result{Merged: raw}, nil
}

// MergeRegistry merges registry.json as a keyed union of projects. Both sides'
// records are kept; the record with the later updated_at wins on a field that
// both changed, and unknown keys from the losing record are preserved. Paths
// and aliases are unions across machines, each path keeping the machine id
// that registered it. An alias or path claimed by two projects stays on one
// record and is reported so nothing is lost silently.
func MergeRegistry(base, ours, theirs []byte, opts Options) (Result, error) {
	if err := checkInputs(base, ours, theirs); err != nil {
		return Result{}, err
	}
	if shortcut, ok := unchangedSide(ours, theirs, base); ok {
		return Result{Merged: shortcut}, nil
	}

	baseDoc, err := parseRegistry(base)
	if err != nil {
		return Result{}, fmt.Errorf("mergedriver: parse base registry: %w", err)
	}
	oursDoc, err := parseRegistry(ours)
	if err != nil {
		return Result{}, fmt.Errorf("mergedriver: parse current registry: %w", err)
	}
	theirsDoc, err := parseRegistry(theirs)
	if err != nil {
		return Result{}, fmt.Errorf("mergedriver: parse incoming registry: %w", err)
	}

	merged, conflicts := mergeRegistryProjects(baseDoc, oursDoc, theirsDoc, opts)
	resolveClaimConflicts(merged, opts, &conflicts)

	raw, err := renderRegistry(oursDoc, theirsDoc, merged)
	if err != nil {
		return Result{}, err
	}
	return Result{Merged: raw, Conflicts: conflicts}, nil
}

func renderNotes(frontmatter map[string]any, entries []noteEntry) ([]byte, error) {
	blocks := make([]string, 0, len(entries))
	for _, entry := range entries {
		if block := strings.TrimSpace(entry.raw); block != "" {
			blocks = append(blocks, block)
		}
	}
	raw, err := markdown.Render(frontmatter, strings.Join(blocks, "\n\n"))
	if err != nil {
		return nil, fmt.Errorf("mergedriver: render notes: %w", err)
	}
	return raw, nil
}

func checkInputs(inputs ...[]byte) error {
	for _, input := range inputs {
		if len(input) > MaxInputBytes {
			return ErrInputTooLarge
		}
	}
	return nil
}

// unchangedSide returns the other side when one side equals the base. That is
// the common rebase case and keeps the untouched file byte-for-byte.
func unchangedSide(ours, theirs, base []byte) ([]byte, bool) {
	if bytes.Equal(ours, base) {
		return theirs, true
	}
	if bytes.Equal(theirs, base) {
		return ours, true
	}
	if len(bytes.TrimSpace(base)) == 0 {
		if len(bytes.TrimSpace(ours)) == 0 {
			return theirs, true
		}
		if len(bytes.TrimSpace(theirs)) == 0 {
			return ours, true
		}
	}
	return nil, false
}

// WriteConflict writes one conflict record to dir and returns its path. The
// file name is <UTC>-<field>.md; a second conflict for the same second and
// field gets a numeric suffix instead of overwriting the first.
func WriteConflict(dir string, conflict Conflict, now time.Time) (string, error) {
	if now.IsZero() {
		now = time.Now()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mergedriver: create conflict directory %s: %w", dir, err)
	}
	stamp := now.UTC().Format("20060102T150405Z")
	field := slug(conflict.Field)
	if field == "" {
		field = "field"
	}
	base := filepath.Join(dir, stamp+"-"+field)
	path := base + ".md"
	for suffix := 2; ; suffix++ {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			break
		}
		path = fmt.Sprintf("%s-%d.md", base, suffix)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# Merge conflict: %s %s\n\n", conflict.Kind, conflict.Field)
	if conflict.Path != "" {
		fmt.Fprintf(&out, "- path: %s\n", conflict.Path)
	}
	if machine := strings.TrimSpace(conflict.Machine); machine != "" {
		fmt.Fprintf(&out, "- machine: %s\n", machine)
	}
	fmt.Fprintf(&out, "- field: %s\n", conflict.Field)
	fmt.Fprintf(&out, "- recorded_at: %s\n", now.UTC().Format(time.RFC3339))
	out.WriteString("\nThe current value was kept in the file. Compare both sides and edit the\n")
	out.WriteString("file if the incoming value is the correct one.\n")
	writeConflictValue(&out, "Current (kept)", conflict.Ours)
	writeConflictValue(&out, "Incoming", conflict.Theirs)
	if conflict.Base != "" {
		writeConflictValue(&out, "Base", conflict.Base)
	}
	if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
		return "", fmt.Errorf("mergedriver: write conflict %s: %w", path, err)
	}
	return path, nil
}

func writeConflictValue(out *strings.Builder, title, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	fmt.Fprintf(out, "\n## %s\n\n%s\n", title, strings.TrimSpace(value))
}

func slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
			lastDash = false
		case !lastDash:
			out.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(out.String(), "-")
}

func entryHash(heading, content string) string {
	sum := sha256.Sum256([]byte(heading + "\x00" + content))
	return hex.EncodeToString(sum[:])
}

// frontmatterTime reads updated_at from either a decoded YAML timestamp or a
// quoted RFC 3339 string.
func frontmatterTime(frontmatter map[string]any) time.Time {
	if frontmatter == nil {
		return time.Time{}
	}
	switch value := frontmatter["updated_at"].(type) {
	case time.Time:
		return value.UTC()
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
				return parsed.UTC()
			}
		}
	}
	return time.Time{}
}

func laterTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}
