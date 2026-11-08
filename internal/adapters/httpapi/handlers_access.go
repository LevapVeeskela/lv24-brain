package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Project access audit actions. Like other security events they carry no
// revision and are visible only through GET /v1/audit.
const (
	AuditProjectMemberAdded    = "project.member.added"
	AuditProjectMemberUpdated  = "project.member.updated"
	AuditProjectMemberRemoved  = "project.member.removed"
	AuditProjectAccessRestrict = "project.access.restricted"
	AuditProjectAccessOpen     = "project.access.opened"
	auditEntityProject         = "project"
)

// searchScope builds the project filter of a search. empty is true when the
// caller can read no project in the requested range, so the handler answers
// with no hits without querying. A requested projectId the caller cannot
// read is 404, like every other project-scoped endpoint.
func (a *API) searchScope(w http.ResponseWriter, r *http.Request, projectID string) (ProjectScope, bool, bool) {
	if projectID != "" {
		_, ok := a.requireProject(w, r, projectID, levelRead)
		return ProjectScope{}, false, ok
	}
	view, err := a.accessView(r.Context())
	if err != nil {
		a.writeError(w, r, err)
		return ProjectScope{}, false, false
	}
	if view.readsEverything() {
		return ProjectScope{}, false, true
	}
	scope := ProjectScope{Exclude: view.unreadable()}
	if view.grant.projectLimited() {
		for _, id := range view.grant.projects {
			if view.project(id).level >= levelRead {
				scope.Only = append(scope.Only, id)
			}
		}
		if len(scope.Only) == 0 {
			return ProjectScope{}, true, true
		}
	}
	return scope, false, true
}

func (a *API) listProjectMembers(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := a.requireProject(w, r, projectID, levelMaintain); !ok {
		return
	}
	record, err := a.store.GetProjectAccess(r.Context(), projectID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

type putProjectMemberRequest struct {
	Role domain.ProjectRole `json:"role"`
}

// putProjectMember adds or changes a membership. Adding the first member
// restricts the project.
func (a *API) putProjectMember(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	var body putProjectMemberRequest
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	if !body.Role.Valid() {
		a.writeError(w, r, domain.NewValidationError("member.role", "must be viewer, member or maintainer"))
		return
	}
	if _, ok := a.requireProject(w, r, projectID, levelMaintain); !ok {
		return
	}
	user, err := a.resolveUser(r, r.PathValue("user"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	stored, previous, err := a.store.PutProjectMember(r.Context(), domain.ProjectMember{
		ProjectID: projectID, UserID: user.ID, Role: body.Role,
	})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	action, status := AuditProjectMemberAdded, http.StatusCreated
	detail := map[string]any{"user_id": user.ID, "login": user.Login, "role": string(stored.Role)}
	if previous != "" {
		action, status = AuditProjectMemberUpdated, http.StatusOK
		detail["previous_role"] = string(previous)
	}
	a.audit(r, "", SecurityEvent{Action: action, EntityKind: auditEntityProject, EntityID: projectID, Detail: detail})
	writeJSON(w, status, stored)
}

// deleteProjectMember removes a membership. The project stays restricted
// even without members; reopening is an explicit PUT .../access.
func (a *API) deleteProjectMember(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, ok := a.requireProject(w, r, projectID, levelMaintain); !ok {
		return
	}
	user, err := a.resolveUser(r, r.PathValue("user"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	role, err := a.store.DeleteProjectMember(r.Context(), projectID, user.ID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.audit(r, "", SecurityEvent{
		Action: AuditProjectMemberRemoved, EntityKind: auditEntityProject, EntityID: projectID,
		Detail: map[string]any{"user_id": user.ID, "login": user.Login, "role": string(role)},
	})
	w.WriteHeader(http.StatusNoContent)
}

type putProjectAccessRequest struct {
	Restricted *bool `json:"restricted"`
}

// putProjectAccess restricts a project (admins only until members are added)
// or reopens it to every user per their global role. Reopening a project
// that still has members is 409.
func (a *API) putProjectAccess(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	var body putProjectAccessRequest
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	if body.Restricted == nil {
		a.writeError(w, r, domain.NewValidationError("access.restricted", "is required"))
		return
	}
	if _, ok := a.requireProject(w, r, projectID, levelMaintain); !ok {
		return
	}
	changed, err := a.store.SetProjectRestricted(r.Context(), projectID, *body.Restricted)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if changed {
		action := AuditProjectAccessOpen
		if *body.Restricted {
			action = AuditProjectAccessRestrict
		}
		a.audit(r, "", SecurityEvent{Action: action, EntityKind: auditEntityProject, EntityID: projectID})
	}
	record, err := a.store.GetProjectAccess(r.Context(), projectID)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

// resolveUser accepts a user id or a login.
func (a *API) resolveUser(r *http.Request, ref string) (domain.User, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return domain.User{}, domain.NewValidationError("member.user", "is required")
	}
	record, err := a.store.GetUser(r.Context(), ref)
	if err == nil {
		return record.User, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return domain.User{}, err
	}
	record, err = a.store.GetUserByLogin(r.Context(), ref)
	if err != nil {
		return domain.User{}, err
	}
	return record.User, nil
}
