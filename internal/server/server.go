// Package server wires the brain-server process: configuration, bootstrap
// admin, authentication, and the HTTP API.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/adapters/oidc"
	"github.com/levapveeskela/lv24-brain/internal/adapters/postgres"
	"github.com/levapveeskela/lv24-brain/internal/adapters/serverstore"
	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Server owns the process HTTP listener and its persistence store.
type Server struct {
	cfg    Config
	store  httpapi.Store
	api    *httpapi.API
	logger *slog.Logger

	closeStore func()
	closeOnce  sync.Once
}

// New builds the server. It fails fast on invalid configuration, bootstrap
// errors, or OIDC discovery failures.
func New(ctx context.Context, cfg Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PasswordParams == (auth.Argon2Params{}) {
		cfg.PasswordParams = auth.DefaultArgon2Params
	}
	if cfg.DatabaseTimeout <= 0 {
		cfg.DatabaseTimeout = 30 * time.Second
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	store, closeStore, err := openStore(ctx, cfg, logger)
	if err != nil {
		return nil, err
	}
	if cfg.BootstrapLogin != "" {
		hash, err := auth.HashPassword(cfg.BootstrapPassword, cfg.PasswordParams)
		if err != nil {
			closeStore()
			return nil, fmt.Errorf("server: hash bootstrap password: %w", err)
		}
		_, _, err = store.CreateUser(ctx, domain.User{
			Login:       cfg.BootstrapLogin,
			DisplayName: "Bootstrap administrator",
			Role:        domain.RoleAdmin,
			AuthSource:  "local",
		}, hash)
		if err != nil && !errors.Is(err, domain.ErrConflict) {
			closeStore()
			return nil, fmt.Errorf("server: create bootstrap admin: %w", err)
		}
	}

	var provider auth.OIDCProvider
	if cfg.OIDC.Issuer != "" {
		discoveryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		authenticator, err := oidc.New(discoveryCtx, cfg.OIDC)
		if err != nil {
			closeStore()
			return nil, fmt.Errorf("server: configure oidc: %w", err)
		}
		provider = authenticator
	}

	api, err := httpapi.New(httpapi.Config{
		Secret:          []byte(cfg.JWTSecret),
		Issuer:          cfg.Issuer,
		Audience:        cfg.Audience,
		AccessTTL:       cfg.AccessTTL,
		RefreshTTL:      cfg.RefreshTTL,
		MaxBodyBytes:    cfg.MaxBodyBytes,
		SSEHeartbeat:    cfg.SSEHeartbeat,
		SSERetry:        cfg.SSERetry,
		LoginRateLimit:  cfg.LoginRateLimit,
		PasswordParams:  cfg.PasswordParams,
		OIDC:            provider,
		OIDCAllowSignup: cfg.OIDCAllowSignup,
		Now:             cfg.Now,
		Logger:          logger,
	}, store)
	if err != nil {
		closeStore()
		return nil, err
	}
	return &Server{cfg: cfg, store: store, api: api, logger: logger, closeStore: closeStore}, nil
}

// Close releases SSE subscribers and the database pool. It is idempotent.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.api != nil {
			s.api.Close()
		}
		if s.closeStore != nil {
			s.closeStore()
		}
	})
	return nil
}

// openStore selects the persistence backend. A database URL enables durable
// server mode; without one the process deliberately falls back to the
// in-memory store, which loses all data on restart.
func openStore(ctx context.Context, cfg Config, logger *slog.Logger) (httpapi.Store, func(), error) {
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		logger.Warn("BRAIN_DATABASE_URL is not set; using the in-memory store (development mode only; all data is lost on restart)")
		return httpapi.NewMemoryStore(), func() {}, nil
	}

	openCtx, cancel := context.WithTimeout(ctx, cfg.DatabaseTimeout)
	defer cancel()
	pg, err := postgres.Open(openCtx, cfg.DatabaseURL, postgres.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("server: open database: %w", err)
	}
	closePostgres := pg.Close

	// Health gate: postgres.Open already pinged the database. The backup gate
	// protects an existing schema before forward-only migrations run.
	if err := migrationGate(openCtx, pg, cfg.DatabaseBackupConfirmed); err != nil {
		closePostgres()
		return nil, nil, err
	}
	result, err := pg.Migrate(openCtx)
	if err != nil {
		closePostgres()
		return nil, nil, fmt.Errorf("server: migrate database: %w", err)
	}
	store, err := serverstore.New(pg)
	if err != nil {
		closePostgres()
		return nil, nil, fmt.Errorf("server: adapt database store: %w", err)
	}
	logger.Info("postgres persistence enabled",
		"database", redactDatabaseURL(cfg.DatabaseURL),
		"schema_version", result.Version,
		"migrations_applied", len(result.Applied),
	)
	return store, closePostgres, nil
}

// migrationGate counts pending migrations for an existing schema and requires
// an explicit backup acknowledgement before applying them. A fresh database
// has nothing to protect and passes immediately.
func migrationGate(ctx context.Context, pg *postgres.Store, backupConfirmed bool) error {
	if pg == nil || pg.Pool() == nil {
		return errors.New("server: database store is not configured")
	}
	migrations, err := postgres.LoadMigrations()
	if err != nil {
		return fmt.Errorf("server: load migrations: %w", err)
	}

	var tableExists bool
	err = pg.Pool().QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = current_schema()
			  AND table_name = 'brain_schema_migrations'
		)`).Scan(&tableExists)
	if err != nil {
		return fmt.Errorf("server: inspect database schema: %w", err)
	}
	if !tableExists {
		return nil
	}

	rows, err := pg.Pool().Query(ctx, `SELECT version FROM brain_schema_migrations`)
	if err != nil {
		return fmt.Errorf("server: read applied migrations: %w", err)
	}
	defer rows.Close()
	applied := make(map[int]struct{})
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return fmt.Errorf("server: read applied migrations: %w", err)
		}
		applied[version] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("server: read applied migrations: %w", err)
	}

	pending := make([]int, 0, len(migrations))
	for _, migration := range migrations {
		if _, ok := applied[migration.Version]; !ok {
			pending = append(pending, migration.Version)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	if !backupConfirmed {
		return fmt.Errorf(
			"server: database has %d pending migration(s) %v; take a backup and set BRAIN_DATABASE_BACKUP_CONFIRMED=1 to continue",
			len(pending), pending,
		)
	}
	return nil
}

// Handler returns the composed HTTP handler.
func (s *Server) Handler() http.Handler { return s.api.Handler() }

// Run serves HTTP until ctx is cancelled, then shuts down gracefully. The
// store and its connection pool are closed exactly once on exit.
func (s *Server) Run(ctx context.Context) error {
	defer func() { _ = s.Close() }()
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		s.api.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			_ = srv.Close()
		}
		<-errCh
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Handler builds a development handler with an ephemeral signing key. It is
// kept for the scaffold CLI and tests; production uses LoadConfig + New.
func Handler() http.Handler {
	srv, err := New(context.Background(), DefaultConfig(), nil)
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "server unavailable", http.StatusInternalServerError)
		})
	}
	return srv.Handler()
}

// Run is the legacy convenience entry used by `brain serve`. It applies
// environment overrides and uses an ephemeral key when BRAIN_JWT_SECRET is
// unset.
func Run(ctx context.Context, addr string) error {
	cfg := DefaultConfig()
	cfg.Addr = addr
	if err := applyEnvironment(&cfg, os.Getenv); err != nil {
		return err
	}
	if os.Getenv("BRAIN_JWT_SECRET") == "" {
		slog.Warn("BRAIN_JWT_SECRET is not set; using an ephemeral signing key")
	}
	srv, err := New(ctx, cfg, nil)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}
