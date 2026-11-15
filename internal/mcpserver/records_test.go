package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

type recordFake struct {
	fakeBackend
	query       RecordQuery
	statusCalls []string
	supersedes  []string
}

func (f *recordFake) ListRecords(_ context.Context, query RecordQuery) ([]RecordSummary, error) {
	f.query = query
	return []RecordSummary{{Kind: "decision", ID: "decision-alpha-1", ProjectID: query.ProjectID, Title: "Use fsstore", Status: "proposed"}}, nil
}

func (f *recordFake) GetRecord(_ context.Context, projectID, id string) (RecordDetail, error) {
	if id != "decision-alpha-1" {
		return RecordDetail{}, fmt.Errorf("record %q: not found", id)
	}
	return RecordDetail{
		Kind: "decision", ID: id, ProjectID: projectID, Title: "Use fsstore", Status: "proposed", Context: "Need persistence",
		Links: []RecordLink{{Field: "evidence", Ref: "note-alpha-1", ProjectID: projectID, ID: "note-alpha-1", State: "found", Kind: "note", Title: "cache"}},
	}, nil
}

func (f *recordFake) SetDecisionStatus(_ context.Context, projectID, id, status string) (RecordSummary, error) {
	f.statusCalls = append(f.statusCalls, id+"="+status)
	return RecordSummary{Kind: "decision", ID: id, ProjectID: projectID, Status: status}, nil
}

func (f *recordFake) Supersede(_ context.Context, projectID, oldID, newID string) (RecordSummary, error) {
	f.supersedes = append(f.supersedes, oldID+"->"+newID)
	return RecordSummary{Kind: "decision", ID: oldID, ProjectID: projectID, Status: "superseded"}, nil
}

func textOf(t *testing.T, got map[string]any) string {
	t.Helper()
	content, ok := got["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content = %v", got["content"])
	}
	return content[0].(map[string]any)["text"].(string)
}

func TestMCPServerRecordsListAction(t *testing.T) {
	backend := &recordFake{}

	got := callTool(t, backend, "brain_record", `{"action":"list","project":"alpha","kinds":["decision"],"since_days":3,"status":"proposed","limit":5}`)
	if got["isError"] == true {
		t.Fatalf("brain_record action=list error: %+v", got)
	}
	// Page size 5 plus one look-ahead row.
	if backend.query.ProjectID != "alpha" || backend.query.Status != "proposed" || backend.query.Limit != 6 {
		t.Fatalf("query = %+v", backend.query)
	}
	if len(backend.query.Kinds) != 1 || backend.query.Kinds[0] != "decision" {
		t.Fatalf("kinds = %v", backend.query.Kinds)
	}
	if age := time.Since(backend.query.Since); age < 3*24*time.Hour-time.Minute || age > 3*24*time.Hour+time.Minute {
		t.Fatalf("since = %v ago, want about 3 days", age)
	}
	if text := textOf(t, got); !strings.Contains(text, "decision decision-alpha-1 [proposed] Use fsstore") {
		t.Fatalf("text = %q", text)
	}
	structured := got["structuredContent"].(map[string]any)
	if records := structured["records"].([]any); len(records) != 1 {
		t.Fatalf("structured records = %v", records)
	}

	if got = callTool(t, backend, "brain_record", `{"action":"list","since_days":-1}`); got["isError"] != true {
		t.Fatalf("negative since_days accepted: %+v", got)
	}
	if got = callTool(t, backend, "brain_record", `{"action":"list","status":"maybe"}`); got["isError"] != true {
		t.Fatalf("unknown status accepted: %+v", got)
	}
}

func TestMCPServerRecordGetAction(t *testing.T) {
	backend := &recordFake{}

	got := callTool(t, backend, "brain_record", `{"id":"decision-alpha-1","project":"alpha"}`)
	if got["isError"] == true {
		t.Fatalf("brain_record error: %+v", got)
	}
	text := textOf(t, got)
	if !strings.Contains(text, "context: Need persistence") || !strings.Contains(text, "status: proposed") {
		t.Fatalf("text = %q", text)
	}
	if got["structuredContent"].(map[string]any)["id"] != "decision-alpha-1" {
		t.Fatalf("structured = %+v", got["structuredContent"])
	}

	if got = callTool(t, backend, "brain_record", `{"id":"missing"}`); got["isError"] != true {
		t.Fatalf("missing record not reported: %+v", got)
	}
	if got = callTool(t, backend, "brain_record", `{}`); got["isError"] != true {
		t.Fatalf("record without id accepted: %+v", got)
	}
}

func TestMCPServerRecordLinksAction(t *testing.T) {
	got := callTool(t, &recordFake{}, "brain_record", `{"action":"links","id":"decision-alpha-1","project":"alpha"}`)
	if got["isError"] == true {
		t.Fatalf("brain_record action=links error: %+v", got)
	}
	obj := got["structuredContent"].(map[string]any)
	if len(obj["links"].([]any)) != 1 || obj["links"].([]any)[0].(map[string]any)["id"] != "note-alpha-1" {
		t.Fatalf("links = %v", obj["links"])
	}
	if text := textOf(t, got); !strings.Contains(text, "link: note-alpha-1") {
		t.Fatalf("text = %q", text)
	}
}

func TestMCPServerDecisionLifecycleMovedToJudge(t *testing.T) {
	backend := &recordFake{}

	got := callTool(t, backend, "brain_judge", `{"action":"accept","id":"decision-alpha-1","project":"alpha"}`)
	if got["isError"] == true {
		t.Fatalf("brain_judge accept error: %+v", got)
	}
	if len(backend.statusCalls) != 1 || backend.statusCalls[0] != "decision-alpha-1=accepted" {
		t.Fatalf("status calls = %v", backend.statusCalls)
	}
	if text := textOf(t, got); text != "decision decision-alpha-1: accepted" {
		t.Fatalf("text = %q", text)
	}

	got = callTool(t, backend, "brain_judge", `{"action":"supersede","id":"decision-alpha-1","to":"decision-alpha-2"}`)
	if got["isError"] == true {
		t.Fatalf("supersede error: %+v", got)
	}
	if len(backend.supersedes) != 1 || backend.supersedes[0] != "decision-alpha-1->decision-alpha-2" {
		t.Fatalf("supersede calls = %v", backend.supersedes)
	}
	if len(backend.statusCalls) != 1 {
		t.Fatalf("supersede must not set a status directly, status calls = %v", backend.statusCalls)
	}

	got = callTool(t, backend, "brain_decision", `{"title":"Adopt Go","status":"accepted"}`)
	if got["isError"] != true {
		t.Fatalf("brain_decision accepted status: %+v", got)
	}
	refusal := got["structuredContent"].(map[string]any)
	if refusal["code"] != RefusalInvalidArguments || !strings.Contains(refusal["try_instead"].(string), "brain_judge") {
		t.Fatalf("refusal = %v", refusal)
	}
}

func TestMCPServerRecordToolIsAlwaysListed(t *testing.T) {
	names := func(backend Backend) string {
		var list []string
		for _, tool := range New(backend, "test").toolList() {
			list = append(list, tool.Name)
		}
		return strings.Join(list, ",")
	}
	if !strings.Contains(names(&fakeBackend{}), "brain_record") {
		t.Fatal("brain_record is missing for a backend without record support")
	}
	listed := names(&recordFake{})
	if !strings.Contains(listed, "brain_record") {
		t.Fatalf("brain_record missing for a record backend: %s", listed)
	}
	if strings.Contains(listed, "trash") || strings.Contains(listed, "restore") {
		t.Fatal("destructive record operations must stay CLI-only")
	}
}
