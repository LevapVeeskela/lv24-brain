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
// change decision status. Trash and restore stay CLI-only. The tools are
// listed only for such backends.
type RecordBackend interface {
	ListRecords(ctx context.Context, query RecordQuery) ([]RecordSummary, error)
	GetRecord(ctx context.Context, projectID, id string) (RecordDetail, error)
	// SetDecisionStatus changes a decision's status and returns it.
	SetDecisionStatus(ctx context.Context, projectID, id, status string) (RecordSummary, error)
	// Supersede marks decision oldID superseded by decision newID.
	Supersede(ctx context.Context, projectID, oldID, newID string) (RecordSummary, error)
}

// RecordList is the structured result of brain_records.
type RecordList struct {
	ProjectID string          `json:"project_id"`
	Records   []RecordSummary `json:"records"`
	// Page is embedded, so page/page_size/has_more/next_cursor are top-level.
	Page
}

func (s *Server) recordTools() []tool {
	if _, ok := s.backend.(RecordBackend); !ok && !s.catalog {
		return nil
	}
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	return []tool{
		{
			Name:        "brain_records",
			Annotations: readOnlyAnnotations("List records"),
			Description: "List a project's notes, decisions, attempts and handoffs, newest first. Use it to find record ids, " +
				"for example proposed decisions (kinds [\"decision\"], status \"proposed\") awaiting confirmation.",
			InputSchema: objectSchema(nil, map[string]any{
				"project":    project,
				"kinds":      stringArrayProp("Keep only these kinds: note, decision, attempt, handoff. Omit for every kind."),
				"since_days": intProp("Keep records created or updated in the last N days; 0 keeps all.", 0),
				"status":     enumProp("Keep decisions with this status (drops other kinds).", decisionStatuses),
				"limit":      intProp("Page size; 0 selects the default page of 50.", 0),
				"cursor":     cursorProp(),
			}),
			OutputSchema: outputObject(pageOutput(map[string]any{
				"project_id": stringProp("Project the records belong to."),
				"records":    arrayProp("Records, newest first.", recordSummarySchema()),
			}), "project_id", "records", "page", "page_size", "has_more"),
			Limits: limitsOf(pageLimitsFor("brain_records")),
			run:    s.recordsList,
		},
		{
			Name:        "brain_record",
			Annotations: readOnlyAnnotations("Read a record"),
			Description: "Read one record in full by id (note, decision, attempt or handoff), including its body, context, evidence or reason, its [[id]] links (resolved) and backlinks from the same project.",
			InputSchema: objectSchema([]string{"id"}, map[string]any{
				"id":      stringProp("Record id, as returned by brain_records or brain_search."),
				"project": project,
			}),
			OutputSchema: outputObject(map[string]any{
				"kind":         stringProp("Record kind."),
				"id":           stringProp("Record id."),
				"project_id":   stringProp("Project that owns the record."),
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
			}, "kind", "id", "project_id", "title", "links", "backlinks"),
			run: s.recordShow,
		},
		{
			Name:        "brain_decision_status",
			Annotations: mutatingAnnotations("Change decision status"),
			Description: "Change a decision's status. Accept a proposed decision once it is confirmed; reject one that was dropped; " +
				"mark it superseded by the decision that replaces it by passing superseded_by (status is then ignored).",
			InputSchema: objectSchema([]string{"id"}, map[string]any{
				"id":            stringProp("Decision id."),
				"status":        enumProp("New status; required unless superseded_by is given.", decisionStatuses),
				"superseded_by": stringProp("Id of the live decision that replaces this one."),
				"project":       project,
			}),
			OutputSchema: recordSummarySchema(),
			confirm:      confirmSupersede,
			run:          s.decisionStatus,
		},
	}
}

// recordSummarySchema is the structured output of one decision summary.
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

// confirmSupersede asks the user to confirm making a decision superseded: the
// change is a curation decision about the record, not an append.
func confirmSupersede(args json.RawMessage) *confirmation {
	var probe struct {
		ID           string `json:"id"`
		SupersededBy string `json:"superseded_by"`
	}
	if err := json.Unmarshal(argsOrDefault(args), &probe); err != nil {
		return nil
	}
	id := strings.TrimSpace(probe.ID)
	superseding := strings.TrimSpace(probe.SupersededBy)
	if id == "" || superseding == "" {
		return nil
	}
	return &confirmation{Message: fmt.Sprintf("Mark decision %s as superseded by %s? The old decision stays in the record but is no longer current.", id, superseding)}
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

func (s *Server) recordsList(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Project   string   `json:"project"`
		Kinds     []string `json:"kinds"`
		SinceDays int      `json:"since_days"`
		Status    string   `json:"status"`
		Limit     int      `json:"limit"`
		Cursor    string   `json:"cursor"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	if args.SinceDays < 0 {
		return toolResult{}, refusalInvalid("since_days must not be negative")
	}
	if args.Status != "" {
		if err := checkDecisionStatus(args.Status); err != nil {
			return toolResult{}, err
		}
	}
	target, err := s.backend.Resolve(ctx, strings.TrimSpace(args.Project))
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
	records, err := s.backend.(RecordBackend).ListRecords(ctx, query)
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

func (s *Server) recordShow(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		ID      string `json:"id"`
		Project string `json:"project"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	id, err := requireText("id", args.ID)
	if err != nil {
		return toolResult{}, err
	}
	target, err := s.backend.Resolve(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	detail, err := s.backend.(RecordBackend).GetRecord(ctx, target.ID, id)
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{Text: detailText(detail), Structured: detail}, nil
}

func (s *Server) decisionStatus(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		ID           string `json:"id"`
		Status       string `json:"status"`
		SupersededBy string `json:"superseded_by"`
		Project      string `json:"project"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
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
	target, err := s.backend.Resolve(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	backend := s.backend.(RecordBackend)
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

// checkDecisionStatus rejects a status outside the decision lifecycle; the
// JSON schema enum is advisory for clients, so the handler enforces it too.
func checkDecisionStatus(status string) error {
	if status == "" {
		return refusalUnsupported("pass status, or superseded_by to mark the decision superseded", "pass status=accepted|rejected, or superseded_by=<decision id>")
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
