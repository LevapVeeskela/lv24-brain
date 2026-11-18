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
	budget := fs.Int("budget", 0, "total token budget; 0 selects context.budget or the default 6000")
	explain := fs.Bool("explain", false, "print the per-layer token budget table")
	var kinds repeatedFlag
	fs.Var(&kinds, "kind", "keep only this record kind: decision, attempt or note (repeatable)")
	since := fs.String("since", "", "drop records older than a window (24h, 7d, 2w) or a date (2026-11-01)")
	subtype := fs.String("subtype", "", "keep only relevant records with this subtype")
	scope := fs.String("scope", "", "keep only relevant records with this scope: project, personal or global")
	topic := fs.String("topic", "", "keep only relevant records with this topic key")
	raw := fs.Bool("raw", false, "include granted raw-zone excerpts when curated context is thin")
	includeUnreviewed := fs.Bool("include-unreviewed", false, "include quarantined records (unreviewed agent, connector or import writes)")
	validity := addValidityFlags(fs)
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
	if *budget > domain.MaxContextBudgetTokens {
		fmt.Fprintf(stderr, "brain context: --budget must be at most %d tokens\n", domain.MaxContextBudgetTokens)
		return 2
	}
	// Validate early so a bad value is a usage error (exit 2) like in search.
	if _, err := domain.ParseSince(*since, time.Now()); err != nil {
		fmt.Fprintf(stderr, "brain context: --%v\n", err)
		return 2
	}
	asOf, includeSuperseded, err := validity.parse()
	if err != nil {
		fmt.Fprintf(stderr, "brain context: %v\n", err)
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "context", err)
	}
	if err := cfg.Context.Validate(); err != nil {
		return reportError(stderr, "context", err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "context", err)
	}
	defer func() { _ = runtime.Close() }()
	ctx := context.Background()
	target, err := resolveTarget(ctx, runtime.App, *project, "", stderr)
	if err != nil {
		return reportError(stderr, "context", err)
	}
	result, err := runtime.App.Context(ctx, app.ContextOptions{
		ProjectID:         target.Project.ID,
		Query:             query,
		TokenBudget:       *budget,
		Kinds:             kinds,
		Since:             *since,
		Subtype:           *subtype,
		Scope:             *scope,
		TopicKey:          *topic,
		Raw:               *raw,
		IncludeUnreviewed: *includeUnreviewed,
		AsOf:              asOf,
		IncludeSuperseded: includeSuperseded,
	})
	if err != nil {
		return reportError(stderr, "context", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "context", toMCPContext(result, *explain).WithEmptyLists())
	}
	printSummary(stdout, result.Summary)
	if *explain {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, result.Pack.ExplainText())
	}
	return 0
}

// Context implements mcpserver.ContextBackend.
func (b mcpBackend) Context(ctx context.Context, in mcpserver.ContextInput) (mcpserver.TaskContext, error) {
	result, err := b.app.Context(ctx, app.ContextOptions{
		ProjectID:         in.ProjectID,
		Query:             in.Query,
		TokenBudget:       in.Budget,
		Kinds:             in.Kinds,
		Since:             in.Since,
		Raw:               in.Raw,
		IncludeUnreviewed: in.IncludeUnreviewed,
	})
	if err != nil {
		return mcpserver.TaskContext{}, err
	}
	return toMCPContext(result, in.Explain), nil
}

// toMCPContext maps the use case result to the shape shared by brain_context
// and brain context --json.
func toMCPContext(result app.ContextResult, explain bool) mcpserver.TaskContext {
	out := mcpserver.TaskContext{
		ProjectID:        result.Project.ID,
		ProjectName:      result.Project.Name,
		Mode:             result.Mode,
		Query:            result.Query,
		Since:            result.Since,
		Budget:           contextBudgetOf(result),
		TotalTokens:      result.Pack.TotalTokens,
		State:            toMCPState(result.State),
		Records:          mapAll(result.Records, toMCPContextRecord),
		FailedAttempts:   mapAll(result.FailedAttempts, toMCPContextRecord),
		Sections:         mapAll(result.Pack.Sections, toMCPContextSection),
		Truncated:        mapAll(result.Pack.Truncated, toMCPContextTruncation),
		Excluded:         mapAll(result.Excluded, toMCPContextExclusion),
		ExcludedTotal:    result.ExcludedTotal,
		NearMisses:       mapAll(result.NearMisses, toMCPContextExclusion),
		QuarantinedCount: len(result.Quarantined),
		StateQuarantined: result.StateQuarantined,
		Findings: mapAll(result.Findings, func(finding app.Finding) mcpserver.ReviewFinding {
			return mcpserver.ReviewFinding{
				Kind: string(finding.Kind), Severity: string(finding.Severity),
				Message: finding.Message, Why: finding.Why, Next: finding.Next, Confirm: finding.Confirm,
				Evidence: finding.Evidence, Fixable: finding.Fixable, Fix: finding.Fix,
			}
		}),
		RawExcerpts: mapAll(result.RawExcerpts, toMCPContextExcerpt),
		Skipped:     result.Skipped,
		Quarantined: result.Quarantined,
		Summary:     result.Summary,
	}
	if result.Handoff != nil {
		out.Handoff = toMCPContextHandoff(*result.Handoff)
	}
	if explain {
		out.Explain = result.Pack.ExplainLines()
	}
	return out
}

// contextBudgetOf reports the token budget actually applied. The legacy rune
// budget is the fallback for a result built without the packer.
func contextBudgetOf(result app.ContextResult) int {
	if result.Pack.Budget > 0 {
		return result.Pack.Budget
	}
	return result.Budget
}

func toMCPContextSection(section app.ContextSection) mcpserver.ContextSection {
	return mcpserver.ContextSection{
		Name: section.Name, Cap: section.Cap, Tokens: section.Tokens,
		Records: mapAll(section.Records, func(record app.ContextSectionRecord) mcpserver.ContextSectionRecord {
			return mcpserver.ContextSectionRecord{
				ID: record.ID, Kind: record.Kind, Title: record.Title, Path: record.Path,
				Tokens: record.Tokens, Score: record.Score,
			}
		}),
	}
}

func toMCPContextTruncation(truncation app.ContextTruncation) mcpserver.ContextTruncation {
	return mcpserver.ContextTruncation{ID: truncation.ID, Section: truncation.Section, Reason: truncation.Reason}
}

func toMCPContextHandoff(handoff domain.Handoff) *mcpserver.ContextHandoff {
	return &mcpserver.ContextHandoff{
		ID: handoff.ID, Summary: handoff.Summary, NextAction: handoff.NextAction, CreatedAt: handoff.CreatedAt,
	}
}

func toMCPContextExclusion(exclusion app.ContextExclusion) mcpserver.ContextExclusion {
	return mcpserver.ContextExclusion{
		ID: exclusion.ID, Kind: exclusion.Kind, Reason: exclusion.Reason, Score: exclusion.Score,
	}
}

func toMCPContextRecord(record app.ContextRecord) mcpserver.ContextRecord {
	return mcpserver.ContextRecord{
		ID: record.ID, Kind: record.Kind, Title: record.Title, Status: record.Status,
		Detail: record.Detail, CreatedAt: record.CreatedAt, Score: record.Score,
		PathSignal: record.PathSignal,
	}
}

func toMCPContextExcerpt(excerpt app.ContextExcerpt) mcpserver.ContextExcerpt {
	return mcpserver.ContextExcerpt{
		ConnectionID: excerpt.ConnectionID,
		ProjectID:    excerpt.ProjectID,
		ItemKey:      excerpt.ItemKey,
		ExternalID:   excerpt.ExternalID,
		Kind:         excerpt.Kind,
		Title:        excerpt.Title,
		URL:          excerpt.URL,
		UpdatedAt:    excerpt.UpdatedAt,
		Citation:     excerpt.Citation,
		Body:         excerpt.Body,
		Truncated:    excerpt.Truncated,
		Untrusted:    true,
	}
}
