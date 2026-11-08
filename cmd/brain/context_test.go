package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// setupContextBrain creates a local brain with project "demo" holding a
// handoff, a decision, an attempt and an unrelated note.
func setupContextBrain(t *testing.T) string {
	t.Helper()
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	steps := [][]string{
		{"init", "--mode", "local", "--data-dir", dataDir},
		{"note", "--create", "--config", cfgPath, "--project", "demo", "--title", "Installer checklist", "--body", "sign the installer"},
		{"decision", "--config", cfgPath, "--project", "demo", "--title", "Parser uses a hand-written lexer", "--context", "the parser must stream"},
		{"attempt", "--config", cfgPath, "--project", "demo", "--title", "Parser generator", "--reason", "the generated parser was too slow"},
		{"handoff", "--config", cfgPath, "--project", "demo", "--summary", "Parser half done", "--next", "Finish the parser", "--no-git"},
	}
	for _, args := range steps {
		if code, _, stderr := runForTest(t, args...); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, stderr)
		}
	}
	return cfgPath
}

func TestContextCommandPrintsMarkdown(t *testing.T) {
	// Arrange
	cfgPath := setupContextBrain(t)

	// Act
	code, stdout, stderr := runForTest(t, "context", "--config", cfgPath, "--project", "demo", "parser")

	// Assert
	if code != 0 {
		t.Fatalf("context: exit %d: %s", code, stderr)
	}
	for _, want := range []string{"# demo — context", "task: parser", "## Latest handoff", "next: Finish the parser",
		"## Relevant records", "Parser uses a hand-written lexer", "Parser generator"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "Installer checklist") {
		t.Fatalf("unrelated note listed:\n%s", stdout)
	}
}

func TestContextCommandJSON(t *testing.T) {
	// Arrange
	cfgPath := setupContextBrain(t)

	// Act
	code, stdout, stderr := runForTest(t, "context", "parser", "--config", cfgPath, "--project", "demo",
		"--kind", "decision", "--since", "7d", "--budget", "2000", "--json")

	// Assert
	if code != 0 {
		t.Fatalf("context --json: exit %d: %s", code, stderr)
	}
	var envelope struct {
		Schema int `json:"schema"`
		Data   struct {
			ProjectID string `json:"project_id"`
			Query     string `json:"query"`
			Budget    int    `json:"budget"`
			Since     string `json:"since"`
			Handoff   *struct {
				NextAction string `json:"next_action"`
			} `json:"handoff"`
			Records []struct {
				Kind  string `json:"kind"`
				Title string `json:"title"`
			} `json:"records"`
			FailedAttempts []struct {
				Title string `json:"title"`
			} `json:"failed_attempts"`
			Findings []any    `json:"findings"`
			Skipped  []string `json:"skipped"`
			Summary  string   `json:"summary"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	data := envelope.Data
	if envelope.Schema != jsonSchemaVersion || data.ProjectID != "demo" || data.Query != "parser" || data.Budget != 2000 || data.Since == "" {
		t.Fatalf("envelope = %+v", envelope)
	}
	if len(data.Records) != 1 || data.Records[0].Kind != "decision" {
		t.Fatalf("records = %+v, want the decision only", data.Records)
	}
	if len(data.FailedAttempts) != 1 || data.FailedAttempts[0].Title != "Parser generator" {
		t.Fatalf("failed attempts = %+v", data.FailedAttempts)
	}
	if data.Handoff == nil || data.Handoff.NextAction != "Finish the parser" || data.Findings == nil || data.Skipped == nil || data.Summary == "" {
		t.Fatalf("data = %+v", data)
	}
}

func TestContextCommandUsageErrors(t *testing.T) {
	cfgPath := setupContextBrain(t)
	for name, args := range map[string][]string{
		"missing task":    {"context", "--config", cfgPath, "--project", "demo"},
		"negative budget": {"context", "x", "--config", cfgPath, "--budget", "-1"},
	} {
		t.Run(name, func(t *testing.T) {
			if code, _, _ := runForTest(t, args...); code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
		})
	}
	if code, _, stderr := runForTest(t, "context", "x", "--config", cfgPath, "--project", "demo", "--kind", "handoff"); code != 1 || !strings.Contains(stderr, "unknown kind") {
		t.Fatalf("unknown kind: exit %d stderr %q", code, stderr)
	}
}
