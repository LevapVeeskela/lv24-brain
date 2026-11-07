package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// setupCatchupBrain creates a git-mode brain with project "demo" whose latest
// handoff was just recorded, and returns its config path.
func setupCatchupBrain(t *testing.T) string {
	t.Helper()
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	steps := [][]string{
		{"init", "--mode", "local", "--data-dir", dataDir},
		{"note", "--create", "--config", cfgPath, "--project", "demo", "--title", "Seed", "--body", "x"},
		{"handoff", "--config", cfgPath, "--project", "demo", "--summary", "Shipped the parser", "--next", "Write tests", "--no-git"},
	}
	for _, args := range steps {
		if code, _, stderr := runForTest(t, args...); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, stderr)
		}
	}
	return cfgPath
}

func TestCatchupReportsNoteAfterHandoff(t *testing.T) {
	cfgPath := setupCatchupBrain(t)
	if code, _, stderr := runForTest(t, "note", "--config", cfgPath, "--project", "demo", "--title", "After handoff", "--body", "y"); code != 0 {
		t.Fatalf("note: exit %d: %s", code, stderr)
	}

	code, stdout, stderr := runForTest(t, "catchup", "--config", cfgPath, "demo")
	if code != 0 {
		t.Fatalf("catchup: exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "handoff") || !strings.Contains(stdout, "notes (1):") || !strings.Contains(stdout, "After handoff") {
		t.Fatalf("catchup stdout = %q", stdout)
	}
	if strings.Contains(stdout, "Seed") {
		t.Fatalf("record before the handoff reported: %q", stdout)
	}
}

func TestCatchupJSON(t *testing.T) {
	cfgPath := setupCatchupBrain(t)
	if code, _, stderr := runForTest(t, "note", "--config", cfgPath, "--project", "demo", "--title", "After handoff", "--body", "y"); code != 0 {
		t.Fatalf("note: exit %d: %s", code, stderr)
	}

	code, stdout, stderr := runForTest(t, "catchup", "--config", cfgPath, "--json", "demo")
	if code != 0 {
		t.Fatalf("catchup --json: exit %d: %s", code, stderr)
	}
	var envelope struct {
		Schema int `json:"schema"`
		Data   struct {
			Project     string `json:"project"`
			SinceSource string `json:"since_source"`
			Notes       []struct {
				Title string `json:"title"`
			} `json:"notes"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if len(envelope.Data.Notes) != 1 || envelope.Data.SinceSource != "handoff" || envelope.Data.Project != "demo" {
		t.Fatalf("catchup json = %+v", envelope)
	}
}

func TestCatchupNothingNewRightAfterHandoff(t *testing.T) {
	cfgPath := setupCatchupBrain(t)

	code, stdout, stderr := runForTest(t, "catchup", "--config", cfgPath, "demo")
	if code != 0 {
		t.Fatalf("catchup: exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "nothing new since") || !strings.Contains(stdout, "(handoff)") {
		t.Fatalf("catchup stdout = %q", stdout)
	}

	code, stdout, stderr = runForTest(t, "catchup", "--config", cfgPath, "--json", "demo")
	if code != 0 {
		t.Fatalf("catchup --json: exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"notes": []`) || !strings.Contains(stdout, `"decisions": []`) {
		t.Fatalf("empty lists must serialize as []: %s", stdout)
	}
}

func TestCatchupRejectsNegativeLimit(t *testing.T) {
	cfgPath := setupCatchupBrain(t)
	if code, _, _ := runForTest(t, "catchup", "--config", cfgPath, "--limit", "-1", "demo"); code != 2 {
		t.Fatalf("--limit -1 exit = %d, want 2", code)
	}
}
