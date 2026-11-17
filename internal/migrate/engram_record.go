package migrate

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/redact"
)

const (
	engramRecordNote     = "note"
	engramRecordDecision = "decision"
	engramSessionMax     = 256
)

func engramExternalID(observation engramObservation) string {
	syncID := strings.TrimSpace(observation.SyncID)
	if syncID == "" {
		return "engram:local-" + fmt.Sprintf("%d", observation.ID)
	}
	return EngramSourceSystem + ":" + syncID
}

func engramSourcePath(observation engramObservation) string {
	return EngramSourceSystem + ":observations/" + strings.TrimPrefix(engramExternalID(observation), EngramSourceSystem+":")
}

func normalizeEngramTitle(value string) string {
	return strings.ToLower(singleLine(value))
}

// parseEngramTime accepts the UTC "YYYY-MM-DD HH:MM:SS" timestamps engram
// writes plus the RFC 3339 forms an export may use.
func parseEngramTime(value string) time.Time {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.EqualFold(trimmed, "null") {
		return time.Time{}
	}
	if parsed, ok := parseLegacyTime(trimmed); ok {
		return parsed.UTC()
	}
	return time.Time{}
}

func buildEngramRecord(row *engramRowPlan, now time.Time, result *Result) *engramRecord {
	observation := row.observation
	createdAt := parseEngramTime(observation.CreatedAt)
	updatedAt := parseEngramTime(observation.UpdatedAt)
	if createdAt.IsZero() {
		createdAt = updatedAt
	}
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	if createdAt.IsZero() {
		createdAt = now.UTC().Truncate(time.Second)
		updatedAt = createdAt
	}
	if updatedAt.Before(createdAt) {
		updatedAt = createdAt
	}
	// Engram stores whole seconds. Truncating here keeps the per-timestamp note
	// file name exact, so a sub-second difference can never put two groups in
	// one file and silently drop one group's name.
	createdAt = createdAt.UTC().Truncate(time.Second)
	updatedAt = updatedAt.UTC().Truncate(time.Second)
	title := singleLine(observation.Title)
	if title == "" {
		title = "Engram " + strings.TrimPrefix(engramExternalID(observation), EngramSourceSystem+":")
	}
	content, redacted := redact.Apply(observation.Content)
	result.Redacted += redacted

	record := &engramRecord{
		syncID:      strings.TrimSpace(observation.SyncID),
		externalID:  engramExternalID(observation),
		project:     row.rule.Project,
		scope:       normalizedEngramScope(observation.Scope, result),
		subtype:     singleLine(observation.Type),
		topicKey:    normalizedEngramTopic(observation.TopicKey, result),
		reviewAfter: parseEngramTime(observation.ReviewAfter),
		createdAt:   createdAt,
		updatedAt:   updatedAt,
		title:       title,
		session:     usableEngramSession(observation.SessionID, result),
		quarantine:  row.rule.Action == projectActionQuarantine,
		occurrences: maxInt(row.occurrences, 1),
		history:     row.history,
		sourceBody:  strings.TrimSpace(content),
	}
	if row.rule.Scope != "" {
		record.scope = row.rule.Scope
	}
	switch {
	case strings.EqualFold(strings.TrimSpace(observation.Type), "decision"),
		strings.EqualFold(strings.TrimSpace(observation.Type), "policy"):
		record.kind = engramRecordDecision
		fields := parseEngramFields(content)
		record.context, record.alternatives, record.consequences = decisionFields(fields, content)
		record.evidence = []string{record.externalID}
	case strings.EqualFold(strings.TrimSpace(observation.Type), "session_summary"):
		record.kind = engramRecordNote
		record.body = strings.TrimSpace(content)
		record.subtype = "session_summary"
	default:
		record.kind = engramRecordNote
		record.body = strings.TrimSpace(content)
	}
	if record.body == "" && record.context == "" && record.kind == engramRecordNote {
		record.body = title
	}
	return record
}

func usableEngramSession(value string, result *Result) string {
	session := strings.TrimSpace(value)
	if session == "" {
		return ""
	}
	if len(session) > engramSessionMax || strings.IndexFunc(session, unicode.IsControl) >= 0 {
		result.warn("a source session id was dropped because it is not a usable provenance label")
		return ""
	}
	return session
}

func normalizedEngramScope(value string, result *Result) string {
	scope := strings.ToLower(strings.TrimSpace(value))
	switch domain.RecordScope(scope) {
	case domain.ScopeProject, domain.ScopePersonal, domain.ScopeGlobal:
		if scope == string(domain.ScopeProject) {
			return ""
		}
		return scope
	case "":
		return ""
	default:
		result.warn(fmt.Sprintf("unknown engram scope %q was imported as project scope", value))
		return ""
	}
}

func normalizedEngramTopic(value string, result *Result) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	topic, err := domain.NormalizeTopicKey(value)
	if err != nil {
		result.warn(fmt.Sprintf("topic key %q was dropped: %v", value, err))
		return ""
	}
	return topic
}

// assignEngramRecordIDs gives every imported record a deterministic id derived
// from its source identity, so a rerun produces the same files and reports
// unchanged instead of a new copy. The hash prefix is extended only on the
// unlikely event of a collision.
func assignEngramRecordIDs(records []*engramRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].project != records[j].project {
			return records[i].project < records[j].project
		}
		if records[i].kind != records[j].kind {
			return records[i].kind < records[j].kind
		}
		if !records[i].createdAt.Equal(records[j].createdAt) {
			return records[i].createdAt.Before(records[j].createdAt)
		}
		return records[i].externalID < records[j].externalID
	})
	used := map[string]bool{}
	for _, record := range records {
		key := record.project + "\x00"
		for _, size := range []int{8, 12, 16, 20, 40} {
			candidate := record.kind + "-" + record.createdAt.UTC().Format("2006-01-02") + "-" + engramHash(record.externalID, size)
			if !used[key+candidate] {
				record.id = candidate
				used[key+candidate] = true
				break
			}
		}
	}
}

func engramHash(value string, size int) string {
	sum := sha1.Sum([]byte(value))
	encoded := hex.EncodeToString(sum[:])
	if size > len(encoded) {
		size = len(encoded)
	}
	return encoded[:size]
}

// engramField is one labelled section of an engram body: "**What**: text".
type engramField struct {
	Label string
	Body  string
}

var engramDecisionLabels = map[string]string{
	"what": "what", "why": "why", "where": "where", "learned": "learned",
	"rule": "rule", "impact": "impact", "context": "context", "decision": "decision",
	"alternatives": "alternatives", "consequences": "consequences", "evidence": "evidence",
	"notes": "notes", "details": "details",
}

// parseEngramFields splits the bold-labelled body engram writes. Text that is
// not a labelled section is kept in a preamble field, so a decision with a free
// body still preserves all of it.
func parseEngramFields(content string) []engramField {
	fields := make([]engramField, 0, 6)
	lines := strings.Split(content, "\n")
	label := ""
	body := make([]string, 0, 8)
	flush := func() {
		text := strings.TrimSpace(strings.Join(body, "\n"))
		if label != "" || text != "" {
			fields = append(fields, engramField{Label: label, Body: text})
		}
		body = body[:0]
	}
	for _, line := range lines {
		if parsed, rest, ok := engramFieldLabel(line); ok {
			flush()
			label = parsed
			if strings.TrimSpace(rest) != "" {
				body = append(body, rest)
			}
			continue
		}
		body = append(body, line)
	}
	flush()
	return fields
}

func engramFieldLabel(line string) (string, string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", "", false
	}
	trimmed = strings.TrimPrefix(trimmed, "- ")
	trimmed = strings.TrimPrefix(trimmed, "* ")
	trimmed = strings.TrimSpace(trimmed)
	trimmed = strings.TrimPrefix(trimmed, "**")
	trimmed = strings.TrimPrefix(trimmed, "__")
	index := strings.Index(trimmed, ":")
	if index <= 0 || index > 40 {
		return "", "", false
	}
	label := strings.TrimSpace(trimmed[:index])
	label = strings.TrimSuffix(label, "**")
	label = strings.TrimSuffix(label, "__")
	label = strings.TrimSpace(label)
	if label == "" || strings.ContainsAny(label, "\t") {
		return "", "", false
	}
	normalized := strings.ToLower(label)
	if _, ok := engramDecisionLabels[normalized]; !ok {
		return "", "", false
	}
	return normalized, strings.TrimSpace(trimmed[index+1:]), true
}

func decisionFields(fields []engramField, content string) (context string, alternatives, consequences []string) {
	parts := make([]string, 0, 6)
	for _, field := range fields {
		if strings.TrimSpace(field.Body) == "" && field.Label != "" {
			continue
		}
		if field.Label == "" {
			parts = append(parts, field.Body)
			continue
		}
		switch field.Label {
		case "learned", "consequences":
			if items := parseList(field.Body); len(items) > 0 {
				consequences = append(consequences, items...)
			}
		case "alternatives":
			if items := parseList(field.Body); len(items) > 0 {
				alternatives = append(alternatives, items...)
			}
		case "evidence":
			// Evidence is the source identity; the body is kept in context by
			// the default branch if it carries text, so nothing is lost.
			if strings.TrimSpace(field.Body) != "" {
				parts = append(parts, "### Evidence\n\n"+field.Body)
			}
		case "what", "context", "decision":
			parts = append(parts, field.Body)
		default:
			parts = append(parts, "### "+engramLabelHeading(field.Label)+"\n\n"+field.Body)
		}
	}
	if len(parts) == 0 {
		parts = append(parts, strings.TrimSpace(content))
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n")), alternatives, consequences
}

func applyEngramVersions(versions []engramVersion, records map[int64]*engramRecord, result *Result) {
	byRecord := map[*engramRecord][]engramVersion{}
	for _, version := range versions {
		record, ok := records[version.ObservationID]
		if !ok || strings.TrimSpace(version.Content) == "" {
			continue
		}
		byRecord[record] = append(byRecord[record], version)
	}
	for record, list := range byRecord {
		sort.SliceStable(list, func(i, j int) bool { return list[i].Version > list[j].Version })
		for _, version := range list {
			body, _ := redact.Apply(strings.TrimSpace(version.Content))
			if body == "" || body == record.sourceBody {
				continue
			}
			at := parseEngramTime(version.CreatedAt)
			if at.IsZero() {
				at = record.updatedAt
			}
			record.history = append(record.history, domain.HistoryEntry{At: at.UTC().Format(time.RFC3339), Body: body})
		}
		record.history = domain.BoundedHistory(record.history)
	}
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func engramLabelHeading(label string) string {
	if label == "" {
		return ""
	}
	upper := strings.ToUpper(label[:1])
	return upper + label[1:]
}
