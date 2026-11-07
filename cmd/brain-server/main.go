// Command brain-server runs the LV24 Brain HTTP server.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/levapveeskela/lv24-brain/internal/server"
	"github.com/levapveeskela/lv24-brain/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := server.LoadConfig(args, os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "brain-server: %v\n", err)
		return 2
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// Request records are JSON for log pipelines; other logs keep the text format.
	cfg.RequestLogger = slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if cfg.Notice != "" {
		logger.Warn(cfg.Notice)
	}
	logger.Info("persistence backend", "backend", cfg.PersistenceBackend())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(ctx, cfg, logger)
	if err != nil {
		logger.Error("startup failed", "error", err)
		return 1
	}
	if cfg.BootstrapLogin != "" {
		logger.Info("bootstrap administrator ensured", "login", cfg.BootstrapLogin)
	}
	if cfg.OIDC.Issuer != "" {
		logger.Info("oidc enabled", "issuer", cfg.OIDC.Issuer)
	}
	fmt.Fprintf(stdout, "brain-server %s listening on %s\n", version.Version, cfg.Addr)
	if err := srv.Run(ctx); err != nil {
		logger.Error("server failed", "error", err)
		return 1
	}
	return 0
}
