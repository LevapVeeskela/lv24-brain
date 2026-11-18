package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// createProvenanceProject creates a project through the API with the given
// admin token.
func createProvenanceProject(t *testing.T, baseURL, token, id string) {
	t.Helper()
	resp, payload := jsonRequest(t, http.MethodPost, baseURL+"/v1/projects", map[string]any{
		"id": id, "name": "Project " + id, "kind": "app",
	}, token)
	requireStatus(t, resp, payload, http.StatusCreated)
}

// recordIDs decodes a {"items":[{...}]} list response and returns its ids.
func recordIDs(t *testing.T, payload []byte) []string {
	t.Helper()
	var body struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode list response: %v (%s)", err, payload)
	}
	ids := make([]string, 0, len(body.Items))
	for _, item := range body.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

func TestCreateRecordStampsNormalizedProvenance(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)
	createProvenanceProject(t, ts.URL, admin.AccessToken, "prov")

	cases := []struct {
		name string
		path string
		body map[string]any
		note bool
	}{
		{"note", "/v1/projects/prov/notes", map[string]any{"title": "Note", "body": "body"}, true},
		{"decision", "/v1/projects/prov/decisions", map[string]any{"title": "Decision", "context": "ctx"}, false},
		{"attempt", "/v1/projects/prov/attempts", map[string]any{"title": "Attempt", "hypothesis": "h"}, false},
		{"handoff", "/v1/projects/prov/handoffs", map[string]any{"summary": "Handoff"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, payload := jsonRequest(t, http.MethodPost, ts.URL+tc.path, tc.body, admin.AccessToken)
			requireStatus(t, resp, payload, http.StatusCreated)
			var provenance domain.Provenance
			if tc.note {
				var record domain.Note
				if err := json.Unmarshal(payload, &record); err != nil {
					t.Fatalf("decode note: %v", err)
				}
				provenance = record.Provenance
			} else {
				var raw struct {
					Origin   domain.Origin `json:"origin"`
					Reviewed bool          `json:"reviewed"`
					Trust    domain.Trust  `json:"trust"`
				}
				if err := json.Unmarshal(payload, &raw); err != nil {
					t.Fatalf("decode record: %v", err)
				}
				provenance = domain.Provenance{Origin: raw.Origin, Reviewed: raw.Reviewed, Trust: raw.Trust}
			}
			if provenance.Origin != domain.OriginHuman {
				t.Fatalf("origin = %q, want human; body = %s", provenance.Origin, payload)
			}
			if !provenance.Reviewed {
				t.Fatalf("reviewed = false, want true; body = %s", payload)
			}
			if provenance.Trust != domain.TrustVerified {
				t.Fatalf("trust = %q, want verified; body = %s", provenance.Trust, payload)
			}
			if provenance.Quarantined() {
				t.Fatalf("a create response is quarantined: %+v", provenance)
			}
		})
	}
}

func TestReviewRecordEndpointFlipsReviewed(t *testing.T) {
	ts, _, store := newTestServer(t, nil)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)
	createProvenanceProject(t, ts.URL, admin.AccessToken, "review")

	ctx := context.Background()
	created, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID: "review",
		Title:     "Agent note",
		Body:      "rollout quarantine candidate",
		Provenance: domain.Provenance{
			Origin: domain.OriginAgent,
			Trust:  domain.TrustStated,
		},
	})
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if !created.Note.Provenance.Quarantined() {
		t.Fatalf("seeded note is not quarantined: %+v", created.Note.Provenance)
	}

	// The default listing hides the quarantined note; the explicit opt-in shows it.
	resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/v1/projects/review/notes", nil, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK)
	if ids := recordIDs(t, payload); len(ids) != 0 {
		t.Fatalf("default listing = %v, want no quarantined note", ids)
	}
	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/projects/review/notes?include_unreviewed=true", nil, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK)
	if ids := recordIDs(t, payload); len(ids) != 1 || ids[0] != created.ID {
		t.Fatalf("include_unreviewed listing = %v, want %s", ids, created.ID)
	}

	// Review clears the quarantine and flips reviewed.
	resp, payload = jsonRequest(t, http.MethodPost,
		ts.URL+"/v1/projects/review/records/note/"+created.ID+"/review",
		map[string]any{"reviewed": true, "revision": created.Revision, "trust": "verified"},
		admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK)
	var reviewed struct {
		domain.Note
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal(payload, &reviewed); err != nil {
		t.Fatalf("decode reviewed note: %v (%s)", err, payload)
	}
	if !reviewed.Reviewed {
		t.Fatalf("reviewed = false, want true; body = %s", payload)
	}
	if reviewed.Origin != domain.OriginAgent {
		t.Fatalf("origin = %q, want agent (review keeps the channel)", reviewed.Origin)
	}
	if reviewed.Trust != domain.TrustVerified {
		t.Fatalf("trust = %q, want verified", reviewed.Trust)
	}
	if reviewed.Note.Provenance.Quarantined() {
		t.Fatalf("reviewed note is still quarantined: %+v", reviewed.Note.Provenance)
	}
	if !reviewed.Changed {
		t.Fatalf("changed = false, want true; body = %s", payload)
	}

	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/projects/review/notes", nil, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK)
	if ids := recordIDs(t, payload); len(ids) != 1 || ids[0] != created.ID {
		t.Fatalf("listing after review = %v, want %s", ids, created.ID)
	}

	// Replaying the stale revision is a conflict, not a silent overwrite.
	resp, payload = jsonRequest(t, http.MethodPost,
		ts.URL+"/v1/projects/review/records/note/"+created.ID+"/review",
		map[string]any{"reviewed": true, "revision": created.Revision},
		admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusConflict)

	// A review that would re-arm the quarantine is refused.
	resp, payload = jsonRequest(t, http.MethodPost,
		ts.URL+"/v1/projects/review/records/note/"+created.ID+"/review",
		map[string]any{"reviewed": false}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusBadRequest)

	// A missing record is 404 and a project mismatch does not leak it.
	resp, payload = jsonRequest(t, http.MethodPost,
		ts.URL+"/v1/records/note/note-missing/review", map[string]any{"reviewed": true}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusNotFound)
	resp, payload = jsonRequest(t, http.MethodPost,
		ts.URL+"/v1/projects/other/records/note/"+created.ID+"/review",
		map[string]any{"reviewed": true}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusNotFound)
}

func TestSearchExcludesQuarantinedButKeepsLegacyRows(t *testing.T) {
	ts, _, store := newTestServer(t, nil)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)
	createProvenanceProject(t, ts.URL, admin.AccessToken, "legacy")

	ctx := context.Background()
	legacy, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID:  "legacy",
		Title:      "Rollout legacy",
		Body:       "rollout written before provenance existed",
		Provenance: domain.Provenance{},
	})
	if err != nil {
		t.Fatalf("CreateNote legacy: %v", err)
	}
	if legacy.Note.Provenance.Quarantined() {
		t.Fatalf("legacy note (empty origin, reviewed=false) must not be quarantined")
	}
	quarantined, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID:  "legacy",
		Title:      "Rollout agent",
		Body:       "rollout pending review",
		Provenance: domain.Provenance{Origin: domain.OriginAgent},
	})
	if err != nil {
		t.Fatalf("CreateNote quarantined: %v", err)
	}
	if !quarantined.Note.Provenance.Quarantined() {
		t.Fatalf("agent note with reviewed=false must be quarantined")
	}
	reviewedAgent, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID: "legacy",
		Title:     "Rollout reviewed agent",
		Body:      "rollout already trusted",
		Provenance: domain.Provenance{
			Origin:   domain.OriginAgent,
			Reviewed: true,
		},
	})
	if err != nil {
		t.Fatalf("CreateNote reviewed agent: %v", err)
	}

	resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/v1/search?q=rollout", nil, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK)
	got := map[string]bool{}
	for _, id := range recordIDs(t, payload) {
		got[id] = true
	}
	if !got[legacy.ID] {
		t.Fatalf("legacy note %s missing from search: %s", legacy.ID, payload)
	}
	if !got[reviewedAgent.ID] {
		t.Fatalf("reviewed agent note %s missing from search: %s", reviewedAgent.ID, payload)
	}
	if got[quarantined.ID] {
		t.Fatalf("quarantined note %s leaked into search: %s", quarantined.ID, payload)
	}

	// Reading the quarantined record by id is an explicit request.
	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/notes/"+quarantined.ID, nil, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK)
}

func TestReviewRecordEndpointUnscopedRoute(t *testing.T) {
	ts, _, store := newTestServer(t, nil)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)
	createProvenanceProject(t, ts.URL, admin.AccessToken, "unscoped")

	created, _, err := store.CreateDecision(context.Background(), domain.Decision{
		ProjectID:  "unscoped",
		Title:      "Agent decision",
		Provenance: domain.Provenance{Origin: domain.OriginAgent},
	})
	if err != nil {
		t.Fatalf("CreateDecision: %v", err)
	}

	resp, payload := jsonRequest(t, http.MethodPost,
		ts.URL+"/v1/records/decision/"+created.ID+"/review",
		map[string]any{"reviewed": true}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK)
	var reviewed struct {
		domain.Decision
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal(payload, &reviewed); err != nil {
		t.Fatalf("decode reviewed decision: %v (%s)", err, payload)
	}
	if !reviewed.Reviewed || reviewed.Origin != domain.OriginAgent || !reviewed.Changed {
		t.Fatalf("reviewed decision = %+v, body = %s", reviewed, payload)
	}
}

func TestReviewExpectedRevisionRequiresCAS(t *testing.T) {
	cases := []struct {
		name     string
		client   int64
		stored   int64
		want     int64
		wantCode int
	}{
		{"current revision", 0, 7, 7, http.StatusOK},
		{"matching client revision", 7, 7, 7, http.StatusOK},
		{"stale client revision", 6, 7, 0, http.StatusConflict},
		{"negative client revision", -1, 7, 0, http.StatusBadRequest},
		{"no stored revision", 0, 0, 0, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			got, ok := reviewExpectedRevision(w, tc.client, tc.stored)
			if tc.wantCode == http.StatusOK {
				if !ok || got != tc.want {
					t.Fatalf("expected revision = (%d, %v), want (%d, true)", got, ok, tc.want)
				}
				return
			}
			if ok {
				t.Fatalf("expected a refusal, got revision %d", got)
			}
			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tc.wantCode, w.Body.String())
			}
		})
	}
}
