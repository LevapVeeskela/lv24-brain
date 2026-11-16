package codeindex

import (
	"context"
	"fmt"

	"github.com/levapveeskela/lv24-brain/internal/adapters/graphalgo"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Cycles lists the strongly connected components of the import graph. A Go
// package import cycle and a TypeScript module cycle are the same shape: an
// edge from the importing node to the imported one.
//
// The walk reads only `imports` edges, so a recursive call or a mutual
// reference is not reported as an import cycle. Self-imports are counted and
// disclosed separately because they are not a cycle between modules.
func (i *Index) Cycles(ctx context.Context, project string, opts ports.CycleQuery) (ports.CyclesView, error) {
	view := ports.CyclesView{Cycles: []ports.GraphCycle{}}
	if err := i.ensureAnalyticsReadable(ctx); err != nil {
		return view, err
	}
	limit := clampLimit(opts.Limit, ports.DefaultGraphCycles, ports.MaxGraphCycles)
	memberLimit := clampLimit(opts.MemberLimit, ports.DefaultGraphCycleMembers, ports.MaxGraphCycleMembers)

	var selfLoops int
	if err := i.db.QueryRowContext(ctx,
		`SELECT count(*) FROM edges WHERE kind = 'imports' AND source = target`).Scan(&selfLoops); err != nil {
		return view, fmt.Errorf("codeindex: count self imports: %w", err)
	}

	rows, err := i.db.QueryContext(ctx,
		`SELECT source, target FROM edges
		 WHERE kind = 'imports' AND source <> target
		 ORDER BY source, target`)
	if err != nil {
		return view, fmt.Errorf("codeindex: read import edges: %w", err)
	}
	index := map[string]int{}
	ids := make([]string, 0, 64)
	edges := make([]graphalgo.Edge, 0, 64)
	lastFrom, lastTo := "", ""
	importEdges := 0
	for rows.Next() {
		var source, target string
		if err := rows.Scan(&source, &target); err != nil {
			_ = rows.Close()
			return view, fmt.Errorf("codeindex: read import edges: %w", err)
		}
		if source == lastFrom && target == lastTo {
			continue
		}
		lastFrom, lastTo = source, target
		from, ok := index[source]
		if !ok {
			from = len(ids)
			index[source] = from
			ids = append(ids, source)
		}
		to, ok := index[target]
		if !ok {
			to = len(ids)
			index[target] = to
			ids = append(ids, target)
		}
		edges = append(edges, graphalgo.Edge{A: from, B: to})
		importEdges++
	}
	rowErr := rows.Err()
	_ = rows.Close()
	if rowErr != nil {
		return view, fmt.Errorf("codeindex: read import edges: %w", rowErr)
	}
	view.SelfLoops = selfLoops
	view.Notes = append(view.Notes, fmt.Sprintf(
		"%d distinct import edge(s) over %d node(s); a self-import is not a cycle between modules", importEdges, len(ids)))
	if len(edges) == 0 {
		view.Notes = append(view.Notes, "the graph holds no imports edges, so it cannot answer import cycles")
		return view, nil
	}

	components := graphalgo.StronglyConnectedComponents(len(ids), edges)
	view.Total = len(components)
	for _, component := range components {
		view.Nodes += len(component.Members)
	}
	shown := components
	if len(shown) > limit {
		shown = shown[:limit]
		view.Truncated = true
	}
	allIDs := make([]string, 0, view.Nodes)
	displayMembers := make([][]string, len(shown))
	for index, component := range shown {
		members := component.Members
		if len(members) > memberLimit {
			members = members[:memberLimit]
			view.Truncated = true
		}
		for _, member := range members {
			displayMembers[index] = append(displayMembers[index], ids[member])
			allIDs = append(allIDs, ids[member])
		}
	}
	symbols, err := i.loadSymbols(ctx, allIDs)
	if err != nil {
		return view, err
	}
	byID := make(map[string]ports.GraphSymbol, len(symbols))
	for _, symbol := range symbols {
		byID[symbol.ID] = symbol
	}
	for index, component := range shown {
		cycle := ports.GraphCycle{Size: len(component.Members)}
		cycle.Members = make([]ports.GraphSymbol, 0, len(displayMembers[index]))
		for _, id := range displayMembers[index] {
			symbol, ok := byID[id]
			if !ok {
				symbol = ports.GraphSymbol{ID: id}
			}
			cycle.Members = append(cycle.Members, symbol)
		}
		cycle.MembersTruncated = len(component.Members) > len(displayMembers[index])
		view.Cycles = append(view.Cycles, cycle)
	}
	if view.Total > limit {
		view.Notes = append(view.Notes, fmt.Sprintf("showing the %d largest of %d import cycle(s); raise limit (at most %d) for more", limit, view.Total, ports.MaxGraphCycles))
	}
	if view.Total == 0 {
		view.Notes = append(view.Notes, "no import cycle was found: the import graph is acyclic")
	}
	return view, nil
}
