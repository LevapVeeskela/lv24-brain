package app

import (
	"fmt"
	"sort"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// FindingPendingRelations flags a relation nobody judged. Pending relations are
// advisory: search ranking and context ignore them, so an unjudged pair silently
// sits between two records until a person or agent decides what it means.
// The kind is declared with the other finding kinds in findings.go.

// pendingRelationFindingLimit bounds how many pairs one review lists
// individually; the rest are summarized in one finding.
const pendingRelationFindingLimit = 10

// pendingRelation is one unjudged relation with the record that carries it.
type pendingRelation struct {
	source   RecordSummary
	relation domain.Relation
}

// pendingRelationFindings reports every pending relation of the project,
// oldest first. The age of a candidate is measured from its source record's
// last write, because candidate detection attaches the relation together with
// that write.
//
// Accepted conflicts are deliberately absent: a judged conflict is a decision,
// not a task, and nothing resolves it automatically.
func pendingRelationFindings(in reviewInputs, now time.Time) []Finding {
	var pending []pendingRelation
	collect := func(record Record) {
		summary := record.Summary()
		for _, relation := range record.Relations().Pending() {
			pending = append(pending, pendingRelation{source: summary, relation: relation})
		}
	}
	for i := range in.notes {
		collect(Record{Note: &in.notes[i]})
	}
	for i := range in.decisions {
		collect(Record{Decision: &in.decisions[i]})
	}
	for i := range in.attempts {
		collect(Record{Attempt: &in.attempts[i]})
	}
	for i := range in.handoffs {
		collect(Record{Handoff: &in.handoffs[i]})
	}
	if len(pending) == 0 {
		return nil
	}
	sort.SliceStable(pending, func(i, j int) bool {
		left, right := pending[i], pending[j]
		leftAt, rightAt := pendingSince(left.source), pendingSince(right.source)
		if !leftAt.Equal(rightAt) {
			return leftAt.Before(rightAt)
		}
		if left.source.ID != right.source.ID {
			return left.source.ID < right.source.ID
		}
		return left.relation.Target < right.relation.Target
	})
	limit := min(len(pending), pendingRelationFindingLimit)
	findings := make([]Finding, 0, limit+1)
	for _, item := range pending[:limit] {
		findings = append(findings, newFinding(FindingPendingRelations,
			fmt.Sprintf("%s %s has a pending %s to %s (%s); judge it with `brain relation judge %s %s --verdict ... --reason ...`",
				item.source.Kind, item.source.ID, item.relation.Relation, item.relation.Target,
				relationAgeLabel(item.source, now), item.source.ID, item.relation.Target),
			item.source.ID+" -> "+item.relation.Target))
	}
	if len(pending) > limit {
		findings = append(findings, newFinding(FindingPendingRelations,
			fmt.Sprintf("%d more pending relations are not shown; list them with `brain relation list --pending`", len(pending)-limit),
			""))
	}
	return findings
}

// pendingSince is the instant a pending relation was proposed: the source
// record's last write.
func pendingSince(source RecordSummary) time.Time {
	if !source.UpdatedAt.IsZero() {
		return source.UpdatedAt
	}
	return source.CreatedAt
}

// relationAgeLabel renders how long a candidate waited.
func relationAgeLabel(source RecordSummary, now time.Time) string {
	since := pendingSince(source)
	if since.IsZero() {
		return "age unknown"
	}
	age := now.Sub(since)
	switch {
	case age < time.Hour:
		return "just now"
	case age < 24*time.Hour:
		return fmt.Sprintf("%d hours old", int(age.Hours()))
	case age < 48*time.Hour:
		return "1 day old"
	default:
		return fmt.Sprintf("%d days old", int(age.Hours()/24))
	}
}
