package httpapi

import (
	"net/http"

	"github.com/levapveeskela/lv24-brain/internal/redact"
)

// AuditKnowledgeRedacted records that a knowledge write carried credentials.
// The event names the entity and the count, never the text.
const AuditKnowledgeRedacted = "knowledge.redacted"

// redactTexts applies the shared ingestion redactor to every value in place
// and returns how many secrets it replaced. The server write path uses it so a
// credential pasted through the API is never persisted, indexed, or injected
// into another member's session.
func redactTexts(values ...*string) int {
	total := 0
	for _, value := range values {
		if value == nil {
			continue
		}
		redacted, count := redact.ApplyIngest(*value)
		*value = redacted
		total += count
	}
	return total
}

// redactList redacts every element of a list in place.
func redactList(values []string) int {
	total := 0
	for i := range values {
		total += redactTexts(&values[i])
	}
	return total
}

// redactWrite reports a write that contained credentials. A redaction here
// means a client pasted a secret (or a pattern false positive); operators get
// the entity and the count in the log and the admin-only audit trail, never
// the text itself.
func (a *API) redactWrite(r *http.Request, kind, projectID string, count int) {
	if count <= 0 {
		return
	}
	a.logger.Warn("knowledge write redacted secrets",
		"request_id", requestIDFrom(r.Context()),
		"kind", kind,
		"project", projectID,
		"redactions", count,
	)
	a.audit(r, "", SecurityEvent{
		Action:     AuditKnowledgeRedacted,
		EntityKind: kind,
		EntityID:   projectID,
		Detail:     map[string]any{"redactions": count},
	})
}
