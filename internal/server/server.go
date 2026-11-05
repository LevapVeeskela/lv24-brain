// Package server provides the shared brain-server HTTP surface.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"runtime"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/version"
)

// Handler builds the HTTP handler for the brain server. It has no database
// yet; health and readiness are process-level only.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", jsonEndpoint(http.MethodGet, func() any {
		return map[string]string{"status": "ok"}
	}))
	mux.HandleFunc("/ready", jsonEndpoint(http.MethodGet, func() any {
		return map[string]string{"status": "ready"}
	}))
	mux.HandleFunc("/version", jsonEndpoint(http.MethodGet, func() any {
		return map[string]string{
			"version": version.Version,
			"commit":  version.Commit,
			"date":    version.Date,
			"go":      runtime.Version(),
		}
	}))
	return mux
}

// Run serves HTTP until ctx is cancelled, then shuts down gracefully.
func Run(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
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

func jsonEndpoint(method string, payload func() any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(payload()); err != nil {
			log.Printf("encode response: %v", err)
		}
	}
}
