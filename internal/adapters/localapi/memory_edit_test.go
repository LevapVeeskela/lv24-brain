package localapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func decodeEditEnvelope(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return envelope
}

func TestEditRecordHandlerValidatesRequestAndReturnsDetail(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	memory.edit = RecordDetailView{
		RecordView: RecordView{ID: "note-1", ProjectID: "brain", Kind: "note", Title: "edited"},
		Body:       "new body for " + testToken,
		BodyTokens: 5, MaxTokens: 4000,
	}

	w := request(fixture, http.MethodPatch, "/v1/records/note-1?project=brain",
		`{"title":"edited","body":"new body","tags":["one","two"],"expected_updated_at":"2026-11-15T10:00:00.123456789Z","allow_duplicate":true}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("edit = %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), testToken) {
		t.Fatalf("edit response leaked the token: %s", w.Body.String())
	}
	field := memory.lastEdit
	if field.Title == nil || *field.Title != "edited" || field.Body == nil || *field.Body != "new body" ||
		field.Tags == nil || len(*field.Tags) != 2 || !field.AllowDuplicate ||
		!field.ExpectedUpdatedAt.Equal(time.Date(2026, 11, 15, 10, 0, 0, 123456789, time.UTC)) {
		t.Fatalf("edit request = %+v", field)
	}

	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"nothing", `{}`, http.StatusBadRequest, "validation"},
		{"empty expected", `{"body":"x","expected_updated_at":""}`, http.StatusBadRequest, "validation"},
		{"bad expected", `{"body":"x","expected_updated_at":"yesterday"}`, http.StatusBadRequest, "validation"},
		{"unknown field", `{"body":"x","id":"other"}`, http.StatusBadRequest, "invalid_json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := request(fixture, http.MethodPatch, "/v1/records/note-1?project=brain", tc.body, nil)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `"`+tc.code+`"`) {
				t.Fatalf("edit = %d %s", w.Code, w.Body.String())
			}
		})
	}

	many := `{"tags":[` + strings.Repeat(`"t",`, recordEditMaxTags) + `"last"]}`
	fixture.handler.opts.MaxBody = 1 << 20
	if w := request(fixture, http.MethodPatch, "/v1/records/note-1?project=brain", many, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("too many tags = %d %s", w.Code, w.Body.String())
	}
}

func TestEditRecordHandlerMapsTypedErrors(t *testing.T) {
	fixture, memory := newMemoryFixture(t)
	conflictAt := time.Date(2026, 11, 15, 10, 0, 0, 123456789, time.UTC)
	cases := []struct {
		name   string
		err    error
		status int
		code   string
		extra  string
	}{
		{
			name: "revision", err: &domain.RevisionConflictError{Kind: "note", ID: "note-1", CurrentUpdatedAt: conflictAt},
			status: http.StatusConflict, code: "conflict", extra: conflictAt.Format(time.RFC3339Nano),
		},
		{
			name: "limit", err: &domain.LimitError{Field: "note.body", ConfigKey: "limits.max_body_bytes", Limit: 8, Actual: 10, Unit: "bytes"},
			status: http.StatusRequestEntityTooLarge, code: "limit_exceeded", extra: "limits.max_body_bytes",
		},
		{
			name: "protected", err: &domain.ProtectedError{Kind: "project", ConfigKey: "limits.protected_projects", ProjectID: "brain"},
			status: http.StatusForbidden, code: "forbidden", extra: "protected_projects",
		},
		{
			name: "duplicate", err: &domain.DuplicateError{Kind: "note", ID: "note-2", ProjectID: "brain"},
			status: http.StatusConflict, code: "duplicate", extra: "note-2",
		},
		{
			name: "missing", err: domain.ErrNotFound,
			status: http.StatusNotFound, code: "not_found",
		},
		{
			name: "unsupported", err: domain.ErrUnsupported,
			status: http.StatusBadRequest, code: "unsupported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			memory.editErr = tc.err
			w := request(fixture, http.MethodPatch, "/v1/records/note-1?project=brain", `{"body":"x"}`, nil)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `"`+tc.code+`"`) {
				t.Fatalf("edit = %d %s", w.Code, w.Body.String())
			}
			if tc.extra != "" && !strings.Contains(w.Body.String(), tc.extra) {
				t.Fatalf("edit response does not name %q: %s", tc.extra, w.Body.String())
			}
			if tc.code == "conflict" {
				envelope := decodeEditEnvelope(t, w)
				errorBody, _ := envelope["error"].(map[string]any)
				if errorBody["current_revision"] != conflictAt.Format(time.RFC3339Nano) {
					t.Fatalf("current_revision = %v", errorBody["current_revision"])
				}
			}
		})
	}
	memory.editErr = nil

	// A protected path that embeds the API token must not leak it in the
	// error message.
	memory.editErr = &domain.ProtectedError{
		Kind: "glob", ConfigKey: "limits.protected_globs",
		Path: "projects/brain/" + testToken, Pattern: "projects/brain/**",
	}
	w := request(fixture, http.MethodPatch, "/v1/records/note-1?project=brain", `{"body":"x"}`, nil)
	if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), testToken) {
		t.Fatalf("redacted protected error = %d %s", w.Code, w.Body.String())
	}
}
