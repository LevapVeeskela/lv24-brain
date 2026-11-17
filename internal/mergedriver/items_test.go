package mergedriver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func itemFile(body, observed string) []byte {
	front := "---\nschema_version: 1\nref:\n  connection_id: \"work\"\n  external_id: \"a\"\nhash: \"sha256:abc\"\n"
	if observed != "" {
		front += "observed_at: \"" + observed + "\"\n"
	}
	return []byte(front + "---\n" + body + "\n")
}

func TestMergeItemsStripsHostObservedAt(t *testing.T) {
	base := itemFile("shared body", "2026-11-17T05:00:00Z")
	ours := itemFile("shared body", "2026-11-17T06:00:00Z")
	theirs := itemFile("shared body", "2026-11-17T07:00:00Z")
	result, err := MergeItems(base, ours, theirs, Options{Path: "connectors/work/items/abc.md", Now: time.Now()})
	if err != nil {
		t.Fatalf("MergeItems: %v", err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", result.Conflicts)
	}
	if strings.Contains(string(result.Merged), "observed_at") {
		t.Fatalf("merged item still has observed_at:\n%s", result.Merged)
	}
	if !strings.Contains(string(result.Merged), "shared body") {
		t.Fatalf("merged item lost the body:\n%s", result.Merged)
	}
}

func TestMergeItemsKeepsBothDivergentSides(t *testing.T) {
	dir := t.TempDir()
	base := itemFile("base body", "")
	ours := itemFile("body from A", "2026-11-17T06:00:00Z")
	theirs := itemFile("body from B", "2026-11-17T07:00:00Z")
	opts := Options{Path: "connectors/work/items/abc.md", OutputDir: dir, Now: time.Now()}
	result, err := MergeItems(base, ours, theirs, opts)
	if err != nil {
		t.Fatalf("MergeItems: %v", err)
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("conflicts = %d, want 1", len(result.Conflicts))
	}
	if !strings.Contains(string(result.Merged), "body from A") {
		t.Fatalf("canonical path did not keep ours:\n%s", result.Merged)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "connectors", "work", "items", "abc-*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("incoming item files = %v, want one", matches)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "body from B") {
		t.Fatalf("incoming item lost theirs:\n%s", raw)
	}
	// Re-running the same merge must be deterministic: the same sibling path.
	again, err := MergeItems(base, ours, theirs, opts)
	if err != nil {
		t.Fatal(err)
	}
	if again.Conflicts[0].Theirs != result.Conflicts[0].Theirs {
		t.Fatalf("incoming path is not deterministic: %q vs %q", again.Conflicts[0].Theirs, result.Conflicts[0].Theirs)
	}
}

func TestMergeItemsCRLF(t *testing.T) {
	crlf := func(raw []byte) []byte {
		return []byte(strings.ReplaceAll(string(raw), "\n", "\r\n"))
	}
	base := itemFile("body", "2026-11-17T05:00:00Z")
	ours := crlf(itemFile("body", "2026-11-17T06:00:00Z"))
	theirs := itemFile("body", "2026-11-17T07:00:00Z")
	result, err := MergeItems(base, ours, theirs, Options{Path: "connectors/work/items/abc.md"})
	if err != nil {
		t.Fatalf("MergeItems: %v", err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("CRLF/LF sides conflicted: %+v", result.Conflicts)
	}
	if strings.Contains(string(result.Merged), "\r") {
		t.Fatalf("merged item has CRLF:\n%q", result.Merged)
	}
}
