package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/mcpserver"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const recordTimeLayout = "2006-01-02 15:04"

// runRecord implements `brain record`: list, read, edit, change the status of,
// supersede, trash and restore single records of any kind.
func runRecord(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) {
		fmt.Fprint(w, `Usage:
  brain record list [project] [--kind note,decision] [--since 7d|24h]
                    [--status <status>] [--limit N]
                    [--as-of <time|date>] [--include-superseded|--all-time] [--json]
  brain record show <id> [--project p] [--history] [--as-of <time|date>] [--json]
  brain record edit <id> [--title T] [--body-file F|-] [--tags a,b]
                         [--expected-updated-at RFC3339] [--allow-duplicate]
                         [--dry-run] [--project p] [--json]
  brain record status <id> <proposed|accepted|rejected|superseded> [--project p]
  brain record supersede <old-id> --by <new-id> [--project p]
  brain record review <id> [--trust verified|stated|inferred] [--review-after 90d|2027-02-05] [--project p]
  brain record review <id> --clear-review-after [--project p]
  brain record review --all [--trust verified|stated|inferred] [--project p]
  brain record trash <id> --yes [--project p]
  brain record restore <id> [--project p]
  brain record merge <survivor> <duplicate>... [--dry-run|--apply] [--json]
  brain record merge --rollback <manifest> [--json]
  brain record trash-list [project] [--json]

[project] is an id, alias or path (default: the working directory's project).
By default only current records are listed; --as-of reads the records valid at
an instant and --include-superseded (--all-time) adds the ended ones.
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
	case "edit":
		return runRecordEdit(args[1:], stdin, stdout, stderr)
	case "status":
		return runRecordStatus(args[1:], stdout, stderr)
	case "supersede":
		return runRecordSupersede(args[1:], stdout, stderr)
	case "review":
		return runRecordReview(args[1:], stdout, stderr)
	case "trash":
		return runRecordTrash(args[1:], stdout, stderr)
	case "restore":
		return runRecordRestore(args[1:], stdout, stderr)
	case "merge":
		return runRecordMerge(args[1:], stdout, stderr)
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
	subtype := fs.String("subtype", "", "keep only records with this subtype")
	scope := fs.String("scope", "", "keep only records with this scope: project, personal or global")
	topic := fs.String("topic", "", "keep only records with this topic key")
	limit := fs.Int("limit", 0, "maximum records; 0 selects the default of 50")
	validity := addValidityFlags(fs)
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
	asOf, includeSuperseded, err := validity.parse()
	if err != nil {
		fmt.Fprintf(stderr, "brain record list: %v\n", err)
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
		ProjectID:         target.Project.ID,
		Kinds:             splitList(*kind),
		Since:             sinceTime,
		Status:            *status,
		Subtype:           *subtype,
		Scope:             *scope,
		TopicKey:          *topic,
		Limit:             *limit,
		AsOf:              asOf,
		IncludeSuperseded: includeSuperseded,
	})
	if err != nil {
		return reportError(stderr, "record list", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "record list", mapAll(records, toJSONRecordSummary))
	}
	for _, summary := range records {
		fmt.Fprintln(stdout, recordSummaryLine(summary))
	}
	return 0
}

func runRecordShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("record show", stderr)
	project := fs.String("project", "", "project id or alias")
	history := fs.Bool("history", false, "also print the supersede chain the record belongs to")
	asOfValue := fs.String("as-of", "", "mark whether the record was valid at this instant: RFC3339 time or date (2026-11-01)")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("record show", positional, 1, "<id>", stderr) {
		return 2
	}
	asOf, err := domain.ParseAsOf(*asOfValue)
	if err != nil {
		fmt.Fprintf(stderr, "brain record show: %v\n", err)
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
	summary := links.Record.Summary()
	var chain []app.RecordSummary
	if *history {
		chain, err = runtime.App.SupersedeChain(ctx, target.Project.ID, positional[0])
		if err != nil {
			return reportError(stderr, "record show", err)
		}
	}
	var validAt *bool
	if !asOf.IsZero() {
		allowed := links.Record.Validity().AllowsAt(asOf, summary.CreatedAt)
		validAt = &allowed
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "record show", jsonRecordShow{
			Kind:     detail.Kind,
			Record:   detail,
			Validity: toJSONSummaryValidity(summary),
			Classification: jsonClassification{
				Subtype: summary.Subtype, Scope: summary.Scope,
				TopicKey: summary.TopicKey, Occurrences: summary.Occurrences,
			},
			History: mapAll(chain, toJSONRecordSummary),
			ValidAt: validAt,
		})
	}
	printRecordDetail(stdout, detail)
	printRecordValidity(stdout, summary)
	printRecordRelations(stdout, detail.Relations)
	if validAt != nil {
		printRecordLine(stdout, "as_of", asOf.UTC().Format(time.RFC3339))
		printRecordLine(stdout, "valid_at", yesNo(*validAt))
	}
	printLinkSections(stdout, detail.Links, detail.Backlinks)
	if *history {
		fmt.Fprintln(stdout, "history:")
		for _, record := range chain {
			fmt.Fprintf(stdout, "  %s\n", recordSummaryLine(record))
		}
	}
	return 0
}

// recordEditInputLimit bounds what the CLI reads from a body file or stdin
// before the configured record limits are applied. It is deliberately larger
// than the default body cap so a user sees the limit error, not a truncated
// body.
const recordEditInputLimit = 4 << 20

// runRecordEdit changes the text of one live record through the same use case
// the local API uses. Without --dry-run the record file is rewritten in place;
// its ID, path, created_at, provenance, validity and relations stay the same.
func runRecordEdit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("record edit", stderr)
	project := fs.String("project", "", "project id or alias")
	var title string
	var titleSet bool
	fs.Func("title", "new title (an empty value clears a note title)", func(value string) error {
		title, titleSet = value, true
		return nil
	})
	bodyFile := fs.String("body-file", "", "read the new body from this file; - reads stdin")
	var tags []string
	var tagsSet bool
	fs.Func("tags", "replace note tags with a comma-separated list (an empty value clears them)", func(value string) error {
		tags, tagsSet = splitList(value), true
		return nil
	})
	expected := fs.String("expected-updated-at", "", "refuse the edit unless the record still has this RFC3339 updated_at")
	allowDuplicate := fs.Bool("allow-duplicate", false, "accept text another live record of the same kind already has")
	dryRun := fs.Bool("dry-run", false, "validate and print the result without writing")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("record edit", positional, 1, "<id> [--title T] [--body-file F|-] [--tags a,b]", stderr) {
		return 2
	}
	if !titleSet && *bodyFile == "" && !tagsSet {
		fmt.Fprintln(stderr, "brain record edit: nothing to change; pass --title, --body-file or --tags")
		return 2
	}
	var expectedAt time.Time
	if raw := strings.TrimSpace(*expected); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			fmt.Fprintf(stderr, "brain record edit: --expected-updated-at %q: want an RFC3339 timestamp\n", raw)
			return 2
		}
		expectedAt = parsed.UTC()
	}
	edit := app.RecordEdit{
		ID: positional[0], ExpectedUpdatedAt: expectedAt,
		AllowDuplicate: *allowDuplicate, DryRun: *dryRun,
	}
	if titleSet {
		edit.Title = &title
	}
	if *bodyFile != "" {
		body, err := readRecordEditBody(*bodyFile, stdin)
		if err != nil {
			fmt.Fprintf(stderr, "brain record edit: %v\n", err)
			return 2
		}
		edit.Body = &body
	}
	if tagsSet {
		edit.Tags = &tags
	}
	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, *project, nil, stderr, "record edit")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	edit.ProjectID = target.Project.ID
	result, err := runtime.App.EditRecord(ctx, edit)
	if err != nil {
		return reportError(stderr, "record edit", err)
	}
	summary := result.Record.Summary()
	if *asJSON {
		return writeJSONReport(stdout, stderr, "record edit", jsonRecordEdit{
			Kind: summary.Kind, ID: summary.ID, ProjectID: summary.ProjectID,
			Title: summary.Title, UpdatedAt: summary.UpdatedAt,
			Changed: result.Changed, DryRun: result.DryRun,
		})
	}
	switch {
	case result.DryRun:
		fmt.Fprintf(stdout, "would edit %s %s (dry run): %s\n", summary.Kind, summary.ID, summary.Title)
	case !result.Changed:
		fmt.Fprintf(stdout, "no change: %s %s already has this text\n", summary.Kind, summary.ID)
	default:
		fmt.Fprintf(stdout, "edited %s %s: %s\n", summary.Kind, summary.ID, summary.Title)
		fmt.Fprintf(stdout, "updated: %s\n", summary.UpdatedAt.UTC().Format(time.RFC3339Nano))
	}
	return 0
}

// readRecordEditBody reads a body file or stdin, bounded before the record
// limits apply. Trailing line breaks are dropped: Markdown bodies do not keep
// the newline a text file ends with.
func readRecordEditBody(name string, stdin io.Reader) (string, error) {
	var (
		reader io.Reader
		source string
	)
	if name == "-" {
		reader, source = stdin, "stdin"
	} else {
		file, err := os.Open(name)
		if err != nil {
			return "", fmt.Errorf("read --body-file %q: %w", name, err)
		}
		defer file.Close()
		reader, source = file, fmt.Sprintf("--body-file %q", name)
	}
	data, err := io.ReadAll(io.LimitReader(reader, recordEditInputLimit+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", source, err)
	}
	if len(data) > recordEditInputLimit {
		return "", fmt.Errorf("%s is larger than %d bytes", source, recordEditInputLimit)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
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
	if !decision.Changed {
		fmt.Fprintf(stdout, "%s %s: already superseded by %s\n", decision.Old.Kind, decision.Old.ID, newID)
		return 0
	}
	fmt.Fprintf(stdout, "%s %s: superseded by %s\n", decision.Old.Kind, decision.Old.ID, decision.New.ID)
	return 0
}

// runRecordReview clears the quarantine of one record or of every unreviewed
// record of the project, and optionally moves the record's review date.
func runRecordReview(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("record review", stderr)
	project := fs.String("project", "", "project id or alias")
	all := fs.Bool("all", false, "review every unreviewed record of the project")
	trust := fs.String("trust", "", "set the trust level: verified, stated or inferred")
	reviewAfter := fs.String("review-after", "", "set the next review date: a duration (90d, 30d) or a date (2027-02-05)")
	clearReviewAfter := fs.Bool("clear-review-after", false, "remove the review date")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if *all && len(positional) > 0 {
		fmt.Fprintln(stderr, "brain record review: pass either a record id or --all, not both")
		return 2
	}
	if !*all && !expectArgs("record review", positional, 1, "<id> [--trust verified|stated|inferred] [--review-after 90d]", stderr) {
		return 2
	}
	if *reviewAfter != "" && *clearReviewAfter {
		fmt.Fprintln(stderr, "brain record review: pass either --review-after or --clear-review-after, not both")
		return 2
	}
	if *all && (*reviewAfter != "" || *clearReviewAfter) {
		fmt.Fprintln(stderr, "brain record review: a review date applies to one record; pass its id instead of --all")
		return 2
	}
	trustValue := domain.Trust(strings.TrimSpace(*trust))
	if trustValue != "" && !trustValue.Valid() {
		fmt.Fprintf(stderr, "brain record review: unknown trust %q (verified, stated, inferred)\n", *trust)
		return 2
	}
	var reviewDate *time.Time
	if *reviewAfter != "" {
		parsed, err := domain.ParseReviewAfter(*reviewAfter, time.Now())
		if err != nil {
			fmt.Fprintf(stderr, "brain record review: %v\n", err)
			return 2
		}
		reviewDate = &parsed
	}
	id := ""
	if len(positional) == 1 {
		id = positional[0]
	}
	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, *project, nil, stderr, "record review")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	result, err := runtime.App.ReviewRecords(ctx, app.ReviewRecordOptions{
		ProjectID:        target.Project.ID,
		ID:               id,
		All:              *all,
		Trust:            trustValue,
		ReviewAfter:      reviewDate,
		ClearReviewAfter: *clearReviewAfter,
	})
	if err != nil {
		return reportError(stderr, "record review", err)
	}
	if len(result.Reviewed) == 0 {
		fmt.Fprintln(stdout, "no unreviewed records")
		return 0
	}
	for _, record := range result.Reviewed {
		fmt.Fprintf(stdout, "reviewed %s %s\n", record.Kind, record.ID)
	}
	if reviewDate != nil {
		fmt.Fprintf(stdout, "next review: %s\n", reviewDate.UTC().Format("2006-01-02"))
	}
	if *clearReviewAfter {
		fmt.Fprintln(stdout, "review date cleared")
	}
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

// recordSummaryLine is `<time>  <kind>  <id>  [<status>]  <title>`; the status
// column is shown for decisions only, and an ended record names its end.
func recordSummaryLine(summary app.RecordSummary) string {
	line := summary.CreatedAt.Local().Format(recordTimeLayout) + "  " + summary.Kind + "  " + summary.ID
	if summary.Status != "" {
		line += "  [" + summary.Status + "]"
	}
	if !summary.ValidTo.IsZero() {
		line += "  valid_to " + summary.ValidTo.UTC().Format(time.RFC3339)
	}
	return line + "  " + summary.Title
}

// printRecordValidity prints the temporal fields of a record.
func printRecordValidity(w io.Writer, summary app.RecordSummary) {
	if !summary.ValidFrom.IsZero() {
		printRecordLine(w, "valid_from", summary.ValidFrom.UTC().Format(time.RFC3339))
	}
	if summary.ValidTo.IsZero() {
		printRecordLine(w, "valid_to", "current")
	} else {
		printRecordLine(w, "valid_to", summary.ValidTo.UTC().Format(time.RFC3339))
	}
	printRecordLine(w, "superseded_by", summary.SupersededBy)
	printRecordLine(w, "supersedes", summary.Supersedes)
	if summary.Confidence != 0 {
		printRecordLine(w, "confidence", strconv.FormatFloat(summary.Confidence, 'f', -1, 64))
	}
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
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

// jsonRecordShow is `brain record show --json`: the record kind, its fields,
// its temporal validity and optionally the supersede chain. ValidAt is set only
// when --as-of was given.
type jsonRecordShow struct {
	Kind           string                 `json:"kind"`
	Record         mcpserver.RecordDetail `json:"record"`
	Validity       jsonValidity           `json:"validity"`
	Classification jsonClassification     `json:"classification"`
	History        []jsonRecordSummary    `json:"history,omitempty"`
	ValidAt        *bool                  `json:"valid_at,omitempty"`
}

// jsonClassification is the engram-compatible classification of a record.
type jsonClassification struct {
	Subtype     string `json:"subtype,omitempty"`
	Scope       string `json:"scope,omitempty"`
	TopicKey    string `json:"topic_key,omitempty"`
	Occurrences int    `json:"occurrences,omitempty"`
}

// jsonRecordSummary is one record of `brain record list --json` with its
// temporal validity.
type jsonRecordSummary struct {
	Kind        string       `json:"kind"`
	ID          string       `json:"id"`
	ProjectID   string       `json:"project_id"`
	Title       string       `json:"title"`
	Status      string       `json:"status,omitempty"`
	Subtype     string       `json:"subtype,omitempty"`
	Scope       string       `json:"scope,omitempty"`
	TopicKey    string       `json:"topic_key,omitempty"`
	Occurrences int          `json:"occurrences,omitempty"`
	CreatedAt   time.Time    `json:"created_at,omitzero"`
	UpdatedAt   time.Time    `json:"updated_at,omitzero"`
	Validity    jsonValidity `json:"validity"`
}

func toJSONRecordSummary(summary app.RecordSummary) jsonRecordSummary {
	return jsonRecordSummary{
		Kind:        summary.Kind,
		ID:          summary.ID,
		ProjectID:   summary.ProjectID,
		Title:       summary.Title,
		Status:      summary.Status,
		Subtype:     summary.Subtype,
		Scope:       summary.Scope,
		TopicKey:    summary.TopicKey,
		Occurrences: summary.Occurrences,
		CreatedAt:   summary.CreatedAt,
		UpdatedAt:   summary.UpdatedAt,
		Validity:    toJSONSummaryValidity(summary),
	}
}

// jsonRecordEdit is `brain record edit --json`.
type jsonRecordEdit struct {
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	Changed   bool      `json:"changed"`
	DryRun    bool      `json:"dry_run"`
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
		Relations: toMCPRelations(record.Relations()),
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
