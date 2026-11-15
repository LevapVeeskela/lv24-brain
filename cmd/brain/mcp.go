package main

import (
	"context"
	"encoding/json"
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
//
// The server is least-privilege by default: writes may only target the project
// of the working directory, and --read-only, --projects and --allow-write
// narrow that further.
func runMCP(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("mcp", stderr)
	cfgPath := configFlag(fs)
	listTools := fs.Bool("list-tools", false, "print the tool catalogue (name, title, annotations, schemas, limits) and exit without serving")
	asJSON := fs.Bool("json", false, "with --list-tools, print the catalogue as JSON")
	readOnly := fs.Bool("read-only", false, "expose read tools only: write tools are omitted and a call to one is refused with UNSUPPORTED")
	projectsFlag := fs.String("projects", "", "comma-separated project ids or aliases this server may read and write (default: every registered project)")
	allowWrite := fs.Bool("allow-write", false, "let write tools target any allowed project, not only the project of the working directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain mcp: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	entries, err := parseProjectAllowlist(*projectsFlag)
	if err != nil {
		fmt.Fprintf(stderr, "brain mcp: %v\n", err)
		return 2
	}
	// --read-only wins over --allow-write: the narrower profile is the one the
	// operator asked to be unable to widen by accident.
	policy := mcpserver.Policy{ReadOnly: *readOnly, AllowWrite: *allowWrite && !*readOnly, Projects: entries}
	if *listTools {
		return printMCPTools(stdout, stderr, *asJSON, policy)
	}
	if *asJSON {
		fmt.Fprintln(stderr, "brain mcp: --json is only valid with --list-tools")
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
	ctx := context.Background()
	resolved, err := resolveProjectAllowlist(ctx, useCase.App, entries)
	if err != nil {
		fmt.Fprintf(stderr, "brain mcp: %v\n", err)
		return 2
	}
	policy.Projects = resolved
	policy.DefaultProject = defaultWriteProject(ctx, useCase.App)
	policy.RestrictWrites = true
	if !policy.ReadOnly && !policy.AllowWrite && policy.DefaultProject == "" {
		fmt.Fprintln(stderr, "brain mcp: no project resolved from the working directory; write tools will refuse until the server is started with --allow-write")
	}
	server := mcpserver.NewWithPolicy(mcpBackend{app: useCase.App, quarantineAgent: cfg.Security.QuarantineAgent}, version.Version, policy)
	if err := server.Serve(ctx, stdin, stdout); err != nil {
		return reportError(stderr, "mcp", err)
	}
	return 0
}

// parseProjectAllowlist splits the --projects value. An empty value means every
// registered project; an empty entry is a usage error, not a silent widening.
func parseProjectAllowlist(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	entries := make([]string, 0, strings.Count(value, ",")+1)
	seen := map[string]bool{}
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fmt.Errorf("--projects must be a comma-separated list of project ids or aliases")
		}
		if seen[strings.ToLower(entry)] {
			continue
		}
		seen[strings.ToLower(entry)] = true
		entries = append(entries, entry)
	}
	return entries, nil
}

// resolveProjectAllowlist maps --projects entries to project ids. An entry is an
// id or an alias, never a path: a flag must not silently widen to whatever
// checkout happens to sit in the working directory.
func resolveProjectAllowlist(ctx context.Context, a *app.App, entries []string) ([]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	projects, err := a.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, entry := range entries {
		matches := make([]string, 0, 1)
		for _, project := range projects {
			if projectMatchesSelector(project, entry) {
				matches = append(matches, project.ID)
			}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("--projects: %q is not a registered project id or alias", entry)
		case 1:
		default:
			return nil, fmt.Errorf("--projects: %q matches several projects (%s); pass the project id", entry, strings.Join(matches, ", "))
		}
		if seen[matches[0]] {
			continue
		}
		seen[matches[0]] = true
		ids = append(ids, matches[0])
	}
	return ids, nil
}

// projectMatchesSelector reports whether the selector is the project id or one
// of its aliases, ignoring case as LookupProject does.
func projectMatchesSelector(project domain.Project, selector string) bool {
	if strings.EqualFold(project.ID, selector) {
		return true
	}
	for _, alias := range project.Aliases {
		if strings.EqualFold(alias, selector) {
			return true
		}
	}
	return false
}

// defaultWriteProject is the project `brain open` would pick in the working
// directory, or "" when the working directory is not a project.
func defaultWriteProject(ctx context.Context, a *app.App) string {
	resolved, err := resolveTarget(ctx, a, "", "", io.Discard)
	if err != nil {
		return ""
	}
	return resolved.Project.ID
}

// mcpCatalogue is the `brain mcp --list-tools --json` document.
type mcpCatalogue struct {
	Server struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"server"`
	ProtocolVersions []string             `json:"protocol_versions"`
	Policy           mcpserver.PolicyInfo `json:"policy"`
	Tools            []mcpserver.ToolSpec `json:"tools"`
	// OmittedTools are the tools the effective policy hides, so the full
	// contract stays visible when a profile is applied.
	OmittedTools []string `json:"omitted_tools"`
}

// printMCPTools writes the tool contract without serving. It needs no config:
// the catalogue is static, so an operator can diff it before deploying. The
// policy is echoed as requested; the running server resolves aliases to ids.
func printMCPTools(stdout, stderr io.Writer, asJSON bool, policy mcpserver.Policy) int {
	tools, omitted := mcpserver.CatalogForPolicy(version.Version, policy)
	info := policy.Info()
	if !asJSON {
		for _, tool := range tools {
			kind := "write"
			if tool.Annotations.ReadOnlyHint {
				kind = "read"
			}
			fmt.Fprintf(stdout, "%-22s %-5s %s\n", tool.Name, kind, tool.Title)
		}
		projects := "(all)"
		if len(info.Projects) > 0 {
			projects = strings.Join(info.Projects, ",")
		}
		fmt.Fprintf(stdout, "policy: read_only=%t allow_write=%t projects=%s\n", info.ReadOnly, info.AllowWrite, projects)
		if len(omitted) > 0 {
			fmt.Fprintf(stdout, "omitted: %s\n", strings.Join(omitted, " "))
		}
		return 0
	}
	catalogue := mcpCatalogue{ProtocolVersions: mcpserver.SupportedVersions(), Policy: info, Tools: tools, OmittedTools: omitted}
	if catalogue.OmittedTools == nil {
		catalogue.OmittedTools = []string{}
	}
	catalogue.Server.Name = mcpserver.ServerName
	catalogue.Server.Version = version.Version
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(catalogue); err != nil {
		return reportError(stderr, "mcp", err)
	}
	return 0
}

// mcpBackend adapts the application layer to the MCP tool backend. Project
// selection follows the CLI rules (resolveTarget) and never creates projects.
type mcpBackend struct {
	app *app.App
	// quarantineAgent makes MCP writes unreviewed (security.quarantine_agent).
	quarantineAgent bool
}

// writeProvenance is the provenance of every record an agent writes through
// MCP. Agent writes are reviewed immediately unless the instance asks to
// quarantine them; a quarantined write can only be cleared by an explicit
// review.
func (b mcpBackend) writeProvenance() domain.Provenance {
	return domain.Provenance{Origin: domain.OriginAgent, Reviewed: !b.quarantineAgent}
}

func (b mcpBackend) Resolve(ctx context.Context, selector string) (mcpserver.Target, error) {
	resolved, err := resolveTarget(ctx, b.app, "", selector, io.Discard)
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

func (b mcpBackend) Bootstrap(ctx context.Context, projectID string, budget int, includeUnreviewed bool) (string, error) {
	result, err := b.app.Bootstrap(ctx, app.BootstrapOptions{
		ProjectID: projectID, Budget: budget, IncludeUnreviewed: includeUnreviewed,
	})
	if err != nil {
		return "", err
	}
	return result.Summary, nil
}

func (b mcpBackend) Search(ctx context.Context, in mcpserver.SearchInput) ([]mcpserver.Hit, error) {
	since, err := domain.ParseSince(in.Since, time.Now())
	if err != nil {
		return nil, err
	}
	hits, err := b.app.SearchWith(ctx, app.SearchOptions{
		Text:         in.Query,
		ProjectID:    in.ProjectID,
		Limit:        in.Limit,
		Kinds:        in.Kinds,
		IncludeDumps: in.IncludeDumps,
		Since:        since,
	})
	if err != nil {
		return nil, err
	}
	out := make([]mcpserver.Hit, 0, len(hits))
	for _, hit := range hits {
		out = append(out, mcpserver.Hit{
			ID:           hit.ID,
			ProjectID:    hit.ProjectID,
			Kind:         hit.Kind,
			Title:        hit.Title,
			Snippet:      hit.Snippet,
			Score:        hit.Score,
			MatchReasons: hit.MatchReasons,
		})
	}
	return out, nil
}

func (b mcpBackend) Note(ctx context.Context, in mcpserver.NoteInput) (mcpserver.Record, error) {
	result, err := b.app.Note(ctx, app.NoteInput{
		ProjectID: in.ProjectID, Title: in.Title, Body: in.Body, Provenance: b.writeProvenance(),
	})
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
		Paths:        in.Paths,
		Provenance:   b.writeProvenance(),
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
		Provenance: b.writeProvenance(),
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
		Provenance: b.writeProvenance(),
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
				Kind: string(finding.Kind), Severity: string(finding.Severity),
				Message: finding.Message, Why: finding.Why, Next: finding.Next, Confirm: finding.Confirm,
				Evidence: finding.Evidence, Fixable: finding.Fixable, Fix: finding.Fix,
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
		Quarantined: nonNilStrings(result.Quarantined),
		Journal:     mapAll(result.Journal, formatJournalEntry),
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
	links, err := b.app.RecordLinks(ctx, projectID, id)
	if err != nil {
		return mcpserver.RecordDetail{}, err
	}
	return toRecordDetailWithLinks(links), nil
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
	result, err := b.app.Supersede(ctx, projectID, oldID, newID)
	if err != nil {
		return mcpserver.RecordSummary{}, err
	}
	return toRecordSummary(result.Old), nil
}

// ReviewRecord implements mcpserver.ReviewRecordBackend.
func (b mcpBackend) ReviewRecord(ctx context.Context, in mcpserver.ReviewRecordInput) (mcpserver.ReviewRecordResult, error) {
	result, err := b.app.ReviewRecords(ctx, app.ReviewRecordOptions{
		ProjectID: in.ProjectID,
		ID:        in.ID,
		All:       in.All,
		Trust:     domain.Trust(in.Trust),
	})
	if err != nil {
		return mcpserver.ReviewRecordResult{}, err
	}
	return mcpserver.ReviewRecordResult{
		ProjectID: result.ProjectID,
		Reviewed:  mapAll(result.Reviewed, toRecordSummary),
	}, nil
}

// ListRelations implements mcpserver.RelationBackend.
func (b mcpBackend) ListRelations(ctx context.Context, query mcpserver.RelationQuery) ([]mcpserver.RelationRow, error) {
	statuses := make([]domain.RelationStatus, 0, len(query.Statuses))
	for _, status := range query.Statuses {
		statuses = append(statuses, domain.RelationStatus(status))
	}
	kinds := make([]domain.RelationKind, 0, len(query.Relations))
	for _, relation := range query.Relations {
		kinds = append(kinds, domain.RelationKind(relation))
	}
	views, err := b.app.Relations(ctx, app.RelationListOptions{
		ProjectID: query.ProjectID,
		ID:        query.ID,
		TargetID:  query.Target,
		Statuses:  statuses,
		Kinds:     kinds,
		Limit:     query.Limit,
	})
	if err != nil {
		return nil, err
	}
	return mapAll(views, toMCPRelation), nil
}

// JudgeRelation implements mcpserver.RelationBackend.
func (b mcpBackend) JudgeRelation(ctx context.Context, in mcpserver.JudgeInput) (mcpserver.JudgedRelation, error) {
	result, err := b.app.JudgeRelation(ctx, app.JudgeOptions{
		ProjectID:  in.ProjectID,
		From:       in.From,
		To:         in.To,
		Verdict:    domain.RelationKind(in.Verdict),
		Reason:     in.Reason,
		Confidence: in.Confidence,
		By:         domain.VerdictBy(in.By),
	})
	if err != nil {
		return mcpserver.JudgedRelation{}, err
	}
	return mcpserver.JudgedRelation{Relation: toMCPRelation(result.Relation), Changed: result.Changed}, nil
}

// toMCPRelation projects one relation view for the MCP tools.
func toMCPRelation(view app.RelationView) mcpserver.RelationRow {
	row := mcpserver.RelationRow{
		SourceKind: view.Source.Kind, SourceID: view.Source.ID, SourceTitle: view.Source.Title,
		TargetID: view.TargetID, TargetState: view.TargetState,
		Relation: string(view.Relation.Relation), Status: string(view.Relation.Status),
		Reason: view.Relation.Reason, Confidence: view.Relation.Confidence,
		JudgedAt: view.Relation.JudgedAt, VerdictBy: string(view.Relation.VerdictBy),
		Direction: view.Direction, AgeDays: view.AgeDays,
	}
	if view.Target != nil {
		row.TargetKind = view.Target.Kind
		row.TargetTitle = view.Target.Title
	}
	return row
}
