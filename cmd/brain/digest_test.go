package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDigestJSONEnvelope(t *testing.T) {
	cfgPath := setupCatchupBrain(t)
	if code, _, stderr := runForTest(t, "note", "--config", cfgPath, "--project", "demo", "--title", "After handoff", "--body", "y"); code != 0 {
		t.Fatalf("note: exit %d: %s", code, stderr)
	}

	code, stdout, stderr := runForTest(t, "digest", "--config", cfgPath, "--json")
	if code != 0 {
		t.Fatalf("digest --json: exit %d: %s", code, stderr)
	}
	var envelope struct {
		Schema int `json:"schema"`
		Data   struct {
			Window    string  `json:"window"`
			Since     *string `json:"since"`
			WrittenTo *string `json:"written_to"`
			Projects  []struct {
				Project       string `json:"project"`
				ChangeCount   int    `json:"change_count"`
				Notes         int    `json:"notes"`
				LatestHandoff *struct {
					Summary string `json:"summary"`
				} `json:"latest_handoff"`
			} `json:"projects"`
			NeedsYou struct {
				ProposedDecisions []json.RawMessage `json:"proposed_decisions"`
				OpenQuestions     []json.RawMessage `json:"open_questions"`
				Blocked           []json.RawMessage `json:"blocked"`
				ReviewFindings    []json.RawMessage `json:"review_findings"`
			} `json:"needs_you"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if envelope.Schema == 0 || envelope.Data.Window != "24h" || envelope.Data.Since == nil {
		t.Fatalf("envelope = %+v", envelope)
	}
	if envelope.Data.WrittenTo != nil {
		t.Fatalf("written_to set without --write: %v", *envelope.Data.WrittenTo)
	}
	// The handoff was recorded just now, so the demo project has activity.
	if len(envelope.Data.Projects) != 1 || envelope.Data.Projects[0].Project != "demo" ||
		envelope.Data.Projects[0].Notes != 2 || envelope.Data.Projects[0].LatestHandoff == nil ||
		envelope.Data.Projects[0].LatestHandoff.Summary != "Shipped the parser" {
		t.Fatalf("projects = %+v", envelope.Data.Projects)
	}
	if envelope.Data.NeedsYou.ProposedDecisions == nil || envelope.Data.NeedsYou.OpenQuestions == nil ||
		envelope.Data.NeedsYou.Blocked == nil || envelope.Data.NeedsYou.ReviewFindings == nil {
		t.Fatalf("needs_you lists must be arrays, not null: %+v", envelope.Data.NeedsYou)
	}
}

func TestDigestWriteCreatesDailyFile(t *testing.T) {
	cfgPath := setupCatchupBrain(t)

	code, stdout, stderr := runForTest(t, "digest", "--config", cfgPath, "--write")
	if code != 0 {
		t.Fatalf("digest --write: exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "brain digest") || !strings.Contains(stdout, "wrote ") {
		t.Fatalf("stdout = %q", stdout)
	}
	dataDir := filepath.Dir(cfgPath)
	path := filepath.Join(dataDir, "digests", time.Now().Local().Format("2006-01-02")+".md")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("digest file: %v (stdout %q)", err, stdout)
	}
	if !strings.Contains(string(content), "Shipped the parser") {
		t.Fatalf("digest file = %q", content)
	}

	// A second run the same day overwrites the file.
	if code, _, stderr := runForTest(t, "digest", "--config", cfgPath, "--write", "--json"); code != 0 {
		t.Fatalf("second digest --write --json: exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("digest file after rerun: %v", err)
	}
}

func TestDigestRejectsBadWindowAndArguments(t *testing.T) {
	cfgPath := setupCatchupBrain(t)
	for _, args := range [][]string{
		{"digest", "--config", cfgPath, "--since", "soon"},
		{"digest", "--config", cfgPath, "--since", "0d"},
		{"digest", "--config", cfgPath, "extra"},
	} {
		if code, _, _ := runForTest(t, args...); code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestParseDigestWindow(t *testing.T) {
	cases := map[string]time.Duration{
		"24h": 24 * time.Hour,
		"90m": 90 * time.Minute,
		"7d":  7 * 24 * time.Hour,
	}
	for input, want := range cases {
		got, err := parseDigestWindow(input)
		if err != nil || got != want {
			t.Errorf("parseDigestWindow(%q) = %v, %v; want %v", input, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1h", "1.5d", "x"} {
		if _, err := parseDigestWindow(bad); err == nil {
			t.Errorf("parseDigestWindow(%q) accepted", bad)
		}
	}
}
