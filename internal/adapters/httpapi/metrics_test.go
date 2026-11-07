package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func scrapeMetrics(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("content type = %q", ct)
	}
	return rec.Body.String()
}

func TestMetricsDisabledByDefault(t *testing.T) {
	ts, api, _ := newTestServer(t, nil)
	if api.metrics != nil {
		t.Fatal("metrics collector must be nil unless configured")
	}
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /metrics status = %d, want 404 on the public listener", resp.StatusCode)
	}
}

func TestMetricsExposeRoutePatternsNotRawPaths(t *testing.T) {
	metrics := NewMetrics(BuildInfo{Version: "v-test", Commit: "abc123", Go: "go1.27"})
	metrics.SetDBStats(func() DBStats { return DBStats{Acquired: 2, Idle: 1, Total: 3, Max: 10} })
	ts, _, _ := newTestServer(t, func(cfg *Config) { cfg.Metrics = metrics })

	for _, path := range []string{"/health", "/v1/projects/secret-project-id", "/no/such/path"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	body := scrapeMetrics(t, metrics)
	for _, want := range []string{
		`# TYPE http_requests_total counter`,
		`http_requests_total{method="GET",route="/health",code="200"} 1`,
		`http_requests_total{method="GET",route="/v1/projects/{id}",code="401"} 1`,
		`http_requests_total{method="GET",route="unmatched",code="404"} 1`,
		`# TYPE http_request_duration_seconds histogram`,
		`http_request_duration_seconds_bucket{method="GET",route="/health",le="0.005"}`,
		`http_request_duration_seconds_bucket{method="GET",route="/health",le="+Inf"} 1`,
		`http_request_duration_seconds_count{method="GET",route="/health"} 1`,
		`http_requests_in_flight 0`,
		`sse_clients 0`,
		`db_pool_acquired_connections 2`,
		`db_pool_max_connections 10`,
		`brain_build_info{version="v-test",commit="abc123",go_version="go1.27"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "secret-project-id") {
		t.Fatalf("raw path leaked into metric labels:\n%s", body)
	}
}

func TestMetricsOmitDBStatsWithoutPool(t *testing.T) {
	metrics := NewMetrics(BuildInfo{Version: "v", Commit: "c", Go: "go"})
	body := scrapeMetrics(t, metrics)
	if strings.Contains(body, "db_pool_") {
		t.Fatalf("memory-mode metrics must not include pool gauges:\n%s", body)
	}
}

func TestMetricsRejectsWritesAndEscapesLabels(t *testing.T) {
	metrics := NewMetrics(BuildInfo{})
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}

	metrics.observe("BREW", "/v1/x", 200, 0)
	body := scrapeMetrics(t, metrics)
	if !strings.Contains(body, `method="OTHER"`) {
		t.Fatalf("unknown method must collapse to OTHER:\n%s", body)
	}
	if got := escapeLabel("a\\b\"c\nd"); got != `a\\b\"c\nd` {
		t.Fatalf("escapeLabel = %q", got)
	}
}

func TestRouteLabel(t *testing.T) {
	cases := map[string]string{
		"":                    "unmatched",
		"GET /v1/notes/{id}":  "/v1/notes/{id}",
		"POST /v1/auth/login": "/v1/auth/login",
		"/health":             "/health",
	}
	for pattern, want := range cases {
		if got := routeLabel(pattern); got != want {
			t.Fatalf("routeLabel(%q) = %q, want %q", pattern, got, want)
		}
	}
}

func TestRequestLogIsStructuredAndWithoutSecrets(t *testing.T) {
	var logs bytes.Buffer
	requestLogger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ts, _, _ := newTestServer(t, func(cfg *Config) { cfg.RequestLogger = requestLogger })

	session := loginAs(t, ts.URL, adminLogin, adminPassword)
	resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/v1/auth/me", nil, session.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK)
	jsonRequest(t, http.MethodGet, ts.URL+"/health", nil, "")

	var meRecord map[string]any
	var healthSeen bool
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("request log line is not JSON: %v: %q", err, line)
		}
		if record["msg"] != "http request" {
			continue
		}
		if record["route"] == "/v1/auth/me" {
			meRecord = record
		}
		if record["route"] == "/health" {
			healthSeen = true
		}
	}
	if meRecord == nil {
		t.Fatalf("no request record for /v1/auth/me in logs:\n%s", logs.String())
	}
	for _, key := range []string{"request_id", "method", "route", "status", "duration_ms", "user_id"} {
		if _, ok := meRecord[key]; !ok {
			t.Fatalf("request record missing %q: %v", key, meRecord)
		}
	}
	if meRecord["user_id"] != session.User.ID || meRecord["status"] != float64(http.StatusOK) {
		t.Fatalf("request record = %v, want user_id %q and status 200", meRecord, session.User.ID)
	}
	if healthSeen {
		t.Fatal("health probes must log at debug level, not info")
	}
	for _, secret := range []string{session.AccessToken, session.RefreshToken, adminPassword} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("request log leaked credential material")
		}
	}
}
