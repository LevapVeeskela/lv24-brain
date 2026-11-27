package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ideasview"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// runIdea dispatches `brain idea <action>`. Ids are globally unique, so only
// commands that create or move an idea take a project selector; reads accept
// one to narrow the scope.
func runIdea(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printIdeaUsage(stderr)
		return 2
	}
	action, rest := args[0], args[1:]
	switch action {
	case "help", "-h", "--help":
		printIdeaUsage(stdout)
		return 0
	case "add":
		return runIdeaAdd(rest, stdin, stdout, stderr)
	case "list":
		return runIdeaList(rest, stdout, stderr)
	case "show":
		return runIdeaShow(rest, stdout, stderr)
	case "edit":
		return runIdeaEdit(rest, stdin, stdout, stderr)
	case "done":
		return runIdeaDone(rest, stdout, stderr)
	case "reopen":
		return runIdeaReopen(rest, stdout, stderr)
	case "drop":
		return runIdeaDrop(rest, stdout, stderr)
	case "rm":
		return runIdeaRemove(rest, stdout, stderr)
	case "restore":
		return runIdeaRestore(rest, stdout, stderr)
	case "brief":
		return runIdeaBrief(rest, stdout, stderr)
	case "mute":
		return runIdeaMute(rest, stdout, stderr)
	case "ical":
		return runIdeaICAL(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "brain idea: unknown action %q\n\n", action)
		printIdeaUsage(stderr)
		return 2
	}
}

func printIdeaUsage(w io.Writer) {
	fmt.Fprint(w, `Capture and manage ideas (agent-first TODO records).

Usage:
  brain idea <action> [flags]

Actions:
  add      Capture an idea
  list     List ideas (default: open and doing)
  show     Show one idea and its body
  edit     Edit an idea in place
  done     Mark an idea done
  reopen   Reopen a done or dropped idea
  drop     Drop an idea with a reason
  rm       Move an idea to trash
  restore  Restore a trashed idea
  brief    Render the capped session brief
  mute     Mute or unmute the session greeting
  ical     Export dated ideas as an iCalendar file

Run "brain idea <action> --help" for its flags.
`)
}

// ideaDueValue is the parsed --due/--at pair: a timed instant or an all-day
// local date, never both.
type ideaDueValue struct {
	At   *time.Time
	Date string
}

// parseIdeaDue parses --due and --at. today/tomorrow and YYYY-MM-DD are
// all-day local dates; RFC3339 is timed and keeps its offset. --at upgrades a
// date to a timed due in the machine's local zone.
func parseIdeaDue(dueValue, atValue string, now time.Time) (ideaDueValue, error) {
	due := strings.TrimSpace(dueValue)
	at := strings.TrimSpace(atValue)
	if due == "" {
		if at != "" {
			return ideaDueValue{}, fmt.Errorf("--at %q needs a date from --due (today, tomorrow or YYYY-MM-DD)", at)
		}
		return ideaDueValue{}, nil
	}
	switch due {
	case "today", "tomorrow":
		offset := 0
		if due == "tomorrow" {
			offset = 1
		}
		date := now.In(time.Local).AddDate(0, 0, offset).Format(domain.IdeaDateLayout)
		return finishIdeaDueDate(date, at)
	}
	if len(due) == len(domain.IdeaDateLayout) {
		if _, err := time.Parse(domain.IdeaDateLayout, due); err == nil {
			return finishIdeaDueDate(due, at)
		}
	}
	if at != "" {
		return ideaDueValue{}, fmt.Errorf("--at needs a date from --due (today, tomorrow or YYYY-MM-DD), not %q", due)
	}
	timed, err := time.Parse(time.RFC3339, due)
	if err != nil {
		return ideaDueValue{}, fmt.Errorf("--due %q: want today, tomorrow, YYYY-MM-DD or RFC3339", dueValue)
	}
	return ideaDueValue{At: &timed}, nil
}

// finishIdeaDueDate turns a local calendar date into an all-day due or, with
// --at HH:MM, a timed due in the local zone.
func finishIdeaDueDate(date, at string) (ideaDueValue, error) {
	if at == "" {
		return ideaDueValue{Date: date}, nil
	}
	clock, err := time.Parse("15:04", at)
	if err != nil {
		return ideaDueValue{}, fmt.Errorf("--at %q: want HH:MM", at)
	}
	parsed, err := time.ParseInLocation(domain.IdeaDateLayout, date, time.Local)
	if err != nil {
		return ideaDueValue{}, fmt.Errorf("--due date %q: %w", date, err)
	}
	timed := time.Date(parsed.Year(), parsed.Month(), parsed.Day(), clock.Hour(), clock.Minute(), 0, 0, time.Local)
	return ideaDueValue{At: &timed}, nil
}

// parseIdeaRemind parses --remind: 2h, 1d, 90m or plain minutes. The value is
// written as a concrete integer; 0 means no lead.
func parseIdeaRemind(value string) (*int, error) {
	raw := strings.ToLower(strings.TrimSpace(value))
	if raw == "" {
		return nil, nil
	}
	minutes := 0
	var err error
	switch {
	case strings.HasSuffix(raw, "d"):
		minutes, err = strconv.Atoi(strings.TrimSuffix(raw, "d"))
		minutes *= 24 * 60
	case strings.HasSuffix(raw, "h"):
		minutes, err = strconv.Atoi(strings.TrimSuffix(raw, "h"))
		minutes *= 60
	case strings.HasSuffix(raw, "m"):
		minutes, err = strconv.Atoi(strings.TrimSuffix(raw, "m"))
	default:
		minutes, err = strconv.Atoi(raw)
	}
	if err != nil {
		return nil, fmt.Errorf("--remind %q: want 2h, 1d or minutes", value)
	}
	if minutes < 0 || minutes > domain.MaxIdeaRemindBeforeMinutes {
		return nil, fmt.Errorf("--remind %q: must be between 0 and %d minutes", value, domain.MaxIdeaRemindBeforeMinutes)
	}
	return &minutes, nil
}

// ideaBacklogTags flattens repeated --tag flags and comma-separated values.
func ideaBacklogTags(values []string) []string {
	var tags []string
	for _, value := range values {
		tags = append(tags, splitList(value)...)
	}
	return tags
}

// parseIdeaSource parses --source TYPE[:ID] and the optional --harness NAME.
func parseIdeaSource(value, harness string) (*domain.IdeaSource, error) {
	sourceValue := strings.TrimSpace(value)
	harnessValue := strings.TrimSpace(harness)
	if sourceValue == "" {
		if harnessValue != "" {
			return nil, errors.New("--harness needs --source")
		}
		return nil, nil
	}
	source := &domain.IdeaSource{Harness: harnessValue}
	if kind, id, found := strings.Cut(sourceValue, ":"); found {
		source.Type, source.ID = strings.TrimSpace(kind), strings.TrimSpace(id)
	} else {
		source.Type = sourceValue
	}
	if err := source.Validate(); err != nil {
		return nil, err
	}
	return source, nil
}

// ideaProjectSelector validates --project. `all` is accepted only by reads that
// can span every project.
func ideaProjectSelector(value string, allowAll bool) (selector string, all bool, err error) {
	trimmed := strings.TrimSpace(value)
	switch trimmed {
	case "", "auto":
		return "", false, nil
	case "all":
		if !allowAll {
			return "", false, fmt.Errorf("--project all is not valid for this action")
		}
		return "", true, nil
	default:
		return trimmed, false, nil
	}
}

// ideaStatusSelection resolves --open/--done/--all and --status. --status wins
// over the shortcuts, as the contract says.
func ideaStatusSelection(open, done, all bool, statusFlag string) ([]domain.IdeaStatus, error) {
	if strings.TrimSpace(statusFlag) != "" {
		parts := splitList(statusFlag)
		if len(parts) == 0 {
			return nil, errors.New("--status needs at least one status")
		}
		statuses := make([]domain.IdeaStatus, 0, len(parts))
		for _, part := range parts {
			status, err := domain.ParseIdeaStatus(part)
			if err != nil {
				return nil, err
			}
			statuses = append(statuses, status)
		}
		return statuses, nil
	}
	shortcuts := 0
	var statuses []domain.IdeaStatus
	if open {
		shortcuts++
		statuses = []domain.IdeaStatus{domain.IdeaOpen, domain.IdeaDoing}
	}
	if done {
		shortcuts++
		statuses = []domain.IdeaStatus{domain.IdeaDone}
	}
	if all {
		shortcuts++
		statuses = []domain.IdeaStatus{domain.IdeaOpen, domain.IdeaDoing, domain.IdeaSomeday, domain.IdeaDone, domain.IdeaDropped}
	}
	if shortcuts > 1 {
		return nil, errors.New("--open, --done and --all are mutually exclusive")
	}
	if shortcuts == 0 {
		statuses = []domain.IdeaStatus{domain.IdeaOpen, domain.IdeaDoing}
	}
	return statuses, nil
}

// ideaListDueWindow maps --due on list to the half-open window of the contract.
func ideaListDueWindow(value string, now time.Time) (after, before time.Time, err error) {
	now = now.In(time.Local)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	switch strings.TrimSpace(value) {
	case "":
		return time.Time{}, time.Time{}, nil
	case "overdue":
		return time.Time{}, today, nil
	case "today":
		return today, today.AddDate(0, 0, 1), nil
	case "tomorrow":
		start := today.AddDate(0, 0, 1)
		return start, start.AddDate(0, 0, 1), nil
	case "week":
		return today, today.AddDate(0, 0, 7), nil
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("--due %q: want today, tomorrow, week or overdue", value)
	}
}

// newIdeaApp loads the brain and returns the app plus its configuration. The
// returned close function releases the store.
func newIdeaApp(cfgPath string, stderr io.Writer, command string) (config.BrainConfig, *app.App, func(), int) {
	cfg, err := loadRequiredConfig(cfgPath)
	if err != nil {
		return config.BrainConfig{}, nil, nil, reportError(stderr, command, err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		return config.BrainConfig{}, nil, nil, reportError(stderr, command, err)
	}
	return cfg, runtime.App, func() { _ = runtime.Close() }, 0
}

// reportIdeaError prints a problem file as path + reason, as the contract asks,
// and every other failure through the usual reporter.
func reportIdeaError(stderr io.Writer, command, dataDir string, err error) int {
	var problem *domain.ProblemError
	if errors.As(err, &problem) {
		fmt.Fprintf(stderr, "brain %s: problem at %s: %s\n", command, ideaRelativePath(dataDir, problem.Path), problem.Reason)
		return 1
	}
	return reportError(stderr, command, err)
}

// ideaRelativePath renders problem paths through the shared view so the CLI
// and the local API name the same file.
func ideaRelativePath(dataDir, path string) string {
	return ideasview.RelativePath(dataDir, path)
}

// ideaRowPath is the stable storage path of an idea, independent of the data
// directory.
func ideaRowPath(idea domain.Idea) string {
	return ideasview.StoragePath(idea)
}

// ---------------------------------------------------------------------------
// JSON rows (§9.3)
//
// The row, problem row and list/brief envelopes are shared with the local API
// so the two surfaces cannot drift. These aliases keep the CLI's local names
// and the jsonIdeaWrite envelope (which has always emitted project_source)
// unchanged.

type jsonIdeaRow = ideasview.Row
type jsonIdeaProblem = ideasview.Problem
type jsonIdeaList = ideasview.List
type jsonIdeaShow = ideasview.Show
type jsonIdeaRemoved = ideasview.Trash
type jsonIdeaBrief = ideasview.Brief

type jsonIdeaWrite struct {
	Idea          jsonIdeaRow `json:"idea"`
	ProjectSource string      `json:"project_source,omitempty"`
}

type jsonIdeaMute struct {
	MutedUntil *string `json:"muted_until"`
}

// toJSONIdeaRow maps one idea to the shared snake_case row of the contract.
// The body is included only where the contract includes it (show and writes).
func toJSONIdeaRow(idea domain.Idea, withBody bool) jsonIdeaRow {
	return ideasview.RowOf(idea, withBody)
}

// toJSONIdeaProblems renders problem rows with data-dir-relative paths when the
// file is inside the brain.
func toJSONIdeaProblems(dataDir string, problems []ports.RecordProblem) []jsonIdeaProblem {
	return ideasview.Problems(dataDir, problems)
}

// ---------------------------------------------------------------------------
// Human output (§9.2)

// printIdeaRows renders a list section with aligned due, title and project
// chips. The order is the store's deterministic order.
func printIdeaRows(w io.Writer, ideas []domain.Idea, now time.Time) {
	type row struct {
		marker  string
		due     string
		title   string
		project string
		remind  string
	}
	rows := make([]row, 0, len(ideas))
	widths := [4]int{}
	for _, idea := range ideas {
		title := idea.Title
		if idea.Status.Terminal() {
			title = "~~" + title + "~~"
		}
		due, _ := app.IdeaDueLabel(idea, now)
		rows = append(rows, row{
			marker:  app.IdeaMarker(idea, now),
			due:     due,
			title:   title,
			project: idea.ProjectID,
			remind:  ideaRemindChip(idea),
		})
		if len(due) > widths[0] {
			widths[0] = len(due)
		}
		if len([]rune(title)) > widths[1] {
			widths[1] = len([]rune(title))
		}
		if len(idea.ProjectID) > widths[2] {
			widths[2] = len(idea.ProjectID)
		}
		if len(rows[len(rows)-1].remind) > widths[3] {
			widths[3] = len(rows[len(rows)-1].remind)
		}
	}
	for _, item := range rows {
		line := "  " + item.marker
		if widths[0] > 0 {
			line += " " + padRight(item.due, widths[0]) + " "
		} else {
			line += " "
		}
		line += padRight(item.title, widths[1])
		if widths[2] > 0 {
			line += "  " + padRight(item.project, widths[2])
		}
		if item.remind != "" {
			line += "  " + item.remind
		}
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
}

// padRight pads a cell with spaces. Titles are measured in runes, so a wide
// Unicode title still lines up.
func padRight(value string, width int) string {
	padding := width - len([]rune(value))
	if padding <= 0 {
		return value
	}
	return value + strings.Repeat(" ", padding)
}

// ideaRemindChip renders the lead reminder as the contract's 🔔2h chip.
func ideaRemindChip(idea domain.Idea) string {
	if idea.RemindBeforeMinutes == nil || *idea.RemindBeforeMinutes <= 0 {
		return ""
	}
	return "🔔" + ideaDurationLabel(*idea.RemindBeforeMinutes)
}

// ideaDurationLabel renders minutes as 2h, 1d or 30m.
func ideaDurationLabel(minutes int) string {
	switch {
	case minutes%1440 == 0:
		return strconv.Itoa(minutes/1440) + "d"
	case minutes%60 == 0:
		return strconv.Itoa(minutes/60) + "h"
	default:
		return strconv.Itoa(minutes) + "m"
	}
}

// printIdeaDetail prints the metadata block, source, completion note and body
// of one idea.
func printIdeaDetail(w io.Writer, idea domain.Idea) {
	status := idea.Status
	if status == "" {
		status = domain.IdeaOpen
	}
	priority := idea.Priority
	if priority == "" {
		priority = domain.IdeaPriorityNormal
	}
	fmt.Fprintf(w, "idea %s\n", idea.ID)
	fmt.Fprintf(w, "title: %s\n", idea.Title)
	fmt.Fprintf(w, "project: %s\n", idea.ProjectID)
	fmt.Fprintf(w, "status: %s\n", status)
	fmt.Fprintf(w, "priority: %s\n", priority)
	if idea.DueAt != nil && !idea.DueAt.IsZero() {
		fmt.Fprintf(w, "due: %s\n", idea.DueAt.In(time.Local).Format(time.RFC3339))
	} else if idea.DueDate != "" {
		fmt.Fprintf(w, "due: %s (all-day)\n", idea.DueDate)
	}
	if idea.RemindBeforeMinutes != nil && *idea.RemindBeforeMinutes > 0 {
		fmt.Fprintf(w, "remind: %s before\n", ideaDurationLabel(*idea.RemindBeforeMinutes))
	}
	if len(idea.Tags) > 0 {
		fmt.Fprintf(w, "tags: %s\n", strings.Join(idea.Tags, ", "))
	}
	fmt.Fprintf(w, "created: %s\n", idea.CreatedAt.In(time.Local).Format(time.RFC3339))
	fmt.Fprintf(w, "updated: %s\n", idea.UpdatedAt.In(time.Local).Format(time.RFC3339))
	if idea.CompletedAt != nil && !idea.CompletedAt.IsZero() {
		fmt.Fprintf(w, "completed: %s\n", idea.CompletedAt.In(time.Local).Format(time.RFC3339))
	}
	if idea.Source != nil {
		source := idea.Source.Type
		if idea.Source.ID != "" {
			source += " " + idea.Source.ID
		}
		if idea.Source.Harness != "" {
			source += " (" + idea.Source.Harness + ")"
		}
		fmt.Fprintf(w, "source: %s\n", source)
	}
	if idea.CompletionNote != "" {
		fmt.Fprintf(w, "note: %s\n", idea.CompletionNote)
	}
	fmt.Fprintf(w, "path: %s\n", ideaRowPath(idea))
	if strings.TrimSpace(idea.Body) != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, strings.TrimRight(idea.Body, "\n"))
	}
}

// printIdeaProblems renders the PROBLEMS section of `brain idea list`.
func printIdeaProblems(w io.Writer, dataDir string, problems []ports.RecordProblem) {
	fmt.Fprintf(w, "PROBLEMS (%d)\n", len(problems))
	for _, problem := range problems {
		fmt.Fprintf(w, "  %s: %s\n", ideaRelativePath(dataDir, problem.Path), problem.Reason)
	}
}

// ---------------------------------------------------------------------------
// Actions

func runIdeaAdd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("idea add", stderr)
	project := fs.String("project", "", "project id, alias, checkout path or auto")
	bodyFile := fs.String("body-file", "", "read the body from this file; - reads stdin")
	due := fs.String("due", "", "today, tomorrow, YYYY-MM-DD or RFC3339")
	at := fs.String("at", "", "time of day (HH:MM) for --due today/tomorrow/YYYY-MM-DD")
	remind := fs.String("remind", "", "reminder lead: 2h, 1d or minutes")
	priority := fs.String("priority", "", "low, normal or high")
	var tagFlags repeatedFlag
	fs.Var(&tagFlags, "tag", "tag (repeatable or comma-separated)")
	source := fs.String("source", "", "source as TYPE:ID (session, note, decision, commit, manual, desktop)")
	harness := fs.String("harness", "", "source harness, e.g. opencode; needs --source")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) == 0 {
		fmt.Fprintln(stderr, "brain idea add: expected <title>")
		return 2
	}
	title := strings.Join(positional, " ")
	selector, _, err := ideaProjectSelector(*project, false)
	if err != nil {
		fmt.Fprintf(stderr, "brain idea add: %v\n", err)
		return 2
	}
	dueValue, err := parseIdeaDue(*due, *at, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "brain idea add: %v\n", err)
		return 2
	}
	remindValue, err := parseIdeaRemind(*remind)
	if err != nil {
		fmt.Fprintf(stderr, "brain idea add: %v\n", err)
		return 2
	}
	priorityValue := domain.IdeaPriority(strings.TrimSpace(*priority))
	if priorityValue != "" {
		parsed, err := domain.ParseIdeaPriority(string(priorityValue))
		if err != nil {
			fmt.Fprintf(stderr, "brain idea add: %v\n", err)
			return 2
		}
		priorityValue = parsed
	}
	sourceValue, err := parseIdeaSource(*source, *harness)
	if err != nil {
		fmt.Fprintf(stderr, "brain idea add: %v\n", err)
		return 2
	}
	body := ""
	if *bodyFile != "" {
		body, err = readRecordEditBody(*bodyFile, stdin)
		if err != nil {
			fmt.Fprintf(stderr, "brain idea add: %v\n", err)
			return 2
		}
	}
	cfg, application, closeApp, code := newIdeaApp(*cfgPath, stderr, "idea add")
	if code != 0 {
		return code
	}
	defer closeApp()
	result, err := application.AddIdea(context.Background(), app.IdeaInput{
		ProjectID:           selector,
		Title:               title,
		Body:                body,
		Priority:            priorityValue,
		DueAt:               dueValue.At,
		DueDate:             dueValue.Date,
		RemindBeforeMinutes: remindValue,
		Tags:                ideaBacklogTags(tagFlags),
		Source:              sourceValue,
	})
	if err != nil {
		return reportIdeaError(stderr, "idea add", cfg.DataDir, err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "idea add", jsonIdeaWrite{
			Idea:          toJSONIdeaRow(result.Idea, true),
			ProjectSource: result.ProjectSource,
		})
	}
	fmt.Fprintf(stdout, "idea %s\n", result.Idea.ID)
	return 0
}

func runIdeaList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("idea list", stderr)
	open := fs.Bool("open", false, "open and doing ideas (the default)")
	done := fs.Bool("done", false, "done ideas only")
	allStatuses := fs.Bool("all", false, "every status; with no --project, also every project")
	var statusFlags repeatedFlag
	fs.Var(&statusFlags, "status", "comma-separated statuses: open, doing, someday, done, dropped (repeatable)")
	project := fs.String("project", "", "project id, alias, checkout path, auto or all")
	due := fs.String("due", "", "today, tomorrow, week or overdue")
	var tagFlags repeatedFlag
	fs.Var(&tagFlags, "tag", "exact tag filter (repeatable or comma-separated, AND)")
	search := fs.String("search", "", "case-insensitive substring of title, body or tags")
	limit := fs.Int("limit", 100, "maximum rows; 0 means no cap")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "brain idea list: unexpected arguments: %s\n", strings.Join(positional, " "))
		return 2
	}
	if *limit < 0 || *limit > 1000 {
		fmt.Fprintln(stderr, "brain idea list: --limit must be between 0 and 1000")
		return 2
	}
	statuses, err := ideaStatusSelection(*open, *done, *allStatuses, strings.Join(statusFlags, ","))
	if err != nil {
		fmt.Fprintf(stderr, "brain idea list: %v\n", err)
		return 2
	}
	selector, all, err := ideaProjectSelector(*project, true)
	if err != nil {
		fmt.Fprintf(stderr, "brain idea list: %v\n", err)
		return 2
	}
	// --all is the status shortcut, but without an explicit project it also
	// widens the scope to every project, exactly like `brief --scope all`. The
	// default list still resolves cwd -> personal, so a typo is never silently
	// turned into a global scan; an explicit --project (including auto) wins.
	if *allStatuses && strings.TrimSpace(*project) == "" {
		all = true
	}
	now := time.Now()
	after, before, err := ideaListDueWindow(*due, now)
	if err != nil {
		fmt.Fprintf(stderr, "brain idea list: %v\n", err)
		return 2
	}
	cfg, application, closeApp, code := newIdeaApp(*cfgPath, stderr, "idea list")
	if code != 0 {
		return code
	}
	defer closeApp()
	result, err := application.ListIdeas(context.Background(), app.IdeaListOptions{
		ProjectID: selector,
		All:       all,
		Statuses:  statuses,
		Tags:      ideaBacklogTags(tagFlags),
		DueAfter:  after,
		DueBefore: before,
		Query:     *search,
		Limit:     0,
	})
	if err != nil {
		return reportIdeaError(stderr, "idea list", cfg.DataDir, err)
	}
	total := len(result.Ideas)
	ideas := result.Ideas
	if *limit > 0 && len(ideas) > *limit {
		ideas = ideas[:*limit]
	}
	if *asJSON {
		rows := make([]jsonIdeaRow, 0, len(ideas))
		for _, idea := range ideas {
			rows = append(rows, toJSONIdeaRow(idea, false))
		}
		return writeJSONReport(stdout, stderr, "idea list", jsonIdeaList{
			Ideas:         rows,
			Problems:      toJSONIdeaProblems(cfg.DataDir, result.Problems),
			Total:         total,
			ProjectSource: result.ProjectSource,
		})
	}
	printIdeaList(stdout, ideas, result.Problems, cfg.DataDir, now)
	return 0
}

// printIdeaList renders the OPEN, DONE and DROPPED sections plus the problem
// rows. Dropped ideas stay visible but never hide under the DONE heading.
func printIdeaList(w io.Writer, ideas []domain.Idea, problems []ports.RecordProblem, dataDir string, now time.Time) {
	if len(ideas) == 0 && len(problems) == 0 {
		fmt.Fprintln(w, "no ideas")
		return
	}
	var active, doneIdeas, droppedIdeas []domain.Idea
	for _, idea := range ideas {
		switch idea.Status {
		case domain.IdeaDone:
			doneIdeas = append(doneIdeas, idea)
		case domain.IdeaDropped:
			droppedIdeas = append(droppedIdeas, idea)
		default:
			active = append(active, idea)
		}
	}
	printed := false
	printSection := func(label string, section []domain.Idea) {
		if len(section) == 0 {
			return
		}
		if printed {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s (%d)\n", label, len(section))
		printIdeaRows(w, section, now)
		printed = true
	}
	printSection("OPEN", active)
	printSection("DONE", doneIdeas)
	printSection("DROPPED", droppedIdeas)
	if len(problems) > 0 {
		if printed {
			fmt.Fprintln(w)
		}
		printIdeaProblems(w, dataDir, problems)
	}
}

func runIdeaShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("idea show", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("idea show", positional, 1, "<id>", stderr) {
		return 2
	}
	cfg, application, closeApp, code := newIdeaApp(*cfgPath, stderr, "idea show")
	if code != 0 {
		return code
	}
	defer closeApp()
	idea, err := application.GetIdea(context.Background(), positional[0])
	if err != nil {
		return reportIdeaError(stderr, "idea show", cfg.DataDir, err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "idea show", jsonIdeaShow{Idea: toJSONIdeaRow(idea, true)})
	}
	printIdeaDetail(stdout, idea)
	return 0
}

func runIdeaEdit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("idea edit", stderr)
	var title string
	titleSet := false
	fs.Func("title", "new title", func(value string) error {
		title, titleSet = value, true
		return nil
	})
	bodyFile := fs.String("body-file", "", "read the new body from this file; - reads stdin")
	project := fs.String("project", "", "move the idea to this project id, alias or checkout path")
	due := fs.String("due", "", "today, tomorrow, YYYY-MM-DD or RFC3339")
	at := fs.String("at", "", "time of day (HH:MM) for --due today/tomorrow/YYYY-MM-DD")
	clearDue := fs.Bool("clear-due", false, "remove both due_at and due_date")
	remind := fs.String("remind", "", "reminder lead: 2h, 1d or minutes")
	clearRemind := fs.Bool("clear-remind", false, "remove the reminder lead")
	status := fs.String("status", "", "open, doing, someday, done or dropped")
	priority := fs.String("priority", "", "low, normal or high")
	tagsValue := ""
	tagsSet := false
	fs.Func("tags", "replace tags with a comma-separated list", func(value string) error {
		tagsValue, tagsSet = value, true
		return nil
	})
	clearTags := fs.Bool("clear-tags", false, "remove every tag")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("idea edit", positional, 1, "<id> [flags]", stderr) {
		return 2
	}
	selector, _, err := ideaProjectSelector(*project, false)
	if err != nil {
		fmt.Fprintf(stderr, "brain idea edit: %v\n", err)
		return 2
	}
	if *clearDue && (*due != "" || *at != "") {
		fmt.Fprintln(stderr, "brain idea edit: --clear-due cannot be combined with --due or --at")
		return 2
	}
	if *clearRemind && *remind != "" {
		fmt.Fprintln(stderr, "brain idea edit: --clear-remind cannot be combined with --remind")
		return 2
	}
	if *clearTags && tagsSet {
		fmt.Fprintln(stderr, "brain idea edit: --clear-tags cannot be combined with --tags")
		return 2
	}
	change := titleSet || *bodyFile != "" || selector != "" || *due != "" || *at != "" ||
		*clearDue || *remind != "" || *clearRemind || *status != "" || *priority != "" || tagsSet || *clearTags
	if !change {
		fmt.Fprintln(stderr, "brain idea edit: nothing to change; pass --title, --body-file, --project, --due, --status, --priority or --tags")
		return 2
	}
	update := app.IdeaUpdateInput{ID: positional[0]}
	if titleSet {
		update.Title = &title
	}
	if *bodyFile != "" {
		body, err := readRecordEditBody(*bodyFile, stdin)
		if err != nil {
			fmt.Fprintf(stderr, "brain idea edit: %v\n", err)
			return 2
		}
		update.Body = &body
	}
	if selector != "" {
		update.ProjectID = selector
	}
	if *clearDue {
		update.ClearDue = true
	} else if *due != "" || *at != "" {
		dueValue, err := parseIdeaDue(*due, *at, time.Now())
		if err != nil {
			fmt.Fprintf(stderr, "brain idea edit: %v\n", err)
			return 2
		}
		if dueValue.At != nil {
			update.DueAt = dueValue.At
		} else if dueValue.Date != "" {
			update.DueDate = &dueValue.Date
		}
	}
	if *clearRemind {
		update.ClearReminder = true
	} else if *remind != "" {
		remindValue, err := parseIdeaRemind(*remind)
		if err != nil {
			fmt.Fprintf(stderr, "brain idea edit: %v\n", err)
			return 2
		}
		update.RemindBeforeMinutes = remindValue
	}
	if *status != "" {
		statusValue, err := domain.ParseIdeaStatus(*status)
		if err != nil {
			fmt.Fprintf(stderr, "brain idea edit: %v\n", err)
			return 2
		}
		update.Status = &statusValue
	}
	if *priority != "" {
		priorityValue, err := domain.ParseIdeaPriority(*priority)
		if err != nil {
			fmt.Fprintf(stderr, "brain idea edit: %v\n", err)
			return 2
		}
		update.Priority = &priorityValue
	}
	if *clearTags {
		update.ClearTags = true
	} else if tagsSet {
		tags := splitList(tagsValue)
		update.Tags = &tags
	}
	cfg, application, closeApp, code := newIdeaApp(*cfgPath, stderr, "idea edit")
	if code != 0 {
		return code
	}
	defer closeApp()
	result, err := application.UpdateIdea(context.Background(), update)
	if err != nil {
		return reportIdeaError(stderr, "idea edit", cfg.DataDir, err)
	}
	if *asJSON {
		projectSource := result.ProjectSource
		if projectSource == "" && selector != "" {
			projectSource = app.IdeaProjectExplicit
		}
		return writeJSONReport(stdout, stderr, "idea edit", jsonIdeaWrite{
			Idea:          toJSONIdeaRow(result.Idea, true),
			ProjectSource: projectSource,
		})
	}
	fmt.Fprintf(stdout, "idea %s\n", result.Idea.ID)
	return 0
}

// ideaSetStatus is the shared done/reopen/drop write. An empty noteFlag skips
// the note flag entirely (reopen has none).
func ideaSetStatus(args []string, command string, status domain.IdeaStatus, noteFlag, noteHelp string, stdout, stderr io.Writer) int {
	fs := newFlagSet("idea "+command, stderr)
	var note *string
	if noteFlag != "" {
		note = fs.String(noteFlag, "", noteHelp)
	}
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("idea "+command, positional, 1, "<id>", stderr) {
		return 2
	}
	update := app.IdeaUpdateInput{ID: positional[0], Status: &status}
	if note != nil && *note != "" {
		update.CompletionNote = note
	}
	cfg, application, closeApp, code := newIdeaApp(*cfgPath, stderr, "idea "+command)
	if code != 0 {
		return code
	}
	defer closeApp()
	result, err := application.UpdateIdea(context.Background(), update)
	if err != nil {
		return reportIdeaError(stderr, "idea "+command, cfg.DataDir, err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "idea "+command, jsonIdeaWrite{
			Idea: toJSONIdeaRow(result.Idea, true),
		})
	}
	fmt.Fprintf(stdout, "%s idea %s\n", ideaStatusVerb(command), result.Idea.ID)
	return 0
}

// ideaStatusVerb is the human past tense of a status command.
func ideaStatusVerb(command string) string {
	switch command {
	case "reopen":
		return "reopened"
	case "drop":
		return "dropped"
	default:
		return command
	}
}

func runIdeaDone(args []string, stdout, stderr io.Writer) int {
	return ideaSetStatus(args, "done", domain.IdeaDone, "note", "completion note", stdout, stderr)
}

func runIdeaReopen(args []string, stdout, stderr io.Writer) int {
	return ideaSetStatus(args, "reopen", domain.IdeaOpen, "", "", stdout, stderr)
}

func runIdeaDrop(args []string, stdout, stderr io.Writer) int {
	return ideaSetStatus(args, "drop", domain.IdeaDropped, "reason", "reason for dropping", stdout, stderr)
}

func runIdeaRemove(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("idea rm", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("idea rm", positional, 1, "<id>", stderr) {
		return 2
	}
	cfg, application, closeApp, code := newIdeaApp(*cfgPath, stderr, "idea rm")
	if code != 0 {
		return code
	}
	defer closeApp()
	idea, err := application.TrashIdea(context.Background(), "", positional[0])
	if err != nil {
		return reportIdeaError(stderr, "idea rm", cfg.DataDir, err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "idea rm", jsonIdeaRemoved{ID: idea.ID, Trashed: true})
	}
	fmt.Fprintf(stdout, "trashed idea %s (brain idea restore %s)\n", idea.ID, idea.ID)
	return 0
}

func runIdeaRestore(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("idea restore", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("idea restore", positional, 1, "<id>", stderr) {
		return 2
	}
	cfg, application, closeApp, code := newIdeaApp(*cfgPath, stderr, "idea restore")
	if code != 0 {
		return code
	}
	defer closeApp()
	idea, err := application.RestoreIdea(context.Background(), "", positional[0])
	if err != nil {
		return reportIdeaError(stderr, "idea restore", cfg.DataDir, err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "idea restore", jsonIdeaWrite{
			Idea: toJSONIdeaRow(idea, true),
		})
	}
	fmt.Fprintf(stdout, "restored idea %s\n", idea.ID)
	return 0
}

func runIdeaBrief(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("idea brief", stderr)
	project := fs.String("project", "", "project id, alias, checkout path, auto or all")
	max := fs.Int("max", app.DefaultIdeaBriefMax, "maximum items (1..10)")
	scope := fs.String("scope", "", "current or all (default all)")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "brain idea brief: unexpected arguments: %s\n", strings.Join(positional, " "))
		return 2
	}
	if *max < 1 || *max > app.MaxIdeaBriefMax {
		fmt.Fprintf(stderr, "brain idea brief: --max must be between 1 and %d\n", app.MaxIdeaBriefMax)
		return 2
	}
	scopeValue := strings.TrimSpace(*scope)
	switch scopeValue {
	case "":
		scopeValue = app.IdeaBriefScopeAll
	case app.IdeaBriefScopeAll, app.IdeaBriefScopeCurrent:
	default:
		fmt.Fprintf(stderr, "brain idea brief: --scope %q: want current or all\n", *scope)
		return 2
	}
	selector, all, err := ideaProjectSelector(*project, true)
	if err != nil {
		fmt.Fprintf(stderr, "brain idea brief: %v\n", err)
		return 2
	}
	if all {
		if scopeValue == app.IdeaBriefScopeCurrent {
			fmt.Fprintln(stderr, "brain idea brief: --project all contradicts --scope current")
			return 2
		}
		scopeValue = app.IdeaBriefScopeAll
	}
	cfg, application, closeApp, code := newIdeaApp(*cfgPath, stderr, "idea brief")
	if code != 0 {
		return code
	}
	defer closeApp()
	result, err := application.IdeaBrief(context.Background(), app.IdeaBriefOptions{
		ProjectID: selector,
		Scope:     scopeValue,
		Max:       *max,
	})
	if err != nil {
		return reportIdeaError(stderr, "idea brief", cfg.DataDir, err)
	}
	if *asJSON {
		rows := make([]jsonIdeaRow, 0, len(result.Ideas))
		for _, idea := range result.Ideas {
			rows = append(rows, toJSONIdeaRow(idea, false))
		}
		return writeJSONReport(stdout, stderr, "idea brief", jsonIdeaBrief{
			Text:     result.Text,
			Ideas:    rows,
			Open:     result.Open,
			DueToday: result.DueToday,
			Muted:    result.Muted,
		})
	}
	fmt.Fprintln(stdout, result.Text)
	return 0
}

func runIdeaMute(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("idea mute", stderr)
	days := 7
	daysSet := false
	fs.Func("days", "mute for this many days (1..365, default 7)", func(value string) error {
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("want a number of days")
		}
		days, daysSet = parsed, true
		return nil
	})
	off := fs.Bool("off", false, "clear the mute")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "brain idea mute: unexpected arguments: %s\n", strings.Join(positional, " "))
		return 2
	}
	if *off && daysSet {
		fmt.Fprintln(stderr, "brain idea mute: --off cannot be combined with --days")
		return 2
	}
	if !*off && (days < 1 || days > 365) {
		fmt.Fprintln(stderr, "brain idea mute: --days must be between 1 and 365")
		return 2
	}
	cfg, application, closeApp, code := newIdeaApp(*cfgPath, stderr, "idea mute")
	if code != 0 {
		return code
	}
	defer closeApp()
	var until *time.Time
	if !*off {
		deadline := time.Now().AddDate(0, 0, days)
		until = &deadline
	}
	mutedUntil, err := application.SetIdeaGreetingMute(until)
	if err != nil {
		return reportIdeaError(stderr, "idea mute", cfg.DataDir, err)
	}
	if *asJSON {
		var rendered *string
		if !mutedUntil.IsZero() {
			formatted := mutedUntil.UTC().Format(time.RFC3339)
			rendered = &formatted
		}
		return writeJSONReport(stdout, stderr, "idea mute", jsonIdeaMute{MutedUntil: rendered})
	}
	if mutedUntil.IsZero() {
		fmt.Fprintln(stdout, "ideas: greeting unmuted")
		return 0
	}
	fmt.Fprintf(stdout, "ideas: greeting muted until %s\n", mutedUntil.UTC().Format(time.RFC3339))
	return 0
}

func runIdeaICAL(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("idea ical", stderr)
	project := fs.String("project", "", "project id, alias, checkout path, auto or all (default: every project)")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "brain idea ical: unexpected arguments: %s\n", strings.Join(positional, " "))
		return 2
	}
	selector, all, err := ideaProjectSelector(*project, true)
	if err != nil {
		fmt.Fprintf(stderr, "brain idea ical: %v\n", err)
		return 2
	}
	// A file export is global by default, like the local API's /v1/ideas/ical.
	// `--project auto` is the explicit opt-in to cwd -> personal resolution.
	if strings.TrimSpace(*project) == "" {
		all = true
	}
	cfg, application, closeApp, code := newIdeaApp(*cfgPath, stderr, "idea ical")
	if code != 0 {
		return code
	}
	defer closeApp()
	result, err := application.ListIdeas(context.Background(), app.IdeaListOptions{
		ProjectID: selector,
		All:       all,
		Statuses:  []domain.IdeaStatus{domain.IdeaOpen, domain.IdeaDoing, domain.IdeaSomeday},
		Limit:     0,
	})
	if err != nil {
		return reportIdeaError(stderr, "idea ical", cfg.DataDir, err)
	}
	fmt.Fprint(stdout, app.RenderIdeaCalendar(result.Ideas, time.Now()))
	return 0
}
