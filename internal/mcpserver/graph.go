package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// graphActions is the closed action vocabulary of brain_graph. stats and
// neighbors read the symbol graph; the analytics actions are listed so a caller
// gets a typed UNSUPPORTED instead of "unknown action", and they are never
// answered with invented nodes, edges or communities.
var graphActions = []string{"stats", "neighbors", "path", "communities", "god_nodes", "pr_impact"}

// graphUnsupported names the analytics actions this release cannot serve and
// the path that can do the equivalent work. It is the single source of both the
// refusal and the capability gap.
var graphUnsupported = map[string]CapabilityGap{
	"path": {
		Tool: "brain_graph", Action: "path",
		Reason:      "path search over the symbol graph is not implemented; graph stats and neighbours are served",
		Remediation: "brain_code(action=impact) for the blast radius of one symbol, or brain code explore for the flow between named symbols",
	},
	"communities": {
		Tool: "brain_graph", Action: "communities",
		Reason:      "community detection is not implemented; no community is invented from memory links",
		Remediation: "brain_code(action=status) for the graph size, or brain search for records that cluster a topic",
	},
	"god_nodes": {
		Tool: "brain_graph", Action: "god_nodes",
		Reason:      "degree ranking is not implemented; a list built from record links would not be a code graph",
		Remediation: "brain_code(action=search) for the symbols you need, or wait for the analytics slice",
	},
	"pr_impact": {
		Tool: "brain_graph", Action: "pr_impact",
		Reason:      "PR impact needs pull-request data this server does not fetch or store",
		Remediation: "brain_code(action=impact) for the blast radius of the changed symbols",
	},
}

// graphArgs is one brain_graph call.
type graphArgs struct {
	Action  string `json:"action"`
	Project string `json:"project"`
	Symbol  string `json:"symbol"`
	Source  string `json:"source"`
	Target  string `json:"target"`
	File    string `json:"file"`
	Engine  string `json:"engine"`
	Limit   int    `json:"limit"`
}

func (s *Server) graphTool() tool {
	return tool{
		Name:        "brain_graph",
		Annotations: readOnlyAnnotations("Inspect the knowledge graph"),
		Description: "Inspect the code knowledge graph of one checkout. " +
			"stats reports the index identity and counts; neighbors lists the direct edges of one symbol. " +
			"path, communities, god_nodes and pr_impact are not implemented in this release and answer UNSUPPORTED with a working alternative; " +
			"no empty collection or fabricated ranking is ever returned.",
		InputSchema: objectSchema(nil, map[string]any{
			"action":  enumProp("stats (default) or neighbors; path, communities, god_nodes and pr_impact answer UNSUPPORTED.", graphActions),
			"project": stringProp("Project id, alias or checkout path. Required: a graph belongs to one checkout."),
			"symbol":  stringProp("Symbol name or node id (neighbors)."),
			"source":  stringProp("Start symbol of a path query (path is UNSUPPORTED in this release)."),
			"target":  stringProp("End symbol of a path query (path is UNSUPPORTED in this release)."),
			"file":    stringProp("Repository-relative path that narrows a symbol to one file."),
			"engine":  enumProp("Graph engine for this call: native or codegraph. Omit to use graph.engine.", []string{ports.GraphEngineNative, ports.GraphEngineCodeGraph}),
			"limit":   intProp("Maximum neighbours per side; 0 selects 20.", 0),
		}),
		OutputSchema: graphOutputSchema(),
		Limits: map[string]int{
			"neighbors": ports.MaxGraphSearchLimit,
		},
		ReadActions: graphActions[:2],
		run:         s.graph,
	}
}

// graphOutputSchema is the envelope of the served actions. The analytics
// actions return a refusal, never a structured document.
func graphOutputSchema() map[string]any {
	return outputObject(map[string]any{
		"action":    stringProp("Action that ran."),
		"project":   stringProp("Project the answer belongs to."),
		"stats":     objectProp("Index identity, counts and age (stats): ports.GraphStatus."),
		"neighbors": objectProp("One symbol with its direct callers and callees (neighbors): ports.NodeView."),
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
	if gap, unsupported := graphUnsupported[action]; unsupported {
		return toolResult{}, refusalUnsupported("brain_graph action "+action+": "+gap.Reason, gap.Remediation)
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
	default:
		return toolResult{}, refusalInvalidf("unknown action %q (%s)", action, strings.Join(graphActions, ", "))
	}
}

// graphStats reports the identity and counts of the index behind the graph.
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
	return toolResult{
		Text:       graphStatusText(status),
		Structured: map[string]any{"action": "stats", "project": projectID, "stats": status},
	}, nil
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
