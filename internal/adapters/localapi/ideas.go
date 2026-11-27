package localapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ideasview"
)

// Idea list bounds from the local API contract. The CLI caps a listing at 200;
// the desktop page asks for more and the API allows up to 1000.
const (
	ideaListDefaultLimit = 100
	ideaListMaxLimit     = 1000
)

// IdeaService is the idea use-case surface the local API serves. It is exactly
// the app methods, so *app.App satisfies it without another adapter, and every
// route reuses the store, limit, redaction and problem-row behaviour of the CLI
// and MCP instead of reimplementing it.
type IdeaService interface {
	ListIdeas(context.Context, app.IdeaListOptions) (app.IdeaListResult, error)
	AddIdea(context.Context, app.IdeaInput) (app.IdeaResult, error)
	GetIdea(context.Context, string) (domain.Idea, error)
	UpdateIdea(context.Context, app.IdeaUpdateInput) (app.IdeaUpdateResult, error)
	TrashIdea(context.Context, string, string) (domain.Idea, error)
	RestoreIdea(context.Context, string, string) (domain.Idea, error)
	IdeaBrief(context.Context, app.IdeaBriefOptions) (app.IdeaBriefResult, error)
}

// The idea row, problem row and response envelopes are shared with the CLI
// (internal/ideasview), so the two frozen surfaces cannot drift.

// ideaCreateRequest is POST /v1/ideas. Every field except title is optional;
// pointers distinguish an omitted value from an explicit empty one.
type ideaCreateRequest struct {
	Title               string             `json:"title"`
	Body                string             `json:"body"`
	Project             string             `json:"project"`
	Status              string             `json:"status"`
	Priority            string             `json:"priority"`
	DueAt               *string            `json:"due_at"`
	DueDate             *string            `json:"due_date"`
	RemindBeforeMinutes *int               `json:"remind_before_minutes"`
	Tags                *[]string          `json:"tags"`
	Source              *domain.IdeaSource `json:"source"`
	CompletionNote      string             `json:"completion_note"`
}

// ideaPatchRequest is PATCH /v1/ideas/{id}. Omitted means unchanged; the clear
// flags are the only way to remove a due, a reminder lead or the tags.
type ideaPatchRequest struct {
	Title               *string   `json:"title"`
	Body                *string   `json:"body"`
	Project             *string   `json:"project"`
	Status              *string   `json:"status"`
	Priority            *string   `json:"priority"`
	DueAt               *string   `json:"due_at"`
	DueDate             *string   `json:"due_date"`
	RemindBeforeMinutes *int      `json:"remind_before_minutes"`
	Tags                *[]string `json:"tags"`
	ClearDue            bool      `json:"clear_due"`
	ClearReminder       bool      `json:"clear_reminder"`
	ClearTags           bool      `json:"clear_tags"`
	ExpectedUpdatedAt   *string   `json:"expected_updated_at"`
	CompletionNote      *string   `json:"completion_note"`
}

// ideas serves GET and POST /v1/ideas.
func (h *Handler) ideas(w http.ResponseWriter, r *http.Request) {
	if h.opts.Ideas == nil {
		unavailable(w)
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.listIdeas(w, r)
	case http.MethodPost:
		h.createIdea(w, r)
	default:
		method(w, r, http.MethodGet, http.MethodPost)
	}
}

// ideaRoute serves the id-addressed idea routes. The literal brief and ical
// paths are matched by the router before this prefix route.
func (h *Handler) ideaRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts, ok := pathParts(rest)
	if !ok || len(parts) == 0 || len(parts) > 2 {
		writeError(w, http.StatusNotFound, "not_found", "idea not found")
		return
	}
	if len(parts) == 2 {
		if parts[1] != "restore" {
			writeError(w, http.StatusNotFound, "not_found", "endpoint not found")
			return
		}
		if !method(w, r, http.MethodPost) {
			return
		}
		h.restoreIdea(w, r, parts[0])
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.getIdea(w, r, parts[0])
	case http.MethodPatch:
		h.patchIdea(w, r, parts[0])
	case http.MethodDelete:
		h.trashIdea(w, r, parts[0])
	default:
		method(w, r, http.MethodGet, http.MethodPatch, http.MethodDelete)
	}
}

// listIdeas applies the contract filters. An omitted project lists every
// registered project; the store applies every predicate before the limit, so
// total counts matches before the page bound.
func (h *Handler) listIdeas(w http.ResponseWriter, r *http.Request) {
	if h.opts.Ideas == nil {
		unavailable(w)
		return
	}
	limit, ok := h.queryLimit(w, r, ideaListDefaultLimit, ideaListMaxLimit)
	if !ok {
		return
	}
	statuses, ok := ideaStatusFilter(w, r)
	if !ok {
		return
	}
	dueBefore, ok := ideaQueryTime(w, r, "due_before")
	if !ok {
		return
	}
	dueAfter, ok := ideaQueryTime(w, r, "due_after")
	if !ok {
		return
	}
	project := queryText(r, "project")
	result, err := h.opts.Ideas.ListIdeas(r.Context(), app.IdeaListOptions{
		ProjectID: project,
		All:       project == "",
		Statuses:  statuses,
		Tags:      ideaQueryValues(r, "tags", "tag"),
		DueBefore: dueBefore,
		DueAfter:  dueAfter,
		Query:     queryText(r, "q"),
		// The store limit would hide matches from total; the handler caps the
		// page after counting.
		Limit: 0,
	})
	if err != nil {
		h.writeIdeaError(w, err)
		return
	}
	total := len(result.Ideas)
	ideas := result.Ideas
	if total > limit {
		ideas = ideas[:limit]
	}
	source := result.ProjectSource
	if project == "" {
		// An all-project listing has no single resolved project.
		source = ""
	}
	writeJSON(w, http.StatusOK, ideasview.List{
		Ideas:         ideasview.Rows(ideas, false),
		Problems:      ideasview.Problems(h.opts.DataDir, result.Problems),
		Total:         total,
		ProjectSource: source,
	})
}

// createIdea validates the request shape, then reuses AddIdea for project
// resolution, redaction, limits, id allocation and the search projection.
func (h *Handler) createIdea(w http.ResponseWriter, r *http.Request) {
	if h.opts.Ideas == nil {
		unavailable(w)
		return
	}
	var body ideaCreateRequest
	if !h.decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Title) == "" {
		writeError(w, http.StatusBadRequest, "validation", "idea.title is required")
		return
	}
	input := app.IdeaInput{
		ProjectID:           body.Project,
		Title:               body.Title,
		Body:                body.Body,
		Status:              domain.IdeaStatus(strings.TrimSpace(body.Status)),
		Priority:            domain.IdeaPriority(strings.TrimSpace(body.Priority)),
		RemindBeforeMinutes: body.RemindBeforeMinutes,
		Source:              body.Source,
		CompletionNote:      body.CompletionNote,
	}
	if body.Tags != nil {
		input.Tags = append([]string(nil), (*body.Tags)...)
	}
	if body.DueAt != nil {
		if *body.DueAt == "" {
			writeError(w, http.StatusBadRequest, "validation", "due_at must be an RFC3339 timestamp")
			return
		}
		due, err := time.Parse(time.RFC3339, *body.DueAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation", "due_at must be an RFC3339 timestamp")
			return
		}
		input.DueAt = &due
	}
	if body.DueDate != nil {
		if *body.DueDate == "" {
			writeError(w, http.StatusBadRequest, "validation", "due_date must be a YYYY-MM-DD date; omit it instead")
			return
		}
		input.DueDate = *body.DueDate
	}
	if input.DueAt != nil && input.DueDate != "" {
		writeError(w, http.StatusBadRequest, "validation", "due_at and due_date are mutually exclusive")
		return
	}
	result, err := h.opts.Ideas.AddIdea(r.Context(), input)
	if err != nil {
		h.writeIdeaError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ideasview.Write{
		Idea:          ideasview.RowOf(result.Idea, true),
		ProjectSource: result.ProjectSource,
	})
}

// getIdea returns one idea with its body. A problem file is a conflict with
// its path and reason, never a zero idea.
func (h *Handler) getIdea(w http.ResponseWriter, r *http.Request, id string) {
	if h.opts.Ideas == nil {
		unavailable(w)
		return
	}
	idea, err := h.opts.Ideas.GetIdea(r.Context(), id)
	if err != nil {
		h.writeIdeaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ideasview.Show{Idea: ideasview.RowOf(idea, true)})
}

// patchIdea applies the mutable fields and the clear flags. expected_updated_at
// makes the write a compare-and-swap; a stale value is a 409 that writes
// nothing.
func (h *Handler) patchIdea(w http.ResponseWriter, r *http.Request, id string) {
	if h.opts.Ideas == nil {
		unavailable(w)
		return
	}
	var body ideaPatchRequest
	if !h.decode(w, r, &body) {
		return
	}
	if ideaPatchEmpty(body) {
		writeError(w, http.StatusBadRequest, "validation",
			"nothing to change; pass title, body, project, status, priority, due, reminder, tags or a clear flag")
		return
	}
	input := app.IdeaUpdateInput{
		ID:                  id,
		Title:               body.Title,
		Body:                body.Body,
		RemindBeforeMinutes: body.RemindBeforeMinutes,
		Tags:                body.Tags,
		ClearDue:            body.ClearDue,
		ClearReminder:       body.ClearReminder,
		ClearTags:           body.ClearTags,
		CompletionNote:      body.CompletionNote,
	}
	if body.Project != nil {
		input.ProjectID = *body.Project
	}
	if body.DueAt != nil {
		if *body.DueAt == "" {
			writeError(w, http.StatusBadRequest, "validation", "due_at must be an RFC3339 timestamp")
			return
		}
		due, err := time.Parse(time.RFC3339, *body.DueAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation", "due_at must be an RFC3339 timestamp")
			return
		}
		input.DueAt = &due
	}
	if body.DueDate != nil {
		if *body.DueDate == "" {
			writeError(w, http.StatusBadRequest, "validation", "due_date must be a YYYY-MM-DD date; use clear_due")
			return
		}
		input.DueDate = body.DueDate
	}
	if body.Status != nil {
		status, err := domain.ParseIdeaStatus(*body.Status)
		if err != nil {
			h.writeIdeaError(w, err)
			return
		}
		input.Status = &status
	}
	if body.Priority != nil {
		priority, err := domain.ParseIdeaPriority(*body.Priority)
		if err != nil {
			h.writeIdeaError(w, err)
			return
		}
		input.Priority = &priority
	}
	if body.ExpectedUpdatedAt != nil {
		expected, err := time.Parse(time.RFC3339, *body.ExpectedUpdatedAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation", "expected_updated_at must be an RFC3339 timestamp")
			return
		}
		input.ExpectedUpdatedAt = expected
	}
	result, err := h.opts.Ideas.UpdateIdea(r.Context(), input)
	if err != nil {
		h.writeIdeaError(w, err)
		return
	}
	// App sets ProjectSource only when the request moved the idea; omitempty
	// keeps an in-place edit's response to just the idea.
	writeJSON(w, http.StatusOK, ideasview.Write{
		Idea:          ideasview.RowOf(result.Idea, true),
		ProjectSource: result.ProjectSource,
	})
}

// trashIdea is the reversible soft delete: the file moves to the trash and is
// never removed.
func (h *Handler) trashIdea(w http.ResponseWriter, r *http.Request, id string) {
	if h.opts.Ideas == nil {
		unavailable(w)
		return
	}
	idea, err := h.opts.Ideas.TrashIdea(r.Context(), "", id)
	if err != nil {
		h.writeIdeaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ideasview.Trash{ID: idea.ID, Trashed: true})
}

// restoreIdea brings one trashed idea back. A live file with the same id is a
// conflict, never overwritten.
func (h *Handler) restoreIdea(w http.ResponseWriter, r *http.Request, id string) {
	if h.opts.Ideas == nil {
		unavailable(w)
		return
	}
	idea, err := h.opts.Ideas.RestoreIdea(r.Context(), "", id)
	if err != nil {
		h.writeIdeaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ideasview.Show{Idea: ideasview.RowOf(idea, true)})
}

// ideasBrief serves GET /v1/ideas/brief: the same capped session block the CLI,
// MCP and the session hook render.
func (h *Handler) ideasBrief(w http.ResponseWriter, r *http.Request) {
	if h.opts.Ideas == nil {
		unavailable(w)
		return
	}
	if !method(w, r, http.MethodGet) {
		return
	}
	scope := strings.ToLower(queryText(r, "scope"))
	switch scope {
	case "":
		scope = app.IdeaBriefScopeAll
	case app.IdeaBriefScopeAll, app.IdeaBriefScopeCurrent:
	default:
		writeError(w, http.StatusBadRequest, "validation", "scope must be current or all")
		return
	}
	max, ok := h.queryBoundedInt(w, r, "max", app.DefaultIdeaBriefMax, 1, app.MaxIdeaBriefMax)
	if !ok {
		return
	}
	result, err := h.opts.Ideas.IdeaBrief(r.Context(), app.IdeaBriefOptions{
		ProjectID: queryText(r, "project"),
		Scope:     scope,
		Max:       max,
	})
	if err != nil {
		h.writeIdeaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ideasview.Brief{
		Text:     result.Text,
		Ideas:    ideasview.Rows(result.Ideas, false),
		Open:     result.Open,
		DueToday: result.DueToday,
		Muted:    result.Muted,
	})
}

// ideasCalendar serves GET /v1/ideas/ical with the shared renderer, so the
// DATE/DATE-TIME and VALARM rules cannot drift from the CLI export. An
// explicit status filter wins over the default set; include_done only widens
// that default. Dropped ideas are terminal and stay out of the export.
func (h *Handler) ideasCalendar(w http.ResponseWriter, r *http.Request) {
	if h.opts.Ideas == nil {
		unavailable(w)
		return
	}
	if !method(w, r, http.MethodGet) {
		return
	}
	includeDone, ok := h.queryFlag(w, r, "include_done")
	if !ok {
		return
	}
	statuses, ok := ideaStatusFilter(w, r)
	if !ok {
		return
	}
	if len(statuses) == 0 {
		statuses = []domain.IdeaStatus{domain.IdeaOpen, domain.IdeaDoing, domain.IdeaSomeday}
		if includeDone {
			statuses = append(statuses, domain.IdeaDone)
		}
	} else {
		filtered := make([]domain.IdeaStatus, 0, len(statuses))
		for _, status := range statuses {
			if status != domain.IdeaDropped {
				filtered = append(filtered, status)
			}
		}
		statuses = filtered
		if len(statuses) == 0 {
			// A status filter that names only dropped has nothing to export.
			w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, app.RenderIdeaCalendar(nil, h.opts.Now()))
			return
		}
	}
	project := queryText(r, "project")
	result, err := h.opts.Ideas.ListIdeas(r.Context(), app.IdeaListOptions{
		ProjectID: project,
		All:       project == "",
		Statuses:  statuses,
		Tags:      ideaQueryValues(r, "tags", "tag"),
		Query:     queryText(r, "q"),
		Limit:     0,
	})
	if err != nil {
		h.writeIdeaError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, app.RenderIdeaCalendar(result.Ideas, h.opts.Now()))
}

// ideaStatusFilter parses the repeated or comma-separated status filter.
func ideaStatusFilter(w http.ResponseWriter, r *http.Request) ([]domain.IdeaStatus, bool) {
	values := ideaQueryValues(r, "status")
	statuses := make([]domain.IdeaStatus, 0, len(values))
	for _, value := range values {
		status, err := domain.ParseIdeaStatus(value)
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation", err.Error())
			return nil, false
		}
		statuses = append(statuses, status)
	}
	return statuses, true
}

// ideaQueryTime parses an optional RFC3339 bound. The store treats due_after as
// inclusive and due_before as exclusive.
func ideaQueryTime(w http.ResponseWriter, r *http.Request, name string) (time.Time, bool) {
	raw := queryText(r, name)
	if raw == "" {
		return time.Time{}, true
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "validation", name+" must be an RFC3339 timestamp")
		return time.Time{}, false
	}
	return value, true
}

// ideaQueryValues reads repeated or comma-separated values of one or more
// parameter names, preserving order and dropping only exact repeats.
func ideaQueryValues(r *http.Request, names ...string) []string {
	seen := make(map[string]bool)
	values := make([]string, 0, len(names))
	for _, name := range names {
		for _, raw := range r.URL.Query()[name] {
			for _, part := range strings.Split(raw, ",") {
				value := strings.TrimSpace(part)
				if value == "" || seen[value] {
					continue
				}
				seen[value] = true
				values = append(values, value)
			}
		}
	}
	return values
}

// ideaPatchEmpty reports a request that would change nothing. A lone
// expected_updated_at is not a change: a compare-and-swap still needs a field
// to write.
func ideaPatchEmpty(body ideaPatchRequest) bool {
	return body.Title == nil && body.Body == nil && body.Project == nil &&
		body.Status == nil && body.Priority == nil && body.DueAt == nil &&
		body.DueDate == nil && body.RemindBeforeMinutes == nil && body.Tags == nil &&
		body.CompletionNote == nil &&
		!body.ClearDue && !body.ClearReminder && !body.ClearTags
}

// writeIdeaError maps the idea use-case errors onto the existing envelope.
// Problem files and revoked revisions are conflicts with structured detail;
// write limits stay 413 and a protected project stays 403.
func (h *Handler) writeIdeaError(w http.ResponseWriter, err error) {
	var problem *domain.ProblemError
	var revision *domain.RevisionConflictError
	var limit *domain.LimitError
	var protected *domain.ProtectedError
	var duplicate *domain.DuplicateError
	switch {
	case errors.As(err, &problem):
		message := ideasview.RelativePath(h.opts.DataDir, problem.Path) + ": " + problem.Reason
		writeError(w, http.StatusConflict, "conflict", h.redact(message))
	case errors.As(err, &revision):
		writeErrorRevision(w, h.redact(revision.Error()), revision.CurrentRevision())
	case errors.As(err, &limit):
		writeError(w, http.StatusRequestEntityTooLarge, "limit_exceeded", h.redact(limit.Error()))
	case errors.As(err, &protected):
		writeError(w, http.StatusForbidden, "forbidden", h.redact(protected.Error()))
	case errors.As(err, &duplicate):
		writeError(w, http.StatusConflict, "duplicate", h.redact(duplicate.Error()))
	case errors.Is(err, domain.ErrValidation):
		writeError(w, http.StatusBadRequest, "validation", h.redact(err.Error()))
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "idea not found")
	case errors.Is(err, domain.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "resource is busy or conflicts with existing data")
	case errors.Is(err, domain.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "the engine refuses this write")
	case errors.Is(err, domain.ErrUnsupported):
		writeError(w, http.StatusBadRequest, "unsupported", "the engine cannot serve ideas in the current mode")
	case errors.Is(err, app.ErrNotImplemented):
		unavailable(w)
	default:
		internal(w)
	}
}
