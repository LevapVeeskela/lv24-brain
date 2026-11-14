package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureIgnorePatternsAddsLockAndTempLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".gitignore")
	if err := os.WriteFile(path, []byte("custom/\n"), 0o644); err != nil {
		t.Fatalf("write existing .gitignore: %v", err)
	}
	if err := ensureIgnorePatterns(path, gitignoreContent); err != nil {
		t.Fatalf("ensureIgnorePatterns: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	content := string(raw)
	for _, want := range []string{"custom/", ".tmp-*", "*.lock", "**/.lock"} {
		if !hasIgnoreLine(content, want) {
			t.Fatalf(".gitignore is missing %q:\n%s", want, content)
		}
	}

	// A second init must not duplicate the managed block.
	if err := ensureIgnorePatterns(path, gitignoreContent); err != nil {
		t.Fatalf("second ensureIgnorePatterns: %v", err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .gitignore again: %v", err)
	}
	if got, want := strings.Count(string(again), "\n*.lock\n"), 1; got != want {
		t.Fatalf("*.lock appears %d times after a second init, want %d:\n%s", got, want, again)
	}
}

func TestEnsureIgnorePatternsCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".gitignore")
	if err := ensureIgnorePatterns(path, gitignoreContent); err != nil {
		t.Fatalf("ensureIgnorePatterns: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !hasIgnoreLine(string(raw), ".tmp-*") || !hasIgnoreLine(string(raw), "*.lock") {
		t.Fatalf("created .gitignore misses lock patterns:\n%s", raw)
	}
}
