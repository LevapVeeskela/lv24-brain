package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Security audit actions. They are recorded without a revision, so only
// admins see them (GET /v1/audit), never viewers through revisions or SSE.
const (
	AuditLoginSucceeded    = "auth.login.succeeded"
	AuditLoginFailed       = "auth.login.failed"
	AuditLoginRateLimited  = "auth.login.rate_limited"
	AuditLogout            = "auth.logout"
	AuditRefreshReused     = "auth.refresh.reused"
	AuditOIDCLogin         = "auth.oidc.login"
	AuditOIDCLinked        = "auth.oidc.linked"
	AuditOIDCSignup        = "auth.oidc.signup"
	AuditOIDCRefused       = "auth.oidc.refused"
	AuditAPITokenCreated   = "auth.token.created"
	AuditAPITokenRevoked   = "auth.token.revoked"
	AuditPasswordChanged   = "auth.password.changed"
	AuditPasswordFailed    = "auth.password.failed"
	AuditSessionsRevoked   = "auth.sessions.revoked"
	auditEntityUser        = "user"
	auditEntityAPIToken    = "api_token"
	maxMemoryAuditEvents   = 10000
	defaultAuditListLimit  = 50
	maximumAuditListLimit  = 500
	auditDetailClientField = "client"
)

// AuditEvent is one append-only audit record. Revision is zero for security
// events, which are not entity changes.
type AuditEvent struct {
	ID         int64          `json:"id"`
	Revision   int64          `json:"revision,omitempty"`
	Actor      string         `json:"actor,omitempty"`
	Action     string         `json:"action"`
	EntityKind string         `json:"entityKind,omitempty"`
	EntityID   string         `json:"entityId,omitempty"`
	Detail     map[string]any `json:"detail,omitempty"`
	CreatedAt  time.Time      `json:"createdAt"`
}

// AuditFilter narrows an audit listing.
type AuditFilter struct {
	Actor      string
	Action     string
	EntityKind string
	EntityID   string
	Since      *time.Time
	Until      *time.Time
	Limit      int
	Offset     int
}

// SecurityEvent is a security audit entry to record. Detail must never carry
// passwords, tokens, or other secrets.
type SecurityEvent struct {
	Action     string
	EntityKind string
	EntityID   string
	Detail     map[string]any
}

// audit records a security event on a best-effort basis: a failing audit
// write is logged but never turns a successful request into an error, and
// it never leaks to the caller. actor overrides the authenticated principal
// (used before authentication, e.g. the attempted login).
func (a *API) audit(r *http.Request, actor string, event SecurityEvent) {
	ctx := r.Context()
	if actor != "" {
		ctx = ports.WithActor(ctx, actor)
	}
	detail := make(map[string]any, len(event.Detail)+1)
	for key, value := range event.Detail {
		detail[key] = value
	}
	detail[auditDetailClientField] = clientIP(r, a.cfg.TrustedProxies)
	event.Detail = detail
	// Detach from the request so a client disconnect does not drop the row.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := a.store.RecordSecurityEvent(writeCtx, event); err != nil {
		a.logger.Error("audit write failed",
			"request_id", requestIDFrom(ctx),
			"action", event.Action,
			"error", err,
		)
	}
}

func (a *API) listAudit(w http.ResponseWriter, r *http.Request) {
	filter := AuditFilter{
		Actor:      queryString(r, "actor"),
		Action:     queryString(r, "action"),
		EntityKind: queryString(r, "entityKind"),
		EntityID:   queryString(r, "entityId"),
		Limit:      queryInt(r, "limit"),
		Offset:     queryInt(r, "offset"),
	}
	for name, target := range map[string]**time.Time{"since": &filter.Since, "until": &filter.Until} {
		raw := strings.TrimSpace(queryString(r, name))
		if raw == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			a.writeError(w, r, domain.NewValidationError("audit."+name, "must be an RFC 3339 timestamp"))
			return
		}
		*target = &parsed
	}
	events, err := a.store.ListAuditEvents(r.Context(), filter)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": events})
}

// clampAuditLimit applies the default and maximum page size.
func clampAuditLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultAuditListLimit
	case limit > maximumAuditListLimit:
		return maximumAuditListLimit
	default:
		return limit
	}
}
