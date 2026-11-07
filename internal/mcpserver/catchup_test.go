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

func TestMCPServerCatchupToolOnlyWithCatchupBackend(t *testing.T) {
	names := func(backend Backend) string {
		var list []string
		for _, tool := range New(backend, "test").toolList() {
			list = append(list, tool.Name)
		}
		return strings.Join(list, ",")
	}
	if strings.Contains(names(&fakeBackend{}), "brain_catchup") {
		t.Fatal("catchup tool listed for a backend without catchup support")
	}
	if !strings.Contains(names(&catchupFake{}), "brain_catchup") {
		t.Fatal("catchup tool missing for a catchup backend")
	}
}
