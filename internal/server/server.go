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
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/adapters/oidc"
	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Server owns the process HTTP listener and its in-memory store.
type Server struct {
	cfg    Config
	store  *httpapi.MemoryStore
	api    *httpapi.API
	logger *slog.Logger
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
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	store := httpapi.NewMemoryStore()
	if cfg.BootstrapLogin != "" {
		hash, err := auth.HashPassword(cfg.BootstrapPassword, cfg.PasswordParams)
		if err != nil {
			return nil, fmt.Errorf("server: hash bootstrap password: %w", err)
		}
		_, _, err = store.CreateUser(ctx, domain.User{
			Login:       cfg.BootstrapLogin,
			DisplayName: "Bootstrap administrator",
			Role:        domain.RoleAdmin,
			AuthSource:  "local",
		}, hash)
		if err != nil && !errors.Is(err, domain.ErrConflict) {
			return nil, fmt.Errorf("server: create bootstrap admin: %w", err)
		}
	}

	var provider auth.OIDCProvider
	if cfg.OIDC.Issuer != "" {
		discoveryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		authenticator, err := oidc.New(discoveryCtx, cfg.OIDC)
		if err != nil {
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
		return nil, err
	}
	return &Server{cfg: cfg, store: store, api: api, logger: logger}, nil
}

// Handler returns the composed HTTP handler.
func (s *Server) Handler() http.Handler { return s.api.Handler() }

// Run serves HTTP until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
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
