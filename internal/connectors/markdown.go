package connectors

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MarkdownSink persists one deterministic connector dump.
type MarkdownSink interface {
	Write(ctx context.Context, relativePath string, content []byte) error
}

// DirSink writes Markdown files below one root directory using a temp file and
// atomic rename. It rejects absolute paths and any path escaping the root.
type DirSink struct {
	root string
}

// NewDirSink creates a directory sink rooted at root.
func NewDirSink(root string) (*DirSink, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("markdown sink root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve markdown sink root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create markdown sink root: %w", err)
	}
	return &DirSink{root: abs}, nil
}

// Write writes one document atomically below the sink root.
func (s *DirSink) Write(ctx context.Context, relativePath string, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clean, err := safeRelativePath(relativePath)
	if err != nil {
		return err
	}
	full := filepath.Join(s.root, filepath.FromSlash(clean))
	if err := ensureWithin(s.root, full); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create markdown directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(full)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create markdown temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write markdown: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close markdown: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return fmt.Errorf("replace markdown: %w", err)
	}
	return nil
}

func safeRelativePath(relativePath string) (string, error) {
	relativePath = strings.TrimSpace(relativePath)
	if relativePath == "" {
		return "", fmt.Errorf("markdown path is required")
	}
	if strings.ContainsRune(relativePath, '\x00') {
		return "", fmt.Errorf("markdown path contains a NUL byte")
	}
	slashed := strings.ReplaceAll(relativePath, "\\", "/")
	if strings.HasPrefix(slashed, "/") {
		return "", fmt.Errorf("markdown path must be relative")
	}
	clean := path.Clean(slashed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("markdown path escapes the sink root")
	}
	if filepath.VolumeName(clean) != "" {
		return "", fmt.Errorf("markdown path must not contain a volume name")
	}
	return clean, nil
}

func ensureWithin(root, full string) error {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return fmt.Errorf("check markdown path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("markdown path escapes the sink root")
	}
	return nil
}

// Slug converts arbitrary text to a stable lowercase path segment.
func Slug(value string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case r == '.', r == '_', r == '-', r == ' ':
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-.")
	if slug == "" {
		return "item"
	}
	if len(slug) > 120 {
		slug = strings.TrimRight(slug[:120], "-.")
	}
	return slug
}

// SlugPath slashes a project path such as group/project into stable segments.
func SlugPath(value string) (string, error) {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	parts := strings.Split(value, "/")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "." || part == ".." {
			return "", fmt.Errorf("path segment %q is not allowed", part)
		}
		segments = append(segments, Slug(part))
	}
	if len(segments) == 0 {
		return "", fmt.Errorf("path is empty")
	}
	return strings.Join(segments, "/"), nil
}

// Document renders deterministic frontmatter followed by body.
func Document(fields map[string]string, body string) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	keys := make([]string, 0, len(fields))
	for key := range fields {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		b.WriteString(key)
		b.WriteString(": ")
		b.WriteString(strconv.Quote(fields[key]))
		b.WriteByte('\n')
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteByte('\n')
	return []byte(b.String())
}

// FormatTime renders a UTC RFC3339 timestamp, or an empty string for zero.
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// DisplayTime renders a timestamp, or fallback for a zero value.
func DisplayTime(t time.Time, fallback string) string {
	if formatted := FormatTime(t); formatted != "" {
		return formatted
	}
	return fallback
}

// EscapeInline makes one line safe inside a Markdown heading or list item.
func EscapeInline(value string) string {
	value = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(value)
	return strings.NewReplacer(
		"\\", "\\\\",
		"*", "\\*",
		"_", "\\_",
		"`", "\\`",
		"[", "\\[",
		"]", "\\]",
		"<", "\\<",
		">", "\\>",
		"|", "\\|",
	).Replace(value)
}

// OneLine collapses whitespace in arbitrary response text.
func OneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// Truncate cuts s to max runes without splitting a UTF-8 sequence.
func Truncate(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max])
}

// UniqueStrings returns sorted, de-duplicated, non-empty values.
func UniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
