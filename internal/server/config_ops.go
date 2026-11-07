package server

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	defaultDatabaseMaxConns         int32         = 10
	defaultDatabaseStatementTimeout time.Duration = 30 * time.Second
	defaultMetricsAddr                            = "127.0.0.1:9090"
	defaultAuditRetention                         = 180 * 24 * time.Hour
	defaultRevisionRetention                      = 90 * 24 * time.Hour
	defaultRetentionInterval                      = 6 * time.Hour

	maxDatabaseMaxConns         = 200
	minDatabaseStatementTimeout = time.Second
	maxDatabaseStatementTimeout = time.Hour
	minRetentionWindow          = 24 * time.Hour
	maxRetentionWindow          = 3650 * 24 * time.Hour
	minRetentionInterval        = time.Minute
	maxRetentionInterval        = 7 * 24 * time.Hour
)

// validateOperations checks the database pool, metrics and retention settings.
// Every error names the environment variable so operators can fix it quickly.
func (c Config) validateOperations() error {
	if c.DatabaseMaxConns < 1 || c.DatabaseMaxConns > maxDatabaseMaxConns {
		return fmt.Errorf("server: BRAIN_DATABASE_MAX_CONNS must be between 1 and %d", maxDatabaseMaxConns)
	}
	if c.DatabaseStatementTimeout != 0 &&
		(c.DatabaseStatementTimeout < minDatabaseStatementTimeout || c.DatabaseStatementTimeout > maxDatabaseStatementTimeout) {
		return errors.New("server: BRAIN_DATABASE_STATEMENT_TIMEOUT must be 0 (disabled) or between 1s and 1h")
	}
	if c.MetricsEnabled {
		if err := validateListenAddr(c.MetricsAddr); err != nil {
			return fmt.Errorf("server: BRAIN_METRICS_ADDR: %w", err)
		}
	}
	if c.AuditRetention < minRetentionWindow || c.AuditRetention > maxRetentionWindow {
		return errors.New("server: BRAIN_AUDIT_RETENTION must be between 1d and 3650d")
	}
	if c.RevisionRetention < minRetentionWindow || c.RevisionRetention > maxRetentionWindow {
		return errors.New("server: BRAIN_REVISION_RETENTION must be between 1d and 3650d")
	}
	if c.RetentionInterval < minRetentionInterval || c.RetentionInterval > maxRetentionInterval {
		return errors.New("server: BRAIN_RETENTION_INTERVAL must be between 1m and 7d")
	}
	return nil
}

func validateListenAddr(addr string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return fmt.Errorf("%q must be host:port", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port %q must be between 1 and 65535", port)
	}
	if host != "" && host != "localhost" && net.ParseIP(host) == nil {
		return fmt.Errorf("host %q must be an IP address or localhost", host)
	}
	return nil
}

// applyOperationsEnvironment reads the database pool, metrics and retention
// variables. Empty values keep the defaults.
func applyOperationsEnvironment(cfg *Config, getenv func(string) string) error {
	maxConns, err := envInt(getenv, "BRAIN_DATABASE_MAX_CONNS", int(cfg.DatabaseMaxConns))
	if err != nil {
		return err
	}
	if maxConns < 1 || maxConns > maxDatabaseMaxConns {
		return fmt.Errorf("server: BRAIN_DATABASE_MAX_CONNS must be between 1 and %d", maxDatabaseMaxConns)
	}
	cfg.DatabaseMaxConns = int32(maxConns)
	if cfg.DatabaseStatementTimeout, err = envDuration(getenv, "BRAIN_DATABASE_STATEMENT_TIMEOUT", cfg.DatabaseStatementTimeout); err != nil {
		return err
	}
	if cfg.MetricsEnabled, err = envBool(getenv, "BRAIN_METRICS_ENABLED", cfg.MetricsEnabled); err != nil {
		return err
	}
	if value := strings.TrimSpace(getenv("BRAIN_METRICS_ADDR")); value != "" {
		cfg.MetricsAddr = value
	}
	if cfg.RetentionDisabled, err = envBool(getenv, "BRAIN_RETENTION_DISABLED", cfg.RetentionDisabled); err != nil {
		return err
	}
	if cfg.AuditRetention, err = envRetention(getenv, "BRAIN_AUDIT_RETENTION", cfg.AuditRetention); err != nil {
		return err
	}
	if cfg.RevisionRetention, err = envRetention(getenv, "BRAIN_REVISION_RETENTION", cfg.RevisionRetention); err != nil {
		return err
	}
	if cfg.RetentionInterval, err = envDuration(getenv, "BRAIN_RETENTION_INTERVAL", cfg.RetentionInterval); err != nil {
		return err
	}
	return nil
}

// envRetention accepts a Go duration ("72h") or a whole number of days ("180d").
func envRetention(getenv func(string) string, name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback, nil
	}
	if days, ok := strings.CutSuffix(value, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("server: %s: %q must be a positive number of days (for example 180d)", name, value)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("server: %s: %w", name, err)
	}
	return parsed, nil
}
