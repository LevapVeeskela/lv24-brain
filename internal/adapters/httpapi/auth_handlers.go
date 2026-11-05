package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type sessionResponse struct {
	AccessToken  string      `json:"accessToken"`
	RefreshToken string      `json:"refreshToken"`
	ExpiresAt    time.Time   `json:"expiresAt"`
	User         domain.User `json:"user"`
}

func newSessionResponse(session ports.Session) sessionResponse {
	return sessionResponse{
		AccessToken:  session.AccessToken,
		RefreshToken: session.RefreshToken,
		ExpiresAt:    session.ExpiresAt,
		User:         session.User,
	}
}

type meResponse struct {
	User       domain.User `json:"user"`
	AuthSource string      `json:"authSource"`
	ExpiresAt  *time.Time  `json:"expiresAt,omitempty"`
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	client := clientHost(r)
	if !a.limit.allow(client) {
		w.Header().Set("Retry-After", "60")
		writeErrorCode(w, http.StatusTooManyRequests, "rate_limited", "too many login attempts")
		return
	}
	var body struct {
		Login    string `json:"login"`
		Password string `json:"password"`
	}
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	session, err := a.auth.Login(r.Context(), ports.Credentials{Login: body.Login, Password: body.Password})
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			writeErrorCode(w, http.StatusUnauthorized, "invalid_credentials", "invalid login or password")
			return
		}
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newSessionResponse(session))
}

func (a *API) refresh(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	session, err := a.auth.Refresh(r.Context(), body.RefreshToken)
	if err != nil {
		// Only credential failures are the caller's fault. Infrastructure
		// failures must not be reported as a bad refresh token.
		if isRefreshCredentialError(err) {
			writeErrorCode(w, http.StatusUnauthorized, "invalid_refresh", "refresh token is invalid")
			return
		}
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newSessionResponse(session))
}

// isRefreshCredentialError reports whether a refresh failure is a bad
// credential rather than an unavailable backend.
func isRefreshCredentialError(err error) bool {
	return errors.Is(err, auth.ErrInvalidRefresh) ||
		errors.Is(err, auth.ErrRefreshReused)
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeAuthError(w)
		return
	}
	if principal.AuthSource == "jwt" {
		if err := a.auth.LogoutFamily(r.Context(), principal.FamilyID); err != nil {
			a.writeError(w, r, err)
			return
		}
		if err := a.auth.Logout(r.Context(), principal.SessionID); err != nil {
			a.writeError(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeAuthError(w)
		return
	}
	response := meResponse{User: principal.User, AuthSource: principal.AuthSource}
	if !principal.ExpiresAt.IsZero() {
		expires := principal.ExpiresAt
		response.ExpiresAt = &expires
	}
	writeJSON(w, http.StatusOK, response)
}

// oidcFlow is one in-flight authorization-code exchange.
type oidcFlow struct {
	Nonce     string
	Verifier  string
	CreatedAt time.Time
}

type flowStore struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	now   func() time.Time
	flows map[string]oidcFlow
}

func newFlowStore(now func() time.Time, ttl time.Duration, max int) *flowStore {
	if now == nil {
		now = time.Now
	}
	return &flowStore{ttl: ttl, max: max, now: now, flows: make(map[string]oidcFlow)}
}

func (f *flowStore) put(state string, flow oidcFlow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	for key, existing := range f.flows {
		if now.Sub(existing.CreatedAt) > f.ttl {
			delete(f.flows, key)
		}
	}
	if len(f.flows) >= f.max {
		return errors.New("too many pending oidc flows")
	}
	f.flows[state] = flow
	return nil
}

func (f *flowStore) pop(state string) (oidcFlow, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	flow, ok := f.flows[state]
	if !ok {
		return oidcFlow{}, false
	}
	delete(f.flows, state)
	if f.now().Sub(flow.CreatedAt) > f.ttl {
		return oidcFlow{}, false
	}
	return flow, true
}

func (a *API) oidcLogin(w http.ResponseWriter, r *http.Request) {
	if a.cfg.OIDC == nil || a.flows == nil {
		writeErrorCode(w, http.StatusNotImplemented, "unsupported", "OIDC is not configured")
		return
	}
	state, err := auth.RandomToken(24)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	nonce, err := auth.RandomToken(24)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	verifier, err := auth.RandomToken(32)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := a.flows.put(state, oidcFlow{Nonce: nonce, Verifier: verifier, CreatedAt: a.cfg.Now()}); err != nil {
		writeErrorCode(w, http.StatusTooManyRequests, "rate_limited", "too many pending OIDC logins")
		return
	}
	http.Redirect(w, r, a.cfg.OIDC.AuthCodeURL(state, nonce, verifier), http.StatusFound)
}

func (a *API) oidcCallback(w http.ResponseWriter, r *http.Request) {
	if a.cfg.OIDC == nil || a.flows == nil {
		writeErrorCode(w, http.StatusNotImplemented, "unsupported", "OIDC is not configured")
		return
	}
	query := r.URL.Query()
	if providerError := query.Get("error"); providerError != "" {
		writeErrorCode(w, http.StatusBadRequest, "oidc_error", "authorization was not granted")
		return
	}
	state := strings.TrimSpace(query.Get("state"))
	code := strings.TrimSpace(query.Get("code"))
	if state == "" || code == "" {
		writeErrorCode(w, http.StatusBadRequest, "invalid_request", "state and code are required")
		return
	}
	flow, ok := a.flows.pop(state)
	if !ok {
		writeErrorCode(w, http.StatusBadRequest, "invalid_state", "authorization state is unknown or expired")
		return
	}
	claims, err := a.cfg.OIDC.Exchange(r.Context(), code, flow.Verifier, flow.Nonce)
	if err != nil {
		a.logger.Warn("oidc exchange failed", "request_id", requestIDFrom(r.Context()), "error", err)
		writeErrorCode(w, http.StatusUnauthorized, "oidc_rejected", "identity provider rejected the login")
		return
	}
	user, err := a.resolveOIDCUser(r.Context(), claims)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	session, err := a.auth.IssueSessionForUser(r.Context(), user)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newSessionResponse(session))
}

func (a *API) resolveOIDCUser(ctx context.Context, claims auth.OIDCClaims) (domain.User, error) {
	if claims.Issuer == "" || claims.Subject == "" {
		return domain.User{}, domain.NewValidationError("oidc.subject", "issuer and subject are required")
	}
	record, err := a.store.GetUserByOIDC(ctx, claims.Issuer, claims.Subject)
	if err == nil {
		return record.User, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return domain.User{}, err
	}
	record, err = a.store.GetUserByLogin(ctx, claims.Login)
	switch {
	case err == nil:
		if record.OIDCIssuer != "" || record.OIDCSubject != "" {
			return domain.User{}, domain.NewValidationError("oidc.subject", "account is linked to another identity provider")
		}
		if _, err := a.store.SetUserOIDC(ctx, record.ID, claims.Issuer, claims.Subject); err != nil {
			return domain.User{}, err
		}
		return record.User, nil
	case !errors.Is(err, domain.ErrNotFound):
		return domain.User{}, err
	}
	if !a.cfg.OIDCAllowSignup {
		return domain.User{}, auth.ErrForbidden
	}
	role := claims.Role
	if !role.Valid() {
		role = domain.RoleViewer
	}
	created, _, err := a.store.CreateUser(ctx, domain.User{
		Login:       claims.Login,
		DisplayName: claims.DisplayName,
		Role:        role,
		AuthSource:  "oidc",
	}, "")
	if err != nil {
		return domain.User{}, err
	}
	if _, err := a.store.SetUserOIDC(ctx, created.ID, claims.Issuer, claims.Subject); err != nil {
		return domain.User{}, err
	}
	return created.User, nil
}

type createTokenRequest struct {
	Name   string      `json:"name"`
	Role   domain.Role `json:"role"`
	UserID string      `json:"userId"`
}

type createTokenResponse struct {
	Token    string         `json:"token"`
	APIToken APITokenRecord `json:"apiToken"`
}

func (a *API) createToken(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())
	var body createTokenRequest
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 100 {
		a.writeError(w, r, domain.NewValidationError("token.name", "must be between 1 and 100 characters"))
		return
	}
	targetID := principal.User.ID
	if body.UserID != "" && body.UserID != principal.User.ID {
		if principal.User.Role != domain.RoleAdmin {
			writeErrorCode(w, http.StatusForbidden, "forbidden", "only admins can mint tokens for other users")
			return
		}
		targetID = body.UserID
	}
	target, err := a.store.GetUser(r.Context(), targetID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	role := body.Role
	if role == "" {
		role = target.User.Role
	}
	if !role.Valid() {
		a.writeError(w, r, domain.NewValidationError("token.role", "unknown role"))
		return
	}
	if roleRank(role) > roleRank(principal.User.Role) {
		writeErrorCode(w, http.StatusForbidden, "forbidden", "token role exceeds your role")
		return
	}
	if roleRank(role) > roleRank(target.User.Role) {
		writeErrorCode(w, http.StatusForbidden, "forbidden", "token role exceeds the owner's role")
		return
	}
	raw, hash, display, err := auth.GenerateToken(a.cfg.TokenPrefix, 32)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	record, err := a.store.CreateAPIToken(r.Context(), APITokenRecord{
		UserID:    target.User.ID,
		Name:      body.Name,
		Role:      role,
		Prefix:    display,
		TokenHash: hash,
		CreatedAt: a.cfg.Now().UTC(),
	})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, createTokenResponse{Token: raw, APIToken: record})
}

func (a *API) listTokens(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())
	target := principal.User.ID
	if requested := queryString(r, "userId"); requested != "" && requested != principal.User.ID {
		if principal.User.Role != domain.RoleAdmin {
			writeErrorCode(w, http.StatusForbidden, "forbidden", "only admins can list other users' tokens")
			return
		}
		target = requested
	}
	if principal.User.Role == domain.RoleAdmin && queryString(r, "all") == "true" {
		target = ""
	}
	tokens, err := a.store.ListAPITokens(r.Context(), target)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": tokens})
}

func (a *API) revokeToken(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())
	record, err := a.store.GetAPIToken(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if record.UserID != principal.User.ID && principal.User.Role != domain.RoleAdmin {
		writeErrorCode(w, http.StatusForbidden, "forbidden", "only the owner or an admin can revoke this token")
		return
	}
	if err := a.store.RevokeAPIToken(r.Context(), record.ID, a.cfg.Now().UTC()); err != nil {
		a.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func clientHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

var _ = fmt.Sprintf
