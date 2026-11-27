package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/codeindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/gitinfo"
	"github.com/levapveeskela/lv24-brain/internal/ideasview"
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
	profileName := fs.String("profile", mcpserver.ProfileLV24, "tool profile to serve: lv24 (default), engram, codegraph or graphify")
	profileVersion := fs.String("profile-version", "", "profile revision to pin (this build serves 1.0; pass 1 to pin the major)")
	strict := fs.Bool("strict", false, "with a migration profile, refuse every approximate alias and every dropped legacy argument; no effect on lv24")
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
	profile, err := mcpserver.ResolveProfile(*profileName, *profileVersion, *strict)
	if err != nil {
		fmt.Fprintf(stderr, "brain mcp: %v\n", err)
		return 2
	}
	// --read-only wins over --allow-write: the narrower profile is the one the
	// operator asked to be unable to widen by accident.
	policy := mcpserver.Policy{ReadOnly: *readOnly, AllowWrite: *allowWrite && !*readOnly, Projects: entries}
	if *listTools {
		return printMCPTools(stdout, stderr, *asJSON, profile, policy)
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
	backend := mcpBackend{app: useCase.App, cfg: cfg, quarantineAgent: cfg.Security.QuarantineAgent, promptsEnabled: cfg.Prompts.Enabled}
	server := mcpserver.NewWithProfile(backend, version.Version, profile, policy)
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
	Profile          mcpserver.Profile      `json:"profile"`
	ProtocolVersions []string               `json:"protocol_versions"`
	Policy           mcpserver.PolicyInfo   `json:"policy"`
	Tools            []mcpserver.ToolSpec   `json:"tools"`
	Aliases          []mcpserver.AliasSpec  `json:"aliases"`
	Capabilities     mcpserver.Capabilities `json:"capabilities"`
	// OmittedTools are the tools the effective policy hides, so the full
	// contract stays visible when a profile is applied.
	OmittedTools []string `json:"omitted_tools"`
	// Digest pins the effective vocabulary: tools, aliases and omitted names.
	Digest string `json:"digest"`
}

// printMCPTools writes the tool contract without serving. It needs no config:
// the catalogue is static, so an operator can diff it before deploying. The
// policy is echoed as requested; the running server resolves aliases to ids.
func printMCPTools(stdout, stderr io.Writer, asJSON bool, profile mcpserver.Profile, policy mcpserver.Policy) int {
	contract := mcpserver.BuildCatalogue(version.Version, profile, policy)
	info := policy.Info()
	if !asJSON {
		for _, tool := range contract.Tools {
			suffix := ""
			switch {
			case tool.Legacy && tool.Target != "" && tool.Action != "":
				suffix = " [" + tool.Grade + " -> " + tool.Target + "(" + tool.Action + ")]"
			case tool.Legacy && tool.Target != "":
				suffix = " [" + tool.Grade + " -> " + tool.Target + "]"
			case tool.Legacy:
				suffix = " [" + tool.Grade + "]"
			case tool.Stub:
				suffix = " [stub]"
			}
			fmt.Fprintf(stdout, "%-22s %-11s %s%s\n", tool.Name, tool.Policy, tool.Title, suffix)
		}
		projects := "(all)"
		if len(info.Projects) > 0 {
			projects = strings.Join(info.Projects, ",")
		}
		fmt.Fprintf(stdout, "profile: %s %s strict=%t\n", profile.Name, profile.Version, profile.Strict)
		if aliased := contract.Capabilities.Tools.Aliased; aliased > 0 {
			grades := contract.Capabilities.Tools.Grades
			fmt.Fprintf(stdout, "aliases: %d (A %d, B %d, C %d, D %d)\n", aliased,
				grades["A"], grades["B"], grades["C"], grades["D"])
		}
		fmt.Fprintf(stdout, "policy: read_only=%t allow_write=%t projects=%s\n", info.ReadOnly, info.AllowWrite, projects)
		if len(contract.OmittedTools) > 0 {
			fmt.Fprintf(stdout, "omitted: %s\n", strings.Join(contract.OmittedTools, " "))
		}
		fmt.Fprintf(stdout, "digest: %s\n", contract.Digest)
		return 0
	}
	catalogue := mcpCatalogue{
		Profile: contract.Profile, ProtocolVersions: mcpserver.SupportedVersions(), Policy: info,
		Tools: contract.Tools, Aliases: contract.Aliases, Capabilities: contract.Capabilities,
		OmittedTools: contract.OmittedTools, Digest: contract.Digest,
	}
	if catalogue.OmittedTools == nil {
		catalogue.OmittedTools = []string{}
	}
	if catalogue.Aliases == nil {
		catalogue.Aliases = []mcpserver.AliasSpec{}
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
	// cfg is the loaded instance configuration: the graph tools resolve the
	// engine, checkout and index path from it exactly as `brain code` does.
	cfg config.BrainConfig
	// quarantineAgent makes MCP writes unreviewed (security.quarantine_agent).
	quarantineAgent bool
	// promptsEnabled mirrors config.prompts.enabled; the prompt tool refuses
	// every action while it is false.
	promptsEnabled bool
}

// OpenGraph implements mcpserver.GraphBackend: it resolves the engine, checkout
// and index file of one project the way `brain code` does, then opens the engine
// read-only. The adapter errors are classified into the graph refusal
// vocabulary so the tool never reports a bare SQLite message.
func (b mcpBackend) OpenGraph(ctx context.Context, projectID, engine string) (ports.GraphIndex, error) {
	return b.openGraph(ctx, projectID, engine, false)
}

// OpenGraphWriter implements mcpserver.GraphBackend for index and sync. Only the
// native engine writes; codegraph answers ErrGraphReadOnly.
func (b mcpBackend) OpenGraphWriter(ctx context.Context, projectID, engine string) (mcpserver.GraphWriter, error) {
	opened, err := b.openGraph(ctx, projectID, engine, true)
	if err != nil {
		return nil, err
	}
	writer, ok := opened.(mcpserver.GraphWriter)
	if !ok {
		_ = closeGraphIndex(opened)
		return nil, mcpserver.ErrGraphReadOnly
	}
	return writer, nil
}

// openGraph resolves one project checkout and opens its code graph. A write
// creates a native store; a read leaves a missing store missing.
func (b mcpBackend) openGraph(ctx context.Context, projectID, engineOverride string, writable bool) (ports.GraphIndex, error) {
	engine := strings.ToLower(strings.TrimSpace(engineOverride))
	if engine == "" {
		engine = b.cfg.Graph.EffectiveEngine()
	}
	switch engine {
	case ports.GraphEngineOff:
		return nil, mcpserver.ErrGraphOff
	case ports.GraphEngineNative:
	case ports.GraphEngineCodeGraph:
		if writable {
			return nil, mcpserver.ErrGraphReadOnly
		}
	default:
		return nil, domain.NewValidationError("graph.engine",
			fmt.Sprintf("unknown engine %q; use %q or %q", engine, ports.GraphEngineNative, ports.GraphEngineCodeGraph))
	}
	resolved, err := resolveTarget(ctx, b.app, projectID, "", io.Discard)
	if err != nil {
		return nil, err
	}
	root := codeRoot("", resolved)
	if root == "" {
		return nil, fmt.Errorf("%w: project %s has no local checkout", mcpserver.ErrGraphAbsent, projectID)
	}
	dbPath, err := bootstrap.GraphDBPath(b.cfg, engine, root, projectID, "")
	if err != nil {
		return nil, err
	}
	if !writable {
		if _, statErr := os.Stat(dbPath); statErr != nil {
			return nil, fmt.Errorf("%w: %s", mcpserver.ErrGraphAbsent, dbPath)
		}
	}
	runtime, err := bootstrap.OpenGraph(ctx, bootstrap.GraphOptions{
		Engine:  engine,
		Root:    root,
		Project: projectID,
		DBPath:  dbPath,
		Create:  writable,
	})
	if err != nil {
		return nil, graphStoreError(err)
	}
	if writable {
		return &graphStoreWriter{GraphWriter: runtime.Native}, nil
	}
	return graphStoreIndex{GraphIndex: runtime.Index}, nil
}

// closeGraphIndex releases an engine that opened a database handle.
func closeGraphIndex(index ports.GraphIndex) error {
	if closer, ok := index.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// graphStoreError turns the native store conditions into the graph refusals the
// tools speak; every other error keeps its meaning.
func graphStoreError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, codeindex.ErrIndexMissing),
		errors.Is(err, codeindex.ErrIndexEmpty),
		errors.Is(err, codeindex.ErrSchemaMismatch):
		return fmt.Errorf("%w: %v", mcpserver.ErrGraphIndexMissing, err)
	default:
		return err
	}
}

// graphStoreIndex classifies the read errors of one engine. The codegraph
// adapter already unwraps its schema error onto domain.ErrUnsupported, which
// needs no translation, so the same wrapper serves both engines.
type graphStoreIndex struct{ ports.GraphIndex }

func (g graphStoreIndex) Status(ctx context.Context, project string) (ports.GraphStatus, error) {
	status, err := g.GraphIndex.Status(ctx, project)
	return status, graphStoreError(err)
}

func (g graphStoreIndex) Search(ctx context.Context, query ports.GraphQuery) ([]ports.SymbolHit, error) {
	hits, err := g.GraphIndex.Search(ctx, query)
	return hits, graphStoreError(err)
}

func (g graphStoreIndex) Node(ctx context.Context, project, symbol, file string, opts ports.NodeOpts) (ports.NodeView, error) {
	view, err := g.GraphIndex.Node(ctx, project, symbol, file, opts)
	return view, graphStoreError(err)
}

func (g graphStoreIndex) Explore(ctx context.Context, project, query string, maxFiles int) (ports.ExploreView, error) {
	view, err := g.GraphIndex.Explore(ctx, project, query, maxFiles)
	return view, graphStoreError(err)
}

func (g graphStoreIndex) Impact(ctx context.Context, project, symbol, file string, depth int) (ports.ImpactView, error) {
	view, err := g.GraphIndex.Impact(ctx, project, symbol, file, depth)
	return view, graphStoreError(err)
}

// Close forwards to the engine; the port does not require it but both adapters
// hold a database handle.
func (g graphStoreIndex) Close() error { return closeGraphIndex(g.GraphIndex) }

// graphStoreWriter adds the writable half on top of the classified reader.
type graphStoreWriter struct {
	mcpserver.GraphWriter
}

func (g graphStoreWriter) Status(ctx context.Context, project string) (ports.GraphStatus, error) {
	status, err := g.GraphWriter.Status(ctx, project)
	return status, graphStoreError(err)
}

func (g graphStoreWriter) Search(ctx context.Context, query ports.GraphQuery) ([]ports.SymbolHit, error) {
	hits, err := g.GraphWriter.Search(ctx, query)
	return hits, graphStoreError(err)
}

func (g graphStoreWriter) Node(ctx context.Context, project, symbol, file string, opts ports.NodeOpts) (ports.NodeView, error) {
	view, err := g.GraphWriter.Node(ctx, project, symbol, file, opts)
	return view, graphStoreError(err)
}

func (g graphStoreWriter) Explore(ctx context.Context, project, query string, maxFiles int) (ports.ExploreView, error) {
	view, err := g.GraphWriter.Explore(ctx, project, query, maxFiles)
	return view, graphStoreError(err)
}

func (g graphStoreWriter) Impact(ctx context.Context, project, symbol, file string, depth int) (ports.ImpactView, error) {
	view, err := g.GraphWriter.Impact(ctx, project, symbol, file, depth)
	return view, graphStoreError(err)
}

func (g graphStoreWriter) Index(ctx context.Context, project string) (ports.GraphSyncResult, error) {
	result, err := g.GraphWriter.Index(ctx, project)
	return result, graphStoreError(err)
}

func (g graphStoreWriter) Sync(ctx context.Context, project string, paths []string) (ports.GraphSyncResult, error) {
	result, err := g.GraphWriter.Sync(ctx, project, paths)
	return result, graphStoreError(err)
}

// Close forwards to the native store.
func (g graphStoreWriter) Close() error { return closeGraphIndex(g.GraphWriter) }

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
	return mcpserver.Target{ID: resolved.Project.ID, Path: resolved.Path, Source: resolveSource(resolved.Project, selector)}, nil
}

// resolveSource names how a selector matched: cwd, id, alias or path.
func resolveSource(project domain.Project, selector string) string {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return "cwd"
	}
	if strings.EqualFold(project.ID, selector) {
		return "id"
	}
	for _, alias := range project.Aliases {
		if strings.EqualFold(alias, selector) {
			return "alias"
		}
	}
	return "path"
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

func (b mcpBackend) searchOptions(in mcpserver.SearchInput) (app.SearchOptions, error) {
	since, err := domain.ParseSince(in.Since, time.Now())
	if err != nil {
		return app.SearchOptions{}, err
	}
	return app.SearchOptions{
		Text:         in.Query,
		ProjectID:    in.ProjectID,
		Limit:        in.Limit,
		MatchMode:    in.MatchMode,
		Kinds:        in.Kinds,
		Scope:        in.Scope,
		IncludeDumps: in.IncludeDumps,
		Since:        since,
	}, nil
}

func (b mcpBackend) Search(ctx context.Context, in mcpserver.SearchInput) ([]mcpserver.Hit, error) {
	options, err := b.searchOptions(in)
	if err != nil {
		return nil, err
	}
	hits, err := b.app.SearchWith(ctx, options)
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
			Tier:         hit.Tier,
			MatchReasons: hit.MatchReasons,
		})
	}
	return out, nil
}

// SearchReport implements mcpserver.SearchReporter so the MCP answer carries
// the support verdict and the records the search withheld.
func (b mcpBackend) SearchReport(ctx context.Context, in mcpserver.SearchInput) (mcpserver.SearchReport, error) {
	options, err := b.searchOptions(in)
	if err != nil {
		return mcpserver.SearchReport{}, err
	}
	report, err := b.app.SearchWithReport(ctx, options)
	if err != nil {
		return mcpserver.SearchReport{}, err
	}
	hits := make([]mcpserver.Hit, 0, len(report.Hits))
	for _, hit := range report.Hits {
		hits = append(hits, mcpserver.Hit{
			ID: hit.ID, ProjectID: hit.ProjectID, Kind: hit.Kind, Title: hit.Title,
			Snippet: hit.Snippet, Score: hit.Score, Tier: hit.Tier, MatchReasons: hit.MatchReasons,
		})
	}
	excluded := make([]mcpserver.ExcludedHit, 0, len(report.Excluded))
	for _, hit := range report.Excluded {
		excluded = append(excluded, mcpserver.ExcludedHit{
			ID: hit.ID, ProjectID: hit.ProjectID, Kind: hit.Kind, Title: hit.Title,
			Score: hit.Score, Reason: hit.Reason,
		})
	}
	return mcpserver.SearchReport{
		Hits: hits,
		Support: mcpserver.Support{
			Tier: report.Support.Tier, Best: report.Support.Best,
			Threshold: report.Support.Threshold, Reason: report.Support.Reason,
		},
		Excluded: excluded,
	}, nil
}

func (b mcpBackend) Note(ctx context.Context, in mcpserver.NoteInput) (mcpserver.Record, error) {
	var source *domain.SourceRef
	if strings.TrimSpace(in.Source) != "" {
		source = &domain.SourceRef{ExternalID: strings.TrimSpace(in.Source)}
	}
	result, err := b.app.Note(ctx, app.NoteInput{
		ProjectID: in.ProjectID, Title: in.Title, Body: in.Body,
		Subtype: in.Subtype, Scope: domain.RecordScope(in.Scope), Topic: in.TopicKey, Source: source,
		Provenance: b.writeProvenance(),
	})
	if err != nil {
		return mcpserver.Record{}, err
	}
	return mcpserver.Record{
		ID: result.Note.ID, ProjectID: in.ProjectID,
		TopicKey: result.Note.Taxonomy.TopicKey,
		Replaced: !result.Already && !result.Note.CreatedAt.Equal(result.Note.UpdatedAt),
	}, nil
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
	return mcpserver.Record{ID: result.Handoff.ID, ProjectID: in.ProjectID, GitContext: gitContext}, nil
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
	return mapAll(projects, toMCPProject), nil
}

// toMCPProject projects one registered project for the MCP tools.
func toMCPProject(project domain.Project) mcpserver.Project {
	paths := project.Paths
	if paths == nil {
		paths = []string{}
	}
	return mcpserver.Project{
		ID:      project.ID,
		Name:    project.Name,
		Kind:    string(project.Kind),
		Paths:   paths,
		RepoURL: project.RepoURL,
	}
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
		Provenance: b.writeProvenance(),
	})
	if err != nil {
		return mcpserver.State{}, err
	}
	return toMCPState(state), nil
}

// SetStateIfRevision implements mcpserver.StateRevisionBackend: the revision
// check and the write happen under the same lock, so two MCP agents cannot
// both pass an advisory read-then-write check.
func (b mcpBackend) SetStateIfRevision(ctx context.Context, in mcpserver.StateInput, expected string) (mcpserver.State, error) {
	state, err := b.app.SetStateIfRevision(ctx, app.StateInput{
		ProjectID: in.ProjectID, Goal: in.Goal, NextAction: in.NextAction,
		Active: in.Active, Blocked: in.Blocked, OpenQuestions: in.OpenQuestions, FreshUntil: in.FreshUntil,
		Provenance: b.writeProvenance(),
	}, expected)
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

// AddIdea implements mcpserver.IdeaBackend.
func (b mcpBackend) AddIdea(ctx context.Context, in mcpserver.IdeaAddInput) (mcpserver.IdeaWrite, error) {
	result, err := b.app.AddIdea(ctx, app.IdeaInput{
		ProjectID:           in.ProjectID,
		Title:               in.Title,
		Body:                in.Body,
		Priority:            domain.IdeaPriority(in.Priority),
		DueAt:               in.DueAt,
		DueDate:             in.DueDate,
		RemindBeforeMinutes: in.RemindBeforeMinutes,
		Tags:                in.Tags,
		Source:              in.Source,
		Provenance:          b.writeProvenance(),
	})
	if err != nil {
		return mcpserver.IdeaWrite{}, err
	}
	return mcpserver.IdeaWrite{Idea: result.Idea, ProjectSource: result.ProjectSource}, nil
}

// ListIdeas implements mcpserver.IdeaBackend.
func (b mcpBackend) ListIdeas(ctx context.Context, in mcpserver.IdeaListQuery) (mcpserver.IdeaList, error) {
	statuses := make([]domain.IdeaStatus, 0, len(in.Statuses))
	for _, status := range in.Statuses {
		statuses = append(statuses, domain.IdeaStatus(status))
	}
	dueBefore, err := parseOptionalIdeaInstant("due_before", in.DueBefore)
	if err != nil {
		return mcpserver.IdeaList{}, err
	}
	dueAfter, err := parseOptionalIdeaInstant("due_after", in.DueAfter)
	if err != nil {
		return mcpserver.IdeaList{}, err
	}
	result, err := b.app.ListIdeas(ctx, app.IdeaListOptions{
		ProjectID: in.ProjectID,
		All:       in.All,
		Statuses:  statuses,
		Tags:      in.Tags,
		DueBefore: dueBefore,
		DueAfter:  dueAfter,
		Query:     in.Query,
		Limit:     in.Limit,
	})
	if err != nil {
		return mcpserver.IdeaList{}, err
	}
	return mcpserver.IdeaList{
		Ideas:         result.Ideas,
		Problems:      ideasview.Problems(b.cfg.DataDir, result.Problems),
		Total:         len(result.Ideas),
		ProjectSource: result.ProjectSource,
	}, nil
}

// GetIdea implements mcpserver.IdeaBackend.
func (b mcpBackend) GetIdea(ctx context.Context, id string) (domain.Idea, error) {
	return b.app.GetIdea(ctx, id)
}

// UpdateIdea implements mcpserver.IdeaBackend.
func (b mcpBackend) UpdateIdea(ctx context.Context, in mcpserver.IdeaUpdateInput) (mcpserver.IdeaWrite, error) {
	update := app.IdeaUpdateInput{
		ID:                  in.ID,
		ProjectID:           in.ProjectID,
		Title:               in.Title,
		Body:                in.Body,
		DueAt:               in.DueAt,
		DueDate:             in.DueDate,
		RemindBeforeMinutes: in.RemindBeforeMinutes,
		Tags:                in.Tags,
		ClearDue:            in.ClearDue,
		ClearReminder:       in.ClearReminder,
		ClearTags:           in.ClearTags,
	}
	if in.Status != nil {
		status := domain.IdeaStatus(strings.ToLower(strings.TrimSpace(*in.Status)))
		update.Status = &status
	}
	if in.Priority != nil {
		priority := domain.IdeaPriority(strings.ToLower(strings.TrimSpace(*in.Priority)))
		update.Priority = &priority
	}
	if strings.TrimSpace(in.Note) != "" {
		note := in.Note
		update.CompletionNote = &note
	}
	result, err := b.app.UpdateIdea(ctx, update)
	if err != nil {
		return mcpserver.IdeaWrite{}, err
	}
	return mcpserver.IdeaWrite{Idea: result.Idea, ProjectSource: result.ProjectSource}, nil
}

// TrashIdea implements mcpserver.IdeaBackend. The id is global, so no project
// selector is needed.
func (b mcpBackend) TrashIdea(ctx context.Context, id string) (domain.Idea, error) {
	return b.app.TrashIdea(ctx, "", id)
}

// RestoreIdea implements mcpserver.IdeaBackend. A non-empty project narrows
// the trash lookup to the server's write scope.
func (b mcpBackend) RestoreIdea(ctx context.Context, projectID, id string) (domain.Idea, error) {
	return b.app.RestoreIdea(ctx, projectID, id)
}

// IdeaBrief implements mcpserver.IdeaBackend.
func (b mcpBackend) IdeaBrief(ctx context.Context, in mcpserver.IdeaBriefQuery) (mcpserver.IdeaBrief, error) {
	result, err := b.app.IdeaBrief(ctx, app.IdeaBriefOptions{
		ProjectID: in.ProjectID,
		Scope:     in.Scope,
		Max:       in.Max,
	})
	if err != nil {
		return mcpserver.IdeaBrief{}, err
	}
	return mcpserver.IdeaBrief{
		Text:     result.Text,
		Ideas:    result.Ideas,
		Open:     result.Open,
		DueToday: result.DueToday,
		Muted:    result.Muted,
	}, nil
}

// parseOptionalIdeaInstant parses a list due bound. The mcpserver validates
// list text at the tool boundary too; the adapter repeats it so a direct
// caller gets a typed validation error.
func parseOptionalIdeaInstant(field, value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, domain.NewValidationError(field,
			fmt.Sprintf("%q is not an RFC3339 timestamp with an offset", value))
	}
	return at, nil
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

// RecordQuarantined implements mcpserver.RecordQuarantineBackend: a record
// resource is withheld unless the caller explicitly passes
// ?include_unreviewed=true, matching the trusted-context rule.
func (b mcpBackend) RecordQuarantined(ctx context.Context, projectID, id string) (bool, error) {
	record, err := b.app.GetRecord(ctx, projectID, id)
	if err != nil {
		return false, err
	}
	switch {
	case record.Note != nil:
		return record.Note.Provenance.Quarantined(), nil
	case record.Decision != nil:
		return record.Decision.Provenance.Quarantined(), nil
	case record.Attempt != nil:
		return record.Attempt.Provenance.Quarantined(), nil
	case record.Handoff != nil:
		return record.Handoff.Provenance.Quarantined(), nil
	default:
		return false, nil
	}
}

// SetPinned implements mcpserver.RecordPinner.
func (b mcpBackend) SetPinned(ctx context.Context, projectID, id string, pinned bool) (mcpserver.RecordSummary, error) {
	record, err := b.app.SetRecordPinned(ctx, projectID, id, pinned)
	if err != nil {
		return mcpserver.RecordSummary{}, err
	}
	return toRecordSummary(record.Summary()), nil
}

// SetRecordPinned implements mcpserver.RecordPinSetter.
func (b mcpBackend) SetRecordPinned(ctx context.Context, projectID, id string, pinned bool) (mcpserver.RecordSummary, error) {
	return b.SetPinned(ctx, projectID, id, pinned)
}

// PinRecord implements mcpserver.RecordPinToggler and RecordPinActioner.
func (b mcpBackend) PinRecord(ctx context.Context, projectID, id string, pinned ...bool) (mcpserver.RecordSummary, error) {
	flag := true
	if len(pinned) > 0 {
		flag = pinned[0]
	}
	record, err := b.app.SetRecordPinned(ctx, projectID, id, flag)
	if err != nil {
		return mcpserver.RecordSummary{}, err
	}
	return toRecordSummary(record.Summary()), nil
}

// UnpinRecord implements mcpserver.RecordPinActioner.
func (b mcpBackend) UnpinRecord(ctx context.Context, projectID, id string) (mcpserver.RecordSummary, error) {
	record, err := b.app.SetRecordPinned(ctx, projectID, id, false)
	if err != nil {
		return mcpserver.RecordSummary{}, err
	}
	return toRecordSummary(record.Summary()), nil
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

// Consolidate implements mcpserver.ConsolidateBackend.
func (b mcpBackend) Consolidate(ctx context.Context, in mcpserver.ConsolidateInput) (mcpserver.ConsolidateResult, error) {
	plan, applied, err := b.app.Consolidate(ctx, app.ConsolidateOptions{
		ProjectID: in.ProjectID, Phase: in.Phase, Ops: in.IDs,
		Fingerprint: in.Fingerprint,
	})
	if err != nil {
		return mcpserver.ConsolidateResult{}, err
	}
	out := mcpserver.ConsolidateResult{Phase: in.Phase, Plan: toMCPConsolidatePlan(plan)}
	if applied != nil {
		out.Phase = "apply"
		out.Applied = &mcpserver.ConsolidateApply{
			Manifest: applied.Manifest,
			Merged:   make([]mcpserver.ConsolidateMerged, 0, len(applied.Merged)),
			Archived: make([]mcpserver.ConsolidateArchived, 0, len(applied.Archived)),
			Skipped:  make([]mcpserver.ConsolidateApplySkip, 0, len(applied.Skipped)),
		}
		for _, merged := range applied.Merged {
			out.Applied.Merged = append(out.Applied.Merged, mcpserver.ConsolidateMerged{
				Op: merged.Op, Survivor: merged.Survivor, Manifest: merged.Manifest,
			})
		}
		for _, archived := range applied.Archived {
			out.Applied.Archived = append(out.Applied.Archived, mcpserver.ConsolidateArchived{
				Op: archived.Op, Kind: archived.Kind, ID: archived.ID,
			})
		}
		for _, skip := range applied.Skipped {
			out.Applied.Skipped = append(out.Applied.Skipped, mcpserver.ConsolidateApplySkip{Op: skip.Op, Reason: skip.Reason})
		}
	}
	return out, nil
}

// toMCPConsolidatePlan projects a consolidation plan for the MCP tool.
func toMCPConsolidatePlan(plan app.ConsolidatePlan) mcpserver.ConsolidatePlan {
	out := mcpserver.ConsolidatePlan{
		Projects: plan.Projects, Fingerprint: plan.Fingerprint, Truncated: plan.Truncated,
		Ops: make([]mcpserver.ConsolidateOp, 0, len(plan.Ops)),
	}
	for _, op := range plan.Ops {
		out.Ops = append(out.Ops, mcpserver.ConsolidateOp{
			ID: op.ID, Kind: op.Kind, ProjectID: op.ProjectID, RecordKind: op.RecordKind,
			IDs: nonNilStrings(op.IDs), Reason: op.Reason, Fingerprint: op.Fingerprint,
			Risk: op.Risk, Reversible: op.Reversible,
		})
	}
	if len(plan.Near) > 0 {
		out.Near = make([]mcpserver.ConsolidateCluster, 0, len(plan.Near))
		for _, cluster := range plan.Near {
			members := make([]mcpserver.ConsolidateClusterMem, 0, len(cluster.Members))
			for _, member := range cluster.Members {
				members = append(members, mcpserver.ConsolidateClusterMem{ID: member.ID, Title: member.Title})
			}
			out.Near = append(out.Near, mcpserver.ConsolidateCluster{
				ProjectID: cluster.ProjectID, Kind: cluster.Kind, Reason: cluster.Reason,
				Similarity: cluster.Similarity, Members: members,
			})
		}
	}
	return out
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

// RecordHistory implements mcpserver.RecordHistoryBackend.
func (b mcpBackend) RecordHistory(ctx context.Context, projectID, id string) ([]mcpserver.RecordSummary, error) {
	chain, err := b.app.SupersedeChain(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	return mapAll(chain, toRecordSummary), nil
}

// Timeline implements mcpserver.TimelineBackend.
func (b mcpBackend) Timeline(ctx context.Context, projectID, id string, window int) (mcpserver.Timeline, error) {
	result, err := b.app.Timeline(ctx, projectID, id, window)
	if err != nil {
		return mcpserver.Timeline{}, err
	}
	return mcpserver.Timeline{
		Anchor:      toRecordSummary(result.Anchor),
		Records:     mapAll(result.Records, toRecordSummary),
		AnchorIndex: result.AnchorIndex,
		Before:      result.Before,
		After:       result.After,
		Total:       result.Total,
	}, nil
}

// DueReviews implements mcpserver.DueReviewBackend.
func (b mcpBackend) DueReviews(ctx context.Context, projectID string) ([]mcpserver.DueReview, error) {
	due, err := b.app.DueReviews(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return mapAll(due, func(item app.DueReview) mcpserver.DueReview {
		days := int(item.Overdue.Hours() / 24)
		if days < 0 {
			days = 0
		}
		return mcpserver.DueReview{
			Kind: item.Kind, ID: item.ID, Title: item.Title,
			ReviewAfter: item.ReviewAfter, OverdueDays: days, Source: item.Source,
		}
	}), nil
}

// Doctor implements mcpserver.DoctorBackend.
func (b mcpBackend) Doctor(ctx context.Context) ([]mcpserver.DoctorCheck, error) {
	report, err := b.app.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		return nil, err
	}
	return mapAll(report.Checks, func(check app.DoctorCheck) mcpserver.DoctorCheck {
		return mcpserver.DoctorCheck{Name: check.Name, Status: check.Status, Message: check.Message, Code: check.Code}
	}), nil
}

// RegisterProject implements mcpserver.ProjectBackend.
func (b mcpBackend) RegisterProject(ctx context.Context, in mcpserver.ProjectInput) (mcpserver.Project, error) {
	input := app.ProjectInput{ID: in.ID}
	if len(in.Paths) > 0 {
		input.Paths = &in.Paths
	}
	if len(in.Aliases) > 0 {
		input.Aliases = &in.Aliases
	}
	project, err := b.app.SaveProject(ctx, input)
	if err != nil {
		return mcpserver.Project{}, err
	}
	return toMCPProject(project), nil
}

// PromptsEnabled implements mcpserver.PromptBackend.
func (b mcpBackend) PromptsEnabled() bool { return b.promptsEnabled }

// SavePrompt implements mcpserver.PromptBackend.
func (b mcpBackend) SavePrompt(ctx context.Context, in mcpserver.PromptInput) (mcpserver.PromptHit, error) {
	prompt, err := b.app.SavePrompt(ctx, app.PromptInput{
		ProjectID: in.ProjectID, Text: in.Text, Session: in.Session,
	})
	if err != nil {
		return mcpserver.PromptHit{}, err
	}
	return toMCPPrompt(prompt), nil
}

// ListPrompts implements mcpserver.PromptBackend.
func (b mcpBackend) ListPrompts(ctx context.Context, query mcpserver.PromptQuery) ([]mcpserver.PromptHit, error) {
	prompts, err := b.app.ListPrompts(ctx, app.PromptListOptions{ProjectID: query.ProjectID, Limit: query.Limit})
	if err != nil {
		return nil, err
	}
	return mapAll(prompts, toMCPPrompt), nil
}

func toMCPPrompt(prompt domain.Prompt) mcpserver.PromptHit {
	return mcpserver.PromptHit{
		ProjectID: prompt.ProjectID, At: prompt.At, Session: prompt.Session,
		Text: prompt.Text, Path: prompt.Path,
	}
}
