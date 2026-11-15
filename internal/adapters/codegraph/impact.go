package codegraph

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// impactChunk bounds the parameters of one IN list. SQLite builds ship with a
// limit near 1000; 400 keeps every query well inside it.
const impactChunk = 400

// lowConfidence is the edge confidence below which a hop is disclosed as
// heuristic rather than presented as a fact.
const lowConfidence = 0.7

// Impact walks incoming edges from symbol and reports the definitions a change
// can reach. It is a breadth-first search over calls, references, imports and
// the other dependency edges, so every hit carries its shallowest distance.
func (i *Index) Impact(ctx context.Context, project, symbol, file string, depth int) (ports.ImpactView, error) {
	root, err := i.resolveSymbol(ctx, symbol, file)
	if err != nil {
		return ports.ImpactView{}, err
	}
	return i.impactFromSymbol(ctx, root, depth)
}

// impactFromSymbol runs the reverse BFS from an already resolved definition.
// Explore reuses it for the blast-radius section of one answer.
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
	if root.Kind == "file" {
		view.Notes = append(view.Notes, "the root is a file node: results are the definitions and files that reference it")
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
		for _, chunk := range chunkStrings(frontier, impactChunk) {
			arguments := append(stringsToAny(chunk), stringsToAny(traversalEdgeKinds)...)
			query := `SELECT source, kind, COALESCE(line, 0), COALESCE(provenance, ''), metadata
				FROM edges WHERE target IN (` + placeholders(len(chunk)) + `)
				AND kind IN (` + placeholders(len(traversalEdgeKinds)) + `)
				ORDER BY source, kind, line`
			rows, err := i.db.QueryContext(ctx, query, arguments...)
			if err != nil {
				return ports.ImpactView{}, fmt.Errorf("codegraph: impact from %s: %w", root.ID, err)
			}
			for rows.Next() {
				var source, edge, provenance string
				var line int
				var metadata sql.NullString
				if err := rows.Scan(&source, &edge, &line, &provenance, &metadata); err != nil {
					_ = rows.Close()
					return ports.ImpactView{}, fmt.Errorf("codegraph: impact from %s: %w", root.ID, err)
				}
				if _, seen := best[source]; seen {
					continue
				}
				if len(best) >= ports.MaxGraphImpactNodes {
					truncated = true
					break
				}
				confidence, resolvedBy := edgeMetadata(metadata)
				if confidence < lowConfidence {
					heuristic++
				}
				best[source] = hop{
					distance:   distance,
					via:        edge,
					viaLine:    line,
					confidence: confidence,
					provenance: provenanceOf(provenance, resolvedBy),
				}
				next = append(next, source)
			}
			rowErr := rows.Err()
			_ = rows.Close()
			if rowErr != nil {
				return ports.ImpactView{}, fmt.Errorf("codegraph: impact from %s: %w", root.ID, rowErr)
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
		if node.Kind == "file" {
			// A file node is a dependency carrier, not a symbol a change
			// "reaches": keep its path and continue the walk, but do not
			// present a whole file as one changed definition.
			files[node.File] = true
			continue
		}
		hit := ports.ImpactHit{
			GraphSymbol: node,
			Distance:    info.distance,
			Via:         info.via,
			ViaFile:     node.File,
			ViaLine:     info.viaLine,
			Confidence:  info.confidence,
			Provenance:  info.provenance,
			Test:        IsTestFile(node.File),
		}
		files[node.File] = true
		view.Hits = append(view.Hits, hit)
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

// loadSymbols loads the given node ids in one batch, ordered by file and line.
func (i *Index) loadSymbols(ctx context.Context, ids []string) ([]ports.GraphSymbol, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]ports.GraphSymbol, 0, len(ids))
	for _, chunk := range chunkStrings(ids, impactChunk) {
		query := `SELECT ` + nodeColumns + ` FROM nodes WHERE id IN (` + placeholders(len(chunk)) + `)`
		rows, err := i.db.QueryContext(ctx, query, stringsToAny(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("codegraph: load symbols: %w", err)
		}
		for rows.Next() {
			symbol, err := scanNode(rows)
			if err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("codegraph: load symbols: %w", err)
			}
			out = append(out, symbol)
		}
		rowErr := rows.Err()
		_ = rows.Close()
		if rowErr != nil {
			return nil, fmt.Errorf("codegraph: load symbols: %w", rowErr)
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

// IsTestFile reports whether a repository path belongs to a test suite. It is
// deliberately suffix and directory based: the question is where the code
// lives, not what the parser inferred.
func IsTestFile(path string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(path, `\`, "/"))
	base := normalized
	if slash := strings.LastIndex(normalized, "/"); slash >= 0 {
		base = normalized[slash+1:]
	}
	suffixes := []string{
		"_test.go", "_test.py", "_test.dart", "_test.rs", "_test.exs",
		".test.ts", ".test.tsx", ".test.js", ".test.jsx", ".test.mjs", ".test.cjs",
		".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx", ".spec.mjs",
		"_spec.rb", ".spec.dart", "test.java", "tests.cs", "test.cs",
	}
	for _, suffix := range suffixes {
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

// chunkStrings splits values into deterministic batches.
func chunkStrings(values []string, size int) [][]string {
	if size <= 0 {
		size = len(values)
	}
	out := make([][]string, 0, (len(values)+size-1)/size)
	for start := 0; start < len(values); start += size {
		end := start + size
		if end > len(values) {
			end = len(values)
		}
		out = append(out, values[start:end])
	}
	return out
}
