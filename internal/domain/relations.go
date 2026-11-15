package domain

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// RelationKind is one verb of the locked typed-relation vocabulary. It is
// deliberately closed: a judgment is stored as a documented verb, never as
// free text, so a caller can branch on it and a review can count it.
//
// The vocabulary combines the authored links of a knowledge graph
// (relates_to, depends_on, implements, blocks, evidences) with the locked
// verdict set of a conflict review (supersedes, conflicts_with, compatible,
// scoped, contradicts, not_conflict).
type RelationKind string

const (
	// RelationRelatesTo links two records about the same subject without a
	// claim about which one wins. It is the only verb candidate detection
	// writes on its own.
	RelationRelatesTo RelationKind = "relates_to"
	// RelationSupersedes replaces the target; unlike a validity supersede it
	// does not end the target by itself.
	RelationSupersedes RelationKind = "supersedes"
	// RelationConflictsWith says the two records cannot both be true.
	RelationConflictsWith RelationKind = "conflicts_with"
	// RelationCompatible says the records can both hold at once.
	RelationCompatible RelationKind = "compatible"
	// RelationScoped says each record holds in its own scope.
	RelationScoped RelationKind = "scoped"
	// RelationDependsOn says the source only holds while the target holds.
	RelationDependsOn RelationKind = "depends_on"
	// RelationImplements links a record to the design it implements.
	RelationImplements RelationKind = "implements"
	// RelationBlocks says the source prevents the target from proceeding.
	RelationBlocks RelationKind = "blocks"
	// RelationEvidences links a record to the claim it supports.
	RelationEvidences RelationKind = "evidences"
	// RelationContradicts says the source is evidence against the target.
	RelationContradicts RelationKind = "contradicts"
	// RelationNotConflict records that a suspected conflict was judged away.
	RelationNotConflict RelationKind = "not_conflict"
)

// relationKinds is the documented order of the vocabulary, used in errors and
// help text.
var relationKinds = []RelationKind{
	RelationRelatesTo, RelationSupersedes, RelationConflictsWith, RelationCompatible,
	RelationScoped, RelationDependsOn, RelationImplements, RelationBlocks,
	RelationEvidences, RelationContradicts, RelationNotConflict,
}

// Valid reports whether k is a documented relation verb.
func (k RelationKind) Valid() bool {
	for _, known := range relationKinds {
		if k == known {
			return true
		}
	}
	return false
}

// MustBeAcyclic reports whether a chain of this verb has to stay a directed
// acyclic graph. A supersede chain and a dependency graph are hierarchies; a
// cycle in either makes "which record is current" and "what depends on what"
// unanswerable.
func (k RelationKind) MustBeAcyclic() bool {
	return k == RelationSupersedes || k == RelationDependsOn
}

// RelationKindNames returns the vocabulary in documented order.
func RelationKindNames() []string {
	names := make([]string, len(relationKinds))
	for i, kind := range relationKinds {
		names[i] = string(kind)
	}
	return names
}

// ParseRelationKind validates a verb and normalizes surrounding whitespace.
func ParseRelationKind(value string) (RelationKind, error) {
	kind := RelationKind(strings.TrimSpace(value))
	if !kind.Valid() {
		return "", NewValidationError("relation",
			fmt.Sprintf("unknown relation %q (known: %s)", value, strings.Join(RelationKindNames(), ", ")))
	}
	return kind, nil
}

// VerdictBy names who judged a relation. The set is closed for the same reason
// the verbs are: a review counts human verdicts and agent verdicts
// differently.
type VerdictBy string

const (
	// VerdictByHuman marks a verdict a person gave.
	VerdictByHuman VerdictBy = "human"
	// VerdictByAgent marks a verdict an agent gave.
	VerdictByAgent VerdictBy = "agent"
)

// Valid reports whether v is empty or a documented judge.
func (v VerdictBy) Valid() bool {
	return v == "" || v == VerdictByHuman || v == VerdictByAgent
}

// RelationStatus is the judgment state of one relation. It is a state, not a
// verdict: "pending" means nobody has decided yet.
type RelationStatus string

const (
	// RelationPending marks a candidate nobody judged. Pending relations are
	// advisory: context and search ranking ignore them, and a neighbor walk
	// does not follow them.
	RelationPending RelationStatus = "pending"
	// RelationAccepted marks a judged relation. Accepted relations are
	// navigable and are the graph the project believes in.
	RelationAccepted RelationStatus = "accepted"
	// RelationRejected marks a relation that was considered and judged away.
	// The row is kept so the pair is not proposed again.
	RelationRejected RelationStatus = "rejected"
)

// Valid reports whether s is a documented status.
func (s RelationStatus) Valid() bool {
	switch s {
	case RelationPending, RelationAccepted, RelationRejected:
		return true
	default:
		return false
	}
}

// RelationStatusNames returns the statuses in lifecycle order.
func RelationStatusNames() []string {
	return []string{string(RelationPending), string(RelationAccepted), string(RelationRejected)}
}

// Relation is one typed, judged link from a record to another record of the
// same project. The target is a record id: records never cross projects inside
// one relation, and a qualified "<project>/<id>" reference is rejected.
//
// The zero status means accepted: a hand-written or legacy relation is an
// authored claim, while candidate detection always writes pending explicitly.
type Relation struct {
	// Target is the id of the related record.
	Target string `json:"target"`
	// Relation is the verb; see the RelationKind constants.
	Relation RelationKind `json:"relation"`
	// Reason explains the link or the verdict in one sentence.
	Reason string `json:"reason,omitempty"`
	// Confidence is the optional 0..1 author or judge confidence.
	Confidence float64 `json:"confidence,omitempty"`
	// Status is pending, accepted or rejected. Empty means accepted.
	Status RelationStatus `json:"status,omitempty"`
	// JudgedAt is when the verdict was given. Empty for a hand-authored
	// relation and for a pending candidate.
	JudgedAt time.Time `json:"judgedAt,omitzero"`
	// VerdictBy names the judge: human or agent.
	VerdictBy VerdictBy `json:"verdictBy,omitempty"`
}

// Normalize trims the text fields and fills the status default. It does not
// validate: Normalize is the forgiving half of the read path.
func (r Relation) Normalize() Relation {
	r.Target = strings.TrimSpace(r.Target)
	r.Relation = RelationKind(strings.TrimSpace(string(r.Relation)))
	r.Reason = strings.TrimSpace(r.Reason)
	r.VerdictBy = VerdictBy(strings.TrimSpace(string(r.VerdictBy)))
	if r.Status == "" {
		r.Status = RelationAccepted
	}
	return r
}

// IsPending reports whether the relation is an unjudged candidate.
func (r Relation) IsPending() bool { return r.Normalize().Status == RelationPending }

// Validate checks one relation. selfID, when known, rejects a self-link.
//
// A pending relation is by definition unjudged, so a judged_at or verdict_by
// on it is a contradiction rather than harmless metadata.
func (r Relation) Validate(field, selfID string) error {
	r = r.Normalize()
	if strings.Contains(r.Target, "/") {
		return NewValidationError(field+".target", "must be a record id of the same project, not a qualified reference")
	}
	if err := ValidateRecordReference(field+".target", r.Target); err != nil {
		return err
	}
	if selfID != "" && r.Target == selfID {
		return NewValidationError(field+".target", "a record cannot relate to itself")
	}
	if !r.Relation.Valid() {
		return NewValidationError(field+".relation",
			fmt.Sprintf("unknown relation %q (known: %s)", r.Relation, strings.Join(RelationKindNames(), ", ")))
	}
	if !r.Status.Valid() {
		return NewValidationError(field+".status",
			fmt.Sprintf("unknown status %q (pending, accepted, rejected)", r.Status))
	}
	if !r.VerdictBy.Valid() {
		return NewValidationError(field+".verdictBy", "must be human or agent")
	}
	if math.IsNaN(r.Confidence) || math.IsInf(r.Confidence, 0) || r.Confidence < 0 || r.Confidence > 1 {
		return NewValidationError(field+".confidence", "must be between 0 and 1")
	}
	if len(r.Reason) > maxRelationReasonRunes {
		return NewValidationError(field+".reason",
			fmt.Sprintf("must be at most %d characters", maxRelationReasonRunes))
	}
	if !utf8.ValidString(r.Reason) {
		return NewValidationError(field+".reason", "must be valid UTF-8")
	}
	if r.Status == RelationPending && (!r.JudgedAt.IsZero() || r.VerdictBy != "") {
		return NewValidationError(field+".status", "a pending relation is unjudged and cannot carry judged_at or verdict_by")
	}
	return nil
}

// maxRelationReasonRunes bounds one relation reason so a hand edit cannot turn
// frontmatter into a second body.
const maxRelationReasonRunes = 500

// RelationList is the ordered relation set of one record.
type RelationList []Relation

// Normalize trims every entry, fills status defaults and deduplicates by
// (target, relation). The last entry of a duplicate pair wins, so a later
// judgment replaces an earlier candidate.
func (l RelationList) Normalize() RelationList {
	if len(l) == 0 {
		return nil
	}
	out := make(RelationList, 0, len(l))
	index := make(map[relationKey]int, len(l))
	for _, relation := range l {
		relation = relation.Normalize()
		if relation.Target == "" || relation.Relation == "" {
			// An empty entry carries nothing to keep; Validate reports the
			// missing fields of a real entry.
			out = append(out, relation)
			continue
		}
		key := relationKey{target: relation.Target, relation: relation.Relation}
		if at, seen := index[key]; seen {
			out[at] = relation
			continue
		}
		index[key] = len(out)
		out = append(out, relation)
	}
	return out
}

type relationKey struct {
	target   string
	relation RelationKind
}

// Validate checks every relation, rejects duplicate (target, relation) pairs
// and rejects targets listed twice with different verbs only when both are
// accepted: a pending candidate and a judged verdict can coexist while a pair
// is being decided. selfID rejects self-links.
func (l RelationList) Validate(field, selfID string) error {
	seen := make(map[relationKey]struct{}, len(l))
	acceptedTargets := make(map[string]RelationKind, len(l))
	for i, relation := range l {
		entryField := fmt.Sprintf("%s[%d]", field, i)
		if err := relation.Validate(entryField, selfID); err != nil {
			return err
		}
		normalized := relation.Normalize()
		key := relationKey{target: normalized.Target, relation: normalized.Relation}
		if _, duplicate := seen[key]; duplicate {
			return NewValidationError(entryField+".relation",
				fmt.Sprintf("%s already has a %s relation to %s", field, normalized.Relation, normalized.Target))
		}
		seen[key] = struct{}{}
		if normalized.Status != RelationAccepted {
			continue
		}
		if other, conflict := acceptedTargets[normalized.Target]; conflict && !sameConflictingPair(other, normalized.Relation) {
			return NewValidationError(entryField+".relation",
				fmt.Sprintf("%s has two accepted relations to %s (%s and %s); a judged pair has one verdict",
					field, normalized.Target, other, normalized.Relation))
		}
		acceptedTargets[normalized.Target] = normalized.Relation
	}
	return nil
}

// sameConflictingPair reports whether two accepted verbs can describe one pair
// at the same time. "relates_to" is a generic link and may sit next to a
// specific verdict; two verdicts cannot.
func sameConflictingPair(a, b RelationKind) bool {
	return a == b || a == RelationRelatesTo || b == RelationRelatesTo
}

// With upserts one relation by (target, relation) and returns the new list.
// The relation is normalized first.
func (l RelationList) With(relation Relation) RelationList {
	relation = relation.Normalize()
	out := make(RelationList, 0, len(l)+1)
	replaced := false
	for _, existing := range l {
		existing = existing.Normalize()
		if existing.Target == relation.Target && existing.Relation == relation.Relation {
			out = append(out, relation)
			replaced = true
			continue
		}
		out = append(out, existing)
	}
	if !replaced {
		out = append(out, relation)
	}
	return out
}

// Without removes every relation to target. A judgment replaces the pair's
// verdict, so the old verb must not linger beside the new one.
func (l RelationList) Without(target string) RelationList {
	target = strings.TrimSpace(target)
	out := make(RelationList, 0, len(l))
	for _, relation := range l {
		if relation.Normalize().Target == target {
			continue
		}
		out = append(out, relation)
	}
	return out
}

// Find returns the relation to target with the given verb.
func (l RelationList) Find(target string, relation RelationKind) (Relation, bool) {
	target = strings.TrimSpace(target)
	for _, existing := range l {
		normalized := existing.Normalize()
		if normalized.Target == target && normalized.Relation == relation {
			return normalized, true
		}
	}
	return Relation{}, false
}

// ForTarget returns every relation to target.
func (l RelationList) ForTarget(target string) RelationList {
	target = strings.TrimSpace(target)
	var out RelationList
	for _, relation := range l {
		if normalized := relation.Normalize(); normalized.Target == target {
			out = append(out, normalized)
		}
	}
	return out
}

// Accepted returns the judged, navigable relations only.
func (l RelationList) Accepted() RelationList {
	var out RelationList
	for _, relation := range l {
		if normalized := relation.Normalize(); normalized.Status == RelationAccepted {
			out = append(out, normalized)
		}
	}
	return out
}

// Pending returns the unjudged candidates only.
func (l RelationList) Pending() RelationList {
	var out RelationList
	for _, relation := range l {
		if normalized := relation.Normalize(); normalized.Status == RelationPending {
			out = append(out, normalized)
		}
	}
	return out
}

// ValidateRelationGraph rejects a supersede or dependency cycle. edges maps a
// record id to its accepted relations; pending and rejected rows are ignored
// because they claim nothing yet.
//
// A missing node is not an error: the graph is partial by nature, since a
// relation may point at a tombstone.
func ValidateRelationGraph(edges map[string]RelationList) error {
	// Build one adjacency list per hierarchical verb, so a supersede edge and
	// a depends_on edge never combine into a false cycle.
	for _, kind := range []RelationKind{RelationSupersedes, RelationDependsOn} {
		graph := make(map[string][]string, len(edges))
		for source, relations := range edges {
			for _, relation := range relations.Accepted() {
				if relation.Relation != kind {
					continue
				}
				graph[source] = append(graph[source], relation.Target)
			}
		}
		if cycle := findRelationCycle(graph); cycle != nil {
			return NewValidationError("relation",
				fmt.Sprintf("a %s chain would loop: %s", kind, strings.Join(cycle, " -> ")))
		}
	}
	return nil
}

// findRelationCycle returns one cycle as the node path, or nil. It uses the
// iterative three-colour DFS so a long chain cannot overflow the stack.
func findRelationCycle(graph map[string][]string) []string {
	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make(map[string]int, len(graph))
	// Iterative DFS: each frame remembers the node and the next edge to walk.
	type frame struct {
		node string
		next int
		path []string
	}
	for start := range graph {
		if state[start] != unvisited {
			continue
		}
		stack := []frame{{node: start, path: []string{start}}}
		state[start] = visiting
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			successors := graph[top.node]
			if top.next >= len(successors) {
				state[top.node] = done
				stack = stack[:len(stack)-1]
				continue
			}
			successor := successors[top.next]
			top.next++
			switch state[successor] {
			case visiting:
				cycle := append([]string(nil), top.path...)
				// Cut the path to the first occurrence of the successor so the
				// reported cycle starts where the loop closes.
				for i, node := range cycle {
					if node == successor {
						cycle = cycle[i:]
						break
					}
				}
				return append(cycle, successor)
			case unvisited:
				state[successor] = visiting
				stack = append(stack, frame{node: successor, path: append(append([]string(nil), top.path...), successor)})
			}
		}
	}
	return nil
}

// MergeInfo is the merge history of one record. It is embedded in every
// curated record next to its validity.
//
// A survivor lists the records it absorbed in MergedFrom. An absorbed record
// becomes a tombstone: it keeps its text and its file (soft-trashed) and names
// its survivor in MergedInto. A record is never both.
type MergeInfo struct {
	// MergedFrom lists the ids this record absorbed, in merge order.
	MergedFrom []string `json:"mergedFrom,omitempty"`
	// MergedInto names the survivor that absorbed this record.
	MergedInto string `json:"mergedInto,omitempty"`
}

// IsTombstone reports whether the record was merged into another one.
func (m MergeInfo) IsTombstone() bool { return strings.TrimSpace(m.MergedInto) != "" }

// Normalized trims and deduplicates the merged_from list.
func (m MergeInfo) Normalized() MergeInfo {
	m.MergedInto = strings.TrimSpace(m.MergedInto)
	if len(m.MergedFrom) == 0 {
		m.MergedFrom = nil
		return m
	}
	seen := make(map[string]struct{}, len(m.MergedFrom))
	out := make([]string, 0, len(m.MergedFrom))
	for _, id := range m.MergedFrom {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	m.MergedFrom = out
	return m
}

// Validate checks the merge fields. selfID rejects a record that absorbed
// itself.
func (m MergeInfo) Validate(field, selfID string) error {
	m = m.Normalized()
	if m.MergedInto != "" {
		if err := ValidateRecordReference(field+".mergedInto", m.MergedInto); err != nil {
			return err
		}
		if m.MergedInto == selfID {
			return NewValidationError(field+".mergedInto", "a record cannot be merged into itself")
		}
		if len(m.MergedFrom) > 0 {
			return NewValidationError(field+".mergedInto", "a tombstone cannot also have absorbed records")
		}
	}
	for i, id := range m.MergedFrom {
		if err := ValidateRecordReference(fmt.Sprintf("%s.mergedFrom[%d]", field, i), id); err != nil {
			return err
		}
		if id == selfID {
			return NewValidationError(field+".mergedFrom", "a record cannot list itself as merged")
		}
		if id == m.MergedInto {
			return NewValidationError(field+".mergedFrom", "a record cannot be merged into its own tombstone")
		}
	}
	return nil
}
