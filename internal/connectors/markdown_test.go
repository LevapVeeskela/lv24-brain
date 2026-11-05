package connectors

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDirSinkWritesAtomicFile(t *testing.T) {
	root := t.TempDir()
	sink, err := NewDirSink(root)
	if err != nil {
		t.Fatalf("NewDirSink: %v", err)
	}
	if err := sink.Write(context.Background(), "github/acme/widgets.md", []byte("# hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "github", "acme", "widgets.md"))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(raw) != "# hello\n" {
		t.Fatalf("content = %q", raw)
	}
}

func TestDirSinkRejectsEscapingPaths(t *testing.T) {
	sink, err := NewDirSink(t.TempDir())
	if err != nil {
		t.Fatalf("NewDirSink: %v", err)
	}
	for _, path := range []string{"", "../secret.md", "/etc/passwd", "a/../../b.md"} {
		if err := sink.Write(context.Background(), path, []byte("x")); err == nil {
			t.Fatalf("Write(%q) = nil, want error", path)
		}
	}
}

func TestDocumentIsDeterministic(t *testing.T) {
	fields := map[string]string{"b": "two", "a": "one", "updated_at": "2026-11-05T12:00:00Z"}
	first := Document(fields, "body\n")
	second := Document(fields, "body\n")
	if string(first) != string(second) {
		t.Fatalf("documents differ:\n%s\n%s", first, second)
	}
	wantPrefix := "---\na: \"one\"\nb: \"two\"\nupdated_at: \"2026-11-05T12:00:00Z\"\n---\n\nbody\n"
	if string(first) != wantPrefix {
		t.Fatalf("document = %q", first)
	}
}

func TestSlugAndSlugPath(t *testing.T) {
	if got := Slug("Owner/Repo Name"); got != "owner-repo-name" {
		t.Fatalf("Slug = %q", got)
	}
	got, err := SlugPath("group/sub-project")
	if err != nil || got != "group/sub-project" {
		t.Fatalf("SlugPath = %q, %v", got, err)
	}
	if _, err := SlugPath("../etc"); err == nil {
		t.Fatal("expected traversal error")
	}
}

func TestEscapeInlineFlattensNewlines(t *testing.T) {
	got := EscapeInline("a\nb *c* [d]")
	if strings.Contains(got, "\n") {
		t.Fatalf("newline survived: %q", got)
	}
	if !strings.Contains(got, `\*c\*`) || !strings.Contains(got, `\[d\]`) {
		t.Fatalf("markdown was not escaped: %q", got)
	}
}

func TestFormatTimeIsUTC(t *testing.T) {
	value := time.Date(2026, 11, 5, 14, 0, 0, 0, time.FixedZone("x", 3600))
	if got := FormatTime(value); got != "2026-11-05T13:00:00Z" {
		t.Fatalf("FormatTime = %q", got)
	}
	if got := FormatTime(time.Time{}); got != "" {
		t.Fatalf("zero FormatTime = %q", got)
	}
}
