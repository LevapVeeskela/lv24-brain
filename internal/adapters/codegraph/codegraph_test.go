package codegraph

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// openFixture builds the fixture and opens it read-only.
func openFixture(t *testing.T, f *fixture) *Index {
	t.Helper()
	root, dbPath := f.build()
	index, err := Open(context.Background(), Options{DBPath: dbPath, Root: root, Project: "demo"})
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() {
		if err := index.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return index
}

func TestStatusReportsIndexIdentity(t *testing.T) {
	// Arrange
	f := newFixture(t)
	index := openFixture(t, f)

	// Act
	status, err := index.Status(context.Background(), "demo")

	// Assert
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Engine != Engine || status.SchemaVersion != SupportedSchemaVersion {
		t.Fatalf("engine/schema = %q/%d", status.Engine, status.SchemaVersion)
	}
	if status.ExtractionVersion != SupportedExtractionVersion || status.IndexedWithVersion != VerifiedIndexedWithVersion {
		t.Fatalf("versions = %d/%q", status.ExtractionVersion, status.IndexedWithVersion)
	}
	if status.Project != "demo" || status.IndexState != "complete" {
		t.Fatalf("project/state = %q/%q", status.Project, status.IndexState)
	}
	if status.Files != len(f.files) || status.Nodes != len(f.nodes) {
		t.Fatalf("counts = %d files/%d nodes, want %d/%d", status.Files, status.Nodes, len(f.files), len(f.nodes))
	}
	if status.Edges != len(f.edges) {
		t.Fatalf("edges = %d, want %d", status.Edges, len(f.edges))
	}
	if status.UnresolvedRefs != 1 {
		t.Fatalf("unresolved = %d, want 1", status.UnresolvedRefs)
	}
	if status.AgeSeconds < 3500 || status.AgeSeconds > 3700 {
		t.Fatalf("age = %d seconds, want about 3600", status.AgeSeconds)
	}
	if status.SizeBytes <= 0 || status.NodesByKind["function"] != 4 {
		t.Fatalf("size/kinds = %d/%v", status.SizeBytes, status.NodesByKind)
	}
	if status.FilesByLanguage["go"] != len(f.files) {
		t.Fatalf("languages = %v", status.FilesByLanguage)
	}
}

func TestOpenRefusesSchemaDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fixture)
		want   string
	}{
		{name: "schema", mutate: func(f *fixture) { f.schema = 12 }, want: "schema 12"},
		{name: "extraction", mutate: func(f *fixture) { f.extraction = 28 }, want: "extraction 28"},
		{name: "old release", mutate: func(f *fixture) { f.indexedWith = "1.5.9" }, want: "older than the supported minimum"},
		{name: "missing version", mutate: func(f *fixture) { f.indexedWith = "" }, want: "indexed_with_version is missing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Arrange
			f := newFixture(t)
			test.mutate(f)
			root, dbPath := f.build()

			// Act
			index, err := Open(context.Background(), Options{DBPath: dbPath, Root: root})

			// Assert
			if err == nil {
				_ = index.Close()
				t.Fatal("expected the index to be refused")
			}
			if !errors.Is(err, domain.ErrUnsupported) {
				t.Fatalf("error does not unwrap to ErrUnsupported: %v", err)
			}
			var schemaErr *SchemaError
			if !errors.As(err, &schemaErr) {
				t.Fatalf("error is not a *SchemaError: %v", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not mention %q", err, test.want)
			}
		})
	}
}

func TestOpenRefusesForeignKeylessDatabase(t *testing.T) {
	// Arrange: a SQLite file that is not a CodeGraph index.
	dbPath := filepath.Join(t.TempDir(), "not-codegraph.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Act
	index, err := Open(context.Background(), Options{DBPath: dbPath})

	// Assert
	if err == nil {
		_ = index.Close()
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "not a CodeGraph index") {
		t.Fatalf("error = %v", err)
	}
}

func TestIndexIsReadOnly(t *testing.T) {
	// Arrange
	index := openFixture(t, newFixture(t))
	ctx := context.Background()

	// Act
	var queryOnly int
	if err := index.db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		t.Fatal(err)
	}
	_, writeErr := index.db.ExecContext(ctx,
		`INSERT INTO project_metadata(key, value, updated_at) VALUES ('written', '1', 0)`)

	// Assert
	if queryOnly != 1 {
		t.Fatalf("query_only = %d, want 1", queryOnly)
	}
	if writeErr == nil {
		t.Fatal("a write succeeded against a read-only index")
	}
	if !strings.Contains(strings.ToLower(writeErr.Error()), "readonly") && !strings.Contains(strings.ToLower(writeErr.Error()), "read-only") {
		t.Fatalf("write error = %v, want a read-only refusal", writeErr)
	}
}

func TestSearchTiers(t *testing.T) {
	index := openFixture(t, newFixture(t))
	ctx := context.Background()

	t.Run("exact name", func(t *testing.T) {
		hits, err := index.Search(ctx, ports.GraphQuery{Text: "EventCache"})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].Match != ports.GraphMatchExact {
			t.Fatalf("hits = %+v, want one exact hit", hits)
		}
		if hits[0].Confidence != 1 || hits[0].Signature != "(key string) string" {
			t.Fatalf("confidence/signature = %v/%q", hits[0].Confidence, hits[0].Signature)
		}
	})
	t.Run("case insensitive qualified name", func(t *testing.T) {
		hits, err := index.Search(ctx, ports.GraphQuery{Text: "api.handler"})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].Name != "Handler" || hits[0].Match != ports.GraphMatchExact {
			t.Fatalf("hits = %+v", hits)
		}
	})
	t.Run("full text from docstring", func(t *testing.T) {
		hits, err := index.Search(ctx, ports.GraphQuery{Text: "stores event"})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) == 0 || hits[0].Match != ports.GraphMatchFTS {
			t.Fatalf("hits = %+v, want a full-text hit", hits)
		}
		if hits[0].Name != "EventCache" || hits[0].Confidence != 0.8 || hits[0].Score <= 0 {
			t.Fatalf("hit = %+v", hits[0])
		}
	})
	t.Run("substring fallback", func(t *testing.T) {
		hits, err := index.Search(ctx, ports.GraphQuery{Text: "ventCach"})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].Match != ports.GraphMatchLike || hits[0].Confidence != 0.5 {
			t.Fatalf("hits = %+v, want one substring hit", hits)
		}
	})
	t.Run("kind filter", func(t *testing.T) {
		hits, err := index.Search(ctx, ports.GraphQuery{Text: "EventCache", Kinds: []string{"method"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 0 {
			t.Fatalf("hits = %+v, want none", hits)
		}
	})
	t.Run("file filter", func(t *testing.T) {
		hits, err := index.Search(ctx, ports.GraphQuery{Text: "ListKeys", File: "internal/cache/keys.go"})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].File != "internal/cache/keys.go" {
			t.Fatalf("hits = %+v", hits)
		}
	})
	t.Run("empty query", func(t *testing.T) {
		if _, err := index.Search(ctx, ports.GraphQuery{Text: "  "}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("error = %v, want a validation error", err)
		}
	})
}

func TestSearchExactTierStopsFuzzyHits(t *testing.T) {
	// Arrange: a node named exactly Cache plus nodes whose docstrings mention
	// caching, so the FTS tier would also answer.
	f := newFixture(t)
	f.addFile("internal/cache/chain.go", `package cache

// Cache is the exact definition.
func Cache() string { return "cache" }
`)
	f.addNode(ports.GraphSymbol{
		ID: "function:cache", Kind: "function", Name: "Cache", QualName: "Cache",
		File: "internal/cache/chain.go", Language: "go", StartLine: 4, EndLine: 4,
		Signature: "() string", Docstring: "exact definition of the cache lookup",
	})
	f.addNode(ports.GraphSymbol{
		ID: "function:cache-lookup", Kind: "function", Name: "CacheLookup", QualName: "CacheLookup",
		File: "internal/cache/chain.go", Language: "go", StartLine: 4, EndLine: 4,
		Signature: "() string", Docstring: "a cache lookup helper",
	})
	index := openFixture(t, f)

	// Act
	hits, err := index.Search(context.Background(), ports.GraphQuery{Text: "Cache"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Name != "Cache" || hits[0].Match != ports.GraphMatchExact {
		t.Fatalf("hits = %+v, want only the exact Cache", hits)
	}
}

func TestNodeReturnsSourceAndNeighbors(t *testing.T) {
	index := openFixture(t, newFixture(t))
	ctx := context.Background()

	// Act
	view, err := index.Node(ctx, "demo", "EventCache", "", ports.NodeOpts{
		IncludeSource: true, IncludeCallers: true, IncludeCallees: true,
	})

	// Assert
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	if view.Symbol.Name != "EventCache" || len(view.Source) != 3 {
		t.Fatalf("symbol/source = %+v/%d lines", view.Symbol, len(view.Source))
	}
	if view.Source[0].Number != 4 || !strings.Contains(view.Source[0].Text, "func EventCache") {
		t.Fatalf("first source line = %+v", view.Source[0])
	}
	if len(view.Callers) != 2 {
		t.Fatalf("callers = %+v, want hashKey and the test file node", view.Callers)
	}
	if view.Callers[0].Name != "hashKey" || view.Callers[0].Edge != "calls" || view.Callers[0].Line != 9 {
		t.Fatalf("first caller = %+v", view.Callers[0])
	}
	if view.Callers[0].Confidence != 0.9 || view.Callers[0].Provenance != "exact-match" {
		t.Fatalf("caller confidence/provenance = %v/%q", view.Callers[0].Confidence, view.Callers[0].Provenance)
	}
	if len(view.Callees) != 0 {
		t.Fatalf("callees = %+v, want none", view.Callees)
	}

	// Act: the caller side of the chain.
	caller, err := index.Node(ctx, "demo", "hashKey", "", ports.NodeOpts{IncludeCallees: true})

	// Assert
	if err != nil {
		t.Fatalf("node hashKey: %v", err)
	}
	if len(caller.Callees) != 1 || caller.Callees[0].Name != "EventCache" {
		t.Fatalf("callees = %+v", caller.Callees)
	}
}

func TestNodeCollapsesRepeatedCallSites(t *testing.T) {
	// Arrange: one caller that calls the same symbol three times.
	f := newFixture(t)
	f.addEdge("function:list-keys", "function:event-cache", "calls", 10, "", 0.9, "exact-match")
	f.addEdge("function:list-keys", "function:event-cache", "calls", 11, "", 0.9, "exact-match")
	f.addEdge("function:list-keys", "function:event-cache", "calls", 12, "", 0.9, "exact-match")
	index := openFixture(t, f)

	// Act
	view, err := index.Node(context.Background(), "demo", "EventCache", "", ports.NodeOpts{IncludeCallers: true})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	for _, caller := range view.Callers {
		if caller.Name != "list-keys" && caller.Name != "ListKeys" {
			continue
		}
		if caller.Count != 3 {
			t.Fatalf("ListKeys count = %d, want 3", caller.Count)
		}
		return
	}
	t.Fatalf("ListKeys is missing from callers: %+v", view.Callers)
}

func TestNodeAmbiguityNeedsAFile(t *testing.T) {
	// Arrange: the same name in two files.
	f := newFixture(t)
	f.addFile("internal/cache/parse.go", "package cache\n\nfunc Parse() string { return \"\" }")
	f.addNode(ports.GraphSymbol{ID: "function:parse-cache", Kind: "function", Name: "Parse", QualName: "Parse",
		File: "internal/cache/parse.go", Language: "go", StartLine: 3, EndLine: 3})
	f.addFile("internal/api/parse.go", "package api\n\nfunc Parse() string { return \"\" }")
	f.addNode(ports.GraphSymbol{ID: "function:parse-api", Kind: "function", Name: "Parse", QualName: "Parse",
		File: "internal/api/parse.go", Language: "go", StartLine: 3, EndLine: 3})
	index := openFixture(t, f)
	ctx := context.Background()

	// Act
	_, err := index.Node(ctx, "demo", "Parse", "", ports.NodeOpts{})

	// Assert
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("error = %v, want ErrAmbiguous", err)
	}
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) || len(ambiguous.Candidates) != 2 {
		t.Fatalf("candidates = %+v", ambiguous)
	}

	// Act and assert: a file disambiguates.
	view, err := index.Node(ctx, "demo", "Parse", "internal/api/parse.go", ports.NodeOpts{})
	if err != nil {
		t.Fatalf("node with file: %v", err)
	}
	if view.Symbol.ID != "function:parse-api" {
		t.Fatalf("resolved %q", view.Symbol.ID)
	}
	if _, err := index.Node(ctx, "demo", "Parse", "internal/none.go", ports.NodeOpts{}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want not found", err)
	}
}

func TestNodeRefusesSourceOutsideCheckout(t *testing.T) {
	// Arrange: the index claims a path outside the checkout.
	f := newFixture(t)
	f.addNode(ports.GraphSymbol{ID: "function:escape", Kind: "function", Name: "Escape", QualName: "Escape",
		File: "../secret.txt", Language: "text", StartLine: 1, EndLine: 1})
	index := openFixture(t, f)

	// Act
	view, err := index.Node(context.Background(), "demo", "Escape", "", ports.NodeOpts{IncludeSource: true})

	// Assert
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	if len(view.Source) != 0 {
		t.Fatalf("source = %+v, want none", view.Source)
	}
	if len(view.Notes) == 0 || !strings.Contains(strings.Join(view.Notes, " "), "escapes the checkout") {
		t.Fatalf("notes = %+v", view.Notes)
	}
}

func TestSourceSanitizesControlSequences(t *testing.T) {
	// Arrange: a source line with ansi escapes and a NUL byte.
	f := newFixture(t)
	f.addFile("internal/cache/ansi.go", "package cache\n\nfunc ANSI() string {\n\treturn \"\x1b[31mred\x1b[0m\" + \"\x00\"\n}")
	f.addNode(ports.GraphSymbol{ID: "function:ansi", Kind: "function", Name: "ANSI", QualName: "ANSI",
		File: "internal/cache/ansi.go", Language: "go", StartLine: 3, EndLine: 5})
	index := openFixture(t, f)

	// Act
	view, err := index.Node(context.Background(), "demo", "ANSI", "", ports.NodeOpts{IncludeSource: true})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, line := range view.Source {
		joined += line.Text + "\n"
	}
	if strings.ContainsRune(joined, 0x1b) || strings.ContainsRune(joined, 0x00) {
		t.Fatalf("sanitized source still has control bytes: %q", joined)
	}
	if !strings.Contains(joined, "\treturn") || !strings.Contains(joined, "red") {
		t.Fatalf("sanitized source = %q", joined)
	}
}

func TestExploreBudgetIsProportionalAndCapped(t *testing.T) {
	// Arrange: six files with 300-line bodies, far more than one answer holds.
	f := newFixture(t)
	f.addRadiusFiles(6, 300)
	index := openFixture(t, f)

	// Act
	view, err := index.Explore(context.Background(), "demo", "Radius", 6)

	// Assert
	if err != nil {
		t.Fatalf("explore: %v", err)
	}
	if len(view.Symbols) == 0 || view.Symbols[0].Match != ports.GraphMatchFTS {
		t.Fatalf("symbols = %+v", view.Symbols)
	}
	if view.Bytes > ports.MaxGraphExploreBytes {
		t.Fatalf("bytes = %d, over the %d cap", view.Bytes, ports.MaxGraphExploreBytes)
	}
	if len(view.Files) < 3 {
		t.Fatalf("files = %d, want at least three", len(view.Files))
	}
	if !view.Truncated {
		t.Fatal("expected the answer to be truncated")
	}
	clipped := false
	for _, file := range view.Files {
		if file.Truncated {
			clipped = true
		}
	}
	if !clipped {
		t.Fatalf("no file reported clipping: %+v", view.Files)
	}
	first, last := len(view.Files[0].Lines), len(view.Files[len(view.Files)-1].Lines)
	if first <= last {
		t.Fatalf("first file has %d lines, last has %d: the budget is not proportional", first, last)
	}

	// Act: a lower file cap moves the rest to pointers.
	narrow, err := index.Explore(context.Background(), "demo", "Radius", 2)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(narrow.Files) != 2 || len(narrow.Pointers) == 0 || !narrow.Truncated {
		t.Fatalf("files/pointers/truncated = %d/%v/%v", len(narrow.Files), narrow.Pointers, narrow.Truncated)
	}
}

func TestExplorePinsPathsAndLineAnchors(t *testing.T) {
	index := openFixture(t, newFixture(t))
	ctx := context.Background()

	// Act
	view, err := index.Explore(ctx, "demo", "internal/cache/cache.go:8 what does this do", 4)

	// Assert
	if err != nil {
		t.Fatalf("explore: %v", err)
	}
	found := false
	for _, symbol := range view.Symbols {
		if symbol.Name == "hashKey" && symbol.Match == ports.GraphMatchPath {
			found = true
		}
	}
	if !found {
		t.Fatalf("symbols = %+v, want the pinned path", view.Symbols)
	}
	if len(view.Symbols) != 1 {
		t.Fatalf("symbols = %+v, want only the definition overlapping line 8", view.Symbols)
	}
	if len(view.Files) == 0 || view.Files[0].Path != "internal/cache/cache.go" {
		t.Fatalf("files = %+v", view.Files)
	}

	// Act: an absolute anchor inside the checkout.
	absolute := filepath.Join(index.Root(), "internal", "cache", "cache.go") + ":8"
	absoluteView, err := index.Explore(ctx, "demo", absolute, 2)

	// Assert
	if err != nil {
		t.Fatalf("explore absolute: %v", err)
	}
	if len(absoluteView.Files) == 0 || absoluteView.Files[0].Path != "internal/cache/cache.go" {
		t.Fatalf("absolute files = %+v, notes = %+v", absoluteView.Files, absoluteView.Notes)
	}

	// Act: an unknown pinned path is disclosed, not silently dropped.
	unknown, err := index.Explore(ctx, "demo", "internal/nowhere/missing.go:3", 2)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(unknown.Notes, " "), "is not in the index") {
		t.Fatalf("notes = %+v", unknown.Notes)
	}
}

func TestExploreDisclosesStaleSource(t *testing.T) {
	// Arrange
	f := newFixture(t)
	index := openFixture(t, f)
	path := filepath.Join(index.Root(), "internal", "cache", "cache.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, []byte("// edited after indexing\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	// Act
	view, err := index.Explore(context.Background(), "demo", "EventCache", 2)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(view.Notes, " "), "changed since the index was built") {
		t.Fatalf("notes = %+v", view.Notes)
	}
}

func TestImpactDepthAndTestDetection(t *testing.T) {
	index := openFixture(t, newFixture(t))
	ctx := context.Background()

	// Act
	view, err := index.Impact(ctx, "demo", "EventCache", "", 3)

	// Assert
	if err != nil {
		t.Fatalf("impact: %v", err)
	}
	byName := map[string]ports.ImpactHit{}
	for _, hit := range view.Hits {
		byName[hit.Name] = hit
	}
	if byName["hashKey"].Distance != 1 || byName["ListKeys"].Distance != 2 || byName["Handler"].Distance != 3 {
		t.Fatalf("distances = %+v", view.Hits)
	}
	if byName["hashKey"].Via != "calls" || byName["hashKey"].ViaLine != 9 {
		t.Fatalf("hashKey hop = %+v", byName["hashKey"])
	}
	if len(view.Tests) == 0 || !strings.Contains(strings.Join(view.Tests, ","), "cache_test.go") {
		t.Fatalf("tests = %+v", view.Tests)
	}
	if !strings.Contains(strings.Join(view.Files, ","), "internal/cache/cache_test.go") {
		t.Fatalf("files = %+v", view.Files)
	}
	for _, hit := range view.Hits {
		if hit.Test && hit.File == "" {
			t.Fatalf("test hit without a file: %+v", hit)
		}
	}

	// Act: one hop sees only direct callers.
	shallow, err := index.Impact(ctx, "demo", "EventCache", "", 1)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range shallow.Hits {
		if hit.Distance != 1 {
			t.Fatalf("distance %d at depth 1: %+v", hit.Distance, shallow.Hits)
		}
	}

	// Act: an oversized depth is clamped and disclosed.
	deep, err := index.Impact(ctx, "demo", "EventCache", "", 99)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if deep.Depth != ports.MaxGraphImpactDepth {
		t.Fatalf("depth = %d", deep.Depth)
	}
	if !strings.Contains(strings.Join(deep.Notes, " "), "clamped") {
		t.Fatalf("notes = %+v", deep.Notes)
	}

	// Act and assert: an unknown root is not found.
	if _, err := index.Impact(ctx, "demo", "Missing", "", 2); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want not found", err)
	}
}

func TestSyncIsUnsupported(t *testing.T) {
	index := openFixture(t, newFixture(t))

	result, err := index.Sync(context.Background(), "demo", []string{"internal/cache/cache.go"})

	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !result.Unsupported || len(result.Notes) == 0 || !strings.Contains(result.Notes[0], "codegraph sync") {
		t.Fatalf("result = %+v", result)
	}
}

func TestIsTestFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"internal/cache/cache_test.go", true},
		{"src/app.test.ts", true},
		{"src/app.spec.tsx", true},
		{"tests/test_orders.py", true},
		{"pkg/__tests__/helper.js", true},
		{"e2e/login.spec.ts", true},
		{"internal/cache/cache.go", false},
		{"internal/testdata/fixture.go", false},
		{"docs/testing.md", false},
	}
	for _, test := range tests {
		if got := IsTestFile(test.path); got != test.want {
			t.Errorf("IsTestFile(%q) = %v, want %v", test.path, got, test.want)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		left, right string
		want        int
	}{
		{"1.6.2", "1.6.2", 0},
		{"1.6.10", "1.6.2", 1},
		{"1.6", "1.6.1", -1},
		{"v1.7.0", "1.6.2", 1},
	}
	for _, test := range tests {
		if got := compareVersions(test.left, test.right); got != test.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
		}
	}
}

// TestLiveIndexSmoke reads the real CodeGraph index when it exists. It is a
// read-only smoke test: no codegraph command runs and nothing is written.
func TestLiveIndexSmoke(t *testing.T) {
	const (
		liveRoot = `C:\0project\lv24\main`
		liveDB   = `C:\0project\lv24\main\.codegraph\codegraph.db`
	)
	if _, err := os.Stat(liveDB); err != nil {
		t.Skipf("live codegraph index is absent: %v", err)
	}
	ctx := context.Background()
	index, err := Open(ctx, Options{DBPath: liveDB, Root: liveRoot, Project: "lv24"})
	if err != nil {
		t.Fatalf("open live index: %v", err)
	}
	defer func() { _ = index.Close() }()

	status, err := index.Status(ctx, "lv24")
	if err != nil {
		t.Fatalf("live status: %v", err)
	}
	if status.SchemaVersion != SupportedSchemaVersion || status.Nodes == 0 || status.Files == 0 {
		t.Fatalf("live status = %+v", status)
	}

	view, err := index.Explore(ctx, "lv24", "hashEventsListQuery", 3)
	if err != nil {
		t.Fatalf("live explore: %v", err)
	}
	if view.Bytes > ports.MaxGraphExploreBytes {
		t.Fatalf("live explore bytes = %d", view.Bytes)
	}
	if len(view.Symbols) == 0 {
		t.Fatalf("live explore returned no symbols: %+v", view.Notes)
	}
	t.Logf("live status: files=%d nodes=%d edges=%d index=%s", status.Files, status.Nodes, status.Edges, status.IndexedAt.Format(time.RFC3339))
	t.Logf("live explore: %d symbols, %d files, %d bytes", len(view.Symbols), len(view.Files), view.Bytes)
}
