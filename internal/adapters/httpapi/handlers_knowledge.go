package httpapi

import (
	"net/http"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

type noteRequest struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Tags      []string `json:"tags"`
	Permalink string   `json:"permalink"`
	Revision  int64    `json:"revision"`
}

func (a *API) listNotes(w http.ResponseWriter, r *http.Request) {
	limit, offset := limitOffset(r)
	items, err := a.store.ListNotes(r.Context(), ListOptions{
		ProjectID: r.PathValue("id"),
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) createNote(w http.ResponseWriter, r *http.Request) {
	var body noteRequest
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	record, revision, err := a.store.CreateNote(r.Context(), domain.Note{
		ProjectID: r.PathValue("id"),
		Title:     body.Title,
		Body:      body.Body,
		Tags:      body.Tags,
		Permalink: body.Permalink,
	})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "note", record.ID, "create")
	writeJSON(w, http.StatusCreated, record)
}

func (a *API) getNote(w http.ResponseWriter, r *http.Request) {
	record, err := a.store.GetNote(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (a *API) updateNote(w http.ResponseWriter, r *http.Request) {
	var body noteRequest
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	existing, err := a.store.GetNote(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if !a.requireOwner(w, r, existing.CreatedBy) {
		return
	}
	record, revision, err := a.store.UpdateNote(r.Context(), domain.Note{
		ID:        existing.ID,
		ProjectID: existing.ProjectID,
		Title:     body.Title,
		Body:      body.Body,
		Tags:      body.Tags,
		Permalink: body.Permalink,
	}, body.Revision)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "note", record.ID, "update")
	writeJSON(w, http.StatusOK, record)
}

func (a *API) deleteNote(w http.ResponseWriter, r *http.Request) {
	existing, err := a.store.GetNote(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if !a.requireOwner(w, r, existing.CreatedBy) {
		return
	}
	revision, err := a.store.DeleteNote(r.Context(), existing.ID, queryInt64(r, "revision"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "note", r.PathValue("id"), "delete")
	w.WriteHeader(http.StatusNoContent)
}

type decisionRequest struct {
	Title        string                `json:"title"`
	Status       domain.DecisionStatus `json:"status"`
	Context      string                `json:"context"`
	Alternatives []string              `json:"alternatives"`
	Consequences []string              `json:"consequences"`
	Evidence     []string              `json:"evidence"`
	Permalink    string                `json:"permalink"`
	Revision     int64                 `json:"revision"`
}

func (a *API) listDecisions(w http.ResponseWriter, r *http.Request) {
	limit, offset := limitOffset(r)
	items, err := a.store.ListDecisions(r.Context(), ListOptions{
		ProjectID: r.PathValue("id"),
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) createDecision(w http.ResponseWriter, r *http.Request) {
	var body decisionRequest
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	record, revision, err := a.store.CreateDecision(r.Context(), domain.Decision{
		ProjectID:    r.PathValue("id"),
		Title:        body.Title,
		Status:       body.Status,
		Context:      body.Context,
		Alternatives: body.Alternatives,
		Consequences: body.Consequences,
		Evidence:     body.Evidence,
		Permalink:    body.Permalink,
	})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "decision", record.ID, "create")
	writeJSON(w, http.StatusCreated, record)
}

func (a *API) getDecision(w http.ResponseWriter, r *http.Request) {
	record, err := a.store.GetDecision(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (a *API) updateDecision(w http.ResponseWriter, r *http.Request) {
	var body decisionRequest
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	existing, err := a.store.GetDecision(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if !a.requireOwner(w, r, existing.CreatedBy) {
		return
	}
	record, revision, err := a.store.UpdateDecision(r.Context(), domain.Decision{
		ID:           existing.ID,
		ProjectID:    existing.ProjectID,
		Title:        body.Title,
		Status:       body.Status,
		Context:      body.Context,
		Alternatives: body.Alternatives,
		Consequences: body.Consequences,
		Evidence:     body.Evidence,
		Permalink:    body.Permalink,
	}, body.Revision)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "decision", record.ID, "update")
	writeJSON(w, http.StatusOK, record)
}

func (a *API) deleteDecision(w http.ResponseWriter, r *http.Request) {
	existing, err := a.store.GetDecision(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if !a.requireOwner(w, r, existing.CreatedBy) {
		return
	}
	revision, err := a.store.DeleteDecision(r.Context(), existing.ID, queryInt64(r, "revision"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "decision", r.PathValue("id"), "delete")
	w.WriteHeader(http.StatusNoContent)
}

type attemptRequest struct {
	Title      string `json:"title"`
	Hypothesis string `json:"hypothesis"`
	Result     string `json:"result"`
	Reason     string `json:"reason"`
	Permalink  string `json:"permalink"`
	Revision   int64  `json:"revision"`
}

func (a *API) listAttempts(w http.ResponseWriter, r *http.Request) {
	limit, offset := limitOffset(r)
	items, err := a.store.ListAttempts(r.Context(), ListOptions{
		ProjectID: r.PathValue("id"),
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) createAttempt(w http.ResponseWriter, r *http.Request) {
	var body attemptRequest
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	record, revision, err := a.store.CreateAttempt(r.Context(), domain.Attempt{
		ProjectID:  r.PathValue("id"),
		Title:      body.Title,
		Hypothesis: body.Hypothesis,
		Result:     body.Result,
		Reason:     body.Reason,
		Permalink:  body.Permalink,
	})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "attempt", record.ID, "create")
	writeJSON(w, http.StatusCreated, record)
}

func (a *API) getAttempt(w http.ResponseWriter, r *http.Request) {
	record, err := a.store.GetAttempt(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (a *API) updateAttempt(w http.ResponseWriter, r *http.Request) {
	var body attemptRequest
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	existing, err := a.store.GetAttempt(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if !a.requireOwner(w, r, existing.CreatedBy) {
		return
	}
	record, revision, err := a.store.UpdateAttempt(r.Context(), domain.Attempt{
		ID:         existing.ID,
		ProjectID:  existing.ProjectID,
		Title:      body.Title,
		Hypothesis: body.Hypothesis,
		Result:     body.Result,
		Reason:     body.Reason,
		Permalink:  body.Permalink,
	}, body.Revision)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "attempt", record.ID, "update")
	writeJSON(w, http.StatusOK, record)
}

func (a *API) deleteAttempt(w http.ResponseWriter, r *http.Request) {
	existing, err := a.store.GetAttempt(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if !a.requireOwner(w, r, existing.CreatedBy) {
		return
	}
	revision, err := a.store.DeleteAttempt(r.Context(), existing.ID, queryInt64(r, "revision"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "attempt", r.PathValue("id"), "delete")
	w.WriteHeader(http.StatusNoContent)
}

type handoffRequest struct {
	Summary    string `json:"summary"`
	GitContext string `json:"gitContext"`
	NextAction string `json:"nextAction"`
	Permalink  string `json:"permalink"`
}

func (a *API) listHandoffs(w http.ResponseWriter, r *http.Request) {
	limit, offset := limitOffset(r)
	items, err := a.store.ListHandoffs(r.Context(), ListOptions{
		ProjectID: r.PathValue("id"),
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) createHandoff(w http.ResponseWriter, r *http.Request) {
	var body handoffRequest
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	record, revision, err := a.store.CreateHandoff(r.Context(), domain.Handoff{
		ProjectID:  r.PathValue("id"),
		Summary:    body.Summary,
		GitContext: body.GitContext,
		NextAction: body.NextAction,
		Permalink:  body.Permalink,
	})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "handoff", record.ID, "create")
	writeJSON(w, http.StatusCreated, record)
}

func (a *API) getHandoff(w http.ResponseWriter, r *http.Request) {
	record, err := a.store.GetHandoff(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (a *API) deleteHandoff(w http.ResponseWriter, r *http.Request) {
	existing, err := a.store.GetHandoff(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if !a.requireOwner(w, r, existing.CreatedBy) {
		return
	}
	revision, err := a.store.DeleteHandoff(r.Context(), existing.ID, queryInt64(r, "revision"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.publish(revision, "handoff", r.PathValue("id"), "delete")
	w.WriteHeader(http.StatusNoContent)
}

// requireOwner enforces "members change only their own knowledge": admins may
// update or delete any note, decision, attempt or handoff; members only those
// they created. Records with an unknown owner (created before ownership was
// tracked) are admin-only. It writes 403 and returns false when denied.
func (a *API) requireOwner(w http.ResponseWriter, r *http.Request, createdBy string) bool {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeAuthError(w)
		return false
	}
	if principal.User.Role == domain.RoleAdmin {
		return true
	}
	if createdBy != "" && createdBy == principal.User.ID {
		return true
	}
	writeErrorCode(w, http.StatusForbidden, "forbidden", "only the record owner or an admin can change this record")
	return false
}
