package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/mcpserver"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const recordTimeLayout = "2006-01-02 15:04"

// runRecord implements `brain record`: list, read, change the status of,
// supersede, trash and restore single records of any kind.
func runRecord(args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) {
		fmt.Fprint(w, `Usage:
  brain record list [project] [--kind note,decision] [--since 7d|24h]
                    [--status <status>] [--limit N] [--json]
  brain record show <id> [--project p] [--json]
  brain record status <id> <proposed|accepted|rejected|superseded> [--project p]
  brain record supersede <old-id> --by <new-id> [--project p]
  brain record trash <id> --yes [--project p]
  brain record restore <id> [--project p]
  brain record trash-list [project] [--json]

[project] is an id, alias or path (default: the working directory's project).
Trash is reversible: restore brings a trashed record back.
`)
	}
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return runRecordList(args[1:], stdout, stderr)
	case "show":
		return runRecordShow(args[1:], stdout, stderr)
	case "status":
		return runRecordStatus(args[1:], stdout, stderr)
	case "supersede":
		return runRecordSupersede(args[1:], stdout, stderr)
	case "trash":
		return runRecordTrash(args[1:], stdout, stderr)
	case "restore":
		return runRecordRestore(args[1:], stdout, stderr)
	case "trash-list":
		return runRecordTrashList(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "brain record: unknown subcommand %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func runRecordList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("record list", stderr)
	project := fs.String("project", "", "project id or alias")
	kind := fs.String("kind", "", "comma-separated kinds: note, decision, attempt, handoff")
	since := fs.String("since", "", "only records created or updated within this window, e.g. 24h or 7d")
	status := fs.String("status", "", "keep decisions with this status")
	limit := fs.Int("limit", 0, "maximum records; 0 selects the default of 50")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if *limit < 0 {
		fmt.Fprintln(stderr, "brain record list: --limit must not be negative")
		return 2
	}
	now := time.Now()
	sinceTime, err := parseRecordSince(*since, now)
	if err != nil {
		fmt.Fprintf(stderr, "brain record list: %v\n", err)
		return 2
	}
	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, *project, positional, stderr, "record list")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	records, err := runtime.App.ListRecords(ctx, app.ListRecordsOptions{
		ProjectID: target.Project.ID,
		Kinds:     splitList(*kind),
		Since:     sinceTime,
		Status:    *status,
		Limit:     *limit,
	})
	if err != nil {
		return reportError(stderr, "record list", err)
	}
	summaries := mapAll(records, toRecordSummary)
	if *asJSON {
		return writeJSONReport(stdout, stderr, "record list", summaries)
	}
	for _, summary := range summaries {
		fmt.Fprintln(stdout, recordListLine(summary))
	}
	return 0
}

func runRecordShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("record show", stderr)
	project := fs.String("project", "", "project id or alias")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("record show", positional, 1, "<id>", stderr) {
		return 2
	}
	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, *project, nil, stderr, "record show")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	links, err := runtime.App.RecordLinks(ctx, target.Project.ID, positional[0])
	if err != nil {
		return reportError(stderr, "record show", err)
	}
	detail := toRecordDetailWithLinks(links)
	if *asJSON {
		return writeJSONReport(stdout, stderr, "record show", jsonRecordShow{Kind: detail.Kind, Record: detail})
	}
	printRecordDetail(stdout, detail)
	printLinkSections(stdout, detail.Links, detail.Backlinks)
	return 0
}

func runRecordStatus(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("record status", stderr)
	project := fs.String("project", "", "project id or alias")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("record status", positional, 2, "<id> <status>", stderr) {
		return 2
	}
	status := domain.DecisionStatus(positional[1])
	if !status.Valid() {
		fmt.Fprintf(stderr, "brain record status: unknown status %q (proposed, accepted, rejected, superseded)\n", positional[1])
		return 2
	}
	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, *project, nil, stderr, "record status")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	decision, err := runtime.App.SetDecisionStatus(ctx, target.Project.ID, positional[0], status)
	if err != nil {
		return reportError(stderr, "record status", err)
	}
	fmt.Fprintf(stdout, "decision %s: %s\n", decision.ID, decision.Status)
	return 0
}

func runRecordSupersede(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("record supersede", stderr)
	project := fs.String("project", "", "project id or alias")
	by := fs.String("by", "", "id of the live decision that replaces this one")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("record supersede", positional, 1, "<old-id> --by <new-id>", stderr) {
		return 2
	}
	newID := strings.TrimSpace(*by)
	if newID == "" {
		fmt.Fprintln(stderr, "brain record supersede: --by <new-id> is required")
		return 2
	}
	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, *project, nil, stderr, "record supersede")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	decision, err := runtime.App.Supersede(ctx, target.Project.ID, positional[0], newID)
	if err != nil {
		return reportError(stderr, "record supersede", err)
	}
	fmt.Fprintf(stdout, "decision %s: superseded by %s\n", decision.ID, newID)
	return 0
}

func runRecordTrash(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("record trash", stderr)
	project := fs.String("project", "", "project id or alias")
	yes := fs.Bool("yes", false, "confirm moving the record to the trash")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("record trash", positional, 1, "<id> --yes", stderr) {
		return 2
	}
	if !*yes {
		fmt.Fprintf(stderr, "brain record trash: refusing without --yes; the record moves to the trash (restore with brain record restore %s)\n", positional[0])
		return 2
	}
	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, *project, nil, stderr, "record trash")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	record, err := runtime.App.TrashRecord(ctx, target.Project.ID, positional[0])
	if err != nil {
		return reportError(stderr, "record trash", err)
	}
	id := record.Summary().ID
	fmt.Fprintf(stdout, "trashed %s %s (brain record restore %s)\n", record.Kind, id, id)
	return 0
}

func runRecordRestore(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("record restore", stderr)
	project := fs.String("project", "", "project id or alias")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("record restore", positional, 1, "<id>", stderr) {
		return 2
	}
	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, *project, nil, stderr, "record restore")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	record, err := runtime.App.RestoreRecord(ctx, target.Project.ID, positional[0])
	if err != nil {
		return reportError(stderr, "record restore", err)
	}
	fmt.Fprintf(stdout, "restored %s %s\n", record.Kind, record.Summary().ID)
	return 0
}

func runRecordTrashList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("record trash-list", stderr)
	project := fs.String("project", "", "project id or alias")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, *project, positional, stderr, "record trash-list")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	trashed, err := runtime.App.ListTrash(ctx, target.Project.ID)
	if err != nil {
		return reportError(stderr, "record trash-list", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "record trash-list", mapAll(trashed, toJSONTrashed))
	}
	if len(trashed) == 0 {
		fmt.Fprintf(stdout, "trash of %s is empty\n", target.Project.ID)
		return 0
	}
	for _, record := range trashed {
		fmt.Fprintf(stdout, "%s  %s  %s\n", record.Kind, record.ID, record.Title)
	}
	return 0
}

// expectArgs reports a usage error unless exactly want positional arguments
// were given.
func expectArgs(name string, positional []string, want int, usage string, stderr io.Writer) bool {
	if len(positional) == want {
		return true
	}
	fmt.Fprintf(stderr, "brain %s: expected %s\n", name, usage)
	return false
}

// parseRecordSince turns --since into the earliest time to keep. Go durations
// and the day suffix (7d) are accepted; an empty value keeps everything.
func parseRecordSince(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	var window time.Duration
	if days, ok := strings.CutSuffix(value, "d"); ok {
		count, err := strconv.Atoi(days)
		if err != nil {
			return time.Time{}, fmt.Errorf("--since %q: want a duration such as 24h or 7d", value)
		}
		window = time.Duration(count) * 24 * time.Hour
	} else {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return time.Time{}, fmt.Errorf("--since %q: want a duration such as 24h or 7d", value)
		}
		window = parsed
	}
	if window <= 0 {
		return time.Time{}, fmt.Errorf("--since %q must be positive", value)
	}
	return now.Add(-window), nil
}

// recordListLine is `<time>  <kind>  <id>  [<status>]  <title>`; the status
// column is shown for decisions only.
func recordListLine(summary mcpserver.RecordSummary) string {
	line := summary.CreatedAt.Local().Format(recordTimeLayout) + "  " + summary.Kind + "  " + summary.ID
	if summary.Status != "" {
		line += "  [" + summary.Status + "]"
	}
	return line + "  " + summary.Title
}

// printRecordDetail renders every non-empty field; multi-line text is
// indented under its label.
func printRecordDetail(w io.Writer, detail mcpserver.RecordDetail) {
	printRecordLine(w, "kind", detail.Kind)
	printRecordLine(w, "id", detail.ID)
	printRecordLine(w, "project", detail.ProjectID)
	printRecordLine(w, "title", detail.Title)
	printRecordLine(w, "status", detail.Status)
	printRecordText(w, "body", detail.Body)
	printRecordText(w, "context", detail.Context)
	printRecordItems(w, "alternatives", detail.Alternatives)
	printRecordItems(w, "consequences", detail.Consequences)
	printRecordItems(w, "evidence", detail.Evidence)
	printRecordText(w, "hypothesis", detail.Hypothesis)
	printRecordText(w, "result", detail.Result)
	printRecordText(w, "reason", detail.Reason)
	printRecordLine(w, "next_action", detail.NextAction)
	printRecordText(w, "git_context", detail.GitContext)
	if !detail.CreatedAt.IsZero() {
		printRecordLine(w, "created", detail.CreatedAt.Local().Format(recordTimeLayout))
	}
	if !detail.UpdatedAt.IsZero() {
		printRecordLine(w, "updated", detail.UpdatedAt.Local().Format(recordTimeLayout))
	}
}

func printRecordLine(w io.Writer, label, value string) {
	if value != "" {
		fmt.Fprintf(w, "%s: %s\n", label, value)
	}
}

func printRecordText(w io.Writer, label, text string) {
	if text == "" {
		return
	}
	if !strings.Contains(text, "\n") {
		printRecordLine(w, label, text)
		return
	}
	fmt.Fprintf(w, "%s:\n", label)
	for _, line := range strings.Split(text, "\n") {
		fmt.Fprintf(w, "  %s\n", line)
	}
}

func printRecordItems(w io.Writer, label string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "%s:\n", label)
	for _, item := range items {
		fmt.Fprintf(w, "  - %s\n", item)
	}
}

// jsonRecordShow is `brain record show --json`: the record kind and its
// fields.
type jsonRecordShow struct {
	Kind   string                 `json:"kind"`
	Record mcpserver.RecordDetail `json:"record"`
}

// jsonTrashedRecord is one entry of `brain record trash-list --json`.
type jsonTrashedRecord struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Title string `json:"title"`
}

func toJSONTrashed(record ports.TrashedRecord) jsonTrashedRecord {
	return jsonTrashedRecord{Kind: record.Kind, ID: record.ID, Title: record.Title}
}

func toRecordSummary(summary app.RecordSummary) mcpserver.RecordSummary {
	return mcpserver.RecordSummary{
		Kind:      summary.Kind,
		ID:        summary.ID,
		ProjectID: summary.ProjectID,
		Title:     summary.Title,
		Status:    summary.Status,
		CreatedAt: summary.CreatedAt,
		UpdatedAt: summary.UpdatedAt,
	}
}

// toRecordDetail fills the fields of the record's kind.
func toRecordDetail(record app.Record) mcpserver.RecordDetail {
	summary := record.Summary()
	detail := mcpserver.RecordDetail{
		Kind:      summary.Kind,
		ID:        summary.ID,
		ProjectID: summary.ProjectID,
		Title:     summary.Title,
		Status:    summary.Status,
		CreatedAt: summary.CreatedAt,
		UpdatedAt: summary.UpdatedAt,
	}
	switch {
	case record.Note != nil:
		detail.Body = record.Note.Body
	case record.Decision != nil:
		detail.Context = record.Decision.Context
		detail.Alternatives = record.Decision.Alternatives
		detail.Consequences = record.Decision.Consequences
		detail.Evidence = record.Decision.Evidence
	case record.Attempt != nil:
		detail.Hypothesis = record.Attempt.Hypothesis
		detail.Result = record.Attempt.Result
		detail.Reason = record.Attempt.Reason
	case record.Handoff != nil:
		detail.Body = record.Handoff.Summary
		detail.NextAction = record.Handoff.NextAction
		detail.GitContext = record.Handoff.GitContext
	}
	return detail
}
