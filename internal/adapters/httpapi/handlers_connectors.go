package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// localOnlyRefusal is the actionable message of a refused local-only run.
const localOnlyRefusal = "publish=local-only needs the explicit admin opt-in: send allowLocalOnly=true to run without storing items in the shared raw zone, or publish=shared to store them for project members"

// runConnector executes one connector through the configured runner.
//
// The publish policy is enforced here and again in the server runner:
//   - "shared" stores raw items for project members;
//   - "local-only" never reaches the shared item store in server mode, so it
//     needs the explicit admin opt-in (allowLocalOnly) and then drops items;
//   - empty keeps the provider default, which for mail/chat is local-only and
//     therefore also needs the opt-in.
//
// A refused attempt is audited with the provider and project, never the text.
func (a *API) runConnector(w http.ResponseWriter, r *http.Request) {
	if a.cfg.ConnectorRunner == nil {
		writeErrorCode(w, http.StatusNotImplemented, "unsupported", "connector runner is not configured")
		return
	}
	var body ConnectorRunRequest
	if err := a.decodeJSON(w, r, &body); err != nil {
		a.writeError(w, r, err)
		return
	}
	body.Name = strings.ToLower(strings.TrimSpace(body.Name))
	if body.Name == "" {
		a.writeError(w, r, domain.NewValidationError("connector.name", "is required"))
		return
	}
	if body.Limit < 0 {
		a.writeError(w, r, domain.NewValidationError("connector.limit", "must not be negative"))
		return
	}
	body.Publish = strings.ToLower(strings.TrimSpace(body.Publish))
	policy := domain.PublishPolicy(body.Publish)
	switch {
	case policy != "" && !policy.Valid():
		a.writeError(w, r, domain.NewValidationError("connector.publish", "must be shared or local-only"))
		return
	case body.AllowLocalOnly && policy == domain.PublishShared:
		a.writeError(w, r, domain.NewValidationError("connector.allowLocalOnly", "cannot be combined with publish=shared"))
		return
	case policy == domain.PublishLocalOnly && !body.AllowLocalOnly:
		a.audit(r, "", SecurityEvent{
			Action:     AuditConnectorLocalOnlyDenied,
			EntityKind: "connector",
			EntityID:   body.Name,
			Detail:     map[string]any{"projectId": body.ProjectID, "reason": "explicit admin opt-in required"},
		})
		writeErrorCode(w, http.StatusForbidden, "forbidden", localOnlyRefusal)
		return
	}
	projects := make([]string, 0, len(body.Projects))
	for _, project := range body.Projects {
		if trimmed := strings.TrimSpace(project); trimmed != "" {
			projects = append(projects, trimmed)
		}
	}
	body.Projects = projects

	run, err := a.cfg.ConnectorRunner.Run(r.Context(), body)
	if err != nil {
		if errors.Is(err, domain.ErrForbidden) {
			a.audit(r, "", SecurityEvent{
				Action:     AuditConnectorLocalOnlyDenied,
				EntityKind: "connector",
				EntityID:   body.Name,
				Detail:     map[string]any{"projectId": body.ProjectID, "reason": "provider defaults to local-only"},
			})
		}
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (a *API) listConnectorRuns(w http.ResponseWriter, r *http.Request) {
	status := domain.ConnectorStatus(strings.ToLower(queryString(r, "status")))
	if status != "" && !status.Valid() {
		a.writeError(w, r, domain.NewValidationError("connectorRun.status", "unknown status"))
		return
	}
	records, err := a.store.ListConnectorRuns(r.Context(), ConnectorRunFilter{
		Source: strings.ToLower(queryString(r, "source")),
		Status: status,
		Limit:  queryInt(r, "limit"),
		Offset: queryInt(r, "offset"),
	})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": records})
}

func (a *API) getConnectorRun(w http.ResponseWriter, r *http.Request) {
	record, err := a.store.GetConnectorRun(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (a *API) exportConnectorRun(w http.ResponseWriter, r *http.Request) {
	record, err := a.store.GetConnectorRun(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	raw, err := connectors.RenderRunMarkdown(record.ConnectorRun)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, connectorDownloadName(record.ID)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func connectorDownloadName(id string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, strings.TrimSpace(id))
	clean = strings.Trim(clean, ".-_")
	if clean == "" {
		clean = "connector-run"
	}
	if len(clean) > 80 {
		clean = clean[:80]
	}
	return clean + ".md"
}
