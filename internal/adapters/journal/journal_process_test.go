package journal

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	testChildEnv = "LV24_JOURNAL_TEST_CHILD_DIR"
	testCountEnv = "LV24_JOURNAL_TEST_COUNT"
)

// TestJournalAppendTwoProcesses appends 100 events from two processes and
// requires exactly 100 distinct lines: no duplicated line and no lost append.
func TestJournalAppendTwoProcesses(t *testing.T) {
	if dir := strings.TrimSpace(os.Getenv(testChildEnv)); dir != "" {
		appendJournalEvents(t, dir, "child")
		return
	}

	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cmd := exec.Command(exe, "-test.run=^TestJournalAppendTwoProcesses$")
	cmd.Env = append(os.Environ(), testChildEnv+"="+dir, testCountEnv+"=50")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the second process: %v", err)
	}

	store := New(dir)
	appendJournalEventsWith(t, store, "parent", 50)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("second process failed: %v\n%s", err, output.String())
	}

	entries, err := store.Read(context.Background(), ports.JournalQuery{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 100 {
		t.Fatalf("read %d entries, want 100", len(entries))
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.Event != "note" || entry.User != "shared" {
			t.Fatalf("unexpected entry: %+v", entry)
		}
		if _, duplicate := seen[entry.ID]; duplicate {
			t.Fatalf("duplicate id %q in the read", entry.ID)
		}
		seen[entry.ID] = struct{}{}
	}
	for i := 0; i < 50; i++ {
		for _, prefix := range []string{"parent", "child"} {
			id := fmt.Sprintf("%s-%02d", prefix, i)
			if _, ok := seen[id]; !ok {
				t.Fatalf("event %s was lost", id)
			}
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, Dir, FileName("shared")))
	if err != nil {
		t.Fatalf("read the journal file: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; lines != 100 {
		t.Fatalf("journal has %d lines, want 100", lines)
	}
	if _, err := os.Stat(filepath.Join(dir, Dir, FileName("shared")+lockSuffix)); !os.IsNotExist(err) {
		t.Fatalf("append left the cross-process lock: %v", err)
	}
}

// appendJournalEvents appends the event batch of one process.
func appendJournalEvents(t *testing.T, dir, prefix string) {
	t.Helper()
	appendJournalEventsWith(t, New(dir), prefix, 50)
}

func appendJournalEventsWith(t *testing.T, store *Store, prefix string, count int) {
	t.Helper()
	if raw := strings.TrimSpace(os.Getenv(testCountEnv)); raw != "" && prefix == "child" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			count = parsed
		}
	}
	for i := 0; i < count; i++ {
		entry := ports.JournalEntry{
			Time: time.Now().UTC(), Event: "note", User: "shared",
			ID: fmt.Sprintf("%s-%02d", prefix, i), Title: prefix,
		}
		if err := store.Append(context.Background(), entry); err != nil {
			t.Fatalf("append %s-%02d: %v", prefix, i, err)
		}
		// Give the other process a chance to interleave its appends.
		time.Sleep(2 * time.Millisecond)
	}
}

// TestJournalReadDedupesByTimestampEventAndID covers a replayed or retried
// line: the same (ts, event, id) tuple is returned once, while a different id
// or timestamp stays a separate event.
func TestJournalReadDedupesByTimestampEventAndID(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"ts":"2026-11-06T10:00:00Z","event":"note","user":"alice","id":"n1"}`
	other := `{"ts":"2026-11-06T10:00:00Z","event":"note","user":"alice","id":"n2"}`
	later := `{"ts":"2026-11-06T10:01:00Z","event":"note","user":"alice","id":"n1"}`
	raw := strings.Join([]string{line, line, other, later, line}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, Dir, "alice.jsonl"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := New(dir).Read(context.Background(), ports.JournalQuery{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("read %d entries, want 3 unique: %+v", len(entries), entries)
	}
	if entries[0].ID != "n1" || !entries[0].Time.Equal(time.Date(2026, 11, 6, 10, 1, 0, 0, time.UTC)) {
		t.Fatalf("newest entry = %+v", entries[0])
	}
}
