package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// RecordSummary is one record in a listing.
type RecordSummary struct {
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Title     string    `json:"title"`
	Status    string    `json:"status,omitempty"`
	Pinned    bool      `json:"pinned,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// RecordDetail is one record in full. Only the fields of its kind are set.
type RecordDetail struct {
	Kind         string    `json:"kind"`
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	Title        string    `json:"title"`
	Status       string    `json:"status,omitempty"`
	Body         string    `json:"body,omitempty"`
	Context      string    `json:"context,omitempty"`
	Alternatives []string  `json:"alternatives,omitempty"`
	Consequences []string  `json:"consequences,omitempty"`
	Evidence     []string  `json:"evidence,omitempty"`
	Hypothesis   string    `json:"hypothesis,omitempty"`
	Result       string    `json:"result,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	NextAction   string    `json:"next_action,omitempty"`
	GitContext   string    `json:"git_context,omitempty"`
	CreatedAt    time.Time `json:"created_at,omitzero"`
	UpdatedAt    time.Time `json:"updated_at,omitzero"`
	// Links are the record's [[id]] references with their targets resolved.
	Links []RecordLink `json:"links"`
	// Backlinks are the records of the same project that reference it.
	Backlinks []RecordBacklink `json:"backlinks"`
	// Relations are the typed relations the record carries, pending ones
	// included.
	Relations []RecordRelation `json:"relations"`
}

// RecordRelation is one typed relation of a record.
type RecordRelation struct {
	Target     string  `json:"target"`
	Relation   string  `json:"relation"`
	Reason     string  `json:"reason,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Status     string  `json:"status"`
	JudgedAt   string  `json:"judged_at,omitempty"`
	VerdictBy  string  `json:"verdict_by,omitempty"`
}

// RecordLink is one outgoing [[id]] reference of a record.
type RecordLink struct {
	// Field is the record field holding the reference.
	Field string `json:"field"`
	// Ref is the reference as written ("id" or "project/id").
	Ref       string `json:"ref"`
	Text      string `json:"text,omitempty"`
	ProjectID string `json:"project_id"`
	ID        string `json:"id"`
	// State is "found", "missing" or "unavailable".
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	// Kind, Title and Status describe the target when it was found.
	Kind   string `json:"kind,omitempty"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
}

// RecordBacklink is a record that references another one.
type RecordBacklink struct {
	Field     string `json:"field"`
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Title     string `json:"title"`
	Status    string `json:"status,omitempty"`
}

// RecordQuery filters a record listing. Zero values match everything.
type RecordQuery struct {
	ProjectID string
	Kinds     []string
	Since     time.Time
	Status    string
	Limit     int
}

// RecordBackend is implemented by backends that can list and read records and
// change decision status. Trash and restore stay CLI-only.
type RecordBackend interface {
	ListRecords(ctx context.Context, query RecordQuery) ([]RecordSummary, error)
	GetRecord(ctx context.Context, projectID, id string) (RecordDetail, error)
	// SetDecisionStatus changes a decision's status and returns it.
	SetDecisionStatus(ctx context.Context, projectID, id, status string) (RecordSummary, error)
	// Supersede marks decision oldID superseded by decision newID.
	Supersede(ctx context.Context, projectID, oldID, newID string) (RecordSummary, error)
}

// RecordHistoryBackend is implemented by backends that can report the
// supersede chain a record belongs to.
type RecordHistoryBackend interface {
	RecordHistory(ctx context.Context, projectID, id string) ([]RecordSummary, error)
}

// ReviewRecordInput is one record review request.
type ReviewRecordInput struct {
	ProjectID string
	// ID reviews one record; empty with All reviews every unreviewed record.
	ID string
	// All reviews every record that is not reviewed yet.
	All bool
	// Trust optionally replaces the trust level; empty keeps it.
	Trust string
}

// ReviewRecordResult is the structured result of a record review.
type ReviewRecordResult struct {
	ProjectID string          `json:"project_id"`
	Reviewed  []RecordSummary `json:"reviewed"`
}

// ReviewRecordBackend is implemented by backends that can clear a record's
// quarantine.
type ReviewRecordBackend interface {
	ReviewRecord(ctx context.Context, in ReviewRecordInput) (ReviewRecordResult, error)
}

// RecordPinner is implemented by backends that can set or clear the pinned flag
// of one record.
type RecordPinner interface {
	SetPinned(ctx context.Context, projectID, id string, pinned bool) (RecordSummary, error)
}

// RecordPinSetter is an alternate spelling of RecordPinner.
type RecordPinSetter interface {
	SetRecordPinned(ctx context.Context, projectID, id string, pinned bool) (RecordSummary, error)
}

// RecordPinToggler is an alternate spelling of RecordPinner.
type RecordPinToggler interface {
	PinRecord(ctx context.Context, projectID, id string, pinned bool) (RecordSummary, error)
}

// RecordPinActioner is implemented by backends exposing separate pin/unpin
// actions.
type RecordPinActioner interface {
	PinRecord(ctx context.Context, projectID, id string) (RecordSummary, error)
	UnpinRecord(ctx context.Context, projectID, id string) (RecordSummary, error)
}

// RecordList is the structured result of brain_record(action=list).
type RecordList struct {
	ProjectID string          `json:"project_id"`
	Records   []RecordSummary `json:"records"`
	// Page is embedded, so page/page_size/has_more/next_cursor are top-level.
	Page
}

// recordArgs is one brain_record request.
type recordArgs struct {
	Action    string   `json:"action"`
	ID        string   `json:"id"`
	Project   string   `json:"project"`
	Kinds     []string `json:"kinds"`
	Status    string   `json:"status"`
	SinceDays int      `json:"since_days"`
	DueReview bool     `json:"due_review"`
	Target    string   `json:"target"`
	Relation  string   `json:"relation"`
	Limit     int      `json:"limit"`
	Cursor    string   `json:"cursor"`
}

func (s *Server) recordTool() tool {
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	return tool{
		Name:        "brain_record",
		Annotations: readOnlyAnnotations("Read a record or record listing"),
		Description: "Read one record in full (action=get, the default) by id, list records with filters (action=list), " +
			"list typed relations (action=relations), read the supersede chain (action=history) or read only the links and backlinks (action=links). " +
			"It never writes: evolution is brain_note(topic_key=...), lifecycle is brain_judge, removal is `brain record trash` (CLI, reversible).",
		InputSchema: objectSchema(nil, map[string]any{
			"action":     enumProp("get (default), list, relations, history or links.", []string{"get", "list", "relations", "history", "links"}),
			"id":         stringProp("Record id, as returned by action=list or brain_search."),
			"project":    project,
			"kinds":      stringArrayProp("Keep only these kinds: note, decision, attempt, handoff. Omit for every kind."),
			"status":     enumProp("Keep decisions with this status (drops other kinds).", decisionStatuses),
			"since_days": intProp("Keep records created or updated in the last N days; 0 keeps all.", 0),
			"due_review": boolProp("Not available here: use brain_review(action=due)."),
			"target":     stringProp("With action=relations, list the relations that point at this record."),
			"relation":   enumProp("With action=relations, keep one relation verb.", relationVerbNames()),
			"limit":      intProp("Page size; 0 selects the default page of 50.", 0),
			"cursor":     cursorProp(),
		}),
		OutputSchema: outputObject(pageOutput(map[string]any{
			"project_id":   stringProp("Project the records belong to."),
			"records":      arrayProp("Records, newest first (action=list).", recordSummarySchema()),
			"versions":     arrayProp("Records of the supersede chain, oldest first (action=history).", recordSummarySchema()),
			"relations":    arrayProp("Relations, pending first (action=relations).", objectProp("One relation with both ends and its status.")),
			"kind":         stringProp("Record kind (action=get or links)."),
			"id":           stringProp("Record id."),
			"title":        stringProp("Record title."),
			"status":       stringProp("Decision status, when the record is a decision."),
			"body":         stringProp("Note body."),
			"context":      stringProp("Why a decision was needed."),
			"alternatives": arrayProp("Options that were considered.", stringProp("Option.")),
			"consequences": arrayProp("Expected consequences.", stringProp("Consequence.")),
			"evidence":     arrayProp("Supporting evidence.", stringProp("Evidence.")),
			"hypothesis":   stringProp("What an attempt expected to work."),
			"result":       stringProp("What an attempt produced."),
			"reason":       stringProp("Why an attempt was kept or abandoned."),
			"next_action":  stringProp("Next action of a handoff."),
			"git_context":  stringProp("Git context captured with a handoff."),
			"created_at":   stringProp("RFC3339 creation time."),
			"updated_at":   stringProp("RFC3339 last update."),
			"links":        arrayProp("Outgoing [[id]] references, resolved.", objectProp("One resolved reference.")),
			"backlinks":    arrayProp("Records of the same project that reference this one.", objectProp("One backlink.")),
		}), "project_id"),
		Limits:      limitsOf(pageLimitsFor("brain_records")),
		ReadActions: []string{"get", "list", "relations", "history", "links"},
		run:         s.record,
	}
}

func (s *Server) record(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args recordArgs
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	action := strings.TrimSpace(args.Action)
	if action == "" {
		action = "get"
	}
	if args.DueReview {
		if action != "list" {
			return toolResult{}, refusalInvalid("due_review only applies to action=list; use brain_review(action=due) for the review queue")
		}
		return toolResult{}, refusedGap("brain_record", "list due_review")
	}
	switch action {
	case "get":
		return s.recordGet(ctx, args)
	case "list":
		return s.recordList(ctx, args)
	case "relations":
		return s.relationsList(ctx, args)
	case "history":
		return s.recordHistory(ctx, args)
	case "links":
		return s.recordLinks(ctx, args)
	default:
		return toolResult{}, refusalInvalidf("unknown action %q (get, list, relations, history, links)", action)
	}
}

func (s *Server) recordGet(ctx context.Context, args recordArgs) (toolResult, error) {
	backend, ok := s.backend.(RecordBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_record(action=get)", "brain_search")
	}
	id, err := requireText("id", args.ID)
	if err != nil {
		return toolResult{}, err
	}
	target, err := s.resolveTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	detail, err := backend.GetRecord(ctx, target.ID, id)
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{Text: detailText(detail), Structured: detail}, nil
}

func (s *Server) recordLinks(ctx context.Context, args recordArgs) (toolResult, error) {
	backend, ok := s.backend.(RecordBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_record(action=links)", "brain_record(action=get)")
	}
	id, err := requireText("id", args.ID)
	if err != nil {
		return toolResult{}, err
	}
	target, err := s.resolveTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	detail, err := backend.GetRecord(ctx, target.ID, id)
	if err != nil {
		return toolResult{}, err
	}
	if detail.Links == nil {
		detail.Links = []RecordLink{}
	}
	if detail.Backlinks == nil {
		detail.Backlinks = []RecordBacklink{}
	}
	var lines []string
	for _, link := range detail.Links {
		lines = append(lines, "link: "+LinkLine(link))
	}
	for _, backlink := range detail.Backlinks {
		lines = append(lines, "backlink: "+BacklinkLine(backlink))
	}
	if len(lines) == 0 {
		lines = []string{"no links"}
	}
	return toolResult{
		Text: strings.Join(lines, "\n"),
		Structured: struct {
			Kind      string           `json:"kind"`
			ID        string           `json:"id"`
			ProjectID string           `json:"project_id"`
			Links     []RecordLink     `json:"links"`
			Backlinks []RecordBacklink `json:"backlinks"`
		}{Kind: detail.Kind, ID: detail.ID, ProjectID: detail.ProjectID, Links: detail.Links, Backlinks: detail.Backlinks},
	}, nil
}

func (s *Server) recordHistory(ctx context.Context, args recordArgs) (toolResult, error) {
	backend, ok := s.backend.(RecordHistoryBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_record(action=history)", "brain record show "+strings.TrimSpace(args.ID)+" --history")
	}
	id, err := requireText("id", args.ID)
	if err != nil {
		return toolResult{}, err
	}
	target, err := s.resolveTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	versions, err := backend.RecordHistory(ctx, target.ID, id)
	if err != nil {
		return toolResult{}, err
	}
	if versions == nil {
		versions = []RecordSummary{}
	}
	var lines []string
	for _, record := range versions {
		lines = append(lines, summaryLine(record))
	}
	if len(lines) == 0 {
		lines = []string{"no supersede chain"}
	}
	return toolResult{
		Text: strings.Join(lines, "\n"),
		Structured: struct {
			ProjectID string          `json:"project_id"`
			Versions  []RecordSummary `json:"versions"`
		}{ProjectID: target.ID, Versions: versions},
	}, nil
}

// recordSummarySchema is the structured output of one record summary.
func recordSummarySchema() map[string]any {
	return outputObject(map[string]any{
		"kind":       stringProp("Record kind."),
		"id":         stringProp("Record id."),
		"project_id": stringProp("Project that owns the record."),
		"title":      stringProp("Record title."),
		"status":     stringProp("Decision status."),
		"created_at": stringProp("RFC3339 creation time."),
		"updated_at": stringProp("RFC3339 last update."),
	}, "kind", "id", "project_id", "title")
}

func (s *Server) recordList(ctx context.Context, args recordArgs) (toolResult, error) {
	backend, ok := s.backend.(RecordBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_record(action=list)", "brain_search")
	}
	if args.SinceDays < 0 {
		return toolResult{}, refusalInvalid("since_days must not be negative")
	}
	if args.Status != "" {
		if err := checkDecisionStatus(args.Status); err != nil {
			return toolResult{}, err
		}
	}
	target, err := s.resolveTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	limits := pageLimitsFor("brain_records")
	filter := filterFingerprint(target.ID, strings.Join(args.Kinds, ","), fmt.Sprint(args.SinceDays), args.Status)
	offset, size, err := pageRequest("brain_records", limits, args.Limit, args.Cursor, filter)
	if err != nil {
		return toolResult{}, err
	}
	requested := fetchLimit(offset, size, limits)
	query := RecordQuery{ProjectID: target.ID, Kinds: args.Kinds, Status: args.Status, Limit: requested}
	if args.SinceDays > 0 {
		query.Since = time.Now().Add(-time.Duration(args.SinceDays) * 24 * time.Hour)
	}
	records, err := backend.ListRecords(ctx, query)
	if err != nil {
		return toolResult{}, err
	}
	pageRecords, page := paginate(records, offset, size, requested, "brain_records", filter)
	if pageRecords == nil {
		pageRecords = []RecordSummary{}
	}
	lines := []string{"project: " + target.ID}
	for _, record := range pageRecords {
		lines = append(lines, summaryLine(record))
	}
	if footer := pageFooter(page); footer != "" {
		lines = append(lines, footer)
	}
	return toolResult{
		Text:       strings.Join(lines, "\n"),
		Structured: RecordList{ProjectID: target.ID, Records: pageRecords, Page: page},
	}, nil
}

// decisionStatusArgs is one decision lifecycle request.
type decisionStatusArgs struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	SupersededBy string `json:"superseded_by"`
	Project      string `json:"project"`
}

// applyDecisionStatus changes a decision's status or supersedes it.
func (s *Server) applyDecisionStatus(ctx context.Context, args decisionStatusArgs) (toolResult, error) {
	backend, ok := s.backend.(RecordBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_judge(action=accept|reject|supersede)", "brain record status <id> <status>")
	}
	id, err := requireText("id", args.ID)
	if err != nil {
		return toolResult{}, err
	}
	superseding := strings.TrimSpace(args.SupersededBy)
	if superseding == "" {
		if err := checkDecisionStatus(args.Status); err != nil {
			return toolResult{}, err
		}
	}
	target, err := s.writeTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	var decision RecordSummary
	if superseding != "" {
		decision, err = backend.Supersede(ctx, target.ID, id, superseding)
	} else {
		decision, err = backend.SetDecisionStatus(ctx, target.ID, id, args.Status)
	}
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{Text: fmt.Sprintf("decision %s: %s", decision.ID, decision.Status), Structured: decision}, nil
}

// trustLevels are the trust values a record may carry.
var trustLevels = []string{"verified", "stated", "inferred"}

// reviewRecordArgs is one record-review request.
type reviewRecordArgs struct {
	ID      string
	IDs     []string
	Trust   string
	Project string
}

// applyReview clears the quarantine of the requested records.
func (s *Server) applyReview(ctx context.Context, args reviewRecordArgs) (toolResult, error) {
	backend, ok := s.backend.(ReviewRecordBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_judge(action=review)", "brain review --record <id>")
	}
	id := strings.TrimSpace(args.ID)
	ids := nonEmptyStrings(args.IDs)
	all := false
	switch {
	case id != "" && len(ids) > 0:
		return toolResult{}, refusalInvalid("pass either id or ids, not both")
	case id != "":
		ids = []string{id}
	case len(ids) == 0:
		return toolResult{}, refusalInvalidWith("pass id, ids or ids=[\"*\"] to review every unreviewed record", "brain_judge(action=review, ids=[\"*\"])")
	}
	if slices.Contains(ids, "*") {
		if len(ids) > 1 {
			return toolResult{}, refusalInvalid("ids must be [\"*\"] or explicit record ids, not both")
		}
		all = true
		ids = nil
	}
	trust := strings.TrimSpace(args.Trust)
	if trust != "" && !slices.Contains(trustLevels, trust) {
		return toolResult{}, refusalInvalidf("unknown trust %q (%s)", trust, strings.Join(trustLevels, ", "))
	}
	target, err := s.writeTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	if all {
		result, err := backend.ReviewRecord(ctx, ReviewRecordInput{ProjectID: target.ID, All: true, Trust: trust})
		if err != nil {
			return toolResult{}, err
		}
		return reviewRecordResult(target.ID, result), nil
	}
	reviewed := make([]RecordSummary, 0, len(ids))
	for _, recordID := range ids {
		result, err := backend.ReviewRecord(ctx, ReviewRecordInput{ProjectID: target.ID, ID: recordID, Trust: trust})
		if err != nil {
			return toolResult{}, fmt.Errorf("%d of %d records reviewed before the failure: %w", len(reviewed), len(ids), err)
		}
		reviewed = append(reviewed, result.Reviewed...)
	}
	return reviewRecordResult(target.ID, ReviewRecordResult{ProjectID: target.ID, Reviewed: reviewed}), nil
}

func reviewRecordResult(projectID string, result ReviewRecordResult) toolResult {
	if result.Reviewed == nil {
		result.Reviewed = []RecordSummary{}
	}
	if result.ProjectID == "" {
		result.ProjectID = projectID
	}
	text := "no unreviewed records"
	if len(result.Reviewed) > 0 {
		lines := make([]string, 0, len(result.Reviewed)+1)
		lines = append(lines, fmt.Sprintf("%d reviewed", len(result.Reviewed)))
		for _, record := range result.Reviewed {
			lines = append(lines, summaryLine(record))
		}
		text = strings.Join(lines, "\n")
	}
	return toolResult{Text: text, Structured: result}
}

// checkDecisionStatus rejects a status outside the decision lifecycle; the
// JSON schema enum is advisory for clients, so the handler enforces it too.
// applyPin sets or clears the pinned flag of one record.
func (s *Server) applyPin(ctx context.Context, args judgeArgs, pinned bool) (toolResult, error) {
	id, err := requireText("id", args.ID)
	if err != nil {
		return toolResult{}, err
	}
	target, err := s.writeTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	record, err := s.pinRecord(ctx, target.ID, id, pinned)
	if err != nil {
		return toolResult{}, err
	}
	verb := "unpinned"
	if pinned {
		verb = "pinned"
	}
	return toolResult{Text: fmt.Sprintf("record %s: %s", record.ID, verb), Structured: record}, nil
}

// pinRecord dispatches to whichever backend interface is available.
func (s *Server) pinRecord(ctx context.Context, projectID, id string, pinned bool) (RecordSummary, error) {
	if p, ok := s.backend.(RecordPinner); ok {
		return p.SetPinned(ctx, projectID, id, pinned)
	}
	if p, ok := s.backend.(RecordPinSetter); ok {
		return p.SetRecordPinned(ctx, projectID, id, pinned)
	}
	if p, ok := s.backend.(RecordPinToggler); ok {
		return p.PinRecord(ctx, projectID, id, pinned)
	}
	if p, ok := s.backend.(RecordPinActioner); ok {
		if pinned {
			return p.PinRecord(ctx, projectID, id)
		}
		return p.UnpinRecord(ctx, projectID, id)
	}
	return RecordSummary{}, unsupportedBackend("brain_judge(action=pin|unpin)", "brain record pin <id>")
}

func checkDecisionStatus(status string) error {
	if status == "" {
		return refusalUnsupported("pass status, or supersede with a target id", "pass status=accepted|rejected, or supersede with to=<decision id>")
	}
	if !slices.Contains(decisionStatuses, status) {
		return refusalInvalidf("unknown status %q (proposed, accepted, rejected, superseded)", status)
	}
	return nil
}

// summaryLine is the human form of a listed record.
func summaryLine(record RecordSummary) string {
	if record.Status == "" {
		return fmt.Sprintf("%s %s %s", record.Kind, record.ID, record.Title)
	}
	return fmt.Sprintf("%s %s [%s] %s", record.Kind, record.ID, record.Status, record.Title)
}

// detailText renders the non-empty fields of a record, one per line.
func detailText(detail RecordDetail) string {
	fields := [][2]string{
		{"kind", detail.Kind}, {"id", detail.ID}, {"project", detail.ProjectID}, {"title", detail.Title},
		{"status", detail.Status}, {"body", detail.Body}, {"context", detail.Context},
		{"alternatives", strings.Join(detail.Alternatives, "; ")}, {"consequences", strings.Join(detail.Consequences, "; ")},
		{"evidence", strings.Join(detail.Evidence, "; ")}, {"hypothesis", detail.Hypothesis}, {"result", detail.Result},
		{"reason", detail.Reason}, {"next_action", detail.NextAction}, {"git_context", detail.GitContext},
	}
	var lines []string
	for _, field := range fields {
		if field[1] != "" {
			lines = append(lines, field[0]+": "+field[1])
		}
	}
	for _, link := range detail.Links {
		lines = append(lines, "link: "+LinkLine(link))
	}
	for _, backlink := range detail.Backlinks {
		lines = append(lines, "backlink: "+BacklinkLine(backlink))
	}
	for _, relation := range detail.Relations {
		line := "relation: " + relation.Relation + " " + relation.Target + " [" + relation.Status + "]"
		if relation.Reason != "" {
			line += " " + relation.Reason
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// LinkLine is the human form of an outgoing link:
// `<ref> -> <kind> [<status>] <title>` or `<ref> -> missing (<detail>)`.
func LinkLine(link RecordLink) string {
	line := link.Ref
	if link.Text != "" {
		line += " (" + link.Text + ")"
	}
	if link.State != "found" {
		return line + " -> " + link.State + " (" + link.Detail + ")"
	}
	line += " -> " + link.Kind
	if link.Status != "" {
		line += " [" + link.Status + "]"
	}
	return line + " " + link.Title
}

// BacklinkLine is the human form of a backlink:
// `<kind> <id> [<status>] <title> (in <field>)`.
func BacklinkLine(backlink RecordBacklink) string {
	line := backlink.Kind + " " + backlink.ID
	if backlink.Status != "" {
		line += " [" + backlink.Status + "]"
	}
	return line + " " + backlink.Title + " (in " + backlink.Field + ")"
}

// argsOrDefault returns the arguments as a JSON object, so an absent or null
// argument set decodes to {}.
func argsOrDefault(raw json.RawMessage) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return []byte("{}")
	}
	return trimmed
}
