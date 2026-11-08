package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/mcpserver"
)

// runContext prints the context relevant to one task: state, review
// findings, the latest handoff, the best-matching records and failed attempts.
func runContext(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("context", stderr)
	project := fs.String("project", "", "project id or alias (default: the working directory's project)")
	budget := fs.Int("budget", app.DefaultBootstrapBudget, "summary size budget in runes")
	var kinds repeatedFlag
	fs.Var(&kinds, "kind", "keep only this record kind: decision, attempt or note (repeatable)")
	since := fs.String("since", "", "drop records older than a window (24h, 7d, 2w) or a date (2026-11-01)")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	query := strings.TrimSpace(strings.Join(positional, " "))
	if query == "" {
		fmt.Fprintln(stderr, `brain context: the task is required, for example brain context "add retry to sync push"`)
		return 2
	}
	if *budget < 0 {
		fmt.Fprintln(stderr, "brain context: --budget must not be negative")
		return 2
	}
	// Validate early so a bad value is a usage error (exit 2) like in search.
	if _, err := domain.ParseSince(*since, time.Now()); err != nil {
		fmt.Fprintf(stderr, "brain context: --%v\n", err)
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "context", err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "context", err)
	}
	defer func() { _ = runtime.Close() }()
	ctx := context.Background()
	target, err := resolveTarget(ctx, runtime.App, *project, "")
	if err != nil {
		return reportError(stderr, "context", err)
	}
	result, err := runtime.App.Context(ctx, app.ContextOptions{
		ProjectID: target.Project.ID,
		Query:     query,
		Budget:    *budget,
		Kinds:     kinds,
		Since:     *since,
	})
	if err != nil {
		return reportError(stderr, "context", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "context", toMCPContext(result).WithEmptyLists())
	}
	printSummary(stdout, result.Summary)
	return 0
}

// Context implements mcpserver.ContextBackend.
func (b mcpBackend) Context(ctx context.Context, in mcpserver.ContextInput) (mcpserver.TaskContext, error) {
	result, err := b.app.Context(ctx, app.ContextOptions{
		ProjectID: in.ProjectID,
		Query:     in.Query,
		Budget:    in.Budget,
		Kinds:     in.Kinds,
		Since:     in.Since,
	})
	if err != nil {
		return mcpserver.TaskContext{}, err
	}
	return toMCPContext(result), nil
}

// toMCPContext maps the use case result to the shape shared by brain_context
// and brain context --json.
func toMCPContext(result app.ContextResult) mcpserver.TaskContext {
	out := mcpserver.TaskContext{
		ProjectID:      result.Project.ID,
		ProjectName:    result.Project.Name,
		Mode:           result.Mode,
		Query:          result.Query,
		Since:          result.Since,
		Budget:         result.Budget,
		State:          toMCPState(result.State),
		Records:        mapAll(result.Records, toMCPContextRecord),
		FailedAttempts: mapAll(result.FailedAttempts, toMCPContextRecord),
		Findings: mapAll(result.Findings, func(finding app.Finding) mcpserver.ReviewFinding {
			return mcpserver.ReviewFinding{
				Kind: string(finding.Kind), Message: finding.Message, Evidence: finding.Evidence,
				Fixable: finding.Fixable, Fix: finding.Fix,
			}
		}),
		Skipped: result.Skipped,
		Summary: result.Summary,
	}
	if result.Handoff != nil {
		out.Handoff = toMCPContextHandoff(*result.Handoff)
	}
	return out
}

func toMCPContextHandoff(handoff domain.Handoff) *mcpserver.ContextHandoff {
	return &mcpserver.ContextHandoff{
		ID: handoff.ID, Summary: handoff.Summary, NextAction: handoff.NextAction, CreatedAt: handoff.CreatedAt,
	}
}

func toMCPContextRecord(record app.ContextRecord) mcpserver.ContextRecord {
	return mcpserver.ContextRecord{
		ID: record.ID, Kind: record.Kind, Title: record.Title, Status: record.Status,
		Detail: record.Detail, CreatedAt: record.CreatedAt, Score: record.Score,
	}
}
