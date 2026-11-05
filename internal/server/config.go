package server

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/oidc"
	"github.com/levapveeskela/lv24-brain/internal/auth"
	brainconfig "github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const (
	defaultAddr        = "127.0.0.1:8080"
	defaultIssuer      = "lv24-brain"
	defaultAudience    = "lv24-brain-api"
	minJWTSecretLength = auth.MinJWTSecretLength
)

// Config is the brain-server runtime configuration.
type Config struct {
	Addr       string
	ConfigPath string
	Notice     string

	JWTSecret string
	Issuer    string
	Audience  string

	AccessTTL       time.Duration
	RefreshTTL      time.Duration
	MaxBodyBytes    int64
	SSEHeartbeat    time.Duration
	SSERetry        time.Duration
	ShutdownTimeout time.Duration
	LoginRateLimit  int

	BootstrapLogin    string
	BootstrapPassword string

	// DatabaseURL enables durable server-mode persistence. When empty the
	// server uses the in-memory store, which is a development-only mode.
	DatabaseURL string
	// DatabaseTimeout bounds the startup health check and migrations.
	DatabaseTimeout time.Duration
	// DatabaseBackupConfirmed acknowledges that a backup exists before
	// pending migrations are applied to an existing schema.
	DatabaseBackupConfirmed bool

	OIDC            oidc.Config
	OIDCAllowSignup bool

	Now            func() time.Time
	PasswordParams auth.Argon2Params
}

// DefaultConfig returns a valid development configuration with an ephemeral
// signing key. Production callers must set BRAIN_JWT_SECRET via LoadConfig.
func DefaultConfig() Config {
	return Config{
		Addr:            defaultAddr,
		JWTSecret:       randomSecret(),
		Issuer:          defaultIssuer,
		Audience:        defaultAudience,
		AccessTTL:       15 * time.Minute,
		RefreshTTL:      30 * 24 * time.Hour,
		MaxBodyBytes:    1 << 20,
		SSEHeartbeat:    15 * time.Second,
		SSERetry:        3 * time.Second,
		ShutdownTimeout: 10 * time.Second,
		LoginRateLimit:  20,
		DatabaseTimeout: 30 * time.Second,
		OIDCAllowSignup: true,
		Now:             time.Now,
		PasswordParams:  auth.DefaultArgon2Params,
	}
}

// LoadConfig parses flags and environment. It fails fast when the signing key
// is absent or any value is invalid.
func LoadConfig(args []string, getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	fs := flag.NewFlagSet("brain-server", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	envAddr := getenv("BRAIN_ADDR")
	if envAddr == "" {
		envAddr = defaultAddr
	}
	addr := fs.String("addr", envAddr, "listen address")
	cfgPath := fs.String("config", "", "optional path to brain.yaml")
	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("parse flags: %w", err)
	}

	cfg := DefaultConfig()
	cfg.Addr = strings.TrimSpace(*addr)
	cfg.ConfigPath = strings.TrimSpace(*cfgPath)
	if cfg.ConfigPath != "" {
		instance, err := brainconfig.Load(cfg.ConfigPath)
		if err != nil {
			return Config{}, err
		}
		if err := instance.Validate(); err != nil {
			return Config{}, err
		}
		if instance.Mode != brainconfig.ModeServer {
			cfg.Notice = fmt.Sprintf("brain.yaml mode is %q, expected %q", instance.Mode, brainconfig.ModeServer)
		}
	}
	if err := applyEnvironment(&cfg, getenv); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(getenv("BRAIN_JWT_SECRET")) == "" {
		return Config{}, errors.New("BRAIN_JWT_SECRET is required (at least 32 bytes)")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	if cfg.BootstrapLogin == "" && cfg.OIDC.Issuer == "" {
		cfg.Notice = addNotice(cfg.Notice, "no bootstrap administrator and no OIDC issuer: no principal can authenticate")
	}
	return cfg, nil
}

func addNotice(existing, message string) string {
	if existing == "" {
		return message
	}
	return existing + "; " + message
}

// Validate checks the server configuration.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Addr) == "" {
		return errors.New("server: addr is required")
	}
	if len(c.JWTSecret) < minJWTSecretLength {
		return fmt.Errorf("server: JWT secret must be at least %d bytes", minJWTSecretLength)
	}
	if c.AccessTTL <= 0 {
		return errors.New("server: access ttl must be positive")
	}
	if c.RefreshTTL <= c.AccessTTL {
		return errors.New("server: refresh ttl must exceed access ttl")
	}
	if c.MaxBodyBytes < 1024 || c.MaxBodyBytes > 16<<20 {
		return errors.New("server: max body bytes must be between 1024 and 16777216")
	}
	if c.SSEHeartbeat < time.Second || c.SSEHeartbeat > 5*time.Minute {
		return errors.New("server: sse heartbeat must be between 1s and 5m")
	}
	if c.SSERetry < 100*time.Millisecond || c.SSERetry > time.Minute {
		return errors.New("server: sse retry must be between 100ms and 1m")
	}
	if c.ShutdownTimeout < time.Second || c.ShutdownTimeout > 2*time.Minute {
		return errors.New("server: shutdown timeout must be between 1s and 2m")
	}
	if c.LoginRateLimit < 0 || c.LoginRateLimit > 10000 {
		return errors.New("server: login rate limit must be between 0 and 10000")
	}
	if c.DatabaseTimeout < time.Second || c.DatabaseTimeout > 5*time.Minute {
		return errors.New("server: database timeout must be between 1s and 5m")
	}
	if err := validateDatabaseURL(c.DatabaseURL); err != nil {
		return err
	}
	if (c.BootstrapLogin == "") != (c.BootstrapPassword == "") {
		return errors.New("server: bootstrap admin login and password must be set together")
	}
	if c.BootstrapLogin != "" {
		if err := auth.ValidatePassword(c.BootstrapPassword); err != nil {
			return fmt.Errorf("server: bootstrap admin password: %w", err)
		}
	}
	if err := validateOIDC(c.OIDC); err != nil {
		return err
	}
	return nil
}

func validateOIDC(cfg oidc.Config) error {
	if strings.TrimSpace(cfg.Issuer) == "" {
		return nil
	}
	issuer, err := url.Parse(cfg.Issuer)
	if err != nil || (issuer.Scheme != "https" && issuer.Scheme != "http") || issuer.Host == "" {
		return errors.New("server: BRAIN_OIDC_ISSUER must be an absolute http(s) URL")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return errors.New("server: BRAIN_OIDC_CLIENT_ID is required when OIDC is enabled")
	}
	if strings.TrimSpace(cfg.RedirectURL) == "" {
		return errors.New("server: BRAIN_OIDC_REDIRECT_URL is required when OIDC is enabled")
	}
	if cfg.DefaultRole != "" && !cfg.DefaultRole.Valid() {
		return fmt.Errorf("server: OIDC default role %q is invalid", cfg.DefaultRole)
	}
	for group, role := range cfg.GroupRoles {
		if strings.TrimSpace(group) == "" {
			return errors.New("server: OIDC group names must not be empty")
		}
		if !role.Valid() {
			return fmt.Errorf("server: OIDC role %q for group %q is invalid", role, group)
		}
	}
	return nil
}

// PersistenceBackend names the configured store for startup logs.
func (c Config) PersistenceBackend() string {
	if strings.TrimSpace(c.DatabaseURL) != "" {
		return "postgres"
	}
	return "memory"
}

// validateDatabaseURL rejects malformed or non-Postgres URLs without ever
// including the configured value in an error.
func validateDatabaseURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return errors.New("server: database url must be a postgres:// URL with a host")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "postgres", "postgresql":
		return nil
	default:
		return errors.New("server: database url must use the postgres or postgresql scheme")
	}
}

// redactDatabaseURL renders a log-safe destination: scheme, host, and path
// stay, while credentials and query parameters are removed.
func redactDatabaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "<redacted>"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func applyEnvironment(cfg *Config, getenv func(string) string) error {
	if value := strings.TrimSpace(getenv("BRAIN_JWT_SECRET")); value != "" {
		cfg.JWTSecret = value
	}
	if value := strings.TrimSpace(getenv("BRAIN_ISSUER")); value != "" {
		cfg.Issuer = value
	}
	if value := strings.TrimSpace(getenv("BRAIN_AUDIENCE")); value != "" {
		cfg.Audience = value
	}
	var err error
	if cfg.AccessTTL, err = envDuration(getenv, "BRAIN_ACCESS_TTL", cfg.AccessTTL); err != nil {
		return err
	}
	if cfg.RefreshTTL, err = envDuration(getenv, "BRAIN_REFRESH_TTL", cfg.RefreshTTL); err != nil {
		return err
	}
	if cfg.SSEHeartbeat, err = envDuration(getenv, "BRAIN_SSE_HEARTBEAT", cfg.SSEHeartbeat); err != nil {
		return err
	}
	if cfg.SSERetry, err = envDuration(getenv, "BRAIN_SSE_RETRY", cfg.SSERetry); err != nil {
		return err
	}
	if cfg.ShutdownTimeout, err = envDuration(getenv, "BRAIN_SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout); err != nil {
		return err
	}
	if cfg.MaxBodyBytes, err = envInt64(getenv, "BRAIN_MAX_BODY_BYTES", cfg.MaxBodyBytes); err != nil {
		return err
	}
	rateLimit, err := envInt(getenv, "BRAIN_LOGIN_RATE_LIMIT", cfg.LoginRateLimit)
	if err != nil {
		return err
	}
	cfg.LoginRateLimit = rateLimit

	if cfg.DatabaseTimeout, err = envDuration(getenv, "BRAIN_DATABASE_TIMEOUT", cfg.DatabaseTimeout); err != nil {
		return err
	}
	if value := strings.TrimSpace(getenv("BRAIN_DATABASE_URL")); value != "" {
		cfg.DatabaseURL = value
	} else if value := strings.TrimSpace(getenv("DATABASE_URL")); value != "" {
		cfg.DatabaseURL = value
	}
	backupConfirmed, err := envBool(getenv, "BRAIN_DATABASE_BACKUP_CONFIRMED", cfg.DatabaseBackupConfirmed)
	if err != nil {
		return err
	}
	cfg.DatabaseBackupConfirmed = backupConfirmed

	cfg.BootstrapLogin = strings.TrimSpace(getenv("BRAIN_BOOTSTRAP_ADMIN_LOGIN"))
	cfg.BootstrapPassword = getenv("BRAIN_BOOTSTRAP_ADMIN_PASSWORD")

	cfg.OIDC.Issuer = strings.TrimSpace(getenv("BRAIN_OIDC_ISSUER"))
	cfg.OIDC.ClientID = strings.TrimSpace(getenv("BRAIN_OIDC_CLIENT_ID"))
	cfg.OIDC.ClientSecret = getenv("BRAIN_OIDC_CLIENT_SECRET")
	cfg.OIDC.RedirectURL = strings.TrimSpace(getenv("BRAIN_OIDC_REDIRECT_URL"))
	cfg.OIDC.GroupsClaim = strings.TrimSpace(getenv("BRAIN_OIDC_GROUPS_CLAIM"))
	if value := strings.TrimSpace(getenv("BRAIN_OIDC_SCOPES")); value != "" {
		cfg.OIDC.Scopes = splitList(value)
	}
	if value := strings.TrimSpace(getenv("BRAIN_OIDC_DEFAULT_ROLE")); value != "" {
		role, roleErr := domain.ParseRole(value)
		if roleErr != nil {
			return fmt.Errorf("server: BRAIN_OIDC_DEFAULT_ROLE: %w", roleErr)
		}
		cfg.OIDC.DefaultRole = role
	}
	if value := strings.TrimSpace(getenv("BRAIN_OIDC_GROUP_ROLES")); value != "" {
		roles, roleErr := parseGroupRoles(value)
		if roleErr != nil {
			return roleErr
		}
		cfg.OIDC.GroupRoles = roles
	}
	if value := strings.TrimSpace(getenv("BRAIN_OIDC_ALLOW_SIGNUP")); value != "" {
		allow, boolErr := strconv.ParseBool(value)
		if boolErr != nil {
			return fmt.Errorf("server: BRAIN_OIDC_ALLOW_SIGNUP: %w", boolErr)
		}
		cfg.OIDCAllowSignup = allow
	}
	return nil
}

func envDuration(getenv func(string) string, name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("server: %s: %w", name, err)
	}
	return parsed, nil
}

func envInt64(getenv func(string) string, name string, fallback int64) (int64, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("server: %s: %w", name, err)
	}
	return parsed, nil
}

func envInt(getenv func(string) string, name string, fallback int) (int, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("server: %s: %w", name, err)
	}
	return parsed, nil
}

func envBool(getenv func(string) string, name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("server: %s: %w", name, err)
	}
	return parsed, nil
}

func parseGroupRoles(value string) (map[string]domain.Role, error) {
	roles := make(map[string]domain.Role)
	for _, pair := range strings.Split(value, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		group, roleText, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("server: BRAIN_OIDC_GROUP_ROLES entry %q must be group=role", pair)
		}
		group = strings.TrimSpace(group)
		if group == "" {
			return nil, errors.New("server: BRAIN_OIDC_GROUP_ROLES contains an empty group")
		}
		role, err := domain.ParseRole(strings.TrimSpace(roleText))
		if err != nil {
			return nil, fmt.Errorf("server: BRAIN_OIDC_GROUP_ROLES: %w", err)
		}
		roles[group] = role
	}
	return roles, nil
}

func splitList(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func randomSecret() string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic("server: generate signing key: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
