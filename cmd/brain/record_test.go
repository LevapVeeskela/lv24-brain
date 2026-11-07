package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func recordCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return runForTest(t, args...)
}

func recordCLIOK(t *testing.T, args ...string) string {
	t.Helper()
	code, stdout, stderr := recordCLI(t, args...)
	if code != 0 {
		t.Fatalf("%v exit %d: %s", args, code, stderr)
	}
	return stdout
}

// recordJSONData decodes the --json envelope and returns its data.
func recordJSONData(t *testing.T, stdout string) any {
	t.Helper()
	var doc struct {
		Schema int `json:"schema"`
		Data   any `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	return doc.Data
}

func TestRecordCLILifecycle(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfg := filepath.Join(dataDir, "brain.yaml")
	recordCLIOK(t, "init", "--mode", "local", "--data-dir", dataDir)
	recordCLIOK(t, "note", "--create", "--config", cfg, "--project", "demo", "--title", "Kickoff", "--body", "first line\nsecond line")
	proposed := strings.Fields(recordCLIOK(t, "decision", "--create", "--config", cfg, "--project", "demo",
		"--title", "Use fsstore", "--context", "Need persistence", "--alternatives", "sqlite,postgres"))[1]
	accepted := strings.Fields(recordCLIOK(t, "decision", "--create", "--config", cfg, "--project", "demo",
		"--title", "Use sqlite", "--status", "accepted"))[1]

	// list: every kind, then the kind and status filters.
	all := recordCLIOK(t, "record", "list", "demo", "--config", cfg)
	if !strings.Contains(all, "  note  ") || !strings.Contains(all, "  decision  "+proposed+"  [proposed]  Use fsstore") {
		t.Fatalf("list = %q", all)
	}
	decisions := recordCLIOK(t, "record", "list", "--project", "demo", "--kind", "decision", "--config", cfg)
	if strings.Contains(decisions, "  note  ") || strings.Count(decisions, "\n") != 2 {
		t.Fatalf("kind filter = %q", decisions)
	}
	onlyAccepted := recordCLIOK(t, "record", "list", "demo", "--status", "accepted", "--config", cfg)
	if strings.TrimSpace(onlyAccepted) == "" || hasID(onlyAccepted, proposed) || !strings.Contains(onlyAccepted, "[accepted]") {
		t.Fatalf("status filter = %q", onlyAccepted)
	}
	if code, _, stderr := recordCLI(t, "record", "list", "demo", "--since", "0", "--config", cfg); code != 2 || stderr == "" {
		t.Fatalf("--since 0 = exit %d", code)
	}
	if code, _, _ := recordCLI(t, "record", "list", "demo", "--since", "soon", "--config", cfg); code != 2 {
		t.Fatalf("--since soon = exit %d, want 2", code)
	}
	recent := recordCLIOK(t, "record", "list", "demo", "--since", "7d", "--config", cfg)
	if !hasID(recent, proposed) {
		t.Fatalf("--since 7d dropped a new record: %q", recent)
	}
	data, ok := recordJSONData(t, recordCLIOK(t, "record", "list", "demo", "--kind", "decision", "--json", "--config", cfg)).([]any)
	if !ok || len(data) != 2 || data[0].(map[string]any)["kind"] != "decision" {
		t.Fatalf("list --json data = %v", data)
	}

	// show: human and JSON.
	human := recordCLIOK(t, "record", "show", proposed, "--project", "demo", "--config", cfg)
	for _, want := range []string{"title: Use fsstore", "status: proposed", "context: Need persistence", "alternatives:\n  - sqlite\n  - postgres"} {
		if !strings.Contains(human, want) {
			t.Fatalf("show missing %q in %q", want, human)
		}
	}
	shown := recordJSONData(t, recordCLIOK(t, "record", "show", proposed, "--project", "demo", "--json", "--config", cfg)).(map[string]any)
	record := shown["record"].(map[string]any)
	if shown["kind"] != "decision" || record["context"] != "Need persistence" || record["project_id"] != "demo" {
		t.Fatalf("show --json = %v", shown)
	}

	// status change and supersede.
	if out := recordCLIOK(t, "record", "status", proposed, "accepted", "--project", "demo", "--config", cfg); strings.TrimSpace(out) != "decision "+proposed+": accepted" {
		t.Fatalf("status = %q", out)
	}
	if code, _, _ := recordCLI(t, "record", "status", proposed, "maybe", "--project", "demo", "--config", cfg); code != 2 {
		t.Fatalf("unknown status exit %d, want 2", code)
	}
	if out := recordCLIOK(t, "record", "supersede", proposed, "--by", accepted, "--project", "demo", "--config", cfg); strings.TrimSpace(out) != "decision "+proposed+": superseded by "+accepted {
		t.Fatalf("supersede = %q", out)
	}
	if code, _, _ := recordCLI(t, "record", "supersede", proposed, "--project", "demo", "--config", cfg); code != 2 {
		t.Fatalf("supersede without --by exit %d, want 2", code)
	}

	// trash needs --yes; with it the record leaves the listing and shows in trash-list.
	if code, _, stderr := recordCLI(t, "record", "trash", accepted, "--project", "demo", "--config", cfg); code != 2 || !strings.Contains(stderr, "--yes") {
		t.Fatalf("trash without --yes = exit %d, stderr %q", code, stderr)
	}
	if out := recordCLIOK(t, "record", "list", "demo", "--config", cfg); !hasID(out, accepted) {
		t.Fatalf("refused trash removed the record: %q", out)
	}
	if out := recordCLIOK(t, "record", "trash", accepted, "--yes", "--project", "demo", "--config", cfg); !strings.Contains(out, "trashed decision "+accepted+" (brain record restore "+accepted+")") {
		t.Fatalf("trash = %q", out)
	}
	if out := recordCLIOK(t, "record", "list", "demo", "--config", cfg); hasID(out, accepted) {
		t.Fatalf("trashed record still listed: %q", out)
	}
	trashed := recordCLIOK(t, "record", "trash-list", "demo", "--config", cfg)
	if !hasID(trashed, accepted) || !strings.Contains(trashed, "Use sqlite") {
		t.Fatalf("trash-list = %q", trashed)
	}
	trashData, ok := recordJSONData(t, recordCLIOK(t, "record", "trash-list", "demo", "--json", "--config", cfg)).([]any)
	if !ok || len(trashData) != 1 || trashData[0].(map[string]any)["id"] != accepted {
		t.Fatalf("trash-list --json = %v", trashData)
	}

	// restore brings it back.
	if out := recordCLIOK(t, "record", "restore", accepted, "--project", "demo", "--config", cfg); strings.TrimSpace(out) != "restored decision "+accepted {
		t.Fatalf("restore = %q", out)
	}
	if out := recordCLIOK(t, "record", "list", "demo", "--config", cfg); !hasID(out, accepted) {
		t.Fatalf("restored record missing from list: %q", out)
	}
}

func TestRecordCLIErrors(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfg := filepath.Join(dataDir, "brain.yaml")
	recordCLIOK(t, "init", "--mode", "local", "--data-dir", dataDir)
	recordCLIOK(t, "note", "--create", "--config", cfg, "--project", "demo", "--title", "Kickoff", "--body", "x")

	if code, _, _ := recordCLI(t, "record"); code != 2 {
		t.Fatalf("bare record exit %d, want 2", code)
	}
	if code, _, _ := recordCLI(t, "record", "frobnicate", "--config", cfg); code != 2 {
		t.Fatalf("unknown subcommand exit %d, want 2", code)
	}
	if code, _, _ := recordCLI(t, "record", "show", "--project", "demo", "--config", cfg); code != 2 {
		t.Fatalf("show without id exit %d, want 2", code)
	}
	if code, _, stderr := recordCLI(t, "record", "show", "decision-missing", "--project", "demo", "--config", cfg); code != 1 || !strings.Contains(stderr, "not found") {
		t.Fatalf("missing record exit %d, stderr %q", code, stderr)
	}
	if code, _, _ := recordCLI(t, "record", "list", "--project", "nope", "--config", cfg); code != 1 {
		t.Fatalf("unknown project exit %d, want 1", code)
	}
	if code, stdout, _ := recordCLI(t, "record", "trash", "--help"); code != 0 || stdout != "" {
		t.Fatalf("trash --help exit %d", code)
	}
}

// hasID reports whether out lists id as a whole field; IDs created in the same
// second share a prefix, so a plain substring check is ambiguous.
func hasID(out, id string) bool {
	return strings.Contains(out, "  "+id+"  ")
}
