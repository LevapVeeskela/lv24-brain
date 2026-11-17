package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"
)

type catchupFake struct {
	fakeBackend
	gotProject string
	gotSince   time.Time
	gotLimit   int
}

func (f *catchupFake) Catchup(_ context.Context, projectID string, since time.Time, limit int) (Catchup, error) {
	f.gotProject, f.gotSince, f.gotLimit = projectID, since, limit
	return Catchup{
		ProjectID:   projectID,
		SinceSource: "handoff",
		Notes:       []CatchupItem{{Kind: "note", ID: "note-alpha-1", Title: "After handoff"}},
		Journal:     []string{},
	}, nil
}

func TestMCPServerCatchupTool(t *testing.T) {
	backend := &catchupFake{}

	got := callTool(t, backend, "brain_catchup", `{"project":"alpha","since_hours":24,"limit":5}`)
	if got["isError"] == true {
		t.Fatalf("brain_catchup error: %+v", got)
	}
	if backend.gotProject != "alpha" || backend.gotLimit != 5 {
		t.Fatalf("catchup called with project %q limit %d", backend.gotProject, backend.gotLimit)
	}
	if age := time.Since(backend.gotSince); age < 24*time.Hour-time.Minute || age > 24*time.Hour+time.Minute {
		t.Fatalf("since = %v ago, want about 24h", age)
	}
	text := got["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "After handoff") {
		t.Fatalf("text = %q", text)
	}
	if got["structuredContent"].(map[string]any)["project_id"] != "alpha" {
		t.Fatalf("structured = %+v", got["structuredContent"])
	}

	if got = callTool(t, backend, "brain_catchup", `{}`); got["isError"] == true {
		t.Fatalf("brain_catchup without arguments: %+v", got)
	}
	if !backend.gotSince.IsZero() {
		t.Fatalf("since_hours 0 must use the latest handoff, got %v", backend.gotSince)
	}

	if got = callTool(t, backend, "brain_catchup", `{"since_hours":-1}`); got["isError"] != true {
		t.Fatalf("negative since_hours accepted: %+v", got)
	}
}

// brain_journal needs an explicit scope: a project, or all=true for every
// project. The schema says the same, so a caller is never surprised by an
// implicit all-project stream.
func TestMCPServerJournalRequiresProjectOrDefaultAll(t *testing.T) {
	backend := &fakeBackend{}
	unscoped := toolCall(t, backend, "brain_catchup", `{"mode":"journal"}`)
	if unscoped["isError"] != true || structured(t, unscoped)["code"] != RefusalInvalidArguments {
		t.Fatalf("journal without scope = %v, want a typed invalid refusal", unscoped)
	}
	if !strings.Contains(unscoped["content"].([]any)[0].(map[string]any)["text"].(string), "all=true") {
		t.Fatalf("refusal does not name all=true: %v", unscoped)
	}

	explicitAll := toolCall(t, backend, "brain_catchup", `{"mode":"journal","all":true}`)
	if explicitAll["isError"] == true || backend.journalQuery.ProjectID != "" {
		t.Fatalf("journal with all=true = %v, query = %+v", explicitAll, backend.journalQuery)
	}
	if both := toolCall(t, backend, "brain_catchup", `{"mode":"journal","project":"alpha","all":true}`); both["isError"] != true {
		t.Fatalf("project and all together accepted: %v", both)
	}
	if changes := toolCall(t, backend, "brain_catchup", `{"all":true}`); changes["isError"] != true {
		t.Fatalf("all outside mode=journal accepted: %v", changes)
	}

	var schema tool
	for _, candidate := range New(backend, "test").toolList() {
		if candidate.Name == "brain_catchup" {
			schema = candidate
		}
	}
	if _, ok := schema.InputSchema["properties"].(map[string]any)["all"]; !ok {
		t.Fatalf("brain_catchup schema has no all property: %v", schema.InputSchema)
	}
	if !strings.Contains(schema.Description, "requires project or all=true") {
		t.Fatalf("brain_catchup description does not state the scope rule: %q", schema.Description)
	}
}

func TestMCPServerCatchupToolIsAlwaysListed(t *testing.T) {
	names := func(backend Backend) string {
		var list []string
		for _, tool := range New(backend, "test").toolList() {
			list = append(list, tool.Name)
		}
		return strings.Join(list, ",")
	}
	if !strings.Contains(names(&fakeBackend{}), "brain_catchup") {
		t.Fatal("brain_catchup is missing from the 15-tool surface")
	}
	if strings.Contains(names(&fakeBackend{}), "brain_journal") {
		t.Fatal("retired brain_journal is still listed")
	}
}
