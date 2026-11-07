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
)

// DefaultJournalLimit is used by brain_journal when no limit is given.
const DefaultJournalLimit = 20

var decisionStatuses = []string{"proposed", "accepted", "rejected", "superseded"}

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
	// selects the default size.
	Bootstrap(ctx context.Context, projectID string, budget int) (string, error)
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

type tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	run         toolFunc
}

func (s *Server) toolList() []tool {
	return slices.Concat(s.coreTools(), s.stateTools(), s.catchupTools(), s.reviewTools(), s.recordTools())
}

func (s *Server) coreTools() []tool {
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	return []tool{
		{
			Name:        "brain_open",
			Description: "Load the curated context of a project: state, recent decisions, attempts and the latest handoff. Call at the start of a session.",
			InputSchema: objectSchema(nil, map[string]any{
				"project": project,
				"budget":  intProp("Summary size budget in runes; 0 selects the default.", 0),
			}),
			run: s.open,
		},
		{
			Name:        "brain_search",
			Description: "Full-text search over notes, decisions, attempts and handoffs. Searches every project unless project is given. Connector dumps (mail, GitHub/GitLab issues and PRs) are excluded unless include_dumps is true or kinds contains \"dump\".",
			InputSchema: objectSchema([]string{"query"}, map[string]any{
				"query":         stringProp("Words to search for."),
				"project":       project,
				"limit":         intProp("Maximum hits; 0 selects the default.", 0),
				"kinds":         stringArrayProp("Keep only these kinds: note, decision, attempt, handoff, dump. Omit for every kind."),
				"include_dumps": boolProp("Include connector dumps in an unfiltered search."),
			}),
			run: s.search,
		},
		{
			Name:        "brain_note",
			Description: "Append a dated note to the project's knowledge base.",
			InputSchema: objectSchema([]string{"text"}, map[string]any{
				"text":    stringProp("Note body."),
				"title":   stringProp("Optional short title; defaults to the first line of text."),
				"project": project,
			}),
			run: s.note,
		},
		{
			Name:        "brain_decision",
			Description: "Record a decision with its context, alternatives and consequences.",
			InputSchema: objectSchema([]string{"title"}, map[string]any{
				"title":        stringProp("Decision title."),
				"context":      stringProp("Why the decision was needed."),
				"alternatives": stringArrayProp("Options that were considered."),
				"consequences": stringArrayProp("Expected consequences."),
				"status":       enumProp("Lifecycle status.", decisionStatuses),
				"project":      project,
			}),
			run: s.decision,
		},
		{
			Name:        "brain_attempt",
			Description: "Record an approach that was tried, with its result and the reason it was kept or abandoned.",
			InputSchema: objectSchema([]string{"title"}, map[string]any{
				"title":      stringProp("What was attempted."),
				"hypothesis": stringProp("What was expected to work."),
				"result":     stringProp("What actually happened."),
				"reason":     stringProp("Why it was abandoned or kept."),
				"project":    project,
			}),
			run: s.attempt,
		},
		{
			Name:        "brain_handoff",
			Description: "Record a session handoff with a summary and next action. Git context of the project checkout is captured automatically.",
			InputSchema: objectSchema([]string{"summary"}, map[string]any{
				"summary": stringProp("What was done and what state it is in."),
				"next":    stringProp("The next action for the following session."),
				"project": project,
			}),
			run: s.handoff,
		},
		{
			Name:        "brain_journal",
			Description: "List recent activity (notes, decisions, attempts, handoffs, commits), newest first.",
			InputSchema: objectSchema(nil, map[string]any{
				"project": project,
				"limit":   intProp("Maximum entries; 0 selects the default of 20.", 0),
			}),
			run: s.journal,
		},
		{
			Name:        "brain_projects",
			Description: "List registered projects with their kind, checkout paths and repository URL.",
			InputSchema: objectSchema(nil, map[string]any{}),
			run:         s.projects,
		},
	}
}

func (s *Server) open(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Project string `json:"project"`
		Budget  int    `json:"budget"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	if args.Budget < 0 {
		return toolResult{}, errors.New("budget must not be negative")
	}
	target, err := s.backend.Resolve(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	summary, err := s.backend.Bootstrap(ctx, target.ID, args.Budget)
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
		Kinds        []string `json:"kinds"`
		IncludeDumps bool     `json:"include_dumps"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	query, err := requireText("query", args.Query)
	if err != nil {
		return toolResult{}, err
	}
	if args.Limit < 0 {
		return toolResult{}, errors.New("limit must not be negative")
	}
	projectID, err := s.optionalProject(ctx, args.Project)
	if err != nil {
		return toolResult{}, err
	}
	hits, err := s.backend.Search(ctx, SearchInput{
		Query:        query,
		ProjectID:    projectID,
		Limit:        args.Limit,
		Kinds:        args.Kinds,
		IncludeDumps: args.IncludeDumps,
	})
	if err != nil {
		return toolResult{}, err
	}
	if hits == nil {
		hits = []Hit{}
	}
	var text strings.Builder
	if len(hits) == 0 {
		text.WriteString("no matches")
	}
	for _, hit := range hits {
		fmt.Fprintf(&text, "- [%s] %s (%s, %s)", hit.Kind, hit.Title, hit.ID, hit.ProjectID)
		if hit.Snippet != "" {
			fmt.Fprintf(&text, ": %s", hit.Snippet)
		}
		text.WriteString("\n")
	}
	return toolResult{
		Text: strings.TrimRight(text.String(), "\n"),
		Structured: struct {
			Hits []Hit `json:"hits"`
		}{Hits: hits},
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
	target, err := s.backend.Resolve(ctx, strings.TrimSpace(args.Project))
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
		return toolResult{}, fmt.Errorf("status must be one of %s", strings.Join(decisionStatuses, ", "))
	}
	target, err := s.backend.Resolve(ctx, strings.TrimSpace(args.Project))
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
	target, err := s.backend.Resolve(ctx, strings.TrimSpace(args.Project))
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
	target, err := s.backend.Resolve(ctx, strings.TrimSpace(args.Project))
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
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	if args.Limit < 0 {
		return toolResult{}, errors.New("limit must not be negative")
	}
	if args.Limit == 0 {
		args.Limit = DefaultJournalLimit
	}
	projectID, err := s.optionalProject(ctx, args.Project)
	if err != nil {
		return toolResult{}, err
	}
	entries, err := s.backend.Journal(ctx, JournalQuery{ProjectID: projectID, Limit: args.Limit})
	if err != nil {
		return toolResult{}, err
	}
	if entries == nil {
		entries = []JournalEntry{}
	}
	var text strings.Builder
	if len(entries) == 0 {
		text.WriteString("no entries")
	}
	for _, entry := range entries {
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
	return toolResult{
		Text: strings.TrimRight(text.String(), "\n"),
		Structured: struct {
			Entries []JournalEntry `json:"entries"`
		}{Entries: entries},
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

// optionalProject resolves an optional project selector; "" stays "" (all
// projects).
func (s *Server) optionalProject(ctx context.Context, selector string) (string, error) {
	if strings.TrimSpace(selector) == "" {
		return "", nil
	}
	target, err := s.backend.Resolve(ctx, strings.TrimSpace(selector))
	if err != nil {
		return "", err
	}
	return target.ID, nil
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
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if dec.More() {
		return errors.New("invalid arguments: unexpected data after the object")
	}
	return nil
}

func requireText(field, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%s is required", field)
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

func boolProp(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func enumProp(description string, values []string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}
