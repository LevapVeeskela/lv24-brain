package graphalgo

import "sort"

// SCC is one strongly connected component of the directed dependency graph
// with at least two members. Members are sorted node indexes.
type SCC struct {
	Members []int
}

// StronglyConnectedComponents finds the directed cycles of a graph with an
// iterative Tarjan walk.
//
// The input is read as directed: edge A -> B. A self-loop is not a cycle
// between modules and is ignored; duplicate edges do not change the result.
// Adjacency lists are sorted and roots are visited in index order, so the
// components and their members depend only on the graph, never on Go map
// iteration order. Components are ordered by size (largest first), then by
// their smallest member, and singletons are omitted.
func StronglyConnectedComponents(nodes int, edges []Edge) []SCC {
	if nodes <= 0 {
		return nil
	}
	adjacency := make([][]int, nodes)
	for _, edge := range edges {
		if edge.A < 0 || edge.B < 0 || edge.A >= nodes || edge.B >= nodes || edge.A == edge.B {
			continue
		}
		adjacency[edge.A] = append(adjacency[edge.A], edge.B)
	}
	for node := range adjacency {
		adjacency[node] = sortedUniqueInts(adjacency[node])
	}

	index := make([]int, nodes)
	low := make([]int, nodes)
	onStack := make([]bool, nodes)
	for node := range index {
		index[node] = -1
	}
	stack := make([]int, 0, nodes)
	type frame struct {
		node int
		next int
	}
	frames := make([]frame, 0, nodes)
	nextIndex := 0
	components := make([]SCC, 0)

	for root := 0; root < nodes; root++ {
		if index[root] != -1 {
			continue
		}
		index[root], low[root] = nextIndex, nextIndex
		nextIndex++
		stack = append(stack, root)
		onStack[root] = true
		frames = append(frames, frame{node: root})

		for len(frames) > 0 {
			current := &frames[len(frames)-1]
			node := current.node
			if current.next < len(adjacency[node]) {
				neighbor := adjacency[node][current.next]
				current.next++
				switch {
				case index[neighbor] == -1:
					index[neighbor], low[neighbor] = nextIndex, nextIndex
					nextIndex++
					stack = append(stack, neighbor)
					onStack[neighbor] = true
					frames = append(frames, frame{node: neighbor})
				case onStack[neighbor] && index[neighbor] < low[node]:
					low[node] = index[neighbor]
				}
				continue
			}
			frames = frames[:len(frames)-1]
			if len(frames) > 0 {
				parent := frames[len(frames)-1].node
				if low[node] < low[parent] {
					low[parent] = low[node]
				}
			}
			if low[node] != index[node] {
				continue
			}
			members := make([]int, 0, 4)
			for {
				member := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[member] = false
				members = append(members, member)
				if member == node {
					break
				}
			}
			if len(members) < 2 {
				continue
			}
			sort.Ints(members)
			components = append(components, SCC{Members: members})
		}
	}

	sort.Slice(components, func(left, right int) bool {
		if len(components[left].Members) != len(components[right].Members) {
			return len(components[left].Members) > len(components[right].Members)
		}
		return components[left].Members[0] < components[right].Members[0]
	})
	return components
}

// sortedUniqueInts sorts one adjacency list and drops duplicate neighbours so
// a repeated edge does not make the walk visit the same node twice.
func sortedUniqueInts(values []int) []int {
	if len(values) == 0 {
		return nil
	}
	sort.Ints(values)
	unique := values[:1]
	for _, value := range values[1:] {
		if value != unique[len(unique)-1] {
			unique = append(unique, value)
		}
	}
	return unique
}
