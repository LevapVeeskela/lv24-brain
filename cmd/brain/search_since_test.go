package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSearchSinceCLI(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}
	if code, _, stderr := runForTest(t, "note", "--create", "--config", cfgPath, "--project", "demo", "--title", "Since note", "--body", "one decision recorded"); code != 0 {
		t.Fatalf("note exit code = %d (stderr: %s)", code, stderr)
	}

	code, stdout, stderr := runForTest(t, "search", "--config", cfgPath, "--since", "7d", "decisions")
	if code != 0 || !strings.Contains(stdout, "Since note") {
		t.Fatalf("search --since 7d = %q, code = %d (stderr: %s)", stdout, code, stderr)
	}
	code, stdout, stderr = runForTest(t, "search", "decisions", "--since", "2099-01-01", "--config", cfgPath)
	if code != 0 || stdout != "" {
		t.Fatalf("search --since future = %q, code = %d (stderr: %s)", stdout, code, stderr)
	}

	for _, bad := range []string{"soon", "0d", "-24h", "2026-13-40"} {
		code, _, stderr = runForTest(t, "search", "--config", cfgPath, "--since", bad, "decision")
		if code != 2 || !strings.Contains(stderr, "--since") || !strings.Contains(stderr, "2026-11-01") {
			t.Fatalf("search --since %q code = %d, stderr = %q; want 2 and a clear message", bad, code, stderr)
		}
	}
}

func TestSearchSinceServerModeCLI(t *testing.T) {
	const token = "brain_server_since_test"
	var gotSince string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/search" || r.Header.Get("Authorization") != "Bearer "+token {
			http.NotFound(w, r)
			return
		}
		gotSince = r.URL.Query().Get("since")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[]}`)
	}))
	t.Cleanup(server.Close)
	t.Setenv("BRAIN_TOKEN", token)

	cfgPath := writeServerConfig(t, server.URL)
	if code, _, stderr := runForTest(t, "search", "release", "--since", "2026-11-01", "--config", cfgPath); code != 0 {
		t.Fatalf("search exit code = %d (stderr: %s)", code, stderr)
	}
	parsed, err := time.Parse(time.RFC3339Nano, gotSince)
	if err != nil {
		t.Fatalf("server since = %q, want RFC3339: %v", gotSince, err)
	}
	want := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.Local)
	if !parsed.Equal(want) {
		t.Fatalf("server since = %v, want %v", parsed, want)
	}
}
