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

	"github.com/levapveeskela/lv24-brain/internal/textbudget"
)

// DefaultJournalLimit is used by brain_journal when no limit is given.
const DefaultJournalLimit = 20

var decisionStatuses = []string{"proposed", "accepted", "rejected", "superseded"}

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
}

// Record identifies a stored knowledge record.
type Record struct {
	ID        string
	ProjectID string
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

// HandoffInput is a handoff to append. The backend captures git context for
// the project checkout itself.
type HandoffInput struct {
	ProjectID  string
	Summary    string
	NextAction string
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
// JSON object.
type toolResult struct {
	Text       string
	Structured any
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
	// confirm returns the question to ask the user before the call runs, or
	// nil when the call needs no confirmation. It is consulted only for 2026
	// clients that declared elicitation (see mrtr.go).
	confirm func(args json.RawMessage) *confirmation
	run     toolFunc
}

func (s *Server) toolList() []tool {
	core := s.coreTools()
	// brain_context sits right after brain_open: both load context.
	return slices.Concat(core[:1], s.contextTools(), core[1:], s.stateTools(), s.catchupTools(), s.reviewTools(), s.recordTools())
}

func (s *Server) coreTools() []tool {
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	return []tool{
		{
			Name:        "brain_open",
			Annotations: readOnlyAnnotations("Open project context"),
			Description: "Load the curated context of a project: state, recent decisions, attempts and the latest handoff. Call at the start of a session.",
			InputSchema: objectSchema(nil, map[string]any{
				"project":            project,
				"budget":             intProp("Summary size budget in runes; 0 selects the default.", 0),
				"include_unreviewed": boolProp("Include quarantined records (unreviewed agent, connector or import writes) that are excluded by default."),
			}),
			OutputSchema: outputObject(map[string]any{
				"workspace":  boolProp("True when the selector resolved to a workspace root; overview then holds its projects."),
				"overview":   stringProp("Workspace overview text, in workspace mode."),
				"project_id": stringProp("Resolved project id, in project mode."),
				"path":       stringProp("Checkout path the project resolved through."),
				"summary":    stringProp("Bounded context summary, in project mode."),
			}),
			run: s.open,
		},
		{
			Name:        "brain_search",
			Annotations: readOnlyAnnotations("Search the brain"),
			Description: "Full-text search over notes, decisions, attempts and handoffs. Searches every project unless project is given. Connector dumps (mail, GitHub/GitLab issues and PRs) are excluded unless include_dumps is true or kinds contains \"dump\". Raw-zone external items are excluded unless kinds contains \"item\". " +
				"Hits carry an id, the kind, the title, a preview of at most " + fmt.Sprint(textbudget.PreviewRunes) + " characters and the ranking score; bodies are never returned by default. Read one with expand_ids, or set compact=false to expand the whole page.",
			InputSchema: objectSchema([]string{"query"}, map[string]any{
				"query":         stringProp("Words to search for."),
				"project":       project,
				"limit":         intProp("Page size; 0 selects the default page of 20.", 0),
				"cursor":        cursorProp(),
				"kinds":         stringArrayProp("Keep only these kinds: note, decision, attempt, handoff, dump, item. Omit for curated kinds; raw-zone dumps and external items require an explicit request."),
				"include_dumps": boolProp("Include connector dumps in an unfiltered search."),
				"since":         stringProp("Only documents updated since: a duration (24h, 7d), a date (2026-11-01) or an RFC3339 time. Omit for all time."),
				"compact":       boolProp("Return previews only (the default). Set false to return the whole body of every hit on the page; prefer expand_ids for one record."),
				"expand":        boolProp("Expand every hit on this page to its full record. Prefer expand_ids when only one or two records matter."),
				"expand_ids":    stringArrayProp("Ids from the hits of this call to return in full in the same response. At most " + fmt.Sprint(ExpandMaxRecords) + " ids."),
				"max_tokens":    intProp("Token budget per expanded record; 0 selects the default of "+fmt.Sprint(ExpandDefaultMaxTokens)+", the maximum is "+fmt.Sprint(ExpandMaxTokens)+".", 0),
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
			run:    s.search,
		},
		{
			Name:        "brain_note",
			Annotations: appendOnlyAnnotations("Record a note"),
			Description: "Append a dated note to the project's knowledge base.",
			InputSchema: objectSchema([]string{"text"}, map[string]any{
				"text":    stringProp("Note body."),
				"title":   stringProp("Optional short title; defaults to the first line of text."),
				"project": project,
			}),
			OutputSchema: recordOutputSchema(),
			run:          s.note,
		},
		{
			Name:        "brain_decision",
			Annotations: appendOnlyAnnotations("Record a decision"),
			Description: "Record a decision with its context, alternatives and consequences. Pass paths for decisions about specific code so review flags them when that code changes.",
			InputSchema: objectSchema([]string{"title"}, map[string]any{
				"title":        stringProp("Decision title."),
				"context":      stringProp("Why the decision was needed."),
				"alternatives": stringArrayProp("Options that were considered."),
				"consequences": stringArrayProp("Expected consequences."),
				"paths":        stringArrayProp("Repo-relative files or directories the decision governs (forward slashes, no absolute paths or \"..\")."),
				"status":       enumProp("Lifecycle status.", decisionStatuses),
				"project":      project,
			}),
			OutputSchema: recordOutputSchema(),
			run:          s.decision,
		},
		{
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
		},
		{
			Name:        "brain_handoff",
			Annotations: appendOnlyAnnotations("Record a handoff"),
			Description: "Record a session handoff with a summary and next action. Git context of the project checkout is captured automatically.",
			InputSchema: objectSchema([]string{"summary"}, map[string]any{
				"summary": stringProp("What was done and what state it is in."),
				"next":    stringProp("The next action for the following session."),
				"project": project,
			}),
			OutputSchema: recordOutputSchema(),
			run:          s.handoff,
		},
		{
			Name:        "brain_journal",
			Annotations: readOnlyAnnotations("Read the activity journal"),
			Description: "List recent activity (notes, decisions, attempts, handoffs, commits), newest first.",
			InputSchema: objectSchema(nil, map[string]any{
				"project": project,
				"limit":   intProp("Page size; 0 selects the default page of 20.", 0),
				"cursor":  cursorProp(),
			}),
			OutputSchema: outputObject(pageOutput(map[string]any{
				"entries": arrayProp("Journal entries, newest first.", outputObject(map[string]any{
					"time":    stringProp("RFC3339 time of the event."),
					"user":    stringProp("User or harness that recorded it."),
					"event":   stringProp("Event name, for example note or commit."),
					"project": stringProp("Project the event belongs to."),
					"id":      stringProp("Record id, when the event has one."),
					"title":   stringProp("Human summary of the event."),
					"fields":  anyProp("Event-specific fields."),
				}, "time", "user", "event")),
			}), "entries", "page", "page_size", "has_more"),
			Limits: limitsOf(pageLimitsFor("brain_journal")),
			run:    s.journal,
		},
		{
			Name:        "brain_projects",
			Annotations: readOnlyAnnotations("List projects"),
			Description: "List registered projects with their kind, checkout paths and repository URL.",
			InputSchema: objectSchema(nil, map[string]any{}),
			OutputSchema: outputObject(map[string]any{
				"projects": arrayProp("Registered projects.", outputObject(map[string]any{
					"id":       stringProp("Project id."),
					"name":     stringProp("Display name."),
					"kind":     stringProp("Project kind, for example repository."),
					"paths":    arrayProp("Checkout paths.", stringProp("Path.")),
					"repo_url": stringProp("Repository URL, when known."),
				}, "id", "name", "kind", "paths", "repo_url")),
			}, "projects"),
			run: s.projects,
		},
	}
}

func (s *Server) open(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Project           string `json:"project"`
		Budget            int    `json:"budget"`
		IncludeUnreviewed bool   `json:"include_unreviewed"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	if args.Budget < 0 {
		return toolResult{}, refusalInvalid("budget must not be negative")
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
			Text: overview.Text,
			Structured: struct {
				Workspace bool   `json:"workspace"`
				Overview  string `json:"overview"`
			}{Workspace: true, Overview: overview.Text},
		}, nil
	}
	if err != nil {
		return toolResult{}, err
	}
	summary, err := s.backend.Bootstrap(ctx, target.ID, args.Budget, args.IncludeUnreviewed)
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{
		Text: summary,
		Structured: struct {
			ProjectID string `json:"project_id"`
			Path      string `json:"path"`
			Summary   string `json:"summary"`
		}{ProjectID: target.ID, Path: target.Path, Summary: summary},
	}, nil
}

func (s *Server) search(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Query        string   `json:"query"`
		Project      string   `json:"project"`
		Limit        int      `json:"limit"`
		Cursor       string   `json:"cursor"`
		Kinds        []string `json:"kinds"`
		IncludeDumps bool     `json:"include_dumps"`
		Since        string   `json:"since"`
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
	if len(args.ExpandIDs) > ExpandMaxRecords {
		return toolResult{}, refusalInvalidf("expand_ids holds %d ids; the limit is %d", len(args.ExpandIDs), ExpandMaxRecords)
	}
	if args.MaxTokens < 0 || args.MaxTokens > ExpandMaxTokens {
		return toolResult{}, refusalInvalidf("max_tokens must be between 0 and %d", ExpandMaxTokens)
	}
	projectID, err := s.optionalProject(ctx, args.Project)
	if err != nil {
		return toolResult{}, err
	}
	since := strings.TrimSpace(args.Since)
	limits := pageLimitsFor("brain_search")
	filter := filterFingerprint(query, projectID, since, strings.Join(args.Kinds, ","), fmt.Sprint(args.IncludeDumps))
	offset, size, err := pageRequest("brain_search", limits, args.Limit, args.Cursor, filter)
	if err != nil {
		return toolResult{}, err
	}
	requested := fetchLimit(offset, size, limits)
	hits, err := s.backend.Search(ctx, SearchInput{
		Query:        query,
		ProjectID:    projectID,
		Limit:        requested,
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

func (s *Server) note(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Text    string `json:"text"`
		Title   string `json:"title"`
		Project string `json:"project"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
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
	})
	if err != nil {
		return toolResult{}, err
	}
	return recordResult("note", rec), nil
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
	if args.Status != "" && !slices.Contains(decisionStatuses, args.Status) {
		return toolResult{}, refusalInvalidf("status must be one of %s", strings.Join(decisionStatuses, ", "))
	}
	target, err := s.writeTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	rec, err := s.backend.Decision(ctx, DecisionInput{
		ProjectID:    target.ID,
		Title:        title,
		Status:       args.Status,
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

func (s *Server) handoff(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Summary string `json:"summary"`
		Next    string `json:"next"`
		Project string `json:"project"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	summary, err := requireText("summary", args.Summary)
	if err != nil {
		return toolResult{}, err
	}
	target, err := s.writeTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	rec, err := s.backend.Handoff(ctx, HandoffInput{
		ProjectID:  target.ID,
		Summary:    summary,
		NextAction: strings.TrimSpace(args.Next),
	})
	if err != nil {
		return toolResult{}, err
	}
	return recordResult("handoff", rec), nil
}

func (s *Server) journal(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Project string `json:"project"`
		Limit   int    `json:"limit"`
		Cursor  string `json:"cursor"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	projectID, err := s.optionalProject(ctx, args.Project)
	if err != nil {
		return toolResult{}, err
	}
	limits := pageLimitsFor("brain_journal")
	filter := filterFingerprint(projectID)
	offset, size, err := pageRequest("brain_journal", limits, args.Limit, args.Cursor, filter)
	if err != nil {
		return toolResult{}, err
	}
	requested := fetchLimit(offset, size, limits)
	entries, err := s.backend.Journal(ctx, JournalQuery{ProjectID: projectID, Limit: requested})
	if err != nil {
		return toolResult{}, err
	}
	pageEntries, page := paginate(entries, offset, size, requested, "brain_journal", filter)
	if pageEntries == nil {
		pageEntries = []JournalEntry{}
	}
	var text strings.Builder
	if len(pageEntries) == 0 {
		text.WriteString("no entries")
	}
	for _, entry := range pageEntries {
		line := fmt.Sprintf("%s  %s  %s", entry.Time.UTC().Format(time.RFC3339), entry.User, entry.Event)
		if entry.Project != "" {
			line += "  " + entry.Project
		}
		switch {
		case entry.Title != "":
			line += "  " + entry.Title
		case entry.ID != "":
			line += "  " + entry.ID
		}
		text.WriteString(line + "\n")
	}
	if footer := pageFooter(page); footer != "" {
		text.WriteString(footer)
	}
	return toolResult{
		Text: strings.TrimRight(text.String(), "\n"),
		Structured: struct {
			Entries []JournalEntry `json:"entries"`
			Page
		}{Entries: pageEntries, Page: page},
	}, nil
}

func (s *Server) projects(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct{}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
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
