package main

import (
	"strings"
	"testing"
)

// stdoutRecordID reads the id from the first line ("<kind> <id>") of a write.
func stdoutRecordID(t *testing.T, stdout, kind string) string {
	t.Helper()
	line, _, _ := strings.Cut(strings.TrimSpace(stdout), "\n")
	fields := strings.Fields(line)
	if len(fields) != 2 || fields[0] != kind {
		t.Fatalf("first line = %q, want %q <id>", line, kind)
	}
	return fields[1]
}

// similarBlock collects the --similar lines of a write.
func similarBlock(stdout string) string {
	var lines []string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "similar:") || strings.HasPrefix(line, "hint:") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func TestNoteSimilarFlagSurfacesTheOriginal(t *testing.T) {
	cfg := setupLimitsMCPBrain(t, "")
	code, stdout, stderr := runForTest(t, "note", "--config", cfg, "--project", "demo",
		"--title", "Cache fix in SQLite", "--body", "use FTS5 with bm25 ranking")
	if code != 0 {
		t.Fatalf("first note: exit %d: %s", code, stderr)
	}
	originalID := stdoutRecordID(t, stdout, "note")

	code, stdout, stderr = runForTest(t, "note", "--config", cfg, "--project", "demo",
		"--title", "Cache fix in SQLite", "--body", "use a trigram index instead", "--similar")
	if code != 0 {
		t.Fatalf("second note: exit %d: %s", code, stderr)
	}
	newID := stdoutRecordID(t, stdout, "note")
	if newID == originalID {
		t.Fatalf("second note reused %s", originalID)
	}
	block := similarBlock(stdout)
	if !strings.Contains(block, "similar: "+originalID+" ") {
		t.Fatalf("similar block does not name the original %s:\n%s", originalID, block)
	}
	if !strings.Contains(block, "hint: pass --supersedes "+originalID+" if this replaces "+originalID) {
		t.Fatalf("similar block has no supersedes hint for %s:\n%s", originalID, block)
	}
	if strings.Contains(block, newID) {
		t.Fatalf("similar block names the new record %s:\n%s", newID, block)
	}
}

func TestNoteWithoutSimilarFlagKeepsStdoutUnchanged(t *testing.T) {
	cfg := setupLimitsMCPBrain(t, "")
	if code, _, stderr := runForTest(t, "note", "--config", cfg, "--project", "demo",
		"--title", "Cache fix in SQLite", "--body", "use FTS5 with bm25 ranking"); code != 0 {
		t.Fatalf("first note: exit %d: %s", code, stderr)
	}
	code, stdout, stderr := runForTest(t, "note", "--config", cfg, "--project", "demo",
		"--title", "Cache fix in SQLite", "--body", "use a trigram index instead")
	if code != 0 {
		t.Fatalf("second note: exit %d: %s", code, stderr)
	}
	if lines := strings.Split(strings.TrimSpace(stdout), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], "note ") {
		t.Fatalf("stdout = %q, want only the record id line", stdout)
	}
	if block := similarBlock(stdout); block != "" {
		t.Fatalf("default stdout carries a similar block:\n%s", block)
	}
}

func TestDecisionSimilarFlagSurfacesTheOriginal(t *testing.T) {
	cfg := setupLimitsMCPBrain(t, "")
	code, stdout, stderr := runForTest(t, "decision", "--config", cfg, "--project", "demo",
		"--title", "Adopt SQLite FTS5", "--context", "search must stay local")
	if code != 0 {
		t.Fatalf("first decision: exit %d: %s", code, stderr)
	}
	originalID := stdoutRecordID(t, stdout, "decision")

	code, stdout, stderr = runForTest(t, "decision", "--config", cfg, "--project", "demo",
		"--title", "Adopt SQLite FTS5", "--context", "a trigram index was also considered", "--similar")
	if code != 0 {
		t.Fatalf("second decision: exit %d: %s", code, stderr)
	}
	newID := stdoutRecordID(t, stdout, "decision")
	block := similarBlock(stdout)
	if !strings.Contains(block, "similar: "+originalID+" ") {
		t.Fatalf("similar block does not name %s:\n%s", originalID, block)
	}
	if !strings.Contains(block, "hint: pass --supersedes "+originalID) {
		t.Fatalf("similar block has no hint for %s:\n%s", originalID, block)
	}
	if strings.Contains(block, newID) {
		t.Fatalf("similar block names the new record %s:\n%s", newID, block)
	}
}

func TestAttemptSimilarFlagSurfacesTheOriginal(t *testing.T) {
	cfg := setupLimitsMCPBrain(t, "")
	code, stdout, stderr := runForTest(t, "attempt", "--config", cfg, "--project", "demo",
		"--title", "Warm the cache on boot", "--hypothesis", "faster first query", "--result", "no visible gain")
	if code != 0 {
		t.Fatalf("first attempt: exit %d: %s", code, stderr)
	}
	originalID := stdoutRecordID(t, stdout, "attempt")

	code, stdout, stderr = runForTest(t, "attempt", "--config", cfg, "--project", "demo",
		"--title", "Warm the cache on boot", "--hypothesis", "faster first query", "--result", "startup got slower", "--similar")
	if code != 0 {
		t.Fatalf("second attempt: exit %d: %s", code, stderr)
	}
	newID := stdoutRecordID(t, stdout, "attempt")
	block := similarBlock(stdout)
	if !strings.Contains(block, "similar: "+originalID+" ") {
		t.Fatalf("similar block does not name %s:\n%s", originalID, block)
	}
	if !strings.Contains(block, "hint: pass --supersedes "+originalID) {
		t.Fatalf("similar block has no hint for %s:\n%s", originalID, block)
	}
	if strings.Contains(block, newID) {
		t.Fatalf("similar block names the new record %s:\n%s", newID, block)
	}
}
