package httpapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Authorization model (server mode). A request passes, in order:
//
//  1. authentication (JWT, API token or event-stream ticket);
//  2. token scopes, for API tokens that carry them (read/write/admin);
//  3. the route's minimum global role;
//  4. project access, in every handler that touches a project: the token's
//     project allow-list, then the project ACL. A project without a
//     restriction is open to every user per their global role (the behaviour
//     before ACLs). A restricted project admits only its members and admins;
//     the per-project role (viewer|member|maintainer) is capped by the global
//     role: a global viewer is read-only everywhere.
//
// An unreadable project is reported as 404 so its existence does not leak.

// tokenGrant is what an API token adds on top of its owner's role. A nil
// grant (JWT, unscoped token) adds no limits.
type tokenGrant struct {
	tokenID   string
	scopes    []domain.TokenScope
	projects  []string
	expiresAt *time.Time
}

func newTokenGrant(record APITokenRecord) *tokenGrant {
	if len(record.Scopes) == 0 && len(record.ProjectIDs) == 0 {
		return &tokenGrant{tokenID: record.ID, expiresAt: record.ExpiresAt}
	}
	return &tokenGrant{
		tokenID:   record.ID,
		scopes:    slices.Clone(record.Scopes),
		projects:  slices.Clone(record.ProjectIDs),
		expiresAt: record.ExpiresAt,
	}
}

// scoped reports whether the token carries explicit scopes.
func (g *tokenGrant) scoped() bool { return g != nil && len(g.scopes) > 0 }

// projectLimited reports whether the token carries a project allow-list.
func (g *tokenGrant) projectLimited() bool { return g != nil && len(g.projects) > 0 }

// allowsScope applies scope implication: write and admin imply read.
func (g *tokenGrant) allowsScope(need domain.TokenScope) bool {
	if !g.scoped() {
		return true
	}
	if slices.Contains(g.scopes, need) {
		return true
	}
	if need == domain.TokenScopeRead {
		return slices.Contains(g.scopes, domain.TokenScopeWrite) || slices.Contains(g.scopes, domain.TokenScopeAdmin)
	}
	return false
}

func (g *tokenGrant) allowsProject(projectID string) bool {
	return !g.projectLimited() || slices.Contains(g.projects, projectID)
}

type grantContextKey struct{}

func grantFrom(ctx context.Context) *tokenGrant {
	grant, _ := ctx.Value(grantContextKey{}).(*tokenGrant)
	return grant
}

// readOnlyRoutes are non-GET routes that only need the read scope: they
// change no shared data.
var readOnlyRoutes = map[string]bool{
	"POST /v1/events/ticket": true,
	"POST /v1/auth/logout":   true,
}

// requiredScope derives the token scope a route needs: admin-only routes need
// admin, safe methods need read, everything else needs write.
func requiredScope(pattern string, min domain.Role) domain.TokenScope {
	if min == domain.RoleAdmin {
		return domain.TokenScopeAdmin
	}
	method, _, _ := strings.Cut(pattern, " ")
	if method == http.MethodGet || method == http.MethodHead || readOnlyRoutes[pattern] {
		return domain.TokenScopeRead
	}
	return domain.TokenScopeWrite
}

// projectLevel is what a caller may do inside one project.
type projectLevel int

const (
	levelNone projectLevel = iota
	levelRead
	levelWrite
	levelMaintain
)

// projectAccess is the caller's resolved access to one project.
type projectAccess struct {
	level      projectLevel
	restricted bool
}

// accessView is the caller's access to every project, resolved once.
type accessView struct {
	admin        bool
	globalRole   domain.Role
	grant        *tokenGrant
	restrictions map[string]domain.ProjectRole
}

// accessView loads the caller's project restrictions (one store query).
func (a *API) accessView(ctx context.Context) (accessView, error) {
	principal, ok := principalFrom(ctx)
	if !ok {
		return accessView{}, auth.ErrInvalidToken
	}
	restrictions, err := a.store.ProjectRestrictions(ctx, principal.User.ID)
	if err != nil {
		return accessView{}, err
	}
	return accessView{
		admin:        principal.User.Role == domain.RoleAdmin,
		globalRole:   principal.User.Role,
		grant:        grantFrom(ctx),
		restrictions: restrictions,
	}, nil
}

// project resolves the caller's access to projectID.
func (v accessView) project(projectID string) projectAccess {
	role, restricted := v.restrictions[projectID]
	access := projectAccess{restricted: restricted}
	if !v.grant.allowsProject(projectID) {
		return access
	}
	switch {
	case v.admin:
		access.level = levelMaintain
	case !restricted:
		access.level = globalLevel(v.globalRole)
	default:
		access.level = min(memberLevel(role), restrictedCap(v.globalRole))
	}
	return access
}

// globalLevel is the level a global role grants in an open project.
func globalLevel(role domain.Role) projectLevel {
	switch role {
	case domain.RoleAdmin:
		return levelMaintain
	case domain.RoleMember:
		return levelWrite
	case domain.RoleViewer:
		return levelRead
	default:
		return levelNone
	}
}

// restrictedCap is the most a global role allows inside a restricted project:
// a global member may act as maintainer where the project grants it, a
// global viewer is never raised above read.
func restrictedCap(role domain.Role) projectLevel {
	switch role {
	case domain.RoleMember:
		return levelMaintain
	case domain.RoleViewer:
		return levelRead
	default:
		return levelNone
	}
}

func memberLevel(role domain.ProjectRole) projectLevel {
	switch role {
	case domain.ProjectRoleMaintainer:
		return levelMaintain
	case domain.ProjectRoleMember:
		return levelWrite
	case domain.ProjectRoleViewer:
		return levelRead
	default:
		return levelNone
	}
}

// readsEverything reports whether no project is hidden from the caller; list
// and event filtering then take the unfiltered fast path.
func (v accessView) readsEverything() bool {
	if v.grant.projectLimited() {
		return false
	}
	if v.admin {
		return true
	}
	for _, role := range v.restrictions {
		if role == "" {
			return false
		}
	}
	return true
}

// unreadable lists restricted projects the caller is not a member of.
func (v accessView) unreadable() []string {
	if v.admin {
		return nil
	}
	out := make([]string, 0)
	for projectID, role := range v.restrictions {
		if role == "" {
			out = append(out, projectID)
		}
	}
	slices.Sort(out)
	return out
}

// requireProject enforces project access in a handler. It answers 404 when
// the project is unreadable (no existence leak) and 403 when the caller can
// read but needs more. It returns false after writing the response.
func (a *API) requireProject(w http.ResponseWriter, r *http.Request, projectID string, need projectLevel) (projectAccess, bool) {
	view, err := a.accessView(r.Context())
	if err != nil {
		a.writeError(w, r, err)
		return projectAccess{}, false
	}
	access := view.project(projectID)
	if access.level < levelRead {
		writeErrorCode(w, http.StatusNotFound, "not_found", "not found")
		return access, false
	}
	if access.level < need {
		writeErrorCode(w, http.StatusForbidden, "forbidden", "insufficient project role")
		return access, false
	}
	return access, true
}

// requireProjectManage gates project metadata changes: members may edit open
// projects (the behaviour before ACLs); restricted projects need maintainer.
func (a *API) requireProjectManage(w http.ResponseWriter, r *http.Request, projectID string) bool {
	access, ok := a.requireProject(w, r, projectID, levelWrite)
	if !ok {
		return false
	}
	if access.restricted && access.level < levelMaintain {
		writeErrorCode(w, http.StatusForbidden, "forbidden", "only project maintainers can change a restricted project")
		return false
	}
	return true
}

// knowledgeKinds are revision entity kinds that belong to one project.
var knowledgeKinds = map[string]bool{"note": true, "decision": true, "attempt": true, "handoff": true}

// revisionProject resolves the project of a revision or event. ok is false
// when the entity is project data but its project is unknown (for example a
// delete recorded before revisions carried project_id); such entries are
// shown only to callers who can read everything.
func (a *API) revisionProject(ctx context.Context, kind, entityID, hinted string, detail map[string]any) (string, bool, bool) {
	switch {
	case kind == "project" || kind == "project_state":
		return entityID, true, true
	case !knowledgeKinds[kind]:
		return "", false, true
	}
	if hinted != "" {
		return hinted, true, true
	}
	if value, ok := detail[revisionProjectKey].(string); ok && value != "" {
		return value, true, true
	}
	projectID, err := a.lookupEntityProject(ctx, kind, entityID)
	if err != nil || projectID == "" {
		return "", true, false
	}
	return projectID, true, true
}

func (a *API) lookupEntityProject(ctx context.Context, kind, id string) (string, error) {
	switch kind {
	case "note":
		record, err := a.store.GetNote(ctx, id)
		return record.ProjectID, err
	case "decision":
		record, err := a.store.GetDecision(ctx, id)
		return record.ProjectID, err
	case "attempt":
		record, err := a.store.GetAttempt(ctx, id)
		return record.ProjectID, err
	case "handoff":
		record, err := a.store.GetHandoff(ctx, id)
		return record.ProjectID, err
	}
	return "", errors.New("unknown entity kind")
}

// visibleRevision reports whether the caller may see a revision or event.
// Non-project entries (users, connector runs) keep their previous visibility.
func (a *API) visibleRevision(ctx context.Context, view accessView, kind, entityID, hinted string, detail map[string]any) bool {
	if view.readsEverything() {
		return true
	}
	projectID, projectData, known := a.revisionProject(ctx, kind, entityID, hinted, detail)
	if !projectData {
		return true
	}
	if !known {
		return false
	}
	return view.project(projectID).level >= levelRead
}

// maxTokenProjects bounds a token's project allow-list.
const maxTokenProjects = 100

// resolveTokenLimits validates the optional scopes and project allow-list of
// a token request. Every listed project must exist.
func (a *API) resolveTokenLimits(r *http.Request, body createTokenRequest, role domain.Role) ([]domain.TokenScope, []string, error) {
	scopes, err := domain.NormalizeTokenScopes(body.Scopes)
	if err != nil {
		return nil, nil, err
	}
	if slices.Contains(scopes, domain.TokenScopeAdmin) && role != domain.RoleAdmin {
		return nil, nil, domain.NewValidationError("token.scopes", "the admin scope requires an admin token role")
	}
	projectIDs := make([]string, 0, len(body.ProjectIDs))
	for _, raw := range body.ProjectIDs {
		id := strings.TrimSpace(raw)
		if id == "" || slices.Contains(projectIDs, id) {
			continue
		}
		if _, err := a.store.GetProject(r.Context(), id); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return nil, nil, domain.NewValidationError("token.projectIds", "unknown project "+id)
			}
			return nil, nil, err
		}
		projectIDs = append(projectIDs, id)
	}
	if len(projectIDs) > maxTokenProjects {
		return nil, nil, domain.NewValidationError("token.projectIds", "at most 100 projects")
	}
	slices.Sort(projectIDs)
	if len(scopes) == 0 {
		scopes = nil
	}
	if len(projectIDs) == 0 {
		projectIDs = nil
	}
	return scopes, projectIDs, nil
}

// narrowTokenLimits keeps a token minted by another token from widening it:
// the new token inherits the caller's scopes, allow-list and expiry when it
// sets none and may only narrow them. JWT callers (nil grant) and unscoped
// tokens pass unchanged. It writes 403 and returns false when refused.
func narrowTokenLimits(w http.ResponseWriter, caller *tokenGrant, scopes []domain.TokenScope, projectIDs []string, expiresAt *time.Time) (*time.Time, bool) {
	if caller == nil {
		return expiresAt, true
	}
	if caller.scoped() {
		if len(scopes) == 0 {
			writeErrorCode(w, http.StatusForbidden, "forbidden", "a scoped api token can only mint tokens with a subset of its scopes")
			return nil, false
		}
		for _, scope := range scopes {
			if !caller.allowsScope(scope) {
				writeErrorCode(w, http.StatusForbidden, "forbidden", "token scope exceeds the calling token's scopes")
				return nil, false
			}
		}
	}
	if caller.projectLimited() {
		if len(projectIDs) == 0 {
			writeErrorCode(w, http.StatusForbidden, "forbidden", "a project-limited api token can only mint tokens for a subset of its projects")
			return nil, false
		}
		for _, id := range projectIDs {
			if !caller.allowsProject(id) {
				writeErrorCode(w, http.StatusForbidden, "forbidden", "token project exceeds the calling token's projects")
				return nil, false
			}
		}
	}
	if caller.expiresAt != nil {
		if expiresAt == nil {
			inherited := *caller.expiresAt
			return &inherited, true
		}
		if expiresAt.After(*caller.expiresAt) {
			writeErrorCode(w, http.StatusForbidden, "forbidden", "token expiry exceeds the calling token's expiry")
			return nil, false
		}
	}
	return expiresAt, true
}

// auditScopes renders scopes for the audit log; [] means unscoped.
func auditScopes(scopes []domain.TokenScope) []string {
	out := make([]string, len(scopes))
	for i, scope := range scopes {
		out[i] = string(scope)
	}
	return out
}
