package codeindex

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/adapters/graphalgo"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// CommunityAlgorithm names the community detection the store was filled with.
// It is written to meta so a later release can tell an old partition from a new
// one without guessing.
const CommunityAlgorithm = "louvain-multilevel/1"

// communityHubPercentile is the fraction of highest-degree nodes excluded from
// the partition and reattached afterwards. Hubs such as NewValidationError glue
// half the graph together; without this the modularity optimum collapses into a
// few hundred-node blobs. It matches the graphify audit's optional hub
// exclusion and applies only to graphs of at least 500 nodes.
const communityHubPercentile = 0.05

// communitySignatureRunes is the length of the member hash reported as a
// community signature.
const communitySignatureRunes = 16

var _ ports.GraphAnalytics = (*Index)(nil)

// Analytics reports the native store's analytics capability. The store always
// serves it; a caller still reports UNSUPPORTED when the engine is off or the
// index is absent.
func (i *Index) Analytics() (ports.GraphAnalytics, bool) { return i, true }

// graphNode is the part of a symbol the analytics walk needs. Signatures and
// doc comments are loaded only for the definitions an answer renders.
type graphNode struct {
	ID        string
	Kind      string
	Name      string
	QualName  string
	File      string
	StartLine int
}

// graphHop is one directed dependency edge, deduplicated per (source, target)
// pair. The representative site is the highest-confidence one, then the
// smallest edge kind and line, so a path reports a stable answer.
type graphHop struct {
	to         int
	edge       string
	line       int
	confidence float64
}

// symbolGraph is one in-memory view of the native graph: node index, both edge
// directions, the unique undirected edge list and the total degree.
type symbolGraph struct {
	nodes  []graphNode
	index  map[string]int
	out    [][]graphHop
	in     [][]graphHop
	edges  []graphalgo.Edge
	degree []int
}

// queryer is the shared surface of *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// loadSymbolGraph reads the whole dependency graph. The native store is small
// enough (about 11k symbols and 53k edges on this repository) that one load
// beats a query per hop for every analytics action.
func (i *Index) loadSymbolGraph(ctx context.Context) (*symbolGraph, error) {
	return loadSymbolGraphFrom(ctx, i.db)
}

func loadSymbolGraphFrom(ctx context.Context, db queryer) (*symbolGraph, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, kind, name, qualname, path, start_line FROM symbols ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("codeindex: read symbols for analytics: %w", err)
	}
	graph := &symbolGraph{index: map[string]int{}}
	for rows.Next() {
		var node graphNode
		if err := rows.Scan(&node.ID, &node.Kind, &node.Name, &node.QualName, &node.File, &node.StartLine); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("codeindex: read symbols for analytics: %w", err)
		}
		graph.index[node.ID] = len(graph.nodes)
		graph.nodes = append(graph.nodes, node)
	}
	rowErr := rows.Err()
	_ = rows.Close()
	if rowErr != nil {
		return nil, fmt.Errorf("codeindex: read symbols for analytics: %w", rowErr)
	}
	graph.out = make([][]graphHop, len(graph.nodes))
	graph.degree = make([]int, len(graph.nodes))

	arguments := stringsToAny(traversalEdgeKinds)
	edgeRows, err := db.QueryContext(ctx,
		`SELECT source, target, kind, line, confidence FROM edges
		 WHERE kind IN (`+placeholders(len(traversalEdgeKinds))+`)
		 ORDER BY source, target, kind, line`, arguments...)
	if err != nil {
		return nil, fmt.Errorf("codeindex: read edges for analytics: %w", err)
	}
	sourceIndex, targetIndex := -1, -1
	hop := graphHop{}
	pending := false
	commit := func() {
		if !pending {
			return
		}
		graph.out[sourceIndex] = append(graph.out[sourceIndex], hop)
		graph.degree[sourceIndex]++
		graph.degree[targetIndex]++
		graph.edges = append(graph.edges, graphalgo.Edge{A: sourceIndex, B: targetIndex})
		pending = false
	}
	for edgeRows.Next() {
		var source, target, kind string
		var line int
		var confidence float64
		if err := edgeRows.Scan(&source, &target, &kind, &line, &confidence); err != nil {
			_ = edgeRows.Close()
			return nil, fmt.Errorf("codeindex: read edges for analytics: %w", err)
		}
		from, fromOK := graph.index[source]
		to, toOK := graph.index[target]
		if !fromOK || !toOK || from == to {
			continue
		}
		if pending && from == sourceIndex && to == targetIndex {
			if confidence > hop.confidence {
				hop.confidence = confidence
				hop.edge = kind
				hop.line = line
			}
			continue
		}
		commit()
		sourceIndex, targetIndex = from, to
		hop = graphHop{to: to, edge: kind, line: line, confidence: confidence}
		pending = true
	}
	rowErr = edgeRows.Err()
	_ = edgeRows.Close()
	if rowErr != nil {
		return nil, fmt.Errorf("codeindex: read edges for analytics: %w", rowErr)
	}
	commit()

	graph.in = make([][]graphHop, len(graph.nodes))
	for from, hops := range graph.out {
		for _, outbound := range hops {
			graph.in[outbound.to] = append(graph.in[outbound.to], graphHop{
				to: from, edge: outbound.edge, line: outbound.line, confidence: outbound.confidence,
			})
		}
	}
	return graph, nil
}

// ensureAnalyticsReadable refuses a store that cannot answer analytics: a
// missing or empty index, a stale schema, or a checkout the Go extractor found
// no symbols in. It never returns an empty ranking.
func (i *Index) ensureAnalyticsReadable(ctx context.Context) error {
	if err := i.ensureReadable(ctx); err != nil {
		if errors.Is(err, ErrIndexEmpty) && i.module == "" {
			return fmt.Errorf("%w: the native engine indexes Go modules only and no go.mod was found for %s", domain.ErrUnsupported, i.root)
		}
		return err
	}
	var symbols int
	if err := i.db.QueryRowContext(ctx, `SELECT count(*) FROM symbols`).Scan(&symbols); err != nil {
		return fmt.Errorf("codeindex: count symbols: %w", err)
	}
	if symbols == 0 {
		return fmt.Errorf("%w: the native engine indexes Go modules only and this checkout has no indexed Go symbols", domain.ErrUnsupported)
	}
	return nil
}

// Communities lists the detected communities, or one community with its
// members.
func (i *Index) Communities(ctx context.Context, project string, opts ports.CommunityQuery) (ports.CommunityView, error) {
	view := ports.CommunityView{Communities: []ports.GraphCommunity{}}
	if err := i.ensureAnalyticsReadable(ctx); err != nil {
		return view, err
	}
	total, err := i.communityCount(ctx)
	if err != nil {
		return view, err
	}
	view.Total = total
	if total == 0 {
		view.Notes = append(view.Notes, "no community partition is stored in this index; run brain code index --rebuild")
		return view, nil
	}
	singletons, err := i.singletonCommunityCount(ctx)
	if err != nil {
		return view, err
	}
	if singletons > 0 {
		view.Notes = append(view.Notes, fmt.Sprintf("%d singleton community(ies) hold definitions with no dependency edge; stats.orphans counts the same nodes", singletons))
	}
	if opts.ID != 0 {
		if opts.ID < 0 {
			return view, domain.NewValidationError("graph.community_id", "must be a positive community id")
		}
		community, err := i.loadCommunity(ctx, opts.ID, opts.MemberLimit)
		if err != nil {
			return view, err
		}
		view.Communities = []ports.GraphCommunity{community}
		return view, nil
	}
	limit := clampLimit(opts.Limit, ports.DefaultGraphCommunities, ports.MaxGraphCommunities)
	rows, err := i.db.QueryContext(ctx,
		`SELECT id, name, top_symbol, size, cohesion, signature FROM communities
		 ORDER BY size DESC, id LIMIT ?`, limit+1)
	if err != nil {
		return view, fmt.Errorf("codeindex: list communities: %w", err)
	}
	type row struct {
		id        int
		name      string
		top       string
		size      int
		cohesion  float64
		signature string
	}
	communities := make([]row, 0, limit+1)
	for rows.Next() {
		var current row
		if err := rows.Scan(&current.id, &current.name, &current.top, &current.size,
			&current.cohesion, &current.signature); err != nil {
			_ = rows.Close()
			return view, fmt.Errorf("codeindex: list communities: %w", err)
		}
		communities = append(communities, current)
	}
	rowErr := rows.Err()
	_ = rows.Close()
	if rowErr != nil {
		return view, fmt.Errorf("codeindex: list communities: %w", rowErr)
	}
	truncated := len(communities) > limit
	if truncated {
		communities = communities[:limit]
	}
	topIDs := make([]string, 0, len(communities))
	for _, current := range communities {
		topIDs = append(topIDs, current.top)
	}
	topSymbols, err := i.loadSymbols(ctx, topIDs)
	if err != nil {
		return view, err
	}
	byID := make(map[string]ports.GraphSymbol, len(topSymbols))
	for _, symbol := range topSymbols {
		byID[symbol.ID] = symbol
	}
	for _, current := range communities {
		view.Communities = append(view.Communities, ports.GraphCommunity{
			ID: current.id, Name: current.name, Size: current.size,
			Cohesion: current.cohesion, Signature: current.signature, Top: byID[current.top],
		})
	}
	view.Truncated = truncated
	if truncated {
		view.Notes = append(view.Notes, fmt.Sprintf("showing the %d largest of %d communities; pass community_id for one", limit, total))
	}
	return view, nil
}

// loadCommunity reads one community and its member list.
func (i *Index) loadCommunity(ctx context.Context, id, memberLimit int) (ports.GraphCommunity, error) {
	var community ports.GraphCommunity
	var topID string
	err := i.db.QueryRowContext(ctx,
		`SELECT id, name, top_symbol, size, cohesion, signature FROM communities WHERE id = ?`, id).
		Scan(&community.ID, &community.Name, &topID, &community.Size, &community.Cohesion, &community.Signature)
	if errors.Is(err, sql.ErrNoRows) {
		return community, fmt.Errorf("%w: community %d is not in this index", domain.ErrNotFound, id)
	}
	if err != nil {
		return community, fmt.Errorf("codeindex: read community %d: %w", id, err)
	}
	limit := clampLimit(memberLimit, ports.DefaultGraphCommunityMembers, ports.MaxGraphCommunityMembers)
	rows, err := i.db.QueryContext(ctx,
		`SELECT symbol_id FROM community_members WHERE community_id = ? ORDER BY symbol_id LIMIT ?`, id, limit+1)
	if err != nil {
		return community, fmt.Errorf("codeindex: read community %d members: %w", id, err)
	}
	ids := make([]string, 0, limit+1)
	for rows.Next() {
		var symbolID string
		if err := rows.Scan(&symbolID); err != nil {
			_ = rows.Close()
			return community, fmt.Errorf("codeindex: read community %d members: %w", id, err)
		}
		ids = append(ids, symbolID)
	}
	rowErr := rows.Err()
	_ = rows.Close()
	if rowErr != nil {
		return community, fmt.Errorf("codeindex: read community %d members: %w", id, rowErr)
	}
	if len(ids) > limit {
		community.MembersTruncated = true
		ids = ids[:limit]
	}
	members, err := i.loadSymbols(ctx, ids)
	if err != nil {
		return community, err
	}
	community.Members = members
	top, err := i.loadSymbols(ctx, []string{topID})
	if err != nil {
		return community, err
	}
	if len(top) == 1 {
		community.Top = top[0]
	}
	return community, nil
}

// GodNodes ranks definitions by total degree over dependency edges.
func (i *Index) GodNodes(ctx context.Context, project string, limit int) (ports.GodNodesView, error) {
	view := ports.GodNodesView{Nodes: []ports.GraphGodNode{}}
	if err := i.ensureAnalyticsReadable(ctx); err != nil {
		return view, err
	}
	graph, err := i.loadSymbolGraph(ctx)
	if err != nil {
		return view, err
	}
	limit = clampLimit(limit, ports.DefaultGraphGodNodes, ports.MaxGraphGodNodes)
	candidates := make([]int, 0, len(graph.nodes))
	packages, isolated := 0, 0
	for index, node := range graph.nodes {
		if node.Kind == "package" {
			packages++
			continue
		}
		if graph.degree[index] == 0 {
			isolated++
			continue
		}
		candidates = append(candidates, index)
	}
	view.Total = len(candidates)
	sort.Slice(candidates, func(left, right int) bool {
		a, b := graph.nodes[candidates[left]], graph.nodes[candidates[right]]
		if graph.degree[candidates[left]] != graph.degree[candidates[right]] {
			return graph.degree[candidates[left]] > graph.degree[candidates[right]]
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
	communities, err := i.communitiesFor(ctx, ids)
	if err != nil {
		return view, err
	}
	denominator := float64(len(graph.nodes) - 1)
	for _, index := range candidates {
		node := graph.nodes[index]
		degree := graph.degree[index]
		centrality := 0.0
		if denominator > 0 {
			centrality = float64(degree) / denominator
		}
		community := communities[node.ID]
		view.Nodes = append(view.Nodes, ports.GraphGodNode{
			GraphSymbol: byID[node.ID],
			Degree:      degree,
			InDegree:    len(graph.in[index]),
			OutDegree:   len(graph.out[index]),
			Centrality:  centrality,
			Community:   community.name,
			CommunityID: community.id,
			Test:        IsTestFile(node.File),
			Why: fmt.Sprintf("%d distinct dependency relationship(s): %d incoming, %d outgoing across %d indexed definition(s); degree centrality %.4f",
				degree, len(graph.in[index]), len(graph.out[index]), len(graph.nodes), centrality),
		})
	}
	view.Truncated = truncated
	if truncated {
		view.Notes = append(view.Notes, fmt.Sprintf("showing the %d most connected of %d definitions; raise limit (at most %d) for more", limit, view.Total, ports.MaxGraphGodNodes))
	}
	if packages > 0 {
		view.Notes = append(view.Notes, fmt.Sprintf("%d package node(s) are excluded from the ranking: an import hub is not an abstraction", packages))
	}
	if isolated > 0 {
		view.Notes = append(view.Notes, fmt.Sprintf("%d definition(s) with no dependency edge are reported as orphans by stats, not ranked here", isolated))
	}
	return view, nil
}

// communityRef is the id and display name of one symbol's community.
type communityRef struct {
	id   int
	name string
}

// communitiesFor resolves the community of the given symbols in one query.
func (i *Index) communitiesFor(ctx context.Context, ids []string) (map[string]communityRef, error) {
	out := map[string]communityRef{}
	if len(ids) == 0 {
		return out, nil
	}
	for _, chunk := range chunkStrings(ids, sqlChunk) {
		query := `SELECT cm.symbol_id, c.id, c.name FROM community_members cm
			JOIN communities c ON c.id = cm.community_id
			WHERE cm.symbol_id IN (` + placeholders(len(chunk)) + `)`
		rows, err := i.db.QueryContext(ctx, query, stringsToAny(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("codeindex: read symbol communities: %w", err)
		}
		for rows.Next() {
			var symbolID, name string
			var id int
			if err := rows.Scan(&symbolID, &id, &name); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("codeindex: read symbol communities: %w", err)
			}
			out[symbolID] = communityRef{id: id, name: name}
		}
		rowErr := rows.Err()
		_ = rows.Close()
		if rowErr != nil {
			return nil, fmt.Errorf("codeindex: read symbol communities: %w", rowErr)
		}
	}
	return out, nil
}

// ShortestPath finds the shortest chain between two definitions. It searches
// outgoing dependency edges first and, only when that finds nothing, the same
// edges in both directions; the answer says which search succeeded.
func (i *Index) ShortestPath(ctx context.Context, project, from, to string, maxDepth int) (ports.GraphPath, error) {
	view := ports.GraphPath{}
	if err := i.ensureAnalyticsReadable(ctx); err != nil {
		return view, err
	}
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
	graph, err := i.loadSymbolGraph(ctx)
	if err != nil {
		return view, err
	}
	expand := func(undirected bool) func([]string) (map[string][]graphalgo.Neighbor, error) {
		return func(frontier []string) (map[string][]graphalgo.Neighbor, error) {
			out := make(map[string][]graphalgo.Neighbor, len(frontier))
			for _, id := range frontier {
				index, ok := graph.index[id]
				if !ok {
					continue
				}
				hops := make([]graphalgo.Neighbor, 0, len(graph.out[index])+len(graph.in[index]))
				seen := make(map[int]bool, len(graph.out[index]))
				for _, hop := range graph.out[index] {
					seen[hop.to] = true
					hops = append(hops, graphalgo.Neighbor{
						ID: graph.nodes[hop.to].ID, Edge: hop.edge, Line: hop.line, Confidence: hop.confidence,
					})
				}
				if undirected {
					for _, hop := range graph.in[index] {
						if seen[hop.to] {
							continue
						}
						hops = append(hops, graphalgo.Neighbor{
							ID: graph.nodes[hop.to].ID, Edge: hop.edge, Line: hop.line,
							Confidence: hop.confidence, Reverse: true,
						})
					}
				}
				out[id] = hops
			}
			return out, nil
		}
	}
	steps, found, err := graphalgo.ShortestPath(fromSymbol.ID, toSymbol.ID, maxDepth, expand(false))
	directed := true
	if err != nil {
		return view, err
	}
	if !found && fromSymbol.ID != toSymbol.ID {
		steps, found, err = graphalgo.ShortestPath(fromSymbol.ID, toSymbol.ID, maxDepth, expand(true))
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

// reachHop is the first edge that reached a symbol in a reverse walk.
type reachHop struct {
	distance   int
	via        string
	line       int
	confidence float64
	provenance string
}

// reverseReach walks incoming dependency edges from every seed at once and
// returns the shallowest hop of each reached symbol, capped at
// MaxGraphPRSymbols non-seed hits.
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
		for _, chunk := range chunkStrings(frontier, sqlChunk) {
			arguments := append(stringsToAny(chunk), stringsToAny(traversalEdgeKinds)...)
			query := `SELECT source, kind, line, confidence, provenance FROM edges
				WHERE target IN (` + placeholders(len(chunk)) + `)
				AND kind IN (` + placeholders(len(traversalEdgeKinds)) + `)
				ORDER BY source, kind, line`
			rows, err := i.db.QueryContext(ctx, query, arguments...)
			if err != nil {
				return nil, false, fmt.Errorf("codeindex: pr impact walk: %w", err)
			}
			for rows.Next() {
				var source, edge, provenance string
				var line int
				var confidence float64
				if err := rows.Scan(&source, &edge, &line, &confidence, &provenance); err != nil {
					_ = rows.Close()
					return nil, false, fmt.Errorf("codeindex: pr impact walk: %w", err)
				}
				if _, seen := best[source]; seen {
					continue
				}
				if hits >= ports.MaxGraphPRSymbols {
					truncated = true
					break
				}
				best[source] = reachHop{distance: distance, via: edge, line: line, confidence: confidence, provenance: provenance}
				hits++
				next = append(next, source)
			}
			rowErr := rows.Err()
			_ = rows.Close()
			if rowErr != nil {
				return nil, false, fmt.Errorf("codeindex: pr impact walk: %w", rowErr)
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

// PRImpact maps a git diff to changed files, changed symbols and their reverse
// blast radius. It runs git diff only; nothing is written to the checkout.
func (i *Index) PRImpact(ctx context.Context, project, base, head string) (ports.PRImpactView, error) {
	view := ports.PRImpactView{Base: strings.TrimSpace(base), Head: strings.TrimSpace(head)}
	if err := i.ensureAnalyticsReadable(ctx); err != nil {
		return view, err
	}
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
	idsByFile, err := i.symbolIDsByFile(ctx, paths)
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
		hop := reach[symbol.ID]
		hit := ports.ImpactHit{
			GraphSymbol: symbol, Distance: hop.distance, Via: hop.via,
			ViaFile: symbol.File, ViaLine: hop.line, Confidence: hop.confidence,
			Provenance: hop.provenance, Test: IsTestFile(symbol.File),
		}
		if hit.Test {
			tests[symbol.File] = true
		}
		view.Impact = append(view.Impact, hit)
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
		view.Notes = append(view.Notes, fmt.Sprintf("%d deleted file(s): their callers are what breaks; symbols still present in the index are included as seeds", deleted))
	}
	if unindexed > 0 {
		view.Notes = append(view.Notes, fmt.Sprintf("%d changed file(s) have no indexed definition (deleted after the index was built, generated or not Go)", unindexed))
	}
	view.Notes = append(view.Notes, fmt.Sprintf("risk %s: %d reachable symbol(s), %d changed symbol(s), %d deleted file(s)",
		view.Risk, len(view.Impact), len(allIDs), deleted))
	return view, nil
}

// symbolIDsByFile returns the definitions of every given path, grouped by
// path.
func (i *Index) symbolIDsByFile(ctx context.Context, paths []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, chunk := range chunkStrings(uniqueStrings(paths), sqlChunk) {
		query := `SELECT id, path FROM symbols WHERE path IN (` + placeholders(len(chunk)) + `) ORDER BY path, start_line`
		rows, err := i.db.QueryContext(ctx, query, stringsToAny(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("codeindex: read symbols of changed files: %w", err)
		}
		for rows.Next() {
			var id, path string
			if err := rows.Scan(&id, &path); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("codeindex: read symbols of changed files: %w", err)
			}
			out[path] = append(out[path], id)
		}
		rowErr := rows.Err()
		_ = rows.Close()
		if rowErr != nil {
			return nil, fmt.Errorf("codeindex: read symbols of changed files: %w", rowErr)
		}
	}
	return out, nil
}

// GraphStats reports the counts that need the whole graph.
func (i *Index) GraphStats(ctx context.Context, project string) (ports.GraphStatsView, error) {
	view := ports.GraphStatsView{}
	if err := i.ensureAnalyticsReadable(ctx); err != nil {
		return view, err
	}
	status, err := i.Status(ctx, project)
	if err != nil {
		return view, err
	}
	communities, err := i.communityCount(ctx)
	if err != nil {
		return view, err
	}
	arguments := append(stringsToAny(traversalEdgeKinds), stringsToAny(traversalEdgeKinds)...)
	var orphans int
	if err := i.db.QueryRowContext(ctx,
		`SELECT count(*) FROM symbols s
		 WHERE NOT EXISTS (SELECT 1 FROM edges e WHERE e.source = s.id AND e.kind IN (`+placeholders(len(traversalEdgeKinds))+`))
		   AND NOT EXISTS (SELECT 1 FROM edges e WHERE e.target = s.id AND e.kind IN (`+placeholders(len(traversalEdgeKinds))+`))`,
		arguments...).Scan(&orphans); err != nil {
		return view, fmt.Errorf("codeindex: count orphan definitions: %w", err)
	}
	languages, err := i.countBy(ctx, `SELECT language, count(*) FROM files GROUP BY language ORDER BY count(*) DESC, language`)
	if err != nil {
		return view, err
	}
	view = ports.GraphStatsView{
		Engine: Engine, Project: status.Project, DBPath: status.DBPath,
		SchemaVersion: status.SchemaVersion, ExtractionVersion: status.ExtractionVersion,
		Files: status.Files, Symbols: status.Nodes, Edges: status.Edges,
		Communities: communities, Orphans: orphans,
		Languages: languages, NodesByKind: status.NodesByKind,
	}
	if communities == 0 && status.Nodes > 0 {
		view.Notes = append(view.Notes, "no community partition is stored in this index; run brain code index --rebuild")
	}
	if singletons, err := i.singletonCommunityCount(ctx); err != nil {
		return view, err
	} else if singletons > 0 {
		view.Notes = append(view.Notes, fmt.Sprintf("%d singleton community(ies) hold definitions with no dependency edge; stats.orphans counts the same nodes", singletons))
	}
	return view, nil
}

// communityCount reads the number of stored communities.
func (i *Index) communityCount(ctx context.Context) (int, error) {
	var count int
	if err := i.db.QueryRowContext(ctx, `SELECT count(*) FROM communities`).Scan(&count); err != nil {
		return 0, fmt.Errorf("codeindex: count communities: %w", err)
	}
	return count, nil
}

// singletonCommunityCount reads the communities with one member: a definition
// with no dependency edge is its own community and is reported as an orphan.
func (i *Index) singletonCommunityCount(ctx context.Context) (int, error) {
	var count int
	if err := i.db.QueryRowContext(ctx, `SELECT count(*) FROM communities WHERE size = 1`).Scan(&count); err != nil {
		return 0, fmt.Errorf("codeindex: count singleton communities: %w", err)
	}
	return count, nil
}

// rebuildCommunities recomputes the partition inside the write transaction and
// preserves the id of a community whose member signature did not change. A new
// or changed community gets the next free id, so a reference to a community
// stays valid while its members do.
func rebuildCommunities(ctx context.Context, tx *sql.Tx) (int, error) {
	graph, err := loadSymbolGraphFrom(ctx, tx)
	if err != nil {
		return 0, err
	}
	if len(graph.nodes) == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM community_members`); err != nil {
			return 0, fmt.Errorf("codeindex: clear community members: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM communities`); err != nil {
			return 0, fmt.Errorf("codeindex: clear communities: %w", err)
		}
		return 0, nil
	}
	partition := graphalgo.DetectCommunities(len(graph.nodes), graph.edges, graphalgo.CommunityOptions{HubPercentile: communityHubPercentile})

	previous := map[string]int{}
	nextID := 1
	rows, err := tx.QueryContext(ctx, `SELECT id, signature FROM communities`)
	if err != nil {
		return 0, fmt.Errorf("codeindex: read previous communities: %w", err)
	}
	for rows.Next() {
		var id int
		var signature string
		if err := rows.Scan(&id, &signature); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("codeindex: read previous communities: %w", err)
		}
		previous[signature] = id
		if id >= nextID {
			nextID = id + 1
		}
	}
	rowErr := rows.Err()
	_ = rows.Close()
	if rowErr != nil {
		return 0, fmt.Errorf("codeindex: read previous communities: %w", rowErr)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM community_members`); err != nil {
		return 0, fmt.Errorf("codeindex: clear community members: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM communities`); err != nil {
		return 0, fmt.Errorf("codeindex: clear communities: %w", err)
	}
	insertCommunity, err := tx.PrepareContext(ctx,
		`INSERT INTO communities(id, name, top_symbol, size, cohesion, signature) VALUES(?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("codeindex: prepare community insert: %w", err)
	}
	defer func() { _ = insertCommunity.Close() }()
	insertMember, err := tx.PrepareContext(ctx,
		`INSERT INTO community_members(community_id, symbol_id) VALUES(?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("codeindex: prepare community member insert: %w", err)
	}
	defer func() { _ = insertMember.Close() }()
	used := map[int]bool{}
	for _, community := range partition.Communities {
		signature := memberSignature(graph.nodes, community.Members)
		id := previous[signature]
		if id == 0 || used[id] {
			for used[nextID] {
				nextID++
			}
			id = nextID
			nextID++
		}
		used[id] = true
		top := community.Members[0]
		for _, member := range community.Members[1:] {
			if graph.degree[member] > graph.degree[top] {
				top = member
			}
		}
		name := graph.nodes[top].Name
		if strings.TrimSpace(name) == "" {
			name = graph.nodes[top].QualName
		}
		if strings.TrimSpace(name) == "" {
			name = "community " + strconv.Itoa(id)
		}
		if _, err := insertCommunity.ExecContext(ctx, id, name, graph.nodes[top].ID,
			len(community.Members), community.Cohesion, signature); err != nil {
			return 0, fmt.Errorf("codeindex: write community %d: %w", id, err)
		}
		for _, member := range community.Members {
			if _, err := insertMember.ExecContext(ctx, id, graph.nodes[member].ID); err != nil {
				return 0, fmt.Errorf("codeindex: write community %d member: %w", id, err)
			}
		}
	}
	return len(partition.Communities), nil
}

// memberSignature hashes the sorted member ids of one community.
func memberSignature(nodes []graphNode, members []int) string {
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, nodes[member].ID)
	}
	sort.Strings(ids)
	hash := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return hex.EncodeToString(hash[:])[:communitySignatureRunes]
}
