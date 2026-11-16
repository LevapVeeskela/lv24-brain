package localapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// decodeJSON decodes a response body into dst.
func decodeJSON(body string, dst any) error { return json.Unmarshal([]byte(body), dst) }

func TestAllProjectsScopePoliciesAndCursorBinding(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	demo := recordRow("note-b", "note", now.Add(-time.Minute))
	demo.ProjectID = "demo"
	memory.records = []RecordView{recordRow("note-a", "note", now), demo}

	w := request(fixture, http.MethodGet, "/v1/records?limit=1", "", nil)
	if w.Code != http.StatusOK || !memory.lastRecord.AllProjects || memory.lastRecord.ProjectID != "" {
		t.Fatalf("all-projects records = %d %s %+v", w.Code, w.Body.String(), memory.lastRecord)
	}
	page := decodeMap(t, w.Body.String())
	items, _ := page["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["project_id"] == "" {
		t.Fatalf("page = %v", page)
	}
	cursor, _ := page["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("all-projects page has no cursor")
	}
	if got := request(fixture, http.MethodGet, "/v1/records?limit=1&cursor="+cursor, "", nil); got.Code != http.StatusOK {
		t.Fatalf("second page = %d %s", got.Code, got.Body.String())
	}
	if got := request(fixture, http.MethodGet, "/v1/records?project=brain&limit=1&cursor="+cursor, "", nil); got.Code != http.StatusBadRequest {
		t.Fatalf("cursor crossed the scope: %d %s", got.Code, got.Body.String())
	}

	fixture.handler.opts.ProjectAllowlist = []string{"brain"}
	if got := request(fixture, http.MethodGet, "/v1/records", "", nil); got.Code != http.StatusOK || memory.lastRecord.ProjectID != "brain" || memory.lastRecord.AllProjects {
		t.Fatalf("single-project policy = %d %+v", got.Code, memory.lastRecord)
	}
	if got := request(fixture, http.MethodGet, "/v1/records?project=demo", "", nil); got.Code != http.StatusBadRequest {
		t.Fatalf("outside allowlist = %d %s", got.Code, got.Body.String())
	}
	if got := request(fixture, http.MethodGet, "/v1/records?project=brain&all_projects=1", "", nil); got.Code != http.StatusBadRequest {
		t.Fatalf("project with all_projects = %d %s", got.Code, got.Body.String())
	}
	memory.projects = map[string]string{"brain-alias": "brain"}
	if got := request(fixture, http.MethodGet, "/v1/records?project=brain-alias", "", nil); got.Code != http.StatusOK || memory.lastRecord.ProjectID != "brain" {
		t.Fatalf("alias = %d %+v", got.Code, memory.lastRecord)
	}

	fixture.handler.opts.ProjectAllowlist = []string{"brain", "demo"}
	for _, path := range []string{"/v1/records", "/v1/records?all_projects=1", "/v1/search?q=x"} {
		if got := request(fixture, http.MethodGet, path, "", nil); got.Code != http.StatusBadRequest {
			t.Fatalf("%s under a multi-project allowlist = %d %s", path, got.Code, got.Body.String())
		}
	}
	if got := request(fixture, http.MethodGet, "/v1/records?project=demo", "", nil); got.Code != http.StatusOK {
		t.Fatalf("allowed project = %d %s", got.Code, got.Body.String())
	}
	fixture.handler.opts.ProjectAllowlist = nil
	if got := request(fixture, http.MethodGet, "/v1/records?all_projects=maybe", "", nil); got.Code != http.StatusBadRequest {
		t.Fatalf("bad all_projects = %d %s", got.Code, got.Body.String())
	}

	if got := request(fixture, http.MethodGet, "/v1/search?q=x", "", nil); got.Code != http.StatusOK || !memory.lastSearch.AllProjects {
		t.Fatalf("all-projects search = %d %+v", got.Code, memory.lastSearch)
	}
	if got := request(fixture, http.MethodGet, "/v1/review", "", nil); got.Code != http.StatusOK || !memory.lastReview.AllProjects {
		t.Fatalf("all-projects review = %d %+v", got.Code, memory.lastReview)
	}
	review := decodeMap(t, request(fixture, http.MethodGet, "/v1/review", "", nil).Body.String())
	if review["project_id"] != AllProjectsProjectID {
		t.Fatalf("all-projects review body = %v", review)
	}
}

func TestProposalBodiesDetailCapsAndRedaction(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.page = ProposalPage{ProjectID: "brain", Total: 2, Items: []ProposalView{
		{ID: "proposal-gh-000000000001", ProjectID: "brain", Kind: "decision", Title: "one", BodyOmitted: true},
		{ID: "proposal-gh-000000000002", ProjectID: "brain", Kind: "note", Title: "two", Body: "preview " + testToken, BodyTokens: 100, MaxTokens: 600},
	}}
	w := request(fixture, http.MethodGet, "/v1/proposals?project=brain&include_body=1&max_tokens=600", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("proposal bodies = %d %s", w.Code, w.Body.String())
	}
	if !memory.lastProposal.IncludeBody || memory.lastProposal.BodyTokens != 600 {
		t.Fatalf("proposal query = %+v", memory.lastProposal)
	}
	body := w.Body.String()
	if strings.Contains(body, testToken) {
		t.Fatalf("proposal body leaked a secret: %s", body)
	}
	page := decodeMap(t, body)
	items, _ := page["items"].([]any)
	first, second := items[0].(map[string]any), items[1].(map[string]any)
	if first["body_omitted"] != true || first["body"] != nil {
		t.Fatalf("omitted body = %v", first)
	}
	if second["body"] != "preview [redacted]" {
		t.Fatalf("bounded body = %v", second)
	}

	memory.proposal = ProposalDetailView{
		ID: "proposal-gh-000000000001", ProjectID: "brain", Kind: "decision",
		Title: "one", Status: "pending", Body: "context " + testToken, BodyTokens: 9, MaxTokens: 600,
	}
	w = request(fixture, http.MethodGet, "/v1/proposals/brain/proposal-gh-000000000001?max_tokens=600", "", nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), testToken) {
		t.Fatalf("proposal detail = %d %s", w.Code, w.Body.String())
	}
	if memory.lastProposalID.ID != "proposal-gh-000000000001" || memory.lastProposalID.MaxTokens != 600 {
		t.Fatalf("detail query = %+v", memory.lastProposalID)
	}

	for _, path := range []string{
		"/v1/proposals?project=brain&include_body=1&max_tokens=0",
		"/v1/proposals?project=brain&include_body=1&max_tokens=2001",
		"/v1/proposals/brain/proposal-gh-000000000001?max_tokens=32001",
	} {
		if got := request(fixture, http.MethodGet, path, "", nil); got.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", path, got.Code, got.Body.String())
		}
	}
	if got := request(fixture, http.MethodPost, "/v1/proposals/brain/proposal-gh-000000000001", `{}`, nil); got.Code != http.StatusMethodNotAllowed || got.Header().Get("Allow") != "GET" {
		t.Fatalf("proposal detail method = %d Allow=%q", got.Code, got.Header().Get("Allow"))
	}
	memory.propErr = domain.ErrNotFound
	if got := request(fixture, http.MethodGet, "/v1/proposals/brain/proposal-gh-000000000001", "", nil); got.Code != http.StatusNotFound {
		t.Fatalf("missing proposal = %d %s", got.Code, got.Body.String())
	}
}

func TestReviewFixDryRunApplyConfirmationAndRedaction(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.fix = ReviewFixView{
		Findings: []FindingView{{Kind: "next-behind-handoff", Severity: "warn", Message: "adopt handoff", Fixable: true}},
		Changes:  []ReviewFixChange{{Kind: "next-behind-handoff", Action: "set_next_action", Field: "next_action", Value: "ship " + testToken}},
	}

	w := request(fixture, http.MethodPost, "/v1/review/fix?project=brain", `{}`, nil)
	if w.Code != http.StatusOK || memory.lastFix.Apply {
		t.Fatalf("dry run = %d %s %+v", w.Code, w.Body.String(), memory.lastFix)
	}
	view := decodeMap(t, w.Body.String())
	if view["dry_run"] != true || strings.Contains(w.Body.String(), testToken) {
		t.Fatalf("dry-run view = %v", view)
	}
	if changes, _ := view["changes"].([]any); len(changes) != 1 {
		t.Fatalf("changes = %v", view["changes"])
	}

	w = request(fixture, http.MethodPost, "/v1/review/fix?project=brain", `{"apply":true}`, nil)
	if w.Code != http.StatusOK || !memory.lastFix.Apply {
		t.Fatalf("apply = %d %s %+v", w.Code, w.Body.String(), memory.lastFix)
	}
	if view := decodeMap(t, w.Body.String()); view["dry_run"] != false {
		t.Fatalf("applied view = %v", view)
	}
	if got := request(fixture, http.MethodPost, "/v1/review/fix?project=brain", "", nil); got.Code != http.StatusOK || memory.lastFix.Apply {
		t.Fatalf("bodyless dry run = %d %s %+v", got.Code, got.Body.String(), memory.lastFix)
	}

	for body, code := range map[string]string{
		`{"apply":true,"dry_run":true}`: "validation",
		`{"dry_run":false}`:             "confirmation_required",
	} {
		w := request(fixture, http.MethodPost, "/v1/review/fix?project=brain", body, nil)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), code) {
			t.Fatalf("%s = %d %s", body, w.Code, w.Body.String())
		}
	}
	kinds := `{"kinds":[` + strings.TrimSuffix(strings.Repeat(`"x",`, 33), ",") + `]}`
	if got := request(fixture, http.MethodPost, "/v1/review/fix?project=brain", kinds, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("too many kinds = %d %s", got.Code, got.Body.String())
	}
	memory.fixErr = domain.ErrValidation
	if got := request(fixture, http.MethodPost, "/v1/review/fix?project=brain", `{}`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("fix validation = %d %s", got.Code, got.Body.String())
	}
	memory.fixErr = domain.ErrNotFound
	if got := request(fixture, http.MethodPost, "/v1/review/fix?project=brain", `{}`, nil); got.Code != http.StatusNotFound {
		t.Fatalf("fix not found = %d %s", got.Code, got.Body.String())
	}
	memory.fixErr = nil
	if got := request(fixture, http.MethodGet, "/v1/review/fix?project=brain", "", nil); got.Code != http.StatusMethodNotAllowed || got.Header().Get("Allow") != "POST" {
		t.Fatalf("fix method = %d Allow=%q", got.Code, got.Header().Get("Allow"))
	}
	if got := request(fixture, http.MethodPost, "/v1/review/fix", `{}`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("fix without project = %d", got.Code)
	}
}

func TestQuarantineListPaginationReviewAndNoBodies(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.quarantine = []QuarantineItemView{
		{ID: "note-2", Kind: "note", ProjectID: "brain", Title: "newer", Reason: "unreviewed", AgeSeconds: 10},
		{ID: "note-1", Kind: "note", ProjectID: "brain", Title: "older", Reason: "unknown_origin", AgeSeconds: 20},
	}
	w := request(fixture, http.MethodGet, "/v1/quarantine?project=brain&limit=1", "", nil)
	if w.Code != http.StatusOK || memory.lastQuarantine.ProjectID != "brain" || memory.lastQuarantine.Limit != 2 {
		t.Fatalf("quarantine = %d %s %+v", w.Code, w.Body.String(), memory.lastQuarantine)
	}
	if strings.Contains(w.Body.String(), `"body"`) {
		t.Fatalf("quarantine leaked a body: %s", w.Body.String())
	}
	page := decodeMap(t, w.Body.String())
	if page["has_more"] != true || page["next_cursor"] == nil {
		t.Fatalf("quarantine page = %v", page)
	}
	cursor, _ := page["next_cursor"].(string)
	if got := request(fixture, http.MethodGet, "/v1/quarantine?project=brain&limit=1&cursor="+cursor, "", nil); got.Code != http.StatusOK {
		t.Fatalf("quarantine page 2 = %d %s", got.Code, got.Body.String())
	} else if second := decodeMap(t, got.Body.String()); len(second["items"].([]any)) != 1 {
		t.Fatalf("quarantine page 2 items = %v", second)
	}
	if got := request(fixture, http.MethodGet, "/v1/quarantine?project=demo&limit=1&cursor="+cursor, "", nil); got.Code != http.StatusBadRequest {
		t.Fatalf("quarantine cursor crossed projects = %d %s", got.Code, got.Body.String())
	}
	for _, path := range []string{
		"/v1/quarantine",
		"/v1/quarantine?project=brain&limit=0",
		"/v1/quarantine?project=brain&limit=201",
	} {
		if got := request(fixture, http.MethodGet, path, "", nil); got.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", path, got.Code, got.Body.String())
		}
	}

	memory.quarantineReview = QuarantineReviewView{
		Record:     recordRow("note-2", "note", time.Now()),
		Provenance: ProvenanceView{Origin: "agent", Trust: "stated", Reviewed: true},
	}
	w = request(fixture, http.MethodPost, "/v1/quarantine/note-2/review?project=brain", `{"trust":"stated"}`, nil)
	if w.Code != http.StatusOK || memory.lastReviewID.ID != "note-2" || memory.lastReviewID.Trust != "stated" {
		t.Fatalf("quarantine review = %d %s %+v", w.Code, w.Body.String(), memory.lastReviewID)
	}
	if got := request(fixture, http.MethodPost, "/v1/quarantine/note-2/review?project=brain", `{"trust":"stated","bogus":1}`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d %s", got.Code, got.Body.String())
	}
	memory.quarantineErr = domain.ErrNotFound
	if got := request(fixture, http.MethodPost, "/v1/quarantine/note-missing/review?project=brain", `{}`, nil); got.Code != http.StatusNotFound {
		t.Fatalf("missing quarantine = %d %s", got.Code, got.Body.String())
	}
	memory.quarantineErr = nil
	if got := request(fixture, http.MethodGet, "/v1/quarantine/note-2/review?project=brain", "", nil); got.Code != http.StatusMethodNotAllowed || got.Header().Get("Allow") != "POST" {
		t.Fatalf("review method = %d Allow=%q", got.Code, got.Header().Get("Allow"))
	}
	if got := request(fixture, http.MethodPost, "/v1/quarantine/note-2/review", `{}`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("review without project = %d", got.Code)
	}
}

func TestRecordTrashRestoreUnsupportedEditAndTypedErrors(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.record = recordRow("note-1", "note", time.Now())

	if got := request(fixture, http.MethodPost, "/v1/records/note-1/trash?project=brain", "", nil); got.Code != http.StatusOK || memory.lastTrashed != "note-1" {
		t.Fatalf("trash = %d %s %+v", got.Code, got.Body.String(), memory.lastTrashed)
	}
	if got := request(fixture, http.MethodPost, "/v1/records/note-1/restore?project=brain", "", nil); got.Code != http.StatusOK || memory.lastRestored != "note-1" {
		t.Fatalf("restore = %d %s %+v", got.Code, got.Body.String(), memory.lastRestored)
	}
	if got := request(fixture, http.MethodPost, "/v1/records/note-1/trash", "", nil); got.Code != http.StatusBadRequest {
		t.Fatalf("trash without project = %d %s", got.Code, got.Body.String())
	}

	memory.trashErr = domain.ErrNotFound
	if got := request(fixture, http.MethodPost, "/v1/records/note-1/trash?project=brain", "", nil); got.Code != http.StatusNotFound {
		t.Fatalf("trash missing = %d %s", got.Code, got.Body.String())
	}
	memory.trashErr = domain.ErrUnsupported
	w := request(fixture, http.MethodPost, "/v1/records/note-1/trash?project=brain", "", nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unsupported") {
		t.Fatalf("trash unsupported = %d %s", w.Code, w.Body.String())
	}
	memory.trashErr = nil
	memory.restoreErr = domain.ErrNotFound
	if got := request(fixture, http.MethodPost, "/v1/records/note-1/restore?project=brain", "", nil); got.Code != http.StatusNotFound {
		t.Fatalf("restore missing = %d %s", got.Code, got.Body.String())
	}
	memory.restoreErr = nil

	memory.edit = RecordDetailView{RecordView: RecordView{ID: "note-1", Kind: "note", Title: "rewrite"}}
	w = request(fixture, http.MethodPatch, "/v1/records/note-1?project=brain",
		`{"body":"rewrite","expected_updated_at":"2026-11-15T10:00:00Z"}`, nil)
	if w.Code != http.StatusOK || memory.lastEdit.Body == nil || *memory.lastEdit.Body != "rewrite" ||
		memory.lastEdit.ExpectedUpdatedAt.IsZero() {
		t.Fatalf("record edit = %d %s %+v", w.Code, w.Body.String(), memory.lastEdit)
	}
	if got := request(fixture, http.MethodPatch, "/v1/records/note-1?project=brain", `{}`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("empty record edit = %d %s", got.Code, got.Body.String())
	}
	if got := request(fixture, http.MethodPatch, "/v1/records/note-1", `{"body":"x"}`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("record edit without project = %d", got.Code)
	}
	if got := request(fixture, http.MethodDelete, "/v1/records/note-1?project=brain", "", nil); got.Code != http.StatusMethodNotAllowed || got.Header().Get("Allow") != "GET, PATCH" {
		t.Fatalf("record delete = %d Allow=%q", got.Code, got.Header().Get("Allow"))
	}
}

func TestRunViewsCarryDerivedProjectScopeAndOmitActor(t *testing.T) {
	fixture, _ := newMemoryFixture(t)
	if err := fixture.connections.Create(context.Background(), domain.Connection{
		ID: "demo-1", Provider: "demo", Projects: []string{"brain", "demo"},
	}); err != nil {
		t.Fatal(err)
	}
	fixture.runs.runs = []domain.ConnectorRun{
		{ID: "run-1", Source: "demo", ConnectionID: "demo-1", Status: domain.ConnectorSuccess, StartedAt: time.Now()},
		{ID: "run-2", Source: "demo", ConnectionID: "ghost", Status: domain.ConnectorSuccess, StartedAt: time.Now().Add(-time.Minute)},
	}
	w := request(fixture, http.MethodGet, "/v1/runs", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("runs = %d %s", w.Code, w.Body.String())
	}
	var runs []map[string]any
	if err := decodeJSON(w.Body.String(), &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %+v", runs)
	}
	first := runs[0]
	if first["connection_id"] != "demo-1" || first["connectionId"] != "demo-1" {
		t.Fatalf("connection ids = %+v", first)
	}
	projects, _ := first["project_ids"].([]any)
	if len(projects) != 2 || projects[0] != "brain" || projects[1] != "demo" {
		t.Fatalf("project ids = %+v", first)
	}
	if _, ok := first["actor"]; ok {
		t.Fatalf("actor was invented: %+v", first)
	}
	second := runs[1]
	if _, ok := second["project_ids"]; ok {
		t.Fatalf("unknown connection gained projects: %+v", second)
	}

	w = request(fixture, http.MethodGet, "/v1/runs/run-1", "", nil)
	one := decodeMap(t, w.Body.String())
	if w.Code != http.StatusOK || one["connection_id"] != "demo-1" {
		t.Fatalf("run detail = %d %v", w.Code, one)
	}
}

func TestNewMemoryRoutesEnforceAuthHostAndOrigin(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.record = recordRow("note-1", "note", time.Now())
	memory.quarantineReview = QuarantineReviewView{Record: recordRow("note-1", "note", time.Now())}
	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/review/fix?project=brain", `{}`},
		{http.MethodGet, "/v1/quarantine?project=brain", ""},
		{http.MethodPost, "/v1/quarantine/note-1/review?project=brain", `{}`},
		{http.MethodPatch, "/v1/records/note-1?project=brain", `{"body":"x"}`},
		{http.MethodPost, "/v1/records/note-1/trash?project=brain", ""},
		{http.MethodPost, "/v1/records/note-1/restore?project=brain", ""},
		{http.MethodGet, "/v1/proposals/brain/proposal-gh-000000000000", ""},
	}
	host := "127.0.0.1:" + strconvI(testPort)
	for _, route := range routes {
		name := route.method + " " + route.path
		if got := request(fixture, route.method, route.path, route.body, nil); got.Code == http.StatusUnauthorized || got.Code == http.StatusForbidden {
			t.Fatalf("%s rejected with valid auth: %d", name, got.Code)
		}
		if got := request(fixture, route.method, route.path, route.body, func(r *http.Request) {
			r.Header.Del("Authorization")
		}); got.Code != http.StatusUnauthorized || got.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("%s without auth = %d", name, got.Code)
		}
		if got := request(fixture, route.method, route.path, route.body, func(r *http.Request) {
			r.Host = "localhost:" + strconvI(testPort)
		}); got.Code != http.StatusForbidden {
			t.Fatalf("%s wrong host = %d", name, got.Code)
		}
		if got := request(fixture, route.method, route.path, route.body, func(r *http.Request) {
			r.Header.Set("Origin", "http://"+host)
		}); got.Code != http.StatusForbidden {
			t.Fatalf("%s origin = %d", name, got.Code)
		}
		if got := request(fixture, route.method, route.path, route.body, func(r *http.Request) {
			r.Header.Set("Referer", "http://"+host+"/")
		}); got.Code != http.StatusForbidden {
			t.Fatalf("%s referer = %d", name, got.Code)
		}
	}
}

func TestProjectAllowlistCoversEveryMemoryRoute(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	fixture.handler.opts.ProjectAllowlist = []string{"brain"}
	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/v1/records/note-1?project=demo", ""},
		{http.MethodPatch, "/v1/records/note-1?project=demo", `{"body":"x"}`},
		{http.MethodPost, "/v1/records/note-1/trash?project=demo", ""},
		{http.MethodPost, "/v1/records/note-1/restore?project=demo", ""},
		{http.MethodGet, "/v1/search?q=x&project=demo", ""},
		{http.MethodGet, "/v1/review?project=demo", ""},
		{http.MethodPost, "/v1/review/fix?project=demo", `{}`},
		{http.MethodGet, "/v1/quarantine?project=demo", ""},
		{http.MethodPost, "/v1/quarantine/note-1/review?project=demo", `{}`},
		{http.MethodGet, "/v1/proposals?project=demo", ""},
		{http.MethodGet, "/v1/proposals/demo/proposal-gh-000000000000", ""},
		{http.MethodPost, "/v1/proposals/demo/proposal-gh-000000000000/promote", `{}`},
		{http.MethodPost, "/v1/proposals/demo/proposal-gh-000000000000/dismiss", `{}`},
		{http.MethodGet, "/v1/catchup?project=demo", ""},
		{http.MethodGet, "/v1/projects/demo/state", ""},
		{http.MethodPut, "/v1/projects/demo/state", `{"goal":"x"}`},
	}
	for _, route := range routes {
		got := request(fixture, route.method, route.path, route.body, nil)
		if got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "allowed projects") {
			t.Fatalf("%s %s = %d %s", route.method, route.path, got.Code, got.Body.String())
		}
	}
	if memory.lastDetail.ProjectID != "" || memory.lastTrashed != "" || memory.lastFix.ProjectID != "" {
		t.Fatal("a refused project reached the memory service")
	}
}

func TestRecordDetailCarriesRelationTargetKind(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.detail = RecordDetailView{
		RecordView: recordRow("decision-1", "decision", time.Now()),
		Links:      []RecordLinkView{},
		Backlinks:  []BacklinkView{},
		Relations:  []RelationView{{Target: "note-1", TargetKind: "note", Relation: "relates_to", Status: "accepted"}},
	}
	w := request(fixture, http.MethodGet, "/v1/records/decision-1?project=brain", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"target_kind":"note"`) {
		t.Fatalf("target kind = %d %s", w.Code, w.Body.String())
	}
}
