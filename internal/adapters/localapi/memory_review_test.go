package localapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestRecordReviewRoutePassesRequestThrough(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	reviewed := recordRow("note-1", "note", time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC))
	reviewed.Provenance = ProvenanceView{Origin: "agent", Trust: "stated", Reviewed: true}
	memory.record = reviewed

	expected := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	w := request(fixture, http.MethodPost, "/v1/records/note-1/review?project=brain",
		`{"reviewed":true,"trust":"verified","expected_updated_at":"2026-11-14T09:00:00Z"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("review = %d %s", w.Code, w.Body.String())
	}
	got := memory.lastRecordReview
	if got.ProjectID != "brain" || got.ID != "note-1" || !got.Reviewed || got.Trust != "verified" {
		t.Fatalf("request = %+v", got)
	}
	if !got.ExpectedUpdatedAt.Equal(expected) {
		t.Fatalf("expected_updated_at = %s, want %s", got.ExpectedUpdatedAt, expected)
	}
	body := decodeMap(t, w.Body.String())
	if body["id"] != "note-1" {
		t.Fatalf("response = %v", body)
	}
	provenance, _ := body["provenance"].(map[string]any)
	if provenance["origin"] != "agent" || provenance["trust"] != "stated" || provenance["reviewed"] != true {
		t.Fatalf("response provenance = %v", provenance)
	}
}

func TestRecordReviewOptionalProjectUsesSingleAllowlist(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	fixture.handler.opts.ProjectAllowlist = []string{"demo"}

	if w := request(fixture, http.MethodPost, "/v1/records/note-1/review", `{"reviewed":true}`, nil); w.Code != http.StatusOK {
		t.Fatalf("review without project = %d %s", w.Code, w.Body.String())
	}
	if memory.lastRecordReview.ProjectID != "demo" {
		t.Fatalf("project = %q, want the single allowed project", memory.lastRecordReview.ProjectID)
	}

	// An explicit selector outside the allowlist is refused before the service.
	if w := request(fixture, http.MethodPost, "/v1/records/note-1/review?project=other", `{}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("project outside the allowlist = %d %s", w.Code, w.Body.String())
	}

	// A wider allowlist needs the explicit project.
	fixture.handler.opts.ProjectAllowlist = []string{"demo", "other"}
	if w := request(fixture, http.MethodPost, "/v1/records/note-1/review", `{}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous scope = %d %s", w.Code, w.Body.String())
	}
}

func TestRecordReviewRefusals(t *testing.T) {
	fixture, memory := newMemoryFixture(t)

	if w := request(fixture, http.MethodPost, "/v1/records/note-1/review?project=brain", `{"reviewed":false}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("reviewed=false = %d %s", w.Code, w.Body.String())
	}
	if w := request(fixture, http.MethodPost, "/v1/records/note-1/review?project=brain", `{"expected_updated_at":"yesterday"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad expected_updated_at = %d %s", w.Code, w.Body.String())
	}
	if w := request(fixture, http.MethodGet, "/v1/records/note-1/review?project=brain", "", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d %s", w.Code, w.Body.String())
	}

	memory.recordReviewErr = domain.NewRevisionConflictError("note", "note-1", time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC))
	w := request(fixture, http.MethodPost, "/v1/records/note-1/review?project=brain", `{"expected_updated_at":"2026-11-14T09:00:00Z"}`, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale CAS = %d %s", w.Code, w.Body.String())
	}
	envelope := decodeMap(t, w.Body.String())
	if envelope["error"].(map[string]any)["current_revision"] == "" {
		t.Fatalf("missing current_revision: %s", w.Body.String())
	}
}

func TestStateReviewRoutePassesRequestThrough(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.state = StateView{ProjectID: "brain", Goal: "goal", Revision: "rev-2"}
	memory.state.Provenance = ProvenanceView{Origin: "agent", Trust: "stated", Reviewed: true}

	w := request(fixture, http.MethodPost, "/v1/projects/brain/state/review",
		`{"reviewed":true,"trust":"verified","expected_revision":"rev-1"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("review = %d %s", w.Code, w.Body.String())
	}
	got := memory.lastStateReview
	if got.ProjectID != "brain" || !got.Reviewed || got.Trust != "verified" || got.ExpectedRevision != "rev-1" {
		t.Fatalf("request = %+v", got)
	}
	body := decodeMap(t, w.Body.String())
	if body["revision"] == "" {
		t.Fatalf("response has no revision: %s", w.Body.String())
	}
	provenance, _ := body["provenance"].(map[string]any)
	if provenance == nil || provenance["origin"] == "" || provenance["reviewed"] != true {
		t.Fatalf("response provenance = %v", provenance)
	}
}

func TestStateReviewCASConflictAndIdempotency(t *testing.T) {
	fixture, memory := newMemoryFixture(t)

	memory.stateReviewErr = &app.StateRevisionConflict{ProjectID: "brain", Expected: "rev-1", Current: "rev-2"}
	w := request(fixture, http.MethodPost, "/v1/projects/brain/state/review", `{"expected_revision":"rev-1"}`, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale CAS = %d %s", w.Code, w.Body.String())
	}
	envelope := decodeMap(t, w.Body.String())
	if got := envelope["error"].(map[string]any)["current_revision"]; got != "rev-2" {
		t.Fatalf("current_revision = %v", got)
	}

	// A repeated review of an already reviewed state writes nothing and keeps
	// the revision, so the same expected_revision keeps working.
	memory.stateReviewErr = nil
	memory.state = StateView{ProjectID: "brain", Revision: "rev-2"}
	for i := 0; i < 2; i++ {
		w := request(fixture, http.MethodPost, "/v1/projects/brain/state/review", `{"expected_revision":"rev-2"}`, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("review %d = %d %s", i, w.Code, w.Body.String())
		}
		if got := memory.lastStateReview.ExpectedRevision; got != "rev-2" {
			t.Fatalf("expected_revision = %q", got)
		}
	}
	if w := request(fixture, http.MethodPost, "/v1/projects/brain/state/review", `{"reviewed":false}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("reviewed=false = %d %s", w.Code, w.Body.String())
	}
	if w := request(fixture, http.MethodGet, "/v1/projects/brain/state/review", "", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d %s", w.Code, w.Body.String())
	}
}

// TestStateReviewUnknownProjectIsValidation keeps the documented project
// semantics: an unregistered selector is 400 validation, not 404.
func TestStateReviewUnknownProjectIsValidation(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.resolveErr = domain.NewValidationError("project", `"nope" is not a registered project id or alias`)

	w := request(fixture, http.MethodPost, "/v1/projects/nope/state/review", `{}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown project = %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "nope is not") {
		t.Fatalf("error leaked the selector: %s", w.Body.String())
	}
}

// TestReviewRoutesWithoutServiceAreUnavailable proves both review routes
// report 500 unavailable when the engine has no memory use cases.
func TestReviewRoutesWithoutServiceAreUnavailable(t *testing.T) {
	fixture := newFixture(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/records/note-1/review?project=brain"},
		{http.MethodPost, "/v1/projects/brain/state/review"},
	} {
		if w := request(fixture, tc.method, tc.path, `{}`, nil); w.Code != http.StatusInternalServerError ||
			!strings.Contains(w.Body.String(), `"unavailable"`) {
			t.Fatalf("%s %s = %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

// TestRecordReviewResponseIsRedacted keeps the redaction guarantee: the review
// response passes through the handler redactor, so a secret-looking record
// title never leaves the process.
func TestRecordReviewResponseIsRedacted(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.record = recordRow("note-1", "note", time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC))
	memory.record.Title = "sk-proj-abcdefghijklmnopqrstuvwxyz0123"
	w := request(fixture, http.MethodPost, "/v1/records/note-1/review?project=brain", `{"reviewed":true}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("review = %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sk-proj") {
		t.Fatalf("response leaked the record title: %s", w.Body.String())
	}
}

// TestRecordReviewServiceErrorPassthrough proves the handler maps domain
// errors unchanged, so clients can branch on the typed envelopes.
func TestRecordReviewServiceErrorPassthrough(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not found", domain.ErrNotFound, http.StatusNotFound},
		{"unsupported", domain.ErrUnsupported, http.StatusBadRequest},
		{"validation", domain.NewValidationError("trust", "unknown trust \"guessed\""), http.StatusBadRequest},
	}
	for _, tc := range cases {
		memory.recordReviewErr = tc.err
		if w := request(fixture, http.MethodPost, "/v1/records/note-1/review?project=brain", `{}`, nil); w.Code != tc.want {
			t.Fatalf("%s = %d, want %d", tc.name, w.Code, tc.want)
		}
	}
}
