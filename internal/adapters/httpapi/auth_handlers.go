package httpapi

import (
	"crypto/subtle"
	"errors"
	"fmt"
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
	if !a.limit.allow(clientIP(r, a.cfg.TrustedProxies)) {
		writeLoginRateLimited(w)
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
	// A second budget per account stops one login being brute-forced from
	// many addresses.
	if !a.accountLimit.allow(strings.ToLower(strings.TrimSpace(body.Login))) {
		a.audit(r, strings.TrimSpace(body.Login), SecurityEvent{Action: AuditLoginRateLimited, EntityKind: auditEntityUser})
		writeLoginRateLimited(w)
		return
	}
	session, err := a.auth.Login(r.Context(), ports.Credentials{Login: body.Login, Password: body.Password})
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			a.audit(r, strings.TrimSpace(body.Login), SecurityEvent{Action: AuditLoginFailed, EntityKind: auditEntityUser})
			writeErrorCode(w, http.StatusUnauthorized, "invalid_credentials", "invalid login or password")
			return
		}
		a.writeError(w, r, err)
		return
	}
	a.audit(r, session.User.Login, SecurityEvent{
		Action: AuditLoginSucceeded, EntityKind: auditEntityUser, EntityID: session.User.ID,
		Detail: map[string]any{"method": "password"},
	})
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
		if errors.Is(err, auth.ErrRefreshReused) {
			// Reuse of a rotated refresh token means it leaked; the auth
			// service revokes the family. Record it for admins.
			a.audit(r, "", SecurityEvent{Action: AuditRefreshReused, EntityKind: auditEntityUser})
		}
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
	a.audit(r, "", SecurityEvent{Action: AuditLogout, EntityKind: auditEntityUser, EntityID: principal.User.ID})
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
	// Bind the flow to this browser so a callback URL carrying someone else's
	// state and code (login CSRF) is rejected.
	http.SetCookie(w, oidcStateCookie(r, state, int(oidcFlowTTL/time.Second)))
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
	bound, err := r.Cookie(oidcStateCookieName)
	if err != nil || subtle.ConstantTimeCompare([]byte(bound.Value), []byte(state)) != 1 {
		writeErrorCode(w, http.StatusBadRequest, "invalid_state", "authorization state does not belong to this browser")
		return
	}
	http.SetCookie(w, oidcStateCookie(r, "", -1))
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
	user, err := a.resolveOIDCUser(r, claims)
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

func (a *API) resolveOIDCUser(r *http.Request, claims auth.OIDCClaims) (domain.User, error) {
	ctx := r.Context()
	oidcDetail := map[string]any{"issuer": claims.Issuer, "subject": claims.Subject}
	refuse := func(reason string, err error) (domain.User, error) {
		detail := map[string]any{"issuer": claims.Issuer, "subject": claims.Subject, "reason": reason}
		a.audit(r, claims.Login, SecurityEvent{Action: AuditOIDCRefused, EntityKind: auditEntityUser, Detail: detail})
		return domain.User{}, err
	}
	if claims.Issuer == "" || claims.Subject == "" {
		return domain.User{}, domain.NewValidationError("oidc.subject", "issuer and subject are required")
	}
	record, err := a.store.GetUserByOIDC(ctx, claims.Issuer, claims.Subject)
	if err == nil {
		a.audit(r, record.User.Login, SecurityEvent{Action: AuditOIDCLogin, EntityKind: auditEntityUser, EntityID: record.ID, Detail: oidcDetail})
		return record.User, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return domain.User{}, err
	}
	record, err = a.store.GetUserByLogin(ctx, claims.Login)
	switch {
	case err == nil:
		if record.OIDCIssuer != "" || record.OIDCSubject != "" {
			return refuse("linked_to_other_identity", domain.NewValidationError("oidc.subject", "account is linked to another identity provider"))
		}
		// Auto-linking an existing local account is an account takeover risk:
		// only a provider-verified email that equals the login may link, and
		// admin accounts are never linked implicitly.
		if !claims.EmailVerified || claims.Email == "" || !strings.EqualFold(claims.Email, record.Login) {
			a.logger.Warn("oidc login matches an existing account without a verified email; refusing to link",
				"request_id", requestIDFrom(ctx), "user_id", record.ID)
			return refuse("unverified_email", auth.ErrForbidden)
		}
		if record.Role == domain.RoleAdmin {
			a.logger.Warn("oidc login matches an admin account; refusing to link implicitly",
				"request_id", requestIDFrom(ctx), "user_id", record.ID)
			return refuse("admin_account", auth.ErrForbidden)
		}
		if _, err := a.store.SetUserOIDC(ctx, record.ID, claims.Issuer, claims.Subject); err != nil {
			return domain.User{}, err
		}
		a.audit(r, record.User.Login, SecurityEvent{Action: AuditOIDCLinked, EntityKind: auditEntityUser, EntityID: record.ID, Detail: oidcDetail})
		return record.User, nil
	case !errors.Is(err, domain.ErrNotFound):
		return domain.User{}, err
	}
	if !a.cfg.OIDCAllowSignup {
		return refuse("signup_disabled", auth.ErrForbidden)
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
	a.audit(r, created.User.Login, SecurityEvent{Action: AuditOIDCSignup, EntityKind: auditEntityUser, EntityID: created.ID, Detail: oidcDetail})
	return created.User, nil
}

type createTokenRequest struct {
	Name   string      `json:"name"`
	Role   domain.Role `json:"role"`
	UserID string      `json:"userId"`
	// ExpiresInDays and ExpiresAt are optional and mutually exclusive. Neither
	// set means the token never expires.
	ExpiresInDays *int    `json:"expiresInDays,omitempty"`
	ExpiresAt     *string `json:"expiresAt,omitempty"`
}

const (
	maxTokenExpiryDays  = 3650
	maxTokenExpiryYears = 10
)

// resolveTokenExpiry validates the optional expiry fields of a token request
// and returns the expiry instant, or nil for a token that never expires.
func resolveTokenExpiry(body createTokenRequest, now time.Time) (*time.Time, error) {
	if body.ExpiresInDays != nil && body.ExpiresAt != nil {
		return nil, domain.NewValidationError("token.expiresAt", "set either expiresInDays or expiresAt, not both")
	}
	if body.ExpiresInDays != nil {
		days := *body.ExpiresInDays
		if days < 1 || days > maxTokenExpiryDays {
			return nil, domain.NewValidationError("token.expiresInDays", "must be between 1 and 3650")
		}
		expiry := now.AddDate(0, 0, days)
		return &expiry, nil
	}
	if body.ExpiresAt != nil {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*body.ExpiresAt))
		if err != nil {
			return nil, domain.NewValidationError("token.expiresAt", "must be an RFC 3339 timestamp")
		}
		expiry := parsed.UTC()
		if !expiry.After(now) {
			return nil, domain.NewValidationError("token.expiresAt", "must be in the future")
		}
		if expiry.After(now.AddDate(maxTokenExpiryYears, 0, 0)) {
			return nil, domain.NewValidationError("token.expiresAt", "must be at most 10 years from now")
		}
		return &expiry, nil
	}
	return nil, nil
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
	now := a.cfg.Now().UTC()
	expiresAt, err := resolveTokenExpiry(body, now)
	if err != nil {
		a.writeError(w, r, err)
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
		CreatedAt: now,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var auditExpiry any
	if record.ExpiresAt != nil {
		auditExpiry = record.ExpiresAt.UTC().Format(time.RFC3339)
	}
	a.audit(r, "", SecurityEvent{
		Action: AuditAPITokenCreated, EntityKind: auditEntityAPIToken, EntityID: record.ID,
		Detail: map[string]any{
			"owner": record.UserID, "role": string(record.Role), "name": record.Name,
			"expires_at": auditExpiry,
		},
	})
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
	a.audit(r, "", SecurityEvent{
		Action: AuditAPITokenRevoked, EntityKind: auditEntityAPIToken, EntityID: record.ID,
		Detail: map[string]any{"owner": record.UserID},
	})
	w.WriteHeader(http.StatusNoContent)
}

var _ = fmt.Sprintf

const (
	// oidcFlowTTL bounds how long an OIDC login may stay pending.
	oidcFlowTTL = 10 * time.Minute
	// oidcStateCookieName binds a pending OIDC flow to the browser that
	// started it.
	oidcStateCookieName = "brain_oidc_state"
)

// oidcStateCookie builds the flow-binding cookie; maxAge < 0 deletes it. The
// cookie is Secure when the request arrived over TLS directly or through a
// proxy that reports https.
func oidcStateCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	secure := r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
	return &http.Cookie{
		Name:     oidcStateCookieName,
		Value:    value,
		Path:     "/v1/auth/oidc",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

func writeLoginRateLimited(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	writeErrorCode(w, http.StatusTooManyRequests, "rate_limited", "too many login attempts")
}
