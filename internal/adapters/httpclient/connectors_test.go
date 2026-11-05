package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

type connectorDeadlineTransport struct{ t *testing.T }

func (transport connectorDeadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	deadline, ok := r.Context().Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining < time.Hour || remaining > time.Hour+time.Minute {
		transport.t.Errorf("connector request timeout = %s, want one hour plus persistence allowance", remaining)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"run-long","source":"github","status":"success"}`))}, nil
}

func TestClientRunConnectorAllowsServerTimeout(t *testing.T) {
	previous := http.DefaultTransport
	http.DefaultTransport = connectorDeadlineTransport{t: t}
	t.Cleanup(func() { http.DefaultTransport = previous })
	client, err := New(Config{BaseURL: "http://brain.example.test", Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.RunConnector(context.Background(), "github", nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if client.httpClient.Timeout != DefaultTimeout {
		t.Fatalf("ordinary/auth timeout = %s, want %s", client.httpClient.Timeout, DefaultTimeout)
	}
}

func TestClientRunConnectorRespectsExplicitTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	t.Cleanup(server.Close)
	for _, cfg := range []Config{
		{BaseURL: server.URL, Timeout: 20 * time.Millisecond},
		{BaseURL: server.URL, HTTPClient: &http.Client{Timeout: 20 * time.Millisecond}},
	} {
		client, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.RunConnector(context.Background(), "github", nil, nil, 0); !errors.Is(err, ErrTimeout) {
			t.Fatalf("RunConnector error = %v, want ErrTimeout", err)
		}
	}
}

func TestClientRunConnectorRefreshRetriesSameRequest(t *testing.T) {
	var requests int
	var refreshes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var got connectorRunRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if got.Name != "gitlab" || got.Limit != 7 || len(got.Projects) != 1 || got.Projects[0] != "team/repo" {
			t.Errorf("request = %+v", got)
		}
		if requests == 1 {
			if r.Header.Get("Authorization") != "Bearer expired" {
				t.Error("first token mismatch")
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fresh" {
			t.Error("retry token mismatch")
		}
		_, _ = io.WriteString(w, `{"id":"run-refreshed","source":"gitlab","status":"success"}`)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{BaseURL: server.URL, Token: "expired", Refresh: func(context.Context) (string, error) {
		refreshes++
		return "fresh", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := client.RunConnector(context.Background(), "gitlab", []string{"team/repo"}, nil, 7)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || refreshes != 1 || run.ID != "run-refreshed" {
		t.Fatalf("requests=%d refreshes=%d run=%+v", requests, refreshes, run)
	}
}

func TestClientRunConnector(t *testing.T) {
	const token = "brain_test_token"
	var got connectorRunRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/connectors/run" {
			http.NotFound(w, r)
			return
		}
		if gotToken := r.Header.Get("Authorization"); gotToken != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(domain.ConnectorRun{
			ID:        "run-1",
			Source:    "github",
			Status:    domain.ConnectorSuccess,
			StartedAt: time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC),
			Counts:    map[string]int{"repos": 1},
		})
	}))
	t.Cleanup(server.Close)

	client, err := New(Config{BaseURL: server.URL, Token: token})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	run, err := client.RunConnector(context.Background(), "github", []string{"acme/widgets"}, &since, 10)
	if err != nil {
		t.Fatalf("RunConnector: %v", err)
	}
	if run.ID != "run-1" || run.Status != domain.ConnectorSuccess || run.Counts["repos"] != 1 {
		t.Fatalf("run = %+v", run)
	}
	if got.Name != "github" || len(got.Projects) != 1 || got.Limit != 10 || got.Since == nil {
		t.Fatalf("request = %+v", got)
	}
}

func TestClientRunConnectorReturnsFailedRunAsValue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(domain.ConnectorRun{
			ID: "run-2", Source: "imap", Status: domain.ConnectorFailed,
			Error: "imap: dial failed",
		})
	}))
	t.Cleanup(server.Close)

	client, err := New(Config{BaseURL: server.URL, Token: "brain_test_token"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	run, err := client.RunConnector(context.Background(), "imap", nil, nil, 0)
	if err != nil {
		t.Fatalf("RunConnector: %v", err)
	}
	if run.Status != domain.ConnectorFailed || run.Error == "" {
		t.Fatalf("run = %+v", run)
	}
}
