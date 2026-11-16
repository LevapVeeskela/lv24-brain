package codeindex

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// lowConfidence is the edge confidence below which a hop is disclosed as
// heuristic rather than presented as a fact.
const lowConfidence = 0.7

// Impact walks incoming edges from symbol and reports the definitions a change
// can reach. It is a breadth-first search over calls, references, imports,
// implements and embeds, so every hit carries its shallowest distance.
func (i *Index) Impact(ctx context.Context, project, symbol, file string, depth int) (ports.ImpactView, error) {
	if err := i.ensureReadable(ctx); err != nil {
		return ports.ImpactView{}, err
	}
	root, err := i.resolveSymbol(ctx, symbol, file)
	if err != nil {
		return ports.ImpactView{}, err
	}
	return i.impactFromSymbol(ctx, root, depth)
}

// impactFromSymbol runs the reverse BFS from an already resolved definition.
func (i *Index) impactFromSymbol(ctx context.Context, root ports.GraphSymbol, depth int) (ports.ImpactView, error) {
	requested := depth
	if depth <= 0 {
		depth = ports.DefaultGraphImpactDepth
	}
	if depth > ports.MaxGraphImpactDepth {
		depth = ports.MaxGraphImpactDepth
	}
	view := ports.ImpactView{Symbol: root, Depth: depth}
	if requested > ports.MaxGraphImpactDepth {
		view.Notes = append(view.Notes, fmt.Sprintf("depth %d clamped to the %d-hop limit", requested, ports.MaxGraphImpactDepth))
	}

	type hop struct {
		distance   int
		via        string
		viaLine    int
		confidence float64
		provenance string
	}
	best := map[string]hop{root.ID: {distance: 0}}
	frontier := []string{root.ID}
	truncated := false
	heuristic := 0
	for distance := 1; distance <= depth && len(frontier) > 0; distance++ {
		next := make([]string, 0, len(frontier))
		for _, chunk := range chunkStrings(frontier, sqlChunk) {
			arguments := append(stringsToAny(chunk), stringsToAny(traversalEdgeKinds)...)
			query := `SELECT source, kind, line, confidence, provenance
				FROM edges WHERE target IN (` + placeholders(len(chunk)) + `)
				AND kind IN (` + placeholders(len(traversalEdgeKinds)) + `)
				ORDER BY source, kind, line`
			rows, err := i.db.QueryContext(ctx, query, arguments...)
			if err != nil {
				return ports.ImpactView{}, fmt.Errorf("codeindex: impact from %s: %w", root.ID, err)
			}
			for rows.Next() {
				var source, edge, provenance string
				var line int
				var confidence float64
				if err := rows.Scan(&source, &edge, &line, &confidence, &provenance); err != nil {
					_ = rows.Close()
					return ports.ImpactView{}, fmt.Errorf("codeindex: impact from %s: %w", root.ID, err)
				}
				if _, seen := best[source]; seen {
					continue
				}
				if len(best) >= ports.MaxGraphImpactNodes {
					truncated = true
					break
				}
				if confidence < lowConfidence {
					heuristic++
				}
				best[source] = hop{
					distance:   distance,
					via:        edge,
					viaLine:    line,
					confidence: confidence,
					provenance: provenance,
				}
				next = append(next, source)
			}
			rowErr := rows.Err()
			_ = rows.Close()
			if rowErr != nil {
				return ports.ImpactView{}, fmt.Errorf("codeindex: impact from %s: %w", root.ID, rowErr)
			}
			if truncated {
				break
			}
		}
		if truncated {
			break
		}
		frontier = next
	}

	ids := make([]string, 0, len(best))
	for id := range best {
		if id != root.ID {
			ids = append(ids, id)
		}
	}
	nodes, err := i.loadSymbols(ctx, ids)
	if err != nil {
		return ports.ImpactView{}, err
	}
	files := map[string]bool{}
	view.Hits = make([]ports.ImpactHit, 0, len(nodes))
	for _, node := range nodes {
		info := best[node.ID]
		files[node.File] = true
		view.Hits = append(view.Hits, ports.ImpactHit{
			GraphSymbol: node,
			Distance:    info.distance,
			Via:         info.via,
			ViaFile:     node.File,
			ViaLine:     info.viaLine,
			Confidence:  info.confidence,
			Provenance:  info.provenance,
			Test:        IsTestFile(node.File),
		})
	}
	sort.SliceStable(view.Hits, func(left, right int) bool {
		if view.Hits[left].Distance != view.Hits[right].Distance {
			return view.Hits[left].Distance < view.Hits[right].Distance
		}
		if view.Hits[left].File != view.Hits[right].File {
			return view.Hits[left].File < view.Hits[right].File
		}
		return view.Hits[left].StartLine < view.Hits[right].StartLine
	})
	view.Files = make([]string, 0, len(files))
	for path := range files {
		view.Files = append(view.Files, path)
	}
	sort.Strings(view.Files)
	view.Tests = make([]string, 0, 4)
	for _, path := range view.Files {
		if IsTestFile(path) {
			view.Tests = append(view.Tests, path)
		}
	}
	view.Truncated = truncated
	if truncated {
		view.Notes = append(view.Notes, fmt.Sprintf("blast radius stopped at %d nodes; narrow the query with --depth or a file", ports.MaxGraphImpactNodes))
	}
	if heuristic > 0 {
		view.Notes = append(view.Notes, fmt.Sprintf("%d hop(s) resolve through heuristics below %.1f confidence", heuristic, lowConfidence))
	}
	return view, nil
}

// loadSymbols loads the given symbol ids in one batch, ordered by file and
// line.
func (i *Index) loadSymbols(ctx context.Context, ids []string) ([]ports.GraphSymbol, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	sort.Strings(ids)
	out := make([]ports.GraphSymbol, 0, len(ids))
	for _, chunk := range chunkStrings(ids, sqlChunk) {
		query := `SELECT ` + symbolColumns + ` FROM symbols WHERE id IN (` + placeholders(len(chunk)) + `)`
		rows, err := i.db.QueryContext(ctx, query, stringsToAny(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("codeindex: load symbols: %w", err)
		}
		for rows.Next() {
			symbol, err := scanSymbol(rows, false)
			if err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("codeindex: load symbols: %w", err)
			}
			out = append(out, symbol)
		}
		rowErr := rows.Err()
		_ = rows.Close()
		if rowErr != nil {
			return nil, fmt.Errorf("codeindex: load symbols: %w", rowErr)
		}
	}
	sort.Slice(out, func(left, right int) bool {
		if out[left].File != out[right].File {
			return out[left].File < out[right].File
		}
		return out[left].StartLine < out[right].StartLine
	})
	return out, nil
}

// IsTestFile reports whether a repository path belongs to a test suite. The
// suffix list covers Go and the script languages the native engine indexes, so
// a TypeScript spec file is marked without a second rule.
func IsTestFile(path string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(path, `\`, "/"))
	base := normalized
	if slash := strings.LastIndex(normalized, "/"); slash >= 0 {
		base = normalized[slash+1:]
	}
	for _, suffix := range []string{
		"_test.go", "_test.py", "_test.dart", "_test.rs", "_test.exs",
		".test.ts", ".test.tsx", ".test.js", ".test.jsx", ".test.mjs", ".test.cjs",
		".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx", ".spec.mjs",
		"_spec.rb", ".spec.dart", "test.java", "tests.cs", "test.cs",
	} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	for _, segment := range strings.Split(normalized, "/") {
		switch segment {
		case "__tests__", "tests", "test", "spec", "specs", "e2e", "integration_tests", "integration-tests":
			return true
		}
	}
	return false
}
