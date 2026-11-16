package ports

import (
	"context"
	"time"
)

// Graph engines accepted by config.graph.engine.
const (
	// GraphEngineOff disables code-graph queries. It is the default.
	GraphEngineOff = "off"
	// GraphEngineCodeGraph reads an external codegraph index in place.
	GraphEngineCodeGraph = "codegraph"
	// GraphEngineNative builds and reads the native Go graph store.
	GraphEngineNative = "native"
)

// Retrieval tiers of a symbol search, best first. A hit records the tier that
// produced it so a caller can disclose weaker matches instead of presenting
// every candidate as equally certain.
const (
	GraphMatchExact = "exact"
	GraphMatchFTS   = "fts"
	GraphMatchLike  = "like"
	// GraphMatchPath marks a symbol selected by a pinned file path rather than
	// by its name.
	GraphMatchPath = "path"
)

// Limits shared by the adapter and the CLI. A zero limit selects the default;
// an adapter clamps anything above the maximum instead of failing a read.
const (
	// DefaultGraphSearchLimit is the number of hits one search returns.
	DefaultGraphSearchLimit = 10
	// MaxGraphSearchLimit bounds one search.
	MaxGraphSearchLimit = 100
	// DefaultGraphImpactDepth is the reverse-BFS depth of one impact query.
	DefaultGraphImpactDepth = 2
	// MaxGraphImpactDepth bounds the reverse-BFS depth.
	MaxGraphImpactDepth = 10
	// MaxGraphImpactNodes bounds the blast radius of one query.
	MaxGraphImpactNodes = 500
	// DefaultGraphExploreFiles is the number of files one explore renders.
	DefaultGraphExploreFiles = 12
	// MaxGraphExploreFiles bounds one explore.
	MaxGraphExploreFiles = 24
	// MaxGraphExploreBytes is the hard size cap of one rendered explore
	// answer. It stays under the host inline limit so a caller receives the
	// whole answer instead of a cliff.
	MaxGraphExploreBytes = 24 << 10

	// DefaultGraphCommunities is the number of communities one listing shows.
	DefaultGraphCommunities = 20
	// MaxGraphCommunities bounds one community listing.
	MaxGraphCommunities = 100
	// DefaultGraphCommunityMembers is the member list length of one selected
	// community.
	DefaultGraphCommunityMembers = 50
	// MaxGraphCommunityMembers bounds the member list of one community.
	MaxGraphCommunityMembers = 200
	// DefaultGraphGodNodes is the number of ranked nodes one god-node answer
	// shows.
	DefaultGraphGodNodes = 10
	// MaxGraphGodNodes bounds one god-node ranking.
	MaxGraphGodNodes = 50
	// DefaultGraphPathDepth is the hop limit of one path query.
	DefaultGraphPathDepth = 8
	// MaxGraphPathDepth bounds the hop limit of one path query.
	MaxGraphPathDepth = 16
	// MaxGraphPathNodes bounds the nodes of one reported path, including the
	// two endpoints.
	MaxGraphPathNodes = 20
	// DefaultGraphPRDepth is the reverse-BFS depth of one PR impact answer.
	DefaultGraphPRDepth = 2
	// MaxGraphPRSymbols bounds the changed and reachable symbols one PR
	// impact answer reports.
	MaxGraphPRSymbols = 500
	// MaxGraphPRFiles bounds the changed files one PR impact answer reports.
	MaxGraphPRFiles = 200

	// DefaultGraphCycles is the number of import cycles one answer lists.
	DefaultGraphCycles = 10
	// MaxGraphCycles bounds the import cycles one answer lists.
	MaxGraphCycles = 50
	// DefaultGraphCycleMembers is the member list length of one cycle.
	DefaultGraphCycleMembers = 20
	// MaxGraphCycleMembers bounds the member list of one cycle.
	MaxGraphCycleMembers = 50
	// DefaultGraphDiffSamples is the number of added and removed keys one
	// graph diff answer lists per category.
	DefaultGraphDiffSamples = 20
	// MaxGraphDiffSamples bounds the samples of one graph diff answer.
	MaxGraphDiffSamples = 100
)

// Risk tiers of a PR impact answer, from least to most likely to break a
// caller. The tier is computed from the reachable symbol count, the changed
// symbol count and deleted files; the adapter reports the inputs in Notes.
const (
	GraphRiskLow    = "low"
	GraphRiskMedium = "medium"
	GraphRiskHigh   = "high"
)

// GraphIndex answers symbol and relationship questions about one checkout.
//
// The project argument labels the answer; an implementation is bound to one
// checkout, because a graph index keyed by file path only means something
// against the tree it was built from. Implementations must never modify the
// index they read: Sync reports that the engine cannot write here and the
// caller runs the indexer itself.
type GraphIndex interface {
	// Status reports the index identity, schema, counts and age.
	Status(ctx context.Context, project string) (GraphStatus, error)
	// Sync asks the engine to refresh the index. A read-only adapter sets
	// GraphSyncResult.Unsupported and explains what to run instead.
	Sync(ctx context.Context, project string, paths []string) (GraphSyncResult, error)
	// Search finds symbols by exact name first, then full text, then
	// substring.
	Search(ctx context.Context, query GraphQuery) ([]SymbolHit, error)
	// Node returns one symbol with its source and its direct neighbours.
	Node(ctx context.Context, project, symbol, file string, opts NodeOpts) (NodeView, error)
	// Explore returns the source of the symbols that answer query, the call
	// flow between them and the blast radius, under MaxGraphExploreBytes.
	Explore(ctx context.Context, project, query string, maxFiles int) (ExploreView, error)
	// Impact walks incoming edges from symbol and reports what a change can
	// reach, marking test files.
	Impact(ctx context.Context, project, symbol, file string, depth int) (ImpactView, error)
	// Analytics exposes the optional analytics half of this engine. An
	// engine that cannot answer analytics returns nil, false and the caller
	// reports UNSUPPORTED instead of inventing an answer. The method is
	// cheap: it reports a capability, it does not compute one.
	Analytics() (GraphAnalytics, bool)
}

// GraphStatus is the read-only identity and size of one graph index.
type GraphStatus struct {
	Engine             string         `json:"engine"`
	Project            string         `json:"project"`
	Root               string         `json:"root,omitempty"`
	DBPath             string         `json:"db_path"`
	SchemaVersion      int            `json:"schema_version"`
	ExtractionVersion  int            `json:"extraction_version"`
	IndexedWithVersion string         `json:"indexed_with_version,omitempty"`
	IndexState         string         `json:"index_state,omitempty"`
	Commit             string         `json:"commit,omitempty"`
	Dirty              bool           `json:"dirty"`
	IndexedAt          time.Time      `json:"indexed_at"`
	AgeSeconds         int64          `json:"age_seconds"`
	SizeBytes          int64          `json:"size_bytes"`
	Files              int            `json:"files"`
	Nodes              int            `json:"nodes"`
	Edges              int            `json:"edges"`
	UnresolvedRefs     int            `json:"unresolved_refs"`
	NodesByKind        map[string]int `json:"nodes_by_kind,omitempty"`
	FilesByLanguage    map[string]int `json:"files_by_language,omitempty"`
	Notes              []string       `json:"notes,omitempty"`
}

// GraphSyncResult reports what a Sync did. A read-only adapter sets
// Unsupported and returns the command the caller should run instead.
type GraphSyncResult struct {
	Engine      string   `json:"engine"`
	Project     string   `json:"project,omitempty"`
	Changed     int      `json:"changed"`
	Unsupported bool     `json:"unsupported"`
	Notes       []string `json:"notes,omitempty"`
}

// GraphQuery narrows a symbol search.
type GraphQuery struct {
	Text string `json:"text"`
	// Kinds keeps only these node kinds. Empty selects the symbol kinds an
	// agent navigates (functions, methods, types, constants, routes); import
	// and file nodes are included only when named explicitly.
	Kinds []string `json:"kinds,omitempty"`
	// File keeps only symbols defined in that repository-relative path.
	File string `json:"file,omitempty"`
	// Limit is the number of hits; zero selects DefaultGraphSearchLimit.
	Limit int `json:"limit,omitempty"`
}

// GraphSymbol is one indexed definition. It carries a line range and the
// declaration text, never the body: source is read from the checkout when a
// caller asks for it.
type GraphSymbol struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	QualName   string `json:"qual_name,omitempty"`
	File       string `json:"file"`
	Language   string `json:"language,omitempty"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	Signature  string `json:"signature,omitempty"`
	Docstring  string `json:"docstring,omitempty"`
	Visibility string `json:"visibility,omitempty"`
	Exported   bool   `json:"exported,omitempty"`
}

// SymbolHit is one ranked search result.
type SymbolHit struct {
	GraphSymbol
	// Score orders hits within one tier: the retrieval rank for full-text
	// hits and a fixed tier bonus for exact and substring hits.
	Score float64 `json:"score"`
	// Match is GraphMatchExact, GraphMatchFTS or GraphMatchLike.
	Match string `json:"match"`
	// Confidence is the identity confidence of the match: 1.0 for an exact
	// indexed name, 0.8 for a full-text candidate, 0.5 for a substring one.
	Confidence float64 `json:"confidence"`
	// Provenance names where the row came from ("indexed" for a node).
	Provenance string `json:"provenance"`
}

// NodeOpts controls how much of one symbol Node returns.
type NodeOpts struct {
	// IncludeSource renders the definition's lines with line numbers.
	IncludeSource bool `json:"include_source"`
	// IncludeCallers and IncludeCallees add the direct neighbours.
	IncludeCallers bool `json:"include_callers"`
	IncludeCallees bool `json:"include_callees"`
	// Limit bounds each neighbour list; zero selects DefaultGraphSearchLimit.
	Limit int `json:"limit,omitempty"`
}

// SourceLine is one numbered line of repository source.
type SourceLine struct {
	Number int    `json:"number"`
	Text   string `json:"text"`
}

// GraphNeighbor is one edge between two symbols: the neighbour, the edge kind
// that links it and the call/import site.
type GraphNeighbor struct {
	GraphSymbol
	Edge string `json:"edge"`
	Line int    `json:"line,omitempty"`
	// Count is the number of distinct sites in this edge group; a caller that
	// invokes the same function five times is one neighbour with count 5.
	Count      int     `json:"count,omitempty"`
	Confidence float64 `json:"confidence"`
	Provenance string  `json:"provenance,omitempty"`
}

// NodeView is one symbol with optional source and direct neighbours. Notes
// carry what a caller must not have to infer: a stale file, an omitted body,
// an ambiguous or clipped answer.
type NodeView struct {
	Symbol          GraphSymbol     `json:"symbol"`
	Source          []SourceLine    `json:"source,omitempty"`
	SourceTruncated bool            `json:"source_truncated,omitempty"`
	Callers         []GraphNeighbor `json:"callers,omitempty"`
	Callees         []GraphNeighbor `json:"callees,omitempty"`
	Notes           []string        `json:"notes,omitempty"`
}

// ExploreFile is one file rendered by Explore, with the lines that belong to
// the matched symbols and markers where unrelated code was skipped.
type ExploreFile struct {
	Path      string       `json:"path"`
	Language  string       `json:"language,omitempty"`
	Symbols   []string     `json:"symbols,omitempty"`
	Lines     []SourceLine `json:"lines,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
	Note      string       `json:"note,omitempty"`
}

// FlowStep is one hop of a call path between named symbols.
type FlowStep struct {
	GraphSymbol
	// Via is the edge kind that reached this step; empty for the first.
	Via        string  `json:"via,omitempty"`
	Confidence float64 `json:"confidence"`
}

// ExploreView is one bounded answer: the matching symbols, their source, the
// flow between them and the blast radius.
type ExploreView struct {
	Query       string        `json:"query"`
	Symbols     []SymbolHit   `json:"symbols"`
	Files       []ExploreFile `json:"files,omitempty"`
	Flow        []FlowStep    `json:"flow,omitempty"`
	BlastRadius []ImpactHit   `json:"blast_radius,omitempty"`
	Pointers    []string      `json:"pointers,omitempty"`
	Notes       []string      `json:"notes,omitempty"`
	// Bytes is the rendered size the budget applied to. It is at most
	// MaxGraphExploreBytes.
	Bytes     int  `json:"bytes"`
	Truncated bool `json:"truncated,omitempty"`
}

// ImpactHit is one symbol a change can reach, with the edge that first
// reached it at the shallowest depth.
type ImpactHit struct {
	GraphSymbol
	Distance   int     `json:"distance"`
	Via        string  `json:"via"`
	ViaFile    string  `json:"via_file,omitempty"`
	ViaLine    int     `json:"via_line,omitempty"`
	Test       bool    `json:"test"`
	Confidence float64 `json:"confidence"`
	Provenance string  `json:"provenance,omitempty"`
}

// ImpactView is the reverse blast radius of one symbol. Hits are ordered by
// distance, then file and line. Files and Tests are the distinct paths that
// contain a hit, so a caller can answer "which tests touch this" without a
// second query.
type ImpactView struct {
	Symbol    GraphSymbol `json:"symbol"`
	Depth     int         `json:"depth"`
	Hits      []ImpactHit `json:"hits"`
	Files     []string    `json:"files,omitempty"`
	Tests     []string    `json:"tests,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
	Notes     []string    `json:"notes,omitempty"`
}

// GraphAnalytics is the optional analytics half of a graph engine. It is a
// separate interface because an engine can serve symbol navigation without
// being able to answer analytics: GraphIndex.Analytics reports whether the half
// exists, and a caller that gets false reports UNSUPPORTED instead of inventing
// an empty collection, a zero count or a ranking derived from something other
// than the indexed edges.
//
// Analytics never writes. PRImpact runs git diff in read-only mode against
// two refs of the checkout; it fetches no pull-request data and stores
// nothing.
type GraphAnalytics interface {
	// Communities lists detected communities by size, or one community with
	// its members. A community id is stable while its member set is
	// unchanged; ordering is by size, not by id.
	Communities(ctx context.Context, project string, opts CommunityQuery) (CommunityView, error)
	// GodNodes ranks the most connected definitions by total degree.
	GodNodes(ctx context.Context, project string, limit int) (GodNodesView, error)
	// ShortestPath finds the shortest chain between two definitions, first
	// along dependency edges and then, when that fails, ignoring direction.
	ShortestPath(ctx context.Context, project, from, to string, maxDepth int) (GraphPath, error)
	// PRImpact maps the files and symbols changed between two git refs to
	// their reverse blast radius, a risk tier and the test files involved.
	PRImpact(ctx context.Context, project, base, head string) (PRImpactView, error)
	// Cycles lists strongly connected components over import edges: a Go
	// package, TypeScript module or Python module cycle. Components below two
	// members and self-imports are not cycles.
	Cycles(ctx context.Context, project string, opts CycleQuery) (CyclesView, error)
	// Diff compares this index with another index of the same engine and
	// reports file, symbol and dependency-edge deltas. Both sides are read
	// only; no index is rewritten.
	Diff(ctx context.Context, project string, opts GraphDiffQuery) (GraphDiffView, error)
	// GraphStats reports counts that need the whole graph: communities,
	// orphan definitions and the language mix.
	GraphStats(ctx context.Context, project string) (GraphStatsView, error)
}

// CommunityQuery selects what a Communities call returns. The zero value lists
// the largest communities; ID selects exactly one.
type CommunityQuery struct {
	// ID is a 1-based community id. Zero returns the largest communities up to
	// Limit.
	ID int `json:"community_id,omitempty"`
	// Limit bounds the listed communities; zero selects
	// DefaultGraphCommunities and an engine clamps above MaxGraphCommunities.
	Limit int `json:"limit,omitempty"`
	// MemberLimit bounds the member list of one selected community; zero
	// selects DefaultGraphCommunityMembers and an engine clamps above
	// MaxGraphCommunityMembers.
	MemberLimit int `json:"member_limit,omitempty"`
}

// GraphCommunity is one detected cluster of definitions.
type GraphCommunity struct {
	ID int `json:"id"`
	// Name is the display label: the name of the highest-degree member.
	Name string `json:"name"`
	// Size is the member count.
	Size int `json:"size"`
	// Cohesion is the internal edge density of the undirected member
	// subgraph: internal edges over n*(n-1)/2. A singleton is 0.
	Cohesion float64 `json:"cohesion"`
	// Signature is a short hash of the sorted member ids. Two answers with
	// the same signature have the same members, which is what makes an id
	// worth citing.
	Signature string `json:"signature"`
	// Top is the highest-degree member.
	Top GraphSymbol `json:"top"`
	// Members is filled only when one community was selected; it is ordered
	// by file and line and capped by the query's MemberLimit.
	Members          []GraphSymbol `json:"members,omitempty"`
	MembersTruncated bool          `json:"members_truncated,omitempty"`
}

// CommunityView is one community listing or one community with members.
type CommunityView struct {
	Communities []GraphCommunity `json:"communities"`
	// Total is the number of detected communities, not the number returned.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated,omitempty"`
	// Cached is true when the partition was served from an in-process cache
	// instead of being computed for this call. The native engine stores its
	// partition and leaves it false.
	Cached bool     `json:"cached,omitempty"`
	Notes  []string `json:"notes,omitempty"`
}

// GraphGodNode is one ranked definition. Degree counts distinct neighbours
// over dependency edges in both directions, so repeated call sites of the same
// pair do not inflate it.
type GraphGodNode struct {
	GraphSymbol
	Degree      int     `json:"degree"`
	InDegree    int     `json:"in_degree"`
	OutDegree   int     `json:"out_degree"`
	Centrality  float64 `json:"centrality"`
	Community   string  `json:"community,omitempty"`
	CommunityID int     `json:"community_id,omitempty"`
	Test        bool    `json:"test,omitempty"`
	// Why states the facts behind the rank: both degree directions and the
	// centrality formula. It is not a guess about importance.
	Why string `json:"why"`
}

// GodNodesView is one degree ranking.
type GodNodesView struct {
	Nodes []GraphGodNode `json:"nodes"`
	// Total is the number of definitions the ranking considered.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated,omitempty"`
	// Cached is true when the community column came from an in-process
	// partition cache instead of a computation of this call.
	Cached bool     `json:"cached,omitempty"`
	Notes  []string `json:"notes,omitempty"`
}

// GraphPathStep is one node of a path. Edge, Line, Confidence and Reverse
// describe the hop that reached it; the first step has an empty Edge.
type GraphPathStep struct {
	GraphSymbol
	Edge       string  `json:"edge,omitempty"`
	Line       int     `json:"line,omitempty"`
	Reverse    bool    `json:"reverse,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

// GraphPath is the shortest chain between two definitions.
type GraphPath struct {
	From GraphSymbol `json:"from"`
	To   GraphSymbol `json:"to"`
	// Steps includes both endpoints: Hops is len(Steps)-1.
	Steps []GraphPathStep `json:"steps,omitempty"`
	Hops  int             `json:"hops"`
	Found bool            `json:"found"`
	// Directed is true when the path follows dependency edges only; false
	// when the search had to ignore direction to find any path.
	Directed bool `json:"directed"`
	// Confidence is the weakest edge confidence on the path.
	Confidence float64  `json:"confidence"`
	Notes      []string `json:"notes,omitempty"`
}

// PRFileImpact is one file changed between the two refs.
type PRFileImpact struct {
	Path string `json:"path"`
	// Status is the git name-status letter: A, M, D, T, R or C.
	Status string `json:"status"`
	// Symbols is the number of indexed definitions in that file.
	Symbols int `json:"symbols"`
}

// PRImpactView is the read-only impact of a git diff. It is not a pull-request
// answer: no GitHub or GitLab API is called, and no PR metadata is stored.
type PRImpactView struct {
	Base string `json:"base"`
	Head string `json:"head"`
	// Files is the changed file list, capped at MaxGraphPRFiles.
	Files []PRFileImpact `json:"files"`
	// ChangedFiles is the number of files git reported, not the number listed.
	ChangedFiles int `json:"changed_files"`
	// ChangedSymbols are the indexed definitions of the changed files, capped
	// at MaxGraphPRSymbols.
	ChangedSymbols []GraphSymbol `json:"changed_symbols,omitempty"`
	// Impact is the reverse blast radius of ChangedSymbols, capped at
	// MaxGraphPRSymbols and ordered by distance.
	Impact []ImpactHit `json:"impact,omitempty"`
	// TotalImpact is the number of reachable symbols the walk found. When
	// Truncated is set the walk stopped at the cap, so the true reach is
	// larger.
	TotalImpact int    `json:"total_impact"`
	Risk        string `json:"risk"`
	// Tests are the test files among the changed files and the reachable
	// symbols.
	Tests     []string `json:"tests,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Notes     []string `json:"notes,omitempty"`
}

// GraphStatsView reports the counts that need the whole graph.
type GraphStatsView struct {
	Engine            string `json:"engine"`
	Project           string `json:"project"`
	DBPath            string `json:"db_path,omitempty"`
	SchemaVersion     int    `json:"schema_version"`
	ExtractionVersion int    `json:"extraction_version"`
	Files             int    `json:"files"`
	Symbols           int    `json:"symbols"`
	Edges             int    `json:"edges"`
	Communities       int    `json:"communities"`
	// Orphans are definitions with no dependency edge in either direction.
	Orphans     int            `json:"orphans"`
	Languages   map[string]int `json:"languages,omitempty"`
	NodesByKind map[string]int `json:"nodes_by_kind,omitempty"`
	// Cached is true when the community count came from an in-process
	// partition cache instead of a computation of this call.
	Cached bool     `json:"cached,omitempty"`
	Notes  []string `json:"notes,omitempty"`
}

// CycleQuery selects how much of the import-cycle report one call returns.
type CycleQuery struct {
	// Limit bounds the listed cycles; zero selects DefaultGraphCycles and an
	// engine clamps above MaxGraphCycles.
	Limit int `json:"limit,omitempty"`
	// MemberLimit bounds the member list of one cycle; zero selects
	// DefaultGraphCycleMembers and an engine clamps above MaxGraphCycleMembers.
	MemberLimit int `json:"member_limit,omitempty"`
}

// GraphCycle is one strongly connected component of the import graph: a Go
// package, TypeScript module or Python module import cycle. Members are
// ordered by file and line.
type GraphCycle struct {
	// Size is the member count, including members a listing clipped.
	Size             int           `json:"size"`
	Members          []GraphSymbol `json:"members,omitempty"`
	MembersTruncated bool          `json:"members_truncated,omitempty"`
}

// CyclesView is the import-cycle report. Total is the number of components
// found, not the number returned; a cut sets Truncated and adds a note.
type CyclesView struct {
	Cycles []GraphCycle `json:"cycles"`
	// Total is the number of import cycles detected.
	Total int `json:"total"`
	// Nodes is the number of distinct nodes that belong to a cycle.
	Nodes int `json:"nodes"`
	// SelfLoops is the number of import edges from a node to itself; a
	// self-import is not a cycle between modules and is reported separately.
	SelfLoops int      `json:"self_loops,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Notes     []string `json:"notes,omitempty"`
}

// GraphDiffQuery selects the index a diff compares against. The base index is
// another index of the same engine, normally the index of a second checkout or
// worktree of the same repository at the base commit.
type GraphDiffQuery struct {
	// BaseProject labels the base side in the answer.
	BaseProject string `json:"base_project,omitempty"`
	// BaseIndexPath is the base index file. It is opened read-only and is
	// never written.
	BaseIndexPath string `json:"base_index_path"`
	// Limit bounds the added and removed samples per category; zero selects
	// DefaultGraphDiffSamples and an engine clamps above MaxGraphDiffSamples.
	Limit int `json:"limit,omitempty"`
}

// GraphDiffCount is the number of added and removed keys of one category.
type GraphDiffCount struct {
	Added   int `json:"added"`
	Removed int `json:"removed"`
}

// GraphDiffEdge is one dependency edge of a diff sample. The edge is keyed by
// source, target and kind, so repeated call sites of one pair are one edge.
type GraphDiffEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

// GraphDiffView is the difference between two indexes of one engine. Samples
// are ordered by key and capped; counts are always the true totals of the set
// difference, so a clipped sample never understates the delta.
type GraphDiffView struct {
	BaseProject string         `json:"base_project"`
	HeadProject string         `json:"head_project"`
	BaseIndex   string         `json:"base_index"`
	HeadIndex   string         `json:"head_index"`
	BaseCommit  string         `json:"base_commit,omitempty"`
	HeadCommit  string         `json:"head_commit,omitempty"`
	Files       GraphDiffCount `json:"files"`
	Symbols     GraphDiffCount `json:"symbols"`
	Edges       GraphDiffCount `json:"edges"`
	// Communities compares stored community signatures. An engine that
	// computes the partition in memory reports zero and says so in Notes.
	Communities        GraphDiffCount  `json:"communities"`
	AddedFiles         []string        `json:"added_files,omitempty"`
	RemovedFiles       []string        `json:"removed_files,omitempty"`
	AddedSymbols       []GraphSymbol   `json:"added_symbols,omitempty"`
	RemovedSymbols     []GraphSymbol   `json:"removed_symbols,omitempty"`
	AddedEdges         []GraphDiffEdge `json:"added_edges,omitempty"`
	RemovedEdges       []GraphDiffEdge `json:"removed_edges,omitempty"`
	AddedCommunities   []string        `json:"added_communities,omitempty"`
	RemovedCommunities []string        `json:"removed_communities,omitempty"`
	Truncated          bool            `json:"truncated,omitempty"`
	Notes              []string        `json:"notes,omitempty"`
}
