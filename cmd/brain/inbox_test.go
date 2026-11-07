package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// "Decision:" lines in the agent's final message become proposed decisions.
func TestSessionEndCapturesProposedDecisions(t *testing.T) {
	cfgPath, _, checkout := codexFixture(t)
	message := strings.Join([]string{
		"Done with the parser.",
		"Decision: Use SQLite FTS5 for the local search index",
		"- **Решение:** хранить дампы почты отдельными файлами",
		"Decision: short",
		"decision noted in passing, not a marker line",
	}, "\n")
	for _, event := range []map[string]string{
		{"session_id": "s1", "cwd": checkout, "hook_event_name": "Stop", "last_assistant_message": message},
		{"session_id": "s1", "cwd": checkout, "hook_event_name": "SessionEnd"},
	} {
		if code, _, stderr := runWithInputForTest(t, codexInput(t, event), "session", "end", "--hook", "codex", "--config", cfgPath); code != 0 {
			t.Fatalf("session end: %s", stderr)
		}
	}
	code, stdout, stderr := runForTest(t, "record", "list", "demo", "--kind", "decision", "--json", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("record list: %s", stderr)
	}
	var doc struct {
		Data []struct {
			Title  string `json:"title"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("decode %s: %v", stdout, err)
	}
	titles := map[string]string{}
	for _, decision := range doc.Data {
		titles[decision.Title] = decision.Status
	}
	if titles["Use SQLite FTS5 for the local search index"] != "proposed" || titles["хранить дампы почты отдельными файлами"] != "proposed" || len(titles) != 2 {
		t.Fatalf("captured decisions = %v", titles)
	}
}
