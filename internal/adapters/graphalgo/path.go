package graphalgo

import "sort"

// Neighbor is one hop discovered by an expander. Reverse marks a hop that
// followed an incoming dependency edge instead of an outgoing one.
type Neighbor struct {
	ID         string
	Edge       string
	Line       int
	Confidence float64
	Reverse    bool
}

// PathStep is one node of a path. The first step carries the start id only;
// each later step carries the hop that reached it.
type PathStep struct {
	ID         string
	Edge       string
	Line       int
	Confidence float64
	Reverse    bool
}

// ShortestPath is a deterministic breadth-first search over string node ids.
//
// expand receives one whole frontier and returns the neighbours of each id in
// it. The adapter decides which edge kinds count and whether reverse hops are
// included, so the same search serves a directed call walk and an undirected
// fallback without a second implementation.
//
// Frontier ids and neighbour lists are sorted before they are expanded, so the
// path found depends only on the graph, not on database row order. A goal is
// visited at its shallowest depth; the search stops at maxDepth hops. The
// returned steps include the start, so Hops is len(steps)-1.
func ShortestPath(start, goal string, maxDepth int, expand func(frontier []string) (map[string][]Neighbor, error)) ([]PathStep, bool, error) {
	if start == "" || goal == "" {
		return nil, false, nil
	}
	if maxDepth < 1 {
		maxDepth = 1
	}
	if start == goal {
		return []PathStep{{ID: start}}, true, nil
	}
	parent := map[string]PathStep{start: {ID: start}}
	previous := map[string]string{}
	frontier := []string{start}
	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		sort.Strings(frontier)
		neighbors, err := expand(frontier)
		if err != nil {
			return nil, false, err
		}
		next := make([]string, 0, len(frontier))
		for _, node := range frontier {
			hops := append([]Neighbor(nil), neighbors[node]...)
			sort.Slice(hops, func(left, right int) bool {
				if hops[left].ID != hops[right].ID {
					return hops[left].ID < hops[right].ID
				}
				if hops[left].Edge != hops[right].Edge {
					return hops[left].Edge < hops[right].Edge
				}
				return hops[left].Line < hops[right].Line
			})
			for _, hop := range hops {
				if hop.ID == "" {
					continue
				}
				if _, seen := parent[hop.ID]; seen {
					continue
				}
				parent[hop.ID] = PathStep{
					ID: hop.ID, Edge: hop.Edge, Line: hop.Line,
					Confidence: hop.Confidence, Reverse: hop.Reverse,
				}
				previous[hop.ID] = node
				if hop.ID == goal {
					return reconstructPath(previous, parent, goal), true, nil
				}
				next = append(next, hop.ID)
			}
		}
		frontier = next
	}
	return nil, false, nil
}

// reconstructPath walks the parent links back to the start and reverses them.
func reconstructPath(previous map[string]string, parent map[string]PathStep, goal string) []PathStep {
	reversed := make([]PathStep, 0, 8)
	for id := goal; ; {
		reversed = append(reversed, parent[id])
		predecessor, ok := previous[id]
		if !ok {
			break
		}
		id = predecessor
	}
	steps := make([]PathStep, 0, len(reversed))
	for index := len(reversed) - 1; index >= 0; index-- {
		steps = append(steps, reversed[index])
	}
	return steps
}
