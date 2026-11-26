package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ideaCLI runs one `brain idea` command (or init/project/note setup) with the
// CLI test harness.
func ideaCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return runForTest(t, args...)
}

func ideaCLIOK(t *testing.T, args ...string) string {
	t.Helper()
	code, stdout, stderr := ideaCLI(t, args...)
	if code != 0 {
		t.Fatalf("%v exit %d: %s", args, code, stderr)
	}
	return stdout
}

// ideaJSONData decodes the --json envelope and returns its data object.
func ideaJSONData(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var doc struct {
		Schema int            `json:"schema"`
		Data   map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, stdout)
	}
	if doc.Schema != 1 {
		t.Fatalf("schema = %d, want 1", doc.Schema)
	}
	return doc.Data
}

func ideaJSONRow(t *testing.T, data map[string]any) map[string]any {
	t.Helper()
	row, ok := data["idea"].(map[string]any)
	if !ok {
		t.Fatalf("data.idea is not an object: %#v", data)
	}
	return row
}

func ideaJSONRows(t *testing.T, data map[string]any) []map[string]any {
	t.Helper()
	raw, ok := data["ideas"].([]any)
	if !ok {
		t.Fatalf("data.ideas is not a list: %#v", data)
	}
	rows := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("idea row is not an object: %#v", item)
		}
		rows = append(rows, row)
	}
	return rows
}

// ideaFixture is a temporary local brain plus helpers to register projects and
// create ideas through the real CLI.
type ideaFixture struct {
	t       *testing.T
	dataDir string
	cfg     string
}

func newIdeaFixture(t *testing.T) *ideaFixture {
	t.Helper()
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfg := filepath.Join(dataDir, "brain.yaml")
	ideaCLIOK(t, "init", "--mode", "local", "--data-dir", dataDir)
	return &ideaFixture{t: t, dataDir: dataDir, cfg: cfg}
}

// register creates a project by writing the first note through the normal
// write path.
func (f *ideaFixture) register(project string) {
	f.t.Helper()
	ideaCLIOK(f.t, "note", "--create", "--config", f.cfg, "--project", project,
		"--title", "seed "+project, "--body", "seed")
}

func (f *ideaFixture) addRaw(project, title string, flags ...string) string {
	f.t.Helper()
	args := []string{"idea", "add", title, "--config", f.cfg}
	if project != "" {
		args = append(args, "--project", project)
	}
	args = append(args, flags...)
	return ideaCLIOK(f.t, args...)
}

func (f *ideaFixture) add(project, title string, flags ...string) string {
	f.t.Helper()
	out := f.addRaw(project, title, flags...)
	fields := strings.Fields(out)
	if len(fields) != 2 || fields[0] != "idea" {
		f.t.Fatalf("add output = %q, want 'idea <id>'", out)
	}
	return fields[1]
}

func (f *ideaFixture) showRow(id string) map[string]any {
	f.t.Helper()
	return ideaJSONRow(f.t, ideaJSONData(f.t, ideaCLIOK(f.t, "idea", "show", id, "--config", f.cfg, "--json")))
}

func (f *ideaFixture) listRows(args ...string) ([]map[string]any, map[string]any) {
	f.t.Helper()
	full := append([]string{"idea", "list", "--config", f.cfg, "--json"}, args...)
	data := ideaJSONData(f.t, ideaCLIOK(f.t, full...))
	return ideaJSONRows(f.t, data), data
}

// localDate is a YYYY-MM-DD date in the machine's local zone, the way --due
// and due_date treat it.
func localDate(t time.Time) string {
	return t.In(time.Local).Format("2006-01-02")
}

func TestIdeaCLIAddShowAndFields(t *testing.T) {
	f := newIdeaFixture(t)
	f.register("demo")
	bodyPath := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(bodyPath, []byte("First line\nSecond line\n"), 0o600); err != nil {
		t.Fatalf("write body file: %v", err)
	}
	add := f.addRaw("demo", "Calendar dark theme",
		"--body-file", bodyPath,
		"--due", "2030-05-06", "--at", "18:30",
		"--remind", "2h",
		"--priority", "high",
		"--tag", "ui,calendar", "--tag", "design",
		"--source", "session:ses_01", "--harness", "opencode",
		"--json",
	)
	data := ideaJSONData(t, add)
	row := ideaJSONRow(t, data)
	if data["project_source"] != "explicit" {
		t.Fatalf("project_source = %v", data["project_source"])
	}
	if row["project_id"] != "demo" || row["title"] != "Calendar dark theme" {
		t.Fatalf("row = %#v", row)
	}
	if row["body"] != "First line\nSecond line" {
		t.Fatalf("body = %q", row["body"])
	}
	if row["status"] != "open" || row["priority"] != "high" {
		t.Fatalf("status/priority = %v/%v", row["status"], row["priority"])
	}
	expectedDue := time.Date(2030, 5, 6, 18, 30, 0, 0, time.Local).Format(time.RFC3339)
	if row["due_at"] != expectedDue || row["due_date"] != nil {
		t.Fatalf("due = %v/%v, want %s", row["due_at"], row["due_date"], expectedDue)
	}
	if row["remind_before_minutes"] != float64(120) {
		t.Fatalf("remind = %v", row["remind_before_minutes"])
	}
	if got := row["tags"].([]any); len(got) != 3 || got[0] != "ui" || got[1] != "calendar" || got[2] != "design" {
		t.Fatalf("tags = %#v", row["tags"])
	}
	source, ok := row["source"].(map[string]any)
	if !ok || source["type"] != "session" || source["id"] != "ses_01" || source["harness"] != "opencode" {
		t.Fatalf("source = %#v", row["source"])
	}
	if !strings.HasPrefix(row["path"].(string), "projects/demo/ideas/idea-") {
		t.Fatalf("path = %v", row["path"])
	}
	if row["created_at"] == nil || row["updated_at"] == nil {
		t.Fatalf("timestamps missing: %#v", row)
	}
	if relations, ok := row["relations"].([]any); !ok || len(relations) != 0 {
		t.Fatalf("relations = %#v", row["relations"])
	}
	provenance := row["provenance"].(map[string]any)
	if provenance["origin"] != "human" || provenance["reviewed"] != true || provenance["trust"] != "verified" {
		t.Fatalf("provenance = %#v", provenance)
	}

	// show includes the body; list omits it.
	showData := ideaJSONData(t, ideaCLIOK(t, "idea", "show", row["id"].(string), "--config", f.cfg, "--json"))
	if showData["idea"].(map[string]any)["body"] != "First line\nSecond line" {
		t.Fatalf("show body = %#v", showData["idea"])
	}
	rows, _ := f.listRows("--project", "demo")
	if len(rows) != 1 {
		t.Fatalf("list = %#v", rows)
	}
	if _, ok := rows[0]["body"]; ok {
		t.Fatalf("list row leaked the body: %#v", rows[0])
	}

	human := ideaCLIOK(t, "idea", "show", row["id"].(string), "--config", f.cfg)
	for _, want := range []string{
		"title: Calendar dark theme",
		"project: demo",
		"priority: high",
		"due: " + expectedDue,
		"remind: 2h before",
		"tags: ui, calendar, design",
		"source: session ses_01 (opencode)",
		"First line",
		"Second line",
	} {
		if !strings.Contains(human, want) {
			t.Fatalf("show missing %q in %q", want, human)
		}
	}
}

func TestIdeaCLIDueForms(t *testing.T) {
	f := newIdeaFixture(t)
	f.register("demo")

	allDay := ideaJSONRow(t, ideaJSONData(t, f.addRaw("demo", "All day", "--due", "2030-05-06", "--json")))
	if allDay["due_at"] != nil || allDay["due_date"] != "2030-05-06" {
		t.Fatalf("all-day due = %v/%v", allDay["due_at"], allDay["due_date"])
	}

	timed := ideaJSONRow(t, ideaJSONData(t, f.addRaw("demo", "Timed",
		"--due", "2030-05-06T09:15:00+05:30", "--json")))
	if timed["due_at"] != "2030-05-06T09:15:00+05:30" || timed["due_date"] != nil {
		t.Fatalf("timed due lost its offset: %v/%v", timed["due_at"], timed["due_date"])
	}

	atDue := ideaJSONRow(t, ideaJSONData(t, f.addRaw("demo", "At time",
		"--due", "2030-05-06", "--at", "18:30", "--json")))
	expected := time.Date(2030, 5, 6, 18, 30, 0, 0, time.Local).Format(time.RFC3339)
	if atDue["due_at"] != expected {
		t.Fatalf("--at due = %v, want %s", atDue["due_at"], expected)
	}

	tomorrow := time.Now().In(time.Local).AddDate(0, 0, 1)
	tomorrowDue := ideaJSONRow(t, ideaJSONData(t, f.addRaw("demo", "Tomorrow",
		"--due", "tomorrow", "--at", "07:05", "--json")))
	if tomorrowDue["due_at"] != time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 7, 5, 0, 0, time.Local).Format(time.RFC3339) {
		t.Fatalf("tomorrow --at = %v", tomorrowDue["due_at"])
	}

	// edit --clear-due removes both forms.
	id := f.add("demo", "Clear me", "--due", "2030-05-07", "--remind", "1h")
	ideaCLIOK(t, "idea", "edit", id, "--config", f.cfg, "--clear-due", "--clear-remind")
	row := f.showRow(id)
	if row["due_at"] != nil || row["due_date"] != nil || row["remind_before_minutes"] != nil {
		t.Fatalf("clear did not remove due/remind: %#v", row)
	}

	// Setting a timed due replaces an all-day due, and vice versa.
	ideaCLIOK(t, "idea", "edit", id, "--config", f.cfg, "--due", "2030-05-08T10:00:00Z")
	row = f.showRow(id)
	if row["due_at"] != "2030-05-08T10:00:00Z" || row["due_date"] != nil {
		t.Fatalf("timed replacement = %v/%v", row["due_at"], row["due_date"])
	}
	ideaCLIOK(t, "idea", "edit", id, "--config", f.cfg, "--due", "2030-05-09")
	row = f.showRow(id)
	if row["due_at"] != nil || row["due_date"] != "2030-05-09" {
		t.Fatalf("all-day replacement = %v/%v", row["due_at"], row["due_date"])
	}

	for _, args := range [][]string{
		{"idea", "add", "X", "--config", f.cfg, "--project", "demo", "--at", "18:00"},
		{"idea", "add", "X", "--config", f.cfg, "--project", "demo", "--due", "2030-05-06", "--at", "25:99"},
		{"idea", "add", "X", "--config", f.cfg, "--project", "demo", "--due", "31.12.2030"},
		{"idea", "edit", id, "--config", f.cfg, "--clear-due", "--due", "2030-05-06"},
		{"idea", "edit", id, "--config", f.cfg, "--clear-remind", "--remind", "1h"},
		{"idea", "edit", id, "--config", f.cfg, "--clear-tags", "--tags", "a"},
	} {
		if code, _, _ := ideaCLI(t, args...); code != 2 {
			t.Fatalf("%v exit %d, want 2", args, code)
		}
	}
}

func TestIdeaCLIProjectResolution(t *testing.T) {
	f := newIdeaFixture(t)
	f.register("demo")
	f.register("personal")
	t.Chdir(t.TempDir())

	fallback := ideaJSONData(t, f.addRaw("", "Fallback", "--json"))
	if fallback["project_source"] != "default" || ideaJSONRow(t, fallback)["project_id"] != "personal" {
		t.Fatalf("personal fallback = %#v", fallback)
	}
	auto := ideaJSONData(t, f.addRaw("auto", "Auto", "--json"))
	if auto["project_source"] != "default" || ideaJSONRow(t, auto)["project_id"] != "personal" {
		t.Fatalf("--project auto = %#v", auto)
	}

	code, _, stderr := ideaCLI(t, "idea", "add", "Typo", "--config", f.cfg, "--project", "dem")
	if code != 1 || !strings.Contains(stderr, "candidates:") || !strings.Contains(stderr, "demo") {
		t.Fatalf("unknown project = exit %d, %q", code, stderr)
	}

	f.add("demo", "From demo")
	rows, data := f.listRows("--project", "all")
	if len(rows) != 3 {
		t.Fatalf("--project all = %#v", rows)
	}
	projects := map[string]bool{}
	for _, row := range rows {
		projects[row["project_id"].(string)] = true
	}
	if !projects["demo"] || !projects["personal"] {
		t.Fatalf("--project all missed a project: %#v", projects)
	}
	if _, ok := data["project_source"]; ok {
		t.Fatalf("--project all reported project_source: %#v", data)
	}

	// The working directory resolves when it is a registered checkout.
	checkout := t.TempDir()
	ideaCLIOK(t, "project", "set", "checkout", "--path", checkout, "--config", f.cfg)
	t.Chdir(checkout)
	fromCWD := ideaJSONData(t, f.addRaw("", "From cwd", "--json"))
	if fromCWD["project_source"] != "cwd" || ideaJSONRow(t, fromCWD)["project_id"] != "checkout" {
		t.Fatalf("cwd resolution = %#v", fromCWD)
	}

	if code, _, _ := ideaCLI(t, "idea", "add", "X", "--config", f.cfg, "--project", "all"); code != 2 {
		t.Fatalf("add --project all exit %d, want 2", code)
	}
	if code, _, _ := ideaCLI(t, "idea", "edit", f.add("demo", "No move"), "--config", f.cfg, "--project", "all"); code != 2 {
		t.Fatalf("edit --project all exit %d, want 2", code)
	}
}

func TestIdeaCLIStatusLifecycle(t *testing.T) {
	f := newIdeaFixture(t)
	f.register("demo")
	id := f.add("demo", "Flow")

	done := ideaJSONData(t, ideaCLIOK(t, "idea", "done", id, "--config", f.cfg, "--note", "finished", "--json"))
	row := ideaJSONRow(t, done)
	if row["status"] != "done" || row["completed_at"] == nil || row["completion_note"] != "finished" {
		t.Fatalf("done row = %#v", row)
	}
	if done["project_source"] == nil {
		t.Fatalf("done omitted project_source: %#v", done)
	}
	completedAt := row["completed_at"]
	again := ideaJSONRow(t, ideaJSONData(t, ideaCLIOK(t, "idea", "done", id, "--config", f.cfg, "--json")))
	if again["completed_at"] != completedAt {
		t.Fatalf("idempotent done moved completed_at: %v -> %v", completedAt, again["completed_at"])
	}

	list := ideaCLIOK(t, "idea", "list", "--config", f.cfg, "--project", "demo", "--done")
	if !strings.Contains(list, "DONE (1)") || !strings.Contains(list, "[x] ~~Flow~~") {
		t.Fatalf("done list = %q", list)
	}
	if code, _, stderr := ideaCLI(t, "idea", "drop", id, "--config", f.cfg); code != 1 || !strings.Contains(stderr, "cannot move") {
		t.Fatalf("done -> dropped = exit %d, %q", code, stderr)
	}

	reopened := ideaJSONRow(t, ideaJSONData(t, ideaCLIOK(t, "idea", "reopen", id, "--config", f.cfg, "--json")))
	if reopened["status"] != "open" || reopened["completed_at"] != nil || reopened["completion_note"] != "" {
		t.Fatalf("reopen = %#v", reopened)
	}

	dropped := ideaJSONRow(t, ideaJSONData(t, ideaCLIOK(t, "idea", "drop", id, "--config", f.cfg, "--reason", "not now", "--json")))
	if dropped["status"] != "dropped" || dropped["completion_note"] != "not now" || dropped["completed_at"] != nil {
		t.Fatalf("drop = %#v", dropped)
	}
	list = ideaCLIOK(t, "idea", "list", "--config", f.cfg, "--project", "demo", "--all")
	if !strings.Contains(list, "[~] ~~Flow~~") {
		t.Fatalf("dropped list = %q", list)
	}

	// Status transitions through edit follow the same rules.
	if code, _, _ := ideaCLI(t, "idea", "edit", id, "--config", f.cfg, "--status", "doing"); code != 1 {
		t.Fatalf("dropped -> doing exit %d, want 1", code)
	}
	ideaCLIOK(t, "idea", "edit", id, "--config", f.cfg, "--status", "open")
	ideaCLIOK(t, "idea", "edit", id, "--config", f.cfg, "--status", "someday")
	row = f.showRow(id)
	if row["status"] != "someday" {
		t.Fatalf("someday edit = %#v", row)
	}
	if rows, _ := f.listRows("--project", "demo"); len(rows) != 0 {
		t.Fatalf("default list included someday: %#v", rows)
	}
	if rows, _ := f.listRows("--project", "demo", "--status", "someday"); len(rows) != 1 {
		t.Fatalf("someday filter = %#v", rows)
	}
}

func TestIdeaCLIEditTrashRestore(t *testing.T) {
	f := newIdeaFixture(t)
	f.register("demo")
	f.register("other")
	id := f.add("demo", "Original", "--due", "2030-05-06", "--tag", "ui")

	body := "Edited body\nwith a second line"
	code, _, stderr := runWithInputForTest(t, body,
		"idea", "edit", id, "--config", f.cfg, "--title", "Edited", "--body-file", "-",
		"--tags", "ui,api", "--priority", "low", "--status", "doing", "--project", "other")
	if code != 0 {
		t.Fatalf("edit exit %d: %s", code, stderr)
	}
	row := f.showRow(id)
	if row["title"] != "Edited" || row["body"] != body || row["status"] != "doing" ||
		row["priority"] != "low" || row["project_id"] != "other" {
		t.Fatalf("edited row = %#v", row)
	}
	if got := row["tags"].([]any); len(got) != 2 || got[0] != "ui" || got[1] != "api" {
		t.Fatalf("edited tags = %#v", row["tags"])
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "projects", "demo", "ideas", id+".md")); !os.IsNotExist(err) {
		t.Fatalf("old project file still exists: %v", err)
	}

	removed := ideaJSONData(t, ideaCLIOK(t, "idea", "rm", id, "--config", f.cfg, "--json"))
	if removed["id"] != id || removed["trashed"] != true {
		t.Fatalf("rm = %#v", removed)
	}
	if code, _, stderr := ideaCLI(t, "idea", "show", id, "--config", f.cfg); code != 1 || !strings.Contains(stderr, "not found") {
		t.Fatalf("show trashed = exit %d, %q", code, stderr)
	}
	if rows, _ := f.listRows("--project", "demo"); len(rows) != 0 {
		t.Fatalf("trashed idea still listed: %#v", rows)
	}

	restored := ideaJSONData(t, ideaCLIOK(t, "idea", "restore", id, "--config", f.cfg, "--json"))
	row = ideaJSONRow(t, restored)
	if restored["project_source"] == nil || row["id"] != id || row["body"] != body || row["title"] != "Edited" {
		t.Fatalf("restore = %#v", restored)
	}
	if code, _, _ := ideaCLI(t, "idea", "restore", id, "--config", f.cfg); code != 1 {
		t.Fatalf("restore twice exit %d, want 1", code)
	}
}

func TestIdeaCLIListFilters(t *testing.T) {
	f := newIdeaFixture(t)
	f.register("demo")
	f.register("other")
	today := localDate(time.Now())
	tomorrow := localDate(time.Now().AddDate(0, 0, 1))
	nextWeek := localDate(time.Now().AddDate(0, 0, 10))

	todayID := f.add("demo", "Due today", "--due", today, "--tag", "ui,calendar")
	tomorrowID := f.add("demo", "Due tomorrow", "--due", tomorrow, "--tag", "ui")
	pastID := f.add("demo", "Overdue", "--due", "2020-01-01")
	f.add("demo", "Later", "--due", nextWeek)
	f.add("other", "Other project", "--tag", "api")
	f.add("demo", "Searchable needle")
	doneID := f.add("demo", "Finished item")
	ideaCLIOK(t, "idea", "done", doneID, "--config", f.cfg)

	defaultRows, _ := f.listRows("--project", "demo")
	if len(defaultRows) != 5 {
		t.Fatalf("default list = %d rows, want 5: %#v", len(defaultRows), defaultRows)
	}
	for _, row := range defaultRows {
		if row["status"] == "done" {
			t.Fatalf("default list included done: %#v", row)
		}
	}
	if rows, _ := f.listRows("--project", "demo", "--done"); len(rows) != 1 || rows[0]["id"] != doneID {
		t.Fatalf("--done = %#v", rows)
	}
	if rows, _ := f.listRows("--project", "demo", "--all"); len(rows) != 6 {
		t.Fatalf("--all = %d rows, want 6", len(rows))
	}
	if rows, _ := f.listRows("--project", "demo", "--status", "open", "--tag", "ui"); len(rows) != 2 {
		t.Fatalf("AND tag filter = %#v", rows)
	}
	if rows, _ := f.listRows("--project", "demo", "--status", "open", "--status", "done"); len(rows) != 6 {
		t.Fatalf("repeated --status = %#v", rows)
	}
	if rows, _ := f.listRows("--project", "demo", "--tag", "ui", "--tag", "calendar"); len(rows) != 1 || rows[0]["id"] != todayID {
		t.Fatalf("two tags = %#v", rows)
	}
	if rows, _ := f.listRows("--project", "demo", "--search", "needle"); len(rows) != 1 || rows[0]["title"] != "Searchable needle" {
		t.Fatalf("--search = %#v", rows)
	}
	if rows, _ := f.listRows("--project", "demo", "--due", "today"); len(rows) != 1 || rows[0]["id"] != todayID {
		t.Fatalf("--due today = %#v", rows)
	}
	if rows, _ := f.listRows("--project", "demo", "--due", "tomorrow"); len(rows) != 1 || rows[0]["id"] != tomorrowID {
		t.Fatalf("--due tomorrow = %#v", rows)
	}
	if rows, _ := f.listRows("--project", "demo", "--due", "overdue"); len(rows) != 1 || rows[0]["id"] != pastID {
		t.Fatalf("--due overdue = %#v", rows)
	}
	if rows, _ := f.listRows("--project", "demo", "--due", "week"); len(rows) != 2 {
		t.Fatalf("--due week = %#v", rows)
	}
	if rows, _ := f.listRows("--project", "other"); len(rows) != 1 || rows[0]["title"] != "Other project" {
		t.Fatalf("--project other = %#v", rows)
	}
	if rows, _ := f.listRows("--project", "all"); len(rows) != 6 {
		t.Fatalf("--project all = %#v", len(rows))
	}
	rows, data := f.listRows("--project", "demo", "--limit", "1")
	if len(rows) != 1 || data["total"] != float64(5) {
		t.Fatalf("--limit 1 = %d rows, total %v", len(rows), data["total"])
	}
	if rows, _ := f.listRows("--project", "demo", "--limit", "0"); len(rows) != 5 {
		t.Fatalf("--limit 0 = %d rows", len(rows))
	}
	if !strings.Contains(ideaCLIOK(t, "idea", "list", "--config", f.cfg, "--project", "demo"), "OPEN (5)") {
		t.Fatalf("human list section missing")
	}

	for _, args := range [][]string{
		{"idea", "list", "--config", f.cfg, "--open", "--done"},
		{"idea", "list", "--config", f.cfg, "--limit", "1001"},
		{"idea", "list", "--config", f.cfg, "--due", "someday"},
		{"idea", "list", "--config", f.cfg, "--status", "nope"},
		{"idea", "list", "--config", f.cfg, "--project", "demo", "unexpected"},
	} {
		if code, _, _ := ideaCLI(t, args...); code != 2 {
			t.Fatalf("%v exit %d, want 2", args, code)
		}
	}

	empty := newIdeaFixture(t)
	empty.register("demo")
	if out := ideaCLIOK(t, "idea", "list", "--config", empty.cfg, "--project", "demo"); strings.TrimSpace(out) != "no ideas" {
		t.Fatalf("empty list = %q", out)
	}
	if code, _, _ := ideaCLI(t, "idea", "list", "--config", f.cfg, "--project", "nope"); code != 1 {
		t.Fatalf("unknown project list exit %d, want 1", code)
	}
}

func TestIdeaCLIBrief(t *testing.T) {
	f := newIdeaFixture(t)
	f.register("demo")
	f.register("other")
	bodyPath := filepath.Join(t.TempDir(), "body.md")
	sentinel := "BODY SENTINEL MUST NOT ENTER THE BRIEF"
	if err := os.WriteFile(bodyPath, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("write body: %v", err)
	}
	f.add("demo", "Due today demo", "--due", localDate(time.Now()), "--body-file", bodyPath)
	f.add("demo", "Overdue demo", "--due", "2020-01-01")
	f.add("demo", "High demo", "--priority", "high")
	f.add("demo", "Fourth demo")
	f.add("other", "Other overdue", "--due", "2020-01-02")
	parked := f.add("demo", "Parked someday")
	ideaCLIOK(t, "idea", "edit", parked, "--config", f.cfg, "--status", "someday")

	data := ideaJSONData(t, ideaCLIOK(t, "idea", "brief", "--config", f.cfg, "--json"))
	if data["open"] != float64(5) || data["due_today"] != float64(1) || data["muted"] != false {
		t.Fatalf("brief counts = %#v", data)
	}
	text := data["text"].(string)
	if !strings.HasPrefix(text, "## Ideas — 5 open · 1 due today\n") {
		t.Fatalf("brief header = %q", text)
	}
	if strings.Contains(text, "Parked someday") {
		t.Fatalf("brief included a parked someday idea: %q", text)
	}
	if strings.Contains(text, sentinel) {
		t.Fatalf("brief leaked the body: %q", text)
	}
	if parkedRows, _ := f.listRows("--project", "demo", "--status", "someday"); len(parkedRows) != 1 || parkedRows[0]["id"] != parked {
		t.Fatalf("someday disappeared from list --status: %#v", parkedRows)
	}
	rows := ideaJSONRows(t, data)
	if len(rows) != 3 {
		t.Fatalf("brief max 3 = %d rows: %#v", len(rows), rows)
	}
	for _, row := range rows {
		if _, ok := row["body"]; ok {
			t.Fatalf("brief row leaked the body: %#v", row)
		}
	}

	// Current project items come first when a project is named for ordering.
	text = ideaJSONData(t, ideaCLIOK(t, "idea", "brief", "--config", f.cfg, "--project", "demo", "--json"))["text"].(string)
	lines := strings.Split(text, "\n")
	if len(lines) < 3 || !strings.Contains(lines[1], "(demo)") || !strings.Contains(lines[2], "(demo)") {
		t.Fatalf("current project was not ordered first: %q", text)
	}
	if !strings.Contains(text, "+2 more · brain idea list") {
		t.Fatalf("brief footer = %q", text)
	}

	scoped := ideaJSONData(t, ideaCLIOK(t, "idea", "brief", "--config", f.cfg, "--scope", "current", "--project", "demo", "--json"))
	if scoped["open"] != float64(4) {
		t.Fatalf("current scope = %#v", scoped)
	}
	if strings.Contains(scoped["text"].(string), "Other overdue") {
		t.Fatalf("current brief leaked another project: %q", scoped["text"])
	}

	one := ideaJSONData(t, ideaCLIOK(t, "idea", "brief", "--config", f.cfg, "--max", "1", "--json"))
	if rows := ideaJSONRows(t, one); len(rows) != 1 {
		t.Fatalf("--max 1 = %#v", rows)
	}

	// The human command prints exactly the shared renderer's text.
	human := strings.TrimRight(ideaCLIOK(t, "idea", "brief", "--config", f.cfg), "\n")
	if human != data["text"] {
		t.Fatalf("human brief differs from JSON text:\n%q\n%q", human, data["text"])
	}

	// The 700-character budget holds even with the maximum item count.
	for i := 0; i < 12; i++ {
		f.add("demo", strings.Repeat("Long idea ", 12)+string(rune('A'+i)), "--due", "2020-01-03")
	}
	long := ideaJSONData(t, ideaCLIOK(t, "idea", "brief", "--config", f.cfg, "--max", "10", "--json"))
	if got := len([]rune(long["text"].(string))); got > 700 {
		t.Fatalf("brief is %d characters, want <= 700", got)
	}

	// Mute is machine-local state and replaces the block with one line.
	muted := ideaJSONData(t, ideaCLIOK(t, "idea", "mute", "--config", f.cfg, "--days", "3", "--json"))
	if muted["muted_until"] == nil {
		t.Fatalf("mute did not return a deadline: %#v", muted)
	}
	mutePath := filepath.Join(f.dataDir, "agent", "ideas.json")
	if _, err := os.Stat(mutePath); err != nil {
		t.Fatalf("mute file missing at %s: %v", mutePath, err)
	}
	mutedBrief := ideaJSONData(t, ideaCLIOK(t, "idea", "brief", "--config", f.cfg, "--json"))
	mutedText := fmt.Sprintf("ideas: %v open (greeting muted)", long["open"])
	if mutedBrief["muted"] != true || mutedBrief["text"] != mutedText {
		t.Fatalf("muted brief = %#v", mutedBrief)
	}
	if rows := ideaJSONRows(t, mutedBrief); len(rows) != 0 {
		t.Fatalf("muted brief showed items: %#v", rows)
	}
	ideaCLIOK(t, "idea", "mute", "--config", f.cfg, "--off")
	if data := ideaJSONData(t, ideaCLIOK(t, "idea", "brief", "--config", f.cfg, "--json")); data["muted"] != false {
		t.Fatalf("unmute did not clear the brief: %#v", data)
	}

	for _, args := range [][]string{
		{"idea", "brief", "--config", f.cfg, "--max", "11"},
		{"idea", "brief", "--config", f.cfg, "--max", "0"},
		{"idea", "brief", "--config", f.cfg, "--scope", "nope"},
		{"idea", "brief", "--config", f.cfg, "--project", "all", "--scope", "current"},
		{"idea", "brief", "--config", f.cfg, "--project", "nope"},
		{"idea", "mute", "--config", f.cfg, "--days", "0"},
		{"idea", "mute", "--config", f.cfg, "--off", "--days", "2"},
	} {
		if code, _, _ := ideaCLI(t, args...); code == 0 {
			t.Fatalf("%v unexpectedly succeeded", args)
		}
	}
}

// icalEventBlock returns one unfolded VEVENT block by its UID, from UID to
// END:VEVENT. Unfolding makes the lookup independent of where the line was
// folded.
func icalEventBlock(t *testing.T, calendar, id string) string {
	t.Helper()
	unfolded := strings.ReplaceAll(calendar, "\r\n ", "")
	start := strings.Index(unfolded, "UID:"+id+"\r\n")
	if start < 0 {
		t.Fatalf("VEVENT %s is missing:\n%s", id, calendar)
	}
	end := strings.Index(unfolded[start:], "END:VEVENT")
	if end < 0 {
		t.Fatalf("VEVENT %s is unterminated", id)
	}
	return unfolded[start : start+end]
}

func TestIdeaCLIICAL(t *testing.T) {
	f := newIdeaFixture(t)
	f.register("demo")
	f.register("other")
	allDay := f.add("demo", "All day, with; chars", "--due", "2030-05-06", "--remind", "1d")
	timed := f.add("demo", "Timed", "--due", "2030-05-07T18:00:00+03:00", "--remind", "2h")
	noAlarm := f.add("demo", "No alarm", "--due", "2030-05-08T10:00:00Z")
	f.add("demo", "No date")
	done := f.add("demo", "Done dated", "--due", "2030-05-09")
	ideaCLIOK(t, "idea", "done", done, "--config", f.cfg)
	f.add("other", "Other dated", "--due", "2030-05-10")
	longID := f.add("demo", strings.Repeat("Long ", 20), "--due", "2030-05-11")

	out := ideaCLIOK(t, "idea", "ical", "--config", f.cfg, "--project", "demo")
	allDayDueTrigger := "TRIGGER;VALUE=DATE-TIME:" +
		time.Date(2030, 5, 6, 9, 0, 0, 0, time.Local).UTC().Format("20060102T150405Z")
	longDueTrigger := "TRIGGER;VALUE=DATE-TIME:" +
		time.Date(2030, 5, 11, 9, 0, 0, 0, time.Local).UTC().Format("20060102T150405Z")
	for _, want := range []string{
		"BEGIN:VCALENDAR\r\n",
		"END:VCALENDAR\r\n",
		"UID:" + allDay + "\r\n",
		"DTSTART;VALUE=DATE:20300506\r\n",
		"SUMMARY:All day\\, with\\; chars\r\n",
		"TRIGGER:-PT1440M\r\n",
		allDayDueTrigger + "\r\n",
		"UID:" + timed + "\r\n",
		"DTSTART:20300507T150000Z\r\n",
		"TRIGGER:-PT120M\r\n",
		"TRIGGER:-PT0M\r\n",
		longDueTrigger + "\r\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("ical missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"No date", "Done dated", "Other dated", "STATUS:COMPLETED"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("ical contains %q:\n%s", unwanted, out)
		}
	}
	// Lead + due means two alarms; a timed idea without a lead still gets the
	// due-time alarm; an all-day idea without a lead gets one absolute alarm.
	allDayBlock := icalEventBlock(t, out, allDay)
	if got := strings.Count(allDayBlock, "BEGIN:VALARM"); got != 2 {
		t.Fatalf("all-day lead+due alarms = %d, want 2:\n%s", got, allDayBlock)
	}
	timedBlock := icalEventBlock(t, out, timed)
	if got := strings.Count(timedBlock, "BEGIN:VALARM"); got != 2 {
		t.Fatalf("timed lead+due alarms = %d, want 2:\n%s", got, timedBlock)
	}
	noAlarmBlock := icalEventBlock(t, out, noAlarm)
	if got := strings.Count(noAlarmBlock, "BEGIN:VALARM"); got != 1 {
		t.Fatalf("timed no-lead alarms = %d, want 1:\n%s", got, noAlarmBlock)
	}
	if !strings.Contains(noAlarmBlock, "TRIGGER:-PT0M\r\n") {
		t.Fatalf("timed no-lead block lacks the due-time trigger:\n%s", noAlarmBlock)
	}
	longBlock := icalEventBlock(t, out, longID)
	if got := strings.Count(longBlock, "BEGIN:VALARM"); got != 1 {
		t.Fatalf("all-day no-lead alarms = %d, want 1:\n%s", got, longBlock)
	}
	if !strings.Contains(longBlock, longDueTrigger) {
		t.Fatalf("all-day no-lead block lacks the absolute trigger:\n%s", longBlock)
	}
	if got := strings.Count(out, "BEGIN:VALARM"); got != 6 {
		t.Fatalf("VALARM count = %d, want 6", got)
	}
	if strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") {
		t.Fatalf("ical contains a bare LF")
	}
	if !strings.Contains(out, "\r\n ") {
		t.Fatalf("ical did not fold a long line")
	}
	for _, line := range strings.Split(out, "\r\n") {
		if len(line) > 75 {
			t.Fatalf("ical line is %d octets: %q", len(line), line)
		}
	}
}

func TestIdeaCLIProblemRows(t *testing.T) {
	f := newIdeaFixture(t)
	f.register("demo")
	validID := f.add("demo", "Still visible")
	problemID := "idea-2030-01-01-conflict-01jcs6abcdefghjkmnpqrstvwx"
	problemPath := filepath.Join(f.dataDir, "projects", "demo", "ideas", problemID+".md")
	conflict := "---\nproject_id: demo\nkind: idea\nid: " + problemID + "\ntitle: Conflict\n---\n" +
		"<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> origin/main\n"
	if err := os.WriteFile(problemPath, []byte(conflict), 0o600); err != nil {
		t.Fatalf("write problem file: %v", err)
	}
	before, err := os.ReadFile(problemPath)
	if err != nil {
		t.Fatalf("read problem file: %v", err)
	}

	out := ideaCLIOK(t, "idea", "list", "--config", f.cfg, "--project", "demo")
	if !strings.Contains(out, "OPEN (1)") || !strings.Contains(out, "Still visible") {
		t.Fatalf("problem broke the healthy list: %q", out)
	}
	if !strings.Contains(out, "PROBLEMS (1)") ||
		!strings.Contains(out, "projects/demo/ideas/"+problemID+".md: git conflict markers") {
		t.Fatalf("problem section = %q", out)
	}
	if strings.Contains(out, "\\") {
		t.Fatalf("problem path is not forward-slash: %q", out)
	}

	rows, data := f.listRows("--project", "demo")
	if len(rows) != 1 || rows[0]["id"] != validID {
		t.Fatalf("list rows = %#v", rows)
	}
	problems := data["problems"].([]any)
	problem := problems[0].(map[string]any)
	if problem["kind"] != "idea" || !strings.Contains(problem["path"].(string), "/") {
		t.Fatalf("problem json = %#v", problem)
	}

	code, _, stderr := ideaCLI(t, "idea", "show", problemID, "--config", f.cfg)
	if code != 1 || !strings.Contains(stderr, problemID+".md") || !strings.Contains(stderr, "git conflict markers") {
		t.Fatalf("show problem = exit %d, %q", code, stderr)
	}
	if code, _, _ := ideaCLI(t, "idea", "edit", problemID, "--config", f.cfg, "--title", "rewrite"); code != 1 {
		t.Fatalf("edit problem exit %d, want 1", code)
	}
	if code, _, _ := ideaCLI(t, "idea", "rm", problemID, "--config", f.cfg); code != 1 {
		t.Fatalf("rm problem exit %d, want 1", code)
	}
	after, err := os.ReadFile(problemPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("problem file was modified: %v", err)
	}
	if code, _, _ := ideaCLI(t, "idea", "list", "--config", f.cfg, "--project", "all"); code != 0 {
		t.Fatalf("global list with a problem exited %d", code)
	}
}

func TestIdeaCLIUsageErrors(t *testing.T) {
	f := newIdeaFixture(t)
	f.register("demo")
	id := f.add("demo", "Usage")

	for _, args := range [][]string{
		{"idea"},
		{"idea", "frobnicate", "--config", f.cfg},
		{"idea", "add", "--config", f.cfg, "--project", "demo"},
		{"idea", "show", "--config", f.cfg},
		{"idea", "done", "--config", f.cfg},
		{"idea", "add", "X", "--config", f.cfg, "--project", "demo", "--harness", "opencode"},
		{"idea", "add", "X", "--config", f.cfg, "--project", "demo", "--source", "nope"},
		{"idea", "add", "X", "--config", f.cfg, "--project", "demo", "--source", "session"},
		{"idea", "add", "X", "--config", f.cfg, "--project", "demo", "--remind", "8d"},
		{"idea", "add", "X", "--config", f.cfg, "--project", "demo", "--priority", "urgent"},
		{"idea", "add", "X", "--config", f.cfg, "--project", "all"},
		{"idea", "ical", "--config", f.cfg, "--project", "all"},
		{"idea", "edit", id, "--config", f.cfg},
		{"idea", "edit", id, "--config", f.cfg, "--status", "nope"},
		{"idea", "edit", id, "--config", f.cfg, "--priority", "urgent"},
	} {
		if code, _, _ := ideaCLI(t, args...); code != 2 {
			t.Fatalf("%v exit %d, want 2", args, code)
		}
	}

	if code, _, _ := ideaCLI(t, "idea", "add", "Too many tags", "--config", f.cfg, "--project", "demo",
		"--tag", "1,2,3,4,5,6,7,8,9,10,11"); code != 1 {
		t.Fatalf("over-limit tags exit %d, want 1", code)
	}
	if code, stdout, _ := ideaCLI(t, "idea", "--help"); code != 0 || !strings.Contains(stdout, "brain idea <action>") {
		t.Fatalf("idea --help = exit %d, %q", code, stdout)
	}
	if code, _, _ := ideaCLI(t, "idea", "add", "--help"); code != 0 {
		t.Fatalf("idea add --help exit %d, want 0", code)
	}
}
