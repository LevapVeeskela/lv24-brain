package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ideasview"
)

// Idea action vocabulary and limits from the 0.28 contract (§10).
const (
	ideaDefaultListLimit = 50
	ideaMaxListLimit     = 200
)

var (
	ideaActions = []string{
		"add", "list", "get", "update", "done", "reopen", "drop", "rm", "restore", "brief",
	}
	// ideaReadActions are the actions a read-only server keeps.
	ideaReadActions = []string{"list", "get", "brief"}
	// ideaWriteActions are the actions a read-only server refuses.
	ideaWriteActions  = []string{"add", "update", "done", "reopen", "drop", "rm", "restore"}
	ideaStatusNames   = []string{"open", "doing", "done", "dropped", "someday"}
	ideaPriorityNames = []string{
		string(domain.IdeaPriorityLow), string(domain.IdeaPriorityNormal), string(domain.IdeaPriorityHigh),
	}
	ideaClearNames  = []string{"due", "reminder", "tags"}
	ideaScopes      = []string{app.IdeaBriefScopeCurrent, app.IdeaBriefScopeAll}
	ideaSourceTypes = []string{"session", "note", "decision", "commit", "manual", "desktop"}
)

// ideaSourceProp is the nested source object of add.
func ideaSourceProp() map[string]any {
	return map[string]any{
		"type":        "object",
		"description": "Where the idea came from (add); optional, but type is required when the object is present.",
		"properties": map[string]any{
			"type":    enumProp("Source type; session/note/decision/commit need an id.", ideaSourceTypes),
			"id":      stringProp("Session id, note id, decision id or commit hash (at most 256 bytes)."),
			"harness": stringProp("Agent harness, for example opencode (at most 64 bytes)."),
		},
		"required":             []string{"type"},
		"additionalProperties": false,
	}
}

// IdeaAddInput is one brain_idea(action=add) request after argument parsing.
type IdeaAddInput struct {
	ProjectID           string
	Title               string
	Body                string
	Priority            string
	DueAt               *time.Time
	DueDate             string
	RemindBeforeMinutes *int
	Tags                []string
	Source              *domain.IdeaSource
}

// IdeaUpdateInput is one brain_idea mutation of an existing idea. Nil means
// "unchanged"; the Clear flags are the only way to remove a value.
type IdeaUpdateInput struct {
	ID                  string
	ProjectID           string
	Title               *string
	Body                *string
	Status              *string
	Priority            *string
	DueAt               *time.Time
	DueDate             *string
	RemindBeforeMinutes *int
	Tags                *[]string
	ClearDue            bool
	ClearReminder       bool
	ClearTags           bool
	// Note is the completion note of done and the reason of drop.
	Note string
}

// IdeaListQuery narrows brain_idea(action=list).
type IdeaListQuery struct {
	ProjectID string
	All       bool
	Statuses  []string
	Tags      []string
	DueBefore string
	DueAfter  string
	Query     string
	Limit     int
}

// IdeaBriefQuery selects the session-form brief.
type IdeaBriefQuery struct {
	ProjectID string
	Scope     string
	Max       int
}

// IdeaWrite is the result of a mutating action.
type IdeaWrite struct {
	Idea          domain.Idea
	ProjectSource string
}

// IdeaProblem is one idea file the store skipped.
type IdeaProblem = ideasview.Problem

// IdeaList is a filtered listing; Ideas is truncated to the requested limit and
// Total counts the matches before it.
type IdeaList struct {
	Ideas         []domain.Idea
	Problems      []IdeaProblem
	Total         int
	ProjectSource string
}

// IdeaBrief is the shared session block with the selected items and counts.
type IdeaBrief struct {
	Text     string
	Ideas    []domain.Idea
	Open     int
	DueToday int
	Muted    bool
}

// IdeaBackend is implemented by backends that serve the idea layer. It is
// separate from Backend so a server without the idea store keeps the rest of
// the surface intact.
type IdeaBackend interface {
	AddIdea(ctx context.Context, in IdeaAddInput) (IdeaWrite, error)
	ListIdeas(ctx context.Context, in IdeaListQuery) (IdeaList, error)
	GetIdea(ctx context.Context, id string) (domain.Idea, error)
	UpdateIdea(ctx context.Context, in IdeaUpdateInput) (IdeaWrite, error)
	TrashIdea(ctx context.Context, id string) (domain.Idea, error)
	RestoreIdea(ctx context.Context, projectID, id string) (domain.Idea, error)
	IdeaBrief(ctx context.Context, in IdeaBriefQuery) (IdeaBrief, error)
}

// ideaArgs is one brain_idea request. Pointer fields distinguish "omitted"
// from an explicit empty value on update.
type ideaArgs struct {
	Action              string          `json:"action"`
	ID                  string          `json:"id"`
	Title               *string         `json:"title"`
	Body                *string         `json:"body"`
	Project             *string         `json:"project"`
	All                 bool            `json:"all"`
	Status              *string         `json:"status"`
	Priority            *string         `json:"priority"`
	DueAt               *string         `json:"due_at"`
	DueDate             *string         `json:"due_date"`
	RemindBeforeMinutes *int            `json:"remind_before_minutes"`
	Tags                []string        `json:"tags"`
	Source              *ideaSourceArgs `json:"source"`
	Note                *string         `json:"note"`
	Reason              *string         `json:"reason"`
	Statuses            []string        `json:"statuses"`
	DueBefore           *string         `json:"due_before"`
	DueAfter            *string         `json:"due_after"`
	Query               *string         `json:"query"`
	Limit               int             `json:"limit"`
	Max                 int             `json:"max"`
	Scope               *string         `json:"scope"`
	Clear               []string        `json:"clear"`
}

// ideaSourceArgs is the small source value object of an idea.
type ideaSourceArgs struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Harness string `json:"harness"`
}

// ideaAnnotations describes brain_idea: mixed reads and writes that are
// reversible (rm is a trash move, restore undoes it).
func ideaAnnotations(title string) Annotations {
	return Annotations{Title: title, ReadOnlyHint: false, DestructiveHint: false, IdempotentHint: false, OpenWorldHint: false}
}

func (s *Server) ideaTool() tool {
	return tool{
		Name:        "brain_idea",
		Annotations: ideaAnnotations("Manage ideas (agent-first TODO layer)"),
		Description: "The agent-first TODO layer. Capture only on explicit intent (\"remember this idea\", " +
			"\"add a todo\", \"don't forget\"); never auto-capture an aside. Tag with the current project or an " +
			"exact registry id; never invent a project. Mark an idea done only when the user says it is done. " +
			"Actions: add, list, get, update, done, reopen, drop, rm, restore, brief. `rm` moves to trash and " +
			"`restore` undoes it; nothing is purged.",
		InputSchema: objectSchema([]string{"action"}, map[string]any{
			"action":                enumProp("Action to run.", ideaActions),
			"id":                    stringProp("Globally unique idea id (get, update, done, reopen, drop, rm, restore)."),
			"title":                 stringProp("Idea title, 1..200 code points (add, update)."),
			"body":                  stringProp("Markdown body, at most 1 MiB (add, update)."),
			"project":               stringProp("Project id, alias or checkout path (add, list, brief). add: target; list: scope, omit to resolve the working directory then personal; brief: ordering project, or the scope with scope=current."),
			"all":                   boolProp("List every registered project (list); default false. Refused on a server limited to named projects."),
			"status":                enumProp("New status (update).", ideaStatusNames),
			"priority":              enumProp("Priority (add, update).", ideaPriorityNames),
			"due_at":                stringProp("RFC3339 with an offset; mutually exclusive with due_date (add, update)."),
			"due_date":              stringProp("YYYY-MM-DD all-day due; mutually exclusive with due_at (add, update)."),
			"remind_before_minutes": intProp("Reminder lead in minutes, 0..10080 (add, update).", 0),
			"tags":                  stringArrayProp("Tags, at most 10; on add/update replaces the list, on list an exact case-insensitive AND filter."),
			"source":                ideaSourceProp(),
			"note":                  stringProp("Completion note (done)."),
			"reason":                stringProp("Why the idea was dropped (drop); stored in completion_note."),
			"statuses":              enumArrayProp("Keep only these statuses (list); empty keeps every status.", ideaStatusNames),
			"due_before":            stringProp("RFC3339 exclusive upper bound (list)."),
			"due_after":             stringProp("RFC3339 inclusive lower bound (list)."),
			"query":                 stringProp("Case-insensitive substring filter (list)."),
			"limit":                 intProp("Page size for list, default 50, max 200.", 0),
			"max":                   intProp("Maximum brief items, default 3, range 1..10 (brief).", 0),
			"scope":                 enumProp("Brief scope: current or all (default all) (brief).", ideaScopes),
			"clear":                 enumArrayProp("Update only: remove due, reminder or tags. Omitted fields are unchanged.", ideaClearNames),
		}),
		OutputSchema: ideaOutputSchema(),
		Limits: map[string]int{
			"list_limit":            ideaMaxListLimit,
			"brief_max":             app.MaxIdeaBriefMax,
			"title_chars":           domain.MaxIdeaTitleChars,
			"body_bytes":            1 << 20,
			"tags":                  domain.MaxIdeaTags,
			"remind_before_minutes": domain.MaxIdeaRemindBeforeMinutes,
		},
		ReadActions:  ideaReadActions,
		WriteActions: ideaWriteActions,
		run:          s.idea,
	}
}

// ideaOutputSchema is the envelope of every action. One typed group is present
// under the key its action promises, so no two actions disagree about a field.
func ideaOutputSchema() map[string]any {
	row := objectProp("Idea row in the contract snake_case shape; list rows omit the body.")
	return outputObject(map[string]any{
		"idea":           row,
		"project_source": stringProp("How the write chose the project: explicit, cwd or default."),
		"ideas":          arrayProp("Idea rows, without bodies.", row),
		"problems":       arrayProp("Idea files the store skipped (path, kind, reason).", objectProp("One problem row.")),
		"text":           stringProp("The shared session block (brief)."),
		"open":           intProp("Open and doing ideas (brief).", 0),
		"due_today":      intProp("Open ideas due today (brief).", 0),
		"muted":          boolProp("The greeting is muted (brief)."),
		"total":          intProp("Matches before the list limit.", 0),
		"id":             stringProp("Idea id."),
		"trashed":        boolProp("The idea moved to trash."),
	})
}

// idea dispatches one brain_idea call.
func (s *Server) idea(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args ideaArgs
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	if action == "" {
		return toolResult{}, refusalInvalid("action is required (" + strings.Join(ideaActions, ", ") + ")")
	}
	backend, ok := s.backend.(IdeaBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_idea", "brain idea in a shell")
	}
	switch action {
	case "add":
		return s.ideaAdd(ctx, backend, args)
	case "list":
		return s.ideaList(ctx, backend, args)
	case "get":
		return s.ideaGet(ctx, backend, args)
	case "update":
		return s.ideaUpdate(ctx, backend, args)
	case "done":
		return s.ideaSetStatus(ctx, backend, args, string(domain.IdeaDone), false)
	case "reopen":
		return s.ideaSetStatus(ctx, backend, args, string(domain.IdeaOpen), false)
	case "drop":
		return s.ideaSetStatus(ctx, backend, args, string(domain.IdeaDropped), true)
	case "rm":
		return s.ideaTrash(ctx, backend, args)
	case "restore":
		return s.ideaRestore(ctx, backend, args)
	case "brief":
		return s.ideaBrief(ctx, backend, args)
	default:
		return toolResult{}, refusalInvalidf("unknown action %q (%s)", action, strings.Join(ideaActions, ", "))
	}
}

func (s *Server) ideaAdd(ctx context.Context, backend IdeaBackend, args ideaArgs) (toolResult, error) {
	title := ""
	if args.Title != nil {
		title = strings.TrimSpace(*args.Title)
	}
	if title == "" {
		return toolResult{}, refusalInvalid("title is required")
	}
	if args.Status != nil {
		return toolResult{}, refusalInvalid("status is not accepted by add: add an open idea, then update its status")
	}
	if len(args.Clear) > 0 {
		return toolResult{}, refusalInvalid("clear is only for update")
	}
	dueAt, dueDate, err := ideaDue(args.DueAt, args.DueDate)
	if err != nil {
		return toolResult{}, err
	}
	if err := checkIdeaPriority(args.Priority); err != nil {
		return toolResult{}, err
	}
	if err := s.checkIdeaAddProject(ctx, optionalString(args.Project)); err != nil {
		return toolResult{}, err
	}
	result, err := backend.AddIdea(ctx, IdeaAddInput{
		ProjectID:           optionalString(args.Project),
		Title:               title,
		Body:                optionalRaw(args.Body),
		Priority:            optionalString(args.Priority),
		DueAt:               dueAt,
		DueDate:             dueDate,
		RemindBeforeMinutes: args.RemindBeforeMinutes,
		Tags:                args.Tags,
		Source:              ideaSource(args.Source),
	})
	if err != nil {
		return toolResult{}, ideaError(err)
	}
	return ideaWriteResult(result), nil
}

func (s *Server) ideaList(ctx context.Context, backend IdeaBackend, args ideaArgs) (toolResult, error) {
	statuses, err := ideaStatuses(args.Statuses)
	if err != nil {
		return toolResult{}, err
	}
	limit := args.Limit
	if limit == 0 {
		limit = ideaDefaultListLimit
	}
	if limit < 0 || limit > ideaMaxListLimit {
		return toolResult{}, refusalInvalidf("limit must be between 1 and %d", ideaMaxListLimit)
	}
	// A read must respect the project allowlist. all=true is a global read and
	// is refused on a limited server; an omitted project follows the same
	// resolution as brain_record (cwd, the single allowed project, or a
	// refusal when several are allowed).
	projectID := ""
	if args.All {
		if optionalString(args.Project) != "" {
			return toolResult{}, refusalInvalid("pass either project or all=true, not both")
		}
		projectID, err = s.scopedReadProject(ctx, "", true, "ideas")
		if err != nil {
			return toolResult{}, err
		}
	} else {
		projectID, err = s.optionalProject(ctx, optionalString(args.Project))
		if err != nil {
			return toolResult{}, err
		}
	}
	list, err := backend.ListIdeas(ctx, IdeaListQuery{
		ProjectID: projectID,
		All:       args.All,
		Statuses:  statuses,
		Tags:      args.Tags,
		DueBefore: optionalString(args.DueBefore),
		DueAfter:  optionalString(args.DueAfter),
		Query:     optionalString(args.Query),
		Limit:     0,
	})
	if err != nil {
		return toolResult{}, ideaError(err)
	}
	total := list.Total
	if total == 0 {
		total = len(list.Ideas)
	}
	ideas := list.Ideas
	if len(ideas) > limit {
		ideas = ideas[:limit]
	}
	rows := ideasview.Rows(ideas, false)
	problems := list.Problems
	if problems == nil {
		problems = []IdeaProblem{}
	}
	text := fmt.Sprintf("%d idea(s)", total)
	if len(rows) > 0 {
		var lines []string
		for _, idea := range ideas {
			lines = append(lines, ideaLine(idea, time.Now()))
		}
		text += "\n" + strings.Join(lines, "\n")
	}
	if len(problems) > 0 {
		text += fmt.Sprintf("\nproblems: %d (resolve in git)", len(problems))
	}
	return toolResult{
		Text: text,
		Structured: ideaListOutput{
			Ideas:    rows,
			Problems: problems,
			Total:    total,
		},
	}, nil
}

func (s *Server) ideaGet(ctx context.Context, backend IdeaBackend, args ideaArgs) (toolResult, error) {
	id, err := requireText("id", args.ID)
	if err != nil {
		return toolResult{}, err
	}
	idea, err := backend.GetIdea(ctx, id)
	if err != nil {
		return toolResult{}, ideaError(err)
	}
	// The id is global, so the stored project, not a selector, is what the
	// read policy must check.
	if err := s.policy.checkProject(idea.ProjectID); err != nil {
		return toolResult{}, err
	}
	return toolResult{
		Text:       ideaText(idea, true),
		Structured: ideaGetOutput{Idea: ideasview.RowOf(idea, true)},
	}, nil
}

func (s *Server) ideaUpdate(ctx context.Context, backend IdeaBackend, args ideaArgs) (toolResult, error) {
	id, err := requireText("id", args.ID)
	if err != nil {
		return toolResult{}, err
	}
	if err := checkIdeaStatus(args.Status); err != nil {
		return toolResult{}, err
	}
	if err := checkIdeaPriority(args.Priority); err != nil {
		return toolResult{}, err
	}
	dueAt, dueDate, clearDue, err := ideaUpdateDue(args)
	if err != nil {
		return toolResult{}, err
	}
	_, clearReminder, clearTags, err := ideaClear(args.Clear)
	if err != nil {
		return toolResult{}, err
	}
	if _, err := s.checkIdeaLiveWrite(ctx, backend, id); err != nil {
		return toolResult{}, err
	}
	if move := optionalString(args.Project); move != "" {
		if _, err := s.writeTarget(ctx, move); err != nil {
			return toolResult{}, err
		}
	}
	in := IdeaUpdateInput{
		ID:                  id,
		ProjectID:           optionalString(args.Project),
		Title:               args.Title,
		Body:                args.Body,
		Status:              args.Status,
		Priority:            args.Priority,
		DueAt:               dueAt,
		DueDate:             dueDate,
		RemindBeforeMinutes: args.RemindBeforeMinutes,
		Tags:                ideaTags(args.Tags),
		ClearDue:            clearDue,
		ClearReminder:       clearReminder,
		ClearTags:           clearTags,
		Note:                optionalString(args.Note),
	}
	result, err := backend.UpdateIdea(ctx, in)
	if err != nil {
		return toolResult{}, ideaError(err)
	}
	return ideaWriteResult(result), nil
}

func (s *Server) ideaSetStatus(ctx context.Context, backend IdeaBackend, args ideaArgs, status string, dropped bool) (toolResult, error) {
	id, err := requireText("id", args.ID)
	if err != nil {
		return toolResult{}, err
	}
	note := optionalString(args.Note)
	if dropped {
		note = optionalString(args.Reason)
	}
	if _, err := s.checkIdeaLiveWrite(ctx, backend, id); err != nil {
		return toolResult{}, err
	}
	result, err := backend.UpdateIdea(ctx, IdeaUpdateInput{ID: id, Status: &status, Note: note})
	if err != nil {
		return toolResult{}, ideaError(err)
	}
	return ideaWriteResult(result), nil
}

func (s *Server) ideaTrash(ctx context.Context, backend IdeaBackend, args ideaArgs) (toolResult, error) {
	id, err := requireText("id", args.ID)
	if err != nil {
		return toolResult{}, err
	}
	if _, err := s.checkIdeaLiveWrite(ctx, backend, id); err != nil {
		return toolResult{}, err
	}
	if _, err := backend.TrashIdea(ctx, id); err != nil {
		return toolResult{}, ideaError(err)
	}
	return toolResult{
		Text:       "trashed " + id,
		Structured: ideaTrashOutput{ID: id, Trashed: true},
	}, nil
}

func (s *Server) ideaRestore(ctx context.Context, backend IdeaBackend, args ideaArgs) (toolResult, error) {
	id, err := requireText("id", args.ID)
	if err != nil {
		return toolResult{}, err
	}
	// A restricted server cannot look up the trashed idea's project, so it
	// restores only from its own default project: an idea from another project
	// is NOT_FOUND instead of a write outside the scope.
	projectID := ""
	if s.policy.RestrictWrites && !s.policy.AllowWrite {
		if s.policy.DefaultProject == "" {
			return toolResult{}, unsupportedBackend("brain_idea(action=restore)",
				"start the server in the project checkout, or pass --allow-write")
		}
		projectID = s.policy.DefaultProject
	}
	idea, err := backend.RestoreIdea(ctx, projectID, id)
	if err != nil {
		return toolResult{}, ideaError(err)
	}
	return ideaWriteResult(IdeaWrite{Idea: idea, ProjectSource: app.IdeaProjectExplicit}), nil
}

func (s *Server) ideaBrief(ctx context.Context, backend IdeaBackend, args ideaArgs) (toolResult, error) {
	scope := strings.ToLower(strings.TrimSpace(optionalString(args.Scope)))
	switch scope {
	case "":
		scope = app.IdeaBriefScopeAll
	case app.IdeaBriefScopeCurrent, app.IdeaBriefScopeAll:
	default:
		return toolResult{}, refusalInvalidf("unknown scope %q (current or all)", optionalString(args.Scope))
	}
	max := args.Max
	if max == 0 {
		max = app.DefaultIdeaBriefMax
	}
	if max < 1 || max > app.MaxIdeaBriefMax {
		return toolResult{}, refusalInvalidf("max must be between 1 and %d", app.MaxIdeaBriefMax)
	}
	projectID, scope, err := s.ideaBriefScope(ctx, optionalString(args.Project), scope)
	if err != nil {
		return toolResult{}, err
	}
	brief, err := backend.IdeaBrief(ctx, IdeaBriefQuery{
		ProjectID: projectID,
		Scope:     scope,
		Max:       max,
	})
	if err != nil {
		return toolResult{}, ideaError(err)
	}
	rows := ideasview.Rows(brief.Ideas, false)
	return toolResult{
		Text: brief.Text,
		Structured: ideaBriefOutput{
			Text: brief.Text, Ideas: rows, Open: brief.Open, DueToday: brief.DueToday, Muted: brief.Muted,
		},
	}, nil
}

// ideaWriteResult renders a mutating action: the idea row with its body and how
// the project was chosen. An id-addressed write reports explicit, matching the
// CLI.
func ideaWriteResult(result IdeaWrite) toolResult {
	source := result.ProjectSource
	if source == "" {
		source = app.IdeaProjectExplicit
	}
	return toolResult{
		Text:       ideaText(result.Idea, true),
		Structured: ideaWriteOutput{Idea: ideasview.RowOf(result.Idea, true), ProjectSource: source},
	}
}

// ideaGetOutput is one idea with its body.
type ideaGetOutput struct {
	Idea ideasview.Row `json:"idea"`
}

// ideaWriteOutput is a mutated idea plus how its project was chosen.
type ideaWriteOutput struct {
	Idea          ideasview.Row `json:"idea"`
	ProjectSource string        `json:"project_source"`
}

// ideaListOutput is a filtered listing.
type ideaListOutput struct {
	Ideas    []ideasview.Row `json:"ideas"`
	Problems []IdeaProblem   `json:"problems"`
	Total    int             `json:"total"`
}

// ideaBriefOutput is the shared session block.
type ideaBriefOutput struct {
	Text     string          `json:"text"`
	Ideas    []ideasview.Row `json:"ideas"`
	Open     int             `json:"open"`
	DueToday int             `json:"due_today"`
	Muted    bool            `json:"muted"`
}

// ideaTrashOutput is the reversible remove result.
type ideaTrashOutput struct {
	ID      string `json:"id"`
	Trashed bool   `json:"trashed"`
}

// ideaText renders one idea for the text channel. The body is included where
// the contract includes it.
func ideaText(idea domain.Idea, withBody bool) string {
	text := ideaLine(idea, time.Now())
	if withBody && strings.TrimSpace(idea.Body) != "" {
		text += "\n" + idea.Body
	}
	return text
}

// ideaLine renders marker, due or priority chip, title and project through the
// shared app formatter.
func ideaLine(idea domain.Idea, now time.Time) string {
	return app.IdeaItemLine(idea, now)
}

// ideaDue parses the mutually exclusive due arguments of add.
func ideaDue(dueAt, dueDate *string) (*time.Time, string, error) {
	if dueAt != nil && strings.TrimSpace(*dueAt) == "" {
		return nil, "", refusalInvalid("due_at must not be empty; omit it for no due")
	}
	if dueDate != nil && strings.TrimSpace(*dueDate) == "" {
		return nil, "", refusalInvalid("due_date must not be empty; omit it for no due")
	}
	atText := optionalString(dueAt)
	dateText := optionalString(dueDate)
	if atText != "" && dateText != "" {
		return nil, "", refusalInvalid("due_at and due_date are mutually exclusive")
	}
	at, err := parseIdeaInstant("due_at", atText)
	if err != nil {
		return nil, "", err
	}
	if dateText != "" {
		if err := validateIdeaDate(dateText); err != nil {
			return nil, "", err
		}
	}
	return at, dateText, nil
}

// ideaUpdateDue parses the due fields of update. clear:["due"] is the only way
// to remove a due; setting one replaces the other in the app.
func ideaUpdateDue(args ideaArgs) (*time.Time, *string, bool, error) {
	clearDue, _, _, err := ideaClear(args.Clear)
	if err != nil {
		return nil, nil, false, err
	}
	if args.DueAt != nil && strings.TrimSpace(*args.DueAt) == "" {
		return nil, nil, false, refusalInvalid("due_at must not be empty; use clear:[\"due\"]")
	}
	if args.DueDate != nil && strings.TrimSpace(*args.DueDate) == "" {
		return nil, nil, false, refusalInvalid("due_date must not be empty; use clear:[\"due\"]")
	}
	atText := optionalString(args.DueAt)
	dateText := optionalString(args.DueDate)
	if atText != "" && dateText != "" {
		return nil, nil, false, refusalInvalid("due_at and due_date are mutually exclusive")
	}
	if clearDue && (atText != "" || dateText != "") {
		return nil, nil, false, refusalInvalid("clear \"due\" and due_at/due_date are mutually exclusive")
	}
	at, err := parseIdeaInstant("due_at", atText)
	if err != nil {
		return nil, nil, false, err
	}
	var date *string
	if dateText != "" {
		if err := validateIdeaDate(dateText); err != nil {
			return nil, nil, false, err
		}
		date = &dateText
	}
	return at, date, clearDue, nil
}

// ideaClear validates the clear list and returns its three flags.
func ideaClear(values []string) (due, reminder, tags bool, err error) {
	for _, value := range values {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "due":
			due = true
		case "reminder":
			reminder = true
		case "tags":
			tags = true
		default:
			return false, false, false, refusalInvalidf("unknown clear %q (due, reminder, tags)", value)
		}
	}
	return due, reminder, tags, nil
}

// ideaStatuses validates a list status filter.
func ideaStatuses(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, value := range values {
		status := strings.ToLower(strings.TrimSpace(value))
		if status == "" {
			continue
		}
		if !domain.IdeaStatus(status).Valid() {
			return nil, refusalInvalidf("unknown status %q (%s)", value, strings.Join(ideaStatusNames, ", "))
		}
		out = append(out, status)
	}
	return out, nil
}

func checkIdeaStatus(status *string) error {
	value := optionalString(status)
	if value == "" {
		return nil
	}
	if !domain.IdeaStatus(strings.ToLower(value)).Valid() {
		return refusalInvalidf("unknown status %q (%s)", value, strings.Join(ideaStatusNames, ", "))
	}
	return nil
}

func checkIdeaPriority(priority *string) error {
	value := optionalString(priority)
	if value == "" {
		return nil
	}
	if !domain.IdeaPriority(strings.ToLower(value)).Valid() {
		return refusalInvalidf("unknown priority %q (%s)", value, strings.Join(ideaPriorityNames, ", "))
	}
	return nil
}

// ideaTags distinguishes an omitted tags property from an explicit empty list.
func ideaTags(values []string) *[]string {
	if values == nil {
		return nil
	}
	tags := append([]string(nil), values...)
	return &tags
}

// ideaSource maps the source object of add.
func ideaSource(source *ideaSourceArgs) *domain.IdeaSource {
	if source == nil {
		return nil
	}
	return &domain.IdeaSource{Type: source.Type, ID: source.ID, Harness: source.Harness}
}

// optionalString dereferences an optional argument; nil and empty both mean
// "not provided".
func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

// optionalRaw dereferences an optional text field without trimming it: a body
// keeps its leading and trailing whitespace.
func optionalRaw(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// parseIdeaInstant parses an RFC3339 timestamp with an offset.
func parseIdeaInstant(field, value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, refusalInvalid(field + " must be RFC3339 with an offset, for example 2026-11-27T18:00:00+03:00")
	}
	return &at, nil
}

func validateIdeaDate(value string) error {
	if _, err := time.Parse(domain.IdeaDateLayout, value); err != nil {
		return refusalInvalidf("due_date %q is not a %s date", value, domain.IdeaDateLayout)
	}
	return nil
}

// ideaBriefScope resolves the brief scope under the read policy. A limited
// server may not answer a global brief (scope=all), because the global counts
// would name projects the caller cannot read; it must ask for one project with
// scope=current. selector is resolved and checked like any other read.
func (s *Server) ideaBriefScope(ctx context.Context, selector, scope string) (string, string, error) {
	if scope == app.IdeaBriefScopeCurrent {
		projectID, err := s.optionalProject(ctx, selector)
		if err != nil {
			return "", "", err
		}
		return projectID, app.IdeaBriefScopeCurrent, nil
	}
	if len(s.policy.Projects) > 0 {
		return "", "", refusalInvalidf(
			"scope=all is not available: this server is limited to %s; pass scope=current (and optionally project)",
			s.policy.projectList())
	}
	if selector != "" {
		target, err := s.resolveTarget(ctx, selector)
		if err != nil {
			return "", "", err
		}
		return target.ID, app.IdeaBriefScopeAll, nil
	}
	return "", app.IdeaBriefScopeAll, nil
}

// checkIdeaAddProject enforces the write scope before an add. An explicit
// selector resolves through writeTarget; an omitted one follows the app's
// working-directory resolution, which is the server's default project, so the
// policy checks that project. A server without a default project refuses.
func (s *Server) checkIdeaAddProject(ctx context.Context, selector string) error {
	if selector != "" {
		_, err := s.writeTarget(ctx, selector)
		return err
	}
	if !s.policy.RestrictWrites || s.policy.AllowWrite {
		return nil
	}
	return s.policy.checkWrite(s.policy.DefaultProject)
}

// checkIdeaLiveWrite resolves the idea first and enforces the write scope on
// its stored project. The id is global, so the idea itself supplies the project
// a selector would otherwise name.
func (s *Server) checkIdeaLiveWrite(ctx context.Context, backend IdeaBackend, id string) (domain.Idea, error) {
	idea, err := backend.GetIdea(ctx, id)
	if err != nil {
		return domain.Idea{}, ideaError(err)
	}
	if err := s.policy.checkWrite(idea.ProjectID); err != nil {
		return domain.Idea{}, err
	}
	return idea, nil
}

// ideaError maps idea failures to the documented refusals. An unknown id is a
// NOT_FOUND that points at brain_idea, never a silent empty result.
func ideaError(err error) error {
	if errors.Is(err, domain.ErrNotFound) {
		return &RefusalError{Refusal: Refusal{
			Code:       RefusalNotFound,
			Message:    err.Error(),
			TryInstead: "brain_idea(action=list) shows ids; brain_idea(action=add) captures a new one",
		}}
	}
	return err
}
