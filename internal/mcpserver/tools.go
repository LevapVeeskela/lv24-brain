package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/textbudget"
)

// DefaultJournalLimit is used by brain_catchup(mode=journal) when no limit is
// given.
const DefaultJournalLimit = 20

// countsScanLimit bounds how many records brain_open(include=counts) reads to
// count them.
const countsScanLimit = 10000

var decisionStatuses = []string{"proposed", "accepted", "rejected", "superseded"}

// openIncludes are the optional sections brain_open can add.
var openIncludes = []string{"state", "handoff", "recent", "counts"}

// recordScopes are the engram-compatible record scopes.
var recordScopes = []string{"project", "personal", "global"}

// WorkspaceOverview is returned by Backend.Resolve when the selector (or the
// working directory) is a workspace root holding several projects. brain_open
// answers with the overview instead of an error so the agent can pick one.
type WorkspaceOverview struct {
	Text    string
	Message string
}

func (w *WorkspaceOverview) Error() string { return w.Message }

// Target is a project resolved from a selector.
type Target struct {
	ID string
	// Path is the checkout the selector resolved through; "" when none.
	Path string
	// Source says how the selector resolved: cwd, id, alias or path. The
	// native backend fills it; an empty source means the backend did not say.
	Source string
}

// Record identifies a stored knowledge record.
type Record struct {
	ID        string
	ProjectID string
	// TopicKey is the classification the write landed under, when any.
	TopicKey string
	// Replaced reports that a topic upsert updated an existing record.
	Replaced bool
	// GitContext is the context captured by a handoff write.
	GitContext string
}

// Hit is one search result.
type Hit struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Snippet   string `json:"snippet"`
	// Score is the composite relevance in [0, 1].
	Score float64 `json:"score"`
	// MatchReasons names the ranking signals that fired: bm25, recency,
	// importance, pin, link. A backend without a ranked index returns none.
	MatchReasons []string `json:"match_reasons,omitempty"`
}

// SearchInput is one brain_search request after argument validation.
type SearchInput struct {
	Query     string
	ProjectID string
	Limit     int
	// MatchMode combines query terms: all requires every term, any accepts
	// one. Empty keeps the historical relaxed behaviour.
	MatchMode string
	// Scope keeps only records with that scope: project, personal or global.
	Scope string
	// Kinds keeps only these kinds; empty keeps every non-excluded kind.
	Kinds []string
	// IncludeDumps adds connector dumps to an unfiltered search.
	IncludeDumps bool
	// Since keeps only documents updated since this point: a duration (24h,
	// 7d), a date (2026-11-01) or an RFC3339 time; empty keeps everything.
	// The backend parses and validates it.
	Since string
}

// Project is a registered project.
type Project struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Paths   []string `json:"paths"`
	RepoURL string   `json:"repo_url"`
}

// ProjectInput registers or updates one project.
type ProjectInput struct {
	ID      string
	Name    string
	Paths   []string
	Aliases []string
}

// ProjectBackend is implemented by backends that can register a project. The
// read actions stay in Backend; only register needs this interface.
type ProjectBackend interface {
	RegisterProject(ctx context.Context, in ProjectInput) (Project, error)
}

// JournalEntry is one activity journal line.
type JournalEntry struct {
	Time    time.Time         `json:"time"`
	User    string            `json:"user"`
	Event   string            `json:"event"`
	Project string            `json:"project"`
	ID      string            `json:"id"`
	Title   string            `json:"title"`
	Fields  map[string]string `json:"fields"`
}

// JournalQuery filters a journal read. Zero values match everything.
type JournalQuery struct {
	ProjectID string
	Limit     int
}

// NoteInput is a note to append. Title may be empty; the backend derives it.
type NoteInput struct {
	ProjectID string
	Title     string
	Body      string
	Subtype   string
	Scope     string
	TopicKey  string
	// Source is an optional external id the note was imported from.
	Source string
}

// DecisionInput is a decision record to append.
type DecisionInput struct {
	ProjectID    string
	Title        string
	Status       string
	Context      string
	Alternatives []string
	Consequences []string
	Paths        []string
}

// AttemptInput is an attempt record to append.
type AttemptInput struct {
	ProjectID  string
	Title      string
	Hypothesis string
	Result     string
	Reason     string
}

// HandoffSection is one named part of a handoff. Empty parts are dropped.
type HandoffSection struct {
	Goal         string
	Instructions string
	Discoveries  string
	Accomplished string
	Next         string
	Files        []string
}

// HandoffInput is a handoff to append. The backend captures git context for
// the project checkout itself.
type HandoffInput struct {
	ProjectID  string
	Summary    string
	NextAction string
	// Sections are rendered into the stored summary when present.
	Sections HandoffSection
}

// Backend is the set of brain operations the MCP tools need. cmd/brain adapts
// the application layer to this interface; tests supply a fake.
type Backend interface {
	// Resolve maps an id, alias or path to a registered project. An empty
	// selector means the working directory. It never creates a project.
	Resolve(ctx context.Context, selector string) (Target, error)
	// Bootstrap returns the bounded context summary of a project. budget zero
	// selects the default size. includeUnreviewed includes quarantined
	// records that are excluded from trusted context by default.
	Bootstrap(ctx context.Context, projectID string, budget int, includeUnreviewed bool) (string, error)
	// Search runs full-text search. An empty ProjectID searches every project.
	Search(ctx context.Context, in SearchInput) ([]Hit, error)
	Note(ctx context.Context, in NoteInput) (Record, error)
	Decision(ctx context.Context, in DecisionInput) (Record, error)
	Attempt(ctx context.Context, in AttemptInput) (Record, error)
	Handoff(ctx context.Context, in HandoffInput) (Record, error)
	// Journal returns entries newest first. An empty ProjectID matches all.
	Journal(ctx context.Context, query JournalQuery) ([]JournalEntry, error)
	Projects(ctx context.Context) ([]Project, error)
}

// toolResult is the successful output of a tool. Structured must marshal to a
// JSON object. Meta carries the per-call compatibility envelope of a migration
// alias; it is emitted as the result _meta object.
type toolResult struct {
	Text       string
	Structured any
	Meta       map[string]any
}

type toolFunc func(ctx context.Context, args json.RawMessage) (toolResult, error)

// tool is one MCP tool: its protocol metadata and its handler.
type tool struct {
	Name        string
	Description string
	// Annotations carries the title and the behaviour hints hosts read before
	// asking the user to approve a call.
	Annotations  Annotations
	InputSchema  map[string]any
	OutputSchema map[string]any
	// Limits are the caps the catalogue reports for this tool.
	Limits map[string]int
	// ReadActions names the actions of a mixed tool that never mutate.
	ReadActions []string
	// WriteActions names the mutating actions of a mixed tool. A read-only
	// server still lists the tool for its read actions and refuses these
	// before the backend runs; a tool without WriteActions is listed only
	// when it is read-only.
	WriteActions []string
	// Legacy, Target, Action, Grade, Unsupported and Note describe a migration
	// alias.
	Legacy      bool
	Deprecated  bool
	Target      string
	Action      string
	Grade       string
	Unsupported []string
	Note        string
	Reason      string
	Remediation string
	// Stub marks a codegraph/graphify placeholder.
	Stub bool
	// confirm returns the question to ask the user before the call runs, or
	// nil when the call needs no confirmation. It is consulted only for 2026
	// clients that declared elicitation (see mrtr.go).
	confirm func(args json.RawMessage) *confirmation
	run     toolFunc
}

// nativeToolList is the 17-tool native surface, in a fixed order: session
// start, task context, search, one-record access, the four appends, the
// curation tools, the code graph, then introspection.
func (s *Server) nativeToolList() []tool {
	return []tool{
		s.openTool(),
		s.contextTool(),
		s.searchTool(),
		s.recordTool(),
		s.noteTool(),
		s.decisionTool(),
		s.attemptTool(),
		s.handoffTool(),
		s.stateTool(),
		s.catchupTool(),
		s.reviewTool(),
		s.judgeTool(),
		s.projectsTool(),
		s.promptTool(),
		s.codeTool(),
		s.graphTool(),
		s.capabilitiesTool(),
	}
}

// retiredTool is a 0.16 name and the native call that replaced it.
type retiredTool struct {
	Replacement string
	Action      string
}

// retiredTools maps the names 0.17 removed from the default profile. A call to
// one is a typed NOT_FOUND with the replacement, never a silent success.
var retiredTools = map[string]retiredTool{
	"brain_state_get":       {Replacement: "brain_state", Action: "get"},
	"brain_state_set":       {Replacement: "brain_state", Action: "set"},
	"brain_journal":         {Replacement: "brain_catchup", Action: "journal"},
	"brain_records":         {Replacement: "brain_record", Action: "list"},
	"brain_relations":       {Replacement: "brain_record", Action: "relations"},
	"brain_review_record":   {Replacement: "brain_judge", Action: "review"},
	"brain_decision_status": {Replacement: "brain_judge", Action: "accept|reject|supersede"},
}

// retiredRefusal renders the migration hint of a retired tool name.
func retiredRefusal(name string, retired retiredTool) Refusal {
	return Refusal{
		Code:       RefusalNotFound,
		Message:    name + " was retired in 0.17; the tool is " + retired.Replacement + " with action " + retired.Action,
		TryInstead: retired.Replacement + "(action=" + retired.Action + ")",
	}
}

// unsupportedBackend is the refusal of a tool whose optional backend
// interface the running server does not provide.
func unsupportedBackend(toolName, tryInstead string) error {
	return refusalUnsupported(toolName+" is not available: the configured backend does not support it", tryInstead)
}

func (s *Server) openTool() tool {
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	return tool{
		Name:        "brain_open",
		Annotations: readOnlyAnnotations("Open project context"),
		Description: "Load the curated context of a project: state, recent decisions, attempts and the latest handoff. Call at the start of a session. " +
			"include adds selected sections; without it the result is the bounded summary only.",
		InputSchema: objectSchema(nil, map[string]any{
			"project":            project,
			"budget":             intProp("Summary size budget in runes; 0 selects the default.", 0),
			"include":            enumArrayProp("Add these sections: state, handoff, recent, counts.", openIncludes),
			"since":              stringProp("With include=recent, keep records updated since: a duration (24h, 7d) or a date (2026-11-01)."),
			"include_unreviewed": boolProp("Include quarantined records (unreviewed agent, connector or import writes) that are excluded by default."),
		}),
		OutputSchema: outputObject(map[string]any{
			"workspace":  boolProp("True when the selector resolved to a workspace root; overview then holds its projects."),
			"overview":   stringProp("Workspace overview text, in workspace mode."),
			"project_id": stringProp("Resolved project id, in project mode."),
			"path":       stringProp("Checkout path the project resolved through."),
			"summary":    stringProp("Bounded context summary, in project mode."),
			"state":      objectProp("Project state, with include=state."),
			"handoff":    objectProp("Latest handoff summary, with include=handoff."),
			"recent":     arrayProp("Recent records, with include=recent.", recordSummarySchema()),
			"counts":     objectProp("Record and project counts, with include=counts."),
		}),
		run: s.open,
	}
}

// OpenCounts is the counts section of brain_open.
type OpenCounts struct {
	Records  int            `json:"records"`
	ByKind   map[string]int `json:"by_kind"`
	Projects int            `json:"projects"`
}

// openResult is the structured output of brain_open. Optional sections are
// omitted unless requested, so the historical call keeps its shape.
type openResult struct {
	Workspace bool            `json:"workspace,omitempty"`
	Overview  string          `json:"overview,omitempty"`
	ProjectID string          `json:"project_id,omitempty"`
	Path      string          `json:"path,omitempty"`
	Summary   string          `json:"summary,omitempty"`
	State     *State          `json:"state,omitempty"`
	Handoff   *RecordSummary  `json:"handoff,omitempty"`
	Recent    []RecordSummary `json:"recent,omitempty"`
	Counts    *OpenCounts     `json:"counts,omitempty"`
}

func (s *Server) open(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Project           string   `json:"project"`
		Budget            int      `json:"budget"`
		Include           []string `json:"include"`
		Since             string   `json:"since"`
		IncludeUnreviewed bool     `json:"include_unreviewed"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	if args.Budget < 0 {
		return toolResult{}, refusalInvalid("budget must not be negative")
	}
	includes := map[string]bool{}
	for _, include := range args.Include {
		include = strings.TrimSpace(include)
		if !slices.Contains(openIncludes, include) {
			return toolResult{}, refusalInvalidf("unknown include %q (%s)", include, strings.Join(openIncludes, ", "))
		}
		includes[include] = true
	}
	if len(includes) > 0 {
		if err := s.requireOpenSections(includes); err != nil {
			return toolResult{}, err
		}
	}
	target, err := s.resolveTarget(ctx, strings.TrimSpace(args.Project))
	var overview *WorkspaceOverview
	if errors.As(err, &overview) {
		// A workspace overview lists every project of the data directory, so a
		// project allowlist must not reach it: the client has to name one.
		if len(s.policy.Projects) > 0 {
			return toolResult{}, refusalInvalidf("project is required: this server is limited to %s; a workspace root lists every project", s.policy.projectList())
		}
		return toolResult{
			Text:       overview.Text,
			Structured: openResult{Workspace: true, Overview: overview.Text},
		}, nil
	}
	if err != nil {
		return toolResult{}, err
	}
	summary, err := s.backend.Bootstrap(ctx, target.ID, args.Budget, args.IncludeUnreviewed)
	if err != nil {
		return toolResult{}, err
	}
	result := openResult{ProjectID: target.ID, Path: target.Path, Summary: summary}
	if includes["state"] {
		state, err := s.backend.(StateBackend).GetState(ctx, target.ID)
		if err != nil {
			return toolResult{}, err
		}
		result.State = &state
	}
	if includes["handoff"] {
		handoff, err := s.latestHandoff(ctx, target.ID)
		if err != nil {
			return toolResult{}, err
		}
		result.Handoff = handoff
	}
	if includes["recent"] {
		records, err := s.recentRecords(ctx, target.ID, args.Since)
		if err != nil {
			return toolResult{}, err
		}
		result.Recent = records
	}
	if includes["counts"] {
		counts, err := s.openCounts(ctx, target.ID)
		if err != nil {
			return toolResult{}, err
		}
		result.Counts = counts
	}
	return toolResult{Text: summary, Structured: result}, nil
}

// requireOpenSections checks that the backend can answer the requested
// sections before any work starts, so a call never returns a partial answer.
func (s *Server) requireOpenSections(includes map[string]bool) error {
	if includes["state"] {
		if _, ok := s.backend.(StateBackend); !ok {
			return unsupportedBackend("brain_open(include=state)", "brain_open without include=state")
		}
	}
	if includes["handoff"] || includes["recent"] || includes["counts"] {
		if _, ok := s.backend.(RecordBackend); !ok {
			return unsupportedBackend("brain_open(include=handoff,recent,counts)", "brain_open without those sections")
		}
	}
	return nil
}

func (s *Server) latestHandoff(ctx context.Context, projectID string) (*RecordSummary, error) {
	records, err := s.backend.(RecordBackend).ListRecords(ctx, RecordQuery{ProjectID: projectID, Kinds: []string{"handoff"}, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}
	return &records[0], nil
}

func (s *Server) recentRecords(ctx context.Context, projectID, since string) ([]RecordSummary, error) {
	query := RecordQuery{ProjectID: projectID, Kinds: []string{"decision", "attempt", "handoff", "note"}, Limit: 10}
	if strings.TrimSpace(since) != "" {
		at, err := domain.ParseSince(since, time.Now())
		if err != nil {
			return nil, err
		}
		query.Since = at
	}
	records, err := s.backend.(RecordBackend).ListRecords(ctx, query)
	if err != nil {
		return nil, err
	}
	if records == nil {
		records = []RecordSummary{}
	}
	return records, nil
}

func (s *Server) openCounts(ctx context.Context, projectID string) (*OpenCounts, error) {
	records, err := s.backend.(RecordBackend).ListRecords(ctx, RecordQuery{ProjectID: projectID, Limit: countsScanLimit})
	if err != nil {
		return nil, err
	}
	counts := &OpenCounts{Records: len(records), ByKind: map[string]int{}}
	for _, record := range records {
		counts.ByKind[record.Kind]++
	}
	projects, err := s.backend.Projects(ctx)
	if err != nil {
		return nil, err
	}
	counts.Projects = len(s.policy.filterProjects(projects))
	return counts, nil
}

func (s *Server) searchTool() tool {
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	return tool{
		Name:        "brain_search",
		Annotations: readOnlyAnnotations("Search the brain"),
		Description: "Full-text search over notes, decisions, attempts and handoffs. Searches every project unless project is given. Connector dumps (mail, GitHub/GitLab issues and PRs) are excluded unless include_dumps is true or kinds contains \"dump\". Raw-zone external items are excluded unless kinds contains \"item\". " +
			"Hits carry an id, the kind, the title, a preview of at most " + fmt.Sprint(textbudget.PreviewRunes) + " characters and the ranking score; bodies are never returned by default. Read one with expand_ids, or set compact=false to expand the whole page.",
		InputSchema: objectSchema([]string{"query"}, map[string]any{
			"query":           stringProp("Words to search for."),
			"project":         project,
			"limit":           intProp("Page size; 0 selects the default page of 20.", 0),
			"cursor":          cursorProp(),
			"kinds":           stringArrayProp("Keep only these kinds: note, decision, attempt, handoff, dump, item. Omit for curated kinds; raw-zone dumps and external items require an explicit request."),
			"scope":           enumProp("Keep only records with this scope: project, personal or global.", recordScopes),
			"match_mode":      enumProp("all requires every query term; any accepts one. Omit for the historical relaxed behaviour.", []string{"all", "any"}),
			"include_dumps":   boolProp("Include connector dumps in an unfiltered search."),
			"include_prompts": boolProp("Not available: the prompt zone is local-only and never indexed. Use brain_prompt(action=search)."),
			"all_projects":    boolProp("Search every project explicitly; mutually exclusive with project."),
			"since":           stringProp("Only documents updated since: a duration (24h, 7d), a date (2026-11-01) or an RFC3339 time. Omit for all time."),
			"compact":         boolProp("Return previews only (the default). Set false to return the whole body of every hit on the page; prefer expand_ids for one record."),
			"expand":          boolProp("Expand every hit on this page to its full record. Prefer expand_ids when only one or two records matter."),
			"expand_ids":      stringArrayProp("Ids from the hits of this call to return in full in the same response. At most " + fmt.Sprint(ExpandMaxRecords) + " ids."),
			"max_tokens":      intProp("Token budget per expanded record; 0 selects the default of "+fmt.Sprint(ExpandDefaultMaxTokens)+", the maximum is "+fmt.Sprint(ExpandMaxTokens)+".", 0),
		}),
		OutputSchema: outputObject(pageOutput(map[string]any{
			"hits": arrayProp("Ranked hits, best first.", outputObject(map[string]any{
				"id":            stringProp("Record id."),
				"project_id":    stringProp("Project that owns the record."),
				"kind":          stringProp("Record kind."),
				"title":         stringProp("Record title."),
				"snippet":       stringProp("Matching preview, at most " + fmt.Sprint(textbudget.PreviewRunes) + " characters."),
				"score":         numberProp("Composite relevance in [0, 1]."),
				"match_reasons": stringArrayProp("Ranking signals that fired: bm25, recency, importance, pin, link."),
			}, "id", "project_id", "kind", "title", "snippet")),
			"compact":  boolProp("Whether bodies were withheld from the hits."),
			"expanded": arrayProp("Records returned in full, when expand, expand_ids or compact=false asked for them.", objectProp("One expanded record: its detail fields plus truncated and tokens.")),
		}), "hits", "page", "page_size", "has_more"),
		Limits: limitsOf(pageLimitsFor("brain_search")),
		run:    s.search}
}

func (s *Server) search(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Query          string   `json:"query"`
		Project        string   `json:"project"`
		Limit          int      `json:"limit"`
		Cursor         string   `json:"cursor"`
		Kinds          []string `json:"kinds"`
		Scope          string   `json:"scope"`
		MatchMode      string   `json:"match_mode"`
		IncludeDumps   bool     `json:"include_dumps"`
		IncludePrompts bool     `json:"include_prompts"`
		AllProjects    bool     `json:"all_projects"`
		Since          string   `json:"since"`
		// Compact is a pointer so an omitted flag keeps the preview-only
		// default instead of reading as false.
		Compact   *bool    `json:"compact"`
		Expand    bool     `json:"expand"`
		ExpandIDs []string `json:"expand_ids"`
		MaxTokens int      `json:"max_tokens"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	query, err := requireText("query", args.Query)
	if err != nil {
		return toolResult{}, err
	}
	if args.IncludePrompts {
		return toolResult{}, refusedGap("brain_search", "include_prompts")
	}
	if args.AllProjects && strings.TrimSpace(args.Project) != "" {
		return toolResult{}, refusalInvalid("pass either project or all_projects=true, not both")
	}
	if args.AllProjects && len(s.policy.Projects) > 0 {
		return toolResult{}, refusalInvalidf("all_projects is not available: this server is limited to %s", s.policy.projectList())
	}
	scope := strings.TrimSpace(args.Scope)
	if scope != "" && !slices.Contains(recordScopes, scope) {
		return toolResult{}, refusalInvalidf("unknown scope %q (%s)", scope, strings.Join(recordScopes, ", "))
	}
	matchMode := strings.TrimSpace(args.MatchMode)
	if matchMode != "" && matchMode != "all" && matchMode != "any" {
		return toolResult{}, refusalInvalid("match_mode must be all or any")
	}
	if len(args.ExpandIDs) > ExpandMaxRecords {
		return toolResult{}, refusalInvalidf("expand_ids holds %d ids; the limit is %d", len(args.ExpandIDs), ExpandMaxRecords)
	}
	if args.MaxTokens < 0 || args.MaxTokens > ExpandMaxTokens {
		return toolResult{}, refusalInvalidf("max_tokens must be between 0 and %d", ExpandMaxTokens)
	}
	var projectID string
	if !args.AllProjects {
		projectID, err = s.optionalProject(ctx, args.Project)
		if err != nil {
			return toolResult{}, err
		}
	}
	since := strings.TrimSpace(args.Since)
	limits := pageLimitsFor("brain_search")
	filter := filterFingerprint(query, projectID, since, strings.Join(args.Kinds, ","), fmt.Sprint(args.IncludeDumps), scope)
	offset, size, err := pageRequest("brain_search", limits, args.Limit, args.Cursor, filter)
	if err != nil {
		return toolResult{}, err
	}
	requested := fetchLimit(offset, size, limits)
	hits, err := s.backend.Search(ctx, SearchInput{
		Query:        query,
		ProjectID:    projectID,
		Limit:        requested,
		MatchMode:    matchMode,
		Scope:        scope,
		Kinds:        args.Kinds,
		IncludeDumps: args.IncludeDumps,
		Since:        since,
	})
	if err != nil {
		return toolResult{}, err
	}
	pageHits, page := paginate(hits, offset, size, requested, "brain_search", filter)
	if pageHits == nil {
		pageHits = []Hit{}
	}
	compact := args.Compact == nil || *args.Compact
	var expanded []ExpandedHit
	switch {
	case len(args.ExpandIDs) > 0:
		expanded, err = s.expandSearchRecords(ctx, pageHits, args.ExpandIDs, args.MaxTokens)
	case args.Expand || !compact:
		expanded, err = s.expandSearchRecords(ctx, pageHits, expandPageIDs(pageHits), args.MaxTokens)
	}
	if err != nil {
		return toolResult{}, err
	}
	if expanded == nil {
		expanded = []ExpandedHit{}
	}
	var text strings.Builder
	if len(pageHits) == 0 {
		text.WriteString("no matches")
	}
	for _, hit := range pageHits {
		fmt.Fprintf(&text, "- [%s] %s (%s, %s)", hit.Kind, hit.Title, hit.ID, hit.ProjectID)
		if hit.Snippet != "" {
			fmt.Fprintf(&text, ": %s", hit.Snippet)
		}
		text.WriteString("\n")
	}
	if footer := pageFooter(page); footer != "" {
		text.WriteString(footer)
	}
	if len(expanded) > 0 {
		text.WriteString("\n\n")
		text.WriteString(expandedText(expanded))
	}
	return toolResult{
		Text: strings.TrimRight(text.String(), "\n"),
		Structured: struct {
			Hits []Hit `json:"hits"`
			Page
			Compact  bool          `json:"compact"`
			Expanded []ExpandedHit `json:"expanded"`
		}{Hits: pageHits, Page: page, Compact: compact, Expanded: expanded},
	}, nil
}

func (s *Server) noteTool() tool {
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	return tool{
		Name:        "brain_note",
		Annotations: appendOnlyAnnotations("Record a note"),
		Description: "Append a dated note to the project's knowledge base. Pass topic_key to evolve one topic: an existing note with that key is updated in place and the previous body is kept under History. " +
			"action=suggest_topic returns the topic key a title would produce without writing anything.",
		InputSchema: objectSchema(nil, map[string]any{
			"action":    enumProp("record appends or evolves a note (default); suggest_topic only computes a topic key.", []string{"record", "suggest_topic"}),
			"text":      stringProp("Note body; required with action=record."),
			"title":     stringProp("Optional short title; defaults to the first line of text."),
			"subtype":   stringProp("Optional engram observation type, for example discovery or capture."),
			"scope":     enumProp("Record scope: project (default), personal or global.", recordScopes),
			"topic_key": stringProp("Stable topic key; a write with an existing key updates that record in place."),
			"source":    stringProp("Optional external id of the item the note came from."),
			"project":   project,
		}),
		OutputSchema: outputObject(map[string]any{
			"action":     stringProp("Action that ran."),
			"id":         stringProp("New or updated record id."),
			"project_id": stringProp("Project that owns the record."),
			"topic_key":  stringProp("Topic key the write landed under."),
			"replaced":   boolProp("True when topic_key updated an existing record in place."),
		}),
		ReadActions: []string{"suggest_topic"},
		run:         s.note,
	}
}

func (s *Server) note(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Action   string `json:"action"`
		Text     string `json:"text"`
		Title    string `json:"title"`
		Subtype  string `json:"subtype"`
		Scope    string `json:"scope"`
		TopicKey string `json:"topic_key"`
		Source   string `json:"source"`
		Project  string `json:"project"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	action := strings.TrimSpace(args.Action)
	if action == "" {
		action = "record"
	}
	scope := strings.TrimSpace(args.Scope)
	if scope != "" && !slices.Contains(recordScopes, scope) {
		return toolResult{}, refusalInvalidf("unknown scope %q (%s)", scope, strings.Join(recordScopes, ", "))
	}
	switch action {
	case "suggest_topic":
		text := strings.TrimSpace(args.Title)
		if text == "" {
			text = strings.TrimSpace(args.Text)
		}
		if text == "" {
			return toolResult{}, refusalInvalid("pass title or text to suggest a topic key")
		}
		key := domain.Slugify(text)
		return toolResult{
			Text:       "topic_key: " + key,
			Structured: noteResult{Action: action, TopicKey: key},
		}, nil
	case "record":
	default:
		return toolResult{}, refusalInvalidf("unknown action %q (record, suggest_topic)", action)
	}
	if _, err := requireText("text", args.Text); err != nil {
		return toolResult{}, err
	}
	target, err := s.writeTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	rec, err := s.backend.Note(ctx, NoteInput{
		ProjectID: target.ID,
		Title:     strings.TrimSpace(args.Title),
		Body:      args.Text,
		Subtype:   strings.TrimSpace(args.Subtype),
		Scope:     scope,
		TopicKey:  strings.TrimSpace(args.TopicKey),
		Source:    strings.TrimSpace(args.Source),
	})
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{
		Text: fmt.Sprintf("note %s recorded in project %s", rec.ID, rec.ProjectID),
		Structured: noteResult{
			Action: action, ID: rec.ID, ProjectID: rec.ProjectID,
			TopicKey: rec.TopicKey, Replaced: rec.Replaced,
		},
	}, nil
}

// noteResult is the structured output of brain_note.
type noteResult struct {
	Action    string `json:"action"`
	ID        string `json:"id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	TopicKey  string `json:"topic_key,omitempty"`
	Replaced  bool   `json:"replaced,omitempty"`
}

func (s *Server) decisionTool() tool {
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	return tool{
		Name:        "brain_decision",
		Annotations: appendOnlyAnnotations("Record a decision"),
		Description: "Record a decision with its context, alternatives and consequences. Pass paths for decisions about specific code so review flags them when that code changes. " +
			"Lifecycle changes are not part of this tool: use brain_judge(action=accept|reject|supersede).",
		InputSchema: objectSchema([]string{"title"}, map[string]any{
			"title":        stringProp("Decision title."),
			"context":      stringProp("Why the decision was needed."),
			"alternatives": stringArrayProp("Options that were considered."),
			"consequences": stringArrayProp("Expected consequences."),
			"paths":        stringArrayProp("Repo-relative files or directories the decision governs (forward slashes, no absolute paths or \"..\")."),
			"status":       stringProp("Refused: lifecycle changes moved to brain_judge in 0.17."),
			"project":      project,
		}),
		OutputSchema: recordOutputSchema(),
		run:          s.decision,
	}
}

func (s *Server) decision(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Title        string   `json:"title"`
		Context      string   `json:"context"`
		Alternatives []string `json:"alternatives"`
		Consequences []string `json:"consequences"`
		Paths        []string `json:"paths"`
		Status       string   `json:"status"`
		Project      string   `json:"project"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	title, err := requireText("title", args.Title)
	if err != nil {
		return toolResult{}, err
	}
	if strings.TrimSpace(args.Status) != "" {
		return toolResult{}, refusalInvalidWith(
			"brain_decision no longer accepts status: a decision is recorded proposed and changes lifecycle through brain_judge",
			"brain_judge(action=accept|reject|supersede) with the new decision id")
	}
	target, err := s.writeTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	rec, err := s.backend.Decision(ctx, DecisionInput{
		ProjectID:    target.ID,
		Title:        title,
		Context:      args.Context,
		Alternatives: args.Alternatives,
		Consequences: args.Consequences,
		Paths:        args.Paths,
	})
	if err != nil {
		return toolResult{}, err
	}
	return recordResult("decision", rec), nil
}

func (s *Server) attemptTool() tool {
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	return tool{
		Name:        "brain_attempt",
		Annotations: appendOnlyAnnotations("Record an attempt"),
		Description: "Record an approach that was tried, with its result and the reason it was kept or abandoned.",
		InputSchema: objectSchema([]string{"title"}, map[string]any{
			"title":      stringProp("What was attempted."),
			"hypothesis": stringProp("What was expected to work."),
			"result":     stringProp("What actually happened."),
			"reason":     stringProp("Why it was abandoned or kept."),
			"project":    project,
		}),
		OutputSchema: recordOutputSchema(),
		run:          s.attempt,
	}
}

func (s *Server) attempt(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Title      string `json:"title"`
		Hypothesis string `json:"hypothesis"`
		Result     string `json:"result"`
		Reason     string `json:"reason"`
		Project    string `json:"project"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	title, err := requireText("title", args.Title)
	if err != nil {
		return toolResult{}, err
	}
	target, err := s.writeTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	rec, err := s.backend.Attempt(ctx, AttemptInput{
		ProjectID:  target.ID,
		Title:      title,
		Hypothesis: args.Hypothesis,
		Result:     args.Result,
		Reason:     args.Reason,
	})
	if err != nil {
		return toolResult{}, err
	}
	return recordResult("attempt", rec), nil
}

func (s *Server) handoffTool() tool {
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	return tool{
		Name:        "brain_handoff",
		Annotations: appendOnlyAnnotations("Record a handoff"),
		Description: "Record a session handoff with a summary and next action. Git context of the project checkout is captured automatically. " +
			"sections adds a named template; pass it instead of summary when the fields are already known.",
		InputSchema: objectSchema(nil, map[string]any{
			"summary": stringProp("What was done and what state it is in. Required unless sections is given."),
			"next":    stringProp("The next action for the following session."),
			"sections": objectProp("Named handoff sections: goal, instructions, discoveries, accomplished, next, files[]. " +
				"Non-empty sections are rendered into the stored summary."),
			"project": project,
		}),
		OutputSchema: outputObject(map[string]any{
			"id":         stringProp("New record id."),
			"project_id": stringProp("Project that owns the record."),
			"git":        stringProp("Git context captured with the handoff."),
		}, "id", "project_id"),
		run: s.handoff,
	}
}

func (s *Server) handoff(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Summary  string `json:"summary"`
		Next     string `json:"next"`
		Sections struct {
			Goal         string   `json:"goal"`
			Instructions string   `json:"instructions"`
			Discoveries  string   `json:"discoveries"`
			Accomplished string   `json:"accomplished"`
			Next         string   `json:"next"`
			Files        []string `json:"files"`
		} `json:"sections"`
		Project string `json:"project"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	sections := HandoffSection{
		Goal: args.Sections.Goal, Instructions: args.Sections.Instructions, Discoveries: args.Sections.Discoveries,
		Accomplished: args.Sections.Accomplished, Next: args.Sections.Next, Files: args.Sections.Files,
	}
	summary := strings.TrimSpace(args.Summary)
	if summary == "" {
		summary = renderHandoffSections(sections)
	}
	if summary == "" {
		return toolResult{}, refusalInvalid("pass summary or at least one section")
	}
	if rendered := renderHandoffSections(sections); rendered != "" && strings.TrimSpace(args.Summary) != "" {
		summary += "\n\n" + rendered
	}
	next := strings.TrimSpace(args.Next)
	if next == "" {
		next = strings.TrimSpace(sections.Next)
	}
	target, err := s.writeTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	rec, err := s.backend.Handoff(ctx, HandoffInput{
		ProjectID:  target.ID,
		Summary:    summary,
		NextAction: next,
		Sections:   sections,
	})
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{
		Text: fmt.Sprintf("handoff %s recorded in project %s", rec.ID, rec.ProjectID),
		Structured: struct {
			ID        string `json:"id"`
			ProjectID string `json:"project_id"`
			Git       string `json:"git,omitempty"`
		}{ID: rec.ID, ProjectID: rec.ProjectID, Git: rec.GitContext},
	}, nil
}

// renderHandoffSections renders the named sections of a handoff in a stable
// order. It returns "" when every part is empty.
func renderHandoffSections(sections HandoffSection) string {
	var lines []string
	write := func(label, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		lines = append(lines, "## "+label+"\n"+strings.TrimSpace(value))
	}
	write("Goal", sections.Goal)
	write("Instructions", sections.Instructions)
	write("Discoveries", sections.Discoveries)
	write("Accomplished", sections.Accomplished)
	write("Next", sections.Next)
	if files := nonEmptyStrings(sections.Files); len(files) > 0 {
		lines = append(lines, "## Files\n- "+strings.Join(files, "\n- "))
	}
	return strings.Join(lines, "\n\n")
}

func nonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func (s *Server) projectsTool() tool {
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	return tool{
		Name:        "brain_projects",
		Annotations: readOnlyAnnotations("List projects"),
		Description: "List registered projects with their kind, checkout paths and repository URL; current resolves the project of the working directory (or a selector) and says how it matched. " +
			"register adds or updates a registry entry; merge is not available.",
		InputSchema: objectSchema(nil, map[string]any{
			"action":  enumProp("list (default), current, register or merge.", []string{"list", "current", "register", "merge"}),
			"project": project,
			"from":    stringProp("Project id to register (action=register) or the source project of a merge."),
			"to":      stringProp("Target project of a merge."),
			"path":    stringProp("Checkout path to register (action=register)."),
			"alias":   stringArrayProp("Aliases to register (action=register)."),
		}),
		OutputSchema: outputObject(map[string]any{
			"projects": arrayProp("Registered projects.", outputObject(map[string]any{
				"id":       stringProp("Project id."),
				"name":     stringProp("Display name."),
				"kind":     stringProp("Project kind, for example repository."),
				"paths":    arrayProp("Checkout paths.", stringProp("Path.")),
				"repo_url": stringProp("Repository URL, when known."),
			}, "id", "name", "kind", "paths", "repo_url")),
			"current":    objectProp("Resolved project: id, source (cwd, id, alias, path) and path."),
			"registered": objectProp("Project that was registered."),
		}),
		ReadActions: []string{"list", "current"},
		run:         s.projects,
	}
}

func (s *Server) projects(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Action  string   `json:"action"`
		Project string   `json:"project"`
		From    string   `json:"from"`
		To      string   `json:"to"`
		Path    string   `json:"path"`
		Alias   []string `json:"alias"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	action := strings.TrimSpace(args.Action)
	if action == "" {
		action = "list"
	}
	switch action {
	case "list":
		return s.projectsList(ctx)
	case "current":
		target, err := s.resolveTarget(ctx, strings.TrimSpace(args.Project))
		if err != nil {
			return toolResult{}, err
		}
		current := struct {
			ID     string `json:"id"`
			Source string `json:"source"`
			Path   string `json:"path,omitempty"`
		}{ID: target.ID, Source: target.Source, Path: target.Path}
		return toolResult{
			Text: fmt.Sprintf("current project: %s (%s)%s", target.ID, orUnknown(target.Source), pathSuffix(target.Path)),
			Structured: struct {
				Current any `json:"current"`
			}{Current: current},
		}, nil
	case "register":
		return s.registerProject(ctx, args.From, args.Path, args.Alias)
	case "merge":
		return toolResult{}, refusedGap("brain_projects", "merge")
	default:
		return toolResult{}, refusalInvalidf("unknown action %q (list, current, register, merge)", action)
	}
}

func (s *Server) projectsList(ctx context.Context) (toolResult, error) {
	projects, err := s.backend.Projects(ctx)
	if err != nil {
		return toolResult{}, err
	}
	projects = s.policy.filterProjects(projects)
	if projects == nil {
		projects = []Project{}
	}
	var text strings.Builder
	if len(projects) == 0 {
		text.WriteString("no projects registered")
	}
	for _, project := range projects {
		fmt.Fprintf(&text, "- %s (%s): %s", project.ID, project.Kind, project.Name)
		if len(project.Paths) > 0 {
			fmt.Fprintf(&text, " [%s]", strings.Join(project.Paths, ", "))
		}
		text.WriteString("\n")
	}
	return toolResult{
		Text: strings.TrimRight(text.String(), "\n"),
		Structured: struct {
			Projects []Project `json:"projects"`
		}{Projects: projects},
	}, nil
}

func (s *Server) registerProject(ctx context.Context, id, path string, aliases []string) (toolResult, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return toolResult{}, refusalInvalid("from is required with action=register: it is the project id to register")
	}
	if err := s.policy.checkWrite(id); err != nil {
		return toolResult{}, err
	}
	backend, ok := s.backend.(ProjectBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_projects(action=register)", "brain project set "+id+" --path <path> --alias <alias>")
	}
	project, err := backend.RegisterProject(ctx, ProjectInput{ID: id, Paths: nonEmptyStrings([]string{path}), Aliases: nonEmptyStrings(aliases)})
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{
		Text: "project " + project.ID + " registered",
		Structured: struct {
			Registered Project `json:"registered"`
		}{Registered: project},
	}, nil
}

func orUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "source not reported"
	}
	return value
}

func pathSuffix(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	return " at " + path
}

func (s *Server) capabilitiesTool() tool {
	return tool{
		Name:        "brain_capabilities",
		Annotations: readOnlyAnnotations("Inspect the contract"),
		Description: "Report the active profile, engine version, tool counts, features, limits, known gaps, refusal codes and endpoints. It performs no reads of knowledge records and has no side effects.",
		InputSchema: objectSchema(nil, map[string]any{
			"verbose": boolProp("Include one detail row per tool."),
		}),
		OutputSchema: outputObject(map[string]any{
			"profile":         stringProp("Active profile name."),
			"profile_version": stringProp("Profile revision, major.minor."),
			"engine":          stringProp("Engine version."),
			"policy":          objectProp("Effective policy: read_only, allow_write, projects, all_projects."),
			"tools":           objectProp("Tool counts and names."),
			"features":        objectProp("Feature flags and approximations."),
			"limits":          objectProp("Numeric limits."),
			"gaps":            arrayProp("Actions this profile cannot serve, with the CLI path that can.", objectProp("One gap: tool, action, reason, remediation.")),
			"refusal_codes":   arrayProp("Closed refusal vocabulary.", stringProp("One refusal code.")),
			"endpoints":       objectProp("Transport endpoints."),
			"schemas":         objectProp("Record and refusal schema versions."),
			"tools_detail":    arrayProp("Per-tool rows, with verbose=true.", objectProp("One tool: name, policy, read_actions.")),
		}, "profile", "profile_version", "engine", "tools", "refusal_codes"),
		run: s.capabilitiesToolRun,
	}
}

func (s *Server) capabilitiesToolRun(_ context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Verbose bool `json:"verbose"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	capabilities := s.capabilities()
	payload := struct {
		Capabilities
		ToolsDetail []toolDetail `json:"tools_detail,omitempty"`
	}{Capabilities: capabilities}
	if args.Verbose {
		for _, t := range s.tools {
			payload.ToolsDetail = append(payload.ToolsDetail, toolDetail{
				Name: t.Name, Policy: toolPolicy(t), ReadActions: t.ReadActions,
				Legacy: t.Legacy, Target: t.Target, Grade: t.Grade, Stub: t.Stub,
			})
		}
	}
	var text strings.Builder
	fmt.Fprintf(&text, "profile: %s %s\nengine: %s\ntools: %d (%d read, %d append, %d destructive)\n",
		capabilities.Profile, capabilities.ProfileVersion, capabilities.Engine,
		capabilities.Tools.Total, capabilities.Tools.ReadOnly, capabilities.Tools.AppendOnly, capabilities.Tools.Destructive)
	fmt.Fprintf(&text, "gaps: %d\ntry `brain mcp --list-tools --json` for the full schemas", len(capabilities.Gaps))
	return toolResult{Text: text.String(), Structured: payload}, nil
}

// toolDetail is one row of the verbose capabilities listing.
type toolDetail struct {
	Name        string   `json:"name"`
	Policy      string   `json:"policy"`
	Legacy      bool     `json:"legacy,omitempty"`
	Target      string   `json:"target,omitempty"`
	Grade       string   `json:"grade,omitempty"`
	Stub        bool     `json:"stub,omitempty"`
	ReadActions []string `json:"read_actions,omitempty"`
}

func recordResult(kind string, rec Record) toolResult {
	return toolResult{
		Text: fmt.Sprintf("%s %s recorded in project %s", kind, rec.ID, rec.ProjectID),
		Structured: struct {
			ID        string `json:"id"`
			ProjectID string `json:"project_id"`
		}{ID: rec.ID, ProjectID: rec.ProjectID},
	}
}

// decodeArgs decodes tool arguments strictly: unknown fields are rejected, as
// the schema sets additionalProperties to false. Absent arguments mean {}.
func decodeArgs(raw json.RawMessage, dst any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		trimmed = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return refusalInvalid("invalid arguments: " + err.Error())
	}
	if dec.More() {
		return refusalInvalid("invalid arguments: unexpected data after the object")
	}
	return nil
}

func requireText(field, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", refusalInvalid(field + " is required")
	}
	return trimmed, nil
}

// optionalProject resolves an optional project selector; an empty selector
// means "every project" when the policy has no allowlist, the only allowed
// project when it has one, and a refusal when it has several.
func (s *Server) optionalProject(ctx context.Context, selector string) (string, error) {
	if strings.TrimSpace(selector) == "" {
		return s.policy.readProject()
	}
	target, err := s.resolveTarget(ctx, strings.TrimSpace(selector))
	if err != nil {
		return "", err
	}
	return target.ID, nil
}

// scopedReadProject resolves an explicit read scope: a project selector or
// all=true for every project. Unlike optionalProject it never widens or
// narrows to "every project" silently, so a paginated stream cannot mix
// projects the cursor does not describe.
func (s *Server) scopedReadProject(ctx context.Context, selector string, all bool, what string) (string, error) {
	selector = strings.TrimSpace(selector)
	if selector != "" && all {
		return "", refusalInvalid("pass either project or all=true, not both")
	}
	if all {
		if len(s.policy.Projects) > 0 {
			return "", refusalInvalidf("all=true is not available: this server is limited to %s", s.policy.projectList())
		}
		return "", nil
	}
	if selector == "" {
		return "", refusalInvalid("project is required: pass project or all=true to read " + what)
	}
	target, err := s.resolveTarget(ctx, selector)
	if err != nil {
		return "", err
	}
	return target.ID, nil
}

// resolveTarget resolves a selector and enforces the project allowlist.
func (s *Server) resolveTarget(ctx context.Context, selector string) (Target, error) {
	target, err := s.backend.Resolve(ctx, selector)
	if err != nil {
		return Target{}, err
	}
	if err := s.policy.checkProject(target.ID); err != nil {
		return Target{}, err
	}
	return target, nil
}

// writeTarget resolves a write destination and enforces the write scope.
func (s *Server) writeTarget(ctx context.Context, selector string) (Target, error) {
	target, err := s.resolveTarget(ctx, selector)
	if err != nil {
		return Target{}, err
	}
	if err := s.policy.checkWrite(target.ID); err != nil {
		return Target{}, err
	}
	return target, nil
}

func objectSchema(required []string, properties map[string]any) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// outputObject is the JSON Schema of a structured tool result. Unlike an input
// schema it does not forbid extra properties: a client validates what the tool
// promises, and the server may add fields without a protocol bump.
func outputObject(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// pageOutput merges the pagination fields into a list tool's output properties.
func pageOutput(properties map[string]any) map[string]any {
	out := pageOutputProps()
	for key, value := range properties {
		out[key] = value
	}
	return out
}

// recordOutputSchema is the structured output of the record-append tools.
func recordOutputSchema() map[string]any {
	return outputObject(map[string]any{
		"id":         stringProp("New record id."),
		"project_id": stringProp("Project that owns the record."),
	}, "id", "project_id")
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func stringArrayProp(description string) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": description,
		"items":       map[string]any{"type": "string"},
	}
}

func enumArrayProp(description string, values []string) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": description,
		"items":       map[string]any{"type": "string", "enum": values},
	}
}

func intProp(description string, minimum int) map[string]any {
	return map[string]any{"type": "integer", "description": description, "minimum": minimum}
}

func numberProp(description string) map[string]any {
	return map[string]any{"type": "number", "description": description}
}

func boolProp(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func enumProp(description string, values []string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}

// arrayProp is an array property with described items.
func arrayProp(description string, items map[string]any) map[string]any {
	return map[string]any{"type": "array", "description": description, "items": items}
}

// objectProp is an object property with fixed keys described by the caller.
func objectProp(description string) map[string]any {
	return map[string]any{"type": "object", "description": description}
}

// anyProp is a property whose type the schema does not fix.
func anyProp(description string) map[string]any {
	return map[string]any{"description": description}
}
