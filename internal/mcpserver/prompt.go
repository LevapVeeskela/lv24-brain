package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Prompt zone limits.
const (
	promptDefaultPageSize = 20
	promptMaxPageSize     = 50
)

// PromptInput is one prompt to save in the local-only prompt zone.
type PromptInput struct {
	ProjectID string
	Text      string
	Session   string
}

// PromptQuery selects prompts to list. An empty ProjectID lists every
// registered project.
type PromptQuery struct {
	ProjectID string
	Limit     int
}

// PromptHit is one stored prompt as the MCP tool reports it.
type PromptHit struct {
	ProjectID string    `json:"project_id"`
	At        time.Time `json:"at"`
	Session   string    `json:"session,omitempty"`
	Text      string    `json:"text"`
	Path      string    `json:"path,omitempty"`
}

// PromptBackend is implemented by backends with a local prompt zone.
type PromptBackend interface {
	// PromptsEnabled reports the prompts.enabled opt-in.
	PromptsEnabled() bool
	SavePrompt(ctx context.Context, in PromptInput) (PromptHit, error)
	ListPrompts(ctx context.Context, query PromptQuery) ([]PromptHit, error)
}

func (s *Server) promptTool() tool {
	project := stringProp("Project id, alias or checkout path. action=save defaults to the working directory's project; action=search requires project or all=true.")
	return tool{
		Name:        "brain_prompt",
		Annotations: unrepeatedDestructiveAnnotations("Local-only prompt zone"),
		Description: "Store a user prompt in the opt-in local prompt zone (action=save), search stored prompts (action=search) or discard one (action=discard). " +
			"action=search requires project or all=true. " +
			"Prompts are machine-local: they are never committed, indexed, exposed as resources or returned by brain_search. " +
			"When prompts.enabled is false every action is refused.",
		InputSchema: objectSchema(nil, map[string]any{
			"action":  enumProp("save, search or discard.", []string{"save", "search", "discard"}),
			"content": stringProp("Prompt text (action=save)."),
			"query":   stringProp("Case-insensitive words to keep (action=search); omit for the newest prompts."),
			"session": stringProp("Optional session id recorded with the prompt (action=save)."),
			"project": project,
			"all":     boolProp("action=search only: read stored prompts of every project. Required when project is omitted; mutually exclusive with project."),
			"limit":   intProp("Maximum prompts returned; 0 selects the default of 20, the maximum is 50.", 0),
		}),
		OutputSchema: outputObject(map[string]any{
			"action":     stringProp("Action that ran."),
			"id":         stringProp("Opaque prompt handle (action=save)."),
			"project_id": stringProp("Project the prompt belongs to."),
			"at":         stringProp("RFC3339 time the prompt was stored."),
			"hits":       arrayProp("Matching prompts, newest first (action=search).", objectProp("One prompt: project_id, at, session, text, path.")),
		}, "action"),
		ReadActions: []string{"search"},
		run:         s.prompt,
	}
}

// promptArgs is one brain_prompt request.
type promptArgs struct {
	Action  string `json:"action"`
	Content string `json:"content"`
	Query   string `json:"query"`
	Session string `json:"session"`
	Project string `json:"project"`
	All     bool   `json:"all"`
	Limit   int    `json:"limit"`
}

func (s *Server) prompt(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args promptArgs
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	action := strings.TrimSpace(args.Action)
	switch action {
	case "save", "search", "discard":
	default:
		return toolResult{}, refusalInvalidf("unknown action %q (save, search, discard)", action)
	}
	if args.All && action != "search" {
		return toolResult{}, refusalInvalid("all is only available with action=search")
	}
	backend, ok := s.backend.(PromptBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_prompt", "brain prompt save|list in a shell")
	}
	if !backend.PromptsEnabled() {
		return toolResult{}, refusalUnsupported(
			"the local prompt zone is disabled (config.prompts.enabled is false)",
			"set prompts.enabled: true in brain.yaml and restart brain mcp")
	}
	switch action {
	case "save":
		return s.savePrompt(ctx, backend, args)
	case "search":
		return s.searchPrompts(ctx, backend, args)
	default:
		return toolResult{}, refusedGap("brain_prompt", "discard")
	}
}

func (s *Server) savePrompt(ctx context.Context, backend PromptBackend, args promptArgs) (toolResult, error) {
	content, err := requireText("content", args.Content)
	if err != nil {
		return toolResult{}, err
	}
	target, err := s.writeTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	hit, err := backend.SavePrompt(ctx, PromptInput{ProjectID: target.ID, Text: content, Session: strings.TrimSpace(args.Session)})
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{
		Text: fmt.Sprintf("prompt saved locally in project %s at %s", hit.ProjectID, hit.At.UTC().Format(time.RFC3339)),
		Structured: struct {
			Action    string `json:"action"`
			ID        string `json:"id"`
			ProjectID string `json:"project_id"`
			At        string `json:"at"`
		}{Action: "save", ID: promptHandle(hit), ProjectID: hit.ProjectID, At: hit.At.UTC().Format(time.RFC3339)},
	}, nil
}

func (s *Server) searchPrompts(ctx context.Context, backend PromptBackend, args promptArgs) (toolResult, error) {
	if args.Limit < 0 {
		return toolResult{}, refusalInvalid("limit must not be negative")
	}
	limit := args.Limit
	if limit == 0 {
		limit = promptDefaultPageSize
	}
	if limit > promptMaxPageSize {
		limit = promptMaxPageSize
	}
	projectID, err := s.scopedReadProject(ctx, args.Project, args.All, "every project's stored prompts")
	if err != nil {
		return toolResult{}, err
	}
	hits, err := backend.ListPrompts(ctx, PromptQuery{ProjectID: projectID, Limit: limit})
	if err != nil {
		return toolResult{}, err
	}
	needle := strings.ToLower(strings.TrimSpace(args.Query))
	filtered := make([]PromptHit, 0, len(hits))
	for _, hit := range hits {
		if needle != "" && !strings.Contains(strings.ToLower(hit.Text), needle) {
			continue
		}
		filtered = append(filtered, hit)
	}
	if filtered == nil {
		filtered = []PromptHit{}
	}
	var text strings.Builder
	if len(filtered) == 0 {
		text.WriteString("no prompts")
	}
	for _, hit := range filtered {
		fmt.Fprintf(&text, "- %s %s\n", hit.At.UTC().Format(time.RFC3339), truncateRunes(hit.Text, 160))
	}
	return toolResult{
		Text: strings.TrimRight(text.String(), "\n"),
		Structured: struct {
			Action string      `json:"action"`
			Hits   []PromptHit `json:"hits"`
		}{Action: "search", Hits: filtered},
	}, nil
}

// promptHandle is a stable opaque handle for one stored prompt.
func promptHandle(hit PromptHit) string {
	if strings.TrimSpace(hit.Path) != "" {
		return hit.Path
	}
	return hit.ProjectID + "@" + hit.At.UTC().Format(time.RFC3339Nano)
}
