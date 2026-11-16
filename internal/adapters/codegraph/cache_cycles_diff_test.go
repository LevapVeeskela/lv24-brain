package codegraph

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestAnalyticsCacheReportsHitsAndInvalidates(t *testing.T) {
	resetCodeGraphCache()
	f := newFixture(t)
	root, dbPath := f.build()
	index := openIndex(t, dbPath, root)
	ctx := context.Background()

	first, err := index.Communities(ctx, "demo", ports.CommunityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Cached {
		t.Fatal("the first partition was reported as cached")
	}
	second, err := index.Communities(ctx, "demo", ports.CommunityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Cached {
		t.Fatal("the second partition was not served from the cache")
	}
	if first.Total != second.Total {
		t.Fatalf("cached partition changed: %d -> %d", first.Total, second.Total)
	}
	stats, err := index.GraphStats(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Cached {
		t.Fatal("stats did not reuse the cached partition")
	}

	// Any write to the index file changes its identity, so the next call
	// computes a fresh partition.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO nodes(id, kind, name, qualified_name, file_path, language,
		start_line, end_line, start_column, end_column, updated_at)
		VALUES('function:cache-invalidation', 'function', 'Fresh', 'Fresh', 'internal/api/fresh.go', 'go', 1, 2, 0, 0, 0)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := index.Communities(ctx, "demo", ports.CommunityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if third.Cached {
		t.Fatal("a changed index was served from the cache")
	}

	// The WAL is part of the identity even before a checkpoint.
	key := index.cacheKey()
	wal := dbPath + "-wal"
	if err := os.WriteFile(wal, []byte("wal"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(wal) })
	if other := index.cacheKey(); other == key {
		t.Fatalf("the WAL did not change the cache key: %q", key)
	}
}

func TestAnalyticsCacheEvictsOldEntries(t *testing.T) {
	resetCodeGraphCache()
	t.Cleanup(resetCodeGraphCache)
	for index := 0; index < codeGraphCacheEntries+2; index++ {
		key := string(rune('a' + index))
		graph := &codeGraph{nodes: []codeNode{{ID: key, Name: key}}}
		storeCodeGraph(key, graph)
	}
	graphCache.mu.Lock()
	defer graphCache.mu.Unlock()
	if len(graphCache.entries) != codeGraphCacheEntries {
		t.Fatalf("cache entries = %d, want %d", len(graphCache.entries), codeGraphCacheEntries)
	}
	if _, ok := graphCache.entries["a"]; ok {
		t.Fatalf("the oldest entry was not evicted: %v", graphCache.entries)
	}
	if estimateCodeGraphBytes(&codeGraph{nodes: []codeNode{{ID: "x"}}}) <= 0 {
		t.Fatal("the byte estimate is not positive")
	}
}

func TestCodegraphCycles(t *testing.T) {
	resetCodeGraphCache()
	f := newFixture(t)
	for _, name := range []string{"a", "b", "c", "d"} {
		f.addNode(ports.GraphSymbol{
			ID: "file:import/" + name, Kind: "file", Name: name + ".ts",
			QualName: "import/" + name, File: "import/" + name + ".ts", Language: "typescript",
			StartLine: 1, EndLine: 2,
		})
	}
	f.addEdge("file:import/a", "file:import/b", "imports", 1, "", 1, "exact")
	f.addEdge("file:import/b", "file:import/c", "imports", 1, "", 1, "exact")
	f.addEdge("file:import/c", "file:import/a", "imports", 1, "", 1, "exact")
	f.addEdge("file:import/c", "file:import/d", "imports", 1, "", 1, "exact")
	f.addEdge("file:import/d", "file:import/d", "imports", 1, "", 1, "exact")
	root, dbPath := f.build()
	index := openIndex(t, dbPath, root)
	ctx := context.Background()

	view, err := index.Cycles(ctx, "demo", ports.CycleQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if view.Total != 1 || view.Nodes != 3 || view.SelfLoops != 1 || len(view.Cycles) != 1 {
		t.Fatalf("cycles = %+v", view)
	}
	if view.Cycles[0].Size != 3 || len(view.Cycles[0].Members) != 3 {
		t.Fatalf("cycle = %+v", view.Cycles[0])
	}
	again, err := index.Cycles(ctx, "demo", ports.CycleQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for position := range view.Cycles[0].Members {
		if view.Cycles[0].Members[position].ID != again.Cycles[0].Members[position].ID {
			t.Fatalf("cycle is not deterministic: %+v / %+v", view.Cycles, again.Cycles)
		}
	}
	capped, err := index.Cycles(ctx, "demo", ports.CycleQuery{Limit: 1, MemberLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(capped.Cycles) != 1 || len(capped.Cycles[0].Members) != 1 || !capped.Cycles[0].MembersTruncated {
		t.Fatalf("cycle caps = %+v", capped)
	}
}

func TestCodegraphDiffTwoIndexes(t *testing.T) {
	resetCodeGraphCache()
	baseFixture := newFixture(t)
	baseRoot, basePath := baseFixture.build()
	headFixture := newFixture(t)
	headFixture.addNode(ports.GraphSymbol{
		ID: "function:fresh", Kind: "function", Name: "Fresh", QualName: "Fresh",
		File: "internal/api/fresh.go", Language: "go", StartLine: 1, EndLine: 3,
	})
	headFixture.addEdge("function:fresh", "function:handler", "calls", 2, "", 1, "exact")
	headRoot, headPath := headFixture.build()
	head := openIndex(t, headPath, headRoot)
	ctx := context.Background()

	view, err := head.Diff(ctx, "head", ports.GraphDiffQuery{BaseProject: "base", BaseIndexPath: basePath})
	if err != nil {
		t.Fatal(err)
	}
	if view.BaseIndex != basePath || view.HeadIndex != headPath {
		t.Fatalf("indexes = %+v", view)
	}
	if view.Symbols.Added != 1 || view.Symbols.Removed != 0 {
		t.Fatalf("symbols = %+v", view.Symbols)
	}
	if len(view.AddedSymbols) != 1 || view.AddedSymbols[0].Name != "Fresh" {
		t.Fatalf("added symbols = %+v", view.AddedSymbols)
	}
	if view.Edges.Added != 1 || len(view.AddedEdges) != 1 {
		t.Fatalf("edges = %+v", view.Edges)
	}
	if view.Edges.Added > 0 && view.AddedEdges[0].Kind != "calls" {
		t.Fatalf("edge sample = %+v", view.AddedEdges)
	}
	if view.Files.Added != 0 {
		// The fixture writes every source file in both roots, so only the
		// node rows differ.
		t.Fatalf("files = %+v", view.Files)
	}
	if view.Communities.Added+view.Communities.Removed != 0 {
		t.Fatalf("a codegraph partition was diffed: %+v", view.Communities)
	}
	if !notesContain(view.Notes, "computed in memory") {
		t.Fatalf("notes = %v", view.Notes)
	}
	again, err := head.Diff(ctx, "head", ports.GraphDiffQuery{BaseProject: "base", BaseIndexPath: basePath})
	if err != nil {
		t.Fatal(err)
	}
	if again.Symbols != view.Symbols || again.Edges != view.Edges {
		t.Fatalf("diff is not deterministic: %+v / %+v", view, again)
	}
	if _, err := head.Diff(ctx, "head", ports.GraphDiffQuery{BaseIndexPath: headPath}); err == nil {
		t.Fatal("an index was diffed against itself")
	}
	if _, err := head.Diff(ctx, "head", ports.GraphDiffQuery{BaseIndexPath: baseRoot}); err == nil {
		t.Fatal("a directory was accepted as a base index")
	}
}

// openIndex opens one already-built fixture index.
func openIndex(t *testing.T, dbPath, root string) *Index {
	t.Helper()
	index, err := Open(context.Background(), Options{DBPath: dbPath, Root: root, Project: "demo"})
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return index
}
