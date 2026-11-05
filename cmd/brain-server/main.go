// Command brain-server runs the LV24 Brain HTTP server.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/server"
	"github.com/levapveeskela/lv24-brain/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("brain-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	defaultAddr := os.Getenv("BRAIN_ADDR")
	if defaultAddr == "" {
		defaultAddr = "127.0.0.1:8080"
	}
	addr := fs.String("addr", defaultAddr, "listen address")
	cfgPath := fs.String("config", "", "optional path to brain.yaml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *cfgPath != "" {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintf(stderr, "brain-server: %v\n", err)
			return 1
		}
		if err := cfg.Validate(); err != nil {
			fmt.Fprintf(stderr, "brain-server: %v\n", err)
			return 1
		}
		if cfg.Mode != config.ModeServer {
			fmt.Fprintf(stderr, "brain-server: warning: config mode is %q, expected %q\n", cfg.Mode, config.ModeServer)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(stdout, "brain-server %s listening on %s\n", version.Version, *addr)
	if err := server.Run(ctx, *addr); err != nil {
		fmt.Fprintf(stderr, "brain-server: %v\n", err)
		return 1
	}
	return 0
}
