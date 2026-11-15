package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/mcpserver"
)

// runRelation implements `brain relation`: list, show and judge the typed
// relations between records.
func runRelation(args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) {
		fmt.Fprint(w, `Usage:
  brain relation list [project] [--pending|--accepted|--rejected]
                      [--relation <verb>]... [--id <record>] [--target <record>]
                      [--limit N] [--json]
  brain relation show <id> [project] [--json]
  brain relation judge <from> <to> --verdict <verb> --reason <text>
                        [--confidence 0..1] [--by human|agent] [--project p] [--json]

Relations live in record frontmatter and are the source of truth; the search
index keeps a derived copy. Pending relations are candidates: search ranking and
context ignore them, review lists them, and only `+"`brain relation judge`"+` accepts
or rejects one. Judging never edits or deletes the other record.

Verbs: `+strings.Join(domain.RelationKindNames(), ", ")+`
`)
	}
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return runRelationList(args[1:], stdout, stderr)
	case "show":
		return runRelationShow(args[1:], stdout, stderr)
	case "judge":
		return runRelationJudge(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "brain relation: unknown subcommand %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func runRelationList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("relation list", stderr)
	project := fs.String("project", "", "project id or alias")
	pending := fs.Bool("pending", false, "list pending relations only")
	accepted := fs.Bool("accepted", false, "list accepted relations only")
	rejected := fs.Bool("rejected", false, "list rejected relations only")
	var kinds repeatedFlag
	fs.Var(&kinds, "relation", "keep this relation verb (repeatable)")
	source := fs.String("id", "", "list the relations of one source record")
	target := fs.String("target", "", "list the relations that point at one record")
	limit := fs.Int("limit", 0, "maximum relations; 0 selects the default of 200")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	statuses := make([]domain.RelationStatus, 0, 3)
	if *pending {
		statuses = append(statuses, domain.RelationPending)
	}
	if *accepted {
		statuses = append(statuses, domain.RelationAccepted)
	}
	if *rejected {
		statuses = append(statuses, domain.RelationRejected)
	}
	parsedKinds, err := parseRelationKinds(kinds)
	if err != nil {
		fmt.Fprintf(stderr, "brain relation list: %v\n", err)
		return 2
	}
	if *limit < 0 {
		fmt.Fprintln(stderr, "brain relation list: --limit must not be negative")
		return 2
	}
	ctx := context.Background()
	runtime, selected, code := stateTarget(ctx, *cfgPath, *project, positional, stderr, "relation list")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	views, err := runtime.App.Relations(ctx, app.RelationListOptions{
		ProjectID: selected.Project.ID,
		ID:        *source,
		TargetID:  *target,
		Statuses:  statuses,
		Kinds:     parsedKinds,
		Limit:     *limit,
	})
	if err != nil {
		return reportError(stderr, "relation list", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "relation list", jsonRelationList{
			ProjectID: selected.Project.ID,
			Relations: toJSONRelations(views),
		})
	}
	if len(views) == 0 {
		fmt.Fprintf(stdout, "no relations in %s\n", selected.Project.ID)
		return 0
	}
	for _, view := range views {
		fmt.Fprintln(stdout, relationViewLine(view))
	}
	return 0
}

func runRelationShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("relation show", stderr)
	project := fs.String("project", "", "project id or alias")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("relation show", positional, 1, "<id>", stderr) {
		return 2
	}
	ctx := context.Background()
	runtime, selected, code := stateTarget(ctx, *cfgPath, *project, nil, stderr, "relation show")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	id := positional[0]
	record, err := runtime.App.GetRecord(ctx, selected.Project.ID, id)
	if err != nil {
		return reportError(stderr, "relation show", err)
	}
	outgoing, err := runtime.App.Relations(ctx, app.RelationListOptions{ProjectID: selected.Project.ID, ID: id})
	if err != nil {
		return reportError(stderr, "relation show", err)
	}
	incoming, err := runtime.App.Relations(ctx, app.RelationListOptions{ProjectID: selected.Project.ID, TargetID: id})
	if err != nil {
		return reportError(stderr, "relation show", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "relation show", jsonRelationShow{
			Record:   toJSONRecordSummary(record.Summary()),
			Outgoing: toJSONRelations(outgoing),
			Incoming: toJSONRelations(incoming),
		})
	}
	summary := record.Summary()
	fmt.Fprintf(stdout, "%s %s: %s\n", summary.Kind, summary.ID, summary.Title)
	printRelationSection(stdout, "Relations", outgoing)
	printRelationSection(stdout, "Backlinks", incoming)
	return 0
}

func runRelationJudge(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("relation judge", stderr)
	project := fs.String("project", "", "project id or alias")
	verdict := fs.String("verdict", "", "relation verb: "+strings.Join(domain.RelationKindNames(), ", "))
	reason := fs.String("reason", "", "why the pair is related, conflicting or unrelated")
	confidence := fs.Float64("confidence", 0, "judge confidence between 0 and 1")
	by := fs.String("by", string(domain.VerdictByHuman), "who judged: human or agent")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("relation judge", positional, 2, "<from> <to> --verdict <verb> --reason <text>", stderr) {
		return 2
	}
	if strings.TrimSpace(*verdict) == "" {
		fmt.Fprintln(stderr, "brain relation judge: --verdict is required")
		return 2
	}
	if strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(stderr, "brain relation judge: --reason is required")
		return 2
	}
	verdictKind, err := domain.ParseRelationKind(*verdict)
	if err != nil {
		fmt.Fprintf(stderr, "brain relation judge: %v\n", err)
		return 2
	}
	verdictBy := domain.VerdictBy(strings.TrimSpace(*by))
	if !verdictBy.Valid() {
		fmt.Fprintf(stderr, "brain relation judge: unknown --by %q (human, agent)\n", *by)
		return 2
	}
	ctx := context.Background()
	runtime, selected, code := stateTarget(ctx, *cfgPath, *project, nil, stderr, "relation judge")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	result, err := runtime.App.JudgeRelation(ctx, app.JudgeOptions{
		ProjectID:  selected.Project.ID,
		From:       positional[0],
		To:         positional[1],
		Verdict:    verdictKind,
		Reason:     *reason,
		Confidence: *confidence,
		By:         verdictBy,
	})
	if err != nil {
		return reportError(stderr, "relation judge", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "relation judge", jsonRelationJudge{
			Relation: toJSONRelation(result.Relation), Changed: result.Changed,
		})
	}
	status := "judged"
	if !result.Changed {
		status = "unchanged"
	}
	fmt.Fprintf(stdout, "%s: %s %s -> %s [%s]\n", status,
		result.Relation.Source.ID, result.Relation.Relation.Relation, result.Relation.TargetID,
		result.Relation.Relation.Status)
	return 0
}

// parseRelationKinds parses repeated --relation values.
func parseRelationKinds(values []string) ([]domain.RelationKind, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]domain.RelationKind, 0, len(values))
	for _, value := range values {
		kind, err := domain.ParseRelationKind(value)
		if err != nil {
			return nil, err
		}
		out = append(out, kind)
	}
	return out, nil
}

// relationViewLine is the human form of one listed relation.
func relationViewLine(view app.RelationView) string {
	line := fmt.Sprintf("[%s] %s %s -%s-> %s", view.Relation.Status,
		view.Source.Kind, view.Source.ID, view.Relation.Relation, view.TargetID)
	if view.TargetState != app.RelationTargetFound {
		line += " (" + view.TargetState + ")"
	}
	if !view.Relation.JudgedAt.IsZero() {
		line += " judged " + view.Relation.JudgedAt.UTC().Format("2006-01-02")
		if view.Relation.VerdictBy != "" {
			line += " by " + string(view.Relation.VerdictBy)
		}
	}
	if view.Relation.IsPending() {
		line += fmt.Sprintf(" pending %d days", view.AgeDays)
	}
	if view.Relation.Reason != "" {
		line += ": " + view.Relation.Reason
	}
	return line
}

func printRelationSection(w io.Writer, title string, views []app.RelationView) {
	fmt.Fprintf(w, "%s:\n", title)
	if len(views) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	for _, view := range views {
		fmt.Fprintf(w, "  - %s\n", relationViewLine(view))
	}
}

// printRecordRelations renders the typed relations carried by one record, as
// `brain record show` reports them.
func printRecordRelations(w io.Writer, relations []mcpserver.RecordRelation) {
	fmt.Fprintln(w, "Relations:")
	if len(relations) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	for _, relation := range relations {
		line := fmt.Sprintf("  - [%s] %s %s", relation.Status, relation.Relation, relation.Target)
		if relation.JudgedAt != "" {
			line += " judged " + relation.JudgedAt
		}
		if relation.Reason != "" {
			line += ": " + relation.Reason
		}
		fmt.Fprintln(w, line)
	}
}

// jsonRelation is one relation in a --json payload.
type jsonRelation struct {
	ProjectID   string          `json:"project_id"`
	Source      jsonRelationEnd `json:"source"`
	Target      jsonRelationEnd `json:"target"`
	Relation    string          `json:"relation"`
	Status      string          `json:"status"`
	Reason      string          `json:"reason,omitempty"`
	Confidence  float64         `json:"confidence,omitempty"`
	Direction   string          `json:"direction,omitempty"`
	JudgedAt    *string         `json:"judged_at"`
	VerdictBy   string          `json:"verdict_by,omitempty"`
	TargetState string          `json:"target_state"`
	AgeDays     int             `json:"age_days"`
}

// jsonRelationEnd is one end of a relation.
type jsonRelationEnd struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Title     string `json:"title"`
	Status    string `json:"status,omitempty"`
}

func toJSONRelations(views []app.RelationView) []jsonRelation {
	out := make([]jsonRelation, 0, len(views))
	for _, view := range views {
		out = append(out, toJSONRelation(view))
	}
	return out
}

func toJSONRelation(view app.RelationView) jsonRelation {
	out := jsonRelation{
		ProjectID: view.ProjectID,
		Source: jsonRelationEnd{
			Kind: view.Source.Kind, ID: view.Source.ID, ProjectID: view.Source.ProjectID,
			Title: view.Source.Title, Status: view.Source.Status,
		},
		Target:      jsonRelationEnd{ID: view.TargetID},
		Relation:    string(view.Relation.Relation),
		Status:      string(view.Relation.Status),
		Reason:      view.Relation.Reason,
		Confidence:  view.Relation.Confidence,
		Direction:   view.Direction,
		JudgedAt:    jsonTime(view.Relation.JudgedAt),
		VerdictBy:   string(view.Relation.VerdictBy),
		TargetState: view.TargetState,
		AgeDays:     view.AgeDays,
	}
	if view.Target != nil {
		out.Target = jsonRelationEnd{
			Kind: view.Target.Kind, ID: view.Target.ID, ProjectID: view.Target.ProjectID,
			Title: view.Target.Title, Status: view.Target.Status,
		}
	}
	return out
}

// toMCPRelations projects a record's relations for the record detail output.
func toMCPRelations(relations domain.RelationList) []mcpserver.RecordRelation {
	normalized := relations.Normalize()
	out := make([]mcpserver.RecordRelation, 0, len(normalized))
	for _, relation := range normalized {
		entry := mcpserver.RecordRelation{
			Target: relation.Target, Relation: string(relation.Relation), Reason: relation.Reason,
			Confidence: relation.Confidence, Status: string(relation.Status), VerdictBy: string(relation.VerdictBy),
		}
		if !relation.JudgedAt.IsZero() {
			entry.JudgedAt = relation.JudgedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, entry)
	}
	return out
}

// jsonRelationList is `brain relation list --json`.
type jsonRelationList struct {
	ProjectID string         `json:"project_id"`
	Relations []jsonRelation `json:"relations"`
}

// jsonRelationShow is `brain relation show --json`.
type jsonRelationShow struct {
	Record   jsonRecordSummary `json:"record"`
	Outgoing []jsonRelation    `json:"outgoing"`
	Incoming []jsonRelation    `json:"incoming"`
}

// jsonRelationJudge is `brain relation judge --json`.
type jsonRelationJudge struct {
	Relation jsonRelation `json:"relation"`
	Changed  bool         `json:"changed"`
}
