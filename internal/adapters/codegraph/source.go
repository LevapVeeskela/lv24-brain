package codegraph

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// sourceReader reads checkout files once per query and keeps their numbered
// lines. Source text never enters the index; it is re-read from disk so a
// caller sees the file as it is now, not as it was when the index was built.
type sourceReader struct {
	root    string
	files   map[string]*sourceFile
	indexed map[string]indexedFile
}

// sourceFile is one file read from the checkout.
type sourceFile struct {
	lines []string
	size  int64
	modMS int64
	err   error
}

// indexedFile is what the index recorded for a path, used to detect a file
// that changed after indexing.
type indexedFile struct {
	size       int64
	modifiedMS int64
	language   string
}

// newSourceReader prepares a reader and loads the index metadata of the paths
// the answer may render.
func (i *Index) newSourceReader(ctx context.Context, paths []string) (*sourceReader, error) {
	reader := &sourceReader{
		root:    i.root,
		files:   map[string]*sourceFile{},
		indexed: map[string]indexedFile{},
	}
	paths = uniqueStrings(paths)
	if len(paths) == 0 {
		return reader, nil
	}
	query := `SELECT path, CAST(size AS INTEGER), CAST(modified_at AS INTEGER), language FROM files
		WHERE path IN (` + placeholders(len(paths)) + `)`
	rows, err := i.db.QueryContext(ctx, query, stringsToAny(paths)...)
	if err != nil {
		return nil, fmt.Errorf("codegraph: read file metadata: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var path, language string
		var size, modifiedMS int64
		if err := rows.Scan(&path, &size, &modifiedMS, &language); err != nil {
			return nil, fmt.Errorf("codegraph: read file metadata: %w", err)
		}
		reader.indexed[path] = indexedFile{size: size, modifiedMS: modifiedMS, language: language}
	}
	return reader, rows.Err()
}

// read loads one repository-relative path. A path that escapes the checkout is
// refused: the index is data, not a trusted path list.
func (r *sourceReader) read(path string) (*sourceFile, error) {
	if cached, ok := r.files[path]; ok {
		return cached, cached.err
	}
	entry := &sourceFile{}
	r.files[path] = entry
	full, err := r.resolve(path)
	if err != nil {
		entry.err = err
		return entry, err
	}
	info, err := os.Stat(full)
	if err != nil {
		entry.err = fmt.Errorf("read %s: %w", path, err)
		return entry, entry.err
	}
	if info.IsDir() {
		entry.err = fmt.Errorf("read %s: path is a directory", path)
		return entry, entry.err
	}
	if info.Size() > maxSourceFileBytes {
		entry.err = fmt.Errorf("read %s: file is %d bytes, over the %d-byte render cap", path, info.Size(), maxSourceFileBytes)
		return entry, entry.err
	}
	raw, err := os.ReadFile(full)
	if err != nil {
		entry.err = fmt.Errorf("read %s: %w", path, err)
		return entry, entry.err
	}
	entry.size = info.Size()
	entry.modMS = info.ModTime().UnixMilli()
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	entry.lines = strings.Split(text, "\n")
	if len(entry.lines) > 0 && entry.lines[len(entry.lines)-1] == "" {
		entry.lines = entry.lines[:len(entry.lines)-1]
	}
	for index, line := range entry.lines {
		entry.lines[index] = sanitizeLine(line)
	}
	return entry, nil
}

// resolve turns a repository-relative path into an absolute path under root and
// refuses traversal.
func (r *sourceReader) resolve(path string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(path)))
	if clean == "." || clean == string(filepath.Separator) {
		return "", fmt.Errorf("read %q: empty path", path)
	}
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("read %q: absolute paths are not accepted", path)
	}
	full := filepath.Join(r.root, clean)
	rel, err := filepath.Rel(r.root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("read %q: path escapes the checkout", path)
	}
	return full, nil
}

// repoPath converts a query path to the repository-relative form the index
// uses. An absolute path is accepted only when it points inside the checkout.
func (i *Index) repoPath(value string) (string, bool) {
	normalized := filepath.FromSlash(strings.TrimSpace(value))
	if !filepath.IsAbs(normalized) {
		return normalizeRepoPath(value), true
	}
	if i.root == "" {
		return "", false
	}
	rel, err := filepath.Rel(i.root, normalized)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// lines returns the numbered lines in [start, end], clamped to the file. The
// boolean reports whether the range was clipped.
func (r *sourceReader) lines(path string, start, end int) ([]ports.SourceLine, bool, error) {
	file, err := r.read(path)
	if err != nil {
		return nil, false, err
	}
	if len(file.lines) == 0 {
		return nil, false, nil
	}
	return numberLines(file.lines, start, end)
}

// numberLines renders a clamped, 1-based line range of text.
func numberLines(lines []string, start, end int) ([]ports.SourceLine, bool, error) {
	if len(lines) == 0 {
		return nil, false, nil
	}
	if start < 1 {
		start = 1
	}
	if end < start {
		end = start
	}
	clipped := false
	if start > len(lines) {
		return nil, true, nil
	}
	if end > len(lines) {
		end = len(lines)
		clipped = true
	}
	out := make([]ports.SourceLine, 0, end-start+1)
	for number := start; number <= end; number++ {
		out = append(out, ports.SourceLine{Number: number, Text: lines[number-1]})
	}
	return out, clipped, nil
}

// changed reports whether a file no longer matches the size and modification
// time recorded at index time. Source is then newer than the graph, which is
// disclosed instead of silently mixing two versions.
func (r *sourceReader) changed(path string) (bool, string) {
	indexed, ok := r.indexed[path]
	if !ok {
		return false, ""
	}
	file, err := r.read(path)
	if err != nil {
		return false, ""
	}
	if file.size == indexed.size && absInt64(file.modMS-indexed.modifiedMS) <= 2000 {
		return false, ""
	}
	return true, fmt.Sprintf("%s changed since the index was built (indexed %d bytes, now %d)", path, indexed.size, file.size)
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

// sanitizeLine removes terminal escape sequences and control characters from
// source text before it is handed to an agent or a terminal. Repository content
// is untrusted data: a crafted file must not be able to drive the reader's
// display or inject ANSI sequences into the answer.
func sanitizeLine(line string) string {
	var out strings.Builder
	out.Grow(len(line))
	for index := 0; index < len(line); index++ {
		char := line[index]
		if char == 0x1b {
			// Skip CSI/OSC introducers and their arguments up to the final
			// byte (a letter for CSI, BEL/ST for OSC).
			index++
			if index < len(line) && (line[index] == '[' || line[index] == ']') {
				final := byte('m')
				if line[index] == ']' {
					final = 0x07
				}
				index++
				for index < len(line) && line[index] != final && line[index] != 0x1b {
					index++
				}
			}
			continue
		}
		if char < 0x20 && char != '\t' {
			continue
		}
		if char == 0x7f {
			continue
		}
		out.WriteByte(char)
	}
	return out.String()
}
