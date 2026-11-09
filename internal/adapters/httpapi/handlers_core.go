package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type createUserRequest struct {
	Login       string      `json:"login"`
	DisplayName string      `json:"displayName"`
	Password    string      `json:"password"`
	Role        domain.Role `json:"role"`
	AuthSource  string      `json:"authSource"`
}

func (a *API) createUser(w http.ResponseWriter, r *http.Request) {
	var body createUserRequest
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	body.Login = strings.TrimSpace(body.Login)
	if body.Login == "" {
		a.writeError(w, r, domain.NewValidationError("user.login", "is required"))
		return
	}
	if body.Role == "" {
		body.Role = domain.RoleMember
	}
	if !body.Role.Valid() {
		a.writeError(w, r, domain.NewValidationError("user.role", "unknown role"))
		return
	}
	body.AuthSource = strings.TrimSpace(body.AuthSource)
	if body.AuthSource == "" {
		body.AuthSource = "local"
	}
	if body.AuthSource != "local" && body.AuthSource != "oidc" {
		a.writeError(w, r, domain.NewValidationError("user.authSource", "must be local or oidc"))
		return
	}
	var passwordHash string
	if body.AuthSource == "local" {
		if err := auth.ValidatePassword(body.Password); err != nil {
			a.writeError(w, r, domain.NewValidationError("user.password", err.Error()))
			return
		}
		hash, err := auth.HashPassword(body.Password, a.cfg.PasswordParams)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		passwordHash = hash
	}
	record, revision, err := a.store.CreateUser(r.Context(), domain.User{
		Login:       body.Login,
		DisplayName: strings.TrimSpace(body.DisplayName),
		Role:        body.Role,
		AuthSource:  body.AuthSource,
	}, passwordHash)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "user", record.ID, "create")
	writeJSON(w, http.StatusCreated, record)
}

func (a *API) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.store.ListUsers(r.Context())
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": users})
}

func (a *API) getUser(w http.ResponseWriter, r *http.Request) {
	record, err := a.store.GetUser(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

type updateUserRequest struct {
	DisplayName *string      `json:"displayName"`
	Role        *domain.Role `json:"role"`
	AuthSource  *string      `json:"authSource"`
	Revision    int64        `json:"revision"`
}

func (a *API) updateUser(w http.ResponseWriter, r *http.Request) {
	var body updateUserRequest
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	record, err := a.store.GetUser(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	user := record.User
	if body.DisplayName != nil {
		user.DisplayName = strings.TrimSpace(*body.DisplayName)
	}
	if body.Role != nil {
		if !body.Role.Valid() {
			a.writeError(w, r, domain.NewValidationError("user.role", "unknown role"))
			return
		}
		user.Role = *body.Role
	}
	if body.AuthSource != nil {
		source := strings.TrimSpace(*body.AuthSource)
		if source != "local" && source != "oidc" {
			a.writeError(w, r, domain.NewValidationError("user.authSource", "must be local or oidc"))
			return
		}
		user.AuthSource = source
	}
	updated, revision, err := a.store.UpdateUser(r.Context(), user, body.Revision)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "user", updated.ID, "update")
	writeJSON(w, http.StatusOK, updated)
}

func (a *API) deleteUser(w http.ResponseWriter, r *http.Request) {
	revision, err := a.store.DeleteUser(r.Context(), r.PathValue("id"), queryInt64(r, "revision"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "user", r.PathValue("id"), "delete")
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) createProject(w http.ResponseWriter, r *http.Request) {
	var project domain.Project
	if err := a.decodeJSON(w, r, &project); err != nil {
		a.writeError(w, r, err)
		return
	}
	// A project-limited token may only create projects on its allow-list.
	if grant := grantFrom(r.Context()); grant.projectLimited() && !grant.allowsProject(project.ID) {
		writeErrorCode(w, http.StatusForbidden, "forbidden", "api token is limited to other projects")
		return
	}
	record, revision, err := a.store.CreateProject(r.Context(), project)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "project", record.ID, "create")
	writeJSON(w, http.StatusCreated, record)
}

func (a *API) listProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := a.store.ListProjects(r.Context())
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	view, err := a.accessView(r.Context())
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	visible := make([]ProjectRecord, 0, len(projects))
	for _, project := range projects {
		access := view.project(project.ID)
		if access.level < levelRead {
			continue
		}
		project.Restricted = access.restricted
		visible = append(visible, project)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": visible})
}

func (a *API) getProject(w http.ResponseWriter, r *http.Request) {
	access, ok := a.requireProject(w, r, r.PathValue("id"), levelRead)
	if !ok {
		return
	}
	record, err := a.store.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	record.Restricted = access.restricted
	writeJSON(w, http.StatusOK, record)
}

type updateProjectRequest struct {
	Name          *string             `json:"name"`
	Paths         *[]string           `json:"paths"`
	Aliases       *[]string           `json:"aliases"`
	Kind          *domain.ProjectKind `json:"kind"`
	RepoURL       *string             `json:"repoUrl"`
	DefaultBranch *string             `json:"defaultBranch"`
	Revision      int64               `json:"revision"`
}

func (a *API) updateProject(w http.ResponseWriter, r *http.Request) {
	var body updateProjectRequest
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	if !a.requireProjectManage(w, r, r.PathValue("id")) {
		return
	}
	record, err := a.store.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	project := record.Project
	if body.Name != nil {
		project.Name = strings.TrimSpace(*body.Name)
	}
	if body.Paths != nil {
		project.Paths = *body.Paths
	}
	if body.Aliases != nil {
		project.Aliases = *body.Aliases
	}
	if body.Kind != nil {
		if !body.Kind.Valid() {
			a.writeError(w, r, domain.NewValidationError("project.kind", "unknown kind"))
			return
		}
		project.Kind = *body.Kind
	}
	if body.RepoURL != nil {
		project.RepoURL = strings.TrimSpace(*body.RepoURL)
	}
	if body.DefaultBranch != nil {
		project.DefaultBranch = strings.TrimSpace(*body.DefaultBranch)
	}
	updated, revision, err := a.store.UpdateProject(r.Context(), project, body.Revision)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "project", updated.ID, "update")
	writeJSON(w, http.StatusOK, updated)
}

func (a *API) deleteProject(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireProject(w, r, r.PathValue("id"), levelMaintain); !ok {
		return
	}
	revision, err := a.store.DeleteProject(r.Context(), r.PathValue("id"), queryInt64(r, "revision"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "project", r.PathValue("id"), "delete")
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) getState(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireProject(w, r, r.PathValue("id"), levelRead); !ok {
		return
	}
	record, err := a.store.GetState(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

type putStateRequest struct {
	Goal          string     `json:"goal"`
	Active        []string   `json:"active"`
	Blocked       []string   `json:"blocked"`
	NextAction    string     `json:"nextAction"`
	OpenQuestions []string   `json:"openQuestions"`
	FreshUntil    *time.Time `json:"freshUntil"`
	Revision      int64      `json:"revision"`
}

func (a *API) putState(w http.ResponseWriter, r *http.Request) {
	var body putStateRequest
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	if _, ok := a.requireProject(w, r, r.PathValue("id"), levelWrite); !ok {
		return
	}
	state := domain.State{
		ProjectID:     r.PathValue("id"),
		Goal:          body.Goal,
		Active:        body.Active,
		Blocked:       body.Blocked,
		NextAction:    body.NextAction,
		OpenQuestions: body.OpenQuestions,
		FreshUntil:    derefTime(body.FreshUntil),
	}
	record, revision, err := a.store.PutState(r.Context(), state, body.Revision)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publishProject(revision, "project_state", state.ProjectID, "put", state.ProjectID)
	writeJSON(w, http.StatusOK, record)
}

func (a *API) search(w http.ResponseWriter, r *http.Request) {
	text := queryString(r, "q")
	if text == "" {
		a.writeError(w, r, domain.NewValidationError("search.text", "q is required"))
		return
	}
	kinds, err := searchKindParams(r, "kind")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	excludeKinds, err := searchKindParams(r, "exclude_kind")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	since, err := searchSinceParam(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	query := ports.Query{
		Text:         text,
		ProjectID:    queryString(r, "projectId"),
		Limit:        queryInt(r, "limit"),
		Kinds:        kinds,
		ExcludeKinds: excludeKinds,
		Since:        since,
	}
	scope, empty, ok := a.searchScope(w, r, query.ProjectID)
	if !ok {
		return
	}
	if empty {
		writeJSON(w, http.StatusOK, map[string]any{"items": []ports.Hit{}})
		return
	}
	hits, err := a.store.SearchScoped(r.Context(), query, scope)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": hits})
}

// searchKinds lists the document kinds a search may filter on. Raw-zone
// external items use "item"; clients request them explicitly like dumps.
var searchKinds = map[string]struct{}{
	"note": {}, "decision": {}, "attempt": {}, "handoff": {}, "state": {}, "dump": {}, "item": {},
}

// searchSinceParam reads the optional since query parameter (RFC3339). An
// empty value means no time filter; a malformed one is a validation error.
func searchSinceParam(r *http.Request) (time.Time, error) {
	raw := queryString(r, "since")
	if raw == "" {
		return time.Time{}, nil
	}
	since, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, domain.NewValidationError("search.since", "must be an RFC3339 time")
	}
	return since.UTC(), nil
}

// searchKindParams reads repeated kind query parameters. Blank values are
// ignored; an unknown kind is a validation error.
func searchKindParams(r *http.Request, name string) ([]string, error) {
	values := r.URL.Query()[name]
	kinds := make([]string, 0, len(values))
	for _, raw := range values {
		kind := strings.TrimSpace(raw)
		if kind == "" {
			continue
		}
		if _, ok := searchKinds[kind]; !ok {
			return nil, domain.NewValidationError("search."+name, "unknown kind "+strconv.Quote(kind))
		}
		kinds = append(kinds, kind)
	}
	return kinds, nil
}

func (a *API) listRevisions(w http.ResponseWriter, r *http.Request) {
	limit, offset := limitOffset(r)
	records, err := a.store.ListRevisions(r.Context(), RevisionFilter{
		AfterID:    queryInt64(r, "after"),
		EntityKind: queryString(r, "entityKind"),
		EntityID:   queryString(r, "entityId"),
		Limit:      limit,
		Offset:     offset,
	})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	view, err := a.accessView(r.Context())
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	// Pages can be shorter than limit for callers that cannot read every
	// project; clients page by revision id (after), not by page length.
	visible := make([]RevisionRecord, 0, len(records))
	for _, record := range records {
		if a.visibleRevision(r.Context(), view, record.EntityKind, record.EntityID, "", record.Detail) {
			visible = append(visible, record)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": visible})
}

func derefTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}
