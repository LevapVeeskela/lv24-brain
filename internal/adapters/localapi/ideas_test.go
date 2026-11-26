package localapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// fakeIdeas is the idea use-case double. It records the exact app request so a
// handler test proves the wire query maps onto the shared use case, and it
// implements the two listing predicates the response shape depends on
// (project and status).
type fakeIdeas struct {
	ideas         []domain.Idea
	problems      []ports.RecordProblem
	projectSource string
	listErr       error

	added      app.IdeaResult
	addErr     error
	got        domain.Idea
	getErr     error
	updated    app.IdeaUpdateResult
	updateErr  error
	trashed    domain.Idea
	trashErr   error
	restored   domain.Idea
	restoreErr error

	brief    app.IdeaBriefResult
	briefErr error

	lastList           app.IdeaListOptions
	lastAdd            app.IdeaInput
	lastGetID          string
	lastUpdate         app.IdeaUpdateInput
	lastTrashProject   string
	lastTrashID        string
	lastRestoreID      string
	lastRestoreProject string
	lastBrief          app.IdeaBriefOptions
}

func newFakeIdeas() *fakeIdeas {
	idea := testIdea("idea-contract-1", "Smoke idea")
	return &fakeIdeas{
		ideas:         []domain.Idea{idea},
		projectSource: app.IdeaProjectExplicit,
		added:         app.IdeaResult{Idea: idea, ProjectSource: app.IdeaProjectExplicit},
		got:           idea,
		updated:       app.IdeaUpdateResult{Idea: idea},
		trashed:       idea,
		restored:      idea,
		brief:         app.IdeaBriefResult{Ideas: []domain.Idea{}, Text: "## Ideas — 0 open"},
	}
}

func testIdea(id, title string) domain.Idea {
	due := time.Date(2026, 12, 1, 15, 0, 0, 0, time.UTC)
	remind := 30
	created := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	return domain.Idea{
		ID:                  id,
		ProjectID:           "brain",
		Title:               title,
		Body:                "body of " + title,
		Status:              domain.IdeaOpen,
		Priority:            domain.IdeaPriorityHigh,
		DueAt:               &due,
		RemindBeforeMinutes: &remind,
		Tags:                []string{"ui"},
		CreatedAt:           created,
		UpdatedAt:           created,
	}
}

func (f *fakeIdeas) ListIdeas(_ context.Context, opts app.IdeaListOptions) (app.IdeaListResult, error) {
	f.lastList = opts
	if f.listErr != nil {
		return app.IdeaListResult{}, f.listErr
	}
	statuses := make(map[domain.IdeaStatus]bool, len(opts.Statuses))
	for _, status := range opts.Statuses {
		statuses[status] = true
	}
	ideas := make([]domain.Idea, 0, len(f.ideas))
	for _, idea := range f.ideas {
		if opts.ProjectID != "" && idea.ProjectID != opts.ProjectID {
			continue
		}
		if len(statuses) > 0 && !statuses[idea.Status] {
			continue
		}
		ideas = append(ideas, idea)
	}
	return app.IdeaListResult{Ideas: ideas, Problems: f.problems, ProjectSource: f.projectSource}, nil
}

func (f *fakeIdeas) AddIdea(_ context.Context, input app.IdeaInput) (app.IdeaResult, error) {
	f.lastAdd = input
	return f.added, f.addErr
}

func (f *fakeIdeas) GetIdea(_ context.Context, id string) (domain.Idea, error) {
	f.lastGetID = id
	return f.got, f.getErr
}

func (f *fakeIdeas) UpdateIdea(_ context.Context, input app.IdeaUpdateInput) (app.IdeaUpdateResult, error) {
	f.lastUpdate = input
	return f.updated, f.updateErr
}

func (f *fakeIdeas) TrashIdea(_ context.Context, projectID, id string) (domain.Idea, error) {
	f.lastTrashProject, f.lastTrashID = projectID, id
	return f.trashed, f.trashErr
}

func (f *fakeIdeas) RestoreIdea(_ context.Context, projectID, id string) (domain.Idea, error) {
	f.lastRestoreProject, f.lastRestoreID = projectID, id
	return f.restored, f.restoreErr
}

func (f *fakeIdeas) IdeaBrief(_ context.Context, opts app.IdeaBriefOptions) (app.IdeaBriefResult, error) {
	f.lastBrief = opts
	return f.brief, f.briefErr
}

func decodeIdeaJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode response %q: %v", body, err)
	}
	return payload
}

func TestIdeasListParsesFiltersAndCountsBeforeLimit(t *testing.T) {
	fixture := newFixture(t)
	first := testIdea("idea-1", "First")
	second := testIdea("idea-2", "Second")
	third := testIdea("idea-3", "Third")
	second.Status = domain.IdeaDoing
	third.Status = domain.IdeaDoing
	fixture.ideas.ideas = []domain.Idea{first, second, third}

	response := request(fixture, http.MethodGet,
		"/v1/ideas?project=brain&status=open&status=doing&tag=UI&tag=backend"+
			"&due_before=2026-12-01T00:00:00Z&due_after=2026-11-01T00:00:00Z&q=theme&limit=2", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list = %d %s", response.Code, response.Body.String())
	}
	payload := decodeIdeaJSON(t, response.Body.Bytes())
	if payload["total"] != float64(3) {
		t.Fatalf("total = %v, want 3 (all matches before limit)", payload["total"])
	}
	if payload["project_source"] != "explicit" {
		t.Fatalf("project_source = %v", payload["project_source"])
	}
	ideas, _ := payload["ideas"].([]any)
	if len(ideas) != 2 {
		t.Fatalf("len(ideas) = %d, want the limit 2", len(ideas))
	}
	if _, present := ideas[0].(map[string]any)["body"]; present {
		t.Fatal("a list row must not carry body")
	}
	problems, ok := payload["problems"].([]any)
	if !ok || len(problems) != 0 {
		t.Fatalf("problems = %v, want []", payload["problems"])
	}

	got := fixture.ideas.lastList
	if got.ProjectID != "brain" || got.All {
		t.Fatalf("project scope = %+v", got)
	}
	if len(got.Statuses) != 2 || got.Statuses[0] != domain.IdeaOpen || got.Statuses[1] != domain.IdeaDoing {
		t.Fatalf("statuses = %v", got.Statuses)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "UI" || got.Tags[1] != "backend" {
		t.Fatalf("tags = %v", got.Tags)
	}
	if got.DueBefore.Format(time.RFC3339) != "2026-12-01T00:00:00Z" ||
		got.DueAfter.Format(time.RFC3339) != "2026-11-01T00:00:00Z" {
		t.Fatalf("due window = %v .. %v", got.DueAfter, got.DueBefore)
	}
	if got.Query != "theme" {
		t.Fatalf("query = %q", got.Query)
	}
	if got.Limit != 0 {
		t.Fatalf("store limit = %d, want 0 so total counts every match", got.Limit)
	}
}

func TestIdeasListAllOmitsProjectSourceAndAcceptsTagsAlias(t *testing.T) {
	fixture := newFixture(t)
	fixture.ideas.ideas = []domain.Idea{
		testIdea("idea-1", "One"),
		{ID: "idea-2", ProjectID: "personal", Title: "Two", Status: domain.IdeaOpen},
	}

	response := request(fixture, http.MethodGet, "/v1/ideas?tags=UI,backend", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list = %d %s", response.Code, response.Body.String())
	}
	payload := decodeIdeaJSON(t, response.Body.Bytes())
	if _, present := payload["project_source"]; present {
		t.Fatalf("an all-project listing must omit project_source: %v", payload["project_source"])
	}
	if payload["total"] != float64(2) {
		t.Fatalf("total = %v, want both projects", payload["total"])
	}
	if !fixture.ideas.lastList.All || fixture.ideas.lastList.ProjectID != "" {
		t.Fatalf("list options = %+v, want All", fixture.ideas.lastList)
	}
	if len(fixture.ideas.lastList.Tags) != 2 {
		t.Fatalf("tags = %v", fixture.ideas.lastList.Tags)
	}
}

func TestIdeasListProblemsAreReportedRelativeToTheDataDir(t *testing.T) {
	fixture := newFixture(t)
	fixture.ideas.problems = []ports.RecordProblem{{
		Path:   "C:/brain-data/projects/brain/ideas/idea-9.md",
		Kind:   ports.RecordIdea,
		Reason: "git conflict markers",
	}}
	response := request(fixture, http.MethodGet, "/v1/ideas?project=brain", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("list = %d %s", response.Code, response.Body.String())
	}
	payload := decodeIdeaJSON(t, response.Body.Bytes())
	problems, _ := payload["problems"].([]any)
	if len(problems) != 1 {
		t.Fatalf("problems = %v", payload["problems"])
	}
	problem, _ := problems[0].(map[string]any)
	if problem["path"] != "projects/brain/ideas/idea-9.md" || problem["kind"] != "idea" ||
		problem["reason"] != "git conflict markers" {
		t.Fatalf("problem row = %v", problem)
	}
}

func TestIdeasListRejectsInvalidFilters(t *testing.T) {
	fixture := newFixture(t)
	cases := map[string]string{
		"unknown status":     "/v1/ideas?status=urgent",
		"bad due_before":     "/v1/ideas?due_before=tomorrow",
		"bad due_after":      "/v1/ideas?due_after=2026-13-01",
		"limit zero":         "/v1/ideas?limit=0",
		"limit over maximum": "/v1/ideas?limit=1001",
		"limit not a number": "/v1/ideas?limit=all",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			response := request(fixture, http.MethodGet, path, "", nil)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
			}
			payload := decodeIdeaJSON(t, response.Body.Bytes())
			envelope, _ := payload["error"].(map[string]any)
			if envelope["code"] != "validation" {
				t.Fatalf("code = %v, want validation", envelope["code"])
			}
		})
	}
}

func TestIdeasCreateMapsTheRequestAndReturnsTheBody(t *testing.T) {
	fixture := newFixture(t)
	created := testIdea("idea-new", "Dark theme")
	created.Body = "Ship the dark calendar"
	fixture.ideas.added = app.IdeaResult{Idea: created, ProjectSource: app.IdeaProjectExplicit}

	response := request(fixture, http.MethodPost, "/v1/ideas", `{
		"title":"Dark theme","body":"Ship the dark calendar","project":"brain",
		"priority":"high","due_at":"2026-12-01T18:00:00+03:00","remind_before_minutes":30,
		"tags":["ui"],"source":{"type":"desktop"},"completion_note":"done via API"
	}`, nil)
	if response.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", response.Code, response.Body.String())
	}
	payload := decodeIdeaJSON(t, response.Body.Bytes())
	if payload["project_source"] != "explicit" {
		t.Fatalf("project_source = %v", payload["project_source"])
	}
	idea, _ := payload["idea"].(map[string]any)
	if idea["body"] != "Ship the dark calendar" || idea["path"] != "projects/brain/ideas/idea-new.md" {
		t.Fatalf("idea row = %v", idea)
	}

	got := fixture.ideas.lastAdd
	if got.Title != "Dark theme" || got.ProjectID != "brain" || got.Priority != domain.IdeaPriorityHigh {
		t.Fatalf("add input = %+v", got)
	}
	if got.DueAt == nil || got.DueAt.Format(time.RFC3339) != "2026-12-01T18:00:00+03:00" {
		t.Fatalf("due_at = %v, want the stored offset", got.DueAt)
	}
	if got.Source == nil || got.Source.Type != domain.IdeaSourceDesktop {
		t.Fatalf("source = %+v", got.Source)
	}
	if got.RemindBeforeMinutes == nil || *got.RemindBeforeMinutes != 30 {
		t.Fatalf("reminder = %v", got.RemindBeforeMinutes)
	}
	if got.CompletionNote != "done via API" {
		t.Fatalf("completion note = %q", got.CompletionNote)
	}
}

func TestIdeasCreateRejectsInvalidBodies(t *testing.T) {
	fixture := newFixture(t)
	cases := map[string]string{
		"missing title":       `{"body":"x"}`,
		"due_at and due_date": `{"title":"x","due_at":"2026-12-01T18:00:00Z","due_date":"2026-12-01"}`,
		"bad due_at":          `{"title":"x","due_at":"tomorrow"}`,
		"empty due_date":      `{"title":"x","due_date":""}`,
		"unknown field":       `{"title":"x","colour":"red"}`,
		"invalid json":        `{`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			response := request(fixture, http.MethodPost, "/v1/ideas", body, nil)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestIdeasGetProblemFileIsAConflictAndNotFoundIs404(t *testing.T) {
	fixture := newFixture(t)
	got := testIdea("idea-1", "One")
	fixture.ideas.got = got

	response := request(fixture, http.MethodGet, "/v1/ideas/idea-1", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("get = %d %s", response.Code, response.Body.String())
	}
	payload := decodeIdeaJSON(t, response.Body.Bytes())
	idea, _ := payload["idea"].(map[string]any)
	if idea["body"] != got.Body || idea["due_at"] != "2026-12-01T15:00:00Z" {
		t.Fatalf("idea = %v", idea)
	}
	if idea["due_date"] != nil || idea["remind_before_minutes"] != float64(30) {
		t.Fatalf("nullable fields = %v", idea)
	}
	provenance, _ := idea["provenance"].(map[string]any)
	if provenance["origin"] != "human" || provenance["reviewed"] != true {
		t.Fatalf("provenance = %v", provenance)
	}
	for _, key := range []string{"validity", "review", "relations"} {
		if _, ok := idea[key]; !ok {
			t.Fatalf("row is missing %s", key)
		}
	}

	fixture.ideas.getErr = domain.ErrNotFound
	if response := request(fixture, http.MethodGet, "/v1/ideas/idea-404", "", nil); response.Code != http.StatusNotFound {
		t.Fatalf("unknown id = %d %s", response.Code, response.Body.String())
	}

	fixture.ideas.getErr = &domain.ProblemError{
		Path:   "C:/brain-data/projects/brain/ideas/idea-2.md",
		Kind:   ports.RecordIdea,
		Reason: "git conflict markers",
	}
	response = request(fixture, http.MethodGet, "/v1/ideas/idea-2", "", nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("problem file = %d %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "projects/brain/ideas/idea-2.md") || !strings.Contains(body, "git conflict markers") {
		t.Fatalf("problem message lost path or reason: %s", body)
	}
	if strings.Contains(body, `"idea"`) {
		t.Fatalf("a problem file must not return a zero idea: %s", body)
	}
}

func TestIdeasPatchClearFlagsAndCompareAndSwap(t *testing.T) {
	fixture := newFixture(t)
	patch := testIdea("idea-1", "New title")
	fixture.ideas.updated = app.IdeaUpdateResult{Idea: patch}

	response := request(fixture, http.MethodPatch, "/v1/ideas/idea-1",
		`{"title":"New title","clear_due":true,"expected_updated_at":"2026-11-26T10:00:00Z","completion_note":"shipped"}`, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", response.Code, response.Body.String())
	}
	payload := decodeIdeaJSON(t, response.Body.Bytes())
	if _, present := payload["project_source"]; present {
		t.Fatalf("an in-place edit must omit project_source: %v", payload["project_source"])
	}
	got := fixture.ideas.lastUpdate
	if got.ID != "idea-1" || got.Title == nil || *got.Title != "New title" || !got.ClearDue {
		t.Fatalf("update input = %+v", got)
	}
	if got.ExpectedUpdatedAt.Format(time.RFC3339) != "2026-11-26T10:00:00Z" {
		t.Fatalf("expected_updated_at = %v", got.ExpectedUpdatedAt)
	}
	if got.CompletionNote == nil || *got.CompletionNote != "shipped" {
		t.Fatalf("completion note = %v", got.CompletionNote)
	}

	fixture.ideas.updated = app.IdeaUpdateResult{Idea: patch, Moved: true, ProjectSource: app.IdeaProjectExplicit}
	response = request(fixture, http.MethodPatch, "/v1/ideas/idea-1", `{"project":"personal"}`, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("move = %d %s", response.Code, response.Body.String())
	}
	if payload := decodeIdeaJSON(t, response.Body.Bytes()); payload["project_source"] != "explicit" {
		t.Fatalf("a move must report project_source: %v", payload["project_source"])
	}

	fixture.ideas.updateErr = domain.NewRevisionConflictError(ports.RecordIdea, "idea-1",
		time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC))
	response = request(fixture, http.MethodPatch, "/v1/ideas/idea-1",
		`{"title":"Stale","expected_updated_at":"2026-11-26T10:00:00Z"}`, nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("stale revision = %d %s", response.Code, response.Body.String())
	}
	payload = decodeIdeaJSON(t, response.Body.Bytes())
	envelope, _ := payload["error"].(map[string]any)
	if envelope["code"] != "conflict" || envelope["current_revision"] != "2026-11-27T09:00:00Z" {
		t.Fatalf("stale envelope = %v", envelope)
	}
}

func TestIdeasPatchRejectsEmptyAndConflictingRequests(t *testing.T) {
	fixture := newFixture(t)
	for name, body := range map[string]string{
		"empty patch":       `{}`,
		"unknown field":     `{"colour":"red"}`,
		"bad expected time": `{"title":"x","expected_updated_at":"yesterday"}`,
		"empty due_at":      `{"due_at":""}`,
		"empty due_date":    `{"due_date":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := request(fixture, http.MethodPatch, "/v1/ideas/idea-1", body, nil)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
			}
		})
	}

	fixture.ideas.updateErr = domain.NewValidationError("idea.due", "clear and set are mutually exclusive")
	response := request(fixture, http.MethodPatch, "/v1/ideas/idea-1",
		`{"clear_due":true,"due_at":"2026-12-01T18:00:00Z"}`, nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("clear+set = %d %s", response.Code, response.Body.String())
	}
}

func TestIdeasTrashAndRestore(t *testing.T) {
	fixture := newFixture(t)
	fixture.ideas.trashed = testIdea("idea-1", "One")
	fixture.ideas.restored = testIdea("idea-1", "One")
	response := request(fixture, http.MethodDelete, "/v1/ideas/idea-1", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", response.Code, response.Body.String())
	}
	payload := decodeIdeaJSON(t, response.Body.Bytes())
	if payload["id"] != "idea-1" || payload["trashed"] != true {
		t.Fatalf("trash response = %v", payload)
	}
	if fixture.ideas.lastTrashID != "idea-1" || fixture.ideas.lastTrashProject != "" {
		t.Fatalf("trash ids = %q %q", fixture.ideas.lastTrashProject, fixture.ideas.lastTrashID)
	}

	fixture.ideas.trashErr = domain.ErrNotFound
	if response := request(fixture, http.MethodDelete, "/v1/ideas/idea-404", "", nil); response.Code != http.StatusNotFound {
		t.Fatalf("unknown delete = %d %s", response.Code, response.Body.String())
	}

	response = request(fixture, http.MethodPost, "/v1/ideas/idea-1/restore", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("restore = %d %s", response.Code, response.Body.String())
	}
	if payload := decodeIdeaJSON(t, response.Body.Bytes()); payload["idea"] == nil {
		t.Fatalf("restore response = %v", payload)
	}
	if fixture.ideas.lastRestoreID != "idea-1" {
		t.Fatalf("restore id = %q", fixture.ideas.lastRestoreID)
	}

	fixture.ideas.restoreErr = domain.NewTargetExistsError(ports.RecordIdea, "idea-1", "C:/brain-data/projects/brain/ideas/idea-1.md")
	if response := request(fixture, http.MethodPost, "/v1/ideas/idea-1/restore", "", nil); response.Code != http.StatusConflict {
		t.Fatalf("restore collision = %d %s", response.Code, response.Body.String())
	}
}

func TestIdeasBriefMatchesTheSessionBlockShape(t *testing.T) {
	fixture := newFixture(t)
	item := testIdea("idea-1", "One")
	fixture.ideas.brief = app.IdeaBriefResult{
		Text:     "## Ideas — 1 open · 1 due today\n- [ ] today 15:00 — One (brain)",
		Ideas:    []domain.Idea{item},
		Open:     1,
		DueToday: 1,
	}

	response := request(fixture, http.MethodGet, "/v1/ideas/brief?project=brain&scope=current&max=5", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("brief = %d %s", response.Code, response.Body.String())
	}
	payload := decodeIdeaJSON(t, response.Body.Bytes())
	if payload["open"] != float64(1) || payload["due_today"] != float64(1) || payload["muted"] != false {
		t.Fatalf("brief counts = %v", payload)
	}
	if !strings.Contains(payload["text"].(string), "One (brain)") {
		t.Fatalf("text = %q", payload["text"])
	}
	ideas, _ := payload["ideas"].([]any)
	if len(ideas) != 1 {
		t.Fatalf("ideas = %v", payload["ideas"])
	}
	if _, present := ideas[0].(map[string]any)["body"]; present {
		t.Fatal("the brief must not carry body text")
	}
	got := fixture.ideas.lastBrief
	if got.ProjectID != "brain" || got.Scope != app.IdeaBriefScopeCurrent || got.Max != 5 {
		t.Fatalf("brief options = %+v", got)
	}
	// The literal brief path must beat the {id} route.
	if fixture.ideas.lastGetID != "" {
		t.Fatalf("brief dispatched to the id route as %q", fixture.ideas.lastGetID)
	}

	for _, path := range []string{"/v1/ideas/brief?max=11", "/v1/ideas/brief?scope=week"} {
		if response := request(fixture, http.MethodGet, path, "", nil); response.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", path, response.Code, response.Body.String())
		}
	}
}

func TestIdeasCalendarRendersDateAndDateTimeWithAlarms(t *testing.T) {
	fixture := newFixture(t)
	allDay := testIdea("idea-day", "All day")
	allDay.DueAt = nil
	allDay.DueDate = "2026-12-01"
	// Leave the lead on the timed idea only: the all-day lead trigger is
	// being redefined by the shared renderer, and this test must not pin it.
	allDay.RemindBeforeMinutes = nil
	timed := testIdea("idea-time", "Timed")
	done := testIdea("idea-done", "Finished")
	done.Status = domain.IdeaDone
	fixture.ideas.ideas = []domain.Idea{allDay, timed, done}

	response := request(fixture, http.MethodGet, "/v1/ideas/ical", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("ical = %d %s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/calendar; charset=utf-8" {
		t.Fatalf("content type = %q", contentType)
	}
	body := response.Body.String()
	for _, want := range []string{
		"BEGIN:VCALENDAR",
		"DTSTART;VALUE=DATE:20261201",
		"DTSTART:20261201T150000Z",
		"BEGIN:VALARM",
		"TRIGGER:-PT30M",
		"UID:idea-day",
		"UID:idea-time",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("calendar is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "UID:idea-done") {
		t.Fatalf("a done idea must be omitted by default:\n%s", body)
	}
	for _, status := range fixture.ideas.lastList.Statuses {
		if status == domain.IdeaDone {
			t.Fatal("the default export must not ask for done ideas")
		}
	}
	if !fixture.ideas.lastList.All {
		t.Fatalf("an omitted project exports every project: %+v", fixture.ideas.lastList)
	}

	response = request(fixture, http.MethodGet, "/v1/ideas/ical?include_done=1&project=brain", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("ical include done = %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "UID:idea-done") {
		t.Fatalf("include_done must render the done idea:\n%s", response.Body.String())
	}
	foundDone := false
	for _, status := range fixture.ideas.lastList.Statuses {
		if status == domain.IdeaDone {
			foundDone = true
		}
	}
	if !foundDone {
		t.Fatalf("include_done statuses = %v", fixture.ideas.lastList.Statuses)
	}
	// The literal ical path must beat the {id} route.
	if fixture.ideas.lastGetID != "" {
		t.Fatalf("ical dispatched to the id route as %q", fixture.ideas.lastGetID)
	}
}

func TestIdeasCalendarAppliesListFilters(t *testing.T) {
	fixture := newFixture(t)
	done := testIdea("idea-done", "Finished")
	done.Status = domain.IdeaDone
	fixture.ideas.ideas = []domain.Idea{testIdea("idea-1", "One"), done}

	response := request(fixture, http.MethodGet,
		"/v1/ideas/ical?project=brain&q=theme&tag=UI&status=doing", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("filtered ical = %d %s", response.Code, response.Body.String())
	}
	got := fixture.ideas.lastList
	if got.Query != "theme" || len(got.Tags) != 1 || got.Tags[0] != "UI" ||
		len(got.Statuses) != 1 || got.Statuses[0] != domain.IdeaDoing {
		t.Fatalf("ical filter options = %+v", got)
	}

	response = request(fixture, http.MethodGet, "/v1/ideas/ical?status=done", "", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "UID:idea-done") {
		t.Fatalf("explicit done export = %d %s", response.Code, response.Body.String())
	}
	foundDone := false
	for _, status := range fixture.ideas.lastList.Statuses {
		if status == domain.IdeaDone {
			foundDone = true
		}
	}
	if !foundDone {
		t.Fatalf("explicit status=done options = %v", fixture.ideas.lastList.Statuses)
	}

	response = request(fixture, http.MethodGet, "/v1/ideas/ical?status=dropped", "", nil)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "BEGIN:VEVENT") {
		t.Fatalf("dropped-only export = %d %s", response.Code, response.Body.String())
	}
}

func TestIdeasRequireBearerAndReportUnavailable(t *testing.T) {
	fixture := newFixture(t)
	requestWithoutAuth := httptest.NewRequest(http.MethodGet, "/v1/ideas", nil)
	requestWithoutAuth.Host = "127.0.0.1:43123"
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, requestWithoutAuth)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer = %d %s", recorder.Code, recorder.Body.String())
	}

	fixture.handler.opts.Ideas = nil
	response := request(fixture, http.MethodGet, "/v1/ideas", "", nil)
	if response.Code != http.StatusInternalServerError ||
		!strings.Contains(response.Body.String(), `"unavailable"`) {
		t.Fatalf("unwired ideas = %d %s", response.Code, response.Body.String())
	}
}

func TestIdeasMethodsAreEnforced(t *testing.T) {
	fixture := newFixture(t)
	for name, tc := range map[string]struct {
		method string
		path   string
		allow  string
	}{
		"collection put": {http.MethodPut, "/v1/ideas", "GET, POST"},
		"brief post":     {http.MethodPost, "/v1/ideas/brief", "GET"},
		"item post":      {http.MethodPost, "/v1/ideas/idea-1", "GET, PATCH, DELETE"},
		"restore get":    {http.MethodGet, "/v1/ideas/idea-1/restore", "POST"},
	} {
		t.Run(name, func(t *testing.T) {
			response := request(fixture, tc.method, tc.path, "", nil)
			if response.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405: %s", response.Code, response.Body.String())
			}
			if allow := response.Header().Get("Allow"); allow != tc.allow {
				t.Fatalf("allow = %q, want %q", allow, tc.allow)
			}
		})
	}
}

func TestIdeasWriteLimitAndServerModeKeepTheirEnvelopes(t *testing.T) {
	fixture := newFixture(t)
	fixture.ideas.addErr = &domain.LimitError{
		Field: "idea.body", ConfigKey: "limits.max_body_bytes", Limit: 16, Actual: 32,
	}
	response := request(fixture, http.MethodPost, "/v1/ideas", `{"title":"Big","body":"x"}`, nil)
	if response.Code != http.StatusRequestEntityTooLarge ||
		!strings.Contains(response.Body.String(), `"limit_exceeded"`) {
		t.Fatalf("limit = %d %s", response.Code, response.Body.String())
	}

	fixture.ideas.addErr = domain.ErrUnsupported
	response = request(fixture, http.MethodPost, "/v1/ideas", `{"title":"Server"}`, nil)
	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), `"unsupported"`) {
		t.Fatalf("server mode = %d %s", response.Code, response.Body.String())
	}
}

func TestIdeasUpdatedAtRoundTripsThroughExpectedUpdatedAt(t *testing.T) {
	fixture := newFixture(t)
	idea := testIdea("idea-1", "One")
	idea.UpdatedAt = time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	fixture.ideas.got = idea

	response := request(fixture, http.MethodGet, "/v1/ideas/idea-1", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("get = %d %s", response.Code, response.Body.String())
	}
	payload := decodeIdeaJSON(t, response.Body.Bytes())
	row, _ := payload["idea"].(map[string]any)
	revision, _ := row["updated_at"].(string)
	if revision != "2026-11-26T10:00:00Z" {
		t.Fatalf("updated_at = %q, want RFC3339", revision)
	}

	fixture.ideas.updated = app.IdeaUpdateResult{Idea: idea}
	response = request(fixture, http.MethodPatch, "/v1/ideas/idea-1",
		`{"title":"Two","expected_updated_at":"`+revision+`"}`, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", response.Code, response.Body.String())
	}
	if got := fixture.ideas.lastUpdate.ExpectedUpdatedAt; got.IsZero() || !got.Equal(idea.UpdatedAt) {
		t.Fatalf("expected_updated_at = %v, want %v", got, idea.UpdatedAt)
	}
}

func TestIdeasProtectedProjectIsForbidden(t *testing.T) {
	fixture := newFixture(t)
	fixture.ideas.addErr = domain.NewProtectedProjectError("brain")
	response := request(fixture, http.MethodPost, "/v1/ideas", `{"title":"x","project":"brain"}`, nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("protected create = %d %s", response.Code, response.Body.String())
	}
	payload := decodeIdeaJSON(t, response.Body.Bytes())
	envelope, _ := payload["error"].(map[string]any)
	if envelope["code"] != "forbidden" {
		t.Fatalf("protected create code = %v", envelope)
	}

	fixture.ideas.addErr = nil
	fixture.ideas.updateErr = domain.NewProtectedProjectError("brain")
	response = request(fixture, http.MethodPatch, "/v1/ideas/idea-1", `{"title":"x"}`, nil)
	if response.Code != http.StatusForbidden {
		t.Fatalf("protected edit = %d %s", response.Code, response.Body.String())
	}
}

func TestIdeasRoutesIgnoreTheMemoryAllowlist(t *testing.T) {
	fixture := newFixture(t)
	fixture.handler.opts.ProjectAllowlist = []string{"brain"}

	response := request(fixture, http.MethodGet, "/v1/ideas", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("global list = %d %s", response.Code, response.Body.String())
	}
	if !fixture.ideas.lastList.All || fixture.ideas.lastList.ProjectID != "" {
		t.Fatalf("global list options = %+v", fixture.ideas.lastList)
	}

	response = request(fixture, http.MethodGet, "/v1/ideas?project=personal", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("explicit list = %d %s", response.Code, response.Body.String())
	}
	if fixture.ideas.lastList.ProjectID != "personal" || fixture.ideas.lastList.All {
		t.Fatalf("explicit list options = %+v", fixture.ideas.lastList)
	}
}

func TestOpenAPIDocumentsTheIdeaSchemas(t *testing.T) {
	data, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Idea", "IdeaPage", "IdeaWrite", "IdeaShow", "IdeaBrief", "IdeaCreate", "IdeaPatch", "IdeaTrash", "IdeaProblem"} {
		if _, ok := parsed.Components.Schemas[name]; !ok {
			t.Fatalf("openapi.json is missing schema %s", name)
		}
	}
	var idea map[string]any
	if err := json.Unmarshal(parsed.Components.Schemas["Idea"], &idea); err != nil {
		t.Fatal(err)
	}
	properties, _ := idea["properties"].(map[string]any)
	for _, field := range []string{"id", "project_id", "title", "body", "status", "priority", "due_at", "due_date", "remind_before_minutes", "tags", "source", "completed_at", "created_at", "updated_at", "path", "provenance", "validity", "relations", "review"} {
		if _, ok := properties[field]; !ok {
			t.Fatalf("Idea schema is missing %s", field)
		}
	}

	schemaObject := func(name string) map[string]any {
		var object map[string]any
		if err := json.Unmarshal(parsed.Components.Schemas[name], &object); err != nil {
			t.Fatalf("decode schema %s: %v", name, err)
		}
		return object
	}
	for field, want := range map[string]string{
		"relations": "#/components/schemas/Relation",
	} {
		block, _ := properties[field].(map[string]any)
		items, _ := block["items"].(map[string]any)
		if items["$ref"] != want {
			t.Fatalf("Idea.%s items = %v, want $ref %s", field, items, want)
		}
	}
	for _, field := range []string{"body"} {
		block, _ := properties[field].(map[string]any)
		if block["x-max-bytes"] != float64(1048576) {
			t.Fatalf("Idea.%s x-max-bytes = %v", field, block["x-max-bytes"])
		}
	}

	for _, name := range []string{"IdeaCreate", "IdeaPatch"} {
		object := schemaObject(name)
		props, _ := object["properties"].(map[string]any)
		for _, field := range []string{"due_at", "due_date", "remind_before_minutes"} {
			block, _ := props[field].(map[string]any)
			if block["nullable"] != true {
				t.Fatalf("%s.%s must be nullable: %v", name, field, block)
			}
		}
		body, _ := props["body"].(map[string]any)
		if body["x-max-bytes"] != float64(1048576) {
			t.Fatalf("%s.body x-max-bytes = %v", name, body["x-max-bytes"])
		}
		if _, ok := props["completion_note"]; !ok {
			t.Fatalf("%s is missing completion_note", name)
		}
	}

	page := schemaObject("IdeaPage")
	pageProps, _ := page["properties"].(map[string]any)
	source, _ := pageProps["project_source"].(map[string]any)
	enum, _ := source["enum"].([]any)
	seen := map[string]bool{}
	for _, value := range enum {
		seen[value.(string)] = true
	}
	for _, want := range []string{"explicit", "cwd", "default"} {
		if !seen[want] {
			t.Fatalf("IdeaPage.project_source enum = %v, missing %s", enum, want)
		}
	}
}

func TestIdeasBodyCapIsOneMiB(t *testing.T) {
	fixture := newFixture(t)
	fixture.handler.opts.MaxBody = DefaultMaxBodyBytes
	body := `{"title":"Huge","body":"` + strings.Repeat("x", DefaultMaxBodyBytes) + `"}`
	response := request(fixture, http.MethodPost, "/v1/ideas", body, nil)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"body_too_large"`) {
		t.Fatalf("oversized body envelope = %s", response.Body.String())
	}
	if len(fixture.ideas.lastAdd.Title) != 0 {
		t.Fatal("an oversized body must never reach the use case")
	}
}
