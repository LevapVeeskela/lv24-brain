package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentLoopCLI: an agent can keep state current, writes go to the right
// project (alias works, typos are refused instead of creating a project),
// retries do not duplicate records, and --help succeeds.
func TestAgentLoopCLI(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfg := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	if code, _, stderr := runForTest(t, "project", "set", "city", "--alias", "lv", "--config", cfg); code != 0 {
		t.Fatalf("project set: %s", stderr)
	}

	// state set / get
	code, stdout, stderr := runForTest(t, "state", "set", "lv", "--goal", "Ship v1", "--next", "Write tests",
		"--active", "agent loop", "--active", "mcp", "--question", "JSON envelope?", "--config", cfg)
	if code != 0 || !strings.Contains(stdout, "goal: Ship v1") || !strings.Contains(stdout, "project: city") {
		t.Fatalf("state set: %d %q %s", code, stdout, stderr)
	}
	code, stdout, _ = runForTest(t, "state", "get", "city", "--json", "--config", cfg)
	var got struct {
		Schema int `json:"schema"`
		Data   struct {
			Goal          string   `json:"goal"`
			NextAction    string   `json:"nextAction"`
			Active        []string `json:"active"`
			OpenQuestions []string `json:"openQuestions"`
		} `json:"data"`
	}
	if code != 0 || json.Unmarshal([]byte(stdout), &got) != nil || got.Schema != 1 || got.Data.Goal != "Ship v1" ||
		got.Data.NextAction != "Write tests" || len(got.Data.Active) != 2 || len(got.Data.OpenQuestions) != 1 {
		t.Fatalf("state get --json: %d %s", code, stdout)
	}
	if code, stdout, _ = runForTest(t, "state", "set", "city", "--clear-questions", "--config", cfg); code != 0 || strings.Contains(stdout, "JSON envelope?") {
		t.Fatalf("clear questions: %d %s", code, stdout)
	}
	if code, _, _ = runForTest(t, "state", "set", "city", "--config", cfg); code == 0 {
		t.Fatal("state set without fields succeeded")
	}
	if code, _, _ = runForTest(t, "open", "--project", "cty", "--config", cfg); code == 0 {
		t.Fatal("open with a mistyped --project succeeded")
	}

	// A typo is refused and creates nothing; an alias writes to its project.
	if code, _, stderr = runForTest(t, "note", "--project", "cty", "oops", "--config", cfg); code == 0 || !strings.Contains(stderr, "--create") {
		t.Fatalf("typo note: %d %s", code, stderr)
	}
	if _, stdout, _ = runForTest(t, "project", "list", "--config", cfg); strings.Contains(stdout, "cty") {
		t.Fatalf("typo created a project: %s", stdout)
	}
	code, first, stderr := runForTest(t, "note", "--project", "lv", "Retried note", "--config", cfg)
	if code != 0 || !strings.Contains(first, "note-city-") {
		t.Fatalf("alias note: %d %q %s", code, first, stderr)
	}
	// The same write again (an agent retrying) returns the same record.
	if code, second, _ := runForTest(t, "note", "--project", "city", "Retried note", "--config", cfg); code != 0 || second != first {
		t.Fatalf("retry created a different record: %q vs %q", second, first)
	}

	// Leaf help is a success.
	for _, args := range [][]string{{"note", "--help"}, {"state", "set", "-h"}, {"open", "--help"}} {
		if code, _, _ := runForTest(t, args...); code != 0 {
			t.Fatalf("%v exit = %d, want 0", args, code)
		}
	}
}
