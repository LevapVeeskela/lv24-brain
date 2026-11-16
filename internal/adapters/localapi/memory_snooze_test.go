package localapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestProposalSnoozeRouteMapsUntilClearAndFields(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	until := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	expected := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	memory.snooze = ProposalSnoozeView{
		ProjectID: "brain", ProposalID: "proposal-gh-000000000000", Status: "pending",
		Snoozed: true, SnoozedUntil: until, UpdatedAt: expected,
	}
	path := "/v1/proposals/brain/proposal-gh-000000000000/snooze"

	w := request(fixture, http.MethodPost, path,
		`{"until":"2026-12-01T09:00:00Z","expected_updated_at":"2026-11-16T10:00:00Z"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("snooze = %d %s", w.Code, w.Body.String())
	}
	if !memory.lastSnooze.Until.Equal(until) || memory.lastSnooze.Clear ||
		!memory.lastSnooze.ExpectedUpdatedAt.Equal(expected) ||
		memory.lastSnooze.ProposalID != "proposal-gh-000000000000" || memory.lastSnooze.ProjectID != "brain" {
		t.Fatalf("snooze request = %+v", memory.lastSnooze)
	}
	body := decodeEditEnvelope(t, w)
	if body["snoozed"] != true || body["status"] != "pending" || body["snoozed_until"] == nil {
		t.Fatalf("snooze response = %v", body)
	}

	if w := request(fixture, http.MethodPost, path, `{"until":"7d"}`, nil); w.Code != http.StatusOK {
		t.Fatalf("duration snooze = %d %s", w.Code, w.Body.String())
	}
	if memory.lastSnooze.Until.Before(time.Now().Add(6 * 24 * time.Hour)) {
		t.Fatalf("duration snooze until = %v", memory.lastSnooze.Until)
	}
	if w := request(fixture, http.MethodPost, path, `{"until":null}`, nil); w.Code != http.StatusOK {
		t.Fatalf("null snooze = %d %s", w.Code, w.Body.String())
	}
	if !memory.lastSnooze.Clear || !memory.lastSnooze.Until.IsZero() {
		t.Fatalf("null snooze request = %+v", memory.lastSnooze)
	}
	if w := request(fixture, http.MethodDelete, path, "", nil); w.Code != http.StatusOK {
		t.Fatalf("delete snooze = %d %s", w.Code, w.Body.String())
	}
	if !memory.lastSnooze.Clear {
		t.Fatalf("delete snooze request = %+v", memory.lastSnooze)
	}
	if w := request(fixture, http.MethodGet, path, "", nil); w.Code != http.StatusMethodNotAllowed ||
		w.Header().Get("Allow") != "POST, DELETE" {
		t.Fatalf("snooze GET = %d Allow=%q", w.Code, w.Header().Get("Allow"))
	}

	for _, body := range []string{
		"",
		`{}`,
		`{"until":7}`,
		`{"until":"soon"}`,
		`{"until":"2026-12-01","expected_updated_at":"yesterday"}`,
		`{"until":"2026-12-01","unknown":true}`,
	} {
		if w := request(fixture, http.MethodPost, path, body, nil); w.Code != http.StatusBadRequest {
			t.Fatalf("snooze body %q = %d %s", body, w.Code, w.Body.String())
		}
	}

	conflictAt := time.Date(2026, 11, 16, 11, 0, 0, 0, time.UTC)
	memory.snoozeErr = &domain.RevisionConflictError{Kind: "proposal", ID: "proposal-gh-000000000000", CurrentUpdatedAt: conflictAt}
	w = request(fixture, http.MethodPost, path, `{"until":"2026-12-01T09:00:00Z"}`, nil)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), conflictAt.Format(time.RFC3339Nano)) {
		t.Fatalf("snooze conflict = %d %s", w.Code, w.Body.String())
	}
	memory.snoozeErr = domain.ErrValidation
	if w := request(fixture, http.MethodPost, path, `{"until":"2026-12-01T09:00:00Z"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("snooze validation = %d %s", w.Code, w.Body.String())
	}
	memory.snoozeErr = domain.ErrNotFound
	if w := request(fixture, http.MethodDelete, path, "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("snooze missing = %d %s", w.Code, w.Body.String())
	}
}

func TestProposalListIncludeSnoozedAndSnoozeFields(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	until := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	memory.page = ProposalPage{ProjectID: "brain", Total: 1, Items: []ProposalView{{
		ID: "proposal-gh-000000000001", ProjectID: "brain", Kind: "decision", Title: "one",
		Status: "pending", Confidence: 0.8, Snoozed: true, SnoozedUntil: until,
	}}}
	w := request(fixture, http.MethodGet, "/v1/proposals?project=brain&include_snoozed=true", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("proposals = %d %s", w.Code, w.Body.String())
	}
	if !memory.lastProposal.IncludeSnoozed {
		t.Fatalf("include_snoozed not passed through: %+v", memory.lastProposal)
	}
	page := decodeMap(t, w.Body.String())
	items, _ := page["items"].([]any)
	first, _ := items[0].(map[string]any)
	if first["snoozed"] != true || first["snoozed_until"] == nil {
		t.Fatalf("snoozed proposal row = %v", first)
	}

	if w := request(fixture, http.MethodGet, "/v1/proposals?project=brain", "", nil); w.Code != http.StatusOK {
		t.Fatalf("default proposals = %d %s", w.Code, w.Body.String())
	}
	if memory.lastProposal.IncludeSnoozed {
		t.Fatalf("default list asked for snoozed rows: %+v", memory.lastProposal)
	}
	if w := request(fixture, http.MethodGet, "/v1/proposals?project=brain&include_snoozed=maybe", "", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad include_snoozed = %d %s", w.Code, w.Body.String())
	}
}

func TestRecordRoutesExposeNoteTags(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	row := recordRow("note-1", "note", time.Now().UTC())
	row.Tags = []string{"search", "design"}
	memory.records = []RecordView{row}
	memory.detail = RecordDetailView{RecordView: row, Body: "body"}

	w := request(fixture, http.MethodGet, "/v1/records?project=brain", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"tags":["search","design"]`) {
		t.Fatalf("record list = %d %s", w.Code, w.Body.String())
	}
	w = request(fixture, http.MethodGet, "/v1/records/note-1?project=brain", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"tags":["search","design"]`) {
		t.Fatalf("record detail = %d %s", w.Code, w.Body.String())
	}
}
