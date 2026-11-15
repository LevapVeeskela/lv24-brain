package main

import (
	"strings"
	"testing"
)

// TestReviewDueCLIListsExplicitDates: `record review --review-after` moves the
// date, `review --due` lists it, and `--clear-review-after` takes it back out.
func TestReviewDueCLIListsExplicitDates(t *testing.T) {
	cfg := setupLimitsMCPBrain(t, "")
	code, stdout, stderr := runForTest(t, "note", "--config", cfg, "--project", "demo", "--title", "Seed", "--body", "body")
	if code != 0 {
		t.Fatalf("note: exit %d: %s", code, stderr)
	}
	id := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(stdout, "\n", 2)[0], "note "))

	if code, _, stderr := runForTest(t, "review", "--due", "--json", "--config", cfg, "--project", "demo"); code != 0 {
		t.Fatalf("review --due: exit %d: %s", code, stderr)
	}
	_, empty := runJSONForTest(t, "review", "--due", "--json", "--config", cfg, "--project", "demo")
	if len(asList(empty["due"])) != 0 || empty["due"] == nil {
		t.Fatalf("fresh project due list = %+v, want an empty list", empty["due"])
	}

	code, stdout, stderr = runForTest(t, "record", "review", id, "--review-after", "2020-01-02", "--config", cfg, "--project", "demo")
	if code != 0 {
		t.Fatalf("record review --review-after: exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "next review: 2020-01-02") {
		t.Fatalf("record review stdout = %q", stdout)
	}

	code, stdout, stderr = runForTest(t, "review", "--due", "--config", cfg, "--project", "demo")
	if code != 0 {
		t.Fatalf("review --due text: exit %d: %s", code, stderr)
	}
	for _, want := range []string{"## Due", id, "is", "overdue", "frontmatter"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("review --due output missing %q:\n%s", want, stdout)
		}
	}

	_, data := runJSONForTest(t, "review", "--due", "--json", "--config", cfg, "--project", "demo")
	due, ok := data["due"].([]any)
	if !ok || len(due) != 1 {
		t.Fatalf("due = %+v", data["due"])
	}
	first := due[0].(map[string]any)
	if first["kind"] != "note" || first["id"] != id || first["source"] != "frontmatter" {
		t.Fatalf("due entry = %+v", first)
	}
	if days, _ := first["overdue_days"].(float64); days < 1 {
		t.Fatalf("overdue_days = %v, want a positive number", first["overdue_days"])
	}

	if code, _, stderr := runForTest(t, "record", "review", id, "--clear-review-after", "--config", cfg, "--project", "demo"); code != 0 {
		t.Fatalf("record review --clear-review-after: exit %d: %s", code, stderr)
	}
	_, cleared := runJSONForTest(t, "review", "--due", "--json", "--config", cfg, "--project", "demo")
	for _, entry := range asList(cleared["due"]) {
		if entry["id"] == id && entry["source"] == "frontmatter" {
			t.Fatalf("cleared record still due: %+v", entry)
		}
	}
}

func TestReviewDueRejectsScheduleCombinations(t *testing.T) {
	cfg := setupLimitsMCPBrain(t, "")
	for _, args := range [][]string{
		{"record", "review", "note-x", "--review-after", "30d", "--clear-review-after"},
		{"record", "review", "--all", "--review-after", "30d"},
		{"record", "review", "note-x", "--review-after", "someday"},
	} {
		if code, _, _ := runForTest(t, append(args, "--config", cfg, "--project", "demo")...); code != 2 {
			t.Fatalf("%v exit = %d, want 2", args, code)
		}
	}
}

func asList(value any) []map[string]any {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		if object, ok := entry.(map[string]any); ok {
			out = append(out, object)
		}
	}
	return out
}
