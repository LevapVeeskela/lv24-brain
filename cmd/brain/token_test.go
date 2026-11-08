package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/config"
)

const (
	tokenCLIBearer = "brain_cli_token_test_bearer"
	tokenCLIRaw    = "brain_freshly_minted_secret_value"
)

// tokenCLIServer fakes the /v1/tokens endpoints and records the requests.
type tokenCLIServer struct {
	mu       sync.Mutex
	requests []string
	created  map[string]any
}

func newTokenCLIServer(t *testing.T) (*tokenCLIServer, string) {
	t.Helper()
	fake := &tokenCLIServer{}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	t.Setenv("BRAIN_TOKEN", tokenCLIBearer)
	return fake, writeServerConfig(t, server.URL)
}

func (s *tokenCLIServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())
	if r.Header.Get("Authorization") != "Bearer "+tokenCLIBearer {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/tokens":
		_, _ = io.WriteString(w, `{"items":[
			{"id":"token_live","userId":"user_1","name":"ci","role":"member","prefix":"brain_abcd1234","scopes":["read"],"projectIds":["acme"],"createdAt":"2026-01-01T00:00:00Z","lastUsedAt":"2026-02-01T00:00:00Z"},
			{"id":"token_dead","userId":"user_2","name":"old","role":"viewer","prefix":"brain_efgh5678","createdAt":"2025-01-01T00:00:00Z","revokedAt":"2025-06-01T00:00:00Z"}]}`)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/tokens":
		_ = json.NewDecoder(r.Body).Decode(&s.created)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"`+tokenCLIRaw+`","apiToken":{"id":"token_new","userId":"user_1","name":"ci","role":"member","prefix":"brain_fresh","scopes":["read"],"projectIds":["acme"],"createdAt":"2026-03-01T00:00:00Z","expiresAt":"2026-03-31T00:00:00Z"}}`)
	case r.Method == http.MethodDelete && r.URL.Path == "/v1/tokens/token_live":
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"not found"}}`)
	default:
		http.NotFound(w, r)
	}
}

func (s *tokenCLIServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func TestTokenListCLI(t *testing.T) {
	fake, cfgPath := newTokenCLIServer(t)

	code, stdout, stderr := runForTest(t, "token", "list", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("token list exit = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"token_live", "token_dead", "revoked", "active", "acme", "unscoped", "all projects"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if got := fake.seen(); len(got) != 1 || got[0] != "GET /v1/tokens?all=true" {
		t.Fatalf("requests = %v", got)
	}

	code, stdout, stderr = runForTest(t, "token", "list", "--json", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("token list --json exit = %d (stderr: %s)", code, stderr)
	}
	var envelope struct {
		Schema int `json:"schema"`
		Data   []struct {
			ID        string   `json:"id"`
			Revoked   bool     `json:"revoked"`
			Status    string   `json:"status"`
			Scopes    []string `json:"scopes"`
			RevokedAt *string  `json:"revoked_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if envelope.Schema != jsonSchemaVersion || len(envelope.Data) != 2 {
		t.Fatalf("envelope = %+v", envelope)
	}
	live, dead := envelope.Data[0], envelope.Data[1]
	if live.Revoked || live.Status != "active" || dead.Scopes == nil || !dead.Revoked || dead.Status != "revoked" || dead.RevokedAt == nil {
		t.Fatalf("data = %+v", envelope.Data)
	}
}

func TestTokenCreateCLI(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantDays  float64
		wantAtGap time.Duration
	}{
		{"days", []string{"--expires", "30d"}, 30, 0},
		{"duration", []string{"--expires", "720h"}, 0, 720 * time.Hour},
		{"legacy days", []string{"--expires-in-days", "7"}, 7, 0},
		{"never", nil, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake, cfgPath := newTokenCLIServer(t)
			args := append([]string{"token", "create", "--name", "ci", "--scope", "read", "--project", "acme", "--config", cfgPath}, tc.args...)
			before := time.Now().UTC()
			code, stdout, stderr := runForTest(t, args...)
			if code != 0 {
				t.Fatalf("exit = %d (stderr: %s)", code, stderr)
			}
			if strings.Count(stdout, tokenCLIRaw) != 1 || !strings.Contains(stdout, "cannot be shown again") {
				t.Fatalf("stdout = %q", stdout)
			}
			if strings.Contains(stderr, tokenCLIRaw) {
				t.Fatalf("secret on stderr: %q", stderr)
			}
			body := fake.created
			if body["name"] != "ci" || !sameStrings(body["scopes"], "read") || !sameStrings(body["projectIds"], "acme") {
				t.Fatalf("request = %v", body)
			}
			days, hasDays := body["expiresInDays"].(float64)
			if tc.wantDays > 0 && days != tc.wantDays || tc.wantDays == 0 && hasDays {
				t.Fatalf("expiresInDays = %v, want %v", body["expiresInDays"], tc.wantDays)
			}
			at, hasAt := body["expiresAt"].(string)
			if tc.wantAtGap == 0 {
				if hasAt {
					t.Fatalf("unexpected expiresAt %q", at)
				}
				return
			}
			parsed, err := time.Parse(time.RFC3339, at)
			if err != nil {
				t.Fatalf("expiresAt %q: %v", at, err)
			}
			if gap := parsed.Sub(before); gap < tc.wantAtGap-time.Second || gap > tc.wantAtGap+time.Minute {
				t.Fatalf("expiresAt %s is %s from now, want %s", at, gap, tc.wantAtGap)
			}
		})
	}
}

func sameStrings(value any, want ...string) bool {
	items, ok := value.([]any)
	if !ok || len(items) != len(want) {
		return false
	}
	for i, item := range items {
		if item != want[i] {
			return false
		}
	}
	return true
}

func TestTokenCreateJSONCLI(t *testing.T) {
	_, cfgPath := newTokenCLIServer(t)
	code, stdout, stderr := runForTest(t, "token", "create", "--name", "ci", "--json", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	var envelope struct {
		Schema int `json:"schema"`
		Data   struct {
			Token    string `json:"token"`
			Notice   string `json:"notice"`
			APIToken struct {
				ID        string  `json:"id"`
				ExpiresAt *string `json:"expires_at"`
			} `json:"api_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if envelope.Schema != jsonSchemaVersion || envelope.Data.Token != tokenCLIRaw || envelope.Data.APIToken.ID != "token_new" ||
		envelope.Data.APIToken.ExpiresAt == nil || envelope.Data.Notice == "" {
		t.Fatalf("envelope = %+v", envelope)
	}
}

func TestTokenCreateRejectsBadArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no name", nil},
		{"bad expires", []string{"--name", "ci", "--expires", "soon"}},
		{"zero days", []string{"--name", "ci", "--expires", "0d"}},
		{"too long", []string{"--name", "ci", "--expires", "3651d"}},
		{"negative duration", []string{"--name", "ci", "--expires", "-1h"}},
		{"both expiry flags", []string{"--name", "ci", "--expires", "1d", "--expires-in-days", "2"}},
		{"bad scope", []string{"--name", "ci", "--scope", "root"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake, cfgPath := newTokenCLIServer(t)
			code, _, _ := runForTest(t, append([]string{"token", "create", "--config", cfgPath}, tc.args...)...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2", code)
			}
			if got := fake.seen(); len(got) != 0 {
				t.Fatalf("requests = %v, want none", got)
			}
		})
	}
}

func TestTokenRevokeCLI(t *testing.T) {
	fake, cfgPath := newTokenCLIServer(t)
	code, stdout, stderr := runForTest(t, "token", "revoke", "token_live", "--config", cfgPath)
	if code != 0 || !strings.Contains(stdout, "revoked token token_live") {
		t.Fatalf("exit = %d stdout = %q stderr = %q", code, stdout, stderr)
	}
	code, _, stderr = runForTest(t, "token", "revoke", "token_missing", "--config", cfgPath)
	if code != 1 || !strings.Contains(stderr, `no token "token_missing"`) {
		t.Fatalf("missing: exit = %d stderr = %q", code, stderr)
	}
	code, _, _ = runForTest(t, "token", "revoke", "--config", cfgPath)
	if code != 2 {
		t.Fatalf("no id: exit = %d, want 2", code)
	}
	want := []string{"DELETE /v1/tokens/token_live", "DELETE /v1/tokens/token_missing"}
	if got := fake.seen(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

func TestTokenRefusesLocalMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brain.yaml")
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	if cfg.Mode == config.ModeServer {
		t.Fatal("default config is server mode")
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	for _, args := range [][]string{
		{"token", "list"},
		{"token", "create", "--name", "ci"},
		{"token", "revoke", "token_1"},
	} {
		code, stdout, stderr := runForTest(t, append(args, "--config", path)...)
		if code != 2 || !strings.Contains(stderr, "only in server mode") || stdout != "" {
			t.Fatalf("%v: exit = %d stdout = %q stderr = %q", args, code, stdout, stderr)
		}
	}
}
