package migrate

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/redact"
)

// mapEngramRelation maps the engram verdict vocabulary onto the locked brain
// relation verbs. Engram's "related" is brain's "relates_to".
func mapEngramRelation(value string) (domain.RelationKind, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "related", "relates_to", "relates-to":
		return domain.RelationRelatesTo, true
	case "compatible":
		return domain.RelationCompatible, true
	case "scoped":
		return domain.RelationScoped, true
	case "conflicts_with", "conflicts-with", "conflicts":
		return domain.RelationConflictsWith, true
	case "supersedes", "supersede":
		return domain.RelationSupersedes, true
	case "not_conflict", "not-conflict":
		return domain.RelationNotConflict, true
	default:
		return "", false
	}
}

// applyEngramRelations attaches judged, in-scope relations to their source
// records. Pending, orphaned, superseded, cross-project and dangling relations
// are skipped and counted: an import may report a conflict, never invent one.
func applyEngramRelations(relations []engramRelation, byExternal map[string]*engramRecord, result *Result) {
	byTarget := make(map[*engramRecord]map[string]domain.Relation)
	for _, relation := range relations {
		if strings.TrimSpace(relation.SupersededAt) != "" {
			result.RelationsSkipped++
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(relation.JudgmentStatus), "judged") {
			result.RelationsSkipped++
			continue
		}
		kind, ok := mapEngramRelation(relation.Relation)
		if !ok {
			result.RelationsSkipped++
			continue
		}
		source := byExternal[EngramSourceSystem+":"+strings.TrimSpace(relation.SourceID)]
		target := byExternal[EngramSourceSystem+":"+strings.TrimSpace(relation.TargetID)]
		if source == nil || target == nil || source.id == "" || target.id == "" {
			result.RelationsSkipped++
			continue
		}
		if source.project != target.project || source.id == target.id {
			result.RelationsSkipped++
			continue
		}
		judgedAt := parseEngramTime(relation.UpdatedAt)
		if judgedAt.IsZero() {
			judgedAt = parseEngramTime(relation.CreatedAt)
		}
		candidate := domain.Relation{
			Target:     target.id,
			Relation:   kind,
			Reason:     truncateRunes(strings.TrimSpace(relation.Reason), 500),
			Confidence: clampConfidence(relation.Confidence),
			Status:     domain.RelationAccepted,
			JudgedAt:   judgedAt,
			VerdictBy:  engramVerdictBy(relation.MarkedByKind),
		}
		if byTarget[source] == nil {
			byTarget[source] = map[string]domain.Relation{}
		}
		current, exists := byTarget[source][target.id]
		if exists {
			// A judged pair has exactly one verdict in the brain relation
			// model. The newest engram judgment wins and the older row is
			// reported as skipped instead of producing an unreadable record.
			result.RelationsSkipped++
			if candidate.JudgedAt.Before(current.JudgedAt) {
				continue
			}
		}
		byTarget[source][target.id] = candidate
	}
	for record, targets := range byTarget {
		ordered := make([]string, 0, len(targets))
		for target := range targets {
			ordered = append(ordered, target)
		}
		sort.Strings(ordered)
		record.relations = make(domain.RelationList, 0, len(ordered))
		for _, target := range ordered {
			record.relations = append(record.relations, targets[target])
		}
		result.Relations += len(record.relations)
	}
}

func engramVerdictBy(kind string) domain.VerdictBy {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "human", "user", "owner", "manual":
		return domain.VerdictByHuman
	default:
		return domain.VerdictByAgent
	}
}

func clampConfidence(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return strings.TrimSpace(string(runes[:limit]))
}

// planPrompts maps prompts into the local-only prompt zone. A prompt whose
// project is unknown or a test path stays out: prompt text is the most
// sensitive data in the source and is never quarantined into the shared tree.
func planPrompts(plan *engramPlan, prompts []engramPrompt, projectMap normalizedProjectMap, sessions map[string]engramSession, known map[string]bool, result *Result) {
	seen := map[string]bool{}
	for _, prompt := range prompts {
		directory := sessionDirectory(sessions, prompt.SessionID)
		rule := projectMap.resolve(prompt.Project, directory)
		if rule.Action != projectActionImport || !known[rule.Project] {
			result.PromptsSkipped++
			continue
		}
		text := strings.TrimSpace(prompt.Content)
		if text == "" {
			result.PromptsSkipped++
			continue
		}
		at := parseEngramTime(prompt.CreatedAt)
		if at.IsZero() {
			result.PromptsSkipped++
			result.warn(fmt.Sprintf("prompt %d has no usable created_at; skipped", prompt.ID))
			continue
		}
		text, redacted := redact.Apply(text)
		result.Redacted += redacted
		key := strings.ToLower(rule.Project) + "\x00" + at.UTC().Format(time.RFC3339Nano) + "\x00" + strings.TrimSpace(prompt.SessionID) + "\x00" + text
		if seen[key] {
			result.PromptsSkipped++
			continue
		}
		seen[key] = true
		plan.prompts = append(plan.prompts, engramPromptEntry{
			project: rule.Project, at: at.UTC(), session: usableEngramSession(prompt.SessionID, result), text: text,
		})
	}
	result.Prompts = len(plan.prompts)
}
