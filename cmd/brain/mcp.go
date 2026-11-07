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
	"github.com/levapveeskela/lv24-brain/internal/gitinfo"
	"github.com/levapveeskela/lv24-brain/internal/mcpserver"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/levapveeskela/lv24-brain/internal/version"
)

// runMCP serves the Model Context Protocol over stdin and stdout until stdin
// reaches end of file. Stdout carries protocol messages only; diagnostics go
// to stderr.
func runMCP(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("mcp", stderr)
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain mcp: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "mcp", err)
	}
	useCase, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "mcp", err)
	}
	defer func() { _ = useCase.Close() }()
	server := mcpserver.New(mcpBackend{app: useCase.App}, version.Version)
	if err := server.Serve(context.Background(), stdin, stdout); err != nil {
		return reportError(stderr, "mcp", err)
	}
	return 0
}

// mcpBackend adapts the application layer to the MCP tool backend. Project
// selection follows the CLI rules (resolveTarget) and never creates projects.
type mcpBackend struct {
	app *app.App
}

func (b mcpBackend) Resolve(ctx context.Context, selector string) (mcpserver.Target, error) {
	resolved, err := resolveTarget(ctx, b.app, "", selector)
	if workspace, ok := app.AsWorkspace(err); ok {
		if text, overviewErr := b.app.WorkspaceOverview(ctx, workspace); overviewErr == nil {
			return mcpserver.Target{}, &mcpserver.WorkspaceOverview{Text: text, Message: err.Error()}
		}
	}
	if err != nil {
		return mcpserver.Target{}, err
	}
	return mcpserver.Target{ID: resolved.Project.ID, Path: resolved.Path}, nil
}

func (b mcpBackend) Bootstrap(ctx context.Context, projectID string, budget int) (string, error) {
	result, err := b.app.Bootstrap(ctx, app.BootstrapOptions{ProjectID: projectID, Budget: budget})
	if err != nil {
		return "", err
	}
	return result.Summary, nil
}

func (b mcpBackend) Search(ctx context.Context, in mcpserver.SearchInput) ([]mcpserver.Hit, error) {
	hits, err := b.app.SearchWith(ctx, app.SearchOptions{
		Text:         in.Query,
		ProjectID:    in.ProjectID,
		Limit:        in.Limit,
		Kinds:        in.Kinds,
		IncludeDumps: in.IncludeDumps,
	})
	if err != nil {
		return nil, err
	}
	out := make([]mcpserver.Hit, 0, len(hits))
	for _, hit := range hits {
		out = append(out, mcpserver.Hit{
			ID:        hit.ID,
			ProjectID: hit.ProjectID,
			Kind:      hit.Kind,
			Title:     hit.Title,
			Snippet:   hit.Snippet,
		})
	}
	return out, nil
}

func (b mcpBackend) Note(ctx context.Context, in mcpserver.NoteInput) (mcpserver.Record, error) {
	result, err := b.app.Note(ctx, app.NoteInput{ProjectID: in.ProjectID, Title: in.Title, Body: in.Body})
	if err != nil {
		return mcpserver.Record{}, err
	}
	return mcpserver.Record{ID: result.Note.ID, ProjectID: in.ProjectID}, nil
}

func (b mcpBackend) Decision(ctx context.Context, in mcpserver.DecisionInput) (mcpserver.Record, error) {
	result, err := b.app.Decision(ctx, app.DecisionInput{
		ProjectID:    in.ProjectID,
		Title:        in.Title,
		Status:       domain.DecisionStatus(in.Status),
		Context:      in.Context,
		Alternatives: in.Alternatives,
		Consequences: in.Consequences,
	})
	if err != nil {
		return mcpserver.Record{}, err
	}
	return mcpserver.Record{ID: result.Decision.ID, ProjectID: in.ProjectID}, nil
}

func (b mcpBackend) Attempt(ctx context.Context, in mcpserver.AttemptInput) (mcpserver.Record, error) {
	result, err := b.app.Attempt(ctx, app.AttemptInput{
		ProjectID:  in.ProjectID,
		Title:      in.Title,
		Hypothesis: in.Hypothesis,
		Result:     in.Result,
		Reason:     in.Reason,
	})
	if err != nil {
		return mcpserver.Record{}, err
	}
	return mcpserver.Record{ID: result.Attempt.ID, ProjectID: in.ProjectID}, nil
}

// Handoff captures the git context of the project checkout, as `brain
// handoff` does, before recording the handoff.
func (b mcpBackend) Handoff(ctx context.Context, in mcpserver.HandoffInput) (mcpserver.Record, error) {
	resolved, err := b.app.ResolveSelector(ctx, in.ProjectID)
	if err != nil {
		return mcpserver.Record{}, err
	}
	gitContext := gitinfo.Read(ctx, gitDir(resolved)).Context()
	result, err := b.app.Handoff(ctx, app.HandoffInput{
		ProjectID:  in.ProjectID,
		Summary:    in.Summary,
		GitContext: gitContext,
		NextAction: in.NextAction,
	})
	if err != nil {
		return mcpserver.Record{}, err
	}
	return mcpserver.Record{ID: result.Handoff.ID, ProjectID: in.ProjectID}, nil
}

func (b mcpBackend) Journal(ctx context.Context, query mcpserver.JournalQuery) ([]mcpserver.JournalEntry, error) {
	entries, err := b.app.ReadJournal(ctx, ports.JournalQuery{Project: query.ProjectID, Limit: query.Limit})
	if err != nil {
		return nil, err
	}
	out := make([]mcpserver.JournalEntry, 0, len(entries))
	for _, entry := range entries {
		fields := entry.Fields
		if fields == nil {
			fields = map[string]string{}
		}
		out = append(out, mcpserver.JournalEntry{
			Time:    entry.Time,
			User:    entry.User,
			Event:   entry.Event,
			Project: entry.Project,
			ID:      entry.ID,
			Title:   entry.Title,
			Fields:  fields,
		})
	}
	return out, nil
}

func (b mcpBackend) Projects(ctx context.Context) ([]mcpserver.Project, error) {
	projects, err := b.app.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]mcpserver.Project, 0, len(projects))
	for _, project := range projects {
		paths := project.Paths
		if paths == nil {
			paths = []string{}
		}
		out = append(out, mcpserver.Project{
			ID:      project.ID,
			Name:    project.Name,
			Kind:    string(project.Kind),
			Paths:   paths,
			RepoURL: project.RepoURL,
		})
	}
	return out, nil
}

// GetState implements mcpserver.StateBackend.
func (b mcpBackend) GetState(ctx context.Context, projectID string) (mcpserver.State, error) {
	_, state, err := b.app.GetState(ctx, projectID)
	if err != nil {
		return mcpserver.State{}, err
	}
	return toMCPState(state), nil
}

// SetState implements mcpserver.StateBackend.
func (b mcpBackend) SetState(ctx context.Context, in mcpserver.StateInput) (mcpserver.State, error) {
	state, err := b.app.SetState(ctx, app.StateInput{
		ProjectID: in.ProjectID, Goal: in.Goal, NextAction: in.NextAction,
		Active: in.Active, Blocked: in.Blocked, OpenQuestions: in.OpenQuestions, FreshUntil: in.FreshUntil,
	})
	if err != nil {
		return mcpserver.State{}, err
	}
	return toMCPState(state), nil
}

func toMCPState(state domain.State) mcpserver.State {
	nonNil := func(values []string) []string {
		if values == nil {
			return []string{}
		}
		return values
	}
	return mcpserver.State{
		ProjectID: state.ProjectID, Goal: state.Goal, NextAction: state.NextAction,
		Active: nonNil(state.Active), Blocked: nonNil(state.Blocked), OpenQuestions: nonNil(state.OpenQuestions),
		UpdatedAt: state.UpdatedAt, FreshUntil: state.FreshUntil,
	}
}

// Review implements mcpserver.ReviewBackend: the review summary plus the
// findings that still apply. The tool never applies fixes.
func (b mcpBackend) Review(ctx context.Context, projectID string) (mcpserver.Review, error) {
	result, err := b.app.Review(ctx, app.ReviewOptions{ProjectID: projectID})
	if err != nil {
		return mcpserver.Review{}, err
	}
	findings, err := b.app.Findings(ctx, projectID)
	if err != nil {
		return mcpserver.Review{}, err
	}
	return mcpserver.Review{
		ProjectID: projectID,
		Summary:   result.Summary,
		Findings: mapAll(findings, func(finding app.Finding) mcpserver.ReviewFinding {
			return mcpserver.ReviewFinding{
				Kind: string(finding.Kind), Message: finding.Message, Evidence: finding.Evidence,
				Fixable: finding.Fixable, Fix: finding.Fix,
			}
		}),
	}, nil
}

// Catchup implements mcpserver.CatchupBackend.
func (b mcpBackend) Catchup(ctx context.Context, projectID string, since time.Time, limit int) (mcpserver.Catchup, error) {
	result, err := b.app.Catchup(ctx, app.CatchupOptions{ProjectID: projectID, Since: since, Limit: limit})
	if err != nil {
		return mcpserver.Catchup{}, err
	}
	return toMCPCatchup(result), nil
}

func toMCPCatchup(result app.CatchupResult) mcpserver.Catchup {
	out := mcpserver.Catchup{
		ProjectID:    result.Project.ID,
		Since:        result.Since,
		SinceSource:  result.SinceSource,
		StateChanged: result.StateChanged,
		Notes: mapAll(result.Notes, func(note domain.Note) mcpserver.CatchupItem {
			return mcpserver.CatchupItem{Kind: "note", ID: note.ID, Title: catchupLine(firstNonEmpty(note.Title, note.Body)), CreatedAt: note.CreatedAt}
		}),
		Decisions: mapAll(result.Decisions, func(decision domain.Decision) mcpserver.CatchupItem {
			return mcpserver.CatchupItem{Kind: "decision", ID: decision.ID, Title: catchupLine(decision.Title), CreatedAt: decision.UpdatedAt}
		}),
		Attempts: mapAll(result.Attempts, func(attempt domain.Attempt) mcpserver.CatchupItem {
			return mcpserver.CatchupItem{Kind: "attempt", ID: attempt.ID, Title: catchupLine(attempt.Title), CreatedAt: attempt.CreatedAt}
		}),
		Handoffs: mapAll(result.Handoffs, func(handoff domain.Handoff) mcpserver.CatchupItem {
			return mcpserver.CatchupItem{Kind: "handoff", ID: handoff.ID, Title: catchupLine(handoff.Summary), CreatedAt: handoff.CreatedAt}
		}),
		Journal: mapAll(result.Journal, formatJournalEntry),
	}
	if result.Baseline != nil {
		out.BaselineSummary = catchupLine(result.Baseline.Summary)
	}
	if result.StateChanged {
		state := toMCPState(result.State)
		out.State = &state
	}
	return out
}

// ListRecords implements mcpserver.RecordBackend.
func (b mcpBackend) ListRecords(ctx context.Context, query mcpserver.RecordQuery) ([]mcpserver.RecordSummary, error) {
	records, err := b.app.ListRecords(ctx, app.ListRecordsOptions{
		ProjectID: query.ProjectID,
		Kinds:     query.Kinds,
		Since:     query.Since,
		Status:    query.Status,
		Limit:     query.Limit,
	})
	if err != nil {
		return nil, err
	}
	return mapAll(records, toRecordSummary), nil
}

// GetRecord implements mcpserver.RecordBackend.
func (b mcpBackend) GetRecord(ctx context.Context, projectID, id string) (mcpserver.RecordDetail, error) {
	record, err := b.app.GetRecord(ctx, projectID, id)
	if err != nil {
		return mcpserver.RecordDetail{}, err
	}
	return toRecordDetail(record), nil
}

// SetDecisionStatus implements mcpserver.RecordBackend.
func (b mcpBackend) SetDecisionStatus(ctx context.Context, projectID, id, status string) (mcpserver.RecordSummary, error) {
	decision, err := b.app.SetDecisionStatus(ctx, projectID, id, domain.DecisionStatus(status))
	if err != nil {
		return mcpserver.RecordSummary{}, err
	}
	return toRecordSummary(app.Record{Decision: &decision}.Summary()), nil
}

// Supersede implements mcpserver.RecordBackend.
func (b mcpBackend) Supersede(ctx context.Context, projectID, oldID, newID string) (mcpserver.RecordSummary, error) {
	decision, err := b.app.Supersede(ctx, projectID, oldID, newID)
	if err != nil {
		return mcpserver.RecordSummary{}, err
	}
	return toRecordSummary(app.Record{Decision: &decision}.Summary()), nil
}
