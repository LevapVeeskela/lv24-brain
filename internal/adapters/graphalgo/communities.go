// Package graphalgo is the shared analytics kernel of the two graph adapters.
//
// It holds the deterministic algorithms that must not differ between the native
// store and the external codegraph index: community detection, breadth-first
// path search and the read-only git diff that feeds PR impact. It works on
// integer node indexes and string node ids only; every store query, symbol DTO
// and confidence rule stays in the adapter.
package graphalgo

import (
	"math"
	"sort"
)

// Edge is one undirected weighted link between two node indexes. A weight of
// zero or less is treated as 1. Self-loops and out-of-range endpoints are
// ignored.
type Edge struct {
	A, B   int
	Weight float64
}

// CommunityOptions tunes DetectCommunities. The zero value selects resolution
// 1.0, 12 local-moving passes and 8 aggregation levels.
type CommunityOptions struct {
	// Resolution is the Louvain resolution parameter: above 1 finds more,
	// smaller communities, below 1 fewer, larger ones.
	Resolution float64
	// MaxPasses bounds the local-moving passes of one level.
	MaxPasses int
	// MaxLevels bounds the aggregation levels.
	MaxLevels int
	// HubPercentile removes the highest-degree fraction of nodes from the
	// partition and reattaches them afterwards. It is ignored below 500 nodes
	// because one hub changes a tiny graph more than it clarifies it.
	HubPercentile float64
}

// Community is one detected cluster. Members are sorted node indexes.
type Community struct {
	Members []int
	// Cohesion is the internal edge weight over n*(n-1)/2, the density of the
	// undirected member subgraph. A singleton is 0.
	Cohesion float64
	// InternalWeight is the summed weight of the edges inside the community.
	InternalWeight float64
}

// Partition is a complete assignment: every node belongs to exactly one
// community. Assignment maps a node index to an index in Communities, and
// Communities is ordered by size, then by the smallest member.
type Partition struct {
	Assignment  []int
	Communities []Community
}

// levelLink is one undirected edge of the current aggregation level.
type levelLink struct {
	to     int
	weight float64
}

// levelGraph is the graph of one Louvain level. Self-loops are kept separately
// because they carry a community's internal weight and cancel out of the move
// comparison.
type levelGraph struct {
	adjacency [][]levelLink
	selfLoop  []float64
	degree    []float64
	total     float64
}

// DetectCommunities partitions an undirected graph with deterministic Louvain
// modularity optimisation.
//
// Nodes are visited in index order; a move must strictly improve modularity, so
// an equal-gain move keeps the current community; after each level the
// communities become the nodes of the next graph and the passes repeat. The
// result depends only on the input, never on Go's map iteration order or on a
// random seed.
//
// Two refinements keep the partition navigable on a hub-dominated code graph:
// the best modularity seen across levels and across a forward/reverse node
// order wins (a greedy order can otherwise strand a dense group), and the
// highest-degree HubPercentile nodes are partitioned separately and reattached
// to the community they share the most edge weight with. The reported cohesion
// is what tells a caller whether a split is meaningful.
func DetectCommunities(nodes int, edges []Edge, opts CommunityOptions) Partition {
	if nodes <= 0 {
		return Partition{Assignment: []int{}}
	}
	resolution := opts.Resolution
	if resolution <= 0 {
		resolution = 1
	}
	passes := opts.MaxPasses
	if passes <= 0 {
		passes = 12
	}
	levels := opts.MaxLevels
	if levels <= 0 {
		levels = 8
	}
	initial := initialGraph(nodes, edges)
	hubCut := 0
	if opts.HubPercentile > 0 && nodes >= minHubGraphNodes {
		hubCut = int(float64(nodes) * opts.HubPercentile)
	}
	if hubCut < 1 {
		return buildPartition(nodes, edges, partitionLevels(initial, edges, resolution, passes, levels))
	}
	assignment := partitionWithHubs(initial, nodes, edges, hubCut, resolution, passes, levels)
	return buildPartition(nodes, edges, assignment)
}

// minHubGraphNodes is the size below which hub exclusion is noise: on a tiny
// fixture the top 5% is one node and removing it changes nothing useful.
const minHubGraphNodes = 500

// partitionLevels runs the multi-level search on one graph and returns the best
// assignment of its nodes found at any level or order.
func partitionLevels(initial *levelGraph, edges []Edge, resolution float64, passes, levels int) []int {
	nodes := len(initial.adjacency)
	assignment := make([]int, nodes)
	for node := range assignment {
		assignment[node] = node
	}
	bestAssignment := append([]int(nil), assignment...)
	bestQuality := modularityOf(nodes, edges, assignment, resolution)
	current := initial
	if current.total > 0 {
		for level := 0; level < levels; level++ {
			// Two deterministic orders: forward catches left-to-right density,
			// reverse avoids the classic local optimum where a middle node
			// joins a singleton and strands its clique. The better modularity
			// wins; a tie keeps the forward result.
			var bestLevel []int
			bestLevelQuality := math.Inf(-1)
			bestCount := len(current.adjacency)
			for _, reverse := range []bool{false, true} {
				levelAssignment, count := localMoving(current, resolution, passes, reverse)
				candidate := make([]int, nodes)
				for node := range candidate {
					candidate[node] = levelAssignment[assignment[node]]
				}
				quality := modularityOf(nodes, edges, candidate, resolution)
				if quality > bestLevelQuality+1e-12 {
					bestLevel, bestLevelQuality, bestCount = levelAssignment, quality, count
				}
			}
			if bestLevel == nil || bestCount == len(current.adjacency) {
				break
			}
			for node := range assignment {
				assignment[node] = bestLevel[assignment[node]]
			}
			if bestLevelQuality > bestQuality+1e-12 {
				bestQuality = bestLevelQuality
				bestAssignment = append(bestAssignment[:0], assignment...)
			}
			if bestCount <= 1 || level == levels-1 {
				break
			}
			current = aggregate(current, bestLevel, bestCount)
		}
	}
	return bestAssignment
}

// partitionWithHubs removes the highest-degree nodes, partitions the rest and
// reattaches every removed node to the community with which it shares the most
// edge weight. A hub with no kept neighbour gets its own community.
func partitionWithHubs(initial *levelGraph, nodes int, edges []Edge, hubCut int, resolution float64, passes, levels int) []int {
	order := make([]int, nodes)
	for node := range order {
		order[node] = node
	}
	sort.Slice(order, func(left, right int) bool {
		if initial.degree[order[left]] != initial.degree[order[right]] {
			return initial.degree[order[left]] > initial.degree[order[right]]
		}
		return order[left] < order[right]
	})
	hub := make([]bool, nodes)
	for index := 0; index < hubCut; index++ {
		hub[order[index]] = true
	}
	sub := make([]int, nodes)
	for node := range sub {
		sub[node] = -1
	}
	kept := 0
	for node := 0; node < nodes; node++ {
		if !hub[node] {
			sub[node] = kept
			kept++
		}
	}
	reduced := make([]Edge, 0, len(edges))
	for node := 0; node < nodes; node++ {
		if hub[node] {
			continue
		}
		for _, link := range initial.adjacency[node] {
			if node > link.to || hub[link.to] {
				continue
			}
			reduced = append(reduced, Edge{A: sub[node], B: sub[link.to], Weight: link.weight})
		}
	}
	assignment := make([]int, nodes)
	subAssignment := partitionLevels(initialGraph(kept, reduced), reduced, resolution, passes, levels)
	for node := 0; node < nodes; node++ {
		if sub[node] >= 0 {
			assignment[node] = subAssignment[sub[node]]
		}
	}
	next := 0
	for _, id := range subAssignment {
		if id >= next {
			next = id + 1
		}
	}
	for node := 0; node < nodes; node++ {
		if !hub[node] {
			continue
		}
		weights := make(map[int]float64, len(initial.adjacency[node]))
		for _, link := range initial.adjacency[node] {
			if hub[link.to] {
				continue
			}
			weights[assignment[link.to]] += link.weight
		}
		best := next
		bestWeight := 0.0
		candidates := make([]int, 0, len(weights))
		for candidate := range weights {
			candidates = append(candidates, candidate)
		}
		sort.Ints(candidates)
		for _, candidate := range candidates {
			if weights[candidate] > bestWeight || (weights[candidate] == bestWeight && candidate < best) {
				best, bestWeight = candidate, weights[candidate]
			}
		}
		if best == next {
			next++
		}
		assignment[node] = best
	}
	adoptSingletons(initial, assignment)
	return assignment
}

// adoptSingletons attaches a one-member community to the community it shares
// the most edge weight with. Removing hubs can leave a non-hub node connected
// only to hubs; without this pass it would stay a singleton although it has a
// real neighbour. True orphans (no edge at all) stay singleton and are reported
// as orphans.
func adoptSingletons(graph *levelGraph, assignment []int) {
	sizes := make(map[int]int, len(assignment))
	for _, id := range assignment {
		sizes[id]++
	}
	for pass := 0; pass < 2; pass++ {
		moved := false
		for node := range assignment {
			if sizes[assignment[node]] != 1 || len(graph.adjacency[node]) == 0 {
				continue
			}
			weights := make(map[int]float64, len(graph.adjacency[node]))
			for _, link := range graph.adjacency[node] {
				weights[assignment[link.to]] += link.weight
			}
			best, bestWeight := -1, 0.0
			candidates := make([]int, 0, len(weights))
			for candidate := range weights {
				candidates = append(candidates, candidate)
			}
			sort.Ints(candidates)
			for _, candidate := range candidates {
				if candidate == assignment[node] {
					continue
				}
				if weights[candidate] > bestWeight || (weights[candidate] == bestWeight && candidate < best) {
					best, bestWeight = candidate, weights[candidate]
				}
			}
			if best < 0 {
				continue
			}
			sizes[assignment[node]]--
			assignment[node] = best
			sizes[best]++
			moved = true
		}
		if !moved {
			break
		}
	}
}

// modularityOf scores a partition on the original graph. It is the objective
// the local-moving passes optimise, so the best level and the best order can be
// compared without trusting the last pass.
func modularityOf(nodes int, edges []Edge, assignment []int, resolution float64) float64 {
	if nodes == 0 {
		return 0
	}
	degree := make([]float64, nodes)
	internal := make([]float64, nodes)
	total := 0.0
	for _, edge := range edges {
		if edge.A < 0 || edge.B < 0 || edge.A >= nodes || edge.B >= nodes || edge.A == edge.B {
			continue
		}
		weight := edgeWeight(edge.Weight)
		degree[edge.A] += weight
		degree[edge.B] += weight
		total += weight
		if assignment[edge.A] == assignment[edge.B] {
			internal[assignment[edge.A]] += weight
		}
	}
	if total <= 0 {
		return 0
	}
	communityDegree := make(map[int]float64, nodes)
	for node, id := range assignment {
		communityDegree[id] += degree[node]
	}
	quality := 0.0
	for id, k := range communityDegree {
		quality += internal[id]/total - resolution*math.Pow(k/(2*total), 2)
	}
	return quality
}

// initialGraph builds the first level, merging duplicate edges and ignoring
// self-loops.
func initialGraph(nodes int, edges []Edge) *levelGraph {
	graph := &levelGraph{
		adjacency: make([][]levelLink, nodes),
		selfLoop:  make([]float64, nodes),
		degree:    make([]float64, nodes),
	}
	for _, edge := range edges {
		if edge.A < 0 || edge.B < 0 || edge.A >= nodes || edge.B >= nodes || edge.A == edge.B {
			continue
		}
		weight := edgeWeight(edge.Weight)
		graph.adjacency[edge.A] = append(graph.adjacency[edge.A], levelLink{to: edge.B, weight: weight})
		graph.adjacency[edge.B] = append(graph.adjacency[edge.B], levelLink{to: edge.A, weight: weight})
		graph.degree[edge.A] += weight
		graph.degree[edge.B] += weight
		graph.total += weight
	}
	for node := range graph.adjacency {
		graph.adjacency[node] = mergeLinks(graph.adjacency[node])
	}
	return graph
}

// aggregate turns one level's communities into the nodes of the next level.
func aggregate(current *levelGraph, community []int, count int) *levelGraph {
	next := &levelGraph{
		adjacency: make([][]levelLink, count),
		selfLoop:  make([]float64, count),
		degree:    make([]float64, count),
		total:     current.total,
	}
	for node := range current.adjacency {
		id := community[node]
		next.selfLoop[id] += current.selfLoop[node]
		next.degree[id] += current.degree[node]
		for _, link := range current.adjacency[node] {
			if node > link.to {
				continue
			}
			target := community[link.to]
			if target == id {
				next.selfLoop[id] += link.weight
				continue
			}
			next.adjacency[id] = append(next.adjacency[id], levelLink{to: target, weight: link.weight})
			next.adjacency[target] = append(next.adjacency[target], levelLink{to: id, weight: link.weight})
		}
	}
	for node := range next.adjacency {
		next.adjacency[node] = mergeLinks(next.adjacency[node])
	}
	return next
}

// localMoving runs the modularity local-moving passes of one level and returns
// a contiguous community id per node, ordered by the smallest member. reverse
// visits the nodes from the last to the first.
func localMoving(graph *levelGraph, resolution float64, passes int, reverse bool) ([]int, int) {
	nodes := len(graph.adjacency)
	order := make([]int, nodes)
	for node := 0; node < nodes; node++ {
		if reverse {
			order[node] = nodes - 1 - node
			continue
		}
		order[node] = node
	}
	community := make([]int, nodes)
	totals := make([]float64, nodes)
	for node := 0; node < nodes; node++ {
		community[node] = node
		totals[node] = graph.degree[node]
	}
	m2 := 2 * graph.total
	if m2 > 0 {
		for pass := 0; pass < passes; pass++ {
			moved := false
			for _, node := range order {
				if graph.degree[node] == 0 {
					continue
				}
				current := community[node]
				weights := make(map[int]float64, len(graph.adjacency[node]))
				for _, link := range graph.adjacency[node] {
					weights[community[link.to]] += link.weight
				}
				totals[current] -= graph.degree[node]
				best := current
				bestGain := weights[current] - resolution*graph.degree[node]*totals[current]/m2
				candidates := make([]int, 0, len(weights))
				for candidate := range weights {
					candidates = append(candidates, candidate)
				}
				sort.Ints(candidates)
				for _, candidate := range candidates {
					if candidate == current {
						continue
					}
					gain := weights[candidate] - resolution*graph.degree[node]*totals[candidate]/m2
					if gain > bestGain+1e-12 {
						best, bestGain = candidate, gain
					}
				}
				community[node] = best
				totals[best] += graph.degree[node]
				if best != current {
					moved = true
				}
			}
			if !moved {
				break
			}
		}
	}
	return renumberCommunities(community)
}

// renumberCommunities makes ids contiguous and orders them by the smallest
// member, which is what makes the next level deterministic.
func renumberCommunities(community []int) ([]int, int) {
	groups := make(map[int][]int, len(community))
	for node, id := range community {
		groups[id] = append(groups[id], node)
	}
	ids := make([]int, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool {
		return groups[ids[left]][0] < groups[ids[right]][0]
	})
	remapped := make(map[int]int, len(ids))
	for index, id := range ids {
		remapped[id] = index
	}
	normalized := make([]int, len(community))
	for node, id := range community {
		normalized[node] = remapped[id]
	}
	return normalized, len(ids)
}

// buildPartition groups the original nodes by their final community, orders the
// communities by size, and computes each one's cohesion from the original
// edges.
func buildPartition(nodes int, edges []Edge, assignment []int) Partition {
	grouped := make(map[int][]int, nodes)
	for node, id := range assignment {
		grouped[id] = append(grouped[id], node)
	}
	communities := make([]Community, 0, len(grouped))
	for _, members := range grouped {
		sort.Ints(members)
		communities = append(communities, Community{Members: members})
	}
	sort.Slice(communities, func(left, right int) bool {
		if len(communities[left].Members) != len(communities[right].Members) {
			return len(communities[left].Members) > len(communities[right].Members)
		}
		return communities[left].Members[0] < communities[right].Members[0]
	})
	position := make([]int, nodes)
	for index := range communities {
		for _, member := range communities[index].Members {
			position[member] = index
		}
	}
	for _, edge := range edges {
		if edge.A < 0 || edge.B < 0 || edge.A >= nodes || edge.B >= nodes || edge.A == edge.B {
			continue
		}
		id := position[edge.A]
		if id != position[edge.B] {
			continue
		}
		communities[id].InternalWeight += edgeWeight(edge.Weight)
	}
	for index := range communities {
		size := len(communities[index].Members)
		if size < 2 {
			continue
		}
		pairs := float64(size) * float64(size-1) / 2
		communities[index].Cohesion = math.Min(1, communities[index].InternalWeight/pairs)
	}
	normalized := make([]int, nodes)
	for node := range normalized {
		normalized[node] = position[node]
	}
	return Partition{Assignment: normalized, Communities: communities}
}

// mergeLinks sorts one adjacency list and sums duplicate neighbour weights.
func mergeLinks(links []levelLink) []levelLink {
	if len(links) == 0 {
		return nil
	}
	sort.Slice(links, func(left, right int) bool {
		if links[left].to != links[right].to {
			return links[left].to < links[right].to
		}
		return links[left].weight < links[right].weight
	})
	merged := make([]levelLink, 0, len(links))
	for _, link := range links {
		if len(merged) > 0 && merged[len(merged)-1].to == link.to {
			merged[len(merged)-1].weight += link.weight
			continue
		}
		merged = append(merged, link)
	}
	return merged
}

// edgeWeight treats a non-positive weight as 1.
func edgeWeight(weight float64) float64 {
	if weight <= 0 {
		return 1
	}
	return weight
}
