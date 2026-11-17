package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// consolidateNote writes one note and returns its id.
func consolidateNote(t *testing.T, cfg, project, title, body string, extra ...string) string {
	t.Helper()
	args := append([]string{"note", "--config", cfg, "--project", project, "--title", title, "--body", body}, extra...)
	code, stdout, stderr := runForTest(t, args...)
	if code != 0 {
		t.Fatalf("note %q: exit %d: %s", title, code, stderr)
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.SplitN(stdout, "\n", 2)[0], "note "))
}

// consolidatePlanJSON runs `brain consolidate --json` and decodes the plan
// inside the shared {"schema": 1, "data": ...} envelope.
func consolidatePlanJSON(t *testing.T, cfg string, extra ...string) (int, consolidatePlanPayload) {
	t.Helper()
	args := append([]string{"consolidate", "--config", cfg, "--json"}, extra...)
	code, stdout, stderr := runForTest(t, args...)
	var payload consolidatePlanPayload
	if code == 0 {
		var envelope struct {
			Schema int             `json:"schema"`
			Data   json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
			t.Fatalf("plan envelope: %v\n%s", err, stdout)
		}
		if err := json.Unmarshal(envelope.Data, &payload); err != nil {
			t.Fatalf("plan JSON: %v\n%s", err, stdout)
		}
	} else {
		t.Logf("consolidate exit %d stderr: %s", code, stderr)
	}
	return code, payload
}

type consolidatePlanPayload struct {
	Projects int `json:"projects"`
	Ops      []struct {
		ID          string   `json:"id"`
		Kind        string   `json:"kind"`
		IDs         []string `json:"ids"`
		Fingerprint string   `json:"fingerprint"`
	} `json:"ops"`
	Near        []any  `json:"near"`
	Fingerprint string `json:"fingerprint"`
	Truncated   bool   `json:"truncated"`
}

// duplicatePairForConsolidate writes two notes whose normalized text is
// identical, backdating the first so the retry window does not collapse them.
func duplicatePairForConsolidate(t *testing.T, cfg, dataDir string) (string, string) {
	t.Helper()
	first := consolidateNote(t, cfg, "demo", "Release checklist", "tag the release, publish the notes")
	backdateNoteCreatedAt(t, dataDir, "demo", first)
	second := consolidateNote(t, cfg, "demo", "Release checklist", "Tag the release; publish the notes!", "--allow-duplicate")
	return first, second
}

func TestConsolidateCLIEndToEndPlanApplyRollback(t *testing.T) {
	cfg := setupLimitsMCPBrain(t, "")
	dataDir := filepath.Dir(cfg)
	first, second := duplicatePairForConsolidate(t, cfg, dataDir)

	// Plan is the default and writes nothing.
	code, plan := consolidatePlanJSON(t, cfg, "--project", "demo")
	if code != 0 {
		t.Fatalf("plan exit %d", code)
	}
	if plan.Projects != 1 || len(plan.Ops) != 1 || plan.Ops[0].Kind != "merge" {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.Fingerprint == "" || plan.Ops[0].Fingerprint == "" {
		t.Fatalf("plan fingerprints are empty: %+v", plan)
	}
	if len(plan.Ops[0].IDs) != 2 || plan.Ops[0].IDs[0] != first || plan.Ops[0].IDs[1] != second {
		t.Fatalf("op ids = %v, want %s then %s", plan.Ops[0].IDs, first, second)
	}
	if code, _, stderr := runForTest(t, "get", "--config", cfg, "--project", "demo", second); code != 0 {
		t.Fatalf("plan trashed the duplicate: exit %d: %s", code, stderr)
	}

	// Apply executes only the named op and records a manifest.
	code, stdout, stderr := runForTest(t, "consolidate", "--config", cfg, "--project", "demo",
		"--apply", "--op", plan.Ops[0].ID, "--fingerprint", plan.Fingerprint, "--json")
	if code != 0 {
		t.Fatalf("apply exit %d: %s", code, stderr)
	}
	var envelope struct {
		Schema int             `json:"schema"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("apply envelope: %v\n%s", err, stdout)
	}
	var applied struct {
		Manifest string `json:"manifest"`
		Merged   []struct {
			Survivor string `json:"survivor"`
			Manifest string `json:"manifest"`
		} `json:"merged"`
	}
	if err := json.Unmarshal(envelope.Data, &applied); err != nil {
		t.Fatalf("apply JSON: %v\n%s", err, stdout)
	}
	if applied.Manifest == "" || len(applied.Merged) != 1 || applied.Merged[0].Survivor != first {
		t.Fatalf("applied = %+v", applied)
	}
	if !strings.Contains(applied.Manifest, "backups/consolidate-") {
		t.Fatalf("manifest = %q", applied.Manifest)
	}
	if code, _, stderr := runForTest(t, "get", "--config", cfg, "--project", "demo", second); code == 0 {
		t.Fatalf("duplicate still live after apply: %s", stderr)
	}

	// The rollback restores the merged state and is idempotent.
	code, stdout, stderr = runForTest(t, "consolidate", "--config", cfg, "--rollback", applied.Manifest)
	if code != 0 || !strings.Contains(stdout, "rolled back") {
		t.Fatalf("rollback exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if code, _, stderr := runForTest(t, "get", "--config", cfg, "--project", "demo", second); code != 0 {
		t.Fatalf("rollback did not restore the duplicate: exit %d: %s", code, stderr)
	}
	code, stdout, _ = runForTest(t, "consolidate", "--config", cfg, "--rollback", applied.Manifest)
	if code != 0 || !strings.Contains(stdout, "already rolled back") {
		t.Fatalf("second rollback exit %d stdout %q", code, stdout)
	}
}

func TestConsolidateCLIRefusesStaleFingerprintAndUnknownOps(t *testing.T) {
	cfg := setupLimitsMCPBrain(t, "")
	dataDir := filepath.Dir(cfg)
	_, second := duplicatePairForConsolidate(t, cfg, dataDir)
	_, plan := consolidatePlanJSON(t, cfg, "--project", "demo")
	if len(plan.Ops) != 1 {
		t.Fatalf("plan = %+v", plan)
	}

	code, _, stderr := runForTest(t, "consolidate", "--config", cfg, "--project", "demo",
		"--apply", "--op", plan.Ops[0].ID, "--fingerprint", "stale")
	if code == 0 || !strings.Contains(stderr, "fingerprint") {
		t.Fatalf("stale apply exit %d stderr %q", code, stderr)
	}
	if code, _, _ := runForTest(t, "get", "--config", cfg, "--project", "demo", second); code != 0 {
		t.Fatal("a refused apply changed the records")
	}

	code, _, stderr = runForTest(t, "consolidate", "--config", cfg, "--project", "demo",
		"--apply", "--op", "merge-missing")
	if code == 0 || !strings.Contains(stderr, "not in the current plan") {
		t.Fatalf("unknown op exit %d stderr %q", code, stderr)
	}

	// --op is repeatable, and naming one op twice is refused rather than
	// silently deduplicated.
	code, _, stderr = runForTest(t, "consolidate", "--config", cfg, "--project", "demo",
		"--apply", "--op", plan.Ops[0].ID, "--op", plan.Ops[0].ID, "--fingerprint", plan.Fingerprint)
	if code == 0 || !strings.Contains(stderr, "named twice") {
		t.Fatalf("duplicate op exit %d stderr %q", code, stderr)
	}

	code, _, stderr = runForTest(t, "consolidate", "--config", cfg, "--project", "demo", "--plan", "--apply")
	if code != 2 {
		t.Fatalf("--plan --apply exit %d stderr %q", code, stderr)
	}
}

func TestConsolidateCLIScanPlansEveryProject(t *testing.T) {
	cfg := setupLimitsMCPBrain(t, "")
	code, plan := consolidatePlanJSON(t, cfg, "--scan")
	if code != 0 {
		t.Fatalf("scan exit %d", code)
	}
	if plan.Projects != 2 {
		t.Fatalf("scan projects = %d, want 2: %+v", plan.Projects, plan)
	}
	code, _, stderr := runForTest(t, "consolidate", "--config", cfg, "--scan", "--project", "demo")
	if code != 2 {
		t.Fatalf("--scan --project exit %d stderr %q", code, stderr)
	}
}
