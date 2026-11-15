package main

import (
	"strings"
	"testing"
)

// TestDuplicatesCLIListsExactClusters writes one intentional copy and checks
// the listing shows both ids without touching either record.
func TestDuplicatesCLIListsExactClusters(t *testing.T) {
	cfg := setupLimitsMCPBrain(t, "")
	code, stdout, stderr := runForTest(t, "note", "--config", cfg, "--project", "demo", "--title", "Release checklist", "--body", "tag the release, publish the notes, then push the mirror")
	if code != 0 {
		t.Fatalf("first note: exit %d: %s", code, stderr)
	}
	first := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(stdout, "\n", 2)[0], "note "))
	code, stdout, stderr = runForTest(t, "note", "--config", cfg, "--project", "demo", "--title", "Release checklist", "--body", "Tag the release; publish the notes; then push the mirror!", "--allow-duplicate")
	if code != 0 {
		t.Fatalf("duplicate note: exit %d: %s", code, stderr)
	}
	second := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(stdout, "\n", 2)[0], "note "))
	if second == first {
		t.Fatalf("override returned the existing id %s", first)
	}

	code, stdout, stderr = runForTest(t, "duplicates", "--config", cfg, "--project", "demo")
	if code != 0 {
		t.Fatalf("duplicates: exit %d: %s", code, stderr)
	}
	for _, want := range []string{"exact clusters: 1", "[exact]", first, second} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("duplicates output missing %q:\n%s", want, stdout)
		}
	}

	_, data := runJSONForTest(t, "duplicates", "--config", cfg, "--project", "demo", "--json")
	clusters := asList(data["clusters"])
	if len(clusters) != 1 {
		t.Fatalf("clusters = %+v", data["clusters"])
	}
	cluster := clusters[0]
	if cluster["reason"] != "exact" || cluster["kind"] != "note" || cluster["project_id"] != "demo" {
		t.Fatalf("cluster = %+v", cluster)
	}
	if members := asList(cluster["members"]); len(members) != 2 || members[0]["title"] != "Release checklist" {
		t.Fatalf("members = %+v", cluster["members"])
	}
	if data["exact"] != float64(1) || data["projects"] != float64(1) {
		t.Fatalf("counts = %+v", data)
	}

	// The listing must not trash or merge anything.
	for _, id := range []string{first, second} {
		code, stdout, stderr := runForTest(t, "record", "show", id, "--config", cfg, "--project", "demo")
		if code != 0 || !strings.Contains(stdout, id) {
			t.Fatalf("record %s unreadable after duplicates: exit %d stdout %q stderr %q", id, code, stdout, stderr)
		}
	}
}

func TestDuplicatesCLIScanCoversEveryProject(t *testing.T) {
	cfg := setupLimitsMCPBrain(t, "")
	for _, project := range []string{"demo", "global"} {
		if code, _, stderr := runForTest(t, "note", "--config", cfg, "--project", project, "--title", "Shared note", "--body", "the same text in two projects is not a duplicate"); code != 0 {
			t.Fatalf("note in %s: exit %d: %s", project, code, stderr)
		}
	}
	_, scanned := runJSONForTest(t, "duplicates", "--scan", "--config", cfg, "--json")
	if scanned["scan"] != true || len(asList(scanned["clusters"])) != 0 {
		t.Fatalf("scan report = %+v (cross-project text must not cluster)", scanned)
	}
	if scanned["projects"] != float64(2) {
		t.Fatalf("projects = %v, want 2", scanned["projects"])
	}
}
