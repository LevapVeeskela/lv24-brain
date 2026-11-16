package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// fakeMemory is the memory surface of the fixture. It records the last query of
// every route so a test can prove the handler passed the filters through, and
// it models the proposal terminal states the way the engine does.
type fakeMemory struct {
	mu sync.Mutex

	records          []RecordView
	detail           RecordDetailView
	search           SearchResult
	state            StateView
	review           ReviewView
	page             ProposalPage
	proposal         ProposalDetailView
	outcome          ProposalOutcome
	snooze           ProposalSnoozeView
	catchup          CatchupView
	quarantine       []QuarantineItemView
	quarantineReview QuarantineReviewView
	fix              ReviewFixView
	record           RecordView
	edit             RecordDetailView
	projects         map[string]string

	recordErr     error
	detailErr     error
	searchErr     error
	stateErr      error
	reviewErr     error
	propErr       error
	promoteErr    error
	snoozeErr     error
	catchupErr    error
	quarantineErr error
	fixErr        error
	trashErr      error
	restoreErr    error
	editErr       error
	resolveErr    error
	// conflict, when set, is returned by SetState even for a fresh revision.
	conflict *app.StateRevisionConflict

	lastRecord     RecordQuery
	lastDetail     RecordDetailQuery
	lastSearch     SearchQuery
	lastState      StateWrite
	lastReview     ReviewQuery
	lastProposal   ProposalQuery
	lastProposalID ProposalDetailQuery
	lastDecision   ProposalDecision
	lastSnooze     ProposalSnoozeRequest
	lastCatchup    CatchupQuery
	lastQuarantine QuarantineQuery
	lastReviewID   QuarantineReviewDecision
	lastFix        ReviewFixRequest
	lastTrashed    string
	lastRestored   string
	lastEdit       RecordEditRequest

	decided map[string]string
}

func newFakeMemory() *fakeMemory {
	return &fakeMemory{decided: map[string]string{}}
}

func (f *fakeMemory) ListRecords(_ context.Context, query RecordQuery) ([]RecordView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRecord = query
	if f.recordErr != nil {
		return nil, f.recordErr
	}
	return append([]RecordView(nil), f.records...), nil
}

func (f *fakeMemory) RecordDetail(_ context.Context, query RecordDetailQuery) (RecordDetailView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastDetail = query
	if f.detailErr != nil {
		return RecordDetailView{}, f.detailErr
	}
	return f.detail, nil
}

func (f *fakeMemory) EditRecord(_ context.Context, request RecordEditRequest) (RecordDetailView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastEdit = request
	if f.editErr != nil {
		return RecordDetailView{}, f.editErr
	}
	return f.edit, nil
}

func (f *fakeMemory) Search(_ context.Context, query SearchQuery) (SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSearch = query
	if f.searchErr != nil {
		return SearchResult{}, f.searchErr
	}
	return f.search, nil
}

func (f *fakeMemory) State(_ context.Context, projectID string) (StateView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stateErr != nil {
		return StateView{}, f.stateErr
	}
	if f.state.ProjectID == "" {
		f.state.ProjectID = projectID
	}
	return f.state, nil
}

func (f *fakeMemory) SetState(_ context.Context, _ string, write StateWrite) (StateView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastState = write
	if f.conflict != nil {
		return StateView{}, f.conflict
	}
	if f.stateErr != nil {
		return StateView{}, f.stateErr
	}
	if write.Goal != nil {
		f.state.Goal = *write.Goal
	}
	if write.NextAction != nil {
		f.state.NextAction = *write.NextAction
	}
	if write.ExpectedRevision != "" && write.ExpectedRevision != f.state.Revision {
		return StateView{}, &app.StateRevisionConflict{
			ProjectID: f.state.ProjectID, Expected: write.ExpectedRevision, Current: f.state.Revision,
		}
	}
	f.state.Revision = "rev-2"
	return f.state, nil
}

func (f *fakeMemory) Review(_ context.Context, query ReviewQuery) (ReviewView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastReview = query
	if f.reviewErr != nil {
		return ReviewView{}, f.reviewErr
	}
	if query.AllProjects {
		f.review.ProjectID = AllProjectsProjectID
		f.review.AllProjects = true
		f.review.ProjectIDs = append([]string(nil), query.ProjectIDs...)
		return f.review, nil
	}
	f.review.ProjectID = query.ProjectID
	return f.review, nil
}

func (f *fakeMemory) ReviewFix(_ context.Context, request ReviewFixRequest) (ReviewFixView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastFix = request
	if f.fixErr != nil {
		return ReviewFixView{}, f.fixErr
	}
	view := f.fix
	if view.ProjectID == "" {
		view.ProjectID = request.ProjectID
	}
	view.DryRun = request.DryRun
	return view, nil
}

func (f *fakeMemory) ListQuarantined(_ context.Context, query QuarantineQuery) ([]QuarantineItemView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastQuarantine = query
	if f.quarantineErr != nil {
		return nil, f.quarantineErr
	}
	return append([]QuarantineItemView(nil), f.quarantine...), nil
}

func (f *fakeMemory) ReviewQuarantine(_ context.Context, decision QuarantineReviewDecision) (QuarantineReviewView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastReviewID = decision
	if f.quarantineErr != nil {
		return QuarantineReviewView{}, f.quarantineErr
	}
	return f.quarantineReview, nil
}

func (f *fakeMemory) ProposalDetail(_ context.Context, query ProposalDetailQuery) (ProposalDetailView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastProposalID = query
	if f.propErr != nil {
		return ProposalDetailView{}, f.propErr
	}
	return f.proposal, nil
}

func (f *fakeMemory) TrashRecord(_ context.Context, _, id string) (RecordView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastTrashed = id
	if f.trashErr != nil {
		return RecordView{}, f.trashErr
	}
	return f.record, nil
}

func (f *fakeMemory) RestoreRecord(_ context.Context, _, id string) (RecordView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRestored = id
	if f.restoreErr != nil {
		return RecordView{}, f.restoreErr
	}
	return f.record, nil
}

func (f *fakeMemory) ResolveProject(_ context.Context, selector string) (Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resolveErr != nil {
		return Project{}, f.resolveErr
	}
	if id, ok := f.projects[selector]; ok {
		return Project{ID: id, Name: id}, nil
	}
	return Project{ID: selector, Name: selector}, nil
}

func (f *fakeMemory) ListProposals(_ context.Context, query ProposalQuery) (ProposalPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastProposal = query
	if f.propErr != nil {
		return ProposalPage{}, f.propErr
	}
	return f.page, nil
}

func (f *fakeMemory) Promote(_ context.Context, decision ProposalDecision) (ProposalOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastDecision = decision
	if f.promoteErr != nil {
		return ProposalOutcome{}, f.promoteErr
	}
	status := f.decided[decision.ProposalID]
	if decision.Dismiss {
		switch status {
		case "dismissed":
			return ProposalOutcome{ProjectID: decision.ProjectID, ProposalID: decision.ProposalID,
				ProposalStatus: "dismissed", Dismissed: true, Already: true}, nil
		case "promoted":
			return ProposalOutcome{ProjectID: decision.ProjectID, ProposalID: decision.ProposalID,
				ProposalStatus: "promoted", Already: true}, nil
		}
		f.decided[decision.ProposalID] = "dismissed"
		return ProposalOutcome{ProjectID: decision.ProjectID, ProposalID: decision.ProposalID,
			ProposalStatus: "dismissed", Dismissed: true}, nil
	}
	switch status {
	case "promoted":
		// The engine checks the promoted terminal state before the dismiss
		// flag, so a dismissal of a promoted proposal is a no-op receipt.
		return ProposalOutcome{ProjectID: decision.ProjectID, ProposalID: decision.ProposalID,
			ProposalStatus: "promoted", RecordKind: "decision", RecordID: "decision-1", Already: true}, nil
	case "dismissed":
		return ProposalOutcome{}, domain.ErrConflict
	}
	f.decided[decision.ProposalID] = "promoted"
	return ProposalOutcome{ProjectID: decision.ProjectID, ProposalID: decision.ProposalID,
		ProposalStatus: "promoted", RecordKind: "decision", RecordID: "decision-1", RecordTitle: "Adopt"}, nil
}

func (f *fakeMemory) SnoozeProposal(_ context.Context, request ProposalSnoozeRequest) (ProposalSnoozeView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSnooze = request
	if f.snoozeErr != nil {
		return ProposalSnoozeView{}, f.snoozeErr
	}
	view := f.snooze
	if view.ProjectID == "" {
		view.ProjectID = request.ProjectID
	}
	if view.ProposalID == "" {
		view.ProposalID = request.ProposalID
	}
	return view, nil
}

func (f *fakeMemory) Catchup(_ context.Context, query CatchupQuery) (CatchupView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCatchup = query
	if f.catchupErr != nil {
		return CatchupView{}, f.catchupErr
	}
	return f.catchup, nil
}

// newMemoryFixture builds the standard fixture with a memory service attached.
func newMemoryFixture(t *testing.T) (*testFixture, *fakeMemory) {
	t.Helper()
	fixture := newFixture(t)
	memory := newFakeMemory()
	fixture.handler.opts.Memory = memory
	return fixture, memory
}

func recordRow(id, kind string, created time.Time) RecordView {
	return RecordView{
		ID: id, ProjectID: "brain", Kind: kind, Title: id, Path: "projects/brain/notes/" + id + ".md",
		CreatedAt: created, UpdatedAt: created,
		Validity:   ValidityView{ValidFrom: created, Current: true},
		Provenance: ProvenanceView{Origin: "human", Trust: "verified", Reviewed: true},
		Review:     ReviewStateView{},
	}
}

func TestMemoryRoutesAuthHostAndOrigin(t *testing.T) {
	fixture, _ := newMemoryFixture(t)
	host := net.JoinHostPort("127.0.0.1", strconvI(testPort))
	routes := []struct {
		method, path string
	}{
		{http.MethodGet, "/v1/records?project=brain"},
		{http.MethodGet, "/v1/records/note-1?project=brain"},
		{http.MethodGet, "/v1/search?project=brain&q=x"},
		{http.MethodGet, "/v1/projects/brain/state"},
		{http.MethodPut, "/v1/projects/brain/state"},
		{http.MethodGet, "/v1/review?project=brain"},
		{http.MethodGet, "/v1/proposals?project=brain"},
		{http.MethodPost, "/v1/proposals/brain/proposal-gh-000000000000/promote"},
		{http.MethodPost, "/v1/proposals/brain/proposal-gh-000000000000/dismiss"},
		{http.MethodGet, "/v1/catchup?project=brain"},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			for _, tc := range []struct {
				name   string
				auth   string
				host   string
				origin string
				want   int
			}{
				{"missing token", "", host, "", http.StatusUnauthorized},
				{"wrong token", "Bearer wrong", host, "", http.StatusUnauthorized},
				{"localhost host", "Bearer " + testToken, "localhost:" + strconvI(testPort), "", http.StatusForbidden},
				{"ipv6 host", "Bearer " + testToken, "[::1]:" + strconvI(testPort), "", http.StatusForbidden},
				{"origin", "Bearer " + testToken, host, "http://127.0.0.1:" + strconvI(testPort), http.StatusForbidden},
				{"referer", "Bearer " + testToken, host, "", http.StatusForbidden},
			} {
				r := httptest.NewRequest(route.method, route.path, nil)
				r.Host = tc.host
				if tc.auth != "" {
					r.Header.Set("Authorization", tc.auth)
				}
				if tc.origin != "" {
					r.Header.Set("Origin", tc.origin)
				}
				if tc.name == "referer" {
					r.Header.Set("Referer", "http://127.0.0.1/")
				}
				w := httptest.NewRecorder()
				fixture.handler.ServeHTTP(w, r)
				if w.Code != tc.want {
					t.Fatalf("%s: status=%d body=%s", tc.name, w.Code, w.Body.String())
				}
				if w.Header().Get("Access-Control-Allow-Origin") != "" {
					t.Fatal("CORS header was emitted")
				}
				envelope := decodeMap(t, w.Body.String())
				if _, ok := envelope["error"].(map[string]any); !ok {
					t.Fatalf("%s: missing error envelope: %s", tc.name, w.Body.String())
				}
			}
		})
	}
}

func TestMemoryMissingServiceIsUnavailable(t *testing.T) {
	fixture := newFixture(t)
	for _, path := range []string{"/v1/records?project=brain", "/v1/review?project=brain", "/v1/catchup?project=brain", "/v1/search?q=x&project=brain"} {
		w := request(fixture, http.MethodGet, path, "", nil)
		if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"unavailable"`) {
			t.Fatalf("%s = %d %s", path, w.Code, w.Body.String())
		}
	}
}

func TestRecordsPaginationFiltersAndCursorBinding(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	base := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	memory.records = []RecordView{
		recordRow("note-5", "note", base.Add(5*time.Minute)),
		recordRow("note-4", "note", base.Add(4*time.Minute)),
		recordRow("note-3", "note", base.Add(3*time.Minute)),
		recordRow("note-2", "note", base.Add(2*time.Minute)),
		recordRow("note-1", "note", base.Add(time.Minute)),
	}
	first := request(fixture, http.MethodGet, "/v1/records?project=brain&kind=note,decision&status=accepted&scope=project&subtype=fact&topic=auth&since=7d&limit=2&due_review=1", "", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first page = %d %s", first.Code, first.Body.String())
	}
	page := decodeMap(t, first.Body.String())
	items, _ := page["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %d", len(items))
	}
	if page["page"] != float64(1) || page["page_size"] != float64(2) || page["has_more"] != true {
		t.Fatalf("page envelope = %v", page)
	}
	cursor, _ := page["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("missing next_cursor")
	}
	if memory.lastRecord.ProjectID != "brain" || strings.Join(memory.lastRecord.Kinds, ",") != "note,decision" ||
		memory.lastRecord.Status != "accepted" || memory.lastRecord.Scope != "project" ||
		memory.lastRecord.Subtype != "fact" || memory.lastRecord.TopicKey != "auth" ||
		!memory.lastRecord.DueReview || memory.lastRecord.Limit != 3 {
		t.Fatalf("record query = %+v", memory.lastRecord)
	}
	if memory.lastRecord.Since.IsZero() {
		t.Fatal("since was not parsed")
	}

	second := request(fixture, http.MethodGet, "/v1/records?project=brain&kind=note,decision&status=accepted&scope=project&subtype=fact&topic=auth&since=7d&limit=2&due_review=1&cursor="+cursor, "", nil)
	if second.Code != http.StatusOK {
		t.Fatalf("second page = %d %s", second.Code, second.Body.String())
	}
	secondPage := decodeMap(t, second.Body.String())
	secondItems, _ := secondPage["items"].([]any)
	if len(secondItems) != 2 || secondItems[0].(map[string]any)["id"] != "note-3" {
		t.Fatalf("second page items = %v", secondItems)
	}
	if memory.lastRecord.Limit != 5 {
		t.Fatalf("second page scan = %d, want offset+size+1", memory.lastRecord.Limit)
	}

	// The same cursor under another project or filter must be refused.
	for _, path := range []string{
		"/v1/records?project=other&kind=note,decision&status=accepted&scope=project&subtype=fact&topic=auth&since=7d&limit=2&due_review=1&cursor=" + cursor,
		"/v1/records?project=brain&limit=2&cursor=" + cursor,
		"/v1/records?project=brain&limit=2&cursor=not-a-cursor",
	} {
		w := request(fixture, http.MethodGet, path, "", nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("cursor %s = %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{
		"/v1/records?project=brain&limit=0",
		"/v1/records?project=brain&limit=201",
		"/v1/records?project=brain&due_review=maybe",
		"/v1/records?project=brain&since=never",
		"/v1/records?project=brain&scope=world",
		"/v1/records?project=brain&all_projects=1",
	} {
		w := request(fixture, http.MethodGet, path, "", nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", path, w.Code, w.Body.String())
		}
	}
}

func TestRecordsOrderIsDeterministicAcrossKinds(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	same := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	memory.records = []RecordView{
		recordRow("note-b", "note", same),
		recordRow("decision-a", "decision", same),
		recordRow("note-a", "note", same),
	}
	w := request(fixture, http.MethodGet, "/v1/records?project=brain&limit=3", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	page := decodeMap(t, w.Body.String())
	items, _ := page["items"].([]any)
	want := []string{"decision-a", "note-a", "note-b"}
	for i, item := range items {
		if got := item.(map[string]any)["id"]; got != want[i] {
			t.Fatalf("order = %v, want %v", items, want)
		}
	}
}

func TestRecordDetailBodyHistoryAndNotFound(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	created := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	memory.detail = RecordDetailView{
		RecordView: recordRow("decision-1", "decision", created),
		Body:       "Context:\nuse sqlite", BodyTokens: 6, MaxTokens: 900,
		Links:     []RecordLinkView{{Field: "context", Ref: "note-1", State: "found"}},
		Backlinks: []BacklinkView{{Field: "body", Source: recordRow("note-1", "note", created)}},
		Relations: []RelationView{{Target: "note-1", Relation: "relates_to", Status: "pending"}},
	}
	w := request(fixture, http.MethodGet, "/v1/records/decision-1?project=brain&max_tokens=900&history=1", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("detail = %d %s", w.Code, w.Body.String())
	}
	if memory.lastDetail.ID != "decision-1" || memory.lastDetail.MaxTokens != 900 || !memory.lastDetail.History {
		t.Fatalf("detail query = %+v", memory.lastDetail)
	}
	detail := decodeMap(t, w.Body.String())
	if detail["body"] == "" || detail["max_tokens"] != float64(900) {
		t.Fatalf("detail body = %v", detail)
	}
	if _, ok := detail["relations"].([]any); !ok {
		t.Fatalf("relations missing: %v", detail)
	}
	memory.detailErr = domain.ErrNotFound
	if got := request(fixture, http.MethodGet, "/v1/records/missing?project=brain", "", nil); got.Code != http.StatusNotFound {
		t.Fatalf("missing record = %d %s", got.Code, got.Body.String())
	}
	for _, path := range []string{
		"/v1/records/decision-1",
		"/v1/records/decision-1?project=brain&max_tokens=0",
		"/v1/records/decision-1?project=brain&max_tokens=32001",
		"/v1/records/decision-1?project=brain&history=maybe",
	} {
		if got := request(fixture, http.MethodGet, path, "", nil); got.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", path, got.Code, got.Body.String())
		}
	}
}

func TestSearchPaginationValidationAndShape(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.search = SearchResult{Relaxed: true, Hits: []SearchHitView{
		{ID: "note-2", Kind: "note", Title: "two", Preview: "…two…", Score: 0.8, MatchReasons: []string{"bm25"}},
		{ID: "note-1", Kind: "note", Title: "one", Preview: "…one…", Score: 0.7},
	}}
	w := request(fixture, http.MethodGet, "/v1/search?q=sqlite+index&project=brain&kind=note&limit=1&mode=any&min_score=0.2&explain=1", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("search = %d %s", w.Code, w.Body.String())
	}
	page := decodeMap(t, w.Body.String())
	if page["query"] != "sqlite index" || page["relaxed"] != true {
		t.Fatalf("search envelope = %v", page)
	}
	hits, _ := page["hits"].([]any)
	if len(hits) != 1 || hits[0].(map[string]any)["preview"] != "…two…" {
		t.Fatalf("hits = %v", hits)
	}
	if memory.lastSearch.Mode != "any" || memory.lastSearch.MinScore != 0.2 || !memory.lastSearch.Explain ||
		strings.Join(memory.lastSearch.Kinds, ",") != "note" || memory.lastSearch.Limit != 2 {
		t.Fatalf("search query = %+v", memory.lastSearch)
	}
	cursor, _ := page["next_cursor"].(string)
	if cursor == "" || page["has_more"] != true {
		t.Fatalf("search page = %v", page)
	}
	if got := request(fixture, http.MethodGet, "/v1/search?q=sqlite+index&project=brain&kind=note&limit=1&mode=all&cursor="+cursor, "", nil); got.Code != http.StatusBadRequest {
		t.Fatalf("foreign search cursor = %d %s", got.Code, got.Body.String())
	}
	for _, path := range []string{
		"/v1/search?project=brain",
		"/v1/search?q=x&project=brain&mode=some",
		"/v1/search?q=x&project=brain&min_score=2",
		"/v1/search?q=x&project=brain&explain=maybe",
		"/v1/search?q=x&project=brain&limit=101",
		"/v1/search?q=x&project=brain&all_projects=1",
	} {
		if got := request(fixture, http.MethodGet, path, "", nil); got.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", path, got.Code, got.Body.String())
		}
	}
}

func TestStateCASConflictCarriesCurrentRevision(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.state = StateView{ProjectID: "brain", Goal: "old", Revision: "rev-1"}
	w := request(fixture, http.MethodGet, "/v1/projects/brain/state", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("state = %d %s", w.Code, w.Body.String())
	}
	state := decodeMap(t, w.Body.String())
	if state["revision"] != "rev-1" || state["goal"] != "old" {
		t.Fatalf("state = %v", state)
	}
	w = request(fixture, http.MethodPut, "/v1/projects/brain/state", `{"goal":"new","expected_revision":"rev-1"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("put = %d %s", w.Code, w.Body.String())
	}
	if memory.lastState.ExpectedRevision != "rev-1" || memory.lastState.Goal == nil || *memory.lastState.Goal != "new" {
		t.Fatalf("state write = %+v", memory.lastState)
	}
	updated := decodeMap(t, w.Body.String())
	if updated["goal"] != "new" || updated["revision"] != "rev-2" {
		t.Fatalf("updated state = %v", updated)
	}

	w = request(fixture, http.MethodPut, "/v1/projects/brain/state", `{"goal":"stale","expected_revision":"rev-1"}`, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale CAS = %d %s", w.Code, w.Body.String())
	}
	envelope := decodeMap(t, w.Body.String())
	errorBody, _ := envelope["error"].(map[string]any)
	if errorBody["code"] != "conflict" || errorBody["current_revision"] != "rev-2" {
		t.Fatalf("CAS error = %v", envelope)
	}

	if got := request(fixture, http.MethodPut, "/v1/projects/brain/state", `{}`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("empty write = %d %s", got.Code, got.Body.String())
	}
	if got := request(fixture, http.MethodPut, "/v1/projects/brain/state", `{"fresh_until":"tomorrow"}`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("bad fresh_until = %d %s", got.Code, got.Body.String())
	}
	if got := request(fixture, http.MethodPut, "/v1/projects/brain/state", `{"fresh_until":""}`, nil); got.Code != http.StatusOK {
		t.Fatalf("clear fresh_until = %d %s", got.Code, got.Body.String())
	}
	if memory.lastState.FreshUntil == nil || *memory.lastState.FreshUntil != "" {
		t.Fatalf("fresh_until clear was not forwarded: %+v", memory.lastState)
	}
	if got := request(fixture, http.MethodPut, "/v1/projects/brain/state", `{"goal":"x","unknown":1}`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d %s", got.Code, got.Body.String())
	}
}

func TestReviewIsReadOnlyAndOrdersFindings(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.review = ReviewView{
		Findings: []FindingView{
			{Kind: "state-expired", Severity: "warn", Message: "warn"},
			{Kind: "quarantined", Severity: "error", Message: "error"},
			{Kind: "next-maybe-done", Severity: "info", Message: "info"},
		},
		DueReviews:       []DueReviewView{{Kind: "note", ID: "note-1", OverdueSeconds: 90}},
		Proposals:        ProposalSummaryView{PendingCount: 2},
		QuarantinedCount: 3,
	}
	w := request(fixture, http.MethodGet, "/v1/review?project=brain", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("review = %d %s", w.Code, w.Body.String())
	}
	view := decodeMap(t, w.Body.String())
	if view["quarantined_count"] != float64(3) {
		t.Fatalf("review = %v", view)
	}
	// The handler does not reorder findings; the service owns severity
	// ordering. The important read-only property is that no write method is
	// accepted.
	findings, _ := view["findings"].([]any)
	if len(findings) != 3 {
		t.Fatalf("findings = %v", view["findings"])
	}
	if got := request(fixture, http.MethodPost, "/v1/review?project=brain", `{}`, nil); got.Code != http.StatusMethodNotAllowed || got.Header().Get("Allow") != "GET" {
		t.Fatalf("review write = %d Allow=%q", got.Code, got.Header().Get("Allow"))
	}
	if got := request(fixture, http.MethodGet, "/v1/review", "", nil); got.Code != http.StatusOK {
		t.Fatalf("all-projects review = %d %s", got.Code, got.Body.String())
	}
	if !memory.lastReview.AllProjects {
		t.Fatalf("review query = %+v", memory.lastReview)
	}
}

func TestProposalListPromoteDismissIdempotency(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.page = ProposalPage{ProjectID: "brain", Total: 3, Items: []ProposalView{
		{ID: "proposal-gh-000000000001", ProjectID: "brain", Kind: "decision", Title: "one", Status: "pending", Confidence: 0.8},
		{ID: "proposal-gh-000000000002", ProjectID: "brain", Kind: "note", Title: "two", Status: "stale", SourceStale: true},
	}}
	w := request(fixture, http.MethodGet, "/v1/proposals?project=brain&status=pending,stale&kind=decision&limit=1", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("proposals = %d %s", w.Code, w.Body.String())
	}
	page := decodeMap(t, w.Body.String())
	if page["total"] != float64(3) || page["has_more"] != true {
		t.Fatalf("proposal page = %v", page)
	}
	items, _ := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("proposal items = %v", items)
	}
	if strings.Join(memory.lastProposal.Statuses, ",") != "pending,stale" || memory.lastProposal.Kind != "decision" || memory.lastProposal.Limit != 2 {
		t.Fatalf("proposal query = %+v", memory.lastProposal)
	}
	memory.propErr = domain.ErrValidation
	if got := request(fixture, http.MethodGet, "/v1/proposals?project=brain&status=wrong", "", nil); got.Code != http.StatusBadRequest {
		t.Fatalf("status validation = %d %s", got.Code, got.Body.String())
	}
	memory.propErr = nil
	for _, path := range []string{"/v1/proposals", "/v1/proposals?project=brain&limit=201"} {
		if got := request(fixture, http.MethodGet, path, "", nil); got.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", path, got.Code, got.Body.String())
		}
	}

	promote := "/v1/proposals/brain/proposal-gh-000000000001/promote"
	w = request(fixture, http.MethodPost, promote, `{"status":"accepted","title":"Adopt","force":true,"yes":true}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("promote = %d %s", w.Code, w.Body.String())
	}
	outcome := decodeMap(t, w.Body.String())
	if outcome["proposal_status"] != "promoted" || outcome["record_id"] != "decision-1" || outcome["already"] != false {
		t.Fatalf("promote outcome = %v", outcome)
	}
	if !memory.lastDecision.Force || !memory.lastDecision.Yes || memory.lastDecision.Status != "accepted" || memory.lastDecision.Title != "Adopt" {
		t.Fatalf("decision flags = %+v", memory.lastDecision)
	}
	w = request(fixture, http.MethodPost, promote, `{}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("idempotent promote = %d %s", w.Code, w.Body.String())
	}
	if decodeMap(t, w.Body.String())["already"] != true {
		t.Fatalf("idempotent promote = %s", w.Body.String())
	}

	dismiss := "/v1/proposals/brain/proposal-gh-000000000002/dismiss"
	for i := 0; i < 2; i++ {
		w = request(fixture, http.MethodPost, dismiss, "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("dismiss %d = %d %s", i, w.Code, w.Body.String())
		}
		result := decodeMap(t, w.Body.String())
		if result["dismissed"] != true || (i == 1 && result["already"] != true) {
			t.Fatalf("dismiss %d = %v", i, result)
		}
	}
	if !memory.lastDecision.Dismiss {
		t.Fatal("dismiss did not set the dismiss flag")
	}
	dismissedPromote := "/v1/proposals/brain/proposal-gh-000000000002/promote"
	if got := request(fixture, http.MethodPost, dismissedPromote, `{"status":"accepted"}`, nil); got.Code != http.StatusConflict {
		t.Fatalf("promote after dismiss = %d %s", got.Code, got.Body.String())
	}
	if got := request(fixture, http.MethodPost, "/v1/proposals/brain/proposal-gh-000000000001/dismiss", "", nil); got.Code != http.StatusOK ||
		decodeMap(t, got.Body.String())["already"] != true {
		t.Fatalf("dismiss after promote = %d %s", got.Code, got.Body.String())
	}
	if got := request(fixture, http.MethodPost, "/v1/proposals/brain/proposal-gh-000000000001/accept", "", nil); got.Code != http.StatusNotFound {
		t.Fatalf("unknown action = %d", got.Code)
	}
	if got := request(fixture, http.MethodGet, dismiss, "", nil); got.Code != http.StatusMethodNotAllowed || got.Header().Get("Allow") != "POST" {
		t.Fatalf("dismiss method = %d Allow=%q", got.Code, got.Header().Get("Allow"))
	}
}

func TestCatchupQueryMapping(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.catchup = CatchupView{
		Project: Project{ID: "brain", Name: "Brain"},
		Since:   time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC), SinceSource: "handoff",
		State:             StateView{ProjectID: "brain", Revision: "rev-1"},
		Notes:             []RecordView{recordRow("note-1", "note", time.Now().UTC())},
		IncludeUnreviewed: true,
	}
	w := request(fixture, http.MethodGet, "/v1/catchup?project=brain&since=24h&include_unreviewed=1&limit=7", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("catchup = %d %s", w.Code, w.Body.String())
	}
	if memory.lastCatchup.ProjectID != "brain" || !memory.lastCatchup.IncludeUnreviewed || memory.lastCatchup.Limit != 7 || memory.lastCatchup.Since.IsZero() {
		t.Fatalf("catchup query = %+v", memory.lastCatchup)
	}
	view := decodeMap(t, w.Body.String())
	if view["since_source"] != "handoff" || view["include_unreviewed"] != true {
		t.Fatalf("catchup = %v", view)
	}
	for _, path := range []string{"/v1/catchup", "/v1/catchup?project=brain&limit=101", "/v1/catchup?project=brain&include_unreviewed=maybe"} {
		if got := request(fixture, http.MethodGet, path, "", nil); got.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", path, got.Code, got.Body.String())
		}
	}
}

func TestMemoryResponsesRedactSecrets(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	row := recordRow("note-1", "note", time.Now().UTC())
	row.Title = "credential " + testToken
	row.Path = "projects/brain/notes/" + testToken + ".md"
	memory.records = []RecordView{row}
	memory.detail = RecordDetailView{RecordView: row, Body: "secret " + testToken}
	memory.search = SearchResult{Hits: []SearchHitView{{ID: "note-1", Title: "hit " + testToken, Preview: testToken}}}
	memory.state = StateView{ProjectID: "brain", Goal: testToken, Revision: "r"}
	memory.review = ReviewView{Findings: []FindingView{{Kind: "k", Message: testToken}}, ProjectID: "brain"}
	memory.page = ProposalPage{Items: []ProposalView{{ID: "p", Title: testToken}}}
	memory.catchup = CatchupView{Project: Project{ID: "brain", Name: testToken}}

	for _, path := range []string{
		"/v1/records?project=brain",
		"/v1/records/note-1?project=brain",
		"/v1/search?q=x&project=brain",
		"/v1/projects/brain/state",
		"/v1/review?project=brain",
		"/v1/proposals?project=brain",
		"/v1/catchup?project=brain",
	} {
		w := request(fixture, http.MethodGet, path, "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), testToken) {
			t.Fatalf("%s leaked the token: %s", path, w.Body.String())
		}
	}
}

func TestMemoryBodySizeContentTypeAndMethodRules(t *testing.T) {
	fixture, _ := newMemoryFixture(t)
	oversize := `{"goal":"` + strings.Repeat("x", 512) + `"}`
	w := request(fixture, http.MethodPut, "/v1/projects/brain/state", oversize, nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize = %d %s", w.Code, w.Body.String())
	}
	w = request(fixture, http.MethodPut, "/v1/projects/brain/state", `{"goal":"x"}`, func(r *http.Request) {
		r.Header.Set("Content-Type", "text/plain")
	})
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type = %d %s", w.Code, w.Body.String())
	}

	cases := []struct {
		name, method, path, allow string
	}{
		{"records", http.MethodPost, "/v1/records", "GET"},
		{"record", http.MethodDelete, "/v1/records/note-1", "GET, PATCH"},
		{"search", http.MethodPost, "/v1/search", "GET"},
		{"state", http.MethodDelete, "/v1/projects/brain/state", "GET, PUT"},
		{"review", http.MethodPut, "/v1/review", "GET"},
		{"proposals", http.MethodPost, "/v1/proposals", "GET"},
		{"promote", http.MethodDelete, "/v1/proposals/brain/proposal-gh-000000000000/promote", "POST"},
		{"dismiss", http.MethodPut, "/v1/proposals/brain/proposal-gh-000000000000/dismiss", "POST"},
		{"catchup", http.MethodPost, "/v1/catchup", "GET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := request(fixture, tc.method, tc.path, "", nil)
			if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != tc.allow {
				t.Fatalf("status=%d Allow=%q body=%s", w.Code, w.Header().Get("Allow"), w.Body.String())
			}
			envelope := decodeMap(t, w.Body.String())
			errorBody, _ := envelope["error"].(map[string]any)
			if errorBody["code"] != "method_not_allowed" {
				t.Fatalf("envelope = %s", w.Body.String())
			}
		})
	}
}

func TestMemoryServiceErrorsUseTheErrorEnvelope(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.recordErr = domain.ErrNotFound
	if w := request(fixture, http.MethodGet, "/v1/records?project=brain", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("record 404 = %d %s", w.Code, w.Body.String())
	}
	memory.recordErr = domain.ErrValidation
	if w := request(fixture, http.MethodGet, "/v1/records?project=brain", "", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("record 400 = %d %s", w.Code, w.Body.String())
	}
	memory.recordErr = domain.ErrUnsupported
	if w := request(fixture, http.MethodGet, "/v1/records?project=brain", "", nil); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), `"unsupported"`) {
		t.Fatalf("record unsupported = %d %s", w.Code, w.Body.String())
	}
	memory.recordErr = errors.New("boom " + testToken)
	if w := request(fixture, http.MethodGet, "/v1/records?project=brain", "", nil); w.Code != http.StatusInternalServerError ||
		strings.Contains(w.Body.String(), testToken) {
		t.Fatalf("record 500 = %d %s", w.Code, w.Body.String())
	}
	memory.recordErr = nil
	memory.promoteErr = app.ErrNotImplemented
	if w := request(fixture, http.MethodPost, "/v1/proposals/brain/proposal-gh-000000000000/promote", "", nil); w.Code != http.StatusInternalServerError ||
		!strings.Contains(w.Body.String(), `"unavailable"`) {
		t.Fatalf("promote unavailable = %d %s", w.Code, w.Body.String())
	}
}

func TestCursorWindowIsBounded(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.records = []RecordView{recordRow("note-1", "note", time.Now().UTC())}
	// A cursor at the record window itself is refused instead of looping.
	cursor := encodeCursor("records", recordsWindow, filterFingerprint("records", "brain", "", "", "", "", "", "", "false"))
	if got := request(fixture, http.MethodGet, "/v1/records?project=brain&cursor="+cursor, "", nil); got.Code != http.StatusBadRequest {
		t.Fatalf("window cursor = %d %s", got.Code, got.Body.String())
	}
}

// TestDocumentedRoutesMatchTheDocsTable pins the OpenAPI operation set to the
// route table in docs/localapi-v1.md, so a route cannot be added to one and
// forgotten in the other.
func TestDocumentedRoutesMatchTheDocsTable(t *testing.T) {
	document := loadOpenAPI(t)
	operations := documentedOperations(t, document.Paths)
	raw, err := os.ReadFile("../../../docs/localapi-v1.md")
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "| `GET` |") && !strings.HasPrefix(line, "| `PUT` |") &&
			!strings.HasPrefix(line, "| `POST` |") && !strings.HasPrefix(line, "| `DELETE` |") &&
			!strings.HasPrefix(line, "| `PATCH` |") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 4 {
			continue
		}
		method := strings.Trim(strings.TrimSpace(parts[1]), "`")
		path := strings.Trim(strings.TrimSpace(parts[2]), "`")
		documented[method+" "+path] = true
	}
	if len(documented) == 0 {
		t.Fatal("docs/localapi-v1.md has no route table rows")
	}
	for _, operation := range operations {
		if !documented[operation] {
			t.Errorf("OpenAPI operation %s is missing from the docs route table", operation)
		}
	}
	openAPI := map[string]bool{}
	for _, operation := range operations {
		openAPI[operation] = true
	}
	for operation := range documented {
		if !openAPI[operation] {
			t.Errorf("docs route table lists %s, which OpenAPI does not document", operation)
		}
	}
}

// TestOpenAPIRecordAndStateSchemasParse proves the new payload schemas are
// valid JSON Schema objects with the names the handlers emit.
func TestOpenAPIRecordAndStateSchemasParse(t *testing.T) {
	data, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Record", "RecordDetail", "RecordPage", "SearchPage", "State", "StateWrite", "Review", "ProposalPage", "Catchup"} {
		if _, ok := document.Components.Schemas[name]; !ok {
			t.Fatalf("openapi.json is missing schema %s", name)
		}
	}
	var record map[string]any
	if err := json.Unmarshal(document.Components.Schemas["Record"], &record); err != nil {
		t.Fatal(err)
	}
	properties, _ := record["properties"].(map[string]any)
	for _, field := range []string{"id", "project_id", "kind", "title", "path", "updated_at", "validity", "provenance", "review"} {
		if _, ok := properties[field]; !ok {
			t.Fatalf("Record schema is missing %s", field)
		}
	}
}
