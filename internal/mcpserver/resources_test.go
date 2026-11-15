package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// resourceFake gives the resource tests several projects, a configurable
// record body and a quarantine map.
type resourceFake struct {
	fullFake
	detail      RecordDetail
	missing     map[string]bool
	quarantined map[string]bool
	projects    []Project
}

func (f *resourceFake) Projects(context.Context) ([]Project, error) {
	return f.projects, nil
}

func (f *resourceFake) GetRecord(_ context.Context, projectID, id string) (RecordDetail, error) {
	if f.missing[id] {
		return RecordDetail{}, fmt.Errorf("record %q: %w", id, domain.ErrNotFound)
	}
	detail := f.detail
	detail.ID = id
	detail.ProjectID = projectID
	if detail.Kind == "" {
		detail.Kind = "note"
	}
	if detail.Title == "" {
		detail.Title = "Cache fix"
	}
	return detail, nil
}

func (f *resourceFake) RecordQuarantined(_ context.Context, _, id string) (bool, error) {
	return f.quarantined[id], nil
}

func newResourceFake() *resourceFake {
	return &resourceFake{
		detail:      RecordDetail{Kind: "note", ID: "note-alpha-1", Title: "Cache fix", Body: "the cache layer is fixed"},
		missing:     map[string]bool{"note-missing": true},
		quarantined: map[string]bool{},
		projects: []Project{
			{ID: "beta", Name: "Beta", Kind: "repository", Paths: []string{"C:/work/beta"}},
			{ID: "alpha", Name: "Alpha", Kind: "repository", Paths: []string{"C:/work/alpha"}},
		},
	}
}

func resourceResponse(t *testing.T, server *Server, method, params string) map[string]any {
	t.Helper()
	responses := serveSession(t, server,
		fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params))
	return responses[0]
}

func resourceRead(t *testing.T, server *Server, uri string) (map[string]any, map[string]any) {
	t.Helper()
	response := resourceResponse(t, server, "resources/read", fmt.Sprintf(`{"uri":%q}`, uri))
	if result, ok := response["result"].(map[string]any); ok {
		return result, nil
	}
	errObj, ok := response["error"].(map[string]any)
	if !ok {
		t.Fatalf("neither result nor error: %v", response)
	}
	refusal, ok := errObj["data"].(map[string]any)
	if !ok {
		t.Fatalf("error data is not a refusal: %v", errObj)
	}
	return nil, refusal
}

func TestResourcesListAdvertisesMemoryOnly(t *testing.T) {
	server := newProfileServer(t, newResourceFake(), ProfileLV24, false, Policy{})
	result := resultOf(t, resourceResponse(t, server, "resources/list", `{}`))
	resources := result["resources"].([]any)
	uris := map[string]bool{}
	for _, raw := range resources {
		uri := raw.(map[string]any)["uri"].(string)
		uris[uri] = true
		if strings.HasPrefix(uri, "code://") {
			t.Fatalf("code:// is advertised in 0.17: %s", uri)
		}
	}
	for _, want := range []string{
		ResourceProjectsURI, ResourceReviewURI,
		"memory://project/alpha/state", "memory://project/beta/state",
	} {
		if !uris[want] {
			t.Fatalf("resources/list misses %s: %v", want, uris)
		}
	}
	if result["ttlMs"] != float64(resourceProjectsTTLMs) || result["cacheScope"] != cacheScopePrivate {
		t.Fatalf("cache hints = %v/%v", result["ttlMs"], result["cacheScope"])
	}
	templates := resultOf(t, resourceResponse(t, server, "resources/templates/list", `{}`))["resourceTemplates"].([]any)
	templateSet := map[string]bool{}
	for _, raw := range templates {
		templateSet[raw.(map[string]any)["uriTemplate"].(string)] = true
	}
	if !templateSet[ResourceStateTemplate] || !templateSet[ResourceRecordTemplate] {
		t.Fatalf("templates = %v", templateSet)
	}
}

func TestResourcesRespectTheProjectAllowlist(t *testing.T) {
	server := newProfileServer(t, newResourceFake(), ProfileLV24, false, Policy{Projects: []string{"alpha"}})
	list := resultOf(t, resourceResponse(t, server, "resources/list", `{}`))
	for _, raw := range list["resources"].([]any) {
		uri := raw.(map[string]any)["uri"].(string)
		if strings.Contains(uri, "/beta/") {
			t.Fatalf("allowlisted server lists %s", uri)
		}
	}
	projects, refusal := resourceRead(t, server, ResourceProjectsURI)
	if refusal != nil {
		t.Fatalf("projects refusal = %v", refusal)
	}
	var payload struct {
		Count    int       `json:"count"`
		Projects []Project `json:"projects"`
	}
	text := projects["contents"].([]any)[0].(map[string]any)["text"].(string)
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("projects payload: %v", err)
	}
	if payload.Count != 1 || len(payload.Projects) != 1 || payload.Projects[0].ID != "alpha" {
		t.Fatalf("projects payload = %+v", payload)
	}
	if _, refusal := resourceRead(t, server, "memory://project/beta/state"); refusal == nil || refusal["code"] != RefusalNotFound {
		t.Fatalf("beta state refusal = %v", refusal)
	}
	if _, refusal := resourceRead(t, server, "memory://record/note-alpha-1?project=beta"); refusal == nil || refusal["code"] != RefusalNotFound {
		t.Fatalf("beta record refusal = %v", refusal)
	}
}

func TestResourcesReadStateAndRecord(t *testing.T) {
	backend := newResourceFake()
	backend.state = State{Goal: "ship 0.17", OpenQuestions: []string{"resources?"}}
	server := newProfileServer(t, backend, ProfileLV24, false, Policy{})

	state, refusal := resourceRead(t, server, "memory://project/alpha/state")
	if refusal != nil {
		t.Fatalf("state refusal = %v", refusal)
	}
	content := state["contents"].([]any)[0].(map[string]any)
	if content["mimeType"] != "text/markdown" || !strings.Contains(content["text"].(string), "# alpha state") {
		t.Fatalf("state content = %v", content)
	}
	if !strings.Contains(content["text"].(string), "ship 0.17") {
		t.Fatalf("state body misses the goal: %v", content["text"])
	}
	if state["ttlMs"] != float64(resourceStateTTLMs) || state["cacheScope"] != cacheScopePrivate {
		t.Fatalf("state cache hints = %v/%v", state["ttlMs"], state["cacheScope"])
	}

	record, refusal := resourceRead(t, server, "memory://record/note-alpha-1?project=alpha")
	if refusal != nil {
		t.Fatalf("record refusal = %v", refusal)
	}
	body := record["contents"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(body, `id: "note-alpha-1"`) || !strings.Contains(body, "the cache layer is fixed") {
		t.Fatalf("record body = %q", body)
	}

	if _, refusal := resourceRead(t, server, "memory://record/note-missing"); refusal == nil || refusal["code"] != RefusalNotFound {
		t.Fatalf("missing record = %v", refusal)
	}
	if _, refusal := resourceRead(t, server, "code://graph/alpha"); refusal == nil || refusal["code"] != RefusalUnsupported {
		t.Fatalf("code:// = %v", refusal)
	}
	if _, refusal := resourceRead(t, server, "memory://bogus"); refusal == nil || refusal["code"] != RefusalNotFound {
		t.Fatalf("unknown resource = %v", refusal)
	}
	if _, refusal := resourceRead(t, server, "memory://project/alpha/handoff"); refusal == nil || refusal["code"] != RefusalNotFound {
		t.Fatalf("unserved leaf = %v", refusal)
	}
}

func TestResourcesExcludeQuarantinedRecordsUnlessAsked(t *testing.T) {
	backend := newResourceFake()
	backend.quarantined["note-alpha-1"] = true
	server := newProfileServer(t, backend, ProfileLV24, false, Policy{})
	if _, refusal := resourceRead(t, server, "memory://record/note-alpha-1"); refusal == nil {
		t.Fatal("a quarantined record was served by default")
	} else if refusal["code"] != RefusalNotFound || !strings.Contains(refusal["message"].(string), "quarantined") {
		t.Fatalf("quarantine refusal = %v", refusal)
	}
	if _, refusal := resourceRead(t, server, "memory://record/note-alpha-1?include_unreviewed=true"); refusal != nil {
		t.Fatalf("explicit include_unreviewed refused: %v", refusal)
	}
	if _, refusal := resourceRead(t, server, "memory://record/note-alpha-1?include_unreviewed=maybe"); refusal == nil ||
		refusal["code"] != RefusalInvalidArguments {
		t.Fatalf("malformed include_unreviewed = %v", refusal)
	}
}

func TestResourcesReadReview(t *testing.T) {
	backend := newResourceFake()
	backend.review = Review{Summary: "one finding", Findings: []ReviewFinding{{
		Kind: "state_expired", Severity: "warn", Message: "state is stale", Why: "the goal may be old",
		Next: "brain state set", Confirm: "read it first", Evidence: "state.md", Fixable: false,
	}}}
	server := newProfileServer(t, backend, ProfileLV24, false, Policy{})

	single, refusal := resourceRead(t, server, "memory://review?project=alpha")
	if refusal != nil {
		t.Fatalf("review refusal = %v", refusal)
	}
	text := single["contents"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"project_id":"alpha"`) || !strings.Contains(text, `"findings"`) {
		t.Fatalf("review payload = %s", text)
	}
	aggregate, refusal := resourceRead(t, server, ResourceReviewURI)
	if refusal != nil {
		t.Fatalf("aggregate review refusal = %v", refusal)
	}
	aggregateText := aggregate["contents"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(aggregateText, `"count":2`) {
		t.Fatalf("aggregate review = %s", aggregateText)
	}
	if _, refusal := resourceRead(t, server, "memory://review?project=ghost"); refusal == nil || refusal["code"] != RefusalNotFound {
		t.Logf("response: %v", resourceResponse(t, server, "resources/read", `{"uri":"memory://review?project=ghost"}`))
		t.Fatalf("unknown review project = %v", refusal)
	}
}

func TestResourcesTruncateAboveTheCap(t *testing.T) {
	backend := newResourceFake()
	backend.state = State{Goal: strings.Repeat("x", resourceStateMaxBytes+1024)}
	server := newProfileServer(t, backend, ProfileLV24, false, Policy{})
	state, refusal := resourceRead(t, server, "memory://project/alpha/state")
	if refusal != nil {
		t.Fatalf("state refusal = %v", refusal)
	}
	content := state["contents"].([]any)[0].(map[string]any)
	text := content["text"].(string)
	if !strings.HasPrefix(text, "[truncated:") {
		t.Fatalf("truncated body does not start with the preamble: %q", text[:80])
	}
	meta := state["_meta"].(map[string]any)["lv24.brain/resource"].(map[string]any)
	if meta["truncated"] != true || meta["cap"] != float64(resourceStateMaxBytes) {
		t.Fatalf("resource meta = %v", meta)
	}
	if meta["bytes"].(float64) <= float64(resourceStateMaxBytes) {
		t.Fatalf("resource meta bytes = %v", meta["bytes"])
	}
}

func TestResourcesRejectEmptyURI(t *testing.T) {
	server := newProfileServer(t, newResourceFake(), ProfileLV24, false, Policy{})
	response := resourceResponse(t, server, "resources/read", `{}`)
	errObj, ok := response["error"].(map[string]any)
	if !ok || errObj["code"] != float64(codeInvalidParams) {
		t.Fatalf("empty uri response = %v", response)
	}
}
