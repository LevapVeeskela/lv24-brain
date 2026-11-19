package graphalgo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/winproc"
)

// maxDiffOutputBytes bounds the raw --name-status output of one diff. A diff
// above it is refused instead of silently truncated: a partial changed-file
// list would understate the blast radius.
const maxDiffOutputBytes = 8 << 20

// maxRefRunes bounds one git ref so an argument can never grow without limit.
const maxRefRunes = 256

// ChangedFile is one file of a git diff.
type ChangedFile struct {
	Path   string
	Status string
}

// errDiffOutputTooLarge marks a diff whose name-status output exceeded the cap.
var errDiffOutputTooLarge = errors.New("git diff output is too large")

// ChangedFiles reads the files changed between two git refs with a read-only
// `git diff --name-status`. It never writes to the checkout and never fetches:
// both refs must already resolve locally.
//
// Rename detection is disabled so a rename is reported as a delete plus an add,
// which is what the caller must know. Paths come from -z output, so names with
// spaces and non-ASCII bytes survive. The list is sorted by path and cut to
// maxFiles; total is the number of files git reported, so a truncated list is
// disclosed instead of silently understating the diff.
func ChangedFiles(ctx context.Context, root, base, head string, maxFiles int) (files []ChangedFile, total int, truncated bool, err error) {
	base = strings.TrimSpace(base)
	head = strings.TrimSpace(head)
	if head == "" {
		head = "HEAD"
	}
	if err := ValidateRef("base", base); err != nil {
		return nil, 0, false, err
	}
	if err := ValidateRef("head", head); err != nil {
		return nil, 0, false, err
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, 0, false, domain.NewValidationError("graph.root", "a checkout root is required for a git diff")
	}
	command := exec.CommandContext(ctx, "git", "-C", root,
		"diff", "--name-status", "--no-renames", "--no-ext-diff", "-z", base, head)
	winproc.Hide(command)
	stdout := &limitedBuffer{limit: maxDiffOutputBytes}
	stderr := &limitedBuffer{limit: 4 << 10}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if stdout.overflow {
			return nil, 0, false, fmt.Errorf("graphalgo: git diff %s..%s: %w (limit %d bytes)", base, head, errDiffOutputTooLarge, maxDiffOutputBytes)
		}
		detail := strings.TrimSpace(stderr.buffer.String())
		if detail != "" {
			return nil, 0, false, fmt.Errorf("graphalgo: git diff %s..%s: %w: %s", base, head, err, detail)
		}
		return nil, 0, false, fmt.Errorf("graphalgo: git diff %s..%s: %w", base, head, err)
	}
	files = parseNameStatus(stdout.buffer.Bytes())
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	total = len(files)
	truncated = maxFiles > 0 && len(files) > maxFiles
	if truncated {
		files = files[:maxFiles]
	}
	return files, total, truncated, nil
}

// ValidateRef refuses a git ref that could be read as an option, a revision
// range or a path. It is exported so an adapter can validate before it opens a
// store.
func ValidateRef(field, ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return domain.NewValidationError("graph."+field, "is required: pass two git refs, for example base=HEAD~1 head=HEAD")
	}
	if len([]rune(ref)) > maxRefRunes {
		return domain.NewValidationError("graph."+field, "is longer than 256 characters")
	}
	if strings.HasPrefix(ref, "-") {
		return domain.NewValidationError("graph."+field, "must not start with '-'")
	}
	if strings.Contains(ref, "..") || strings.Contains(ref, ":") {
		return domain.NewValidationError("graph."+field, "must be one ref, not a range or a path")
	}
	for _, char := range ref {
		if char < 0x20 || char == 0x7f {
			return domain.NewValidationError("graph."+field, "must not contain control characters")
		}
		if char == ' ' || char == '\t' {
			return domain.NewValidationError("graph."+field, "must not contain whitespace")
		}
	}
	return nil
}

// parseNameStatus reads -z name-status records: STATUS NUL PATH NUL. Rename
// detection is off, so every record is one status and one path.
func parseNameStatus(raw []byte) []ChangedFile {
	tokens := bytes.Split(raw, []byte{0})
	files := make([]ChangedFile, 0, len(tokens)/2)
	for index := 0; index+1 < len(tokens); index += 2 {
		status := strings.TrimSpace(string(tokens[index]))
		path := string(tokens[index+1])
		if status == "" || path == "" {
			continue
		}
		files = append(files, ChangedFile{Path: filepath.ToSlash(path), Status: statusLetter(status)})
	}
	return files
}

// statusLetter keeps the one-letter git status; a combined or unknown token is
// reported as "?" instead of being mistaken for A, M or D.
func statusLetter(status string) string {
	letter := strings.ToUpper(strings.TrimSpace(status))
	if letter == "" {
		return "?"
	}
	switch letter[0] {
	case 'A', 'M', 'D', 'T', 'R', 'C', 'U', 'X', 'B':
		return letter[:1]
	default:
		return "?"
	}
}

// limitedBuffer collects output up to limit bytes and records an overflow
// instead of growing without bound.
type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (w *limitedBuffer) Write(data []byte) (int, error) {
	if w.limit > 0 && w.buffer.Len()+len(data) > w.limit {
		w.overflow = true
		return 0, errDiffOutputTooLarge
	}
	return w.buffer.Write(data)
}
