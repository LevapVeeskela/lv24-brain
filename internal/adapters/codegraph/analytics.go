package codegraph

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/levapveeskela/lv24-brain/internal/adapters/graphalgo"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// communitySignatureRunes is the length of the member hash reported as a
// community signature.
const communitySignatureRunes = 16

// communityHubPercentile is the fraction of highest-degree nodes excluded from
// the partition and reattached afterwards. A hub that imports half the repo
// glues communities together; the codegraph adapter applies the same 5% as the
// native store so both engines answer alike. Graphs below 500 nodes ignore it.
const communityHubPercentile = 0.05

// codeGraphCacheEntries bounds the in-memory partitions kept by one process.
// The codegraph index has no community table, so a partition is computed from
// the stored edges and reused while the index file is unchanged. Two entries
// cover a usual worktree plus its main tree without holding an unbounded graph.
const codeGraphCacheEntries = 2

// structuralKinds are node kinds that carry files and imports rather than
// definitions. They stay in the community partition (they connect it) but are
// not ranked as god nodes, counted as changed symbols or listed as orphans.
var structuralKinds = []string{"file", "import", "module", "export", "parameter"}

var _ ports.GraphAnalytics = (*Index)(nil)

// Analytics reports the codegraph adapter's analytics capability. The adapter
// computes the partition in memory because a CodeGraph index stores no
// communities; nothing is written to the external index.
func (i *Index) Analytics() (ports.GraphAnalytics, bool) { return i, true }

// codeNode is the part of one node the analytics walk needs.
type codeNode struct {
	ID        string
	Kind      string
	Name      string
	QualName  string
	File      string
	StartLine int
}

// codeGraph is one in-memory view of a codegraph index: node index, degree
// counts and the partition. It deliberately keeps no source text.
type codeGraph struct {
	nodes       []codeNode
	index       map[string]int
	outDegree   []int
	inDegree    []int
	communityOf []int
	partition   graphalgo.Partition
	names       []string
	tops        []int
}

// graphCache is process-wide because the MCP server opens a fresh Index per
// call. The key includes the index file size and mtime, so a resynced index is
// never served from a stale partition.
var graphCache struct {
	mu      sync.Mutex
	entries map[string]*codeGraph
	order   []string
}

// loadAnalyticsGraph returns the cached partition of this index file, computing
// it when the file changed or the cache is cold.
func (i *Index) loadAnalyticsGraph(ctx context.Context) (*codeGraph, error) {
	var nodes int
	if err := i.db.QueryRowContext(ctx, `SELECT count(*) FROM nodes`).Scan(&nodes); err != nil {
		return nil, fmt.Errorf("codegraph: count nodes for analytics: %w", err)
	}
	if nodes == 0 {
		return nil, fmt.Errorf("%w: the codegraph index holds no nodes; run codegraph index in the checkout", domain.ErrUnsupported)
	}
	key := i.cacheKey()
	if cached := cachedCodeGraph(key); cached != nil {
		return cached, nil
	}
	graph, err := i.computeCodeGraph(ctx)
	if err != nil {
		return nil, err
	}
	storeCodeGraph(key, graph)
	return graph, nil
}

// cacheKey identifies one index snapshot. The WAL file is part of the identity
// because a live writer appends there before the main file changes.
func (i *Index) cacheKey() string {
	parts := []string{i.dbPath, fileIdentity(i.dbPath)}
	if wal := fileIdentity(i.dbPath + "-wal"); wal != "" {
		parts = append(parts, wal)
	}
	return strings.Join(parts, "|")
}

// fileIdentity renders the size and mtime of one file, or "" when it is absent.
func fileIdentity(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

func cachedCodeGraph(key string) *codeGraph {
	graphCache.mu.Lock()
	defer graphCache.mu.Unlock()
	return graphCache.entries[key]
}

func storeCodeGraph(key string, graph *codeGraph) {
	graphCache.mu.Lock()
	defer graphCache.mu.Unlock()
	if graphCache.entries == nil {
		graphCache.entries = map[string]*codeGraph{}
	}
	if _, exists := graphCache.entries[key]; !exists {
		graphCache.order = append(graphCache.order, key)
	}
	graphCache.entries[key] = graph
	for len(graphCache.order) > codeGraphCacheEntries {
		oldest := graphCache.order[0]
		graphCache.order = graphCache.order[1:]
		delete(graphCache.entries, oldest)
	}
}

// computeCodeGraph loads the node index and the dependency edges, deduplicates
// a repeated (source, target) pair into one relationship and partitions the
// undirected view.
func (i *Index) computeCodeGraph(ctx context.Context) (*codeGraph, error) {
	rows, err := i.db.QueryContext(ctx,
		`SELECT id, kind, name, qualified_name, file_path, start_line FROM nodes ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("codegraph: read nodes for analytics: %w", err)
	}
	graph := &codeGraph{index: map[string]int{}}
	for rows.Next() {
		var node codeNode
		if err := rows.Scan(&node.ID, &node.Kind, &node.Name, &node.QualName, &node.File, &node.StartLine); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("codegraph: read nodes for analytics: %w", err)
		}
		graph.index[node.ID] = len(graph.nodes)
		graph.nodes = append(graph.nodes, node)
	}
	rowErr := rows.Err()
	_ = rows.Close()
	if rowErr != nil {
		return nil, fmt.Errorf("codegraph: read nodes for analytics: %w", rowErr)
	}
	graph.outDegree = make([]int, len(graph.nodes))
	graph.inDegree = make([]int, len(graph.nodes))

	arguments := stringsToAny(traversalEdgeKinds)
	edgeRows, err := i.db.QueryContext(ctx,
		`SELECT source, target, kind FROM edges
		 WHERE kind IN (`+placeholders(len(traversalEdgeKinds))+`)
		 ORDER BY source, target, kind, COALESCE(line, 0)`, arguments...)
	if err != nil {
		return nil, fmt.Errorf("codegraph: read edges for analytics: %w", err)
	}
	edges := make([]graphalgo.Edge, 0, len(graph.nodes))
	lastFrom, lastTo := -1, -1
	for edgeRows.Next() {
		var source, target, kind string
		if err := edgeRows.Scan(&source, &target, &kind); err != nil {
			_ = edgeRows.Close()
			return nil, fmt.Errorf("codegraph: read edges for analytics: %w", err)
		}
		from, fromOK := graph.index[source]
		to, toOK := graph.index[target]
		if !fromOK || !toOK || from == to {
			continue
		}
		if from == lastFrom && to == lastTo {
			continue
		}
		lastFrom, lastTo = from, to
		graph.outDegree[from]++
		graph.inDegree[to]++
		edges = append(edges, graphalgo.Edge{A: from, B: to})
	}
	rowErr = edgeRows.Err()
	_ = edgeRows.Close()
	if rowErr != nil {
		return nil, fmt.Errorf("codegraph: read edges for analytics: %w", rowErr)
	}

	graph.partition = graphalgo.DetectCommunities(len(graph.nodes), edges, graphalgo.CommunityOptions{HubPercentile: communityHubPercentile})
	graph.communityOf = graph.partition.Assignment
	graph.names = make([]string, len(graph.partition.Communities))
	graph.tops = make([]int, len(graph.partition.Communities))
	degree := make([]int, len(graph.nodes))
	for index := range degree {
		degree[index] = graph.outDegree[index] + graph.inDegree[index]
	}
	for index, community := range graph.partition.Communities {
		top := community.Members[0]
		for _, member := range community.Members[1:] {
			if degree[member] > degree[top] {
				top = member
			}
		}
		graph.tops[index] = top
		name := strings.TrimSpace(graph.nodes[top].Name)
		if name == "" {
			name = strings.TrimSpace(graph.nodes[top].QualName)
		}
		if name == "" {
			name = fmt.Sprintf("community %d", index+1)
		}
		graph.names[index] = name
	}
	return graph, nil
}

// singletonCommunities counts the one-member communities, which are nodes with
// no dependency edge and are reported as orphans.
func (g *codeGraph) singletonCommunities() int {
	count := 0
	for _, community := range g.partition.Communities {
		if len(community.Members) == 1 {
			count++
		}
	}
	return count
}

// Communities lists the computed partition, or one community with members.
func (i *Index) Communities(ctx context.Context, project string, opts ports.CommunityQuery) (ports.CommunityView, error) {
	view := ports.CommunityView{Communities: []ports.GraphCommunity{}}
	graph, err := i.loadAnalyticsGraph(ctx)
	if err != nil {
		return view, err
	}
	total := len(graph.partition.Communities)
	view.Total = total
	if singletons := graph.singletonCommunities(); singletons > 0 {
		view.Notes = append(view.Notes, fmt.Sprintf("%d singleton community(ies) hold nodes with no dependency edge; stats.orphans counts the non-structural ones", singletons))
	}
	if opts.ID != 0 {
		if opts.ID < 0 {
			return view, domain.NewValidationError("graph.community_id", "must be a positive community id")
		}
		index := opts.ID - 1
		if index >= total {
			return view, fmt.Errorf("%w: community %d is not in this index (%d communities)", domain.ErrNotFound, opts.ID, total)
		}
		community, err := i.loadCommunity(ctx, graph, index, opts.MemberLimit)
		if err != nil {
			return view, err
		}
		view.Communities = []ports.GraphCommunity{community}
		return view, nil
	}
	limit := clampLimit(opts.Limit, ports.DefaultGraphCommunities, ports.MaxGraphCommunities)
	truncated := total > limit
	if truncated {
		total = limit
	}
	topIDs := make([]string, 0, total)
	for index := 0; index < total; index++ {
		topIDs = append(topIDs, graph.nodes[graph.tops[index]].ID)
	}
	topSymbols, err := i.loadSymbols(ctx, topIDs)
	if err != nil {
		return view, err
	}
	byID := make(map[string]ports.GraphSymbol, len(topSymbols))
	for _, symbol := range topSymbols {
		byID[symbol.ID] = symbol
	}
	for index := 0; index < total; index++ {
		community := graph.partition.Communities[index]
		view.Communities = append(view.Communities, ports.GraphCommunity{
			ID: index + 1, Name: graph.names[index], Size: len(community.Members),
			Cohesion: community.Cohesion, Signature: communitySignature(graph, community.Members),
			Top: byID[graph.nodes[graph.tops[index]].ID],
		})
	}
	view.Truncated = truncated
	if truncated {
		view.Notes = append(view.Notes, fmt.Sprintf("showing the %d largest of %d communities; pass community_id for one", limit, len(graph.partition.Communities)))
	}
	view.Notes = append(view.Notes, "the partition is computed in memory from the stored edges and is not written into the codegraph index")
	return view, nil
}

// loadCommunity renders one community and its member list.
func (i *Index) loadCommunity(ctx context.Context, graph *codeGraph, index, memberLimit int) (ports.GraphCommunity, error) {
	community := graph.partition.Communities[index]
	limit := clampLimit(memberLimit, ports.DefaultGraphCommunityMembers, ports.MaxGraphCommunityMembers)
	members := community.Members
	truncated := len(members) > limit
	if truncated {
		members = members[:limit]
	}
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, graph.nodes[member].ID)
	}
	symbols, err := i.loadSymbols(ctx, ids)
	if err != nil {
		return ports.GraphCommunity{}, err
	}
	top, err := i.loadSymbols(ctx, []string{graph.nodes[graph.tops[index]].ID})
	if err != nil {
		return ports.GraphCommunity{}, err
	}
	out := ports.GraphCommunity{
		ID: index + 1, Name: graph.names[index], Size: len(community.Members),
		Cohesion: community.Cohesion, Signature: communitySignature(graph, community.Members),
		Members: symbols, MembersTruncated: truncated,
	}
	if len(top) == 1 {
		out.Top = top[0]
	}
	return out, nil
}

// communitySignature hashes the sorted member ids of one community.
func communitySignature(graph *codeGraph, members []int) string {
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, graph.nodes[member].ID)
	}
	sort.Strings(ids)
	hash := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return hex.EncodeToString(hash[:])[:communitySignatureRunes]
}

// GodNodes ranks definitions by total degree over dependency edges.
func (i *Index) GodNodes(ctx context.Context, project string, limit int) (ports.GodNodesView, error) {
	view := ports.GodNodesView{Nodes: []ports.GraphGodNode{}}
	graph, err := i.loadAnalyticsGraph(ctx)
	if err != nil {
		return view, err
	}
	limit = clampLimit(limit, ports.DefaultGraphGodNodes, ports.MaxGraphGodNodes)
	structural := map[string]bool{}
	for _, kind := range structuralKinds {
		structural[kind] = true
	}
	candidates := make([]int, 0, len(graph.nodes))
	structuralCount, isolated := 0, 0
	for index, node := range graph.nodes {
		if structural[node.Kind] {
			structuralCount++
			continue
		}
		if graph.outDegree[index]+graph.inDegree[index] == 0 {
			isolated++
			continue
		}
		candidates = append(candidates, index)
	}
	view.Total = len(candidates)
	sort.Slice(candidates, func(left, right int) bool {
		a, b := graph.nodes[candidates[left]], graph.nodes[candidates[right]]
		degreeA := graph.outDegree[candidates[left]] + graph.inDegree[candidates[left]]
		degreeB := graph.outDegree[candidates[right]] + graph.inDegree[candidates[right]]
		if degreeA != degreeB {
			return degreeA > degreeB
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.File != b.File {
			return a.File < b.File
		}
		if a.StartLine != b.StartLine {
			return a.StartLine < b.StartLine
		}
		return a.ID < b.ID
	})
	truncated := len(candidates) > limit
	if truncated {
		candidates = candidates[:limit]
	}
	ids := make([]string, 0, len(candidates))
	for _, index := range candidates {
		ids = append(ids, graph.nodes[index].ID)
	}
	symbols, err := i.loadSymbols(ctx, ids)
	if err != nil {
		return view, err
	}
	byID := make(map[string]ports.GraphSymbol, len(symbols))
	for _, symbol := range symbols {
		byID[symbol.ID] = symbol
	}
	denominator := float64(len(graph.nodes) - 1)
	for _, index := range candidates {
		node := graph.nodes[index]
		outDegree, inDegree := graph.outDegree[index], graph.inDegree[index]
		degree := outDegree + inDegree
		centrality := 0.0
		if denominator > 0 {
			centrality = float64(degree) / denominator
		}
		communityIndex := graph.communityOf[index]
		view.Nodes = append(view.Nodes, ports.GraphGodNode{
			GraphSymbol: byID[node.ID],
			Degree:      degree,
			InDegree:    inDegree,
			OutDegree:   outDegree,
			Centrality:  centrality,
			Community:   graph.names[communityIndex],
			CommunityID: communityIndex + 1,
			Test:        IsTestFile(node.File),
			Why: fmt.Sprintf("%d distinct dependency relationship(s): %d incoming, %d outgoing across %d indexed node(s); degree centrality %.4f",
				degree, inDegree, outDegree, len(graph.nodes), centrality),
		})
	}
	view.Truncated = truncated
	if truncated {
		view.Notes = append(view.Notes, fmt.Sprintf("showing the %d most connected of %d definitions; raise limit (at most %d) for more", limit, view.Total, ports.MaxGraphGodNodes))
	}
	if structuralCount > 0 {
		view.Notes = append(view.Notes, fmt.Sprintf("%d structural node(s) (file, import, module, export, parameter) are excluded from the ranking", structuralCount))
	}
	if isolated > 0 {
		view.Notes = append(view.Notes, fmt.Sprintf("%d node(s) with no dependency edge are reported as orphans by stats, not ranked here", isolated))
	}
	return view, nil
}

// ShortestPath finds the shortest chain between two definitions, first along
// outgoing dependency edges and then ignoring direction.
func (i *Index) ShortestPath(ctx context.Context, project, from, to string, maxDepth int) (ports.GraphPath, error) {
	view := ports.GraphPath{}
	fromSymbol, err := i.resolveSymbol(ctx, from, "")
	if err != nil {
		return view, err
	}
	toSymbol, err := i.resolveSymbol(ctx, to, "")
	if err != nil {
		return view, err
	}
	view.From, view.To = fromSymbol, toSymbol
	requested := maxDepth
	if maxDepth <= 0 {
		maxDepth = ports.DefaultGraphPathDepth
	}
	if maxDepth > ports.MaxGraphPathDepth {
		maxDepth = ports.MaxGraphPathDepth
		view.Notes = append(view.Notes, fmt.Sprintf("max depth %d clamped to the %d-hop limit", requested, ports.MaxGraphPathDepth))
	}
	if maxDepth > ports.MaxGraphPathNodes-1 {
		maxDepth = ports.MaxGraphPathNodes - 1
	}
	steps, found, err := graphalgo.ShortestPath(fromSymbol.ID, toSymbol.ID, maxDepth, i.pathExpander(ctx, false))
	directed := true
	if err != nil {
		return view, err
	}
	if !found && fromSymbol.ID != toSymbol.ID {
		steps, found, err = graphalgo.ShortestPath(fromSymbol.ID, toSymbol.ID, maxDepth, i.pathExpander(ctx, true))
		if err != nil {
			return view, err
		}
		directed = false
	}
	if !found {
		view.Notes = append(view.Notes, fmt.Sprintf(
			"no path within %d hop(s): outgoing dependency edges and then the same edges in both directions were searched; both symbols exist, so this is a fact about the graph",
			maxDepth))
		return view, nil
	}
	if !directed {
		view.Notes = append(view.Notes, "no path along outgoing dependency edges; the reported path crosses at least one edge in reverse")
	}
	ids := make([]string, 0, len(steps))
	for _, step := range steps {
		ids = append(ids, step.ID)
	}
	symbols, err := i.loadSymbols(ctx, ids)
	if err != nil {
		return view, err
	}
	byID := make(map[string]ports.GraphSymbol, len(symbols))
	for _, symbol := range symbols {
		byID[symbol.ID] = symbol
	}
	assembled := graphalgo.PathView(fromSymbol, toSymbol, steps, byID, directed, ports.MaxGraphPathNodes)
	if len(view.Notes) > 0 {
		assembled.Notes = append(view.Notes, assembled.Notes...)
	}
	return assembled, nil
}

// pathExpander builds the level query of one search. The query is bounded by
// the frontier and the traversal edge kinds, so a 750 MB index is never loaded
// whole for a path.
func (i *Index) pathExpander(ctx context.Context, undirected bool) func([]string) (map[string][]graphalgo.Neighbor, error) {
	return func(frontier []string) (map[string][]graphalgo.Neighbor, error) {
		out := make(map[string][]graphalgo.Neighbor, len(frontier))
		seen := map[string]bool{}
		for _, chunk := range chunkStrings(frontier, impactChunk) {
			arguments := append(stringsToAny(chunk), stringsToAny(traversalEdgeKinds)...)
			query := `SELECT source, target, kind, COALESCE(line, 0), metadata FROM edges
				WHERE source IN (` + placeholders(len(chunk)) + `)
				AND kind IN (` + placeholders(len(traversalEdgeKinds)) + `)
				ORDER BY source, target, kind, COALESCE(line, 0)`
			if err := i.expand(ctx, query, arguments, out, seen, false); err != nil {
				return nil, err
			}
			if !undirected {
				continue
			}
			arguments = append(stringsToAny(chunk), stringsToAny(traversalEdgeKinds)...)
			query = `SELECT target, source, kind, COALESCE(line, 0), metadata FROM edges
				WHERE target IN (` + placeholders(len(chunk)) + `)
				AND kind IN (` + placeholders(len(traversalEdgeKinds)) + `)
				ORDER BY target, source, kind, COALESCE(line, 0)`
			if err := i.expand(ctx, query, arguments, out, seen, true); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
}

// expand appends one level's rows to out, deduplicated per (node, neighbour)
// across both directions so a reciprocal edge yields one hop, not two.
func (i *Index) expand(ctx context.Context, query string, arguments []any, out map[string][]graphalgo.Neighbor, seen map[string]bool, reverse bool) error {
	rows, err := i.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return fmt.Errorf("codegraph: expand path frontier: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var node, neighbor, kind string
		var line int
		var metadata sql.NullString
		if err := rows.Scan(&node, &neighbor, &kind, &line, &metadata); err != nil {
			return fmt.Errorf("codegraph: expand path frontier: %w", err)
		}
		key := node + "\x00" + neighbor
		if seen[key] {
			continue
		}
		seen[key] = true
		confidence, _ := edgeMetadata(metadata)
		out[node] = append(out[node], graphalgo.Neighbor{
			ID: neighbor, Edge: kind, Line: line, Confidence: confidence, Reverse: reverse,
		})
	}
	return rows.Err()
}

// reachHop is the first edge that reached a node in a reverse walk.
type reachHop struct {
	distance   int
	via        string
	line       int
	confidence float64
	provenance string
}

// reverseReach walks incoming dependency edges from every seed at once.
func (i *Index) reverseReach(ctx context.Context, seeds []string, depth int) (map[string]reachHop, bool, error) {
	best := make(map[string]reachHop, len(seeds))
	frontier := make([]string, 0, len(seeds))
	for _, seed := range uniqueStrings(seeds) {
		best[seed] = reachHop{}
		frontier = append(frontier, seed)
	}
	sort.Strings(frontier)
	hits := 0
	truncated := false
	for distance := 1; distance <= depth && len(frontier) > 0 && !truncated; distance++ {
		next := make([]string, 0, len(frontier))
		for _, chunk := range chunkStrings(frontier, impactChunk) {
			arguments := append(stringsToAny(chunk), stringsToAny(traversalEdgeKinds)...)
			query := `SELECT source, kind, COALESCE(line, 0), COALESCE(provenance, ''), metadata FROM edges
				WHERE target IN (` + placeholders(len(chunk)) + `)
				AND kind IN (` + placeholders(len(traversalEdgeKinds)) + `)
				ORDER BY source, kind, line`
			rows, err := i.db.QueryContext(ctx, query, arguments...)
			if err != nil {
				return nil, false, fmt.Errorf("codegraph: pr impact walk: %w", err)
			}
			for rows.Next() {
				var source, edge, provenance string
				var line int
				var metadata sql.NullString
				if err := rows.Scan(&source, &edge, &line, &provenance, &metadata); err != nil {
					_ = rows.Close()
					return nil, false, fmt.Errorf("codegraph: pr impact walk: %w", err)
				}
				if _, seen := best[source]; seen {
					continue
				}
				if hits >= ports.MaxGraphPRSymbols {
					truncated = true
					break
				}
				confidence, resolvedBy := edgeMetadata(metadata)
				best[source] = reachHop{
					distance: distance, via: edge, line: line,
					confidence: confidence, provenance: provenanceOf(provenance, resolvedBy),
				}
				hits++
				next = append(next, source)
			}
			rowErr := rows.Err()
			_ = rows.Close()
			if rowErr != nil {
				return nil, false, fmt.Errorf("codegraph: pr impact walk: %w", rowErr)
			}
			if truncated {
				break
			}
		}
		sort.Strings(next)
		frontier = next
	}
	return best, truncated, nil
}

// PRImpact maps a git diff to changed symbols and their reverse blast radius.
func (i *Index) PRImpact(ctx context.Context, project, base, head string) (ports.PRImpactView, error) {
	view := ports.PRImpactView{Base: strings.TrimSpace(base), Head: strings.TrimSpace(head)}
	if view.Head == "" {
		view.Head = "HEAD"
	}
	files, total, filesTruncated, err := graphalgo.ChangedFiles(ctx, i.root, view.Base, view.Head, ports.MaxGraphPRFiles)
	if err != nil {
		return view, err
	}
	view.ChangedFiles = total
	view.Files = make([]ports.PRFileImpact, 0, len(files))
	paths := make([]string, 0, len(files))
	deleted := 0
	for _, file := range files {
		view.Files = append(view.Files, ports.PRFileImpact{Path: file.Path, Status: file.Status})
		paths = append(paths, file.Path)
		if file.Status == "D" {
			deleted++
		}
	}
	if filesTruncated {
		view.Truncated = true
		view.Notes = append(view.Notes, fmt.Sprintf("git reported %d changed file(s); only the first %d are listed", total, ports.MaxGraphPRFiles))
	}
	if len(files) == 0 {
		view.Risk = ports.GraphRiskLow
		view.Notes = append(view.Notes, "the two refs are identical or the diff is empty: no file changed")
		return view, nil
	}
	idsByFile, err := i.nodeIDsByFile(ctx, paths)
	if err != nil {
		return view, err
	}
	allIDs := make([]string, 0, len(paths))
	unindexed := 0
	for index := range view.Files {
		ids := idsByFile[view.Files[index].Path]
		view.Files[index].Symbols = len(ids)
		if len(ids) == 0 {
			unindexed++
		}
		allIDs = append(allIDs, ids...)
	}
	allIDs = uniqueStrings(allIDs)
	sort.Strings(allIDs)
	display := allIDs
	if len(display) > ports.MaxGraphPRSymbols {
		display = display[:ports.MaxGraphPRSymbols]
		view.Truncated = true
		view.Notes = append(view.Notes, fmt.Sprintf("the diff touches more than %d symbol(s); the changed-symbol list is capped", ports.MaxGraphPRSymbols))
	}
	view.ChangedSymbols, err = i.loadSymbols(ctx, display)
	if err != nil {
		return view, err
	}
	reach, reachTruncated, err := i.reverseReach(ctx, allIDs, ports.DefaultGraphPRDepth)
	if err != nil {
		return view, err
	}
	impactIDs := make([]string, 0, len(reach))
	for id, hop := range reach {
		if hop.distance == 0 {
			continue
		}
		impactIDs = append(impactIDs, id)
	}
	sort.Strings(impactIDs)
	symbols, err := i.loadSymbols(ctx, impactIDs)
	if err != nil {
		return view, err
	}
	view.Impact = make([]ports.ImpactHit, 0, len(symbols))
	tests := map[string]bool{}
	for _, file := range view.Files {
		if IsTestFile(file.Path) {
			tests[file.Path] = true
		}
	}
	for _, symbol := range symbols {
		if IsTestFile(symbol.File) {
			tests[symbol.File] = true
		}
		if isStructuralKind(symbol.Kind) {
			// A file or import node is a dependency carrier, not a symbol a
			// change "reaches"; keep its path and continue.
			continue
		}
		hop := reach[symbol.ID]
		view.Impact = append(view.Impact, ports.ImpactHit{
			GraphSymbol: symbol, Distance: hop.distance, Via: hop.via,
			ViaFile: symbol.File, ViaLine: hop.line, Confidence: hop.confidence,
			Provenance: hop.provenance, Test: IsTestFile(symbol.File),
		})
	}
	sort.SliceStable(view.Impact, func(left, right int) bool {
		if view.Impact[left].Distance != view.Impact[right].Distance {
			return view.Impact[left].Distance < view.Impact[right].Distance
		}
		if view.Impact[left].File != view.Impact[right].File {
			return view.Impact[left].File < view.Impact[right].File
		}
		return view.Impact[left].StartLine < view.Impact[right].StartLine
	})
	view.TotalImpact = len(view.Impact)
	view.Tests = make([]string, 0, len(tests))
	for path := range tests {
		view.Tests = append(view.Tests, path)
	}
	sort.Strings(view.Tests)
	view.Risk = graphalgo.RiskTier(len(view.Impact), len(allIDs), deleted)
	view.Truncated = view.Truncated || reachTruncated
	if reachTruncated {
		view.Notes = append(view.Notes, fmt.Sprintf("the reverse blast radius stopped at %d symbol(s); the true reach is larger", ports.MaxGraphPRSymbols))
	}
	if deleted > 0 {
		view.Notes = append(view.Notes, fmt.Sprintf("%d deleted file(s): their callers are what breaks; nodes still present in the index are included as seeds", deleted))
	}
	if unindexed > 0 {
		view.Notes = append(view.Notes, fmt.Sprintf("%d changed file(s) have no indexed definition (deleted after the index was built, generated or unindexed)", unindexed))
	}
	view.Notes = append(view.Notes, fmt.Sprintf("risk %s: %d reachable symbol(s), %d changed symbol(s), %d deleted file(s)",
		view.Risk, len(view.Impact), len(allIDs), deleted))
	return view, nil
}

// nodeIDsByFile returns the definitions of every given path, grouped by path.
// File and import nodes are skipped: they carry a path, not a definition.
func (i *Index) nodeIDsByFile(ctx context.Context, paths []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, chunk := range chunkStrings(uniqueStrings(paths), impactChunk) {
		arguments := stringsToAny(chunk)
		arguments = append(arguments, stringsToAny(structuralKinds)...)
		query := `SELECT id, file_path FROM nodes WHERE file_path IN (` + placeholders(len(chunk)) + `)
			AND kind NOT IN (` + placeholders(len(structuralKinds)) + `)
			ORDER BY file_path, start_line`
		rows, err := i.db.QueryContext(ctx, query, arguments...)
		if err != nil {
			return nil, fmt.Errorf("codegraph: read symbols of changed files: %w", err)
		}
		for rows.Next() {
			var id, path string
			if err := rows.Scan(&id, &path); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("codegraph: read symbols of changed files: %w", err)
			}
			out[path] = append(out[path], id)
		}
		rowErr := rows.Err()
		_ = rows.Close()
		if rowErr != nil {
			return nil, fmt.Errorf("codegraph: read symbols of changed files: %w", rowErr)
		}
	}
	return out, nil
}

// isStructuralKind reports whether a node carries a file or an import instead
// of a definition.
func isStructuralKind(kind string) bool {
	for _, structural := range structuralKinds {
		if kind == structural {
			return true
		}
	}
	return false
}

// GraphStats reports the counts that need the whole graph. The community count
// comes from the in-memory partition; a codegraph index stores no communities.
func (i *Index) GraphStats(ctx context.Context, project string) (ports.GraphStatsView, error) {
	view := ports.GraphStatsView{}
	status, err := i.Status(ctx, project)
	if err != nil {
		return view, err
	}
	graph, err := i.loadAnalyticsGraph(ctx)
	if err != nil {
		return view, err
	}
	arguments := stringsToAny(structuralKinds)
	arguments = append(arguments, stringsToAny(traversalEdgeKinds)...)
	arguments = append(arguments, stringsToAny(traversalEdgeKinds)...)
	var orphans int
	if err := i.db.QueryRowContext(ctx,
		`SELECT count(*) FROM nodes n
		 WHERE n.kind NOT IN (`+placeholders(len(structuralKinds))+`)
		   AND NOT EXISTS (SELECT 1 FROM edges e WHERE e.source = n.id AND e.kind IN (`+placeholders(len(traversalEdgeKinds))+`))
		   AND NOT EXISTS (SELECT 1 FROM edges e WHERE e.target = n.id AND e.kind IN (`+placeholders(len(traversalEdgeKinds))+`))`,
		arguments...).Scan(&orphans); err != nil {
		return view, fmt.Errorf("codegraph: count orphan nodes: %w", err)
	}
	languages, err := i.countBy(ctx, `SELECT language, count(*) FROM files GROUP BY language ORDER BY count(*) DESC, language`)
	if err != nil {
		return view, err
	}
	view = ports.GraphStatsView{
		Engine: Engine, Project: status.Project, DBPath: status.DBPath,
		SchemaVersion: status.SchemaVersion, ExtractionVersion: status.ExtractionVersion,
		Files: status.Files, Symbols: status.Nodes, Edges: status.Edges,
		Communities: len(graph.partition.Communities), Orphans: orphans,
		Languages: languages, NodesByKind: status.NodesByKind,
	}
	view.Notes = append(view.Notes, "communities are computed in memory from the stored edges; a codegraph index carries no community table")
	if singletons := graph.singletonCommunities(); singletons > 0 {
		view.Notes = append(view.Notes, fmt.Sprintf("%d singleton community(ies) hold nodes with no dependency edge; stats.orphans counts the non-structural ones", singletons))
	}
	return view, nil
}
