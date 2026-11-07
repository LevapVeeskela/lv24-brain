package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// requestState carries per-request facts from the router and the auth layer
// up to the outermost request logger. It holds only the route pattern and the
// opaque user id: never tokens, headers, or bodies.
type requestState struct {
	route  string
	userID string
}

type requestStateKey struct{}

func requestStateFrom(ctx context.Context) *requestState {
	state, _ := ctx.Value(requestStateKey{}).(*requestState)
	return state
}

// logRequests is the outermost middleware. It emits one structured record per
// request through requestLogger, after the response is complete.
func (a *API) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := a.cfg.Now()
		state := &requestState{}
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r.WithContext(context.WithValue(r.Context(), requestStateKey{}, state)))
		a.writeRequestLog(r, recorder.status, state, started, w.Header().Get("X-Request-Id"))
	})
}

func (a *API) writeRequestLog(r *http.Request, status int, state *requestState, started time.Time, requestID string) {
	level := slog.LevelInfo
	switch {
	case status >= http.StatusInternalServerError:
		level = slog.LevelError
	case r.URL.Path == "/health" || r.URL.Path == "/ready":
		// Container healthchecks would otherwise log every few seconds.
		level = slog.LevelDebug
	}
	route := state.route
	if route == "" {
		route = unmatchedRoute
	}
	attrs := []any{
		"request_id", requestID,
		"method", normalizeMethod(r.Method),
		"route", route,
		"status", status,
		"duration_ms", a.cfg.Now().Sub(started).Milliseconds(),
	}
	if state.userID != "" {
		attrs = append(attrs, "user_id", state.userID)
	}
	a.requestLogger.Log(r.Context(), level, "http request", attrs...)
}
