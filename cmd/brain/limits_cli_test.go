package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func appendConfig(t *testing.T, cfgPath, block string) {
	t.Helper()
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if err := os.WriteFile(cfgPath, []byte(content+block), 0o600); err != nil {
		t.Fatal(err)
	}
}

func notesInProject(t *testing.T, dataDir, projectID string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dataDir, "projects", projectID, "notes", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestCLIProtectedProjectRefusesWrite(t *testing.T) {
	cfgPath := setupLimitsMCPBrain(t, "limits:\n  protected_projects:\n    - global\n")
	code, _, stderr := runForTest(t, "note", "--config", cfgPath, "--project", "global", "--title", "Blocked", "--body", "agent text")
	if code != 1 || !strings.Contains(stderr, "limits.protected_projects") || !strings.Contains(stderr, "global") {
		t.Fatalf("protected note: exit %d, stderr %q", code, stderr)
	}
	if files := notesInProject(t, filepath.Dir(cfgPath), "global"); len(files) != 0 {
		t.Fatalf("protected project received files: %v", files)
	}

	code, stdout, stderr := runForTest(t, "note", "--config", cfgPath, "--project", "global", "--title", "Allowed", "--body", "owner text", "--allow-protected")
	if code != 0 {
		t.Fatalf("--allow-protected note: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "note ") {
		t.Fatalf("--allow-protected stdout = %q", stdout)
	}
}

func TestCLIBodyLimitRefusesOversizedWrite(t *testing.T) {
	cfgPath := setupLimitsMCPBrain(t, "limits:\n  max_body_bytes: 8\n")
	code, _, stderr := runForTest(t, "note", "--config", cfgPath, "--project", "demo", "--title", "Big", "--body", "0123456789")
	if code != 1 || !strings.Contains(stderr, "limits.max_body_bytes") || !strings.Contains(stderr, "10") {
		t.Fatalf("oversized note: exit %d, stderr %q", code, stderr)
	}
	if files := notesInProject(t, filepath.Dir(cfgPath), "demo"); len(files) != 0 {
		t.Fatalf("refused note left files: %v", files)
	}
}

func TestCLIPromoteRefusesProtectedProject(t *testing.T) {
	cfgPath, store := setupPromoteBrain(t, domain.PublishShared)
	addPromoteCLIItem(t, store, promoteClosedCLIItem("issue-closed", "Serialize writes"))
	appendConfig(t, cfgPath, "limits:\n  protected_projects:\n    - demo\n")
	if code, _, stderr := runForTest(t, "digest", "--config", cfgPath, "--source", "gh", "--write"); code != 0 {
		t.Fatalf("digest: exit %d: %s", code, stderr)
	}
	proposalID := domain.ProposalID("demo", "gh", "issue-closed")

	code, _, stderr := runForTest(t, "promote", "--config", cfgPath, proposalID)
	if code != 1 || !strings.Contains(stderr, "limits.protected_projects") {
		t.Fatalf("protected promote: exit %d, stderr %q", code, stderr)
	}
	if decisions, _ := store.ListDecisions(context.Background(), "demo"); len(decisions) != 0 {
		t.Fatalf("refused promotion created decisions: %+v", decisions)
	}
	proposal, err := store.Get(context.Background(), "demo", proposalID)
	if err != nil || proposal.StatusOrPending() != domain.ProposalPending {
		t.Fatalf("proposal after refused promotion = %+v, %v", proposal, err)
	}

	code, stdout, stderr := runForTest(t, "promote", "--config", cfgPath, proposalID, "--allow-protected")
	if code != 0 {
		t.Fatalf("--allow-protected promote: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "promoted:") {
		t.Fatalf("--allow-protected stdout = %q", stdout)
	}
	if decisions, _ := store.ListDecisions(context.Background(), "demo"); len(decisions) != 1 {
		t.Fatalf("--allow-protected promotion created %d decisions", len(decisions))
	}
}

func TestCLIDoctorReportsLimitsAndLegacyRecords(t *testing.T) {
	cfgPath := setupLimitsMCPBrain(t, "")
	if code, _, stderr := runForTest(t, "note", "--config", cfgPath, "--project", "demo", "--title", "Seed", "--body", "0123456789"); code != 0 {
		t.Fatalf("seed note: exit %d: %s", code, stderr)
	}
	// Add a cap below the seeded record: it becomes legacy data.
	appendConfig(t, cfgPath, "limits:\n  max_body_bytes: 8\n")

	code, stdout, stderr := runForTest(t, "doctor", "--config", cfgPath, "--json")
	if code != 0 {
		t.Fatalf("doctor --json exit %d: %s", code, stderr)
	}
	var report struct {
		Data struct {
			Checks []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
				Code   string `json:"code"`
			} `json:"checks"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("doctor JSON: %v", err)
	}
	var limitsSmall, legacyRecord bool
	for _, check := range report.Data.Checks {
		switch check.Name {
		case "limits":
			limitsSmall = check.Status == "warn" && check.Code == "limits-small"
		case "record-limits":
			legacyRecord = check.Status == "warn" && check.Code == "record-limits"
		}
	}
	if !limitsSmall {
		t.Fatalf("doctor did not warn about the tiny limits: %s", stdout)
	}
	if !legacyRecord {
		t.Fatalf("doctor did not report the legacy oversized record: %s", stdout)
	}
}
