package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// The code graph is an optional capability of a backend. The MCP server never
// opens an index itself: it asks the backend for the read port (or the writable
// native store) of one project, uses it for one call and closes it again. That
// keeps the adapter choice, the checkout root and the engine configuration in
// the composition root, where the CLI already resolves them.
//
// A backend that does not implement GraphBackend answers every graph call with
// UNSUPPORTED and does not advertise the code:// resource.
var (
	// ErrGraphOff marks a project whose graph.engine is off.
	ErrGraphOff = errors.New("the code graph is off")
	// ErrGraphAbsent marks a project without a readable index: no checkout or
	// no index file.
	ErrGraphAbsent = errors.New("no code graph index")
	// ErrGraphIndexMissing marks an index file that exists but cannot answer:
	// an empty store or a schema another engine version wrote.
	ErrGraphIndexMissing = errors.New("the code graph index is missing, empty or stale")
	// ErrGraphReadOnly marks an engine that cannot index, such as codegraph.
	ErrGraphReadOnly = errors.New("the code graph engine is read-only")
)

// GraphWriter is the writable native store behind ports.GraphIndex. Only the
// native engine implements it; the codegraph engine is read-only.
type GraphWriter interface {
	ports.GraphIndex
	Index(ctx context.Context, project string) (ports.GraphSyncResult, error)
	Sync(ctx context.Context, project string, paths []string) (ports.GraphSyncResult, error)
}

// GraphBackend is implemented by a backend that serves the code graph.
type GraphBackend interface {
	// OpenGraph opens the code graph of one project for reading. The caller
	// closes the returned index when it is done. engine is "" to use
	// graph.engine, or native/codegraph to override it for this call.
	OpenGraph(ctx context.Context, projectID, engine string) (ports.GraphIndex, error)
	// OpenGraphWriter opens the writable native store of one project. It
	// returns ErrGraphReadOnly when the selected engine cannot index.
	OpenGraphWriter(ctx context.Context, projectID, engine string) (GraphWriter, error)
}

// codeActions is the closed action vocabulary of brain_code, in output order.
var codeActions = []string{"status", "index", "sync", "search", "node", "explore", "impact"}

// codeReadActions are the actions a read-only server keeps.
var codeReadActions = []string{"status", "search", "node", "explore", "impact"}

// codeWriteActions are the actions a read-only server refuses.
var codeWriteActions = []string{"index", "sync"}

// codeGraphAnnotations describes brain_code: reads by default, and the two
// write actions only refresh a derived local index idempotently.
func codeGraphAnnotations(title string) Annotations {
	return Annotations{Title: title, ReadOnlyHint: false, DestructiveHint: false, IdempotentHint: true, OpenWorldHint: false}
}

// codeArgs is one brain_code call.
type codeArgs struct {
	Action   string `json:"action"`
	Project  string `json:"project"`
	Query    string `json:"query"`
	Symbol   string `json:"symbol"`
	File     string `json:"file"`
	Engine   string `json:"engine"`
	Depth    int    `json:"depth"`
	Limit    int    `json:"limit"`
	MaxBytes int    `json:"max_bytes"`
}

func (s *Server) codeTool() tool {
	return tool{
		Name:        "brain_code",
		Annotations: codeGraphAnnotations("Query the code graph"),
		Description: "Query the code graph of one checkout: status, index, sync, search, node, explore or impact. " +
			"The index is a derived local store of symbols and edges, never the source of truth; source is read from the checkout. " +
			"status/search/node/explore/impact read; index/sync refresh the native index idempotently and are local. " +
			"An engine that is off, absent or unindexed answers with a typed UNSUPPORTED, never an empty result.",
		InputSchema: objectSchema(nil, map[string]any{
			"action":    enumProp("status reads the index state (default); index and sync build or refresh it; search, node, explore and impact query it.", codeActions),
			"project":   stringProp("Project id, alias or checkout path. Required: a code graph belongs to one checkout."),
			"query":     stringProp("Search text (search) or the natural-language query explore ranks symbols by."),
			"symbol":    stringProp("Symbol name or node id (node, impact)."),
			"file":      stringProp("Repository-relative path that narrows a symbol to one file."),
			"engine":    enumProp("Graph engine for this call: native or codegraph. Omit to use graph.engine.", []string{ports.GraphEngineNative, ports.GraphEngineCodeGraph}),
			"depth":     intProp("Reverse-BFS depth of impact, 1.."+strconv.Itoa(ports.MaxGraphImpactDepth)+"; 0 selects "+strconv.Itoa(ports.DefaultGraphImpactDepth)+".", 0),
			"limit":     intProp("Maximum hits (search), neighbours per side (node) or files rendered (explore).", 0),
			"max_bytes": intProp("Cap of an explore answer in bytes, at most "+strconv.Itoa(ports.MaxGraphExploreBytes)+"; 0 selects the cap.", 0),
		}),
		OutputSchema: codeOutputSchema(),
		Limits: map[string]int{
			"search_results": ports.MaxGraphSearchLimit,
			"impact_depth":   ports.MaxGraphImpactDepth,
			"explore_files":  ports.MaxGraphExploreFiles,
			"explore_bytes":  ports.MaxGraphExploreBytes,
			"neighbors":      ports.MaxGraphSearchLimit,
		},
		ReadActions:  codeReadActions,
		WriteActions: codeWriteActions,
		run:          s.code,
	}
}

// codeOutputSchema is the envelope of every action: one typed group is present
// under the key named by action, so no two actions disagree about a field type.
func codeOutputSchema() map[string]any {
	return outputObject(map[string]any{
		"action":  stringProp("Action that ran."),
		"project": stringProp("Project the answer belongs to."),
		"status":  objectProp("Index identity, counts and age (status): ports.GraphStatus."),
		"sync":    objectProp("What index or sync wrote (index, sync): ports.GraphSyncResult."),
		"search":  objectProp("Ranked hits and the query (search): query, hits, count."),
		"node":    objectProp("One symbol with source and direct neighbours (node): ports.NodeView."),
		"explore": objectProp("Bounded symbols, source, flow and blast radius (explore): ports.ExploreView."),
		"impact":  objectProp("Reverse blast radius and tests (impact): ports.ImpactView."),
	})
}

// code dispatches one brain_code call. Every action validates its own required
// arguments, so a missing query never reaches the engine.
func (s *Server) code(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args codeArgs
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	if action == "" {
		action = "status"
	}
	engine, err := codeEngine(args.Engine)
	if err != nil {
		return toolResult{}, err
	}
	switch action {
	case "status":
		return s.codeStatus(ctx, args, engine)
	case "index", "sync":
		return s.codeWrite(ctx, action, args, engine)
	case "search":
		return s.codeSearch(ctx, args, engine)
	case "node":
		return s.codeNode(ctx, args, engine)
	case "explore":
		return s.codeExplore(ctx, args, engine)
	case "impact":
		return s.codeImpact(ctx, args, engine)
	default:
		return toolResult{}, refusalInvalidf("unknown action %q (%s)", action, strings.Join(codeActions, ", "))
	}
}

// codeEngine validates the per-call engine override.
func codeEngine(value string) (string, error) {
	engine := strings.ToLower(strings.TrimSpace(value))
	switch engine {
	case "", ports.GraphEngineNative, ports.GraphEngineCodeGraph:
		return engine, nil
	default:
		return "", refusalInvalidf("unknown engine %q; use %q or %q", value, ports.GraphEngineNative, ports.GraphEngineCodeGraph)
	}
}

// graphBackend returns the optional capability or the refusal that names why
// the tool cannot answer.
func (s *Server) graphBackend() (GraphBackend, error) {
	if s.backend == nil {
		return nil, refusalUnsupported("brain_code is not available: no backend is configured",
			"brain mcp over a local or git brain")
	}
	backend, ok := s.backend.(GraphBackend)
	if !ok {
		return nil, unsupportedBackend("brain_code", "this backend does not serve a code graph; run brain mcp over a local checkout")
	}
	return backend, nil
}

// openGraph opens one project index for reading and reports failures as typed
// refusals instead of adapter text.
func (s *Server) openGraph(ctx context.Context, projectID, engine, action string) (ports.GraphIndex, error) {
	backend, err := s.graphBackend()
	if err != nil {
		return nil, err
	}
	index, err := backend.OpenGraph(ctx, projectID, engine)
	if err != nil {
		return nil, graphFailure(projectID, err)
	}
	return index, nil
}

// graphFailure maps the graph sentinels to a refusal that names the recovery.
func graphFailure(projectID string, err error) error {
	switch {
	case errors.Is(err, ErrGraphOff):
		return refusalUnsupported(
			"the code graph is off for "+projectID,
			"set graph.engine: native (then brain code index), pass engine=codegraph for an external index, or run brain code status")
	case errors.Is(err, ErrGraphAbsent):
		return refusalUnsupported(
			"no code graph index for "+projectID,
			"run brain code index --project "+projectID+" (native), or codegraph index in the checkout, then retry")
	case errors.Is(err, ErrGraphIndexMissing):
		return refusalUnsupported(
			"the code graph index for "+projectID+" is missing, empty or written by another schema",
			"rebuild it with brain code index --project "+projectID+" (native) or codegraph index")
	case errors.Is(err, ErrGraphReadOnly):
		return refusalUnsupported(
			"the codegraph engine is read-only; brain mcp cannot index it",
			"run codegraph index in the checkout, or switch graph.engine to native")
	case errors.Is(err, domain.ErrNotFound):
		return refusalNotFound(err.Error(),
			"run brain code search to find the exact symbol, or brain_graph(action=communities) to list community ids")
	case errors.Is(err, domain.ErrUnsupported):
		return refusalUnsupported(err.Error(),
			"run brain code status, or rebuild the index with brain code index --project "+projectID)
	default:
		return err
	}
}

// graphProject resolves the single project a graph call belongs to. Unlike a
// record read, a graph call can never span projects.
func (s *Server) graphProject(ctx context.Context, selector string) (string, error) {
	projectID, err := s.optionalProject(ctx, selector)
	if err != nil {
		return "", err
	}
	if projectID == "" {
		return "", refusalInvalidWith(
			"project is required: a code graph belongs to one checkout",
			"name the project with project=<id|alias|path>, or list ids with brain_projects(action=list)")
	}
	return projectID, nil
}

// graphWriteProject resolves the write destination of index and sync.
func (s *Server) graphWriteProject(ctx context.Context, selector string) (string, error) {
	target, err := s.writeTarget(ctx, selector)
	if err != nil {
		return "", err
	}
	if target.ID == "" {
		return "", refusalInvalidWith(
			"project is required: a code graph belongs to one checkout",
			"name the project with project=<id|alias|path>, or list ids with brain_projects(action=list)")
	}
	return target.ID, nil
}

// closeGraph releases an index; both engines implement Close but the port does
// not require it.
func closeGraph(index ports.GraphIndex) {
	if closer, ok := index.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}

// codeStatus reports the index identity, counts and age.
func (s *Server) codeStatus(ctx context.Context, args codeArgs, engine string) (toolResult, error) {
	projectID, err := s.graphProject(ctx, args.Project)
	if err != nil {
		return toolResult{}, err
	}
	index, err := s.openGraph(ctx, projectID, engine, "status")
	if err != nil {
		return toolResult{}, err
	}
	defer closeGraph(index)
	status, err := index.Status(ctx, projectID)
	if err != nil {
		return toolResult{}, graphFailure(projectID, err)
	}
	return toolResult{
		Text:       graphStatusText(status),
		Structured: map[string]any{"action": "status", "project": projectID, "status": status},
	}, nil
}

// codeWrite runs index or sync against the writable native store.
func (s *Server) codeWrite(ctx context.Context, action string, args codeArgs, engine string) (toolResult, error) {
	projectID, err := s.graphWriteProject(ctx, args.Project)
	if err != nil {
		return toolResult{}, err
	}
	backend, err := s.graphBackend()
	if err != nil {
		return toolResult{}, err
	}
	writer, err := backend.OpenGraphWriter(ctx, projectID, engine)
	if err != nil {
		return toolResult{}, graphFailure(projectID, err)
	}
	defer closeGraph(writer)
	var result ports.GraphSyncResult
	if action == "index" {
		result, err = writer.Index(ctx, projectID)
	} else {
		result, err = writer.Sync(ctx, projectID, nil)
	}
	if err != nil {
		return toolResult{}, graphFailure(projectID, err)
	}
	return toolResult{
		Text:       renderGraphSync(action, result),
		Structured: map[string]any{"action": action, "project": projectID, "sync": result},
	}, nil
}

// codeSearch finds symbols by exact name first, then full text, then substring.
func (s *Server) codeSearch(ctx context.Context, args codeArgs, engine string) (toolResult, error) {
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return toolResult{}, refusalInvalid("query is required for action=search")
	}
	if args.Limit < 0 || args.Limit > ports.MaxGraphSearchLimit {
		return toolResult{}, refusalInvalidf("limit must be between 1 and %d", ports.MaxGraphSearchLimit)
	}
	projectID, err := s.graphProject(ctx, args.Project)
	if err != nil {
		return toolResult{}, err
	}
	index, err := s.openGraph(ctx, projectID, engine, "search")
	if err != nil {
		return toolResult{}, err
	}
	defer closeGraph(index)
	hits, err := index.Search(ctx, ports.GraphQuery{Text: query, File: strings.TrimSpace(args.File), Limit: args.Limit})
	if err != nil {
		return toolResult{}, graphFailure(projectID, err)
	}
	if hits == nil {
		hits = []ports.SymbolHit{}
	}
	return toolResult{
		Text: renderCodeSearch(query, hits),
		Structured: map[string]any{
			"action": "search", "project": projectID,
			"search": map[string]any{"query": query, "hits": hits, "count": len(hits)},
		},
	}, nil
}

// codeNode returns one symbol with its source and its direct neighbours.
func (s *Server) codeNode(ctx context.Context, args codeArgs, engine string) (toolResult, error) {
	symbol := strings.TrimSpace(args.Symbol)
	if symbol == "" {
		return toolResult{}, refusalInvalid("symbol is required for action=node")
	}
	limit, err := neighborLimit(args.Limit)
	if err != nil {
		return toolResult{}, err
	}
	projectID, err := s.graphProject(ctx, args.Project)
	if err != nil {
		return toolResult{}, err
	}
	index, err := s.openGraph(ctx, projectID, engine, "node")
	if err != nil {
		return toolResult{}, err
	}
	defer closeGraph(index)
	view, err := index.Node(ctx, projectID, symbol, strings.TrimSpace(args.File), ports.NodeOpts{
		IncludeSource: true, IncludeCallers: true, IncludeCallees: true, Limit: limit,
	})
	if err != nil {
		return toolResult{}, graphFailure(projectID, err)
	}
	return toolResult{
		Text:       renderCodeNode(view),
		Structured: map[string]any{"action": "node", "project": projectID, "node": view},
	}, nil
}

// codeExplore ranks the symbols that answer a query, renders their source and
// reports the flow and the blast radius under the byte cap.
func (s *Server) codeExplore(ctx context.Context, args codeArgs, engine string) (toolResult, error) {
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return toolResult{}, refusalInvalid("query is required for action=explore")
	}
	maxFiles := args.Limit
	if maxFiles == 0 {
		maxFiles = ports.DefaultGraphExploreFiles
	}
	if maxFiles < 1 || maxFiles > ports.MaxGraphExploreFiles {
		return toolResult{}, refusalInvalidf("limit (max files) must be between 1 and %d", ports.MaxGraphExploreFiles)
	}
	maxBytes := args.MaxBytes
	if maxBytes == 0 {
		maxBytes = ports.MaxGraphExploreBytes
	}
	if maxBytes < 1 || maxBytes > ports.MaxGraphExploreBytes {
		return toolResult{}, refusalInvalidf("max_bytes must be between 1 and %d", ports.MaxGraphExploreBytes)
	}
	projectID, err := s.graphProject(ctx, args.Project)
	if err != nil {
		return toolResult{}, err
	}
	index, err := s.openGraph(ctx, projectID, engine, "explore")
	if err != nil {
		return toolResult{}, err
	}
	defer closeGraph(index)
	view, err := index.Explore(ctx, projectID, query, maxFiles)
	if err != nil {
		return toolResult{}, graphFailure(projectID, err)
	}
	if view.Bytes > maxBytes {
		return toolResult{}, refusalUnsupported(
			fmt.Sprintf("the explore answer is %d bytes, above max_bytes=%d; the engine renders under a fixed budget and never cuts an answer silently", view.Bytes, maxBytes),
			"raise max_bytes (at most "+strconv.Itoa(ports.MaxGraphExploreBytes)+") or lower limit to render fewer files")
	}
	return toolResult{
		Text:       renderCodeExplore(view),
		Structured: map[string]any{"action": "explore", "project": projectID, "explore": view},
	}, nil
}

// codeImpact walks incoming edges and reports what a change can reach.
func (s *Server) codeImpact(ctx context.Context, args codeArgs, engine string) (toolResult, error) {
	symbol := strings.TrimSpace(args.Symbol)
	if symbol == "" {
		return toolResult{}, refusalInvalid("symbol is required for action=impact")
	}
	depth := args.Depth
	if depth == 0 {
		depth = ports.DefaultGraphImpactDepth
	}
	if depth < 1 || depth > ports.MaxGraphImpactDepth {
		return toolResult{}, refusalInvalidf("depth must be between 1 and %d", ports.MaxGraphImpactDepth)
	}
	projectID, err := s.graphProject(ctx, args.Project)
	if err != nil {
		return toolResult{}, err
	}
	index, err := s.openGraph(ctx, projectID, engine, "impact")
	if err != nil {
		return toolResult{}, err
	}
	defer closeGraph(index)
	view, err := index.Impact(ctx, projectID, symbol, strings.TrimSpace(args.File), depth)
	if err != nil {
		return toolResult{}, graphFailure(projectID, err)
	}
	return toolResult{
		Text:       renderCodeImpact(view),
		Structured: map[string]any{"action": "impact", "project": projectID, "impact": view},
	}, nil
}

// neighborLimit bounds one neighbour list; zero selects the documented default.
func neighborLimit(limit int) (int, error) {
	switch {
	case limit == 0:
		return 20, nil
	case limit < 1 || limit > ports.MaxGraphSearchLimit:
		return 0, refusalInvalidf("limit must be between 1 and %d", ports.MaxGraphSearchLimit)
	default:
		return limit, nil
	}
}

// graphStatusText renders the identity and counts of one index. It is shared
// by brain_code(status) and brain_graph(stats), so the two cannot disagree.
func graphStatusText(status ports.GraphStatus) string {
	var b strings.Builder
	fmt.Fprintf(&b, "engine: %s\nproject: %s\n", status.Engine, status.Project)
	if status.Root != "" {
		fmt.Fprintf(&b, "root: %s\n", status.Root)
	}
	fmt.Fprintf(&b, "index: %s\n", status.DBPath)
	fmt.Fprintf(&b, "schema: %d (extraction %d)", status.SchemaVersion, status.ExtractionVersion)
	if status.IndexedWithVersion != "" {
		fmt.Fprintf(&b, " written by %s", status.IndexedWithVersion)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "state: %s\n", orUnknown(status.IndexState))
	fmt.Fprintf(&b, "files: %d\nnodes: %d\nedges: %d\n", status.Files, status.Nodes, status.Edges)
	if status.UnresolvedRefs > 0 {
		fmt.Fprintf(&b, "unresolved refs: %d\n", status.UnresolvedRefs)
	}
	if !status.IndexedAt.IsZero() {
		fmt.Fprintf(&b, "indexed: %s (%s ago)\n", status.IndexedAt.Format("2006-01-02 15:04:05Z07:00"), formatAge(status.AgeSeconds))
	}
	if status.Commit != "" {
		fmt.Fprintf(&b, "commit: %s\n", status.Commit)
	}
	if status.Dirty {
		b.WriteString("worktree: dirty when indexed\n")
	}
	fmt.Fprintf(&b, "size: %d bytes\n", status.SizeBytes)
	for _, note := range status.Notes {
		fmt.Fprintf(&b, "note: %s\n", note)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderGraphSync reports what an index or sync wrote.
func renderGraphSync(action string, result ports.GraphSyncResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", action, result.Engine)
	if result.Project != "" {
		fmt.Fprintf(&b, " project %s", result.Project)
	}
	fmt.Fprintf(&b, "\nchanged: %d\n", result.Changed)
	if result.Unsupported {
		b.WriteString("unsupported: the engine cannot index; no rows were written\n")
	}
	for _, note := range result.Notes {
		fmt.Fprintf(&b, "note: %s\n", note)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderCodeSearch lists the ranked hits with their location and match tier.
func renderCodeSearch(query string, hits []ports.SymbolHit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d symbol(s) for %q\n", len(hits), query)
	for _, hit := range hits {
		fmt.Fprintf(&b, "- %s %s  %s  [%s]\n", hit.Kind, hit.Name, symbolLocation(hit.GraphSymbol), hit.Match)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderCodeNode renders one symbol: the declaration, its source and its
// direct neighbours.
func renderCodeNode(view ports.NodeView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s  %s\n", view.Symbol.Kind, view.Symbol.Name, symbolLocation(view.Symbol))
	if view.Symbol.Signature != "" {
		fmt.Fprintf(&b, "signature: %s\n", view.Symbol.Signature)
	}
	if view.Symbol.Docstring != "" {
		fmt.Fprintf(&b, "doc: %s\n", firstLine(view.Symbol.Docstring))
	}
	if len(view.Source) > 0 {
		b.WriteString("\n")
		writeNumberedLines(&b, view.Source)
	}
	if view.SourceTruncated {
		b.WriteString("\n(source clipped)\n")
	}
	if len(view.Callers) > 0 {
		b.WriteString("\ncallers:\n")
		writeNeighbors(&b, view.Callers)
	}
	if len(view.Callees) > 0 {
		b.WriteString("\ncallees:\n")
		writeNeighbors(&b, view.Callees)
	}
	for _, note := range view.Notes {
		fmt.Fprintf(&b, "note: %s\n", note)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderCodeExplore renders the bounded explore answer: matching symbols, the
// source files that were rendered, the flow and the blast radius.
func renderCodeExplore(view ports.ExploreView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "query: %s (%d bytes)\n", view.Query, view.Bytes)
	if len(view.Symbols) > 0 {
		b.WriteString("\nsymbols:\n")
		for _, hit := range view.Symbols {
			fmt.Fprintf(&b, "- %s %s  %s  [%s]\n", hit.Kind, hit.Name, symbolLocation(hit.GraphSymbol), hit.Match)
		}
	}
	for _, file := range view.Files {
		fmt.Fprintf(&b, "\n## %s", file.Path)
		if file.Truncated {
			b.WriteString(" (clipped)")
		}
		b.WriteString("\n")
		writeNumberedLines(&b, file.Lines)
		if file.Note != "" {
			fmt.Fprintf(&b, "note: %s\n", file.Note)
		}
	}
	if len(view.Flow) > 0 {
		b.WriteString("\nflow:\n")
		for _, step := range view.Flow {
			if step.Via != "" {
				fmt.Fprintf(&b, "  -[%s]-> %s  %s\n", step.Via, step.Name, symbolLocation(step.GraphSymbol))
				continue
			}
			fmt.Fprintf(&b, "  %s  %s\n", step.Name, symbolLocation(step.GraphSymbol))
		}
	}
	if len(view.BlastRadius) > 0 {
		b.WriteString("\nblast radius:\n")
		for _, hit := range view.BlastRadius {
			fmt.Fprintf(&b, "- d%d %s  %s\n", hit.Distance, hit.Name, symbolLocation(hit.GraphSymbol))
		}
	}
	if len(view.Pointers) > 0 {
		b.WriteString("\nnot rendered:\n")
		for _, pointer := range view.Pointers {
			fmt.Fprintf(&b, "- %s\n", pointer)
		}
	}
	for _, note := range view.Notes {
		fmt.Fprintf(&b, "note: %s\n", note)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderCodeImpact renders the reverse blast radius and the tests it reaches.
func renderCodeImpact(view ports.ImpactView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s at depth %d: %d hit(s)\n", view.Symbol.Kind, view.Symbol.Name, view.Depth, len(view.Hits))
	for _, hit := range view.Hits {
		marker := ""
		if hit.Test {
			marker = " [test]"
		}
		fmt.Fprintf(&b, "- d%d via %s %s  %s%s\n", hit.Distance, hit.Via, hit.Name, symbolLocation(hit.GraphSymbol), marker)
	}
	if len(view.Files) > 0 {
		fmt.Fprintf(&b, "\nfiles: %s\n", strings.Join(view.Files, ", "))
	}
	if len(view.Tests) > 0 {
		fmt.Fprintf(&b, "tests: %s\n", strings.Join(view.Tests, ", "))
	}
	if view.Truncated {
		b.WriteString("(clipped at the blast-radius cap)\n")
	}
	for _, note := range view.Notes {
		fmt.Fprintf(&b, "note: %s\n", note)
	}
	return strings.TrimRight(b.String(), "\n")
}

// writeNeighbors renders one side of a symbol's edges.
func writeNeighbors(b *strings.Builder, neighbors []ports.GraphNeighbor) {
	for _, neighbor := range neighbors {
		count := ""
		if neighbor.Count > 1 {
			count = fmt.Sprintf(" x%d", neighbor.Count)
		}
		fmt.Fprintf(b, "- %s %s  %s  via %s%s\n", neighbor.Kind, neighbor.Name, symbolLocation(neighbor.GraphSymbol), neighbor.Edge, count)
	}
}

// writeNumberedLines renders source with a gap marker when the numbers jump, so
// a clipped answer never looks like contiguous code.
func writeNumberedLines(b *strings.Builder, lines []ports.SourceLine) {
	previous := 0
	for _, line := range lines {
		if previous > 0 && line.Number > previous+1 {
			b.WriteString("  ...\n")
		}
		fmt.Fprintf(b, "%5d\t%s\n", line.Number, line.Text)
		previous = line.Number
	}
}

// symbolLocation renders file:line for a symbol, skipping an absent range.
func symbolLocation(symbol ports.GraphSymbol) string {
	if symbol.File == "" {
		return symbol.ID
	}
	if symbol.StartLine <= 0 {
		return symbol.File
	}
	if symbol.EndLine > symbol.StartLine {
		return fmt.Sprintf("%s:%d-%d", symbol.File, symbol.StartLine, symbol.EndLine)
	}
	return fmt.Sprintf("%s:%d", symbol.File, symbol.StartLine)
}

// formatAge renders an age in the largest two units that matter.
func formatAge(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh%dm", seconds/3600, (seconds%3600)/60)
	default:
		return fmt.Sprintf("%dd%dh", seconds/86400, (seconds%86400)/3600)
	}
}
