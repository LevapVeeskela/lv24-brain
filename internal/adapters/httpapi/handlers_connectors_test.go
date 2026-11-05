package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

type fakeConnectorRunner struct {
	run domain.ConnectorRun
	err error
	got ConnectorRunRequest
}

func (f *fakeConnectorRunner) Run(_ context.Context, req ConnectorRunRequest) (domain.ConnectorRun, error) {
	f.got = req
	return f.run, f.err
}

func createViewer(t *testing.T, baseURL, adminToken string) sessionResponse {
	t.Helper()
	resp, payload := jsonRequest(t, http.MethodPost, baseURL+"/v1/users", map[string]any{
		"login":    viewerLogin,
		"password": viewerPassword,
		"role":     "viewer",
	}, adminToken)
	requireStatus(t, resp, payload, http.StatusCreated)
	return loginAs(t, baseURL, viewerLogin, viewerPassword)
}

func TestConnectorRunRequiresAdmin(t *testing.T) {
	runner := &fakeConnectorRunner{run: domain.ConnectorRun{
		ID: "run-1", Source: "github", Status: domain.ConnectorSuccess, StartedAt: time.Now().UTC(),
		Counts: map[string]int{"repos": 1},
	}}
	ts, _, _ := newTestServer(t, func(cfg *Config) { cfg.ConnectorRunner = runner })
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)
	viewer := createViewer(t, ts.URL, admin.AccessToken)

	body := map[string]any{"name": "github", "projects": []string{"acme/widgets"}, "limit": 10}
	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/connectors/run", body, viewer.AccessToken)
	requireStatus(t, resp, payload, http.StatusForbidden)

	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/connectors/run", body, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK)
	var run domain.ConnectorRun
	if err := json.Unmarshal(payload, &run); err != nil {
		t.Fatalf("decode run: %v; body = %s", err, payload)
	}
	if run.ID != "run-1" || run.Status != domain.ConnectorSuccess {
		t.Fatalf("run = %+v", run)
	}
	if runner.got.Name != "github" || len(runner.got.Projects) != 1 || runner.got.Limit != 10 {
		t.Fatalf("runner request = %+v", runner.got)
	}
}

func TestConnectorRunValidation(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)

	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/connectors/run", map[string]any{}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusNotImplemented)

	runner := &fakeConnectorRunner{}
	ts2, _, _ := newTestServer(t, func(cfg *Config) { cfg.ConnectorRunner = runner })
	admin2 := loginAs(t, ts2.URL, adminLogin, adminPassword)

	resp, payload = jsonRequest(t, http.MethodPost, ts2.URL+"/v1/connectors/run", map[string]any{}, admin2.AccessToken)
	requireStatus(t, resp, payload, http.StatusBadRequest)

	resp, payload = jsonRequest(t, http.MethodPost, ts2.URL+"/v1/connectors/run", map[string]any{
		"name": "github", "limit": -1,
	}, admin2.AccessToken)
	requireStatus(t, resp, payload, http.StatusBadRequest)

	resp, payload = jsonRequest(t, http.MethodPost, ts2.URL+"/v1/connectors/run", map[string]any{
		"name": "unknown",
	}, admin2.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK) // fake runner accepts names; real runner validates
}

func TestConnectorRunsListGetAndMarkdown(t *testing.T) {
	ts, _, store := newTestServer(t, nil)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)
	viewer := createViewer(t, ts.URL, admin.AccessToken)

	record, _, err := store.CreateConnectorRun(context.Background(), domain.ConnectorRun{
		Source: "github", Status: domain.ConnectorRunning, StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateConnectorRun: %v", err)
	}
	finished, _, err := store.FinishConnectorRun(context.Background(), record.ID, domain.ConnectorSuccess, map[string]int{"repos": 1}, "", 0)
	if err != nil {
		t.Fatalf("FinishConnectorRun: %v", err)
	}

	resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/v1/connectors/runs?source=github", nil, viewer.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK)
	var list struct {
		Items []ConnectorRunRecord `json:"items"`
	}
	if err := json.Unmarshal(payload, &list); err != nil {
		t.Fatalf("decode list: %v; body = %s", err, payload)
	}
	if len(list.Items) != 1 || list.Items[0].ID != finished.ID {
		t.Fatalf("items = %+v", list.Items)
	}

	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/connectors/runs/"+finished.ID, nil, viewer.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK)
	var one ConnectorRunRecord
	if err := json.Unmarshal(payload, &one); err != nil {
		t.Fatalf("decode run: %v; body = %s", err, payload)
	}
	if one.Status != domain.ConnectorSuccess || one.Revision == 0 {
		t.Fatalf("run = %+v", one)
	}

	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/connectors/runs/"+finished.ID+"/markdown", nil, viewer.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK)
	if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/markdown") {
		t.Fatalf("Content-Type = %q", contentType)
	}
	for _, want := range []string{"# Connector run: github", "## Counts", "repos: 1", "status: `success`"} {
		if !strings.Contains(string(payload), want) {
			t.Fatalf("markdown missing %q:\n%s", want, payload)
		}
	}

	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/connectors/runs?status=bogus", nil, viewer.AccessToken)
	requireStatus(t, resp, payload, http.StatusBadRequest)

	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/connectors/runs/missing", nil, viewer.AccessToken)
	requireStatus(t, resp, payload, http.StatusNotFound)
}
