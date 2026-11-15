package main

import (
	"context"
	"fmt"
	"io"

	"github.com/levapveeskela/lv24-brain/internal/app"
)

// jsonGet is `brain get --json`: the record text under its token budget.
type jsonGet struct {
	Kind      string  `json:"kind"`
	ID        string  `json:"id"`
	ProjectID string  `json:"project_id"`
	Title     string  `json:"title"`
	Status    string  `json:"status,omitempty"`
	CreatedAt *string `json:"created_at"`
	UpdatedAt *string `json:"updated_at"`
	Body      string  `json:"body"`
	Truncated bool    `json:"truncated"`
	Tokens    int     `json:"tokens"`
	MaxTokens int     `json:"max_tokens"`
}

// runGet prints one record's text under a token budget. It is the second step
// of progressive disclosure: a search hit gives the id and a preview, get
// gives the body, and the budget keeps one record from filling the context.
// max_tokens 0 returns the whole record.
func runGet(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("get", stderr)
	project := fs.String("project", "", "project id or alias")
	maxTokens := fs.Int("max-tokens", app.DefaultRecordBodyTokens, "token budget of the body; 0 returns the whole record")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("get", positional, 1, "<id>", stderr) {
		return 2
	}
	if *maxTokens < 0 {
		fmt.Fprintln(stderr, "brain get: --max-tokens must not be negative")
		return 2
	}
	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, *project, nil, stderr, "get")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	body, err := runtime.App.RecordBody(ctx, target.Project.ID, positional[0], *maxTokens)
	if err != nil {
		return reportError(stderr, "get", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "get", jsonGet{
			Kind:      body.Kind,
			ID:        body.ID,
			ProjectID: body.ProjectID,
			Title:     body.Title,
			Status:    body.Status,
			CreatedAt: jsonTime(body.CreatedAt),
			UpdatedAt: jsonTime(body.UpdatedAt),
			Body:      body.Body,
			Truncated: body.Truncated,
			Tokens:    body.Tokens,
			MaxTokens: body.MaxTokens,
		})
	}
	fmt.Fprintf(stdout, "kind: %s\n", body.Kind)
	fmt.Fprintf(stdout, "id: %s\n", body.ID)
	fmt.Fprintf(stdout, "project: %s\n", body.ProjectID)
	fmt.Fprintf(stdout, "title: %s\n", body.Title)
	if body.Status != "" {
		fmt.Fprintf(stdout, "status: %s\n", body.Status)
	}
	if !body.CreatedAt.IsZero() {
		fmt.Fprintf(stdout, "created: %s\n", body.CreatedAt.Local().Format(recordTimeLayout))
	}
	if body.Body != "" {
		fmt.Fprintf(stdout, "body:\n%s\n", body.Body)
	}
	fmt.Fprintf(stdout, "truncated: %t\n", body.Truncated)
	if body.MaxTokens > 0 {
		fmt.Fprintf(stdout, "tokens: %d/%d\n", body.Tokens, body.MaxTokens)
	} else {
		fmt.Fprintf(stdout, "tokens: %d\n", body.Tokens)
	}
	return 0
}
