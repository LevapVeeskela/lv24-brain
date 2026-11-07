package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// jsonTestBrain creates a local brain with one project that has a note, so
// search, journal and open have something to return.
func jsonTestBrain(t *testing.T) string {
	t.Helper()
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}
	if code, _, stderr := runForTest(t, "note", "--create", "--config", cfgPath, "--project", "demo",
		"--title", "Searchable note", "--body", "jsonsearchable body"); code != 0 {
		t.Fatalf("note exit code = %d (stderr: %s)", code, stderr)
	}
	if code, _, stderr := runForTest(t, "state", "set", "demo", "--config", cfgPath,
		"--goal", "Ship JSON output", "--next", "Write tests",
		"--active", "jsonout", "--question", "Which fields?"); code != 0 {
		t.Fatalf("state set exit code = %d (stderr: %s)", code, stderr)
	}
	return cfgPath
}

// runJSONForTest runs args, requires exit code 0, and checks that stdout is
// exactly one JSON document in the {"schema": 1, "data": {...}} envelope. It
// returns the decoded top-level object and its data object.
func runJSONForTest(t *testing.T, args ...string) (map[string]any, map[string]any) {
	t.Helper()
	code, stdout, stderr := runForTest(t, args...)
	if code != 0 {
		t.Fatalf("%v exit code = %d (stderr: %s)", args, code, stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("%v stdout is not a single JSON document: %v\n%s", args, err, stdout)
	}
	if len(doc) != 2 {
		t.Fatalf("%v envelope keys = %v, want schema and data only", args, keysOf(doc))
	}
	if doc["schema"] != float64(1) {
		t.Fatalf("%v schema = %v, want 1", args, doc["schema"])
	}
	data, ok := doc["data"].(map[string]any)
	if !ok {
		t.Fatalf("%v data is %T, want object", args, doc["data"])
	}
	return doc, data
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

func requireKeys(t *testing.T, label string, m map[string]any, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := m[key]; !ok {
			t.Fatalf("%s is missing key %q: %v", label, key, keysOf(m))
		}
	}
}

func asObject(t *testing.T, label string, value any) map[string]any {
	t.Helper()
	obj, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want object", label, value)
	}
	return obj
}

func asArray(t *testing.T, label string, value any) []any {
	t.Helper()
	arr, ok := value.([]any)
	if !ok {
		t.Fatalf("%s is %T, want array", label, value)
	}
	return arr
}

func TestWriteJSONEnvelope(t *testing.T) {
	var buf bytes.Buffer
	if err := writeJSON(&buf, domain.Project{ID: "demo"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "{\n  \"schema\": 1,\n  \"data\": {\n") {
		t.Fatalf("envelope is not schema-first and indented:\n%s", buf.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	requireKeys(t, "envelope", doc, "schema", "data")
}

func TestJSONOpen(t *testing.T) {
	cfgPath := jsonTestBrain(t)
	_, data := runJSONForTest(t, "open", "--json", "--project", "demo", "--config", cfgPath)
	requireKeys(t, "open", data, "project", "state", "git", "graphs", "summary")

	project := asObject(t, "project", data["project"])
	requireKeys(t, "project", project, "id", "name", "kind", "paths", "aliases", "repo_url")
	if project["id"] != "demo" {
		t.Fatalf("project.id = %v, want demo", project["id"])
	}

	state := asObject(t, "state", data["state"])
	requireKeys(t, "state", state, "goal", "next_action", "active", "blocked", "open_questions", "updated_at", "fresh_until")
	if state["goal"] != "Ship JSON output" || state["next_action"] != "Write tests" {
		t.Fatalf("state = %v", state)
	}
	if active := asArray(t, "state.active", state["active"]); len(active) != 1 || active[0] != "jsonout" {
		t.Fatalf("state.active = %v", active)
	}
	if state["fresh_until"] != nil {
		t.Fatalf("state.fresh_until = %v, want null when unset", state["fresh_until"])
	}

	git := asObject(t, "git", data["git"])
	requireKeys(t, "git", git, "is_repo", "branch", "head", "dirty", "remote", "log")
	if git["is_repo"] != false {
		t.Fatalf("git.is_repo = %v, want false for a project without a checkout", git["is_repo"])
	}
	asArray(t, "graphs", data["graphs"])
	summary, _ := data["summary"].(string)
	if !strings.Contains(summary, "project_id: demo") {
		t.Fatalf("summary does not carry the bootstrap text:\n%s", summary)
	}
}

func TestJSONOpenBrief(t *testing.T) {
	cfgPath := jsonTestBrain(t)
	_, data := runJSONForTest(t, "open", "--brief", "--json", "--project", "demo", "--config", cfgPath)
	requireKeys(t, "open --brief", data, "project", "state")
	if _, ok := data["git"]; ok {
		t.Fatalf("open --brief --json must not include git: %v", keysOf(data))
	}
	if _, ok := data["summary"]; ok {
		t.Fatalf("open --brief --json must not include summary: %v", keysOf(data))
	}
	state := asObject(t, "state", data["state"])
	requireKeys(t, "state", state, "goal", "next_action", "updated_at")
}

func TestJSONBootstrap(t *testing.T) {
	cfgPath := jsonTestBrain(t)
	_, data := runJSONForTest(t, "bootstrap", "--json", "--project", "demo", "--config", cfgPath)
	requireKeys(t, "bootstrap", data, "project_id", "summary", "budget")
	if data["project_id"] != "demo" {
		t.Fatalf("project_id = %v", data["project_id"])
	}
	if data["budget"] != float64(app.DefaultBootstrapBudget) {
		t.Fatalf("budget = %v, want the default %d", data["budget"], app.DefaultBootstrapBudget)
	}
	if summary, _ := data["summary"].(string); !strings.Contains(summary, "bootstrap") {
		t.Fatalf("summary = %q", summary)
	}
}

func TestJSONSearch(t *testing.T) {
	cfgPath := jsonTestBrain(t)
	_, data := runJSONForTest(t, "search", "--json", "--config", cfgPath, "jsonsearchable")
	requireKeys(t, "search", data, "query", "hits")
	if data["query"] != "jsonsearchable" {
		t.Fatalf("query = %v", data["query"])
	}
	hits := asArray(t, "hits", data["hits"])
	if len(hits) == 0 {
		t.Fatalf("search returned no hits")
	}
	hit := asObject(t, "hits[0]", hits[0])
	requireKeys(t, "hit", hit, "id", "project_id", "kind", "title", "snippet", "score")

	_, empty := runJSONForTest(t, "search", "--json", "--config", cfgPath, "nothingmatchesthis")
	if hits := asArray(t, "hits", empty["hits"]); len(hits) != 0 {
		t.Fatalf("expected no hits, got %v", hits)
	}
}

func TestJSONJournal(t *testing.T) {
	cfgPath := jsonTestBrain(t)
	_, data := runJSONForTest(t, "journal", "--json", "--config", cfgPath, "--limit", "5")
	entries := asArray(t, "entries", data["entries"])
	if len(entries) == 0 {
		t.Fatalf("journal returned no entries")
	}
	entry := asObject(t, "entries[0]", entries[0])
	requireKeys(t, "entry", entry, "time", "user", "event", "project", "id", "title", "fields")
	if _, err := time.Parse(time.RFC3339, entry["time"].(string)); err != nil {
		t.Fatalf("entry.time = %v: %v", entry["time"], err)
	}
}

func TestJSONStatus(t *testing.T) {
	cfgPath := jsonTestBrain(t)
	_, data := runJSONForTest(t, "status", "--json", "--config", cfgPath)
	requireKeys(t, "status", data, "mode", "data_dir", "server_url", "user", "schema_version", "engine_version", "update_channel")
	if data["mode"] != "local" {
		t.Fatalf("mode = %v, want local", data["mode"])
	}
}

func TestJSONReview(t *testing.T) {
	cfgPath := jsonTestBrain(t)
	_, data := runJSONForTest(t, "review", "--json", "--config", cfgPath, "--project", "demo", "--since", "7d")
	requireKeys(t, "review", data, "summary", "items")
	if summary, _ := data["summary"].(string); !strings.Contains(summary, "# Review — demo") {
		t.Fatalf("review summary = %q", summary)
	}
	asArray(t, "items", data["items"])
}

func TestJSONDoctor(t *testing.T) {
	cfgPath := jsonTestBrain(t)
	_, data := runJSONForTest(t, "doctor", "--json", "--config", cfgPath)
	checks := asArray(t, "checks", data["checks"])
	if len(checks) == 0 {
		t.Fatalf("doctor returned no checks")
	}
	for _, raw := range checks {
		check := asObject(t, "check", raw)
		requireKeys(t, "check", check, "name", "ok", "status", "detail")
	}
}

func TestJSONDoctorExitCodeMatchesHumanOutput(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	missing := filepath.Join(t.TempDir(), "absent", "brain.yaml")
	humanCode, _, _ := runForTest(t, "doctor", "--config", missing)
	jsonCode, stdout, _ := runForTest(t, "doctor", "--json", "--config", missing)
	if jsonCode != humanCode {
		t.Fatalf("doctor --json exit code = %d, human = %d", jsonCode, humanCode)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("doctor --json stdout is not valid JSON: %v\n%s", err, stdout)
	}
}
