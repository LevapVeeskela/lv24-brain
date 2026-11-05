package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/version"
)

// BuildInfo is the version payload served at /version.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
}

// Config configures the HTTP API.
type Config struct {
	Secret          []byte
	Issuer          string
	Audience        string
	AccessTTL       time.Duration
	RefreshTTL      time.Duration
	TokenPrefix     string
	MaxBodyBytes    int64
	SSEHeartbeat    time.Duration
	SSERetry        time.Duration
	LoginRateLimit  int
	PasswordParams  auth.Argon2Params
	OIDC            auth.OIDCProvider
	OIDCAllowSignup bool
	ConnectorRunner ConnectorRunner
	Now             func() time.Time
	Logger          *slog.Logger
	Build           BuildInfo
}

// API is the brain HTTP surface.
type API struct {
	cfg    Config
	store  Store
	auth   *auth.Service
	broker *broker
	flows  *flowStore
	mux    *http.ServeMux
	logger *slog.Logger
	limit  *loginLimiter

	handlerOnce sync.Once
	handler     http.Handler
}

// New validates cfg, wires authentication and returns an API.
func New(cfg Config, store Store) (*API, error) {
	if store == nil {
		return nil, errors.New("httpapi: store is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Issuer == "" {
		cfg.Issuer = "lv24-brain"
	}
	if cfg.Audience == "" {
		cfg.Audience = "lv24-brain-api"
	}
	if cfg.AccessTTL <= 0 {
		cfg.AccessTTL = 15 * time.Minute
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = 30 * 24 * time.Hour
	}
	if cfg.RefreshTTL <= cfg.AccessTTL {
		return nil, errors.New("httpapi: refresh ttl must exceed access ttl")
	}
	if cfg.TokenPrefix == "" {
		cfg.TokenPrefix = "brain_"
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 1 << 20
	}
	if cfg.SSEHeartbeat <= 0 {
		cfg.SSEHeartbeat = 15 * time.Second
	}
	if cfg.SSERetry <= 0 {
		cfg.SSERetry = 3 * time.Second
	}
	if cfg.PasswordParams == (auth.Argon2Params{}) {
		cfg.PasswordParams = auth.DefaultArgon2Params
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Build.Version == "" {
		cfg.Build = BuildInfo{
			Version: version.Version,
			Commit:  version.Commit,
			Date:    version.Date,
			Go:      runtime.Version(),
		}
	}

	signer, err := auth.NewSigner(auth.SignerConfig{
		Secret:   cfg.Secret,
		Issuer:   cfg.Issuer,
		Audience: cfg.Audience,
		TTL:      cfg.AccessTTL,
		Leeway:   30 * time.Second,
		Now:      cfg.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("httpapi: %w", err)
	}
	bridge := authBridge{store: store, now: cfg.Now}
	service, err := auth.NewService(auth.ServiceConfig{
		Signer:         signer,
		Users:          bridge,
		Sessions:       store,
		APITokens:      bridge,
		RefreshTTL:     cfg.RefreshTTL,
		PasswordParams: cfg.PasswordParams,
		Now:            cfg.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("httpapi: %w", err)
	}
	api := &API{
		cfg:    cfg,
		store:  store,
		auth:   service,
		broker: newBroker(),
		mux:    http.NewServeMux(),
		logger: cfg.Logger,
		limit:  newLoginLimiter(cfg.LoginRateLimit, time.Minute, cfg.Now),
	}
	if cfg.OIDC != nil {
		api.flows = newFlowStore(cfg.Now, 10*time.Minute, 1024)
	}
	api.routes()
	return api, nil
}

// Handler returns the composed HTTP handler.
func (a *API) Handler() http.Handler {
	a.handlerOnce.Do(func() {
		var h http.Handler = a.mux
		h = a.logRequests(h)
		h = a.securityHeaders(h)
		h = a.recoverPanics(h)
		h = a.requestIDs(h)
		a.handler = h
	})
	return a.handler
}

// Close releases long-lived SSE subscribers.
func (a *API) Close() { a.broker.close() }

func (a *API) routes() {
	a.mux.HandleFunc("GET /health", a.health)
	a.mux.HandleFunc("GET /ready", a.ready)
	a.mux.HandleFunc("GET /version", a.version)
	a.mux.HandleFunc("GET /openapi.json", a.openAPI)

	a.mux.HandleFunc("POST /v1/auth/login", a.login)
	a.mux.HandleFunc("POST /v1/auth/refresh", a.refresh)
	a.authed("POST /v1/auth/logout", a.logout, domain.RoleViewer, false)
	a.authed("GET /v1/auth/me", a.me, domain.RoleViewer, false)
	a.mux.HandleFunc("GET /v1/auth/oidc/login", a.oidcLogin)
	a.mux.HandleFunc("GET /v1/auth/oidc/callback", a.oidcCallback)

	a.authed("GET /v1/users", a.listUsers, domain.RoleAdmin, false)
	a.authed("POST /v1/users", a.createUser, domain.RoleAdmin, false)
	a.authed("GET /v1/users/{id}", a.getUser, domain.RoleAdmin, false)
	a.authed("PATCH /v1/users/{id}", a.updateUser, domain.RoleAdmin, false)
	a.authed("DELETE /v1/users/{id}", a.deleteUser, domain.RoleAdmin, false)

	a.authed("GET /v1/tokens", a.listTokens, domain.RoleViewer, false)
	a.authed("POST /v1/tokens", a.createToken, domain.RoleViewer, false)
	a.authed("DELETE /v1/tokens/{id}", a.revokeToken, domain.RoleViewer, false)

	a.authed("GET /v1/projects", a.listProjects, domain.RoleViewer, false)
	a.authed("POST /v1/projects", a.createProject, domain.RoleMember, false)
	a.authed("GET /v1/projects/{id}", a.getProject, domain.RoleViewer, false)
	a.authed("PATCH /v1/projects/{id}", a.updateProject, domain.RoleMember, false)
	a.authed("DELETE /v1/projects/{id}", a.deleteProject, domain.RoleAdmin, false)

	a.authed("GET /v1/projects/{id}/state", a.getState, domain.RoleViewer, false)
	a.authed("PUT /v1/projects/{id}/state", a.putState, domain.RoleMember, false)

	a.authed("GET /v1/projects/{id}/notes", a.listNotes, domain.RoleViewer, false)
	a.authed("POST /v1/projects/{id}/notes", a.createNote, domain.RoleMember, false)
	a.authed("GET /v1/notes/{id}", a.getNote, domain.RoleViewer, false)
	a.authed("PATCH /v1/notes/{id}", a.updateNote, domain.RoleMember, false)
	a.authed("DELETE /v1/notes/{id}", a.deleteNote, domain.RoleMember, false)

	a.authed("GET /v1/projects/{id}/decisions", a.listDecisions, domain.RoleViewer, false)
	a.authed("POST /v1/projects/{id}/decisions", a.createDecision, domain.RoleMember, false)
	a.authed("GET /v1/decisions/{id}", a.getDecision, domain.RoleViewer, false)
	a.authed("PATCH /v1/decisions/{id}", a.updateDecision, domain.RoleMember, false)
	a.authed("DELETE /v1/decisions/{id}", a.deleteDecision, domain.RoleMember, false)

	a.authed("GET /v1/projects/{id}/attempts", a.listAttempts, domain.RoleViewer, false)
	a.authed("POST /v1/projects/{id}/attempts", a.createAttempt, domain.RoleMember, false)
	a.authed("GET /v1/attempts/{id}", a.getAttempt, domain.RoleViewer, false)
	a.authed("PATCH /v1/attempts/{id}", a.updateAttempt, domain.RoleMember, false)
	a.authed("DELETE /v1/attempts/{id}", a.deleteAttempt, domain.RoleMember, false)

	a.authed("GET /v1/projects/{id}/handoffs", a.listHandoffs, domain.RoleViewer, false)
	a.authed("POST /v1/projects/{id}/handoffs", a.createHandoff, domain.RoleMember, false)
	a.authed("GET /v1/handoffs/{id}", a.getHandoff, domain.RoleViewer, false)
	a.authed("DELETE /v1/handoffs/{id}", a.deleteHandoff, domain.RoleMember, false)

	a.authed("GET /v1/search", a.search, domain.RoleViewer, false)
	a.authed("GET /v1/revisions", a.listRevisions, domain.RoleViewer, false)

	a.authed("POST /v1/connectors/run", a.runConnector, domain.RoleAdmin, false)
	a.authed("GET /v1/connectors/runs", a.listConnectorRuns, domain.RoleViewer, false)
	a.authed("GET /v1/connectors/runs/{id}/markdown", a.exportConnectorRun, domain.RoleViewer, false)
	a.authed("GET /v1/connectors/runs/{id}", a.getConnectorRun, domain.RoleViewer, false)

	a.authed("GET /v1/events", a.events, domain.RoleViewer, true)
}

func (a *API) authed(pattern string, handler http.HandlerFunc, min domain.Role, allowQueryToken bool) {
	a.mux.Handle(pattern, a.withAuth(min, allowQueryToken, handler))
}

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.store.Ping(ctx); err != nil {
		a.logger.Warn("readiness check failed", "request_id", requestIDFrom(r.Context()), "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (a *API) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.cfg.Build)
}

type principalContextKey struct{}

func principalFrom(ctx context.Context) (auth.Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(auth.Principal)
	return principal, ok
}

func (a *API) withAuth(min domain.Role, allowQueryToken bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := a.authenticate(r, allowQueryToken)
		if err != nil {
			if !isCredentialError(err) {
				// A database or adapter failure is not the caller's fault.
				// Log it with the request id and never echo token material.
				a.logger.Error("authentication failed",
					"request_id", requestIDFrom(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"error", err,
				)
				writeErrorCode(w, http.StatusInternalServerError, "internal", "internal server error")
				return
			}
			writeAuthError(w)
			return
		}
		if !roleAtLeast(principal.User.Role, min) {
			writeErrorCode(w, http.StatusForbidden, "forbidden", "insufficient role")
			return
		}
		ctx := context.WithValue(r.Context(), principalContextKey{}, principal)
		ctx = context.WithValue(ctx, actorContextKey{}, principal.User.Login)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isCredentialError reports failures that legitimately mean the presented
// credential is unusable. Everything else is an infrastructure failure.
func isCredentialError(err error) bool {
	return errors.Is(err, auth.ErrInvalidToken) ||
		errors.Is(err, auth.ErrInvalidCredentials) ||
		errors.Is(err, auth.ErrInvalidRefresh)
}

func (a *API) authenticate(r *http.Request, allowQueryToken bool) (auth.Principal, error) {
	raw := bearerToken(r)
	if raw == "" && allowQueryToken {
		raw = strings.TrimSpace(r.URL.Query().Get("access_token"))
	}
	if raw == "" {
		return auth.Principal{}, auth.ErrInvalidToken
	}
	if strings.HasPrefix(raw, a.cfg.TokenPrefix) {
		user, err := a.auth.AuthenticateAPIToken(r.Context(), raw)
		if err != nil {
			return auth.Principal{}, err
		}
		return auth.Principal{User: user, AuthSource: "api_token"}, nil
	}
	return a.auth.AuthenticateJWT(r.Context(), raw)
}

func bearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return ""
	}
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func roleRank(role domain.Role) int {
	switch role {
	case domain.RoleAdmin:
		return 3
	case domain.RoleMember:
		return 2
	case domain.RoleViewer:
		return 1
	default:
		return 0
	}
}

func roleAtLeast(have, min domain.Role) bool {
	return roleRank(have) >= roleRank(min)
}

func (a *API) publish(revision domain.SyncRevision, entityKind, entityID, action string) {
	a.broker.publish(event{
		Revision:   revisionValue(revision),
		EntityKind: entityKind,
		EntityID:   entityID,
		Action:     action,
		At:         revision.At,
	})
}

type requestIDKey struct{}

func requestIDFrom(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}

func (a *API) requestIDs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

func validRequestID(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func (a *API) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

func (a *API) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				a.logger.Error("panic serving request",
					"request_id", requestIDFrom(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"panic", recovered,
				)
				writeErrorCode(w, http.StatusInternalServerError, "internal", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *API) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := a.cfg.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		level := slog.LevelDebug
		if recorder.status >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		a.logger.Log(r.Context(), level, "http request",
			"request_id", requestIDFrom(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration_ms", a.cfg.Now().Sub(started).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func newRequestID() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "req-unknown"
	}
	return hex.EncodeToString(raw)
}

// loginLimiter is a fixed-window per-key attempt counter.
type loginLimiter struct {
	mu          sync.Mutex
	max         int
	window      time.Duration
	now         func() time.Time
	windowStart time.Time
	counts      map[string]int
}

func newLoginLimiter(max int, window time.Duration, now func() time.Time) *loginLimiter {
	if now == nil {
		now = time.Now
	}
	return &loginLimiter{
		max:         max,
		window:      window,
		now:         now,
		windowStart: now(),
		counts:      make(map[string]int),
	}
}

func (l *loginLimiter) allow(key string) bool {
	if l == nil || l.max <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.windowStart) >= l.window {
		l.windowStart = now
		l.counts = make(map[string]int)
	}
	if l.counts[key] >= l.max {
		return false
	}
	l.counts[key]++
	return true
}
