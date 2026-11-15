package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// backdateNoteCreatedAt rewrites the note marker of one daily file so the
// record looks older than the retry window. It is how the test reaches the
// duplicate path without waiting ten minutes.
func backdateNoteCreatedAt(t *testing.T, dataDir, projectID, id string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dataDir, "projects", projectID, "notes", "*.md"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("notes files = %v, %v", paths, err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`"created_at":"[^"]*"`)
	updated := pattern.ReplaceAllString(string(raw), `"created_at":"2020-01-01T00:00:00Z"`)
	if updated == string(raw) {
		t.Fatalf("no created_at in the note marker:\n%s", raw)
	}
	if !strings.Contains(updated, id) {
		t.Fatalf("note %s not in %s", id, paths[0])
	}
	if err := os.WriteFile(paths[0], []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNoteCLIRefusesExactDuplicate(t *testing.T) {
	cfg := setupLimitsMCPBrain(t, "")
	dataDir := filepath.Dir(cfg)
	code, stdout, stderr := runForTest(t, "note", "--config", cfg, "--project", "demo", "--title", "Release checklist", "--body", "tag the release, publish the notes")
	if code != 0 {
		t.Fatalf("first note: exit %d: %s", code, stderr)
	}
	id := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(stdout, "\n", 2)[0], "note "))
	backdateNoteCreatedAt(t, dataDir, "demo", id)

	code, _, stderr = runForTest(t, "note", "--config", cfg, "--project", "demo", "--title", "Release checklist", "--body", "Tag the release; publish the notes!")
	if code != 1 || !strings.Contains(stderr, id) || !strings.Contains(stderr, "duplicate record") {
		t.Fatalf("duplicate note: exit %d stderr %q, want 1 and the existing id", code, stderr)
	}

	code, stdout, stderr = runForTest(t, "note", "--config", cfg, "--project", "demo", "--title", "Release checklist", "--body", "Tag the release; publish the notes!", "--allow-duplicate")
	if code != 0 {
		t.Fatalf("override note: exit %d: %s", code, stderr)
	}
	if strings.TrimSpace(strings.TrimPrefix(strings.SplitN(stdout, "\n", 2)[0], "note ")) == id {
		t.Fatal("override returned the existing id")
	}
}

// runMCPWithWrite feeds requests to a brain mcp server started with the write
// tools enabled (this test runs outside a project checkout) and returns the
// decoded responses.
func runMCPWithWrite(t *testing.T, cfgPath string, messages ...string) ([]map[string]any, string) {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		for _, msg := range messages {
			if _, err := io.WriteString(pw, msg+"\n"); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()
	var stdout, stderr bytes.Buffer
	code := runMCP([]string{"--config", cfgPath, "--allow-write"}, pr, &stdout, &stderr)
	_ = pr.Close()
	if code != 0 {
		t.Fatalf("runMCP exit code = %d (stderr: %s)", code, stderr.String())
	}
	var responses []map[string]any
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("stdout line is not JSON: %q", line)
		}
		responses = append(responses, msg)
	}
	return responses, stderr.String()
}

func TestMCPRefusesExactDuplicateWithExistingID(t *testing.T) {
	cfg := setupLimitsMCPBrain(t, "")
	dataDir := filepath.Dir(cfg)
	code, stdout, stderr := runForTest(t, "note", "--config", cfg, "--project", "demo", "--title", "Release checklist", "--body", "tag the release, publish the notes")
	if code != 0 {
		t.Fatalf("first note: exit %d: %s", code, stderr)
	}
	id := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(stdout, "\n", 2)[0], "note "))
	backdateNoteCreatedAt(t, dataDir, "demo", id)

	responses, _ := runMCPWithWrite(t, cfg,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		mcpToolCall(2, "brain_note", `{"text":"tag the release, publish the notes","title":"Release checklist","project":"demo"}`),
	)
	if len(responses) != 2 {
		t.Fatalf("responses = %d", len(responses))
	}
	result := mcpResult(t, responses[1])
	if result["isError"] != true {
		t.Fatalf("duplicate write was not refused: %+v", result)
	}
	refusal := mcpStructured(t, result)
	if refusal["code"] != "DUPLICATE" {
		t.Fatalf("refusal = %+v, want DUPLICATE", refusal)
	}
	if try, _ := refusal["try_instead"].(string); !strings.Contains(try, id) {
		t.Fatalf("try_instead = %q, want the existing id %s", try, id)
	}
	if refusal["retryable"] != false {
		t.Fatalf("retryable = %v, want false", refusal["retryable"])
	}
}
