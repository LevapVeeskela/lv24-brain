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
