package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/version"
)

func TestHealthReadyVersion(t *testing.T) {
	handler := Handler()
	tests := []struct {
		path string
		want string
	}{
		{path: "/health", want: "ok"},
		{path: "/ready", want: "ready"},
		{path: "/version", want: version.Version},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			if !strings.Contains(recorder.Body.String(), tt.want) {
				t.Fatalf("body = %q, want %q", recorder.Body.String(), tt.want)
			}
			var payload map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
		})
	}
}

func TestHealthRejectsPost(t *testing.T) {
	recorder := httptest.NewRecorder()
	Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/health", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
	if allow := recorder.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Fatalf("Allow = %q, want GET", allow)
	}
}

func TestLoadConfigRequiresSecret(t *testing.T) {
	env := map[string]string{}
	_, err := LoadConfig(nil, func(key string) string { return env[key] })
	if err == nil || !strings.Contains(err.Error(), "BRAIN_JWT_SECRET") {
		t.Fatalf("error = %v, want missing secret", err)
	}
	env["BRAIN_JWT_SECRET"] = strings.Repeat("s", 32)
	cfg, err := LoadConfig(nil, func(key string) string { return env[key] })
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Addr != defaultAddr {
		t.Fatalf("addr = %q, want %q", cfg.Addr, defaultAddr)
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	env := map[string]string{
		"BRAIN_JWT_SECRET":  strings.Repeat("s", 32),
		"BRAIN_ACCESS_TTL":  "nonsense",
		"BRAIN_REFRESH_TTL": "1h",
	}
	if _, err := LoadConfig(nil, func(key string) string { return env[key] }); err == nil {
		t.Fatal("LoadConfig() succeeded with an invalid duration")
	}
	env["BRAIN_ACCESS_TTL"] = "15m"
	env["BRAIN_REFRESH_TTL"] = "10m"
	if _, err := LoadConfig(nil, func(key string) string { return env[key] }); err == nil {
		t.Fatal("LoadConfig() succeeded with refresh ttl below access ttl")
	}
}
