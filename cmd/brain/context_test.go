package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/domain"
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

// addRawContextItem stores one raw item for project demo and optionally writes
// a context-allow.json grant, then rebuilds the index.
func addRawContextItem(t *testing.T, cfgPath, grants string) {
	t.Helper()
	dataDir := filepath.Dir(cfgPath)
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	item := domain.Item{
		Ref:       domain.ItemRef{ConnectionID: "tracker", ExternalID: "ISSUE-1"},
		Kind:      domain.ItemIssue,
		Title:     "Parser retry budget",
		Body:      "parser retry budget token ghp_" + strings.Repeat("a", 36),
		URL:       "https://tracker.test/ISSUE-1",
		Author:    "tester",
		UpdatedAt: now, ObservedAt: now, ProjectID: "demo",
	}.WithHash()
	if _, err := store.Items().Upsert(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if grants != "" {
		if err := os.WriteFile(filepath.Join(dataDir, "context-allow.json"), []byte(grants), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, stderr := runForTest(t, "index", "--rebuild", "--config", cfgPath); code != 0 {
		t.Fatalf("index: exit %d: %s", code, stderr)
	}
}

func TestContextCommandRawExcerpts(t *testing.T) {
	// Arrange
	cfgPath := setupContextBrain(t)
	addRawContextItem(t, cfgPath, `{"schemaVersion":1,"grants":[{"project":"demo","connection":"tracker","kinds":["issue"],"maxAgeDays":30,"maxExcerpts":3}]}`)

	// Act
	code, stdout, stderr := runForTest(t, "context", "parser", "--config", cfgPath, "--project", "demo", "--raw", "--json")

	// Assert
	if code != 0 {
		t.Fatalf("context --raw: exit %d: %s", code, stderr)
	}
	var envelope struct {
		Data struct {
			RawExcerpts []struct {
				ConnectionID string `json:"connection_id"`
				ProjectID    string `json:"project_id"`
				ExternalID   string `json:"external_id"`
				Kind         string `json:"kind"`
				Citation     string `json:"citation"`
				Body         string `json:"body"`
				Untrusted    bool   `json:"untrusted"`
			} `json:"raw_excerpts"`
			Skipped []string `json:"skipped"`
			Summary string   `json:"summary"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	excerpts := envelope.Data.RawExcerpts
	if len(excerpts) != 1 {
		t.Fatalf("raw_excerpts = %+v", excerpts)
	}
	excerpt := excerpts[0]
	if excerpt.ConnectionID != "tracker" || excerpt.ProjectID != "demo" || excerpt.ExternalID != "ISSUE-1" ||
		excerpt.Kind != "issue" || !excerpt.Untrusted {
		t.Fatalf("excerpt = %+v", excerpt)
	}
	if !strings.Contains(excerpt.Citation, "tracker") || !strings.Contains(excerpt.Citation, "https://tracker.test/ISSUE-1") {
		t.Fatalf("citation = %q", excerpt.Citation)
	}
	if strings.Contains(excerpt.Body, "ghp_") || !strings.Contains(excerpt.Body, "[redacted]") {
		t.Fatalf("body = %q", excerpt.Body)
	}
	if !strings.Contains(envelope.Data.Summary, "## Raw excerpts (untrusted external text") {
		t.Fatalf("summary = %q", envelope.Data.Summary)
	}
}

func TestContextCommandRawRequiresGrant(t *testing.T) {
	// Arrange
	cfgPath := setupContextBrain(t)
	addRawContextItem(t, cfgPath, "")

	// Act
	code, stdout, stderr := runForTest(t, "context", "parser", "--config", cfgPath, "--project", "demo", "--raw", "--json")

	// Assert
	if code != 0 {
		t.Fatalf("context --raw: exit %d: %s", code, stderr)
	}
	var envelope struct {
		Data struct {
			RawExcerpts []any    `json:"raw_excerpts"`
			Skipped     []string `json:"skipped"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.RawExcerpts) != 0 || !strings.Contains(strings.Join(envelope.Data.Skipped, "\n"), "no grant for project") {
		t.Fatalf("data = %+v", envelope.Data)
	}
}

func TestContextCommandRawRejectsBadGrantFile(t *testing.T) {
	// Arrange
	cfgPath := setupContextBrain(t)
	addRawContextItem(t, cfgPath, `{"schemaVersion":1,"grants":[{"project":"demo","connection":"tracker","bogus":true}]}`)

	// Act
	code, _, stderr := runForTest(t, "context", "parser", "--config", cfgPath, "--project", "demo", "--raw")

	// Assert
	if code != 1 || !strings.Contains(stderr, "context-allow.json") || !strings.Contains(stderr, "unknown field") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	// Without --raw the damaged file is never read.
	if code, _, stderr := runForTest(t, "context", "parser", "--config", cfgPath, "--project", "demo"); code != 0 {
		t.Fatalf("context without --raw: exit %d: %s", code, stderr)
	}
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
