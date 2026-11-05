package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/adapters/postgres"
	"github.com/levapveeskela/lv24-brain/internal/adapters/serverstore"
	"github.com/levapveeskela/lv24-brain/internal/auth"
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
	if cfg.PersistenceBackend() != "memory" {
		t.Fatalf("backend = %q, want memory", cfg.PersistenceBackend())
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

func TestLoadConfigDatabaseSettings(t *testing.T) {
	env := map[string]string{
		"BRAIN_JWT_SECRET":                strings.Repeat("s", 32),
		"BRAIN_DATABASE_URL":              " postgres://brain:top-secret@db.example:5432/brain?sslmode=disable ",
		"BRAIN_DATABASE_TIMEOUT":          "45s",
		"BRAIN_DATABASE_BACKUP_CONFIRMED": "true",
	}
	cfg, err := LoadConfig(nil, func(key string) string { return env[key] })
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.DatabaseURL != "postgres://brain:top-secret@db.example:5432/brain?sslmode=disable" {
		t.Fatalf("database url = %q", cfg.DatabaseURL)
	}
	if cfg.DatabaseTimeout != 45*time.Second {
		t.Fatalf("database timeout = %v, want 45s", cfg.DatabaseTimeout)
	}
	if !cfg.DatabaseBackupConfirmed {
		t.Fatal("database backup confirmation was not parsed")
	}
	if cfg.PersistenceBackend() != "postgres" {
		t.Fatalf("backend = %q, want postgres", cfg.PersistenceBackend())
	}

	redacted := redactDatabaseURL(cfg.DatabaseURL)
	if strings.Contains(redacted, "top-secret") {
		t.Fatalf("redacted URL leaked the password: %q", redacted)
	}
	if strings.Contains(redacted, "sslmode") {
		t.Fatalf("redacted URL leaked query parameters: %q", redacted)
	}
	if !strings.Contains(redacted, "db.example") || !strings.Contains(redacted, "/brain") {
		t.Fatalf("redacted URL lost its destination: %q", redacted)
	}

	delete(env, "BRAIN_DATABASE_URL")
	env["DATABASE_URL"] = "postgresql://brain:top-secret@localhost:5432/brain"
	fallback, err := LoadConfig(nil, func(key string) string { return env[key] })
	if err != nil {
		t.Fatalf("LoadConfig() with DATABASE_URL fallback: %v", err)
	}
	if fallback.DatabaseURL != env["DATABASE_URL"] {
		t.Fatalf("fallback database url = %q", fallback.DatabaseURL)
	}
}

func TestLoadConfigRejectsInvalidDatabaseSettings(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		want  string
		leaks string
	}{
		{
			name:  "unsupported scheme",
			env:   map[string]string{"BRAIN_DATABASE_URL": "mysql://brain:very-secret@db.example:3306/brain"},
			want:  "postgres",
			leaks: "very-secret",
		},
		{
			name:  "missing host",
			env:   map[string]string{"BRAIN_DATABASE_URL": "postgres://brain:very-secret@"},
			want:  "host",
			leaks: "very-secret",
		},
		{
			name: "database timeout below minimum",
			env:  map[string]string{"BRAIN_DATABASE_TIMEOUT": "500ms"},
			want: "database timeout",
		},
		{
			name: "invalid backup confirmation",
			env:  map[string]string{"BRAIN_DATABASE_BACKUP_CONFIRMED": "sometimes"},
			want: "BRAIN_DATABASE_BACKUP_CONFIRMED",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"BRAIN_JWT_SECRET": strings.Repeat("s", 32)}
			for key, value := range tt.env {
				env[key] = value
			}
			_, err := LoadConfig(nil, func(key string) string { return env[key] })
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
			if tt.leaks != "" && strings.Contains(err.Error(), tt.leaks) {
				t.Fatalf("error leaked the database URL: %v", err)
			}
		})
	}
}

func TestRedactDatabaseURL(t *testing.T) {
	if got := redactDatabaseURL(""); got != "" {
		t.Fatalf("empty DSN = %q", got)
	}
	if got := redactDatabaseURL("host=db user=brain password=secret"); got != "<redacted>" {
		t.Fatalf("keyword DSN = %q, want <redacted>", got)
	}
	if got := redactDatabaseURL("postgres://brain:secret@db.example:5432/brain?sslmode=disable"); got != "postgres://db.example:5432/brain" {
		t.Fatalf("redacted DSN = %q", got)
	}
}

func TestNewUsesMemoryStoreWithoutDatabase(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := DefaultConfig()
	cfg.DatabaseURL = ""

	srv, err := New(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, ok := srv.store.(*httpapi.MemoryStore); !ok {
		t.Fatalf("store = %T, want *httpapi.MemoryStore", srv.store)
	}
	if _, ok := srv.store.(*serverstore.Store); ok {
		t.Fatal("memory mode must not use the Postgres store")
	}
	lowered := strings.ToLower(logs.String())
	if !strings.Contains(lowered, "in-memory") || !strings.Contains(lowered, "development mode") {
		t.Fatalf("startup log does not warn about the in-memory store: %s", logs.String())
	}

	recorder := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want 200", recorder.Code)
	}

	// Close is idempotent and safe for the memory backend.
	if err := srv.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestServerPostgresWiring(t *testing.T) {
	baseURL := os.Getenv(postgres.TestDatabaseURLEnv)
	if baseURL == "" {
		t.Skipf("%s is not set; skipping PostgreSQL integration test", postgres.TestDatabaseURLEnv)
	}
	databaseURL := isolatedDatabaseURL(t, baseURL)

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := postgresTestConfig(databaseURL)

	srv, err := New(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("New() with Postgres error = %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	if _, ok := srv.store.(*serverstore.Store); !ok {
		t.Fatalf("store = %T, want *serverstore.Store", srv.store)
	}
	if !strings.Contains(logs.String(), "postgres persistence enabled") {
		t.Fatalf("startup log does not mention Postgres: %s", logs.String())
	}
	assertPasswordNotLogged(t, baseURL, logs.String())

	recorder := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}

	token := loginViaHandler(t, srv.Handler(), cfg.BootstrapLogin, cfg.BootstrapPassword)
	createProjectViaHandler(t, srv.Handler(), token, "p1")
}

func TestServerPostgresMigrationGate(t *testing.T) {
	baseURL := os.Getenv(postgres.TestDatabaseURLEnv)
	if baseURL == "" {
		t.Skipf("%s is not set; skipping PostgreSQL integration test", postgres.TestDatabaseURLEnv)
	}
	databaseURL := isolatedDatabaseURL(t, baseURL)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := postgresTestConfig(databaseURL)

	srv, err := New(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("initial New() error = %v", err)
	}
	pgStore, ok := srv.store.(*serverstore.Store)
	if !ok {
		t.Fatalf("store = %T, want *serverstore.Store", srv.store)
	}
	if _, err := pgStore.Postgres().Pool().Exec(
		context.Background(),
		`DELETE FROM brain_schema_migrations WHERE version = 1`,
	); err != nil {
		t.Fatalf("remove applied migration: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if _, err := New(context.Background(), cfg, logger); err == nil ||
		!strings.Contains(err.Error(), "BRAIN_DATABASE_BACKUP_CONFIRMED") {
		t.Fatalf("New() without backup confirmation error = %v", err)
	}

	cfg.DatabaseBackupConfirmed = true
	confirmed, err := New(context.Background(), cfg, logger)
	if err != nil {
		t.Fatalf("New() with backup confirmation error = %v", err)
	}
	confirmedStore := confirmed.store.(*serverstore.Store)
	if _, err := confirmedStore.Postgres().Pool().Exec(
		context.Background(),
		`INSERT INTO brain_schema_migrations (version, name, checksum) VALUES (9999, 'future', 'future')`,
	); err != nil {
		t.Fatalf("insert future migration: %v", err)
	}
	if err := confirmed.Close(); err != nil {
		t.Fatalf("Close() after confirmation error = %v", err)
	}

	if _, err := New(context.Background(), cfg, logger); err == nil ||
		!strings.Contains(err.Error(), "newer than this engine") {
		t.Fatalf("New() with future schema error = %v", err)
	}
}

func postgresTestConfig(databaseURL string) Config {
	cfg := DefaultConfig()
	cfg.DatabaseURL = databaseURL
	cfg.BootstrapLogin = "bootstrap@example.test"
	cfg.BootstrapPassword = "bootstrap-password-1"
	cfg.PasswordParams = auth.Argon2Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1}
	cfg.AccessTTL = 5 * time.Minute
	cfg.RefreshTTL = time.Hour
	return cfg
}

// isolatedDatabaseURL creates a throwaway schema and points the server at it
// through the pgx search_path runtime parameter, so integration tests never
// share tables.
func isolatedDatabaseURL(t *testing.T, baseURL string) string {
	t.Helper()
	adminCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminPool, err := pgxpool.New(adminCtx, baseURL)
	if err != nil {
		t.Fatal("open test admin pool failed")
	}
	if err := adminPool.Ping(adminCtx); err != nil {
		adminPool.Close()
		t.Fatalf("ping test database: %v", err)
	}

	schema := fmt.Sprintf("brain_server_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(adminCtx, "CREATE SCHEMA "+quotedSchema); err != nil {
		adminPool.Close()
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Logf("drop test schema %s: %v", schema, err)
		}
		adminPool.Close()
	})

	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" {
		return baseURL + " search_path=" + schema
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func loginViaHandler(t *testing.T, handler http.Handler, login, password string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"login": login, "password": password})
	if err != nil {
		t.Fatalf("encode login: %v", err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var session struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &session); err != nil || session.AccessToken == "" {
		t.Fatalf("decode login response = %s (%v)", recorder.Body.String(), err)
	}
	return session.AccessToken
}

func createProjectViaHandler(t *testing.T, handler http.Handler, token, id string) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"id": id, "name": "Project " + id, "kind": "app"})
	if err != nil {
		t.Fatalf("encode project: %v", err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/projects", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create project status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func assertPasswordNotLogged(t *testing.T, databaseURL, logs string) {
	t.Helper()
	parsed, err := url.Parse(databaseURL)
	if err != nil || parsed.User == nil {
		return
	}
	password, ok := parsed.User.Password()
	if !ok || len(password) < 4 {
		return
	}
	if strings.Contains(logs, password) {
		t.Fatal("startup logs leaked the database password")
	}
}
