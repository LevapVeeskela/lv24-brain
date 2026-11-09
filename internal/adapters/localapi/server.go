package localapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

// NewServer binds an IPv4 loopback listener. A zero port requests an ephemeral
// OS-selected port; the resulting port is available through Port().
func NewServer(opts Options) (*Server, error) {
	if opts.Port < 0 || opts.Port > 65535 {
		return nil, errors.New("localapi: port must be between 0 and 65535")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Port)))
	if err != nil {
		return nil, fmt.Errorf("localapi: bind loopback listener: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	opts.Port = port
	handler, err := NewHandler(opts)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // SSE streams are canceled by request/server shutdown.
		IdleTimeout:       60 * time.Second,
	}
	return &Server{Listener: listener, HTTP: server, Handler: handler}, nil
}

// Port reports the selected loopback port.
func (s *Server) Port() int {
	if s == nil || s.Listener == nil {
		return 0
	}
	return s.Listener.Addr().(*net.TCPAddr).Port
}

// Serve runs until the listener closes or ctx is canceled. It gracefully
// drains active requests on cancellation.
func (s *Server) Serve(ctx context.Context) error {
	if s == nil || s.HTTP == nil || s.Listener == nil {
		return errors.New("localapi: server is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	shutdownDone := make(chan struct{})
	serveDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.HTTP.Shutdown(shutdownCtx)
		case <-serveDone:
		}
	}()
	err := s.HTTP.Serve(s.Listener)
	close(serveDone)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if ctx.Err() != nil {
		<-shutdownDone
	}
	return err
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.HTTP == nil {
		return nil
	}
	return s.HTTP.Shutdown(ctx)
}
