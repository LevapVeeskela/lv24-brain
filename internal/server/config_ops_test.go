package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
)

func opsEnv(extra map[string]string) func(string) string {
	env := map[string]string{"BRAIN_JWT_SECRET": strings.Repeat("s", 32)}
	for key, value := range extra {
		env[key] = value
	}
	return func(key string) string { return env[key] }
}

func TestLoadConfigOperationsDefaults(t *testing.T) {
	cfg, err := LoadConfig(nil, opsEnv(nil))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DatabaseMaxConns != 10 {
		t.Fatalf("max conns = %d, want 10", cfg.DatabaseMaxConns)
	}
	if cfg.DatabaseStatementTimeout != 30*time.Second {
		t.Fatalf("statement timeout = %s, want 30s", cfg.DatabaseStatementTimeout)
	}
	if cfg.MetricsEnabled || cfg.MetricsAddr != "127.0.0.1:9090" {
		t.Fatalf("metrics = enabled %t addr %q; want disabled on 127.0.0.1:9090", cfg.MetricsEnabled, cfg.MetricsAddr)
	}
	if cfg.RetentionDisabled {
		t.Fatal("retention must be enabled by default")
	}
	if cfg.AuditRetention != 180*24*time.Hour || cfg.RevisionRetention != 90*24*time.Hour {
		t.Fatalf("retention = audit %s revision %s", cfg.AuditRetention, cfg.RevisionRetention)
	}
	if cfg.RetentionInterval != 6*time.Hour {
		t.Fatalf("retention interval = %s, want 6h", cfg.RetentionInterval)
	}
}

func TestLoadConfigOperationsOverrides(t *testing.T) {
	cfg, err := LoadConfig(nil, opsEnv(map[string]string{
		"BRAIN_DATABASE_MAX_CONNS":         "4",
		"BRAIN_DATABASE_STATEMENT_TIMEOUT": "0",
		"BRAIN_METRICS_ENABLED":            "1",
		"BRAIN_METRICS_ADDR":               "127.0.0.1:9191",
		"BRAIN_RETENTION_DISABLED":         "1",
		"BRAIN_AUDIT_RETENTION":            "30d",
		"BRAIN_REVISION_RETENTION":         "72h0m0s",
		"BRAIN_RETENTION_INTERVAL":         "2h",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DatabaseMaxConns != 4 || cfg.DatabaseStatementTimeout != 0 {
		t.Fatalf("pool = %d conns, timeout %s", cfg.DatabaseMaxConns, cfg.DatabaseStatementTimeout)
	}
	if !cfg.MetricsEnabled || cfg.MetricsAddr != "127.0.0.1:9191" {
		t.Fatalf("metrics = %t %q", cfg.MetricsEnabled, cfg.MetricsAddr)
	}
	if !cfg.RetentionDisabled || cfg.AuditRetention != 30*24*time.Hour {
		t.Fatalf("retention disabled=%t audit=%s", cfg.RetentionDisabled, cfg.AuditRetention)
	}
	if cfg.RevisionRetention != 72*time.Hour || cfg.RetentionInterval != 2*time.Hour {
		t.Fatalf("revision=%s interval=%s", cfg.RevisionRetention, cfg.RetentionInterval)
	}
}

func TestLoadConfigRejectsInvalidOperationsSettings(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"max conns zero", map[string]string{"BRAIN_DATABASE_MAX_CONNS": "0"}, "BRAIN_DATABASE_MAX_CONNS"},
		{"max conns above limit", map[string]string{"BRAIN_DATABASE_MAX_CONNS": "201"}, "BRAIN_DATABASE_MAX_CONNS"},
		{"max conns overflow", map[string]string{"BRAIN_DATABASE_MAX_CONNS": "4294967301"}, "BRAIN_DATABASE_MAX_CONNS"},
		{"max conns not a number", map[string]string{"BRAIN_DATABASE_MAX_CONNS": "many"}, "BRAIN_DATABASE_MAX_CONNS"},
		{"statement timeout too small", map[string]string{"BRAIN_DATABASE_STATEMENT_TIMEOUT": "500ms"}, "BRAIN_DATABASE_STATEMENT_TIMEOUT"},
		{"statement timeout too large", map[string]string{"BRAIN_DATABASE_STATEMENT_TIMEOUT": "2h"}, "BRAIN_DATABASE_STATEMENT_TIMEOUT"},
		{"metrics addr without port", map[string]string{"BRAIN_METRICS_ENABLED": "1", "BRAIN_METRICS_ADDR": "127.0.0.1"}, "BRAIN_METRICS_ADDR"},
		{"metrics port out of range", map[string]string{"BRAIN_METRICS_ENABLED": "1", "BRAIN_METRICS_ADDR": "127.0.0.1:99999"}, "BRAIN_METRICS_ADDR"},
		{"metrics host is a name", map[string]string{"BRAIN_METRICS_ENABLED": "1", "BRAIN_METRICS_ADDR": "example.com:9090"}, "BRAIN_METRICS_ADDR"},
		{"metrics enabled flag invalid", map[string]string{"BRAIN_METRICS_ENABLED": "sometimes"}, "BRAIN_METRICS_ENABLED"},
		{"audit retention zero days", map[string]string{"BRAIN_AUDIT_RETENTION": "0d"}, "BRAIN_AUDIT_RETENTION"},
		{"audit retention below one day", map[string]string{"BRAIN_AUDIT_RETENTION": "12h"}, "BRAIN_AUDIT_RETENTION"},
		{"audit retention malformed", map[string]string{"BRAIN_AUDIT_RETENTION": "-5d"}, "BRAIN_AUDIT_RETENTION"},
		{"revision retention too long", map[string]string{"BRAIN_REVISION_RETENTION": "99999d"}, "BRAIN_REVISION_RETENTION"},
		{"interval too short", map[string]string{"BRAIN_RETENTION_INTERVAL": "10s"}, "BRAIN_RETENTION_INTERVAL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadConfig(nil, opsEnv(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestMetricsAddressIgnoredWhileDisabled(t *testing.T) {
	cfg, err := LoadConfig(nil, opsEnv(map[string]string{"BRAIN_METRICS_ADDR": "not-an-address"}))
	if err != nil {
		t.Fatalf("disabled metrics must not validate the address: %v", err)
	}
	if cfg.MetricsEnabled {
		t.Fatal("metrics must stay disabled unless BRAIN_METRICS_ENABLED is set")
	}
}

func TestParseRetentionDuration(t *testing.T) {
	getenv := func(value string) func(string) string {
		return func(string) string { return value }
	}
	if got, err := envRetention(getenv("180d"), "X", 0); err != nil || got != 180*24*time.Hour {
		t.Fatalf("180d = %s, %v", got, err)
	}
	if got, err := envRetention(getenv("48h"), "X", 0); err != nil || got != 48*time.Hour {
		t.Fatalf("48h = %s, %v", got, err)
	}
	if got, err := envRetention(getenv(""), "X", time.Hour); err != nil || got != time.Hour {
		t.Fatalf("empty must keep fallback: %s, %v", got, err)
	}
}

func TestStartMetricsServesOnlyWhenEnabled(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()

	disabled := &Server{cfg: Config{MetricsAddr: addr}, logger: slog.Default()}
	if srv, err := disabled.startMetrics(); err != nil || srv != nil {
		t.Fatalf("disabled metrics: srv=%v err=%v", srv, err)
	}

	enabled := &Server{
		cfg:     Config{MetricsAddr: addr, ShutdownTimeout: time.Second},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		metrics: httpapi.NewMetrics(httpapi.BuildInfo{Version: "t", Commit: "c", Go: "go"}),
	}
	srv, err := enabled.startMetrics()
	if err != nil {
		t.Fatalf("startMetrics: %v", err)
	}
	defer enabled.shutdownHTTP(srv)

	if _, err := enabled.startMetrics(); err == nil || !strings.Contains(err.Error(), "listen for metrics") {
		t.Fatalf("second bind on %s must fail fast, got %v", addr, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/metrics", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "brain_build_info") {
		t.Fatalf("metrics response = %d %q", resp.StatusCode, body)
	}
}
