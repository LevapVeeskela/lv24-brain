package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// RelationRow is one relation as the MCP tools report it, with both ends and
// their resolution state.
type RelationRow struct {
	SourceKind  string `json:"source_kind"`
	SourceID    string `json:"source_id"`
	SourceTitle string `json:"source_title"`
	TargetID    string `json:"target_id"`
	TargetKind  string `json:"target_kind,omitempty"`
	TargetTitle string `json:"target_title,omitempty"`
	// TargetState is found, tombstone or missing.
	TargetState string    `json:"target_state"`
	Relation    string    `json:"relation"`
	Status      string    `json:"status"`
	Reason      string    `json:"reason,omitempty"`
	Confidence  float64   `json:"confidence,omitempty"`
	JudgedAt    time.Time `json:"judged_at,omitzero"`
	VerdictBy   string    `json:"verdict_by,omitempty"`
	// Direction is out for a relation the source carries and in for one that
	// points at the requested record.
	Direction string `json:"direction,omitempty"`
	// AgeDays is how long a pending relation has waited.
	AgeDays int `json:"age_days,omitempty"`
}

// RelationQuery filters a brain_relations listing.
type RelationQuery struct {
	ProjectID string
	// ID limits the listing to the relations of one source record.
	ID string
	// Target limits the listing to relations that point at one record.
	Target string
	// Statuses keeps these statuses; empty keeps every status.
	Statuses []string
	// Relations keeps these verbs; empty keeps every verb.
	Relations []string
	Limit     int
}

// JudgeInput is one brain_judge request.
type JudgeInput struct {
	ProjectID  string
	From       string
	To         string
	Verdict    string
	Reason     string
	Confidence float64
	// By is human or agent; the MCP tool always passes agent.
	By string
}

// JudgedRelation is the result of brain_judge.
type JudgedRelation struct {
	Relation RelationRow `json:"relation"`
	Changed  bool        `json:"changed"`
}

// RelationBackend is implemented by backends that can list and judge typed
// relations. The tools are listed only for such backends.
type RelationBackend interface {
	ListRelations(ctx context.Context, query RelationQuery) ([]RelationRow, error)
	JudgeRelation(ctx context.Context, in JudgeInput) (JudgedRelation, error)
}

// relationTools returns the relations read tool and the judge write tool.
func (s *Server) relationTools() []tool {
	if _, ok := s.backend.(RelationBackend); !ok && !s.catalog {
		return nil
	}
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	relationValues := domain.RelationKindNames()
	return []tool{
		{
			Name:        "brain_relations",
			Annotations: readOnlyAnnotations("List relations"),
			Description: "List the typed relations of a project, one record (id) or the records that point at one (target). " +
				"Rows carry both ends, the status (pending, accepted, rejected) and whether the target is live, a tombstone or missing. " +
				"Pending relations are candidates: search and context ignore them until brain_judge decides.",
			InputSchema: objectSchema(nil, map[string]any{
				"project":  project,
				"id":       stringProp("List the relations the record with this id carries."),
				"target":   stringProp("List the relations that point at the record with this id (backlinks)."),
				"status":   enumProp("Keep one status: pending, accepted, rejected.", domain.RelationStatusNames()),
				"relation": enumProp("Keep one relation verb.", relationValues),
				"limit":    intProp("Page size; 0 selects the default page of 50.", 0),
				"cursor":   cursorProp(),
			}),
			OutputSchema: outputObject(pageOutput(map[string]any{
				"project_id": stringProp("Project the relations belong to."),
				"relations":  arrayProp("Relations, pending first.", objectProp("One relation with both ends and its status.")),
			}), "project_id", "relations", "page", "page_size", "has_more"),
			Limits: limitsOf(pageLimitsFor("brain_relations")),
			run:    s.relationsList,
		},
		{
			Name:        "brain_judge",
			Annotations: mutatingAnnotations("Judge a relation"),
			Description: "Record a verdict on the pair (from, to): the relation becomes accepted (or rejected) with the reason, " +
				"confidence and judge. This is the only way a pending candidate changes status; the target record is never edited, " +
				"ended or deleted by it. Use brain_decision_status or a supersede to change a decision's lifecycle.",
			InputSchema: objectSchema([]string{"from", "to", "verdict", "reason"}, map[string]any{
				"from":       stringProp("Record id that carries the relation."),
				"to":         stringProp("Related record id; a tombstone is accepted."),
				"verdict":    enumProp("Judged relation verb.", relationValues),
				"reason":     stringProp("Why the pair is related, conflicting or unrelated. Required."),
				"confidence": numberProp("Optional judge confidence between 0 and 1."),
				"project":    project,
			}),
			OutputSchema: outputObject(map[string]any{
				"relation": objectProp("The judged relation with both ends."),
				"changed":  boolProp("False when the same verdict was already stored."),
			}, "relation", "changed"),
			confirm: confirmJudge,
			run:     s.judge,
		},
	}
}

// relationsList lists relations with the pagination contract.
func (s *Server) relationsList(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Project  string `json:"project"`
		ID       string `json:"id"`
		Target   string `json:"target"`
		Status   string `json:"status"`
		Relation string `json:"relation"`
		Limit    int    `json:"limit"`
		Cursor   string `json:"cursor"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	id := strings.TrimSpace(args.ID)
	target := strings.TrimSpace(args.Target)
	if id != "" && target != "" {
		return toolResult{}, refusalInvalid("pass either id or target, not both")
	}
	status := strings.TrimSpace(args.Status)
	if status != "" && !domain.RelationStatus(status).Valid() {
		return toolResult{}, refusalInvalidf("unknown status %q (%s)", status, strings.Join(domain.RelationStatusNames(), ", "))
	}
	relation := strings.TrimSpace(args.Relation)
	if relation != "" && !domain.RelationKind(relation).Valid() {
		return toolResult{}, refusalInvalidf("unknown relation %q (%s)", relation, strings.Join(domain.RelationKindNames(), ", "))
	}
	target2, err := s.resolveTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	limits := pageLimitsFor("brain_relations")
	filter := filterFingerprint(target2.ID, id, target, status, relation)
	offset, size, err := pageRequest("brain_relations", limits, args.Limit, args.Cursor, filter)
	if err != nil {
		return toolResult{}, err
	}
	requested := fetchLimit(offset, size, limits)
	query := RelationQuery{ProjectID: target2.ID, ID: id, Target: target, Limit: requested}
	if status != "" {
		query.Statuses = []string{status}
	}
	if relation != "" {
		query.Relations = []string{relation}
	}
	rows, err := s.backend.(RelationBackend).ListRelations(ctx, query)
	if err != nil {
		return toolResult{}, err
	}
	pageRows, page := paginate(rows, offset, size, requested, "brain_relations", filter)
	if pageRows == nil {
		pageRows = []RelationRow{}
	}
	var text strings.Builder
	if len(pageRows) == 0 {
		text.WriteString("no relations")
	}
	for _, row := range pageRows {
		fmt.Fprintf(&text, "- [%s] %s %s -%s-> %s", row.Status, row.SourceKind, row.SourceID, row.Relation, row.TargetID)
		if row.TargetState != "" && row.TargetState != "found" {
			text.WriteString(" (" + row.TargetState + ")")
		}
		if row.Reason != "" {
			text.WriteString(": " + row.Reason)
		}
		text.WriteString("\n")
	}
	if footer := pageFooter(page); footer != "" {
		text.WriteString(footer)
	}
	return toolResult{
		Text: strings.TrimRight(text.String(), "\n"),
		Structured: struct {
			ProjectID string        `json:"project_id"`
			Relations []RelationRow `json:"relations"`
			Page
		}{ProjectID: target2.ID, Relations: pageRows, Page: page},
	}, nil
}

// judge records one relation verdict.
func (s *Server) judge(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		From       string  `json:"from"`
		To         string  `json:"to"`
		Verdict    string  `json:"verdict"`
		Reason     string  `json:"reason"`
		Confidence float64 `json:"confidence"`
		Project    string  `json:"project"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	from, err := requireText("from", args.From)
	if err != nil {
		return toolResult{}, err
	}
	to, err := requireText("to", args.To)
	if err != nil {
		return toolResult{}, err
	}
	verdict, err := requireText("verdict", args.Verdict)
	if err != nil {
		return toolResult{}, err
	}
	if !domain.RelationKind(verdict).Valid() {
		return toolResult{}, refusalInvalidf("unknown verdict %q (%s)", verdict, strings.Join(domain.RelationKindNames(), ", "))
	}
	reason, err := requireText("reason", args.Reason)
	if err != nil {
		return toolResult{}, err
	}
	if args.Confidence < 0 || args.Confidence > 1 {
		return toolResult{}, refusalInvalid("confidence must be between 0 and 1")
	}
	target, err := s.writeTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	result, err := s.backend.(RelationBackend).JudgeRelation(ctx, JudgeInput{
		ProjectID: target.ID, From: from, To: to, Verdict: verdict, Reason: reason,
		Confidence: args.Confidence, By: string(domain.VerdictByAgent),
	})
	if err != nil {
		return toolResult{}, err
	}
	status := "judged"
	if !result.Changed {
		status = "unchanged"
	}
	text := fmt.Sprintf("%s: %s %s -%s-> %s [%s]", status,
		result.Relation.SourceKind, result.Relation.SourceID, result.Relation.Relation, result.Relation.TargetID, result.Relation.Status)
	return toolResult{Text: text, Structured: result}, nil
}

// confirmJudge asks the user to confirm a verdict that replaces an existing
// relation row.
func confirmJudge(args json.RawMessage) *confirmation {
	var probe struct {
		From    string `json:"from"`
		To      string `json:"to"`
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal(argsOrDefault(args), &probe); err != nil {
		return nil
	}
	if strings.TrimSpace(probe.From) == "" || strings.TrimSpace(probe.To) == "" || strings.TrimSpace(probe.Verdict) == "" {
		return nil
	}
	return &confirmation{Message: fmt.Sprintf("Judge %s -> %s as %s? The verdict replaces any existing relation between the pair; neither record is edited.",
		probe.From, probe.To, probe.Verdict)}
}
