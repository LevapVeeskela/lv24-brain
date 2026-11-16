package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// graphActions is the closed action vocabulary of brain_graph, in output
// order. stats and neighbors read the symbol graph; path, communities,
// god_nodes and pr_impact read the optional analytics half of the same engine.
var graphActions = []string{"stats", "neighbors", "path", "communities", "god_nodes", "pr_impact"}

// graphAnalyticsUnavailable is the refusal of an engine that serves symbol
// navigation but not analytics. It is a property of the engine, not of the
// index, so it is not listed as a capability gap.
const graphAnalyticsUnavailable = "this graph engine serves stats and neighbors but not analytics"

// graphArgs is one brain_graph call.
type graphArgs struct {
	Action      string `json:"action"`
	Project     string `json:"project"`
	Symbol      string `json:"symbol"`
	Source      string `json:"source"`
	Target      string `json:"target"`
	File        string `json:"file"`
	Engine      string `json:"engine"`
	Limit       int    `json:"limit"`
	CommunityID int    `json:"community_id"`
	MemberLimit int    `json:"member_limit"`
	Depth       int    `json:"depth"`
	Base        string `json:"base"`
	Head        string `json:"head"`
}

func (s *Server) graphTool() tool {
	return tool{
		Name:        "brain_graph",
		Annotations: readOnlyAnnotations("Inspect the knowledge graph"),
		Description: "Inspect the code knowledge graph of one checkout. " +
			"stats reports the index identity, counts, communities and orphans; neighbors lists the direct edges of one symbol. " +
			"path finds the shortest chain between two symbols; communities lists detected communities or one with its members; " +
			"god_nodes ranks definitions by degree; pr_impact maps a local git diff to changed symbols, reverse impact, a risk tier and tests. " +
			"Analytics is read-only: pr_impact runs git diff against two refs and never fetches or stores pull-request data. " +
			"An engine that cannot answer analytics, or an off/absent index, answers with a typed UNSUPPORTED, never an empty collection.",
		InputSchema: objectSchema(nil, map[string]any{
			"action":       enumProp("stats (default), neighbors, path, communities, god_nodes or pr_impact.", graphActions),
			"project":      stringProp("Project id, alias or checkout path. Required: a graph belongs to one checkout."),
			"symbol":       stringProp("Symbol name or node id (neighbors)."),
			"source":       stringProp("Start symbol of a path query (path)."),
			"target":       stringProp("End symbol of a path query (path)."),
			"file":         stringProp("Repository-relative path that narrows a symbol to one file."),
			"engine":       enumProp("Graph engine for this call: native or codegraph. Omit to use graph.engine.", []string{ports.GraphEngineNative, ports.GraphEngineCodeGraph}),
			"limit":        intProp("Neighbours per side (neighbors, ≤100), communities listed (≤100) or definitions ranked (god_nodes, ≤50).", 0),
			"community_id": intProp("Select one community by its 1-based id (communities); 0 lists the largest.", 0),
			"member_limit": intProp("Members listed for a selected community, at most "+strconv.Itoa(ports.MaxGraphCommunityMembers)+"; 0 selects "+strconv.Itoa(ports.DefaultGraphCommunityMembers)+".", 0),
			"depth":        intProp("Hop limit of path, 1.."+strconv.Itoa(ports.MaxGraphPathDepth)+"; 0 selects "+strconv.Itoa(ports.DefaultGraphPathDepth)+".", 0),
			"base":         stringProp("Base git ref of pr_impact, for example HEAD~1 or origin/main."),
			"head":         stringProp("Head git ref of pr_impact; empty selects HEAD."),
		}),
		OutputSchema: graphOutputSchema(),
		Limits: map[string]int{
			"neighbors":         ports.MaxGraphSearchLimit,
			"communities":       ports.MaxGraphCommunities,
			"community_members": ports.MaxGraphCommunityMembers,
			"god_nodes":         ports.MaxGraphGodNodes,
			"path_depth":        ports.MaxGraphPathDepth,
			"path_nodes":        ports.MaxGraphPathNodes,
			"pr_symbols":        ports.MaxGraphPRSymbols,
			"pr_files":          ports.MaxGraphPRFiles,
		},
		ReadActions: graphActions,
		run:         s.graph,
	}
}

// graphOutputSchema is the envelope of the served actions: one typed group is
// present under the key named by action.
func graphOutputSchema() map[string]any {
	return outputObject(map[string]any{
		"action":      stringProp("Action that ran."),
		"project":     stringProp("Project the answer belongs to."),
		"stats":       objectProp("Index identity, counts and age (stats): ports.GraphStatus."),
		"analytics":   objectProp("Whole-graph counts (stats): ports.GraphStatsView."),
		"neighbors":   objectProp("One symbol with its direct callers and callees (neighbors): ports.NodeView."),
		"path":        objectProp("Shortest chain between two symbols (path): ports.GraphPath."),
		"communities": objectProp("Community listing or one community (communities): ports.CommunityView."),
		"god_nodes":   objectProp("Degree ranking (god_nodes): ports.GodNodesView."),
		"pr_impact":   objectProp("Read-only diff impact (pr_impact): ports.PRImpactView."),
	})
}

// graph dispatches one brain_graph call.
func (s *Server) graph(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args graphArgs
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	if action == "" {
		action = "stats"
	}
	engine, err := codeEngine(args.Engine)
	if err != nil {
		return toolResult{}, err
	}
	switch action {
	case "stats":
		return s.graphStats(ctx, args, engine)
	case "neighbors":
		return s.graphNeighbors(ctx, args, engine)
	case "path":
		return s.graphPath(ctx, args, engine)
	case "communities":
		return s.graphCommunities(ctx, args, engine)
	case "god_nodes":
		return s.graphGodNodes(ctx, args, engine)
	case "pr_impact":
		return s.graphPRImpact(ctx, args, engine)
	default:
		return toolResult{}, refusalInvalidf("unknown action %q (%s)", action, strings.Join(graphActions, ", "))
	}
}

// graphAnalytics returns the analytics half of an open index. The accessor is
// preferred over a type assertion because the composition root wraps the index
// in a classifying reader that forwards the accessor.
func graphAnalytics(index ports.GraphIndex) (ports.GraphAnalytics, bool) {
	if provider, ok := index.(interface {
		Analytics() (ports.GraphAnalytics, bool)
	}); ok {
		analytics, available := provider.Analytics()
		if !available || analytics == nil {
			return nil, false
		}
		return analytics, true
	}
	analytics, ok := index.(ports.GraphAnalytics)
	return analytics, ok
}

// openGraphAnalytics opens one project index and its analytics half.
func (s *Server) openGraphAnalytics(ctx context.Context, args graphArgs, engine string) (string, ports.GraphIndex, ports.GraphAnalytics, error) {
	projectID, err := s.graphProject(ctx, args.Project)
	if err != nil {
		return "", nil, nil, err
	}
	index, err := s.openGraph(ctx, projectID, engine, args.Action)
	if err != nil {
		return "", nil, nil, err
	}
	analytics, ok := graphAnalytics(index)
	if !ok {
		closeGraph(index)
		return "", nil, nil, refusalUnsupported(
			graphAnalyticsUnavailable+" for "+projectID,
			"brain_graph(action=stats|neighbors) still works; switch graph.engine to native for analytics")
	}
	return projectID, index, analytics, nil
}

// graphStats reports the identity and counts of the index behind the graph,
// plus the whole-graph analytics when the engine serves them.
func (s *Server) graphStats(ctx context.Context, args graphArgs, engine string) (toolResult, error) {
	projectID, err := s.graphProject(ctx, args.Project)
	if err != nil {
		return toolResult{}, err
	}
	index, err := s.openGraph(ctx, projectID, engine, "stats")
	if err != nil {
		return toolResult{}, err
	}
	defer closeGraph(index)
	status, err := index.Status(ctx, projectID)
	if err != nil {
		return toolResult{}, graphFailure(projectID, err)
	}
	structured := map[string]any{"action": "stats", "project": projectID, "stats": status}
	text := graphStatusText(status)
	analytics, ok := graphAnalytics(index)
	if !ok {
		structured["note"] = graphAnalyticsUnavailable
		text += "\nnote: " + graphAnalyticsUnavailable
		return toolResult{Text: text, Structured: structured}, nil
	}
	view, err := analytics.GraphStats(ctx, projectID)
	if err != nil {
		return toolResult{}, graphFailure(projectID, err)
	}
	structured["analytics"] = view
	return toolResult{Text: text + "\n" + renderGraphStats(view), Structured: structured}, nil
}

// graphNeighbors lists the direct callers and callees of one symbol.
func (s *Server) graphNeighbors(ctx context.Context, args graphArgs, engine string) (toolResult, error) {
	symbol := strings.TrimSpace(args.Symbol)
	if symbol == "" {
		return toolResult{}, refusalInvalid("symbol is required for action=neighbors")
	}
	limit, err := neighborLimit(args.Limit)
	if err != nil {
		return toolResult{}, err
	}
	projectID, err := s.graphProject(ctx, args.Project)
	if err != nil {
		return toolResult{}, err
	}
	index, err := s.openGraph(ctx, projectID, engine, "neighbors")
	if err != nil {
		return toolResult{}, err
	}
	defer closeGraph(index)
	view, err := index.Node(ctx, projectID, symbol, strings.TrimSpace(args.File), ports.NodeOpts{
		IncludeCallers: true, IncludeCallees: true, Limit: limit,
	})
	if err != nil {
		return toolResult{}, graphFailure(projectID, err)
	}
	return toolResult{
		Text:       renderGraphNeighbors(view),
		Structured: map[string]any{"action": "neighbors", "project": projectID, "neighbors": view},
	}, nil
}

// graphPath finds the shortest chain between two symbols.
func (s *Server) graphPath(ctx context.Context, args graphArgs, engine string) (toolResult, error) {
	source := strings.TrimSpace(args.Source)
	target := strings.TrimSpace(args.Target)
	if source == "" || target == "" {
		return toolResult{}, refusalInvalid("source and target are required for action=path")
	}
	depth := args.Depth
	if depth == 0 {
		depth = ports.DefaultGraphPathDepth
	}
	if depth < 0 || depth > ports.MaxGraphPathDepth {
		return toolResult{}, refusalInvalidf("depth must be between 0 and %d (0 selects %d)", ports.MaxGraphPathDepth, ports.DefaultGraphPathDepth)
	}
	projectID, index, analytics, err := s.openGraphAnalytics(ctx, args, engine)
	if err != nil {
		return toolResult{}, err
	}
	defer closeGraph(index)
	view, err := analytics.ShortestPath(ctx, projectID, source, target, depth)
	if err != nil {
		return toolResult{}, graphFailure(projectID, err)
	}
	return toolResult{
		Text:       renderGraphPath(view),
		Structured: map[string]any{"action": "path", "project": projectID, "path": view},
	}, nil
}

// graphCommunities lists communities, or one community with its members.
func (s *Server) graphCommunities(ctx context.Context, args graphArgs, engine string) (toolResult, error) {
	if args.Limit < 0 || args.Limit > ports.MaxGraphCommunities {
		return toolResult{}, refusalInvalidf("limit must be between 0 and %d (0 selects %d)", ports.MaxGraphCommunities, ports.DefaultGraphCommunities)
	}
	if args.MemberLimit < 0 || args.MemberLimit > ports.MaxGraphCommunityMembers {
		return toolResult{}, refusalInvalidf("member_limit must be between 0 and %d (0 selects %d)", ports.MaxGraphCommunityMembers, ports.DefaultGraphCommunityMembers)
	}
	if args.CommunityID < 0 {
		return toolResult{}, refusalInvalid("community_id must be a positive community id")
	}
	projectID, index, analytics, err := s.openGraphAnalytics(ctx, args, engine)
	if err != nil {
		return toolResult{}, err
	}
	defer closeGraph(index)
	view, err := analytics.Communities(ctx, projectID, ports.CommunityQuery{
		ID: args.CommunityID, Limit: args.Limit, MemberLimit: args.MemberLimit,
	})
	if err != nil {
		return toolResult{}, graphFailure(projectID, err)
	}
	return toolResult{
		Text:       renderGraphCommunities(view),
		Structured: map[string]any{"action": "communities", "project": projectID, "communities": view},
	}, nil
}

// graphGodNodes ranks definitions by degree.
func (s *Server) graphGodNodes(ctx context.Context, args graphArgs, engine string) (toolResult, error) {
	if args.Limit < 0 || args.Limit > ports.MaxGraphGodNodes {
		return toolResult{}, refusalInvalidf("limit must be between 0 and %d (0 selects %d)", ports.MaxGraphGodNodes, ports.DefaultGraphGodNodes)
	}
	projectID, index, analytics, err := s.openGraphAnalytics(ctx, args, engine)
	if err != nil {
		return toolResult{}, err
	}
	defer closeGraph(index)
	view, err := analytics.GodNodes(ctx, projectID, args.Limit)
	if err != nil {
		return toolResult{}, graphFailure(projectID, err)
	}
	return toolResult{
		Text:       renderGraphGodNodes(view),
		Structured: map[string]any{"action": "god_nodes", "project": projectID, "god_nodes": view},
	}, nil
}

// graphPRImpact maps a local git diff to changed symbols and their reverse
// blast radius.
func (s *Server) graphPRImpact(ctx context.Context, args graphArgs, engine string) (toolResult, error) {
	base := strings.TrimSpace(args.Base)
	if base == "" {
		return toolResult{}, refusalInvalid("base is required for action=pr_impact; pass a git ref such as HEAD~1")
	}
	projectID, index, analytics, err := s.openGraphAnalytics(ctx, args, engine)
	if err != nil {
		return toolResult{}, err
	}
	defer closeGraph(index)
	view, err := analytics.PRImpact(ctx, projectID, base, strings.TrimSpace(args.Head))
	if err != nil {
		return toolResult{}, graphFailure(projectID, err)
	}
	return toolResult{
		Text:       renderGraphPRImpact(view),
		Structured: map[string]any{"action": "pr_impact", "project": projectID, "pr_impact": view},
	}, nil
}

// renderGraphStats renders the whole-graph counts below the index envelope.
func renderGraphStats(view ports.GraphStatsView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "analytics: %d communities, %d orphan definition(s)\n", view.Communities, view.Orphans)
	if len(view.Languages) > 0 {
		b.WriteString("languages: ")
		b.WriteString(renderCountPairs(view.Languages, 6))
		b.WriteString("\n")
	}
	if len(view.NodesByKind) > 0 {
		b.WriteString("kinds: ")
		b.WriteString(renderCountPairs(view.NodesByKind, 8))
		b.WriteString("\n")
	}
	for _, note := range view.Notes {
		fmt.Fprintf(&b, "note: %s\n", note)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderCountPairs renders a count map ordered by count, then name, capped.
func renderCountPairs(counts map[string]int, limit int) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		if counts[keys[left]] != counts[keys[right]] {
			return counts[keys[left]] > counts[keys[right]]
		}
		return keys[left] < keys[right]
	})
	parts := make([]string, 0, limit+1)
	for index, key := range keys {
		if index == limit {
			parts = append(parts, fmt.Sprintf("and %d more", len(keys)-limit))
			break
		}
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, ", ")
}

// renderGraphPath renders the chain, its direction and its weakest hop.
func renderGraphPath(view ports.GraphPath) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s -> %s\n", view.From.Name, view.To.Name)
	if !view.Found {
		b.WriteString("no path found\n")
		for _, note := range view.Notes {
			fmt.Fprintf(&b, "note: %s\n", note)
		}
		return strings.TrimRight(b.String(), "\n")
	}
	direction := "directed"
	if !view.Directed {
		direction = "undirected"
	}
	fmt.Fprintf(&b, "%d hop(s), %s, weakest confidence %.2f\n", view.Hops, direction, view.Confidence)
	for index, step := range view.Steps {
		if index == 0 {
			fmt.Fprintf(&b, "  %s  %s\n", step.Name, symbolLocation(step.GraphSymbol))
			continue
		}
		marker := ""
		if step.Reverse {
			marker = " (reverse)"
		}
		fmt.Fprintf(&b, "  -[%s%s]-> %s  %s\n", step.Edge, marker, step.Name, symbolLocation(step.GraphSymbol))
	}
	for _, note := range view.Notes {
		fmt.Fprintf(&b, "note: %s\n", note)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderGraphCommunities renders a listing or one community with members.
func renderGraphCommunities(view ports.CommunityView) string {
	var b strings.Builder
	if len(view.Communities) == 1 && len(view.Communities[0].Members) > 0 {
		community := view.Communities[0]
		fmt.Fprintf(&b, "#%d %s: %d member(s), cohesion %.3f, signature %s\n",
			community.ID, community.Name, community.Size, community.Cohesion, community.Signature)
		if community.Top.Name != "" {
			fmt.Fprintf(&b, "top: %s %s\n", community.Top.Kind, symbolLocation(community.Top))
		}
		b.WriteString("members:\n")
		for _, member := range community.Members {
			fmt.Fprintf(&b, "- %s %s  %s\n", member.Kind, member.Name, symbolLocation(member))
		}
		if community.MembersTruncated {
			b.WriteString("(member list clipped; raise member_limit)\n")
		}
	} else {
		fmt.Fprintf(&b, "%d communities", view.Total)
		if view.Truncated {
			fmt.Fprintf(&b, " (showing %d)", len(view.Communities))
		}
		b.WriteString("\n")
		for _, community := range view.Communities {
			fmt.Fprintf(&b, "- #%d %s: %d member(s), cohesion %.3f, signature %s\n",
				community.ID, community.Name, community.Size, community.Cohesion, community.Signature)
		}
	}
	for _, note := range view.Notes {
		fmt.Fprintf(&b, "note: %s\n", note)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderGraphGodNodes renders the ranking and the facts behind each rank.
func renderGraphGodNodes(view ports.GodNodesView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d definition(s) considered; %d ranked\n", view.Total, len(view.Nodes))
	for index, node := range view.Nodes {
		community := ""
		if node.Community != "" {
			community = fmt.Sprintf(", community %s", node.Community)
		}
		test := ""
		if node.Test {
			test = " [test]"
		}
		fmt.Fprintf(&b, "%d. %s %s  %s\n   degree %d (in %d, out %d), centrality %.4f%s%s\n   why: %s\n",
			index+1, node.Kind, node.Name, symbolLocation(node.GraphSymbol),
			node.Degree, node.InDegree, node.OutDegree, node.Centrality, community, test, node.Why)
	}
	for _, note := range view.Notes {
		fmt.Fprintf(&b, "note: %s\n", note)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderGraphPRImpact renders the diff, its changed symbols and its radius.
func renderGraphPRImpact(view ports.PRImpactView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s..%s: %d file(s) changed, %d changed symbol(s), %d reachable\n",
		view.Base, view.Head, view.ChangedFiles, len(view.ChangedSymbols), view.TotalImpact)
	fmt.Fprintf(&b, "risk: %s\n", view.Risk)
	if len(view.Files) > 0 {
		b.WriteString("files:\n")
		for _, file := range view.Files {
			fmt.Fprintf(&b, "- %s %s (%d symbol(s))\n", file.Status, file.Path, file.Symbols)
		}
	}
	if len(view.ChangedSymbols) > 0 {
		b.WriteString("changed symbols:\n")
		for _, symbol := range view.ChangedSymbols {
			fmt.Fprintf(&b, "- %s %s  %s\n", symbol.Kind, symbol.Name, symbolLocation(symbol))
		}
	}
	if len(view.Impact) > 0 {
		b.WriteString("impact:\n")
		for _, hit := range view.Impact {
			test := ""
			if hit.Test {
				test = " [test]"
			}
			fmt.Fprintf(&b, "- d%d via %s %s  %s%s\n", hit.Distance, hit.Via, hit.Name, symbolLocation(hit.GraphSymbol), test)
		}
	}
	if len(view.Tests) > 0 {
		fmt.Fprintf(&b, "tests: %s\n", strings.Join(view.Tests, ", "))
	}
	for _, note := range view.Notes {
		fmt.Fprintf(&b, "note: %s\n", note)
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderGraphNeighbors renders one symbol and both edge directions.
func renderGraphNeighbors(view ports.NodeView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s  %s\n", view.Symbol.Kind, view.Symbol.Name, symbolLocation(view.Symbol))
	fmt.Fprintf(&b, "callers: %d\ncallees: %d\n", len(view.Callers), len(view.Callees))
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
