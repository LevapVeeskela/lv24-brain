package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// maxJudgeMany bounds one brain_judge(action=judge_many) call.
const maxJudgeMany = 32

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

// RelationQuery filters a relation listing.
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

// JudgeInput is one relation verdict request.
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

// JudgedRelation is the result of one verdict.
type JudgedRelation struct {
	Relation RelationRow `json:"relation"`
	Changed  bool        `json:"changed"`
}

// RelationBackend is implemented by backends that can list and judge typed
// relations.
type RelationBackend interface {
	ListRelations(ctx context.Context, query RelationQuery) ([]RelationRow, error)
	JudgeRelation(ctx context.Context, in JudgeInput) (JudgedRelation, error)
}

// relationVerbNames is the closed verdict vocabulary.
func relationVerbNames() []string {
	return domain.RelationKindNames()
}

// judgeArgs is one brain_judge request. It is a union of the actions; each
// handler validates the fields its action accepts.
type judgeArgs struct {
	Action           string          `json:"action"`
	From             string          `json:"from"`
	To               string          `json:"to"`
	Verdict          string          `json:"verdict"`
	Reason           string          `json:"reason"`
	Confidence       float64         `json:"confidence"`
	ID               string          `json:"id"`
	IDs              []string        `json:"ids"`
	Judgments        []judgmentInput `json:"judgments"`
	Phase            string          `json:"phase"`
	Fingerprint      string          `json:"fingerprint"`
	Trust            string          `json:"trust"`
	ExpectedRevision string          `json:"expected_revision"`
	Project          string          `json:"project"`
	Target           string          `json:"target"`
	Relation         string          `json:"relation"`
	Status           string          `json:"status"`
	Limit            int             `json:"limit"`
	Cursor           string          `json:"cursor"`
}

// judgmentInput is one item of judge_many.
type judgmentInput struct {
	From       string  `json:"from"`
	To         string  `json:"to"`
	Verdict    string  `json:"verdict"`
	Reason     string  `json:"reason"`
	Confidence float64 `json:"confidence"`
}

func (s *Server) judgeTool() tool {
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	return tool{
		Name:        "brain_judge",
		Annotations: mutatingAnnotations("Judge or reclassify existing knowledge"),
		Description: "Change the status of existing knowledge. action=judge (default) records a verdict on the pair (from,to) and is the only way a pending relation becomes accepted or rejected; " +
			"action=list reads the relation rows; action=judge_many records up to 32 verdicts; action=compare records one verdict from two records; " +
			"action=review clears quarantine; action=accept, reject and supersede change a decision's lifecycle; " +
			"action=consolidate plans (phase=plan, read-only) or applies (phase=apply) the dedup job: merge exact duplicate clusters, archive superseded records with no live links, report near clusters. " +
			"Plan never writes; apply trashes duplicates and superseded records (never deletes) and every op names its rollback.",
		InputSchema: objectSchema(nil, map[string]any{
			"action":            enumProp("judge (default), list, judge_many, compare, review, accept, reject, supersede, pin, unpin, consolidate.", []string{"judge", "list", "judge_many", "compare", "review", "accept", "reject", "supersede", "pin", "unpin", "consolidate"}),
			"from":              stringProp("Record id that carries the relation (judge, compare), or the old decision (supersede)."),
			"to":                stringProp("Related record id (judge, compare); a tombstone is accepted. With supersede, the decision that replaces the old one."),
			"verdict":           enumProp("Judged relation verb.", relationVerbNames()),
			"reason":            stringProp("Why the pair is related, conflicting or unrelated. Required by judge, compare and judge_many."),
			"confidence":        numberProp("Optional judge confidence between 0 and 1."),
			"id":                stringProp("Record or decision id (review, accept, reject, supersede)."),
			"ids":               stringArrayProp("Record ids to review; [\"*\"] reviews every unreviewed record. With action=consolidate phase=apply, the op ids to apply."),
			"judgments":         arrayProp("Verdicts for action=judge_many, at most 32.", objectProp("One verdict: from, to, verdict, reason, confidence.")),
			"phase":             enumProp("With action=consolidate: plan (default, read-only) or apply.", []string{"plan", "apply"}),
			"fingerprint":       stringProp("With action=consolidate phase=apply: the plan fingerprint from phase=plan; a stale fingerprint is refused and the fresh plan is returned."),
			"trust":             enumProp("Optional trust level of reviewed records; omit to keep the stored one.", trustLevels),
			"expected_revision": stringProp("Not available: relation verdicts and decision lifecycle are not revision-checked in 0.17."),
			"target":            stringProp("With action=list, list the relations that point at this record."),
			"relation":          enumProp("With action=list, keep one relation verb.", relationVerbNames()),
			"status":            enumProp("With action=list, keep one status: pending, accepted, rejected.", domain.RelationStatusNames()),
			"limit":             intProp("With action=list, page size; 0 selects the default page of 50.", 0),
			"cursor":            cursorProp(),
			"project":           project,
		}),
		OutputSchema: outputObject(pageOutput(map[string]any{
			"action":     stringProp("Action that ran."),
			"relation":   objectProp("The judged relation with both ends."),
			"changed":    boolProp("False when the same verdict was already stored."),
			"relations":  arrayProp("Relations, pending first (action=list).", objectProp("One relation with both ends and its status.")),
			"project_id": stringProp("Project the records or relations belong to."),
			"reviewed":   arrayProp("Records that are reviewed now (action=review).", recordSummarySchema()),
			"results":    arrayProp("One result per judgment (action=judge_many).", objectProp("One judged relation and whether it changed.")),
			"kind":       stringProp("Record kind (accept, reject, supersede)."),
			"id":         stringProp("Record id."),
			"title":      stringProp("Record title."),
			"status":     stringProp("New decision status."),
		})),
		Limits:      limitsOf(pageLimitsFor("brain_relations")),
		ReadActions: []string{"list"},
		confirm:     confirmJudgeAction,
		run:         s.judge,
	}
}

func (s *Server) judge(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args judgeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	action := strings.TrimSpace(args.Action)
	if action == "" {
		action = "judge"
	}
	if strings.TrimSpace(args.ExpectedRevision) != "" {
		return toolResult{}, refusalUnsupported(
			"expected_revision is not available: relation verdicts and decision lifecycle are not revision-checked in 0.17",
			"read the current record with brain_record(action=get) and retry the judgment")
	}
	if strings.TrimSpace(args.Phase) != "" || strings.TrimSpace(args.Fingerprint) != "" {
		if action != "consolidate" {
			return toolResult{}, refusalInvalidWith("phase and fingerprint only apply to action=consolidate",
				"brain_judge action=consolidate phase=plan")
		}
	}
	if strings.TrimSpace(args.Trust) != "" && action != "review" {
		return toolResult{}, refusalInvalid("trust only applies to action=review")
	}
	switch action {
	case "judge":
		return s.judgeRelation(ctx, args, args.Verdict)
	case "list":
		return s.relationsList(ctx, recordArgs{
			Action: "relations", Project: args.Project, ID: args.ID, Target: args.Target,
			Relation: args.Relation, Status: args.Status, Limit: args.Limit, Cursor: args.Cursor,
		})
	case "judge_many":
		return s.judgeMany(ctx, args)
	case "compare":
		verdict := strings.TrimSpace(args.Verdict)
		if verdict == "" {
			verdict = string(domain.RelationRelatesTo)
		}
		return s.judgeRelation(ctx, args, verdict)
	case "review":
		return s.applyReview(ctx, reviewRecordArgs{ID: args.ID, IDs: args.IDs, Trust: args.Trust, Project: args.Project})
	case "accept", "reject":
		status := strings.TrimSpace(action) + "ed"
		if action == "reject" {
			status = "rejected"
		}
		return s.applyDecisionStatus(ctx, decisionStatusArgs{ID: args.ID, Status: status, Project: args.Project})
	case "supersede":
		if strings.TrimSpace(args.To) == "" {
			return toolResult{}, refusalInvalid("to is required with action=supersede: it is the decision that replaces id")
		}
		return s.applyDecisionStatus(ctx, decisionStatusArgs{ID: args.ID, SupersededBy: args.To, Project: args.Project})
	case "consolidate":
		return s.judgeConsolidate(ctx, args)
	case "pin", "unpin":
		return s.applyPin(ctx, args, action == "pin")
	default:
		return toolResult{}, refusalInvalidf("unknown action %q", action)
	}
}

// judgeRelation records one relation verdict.
func (s *Server) judgeRelation(ctx context.Context, args judgeArgs, verdict string) (toolResult, error) {
	backend, ok := s.backend.(RelationBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_judge(action=judge)", "brain relations judge")
	}
	from, err := requireText("from", args.From)
	if err != nil {
		return toolResult{}, err
	}
	to, err := requireText("to", args.To)
	if err != nil {
		return toolResult{}, err
	}
	if err := checkVerdict(verdict); err != nil {
		return toolResult{}, err
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
	result, err := backend.JudgeRelation(ctx, JudgeInput{
		ProjectID: target.ID, From: from, To: to, Verdict: verdict, Reason: reason,
		Confidence: args.Confidence, By: string(domain.VerdictByAgent),
	})
	if err != nil {
		return toolResult{}, err
	}
	return judgedResult(result), nil
}

// judgeMany records several verdicts. Every item is validated before the first
// write, so a malformed list never applies half of itself.
func (s *Server) judgeMany(ctx context.Context, args judgeArgs) (toolResult, error) {
	backend, ok := s.backend.(RelationBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_judge(action=judge_many)", "brain relations judge")
	}
	if len(args.Judgments) == 0 {
		return toolResult{}, refusalInvalid("judgments is required with action=judge_many")
	}
	if len(args.Judgments) > maxJudgeMany {
		return toolResult{}, refusalInvalidf("judgments holds %d items; the limit is %d", len(args.Judgments), maxJudgeMany)
	}
	for index, item := range args.Judgments {
		if _, err := requireText(fmt.Sprintf("judgments[%d].from", index), item.From); err != nil {
			return toolResult{}, err
		}
		if _, err := requireText(fmt.Sprintf("judgments[%d].to", index), item.To); err != nil {
			return toolResult{}, err
		}
		if err := checkVerdict(item.Verdict); err != nil {
			return toolResult{}, err
		}
		if _, err := requireText(fmt.Sprintf("judgments[%d].reason", index), item.Reason); err != nil {
			return toolResult{}, err
		}
		if item.Confidence < 0 || item.Confidence > 1 {
			return toolResult{}, refusalInvalidf("judgments[%d].confidence must be between 0 and 1", index)
		}
	}
	target, err := s.writeTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	results := make([]JudgedRelation, 0, len(args.Judgments))
	for index, item := range args.Judgments {
		result, err := backend.JudgeRelation(ctx, JudgeInput{
			ProjectID: target.ID, From: strings.TrimSpace(item.From), To: strings.TrimSpace(item.To),
			Verdict: strings.TrimSpace(item.Verdict), Reason: strings.TrimSpace(item.Reason),
			Confidence: item.Confidence, By: string(domain.VerdictByAgent),
		})
		if err != nil {
			return toolResult{}, fmt.Errorf("%d of %d judgments were applied before judgment %d failed: %w",
				len(results), len(args.Judgments), index, err)
		}
		results = append(results, result)
	}
	changed := 0
	for _, result := range results {
		if result.Changed {
			changed++
		}
	}
	return toolResult{
		Text: fmt.Sprintf("%d of %d judgments changed", changed, len(results)),
		Structured: struct {
			Action  string           `json:"action"`
			Results []JudgedRelation `json:"results"`
		}{Action: "judge_many", Results: results},
	}, nil
}

// checkVerdict validates a relation verb.
func checkVerdict(verdict string) error {
	verdict = strings.TrimSpace(verdict)
	if verdict == "" {
		return refusalInvalid("verdict is required")
	}
	if !domain.RelationKind(verdict).Valid() {
		return refusalInvalidf("unknown verdict %q (%s)", verdict, strings.Join(relationVerbNames(), ", "))
	}
	return nil
}

// judgedResult renders one verdict in the historical judge shape.
func judgedResult(result JudgedRelation) toolResult {
	status := "judged"
	if !result.Changed {
		status = "unchanged"
	}
	text := fmt.Sprintf("%s: %s %s -%s-> %s [%s]", status,
		result.Relation.SourceKind, result.Relation.SourceID, result.Relation.Relation, result.Relation.TargetID, result.Relation.Status)
	return toolResult{Text: text, Structured: result}
}

// relationsList lists relations with the pagination contract.
func (s *Server) relationsList(ctx context.Context, args recordArgs) (toolResult, error) {
	backend, ok := s.backend.(RelationBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_record(action=relations)", "brain_judge(action=list)")
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
	resolved, err := s.resolveTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	limits := pageLimitsFor("brain_relations")
	filter := filterFingerprint(resolved.ID, id, target, status, relation)
	offset, size, err := pageRequest("brain_relations", limits, args.Limit, args.Cursor, filter)
	if err != nil {
		return toolResult{}, err
	}
	requested := fetchLimit(offset, size, limits)
	query := RelationQuery{ProjectID: resolved.ID, ID: id, Target: target, Limit: requested}
	if status != "" {
		query.Statuses = []string{status}
	}
	if relation != "" {
		query.Relations = []string{relation}
	}
	rows, err := backend.ListRelations(ctx, query)
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
		}{ProjectID: resolved.ID, Relations: pageRows, Page: page},
	}, nil
}

// confirmJudgeAction asks the user before a verdict replaces a relation row or
// a decision is superseded.
func confirmJudgeAction(args json.RawMessage) *confirmation {
	var probe struct {
		Action  string `json:"action"`
		ID      string `json:"id"`
		From    string `json:"from"`
		To      string `json:"to"`
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal(argsOrDefault(args), &probe); err != nil {
		return nil
	}
	action := strings.TrimSpace(probe.Action)
	if action == "" {
		action = "judge"
	}
	switch action {
	case "judge":
		if strings.TrimSpace(probe.From) == "" || strings.TrimSpace(probe.To) == "" || strings.TrimSpace(probe.Verdict) == "" {
			return nil
		}
		return &confirmation{Message: fmt.Sprintf("Judge %s -> %s as %s? The verdict replaces any existing relation between the pair; neither record is edited.",
			probe.From, probe.To, probe.Verdict)}
	case "supersede":
		if strings.TrimSpace(probe.ID) == "" || strings.TrimSpace(probe.To) == "" {
			return nil
		}
		return &confirmation{Message: fmt.Sprintf("Mark decision %s as superseded by %s? The old decision stays in the record but is no longer current.", probe.ID, probe.To)}
	case "consolidate":
		return confirmConsolidate(argsOrDefault(args))
	}
	return nil
}
