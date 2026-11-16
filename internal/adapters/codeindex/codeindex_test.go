package codeindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// fixtureFiles is a small Go module with two packages, an interface, two
// implementations, embedding, generics, constants, a package variable and a
// test.
var fixtureFiles = map[string]string{
	"go.mod": "module example.com/fixture\n\ngo 1.21\n",
	"cache/cache.go": `// Package cache stores values.
package cache

import "errors"

// ErrMiss is returned when a key is absent.
var ErrMiss = errors.New("cache: miss")

// DefaultKey is used when no key is given.
const DefaultKey = "default"

// Store is a key-value reader.
type Store interface {
	// Get returns one value.
	Get(key string) (string, bool)
}

// MemoryStore keeps values in memory.
type MemoryStore struct {
	values map[string]string
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{values: map[string]string{}}
}

// Get returns one value.
func (m *MemoryStore) Get(key string) (string, bool) {
	value, ok := m.values[key]
	return value, ok
}

// Put stores one value.
func (m *MemoryStore) Put(key, value string) {
	m.values[key] = value
}
`,
	"cache/codec.go": `package cache

// Codec encodes values.
type Codec interface {
	Encode(value string) string
}

// JSONCodec is an identity codec in this fixture.
type JSONCodec struct{}

// Encode returns the value unchanged.
func (JSONCodec) Encode(value string) string { return value }
`,
	"cache/embedded.go": `package cache

// Cache parents a store and a codec.
type Cache struct {
	*MemoryStore
	codec Codec
}
`,
	"cache/generic.go": `package cache

// First returns the first item of a generic slice.
func First[T any](items []T) (T, bool) {
	var zero T
	if len(items) == 0 {
		return zero, false
	}
	return items[0], true
}
`,
	"service/service.go": `// Package service reads through a cache.
package service

import "example.com/fixture/cache"

// Fetch returns one value through store.
func Fetch(store cache.Store, key string) string {
	value, ok := store.Get(key)
	if !ok {
		return ""
	}
	return value
}

// New returns a populated store.
func New() cache.Store {
	store := cache.NewMemoryStore()
	store.Put(cache.DefaultKey, "value")
	return store
}

// BodyOnly is never called; its body must not be copied into the database.
func BodyOnly() string {
	marker := "BODY_MARKER_9f3a1"
	return marker
}
`,
	"service/service_test.go": `package service

import (
	"testing"

	"example.com/fixture/cache"
)

func TestFetch(t *testing.T) {
	if got := Fetch(cache.NewMemoryStore(), cache.DefaultKey); got != "" {
		t.Fatal(got)
	}
}
`,
	"cmd/main/main.go": `package main

import (
	"fmt"

	"example.com/fixture/service"
)

func main() {
	fmt.Println(service.New())
}
`,
}

// writeFixture writes the fixture module under root.
func writeFixture(t *testing.T, root string) {
	t.Helper()
	for path, content := range fixtureFiles {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fixtureIndex is one open test store.
type fixtureIndex struct {
	index  *Index
	dbPath string
	root   string
	data   string
	now    time.Time
}

// newFixture creates a checkout, a data directory and an open store.
func newFixture(t *testing.T) *fixtureIndex {
	t.Helper()
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	data := filepath.Join(base, "brain")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, root)
	now := time.Unix(1_700_000_000, 0).UTC()
	dbPath, err := DefaultDBPath(data, "fixture", root)
	if err != nil {
		t.Fatal(err)
	}
	index, err := Open(context.Background(), Options{
		Root:    root,
		DBPath:  dbPath,
		Project: "fixture",
		Create:  true,
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return &fixtureIndex{index: index, dbPath: dbPath, root: root, data: data, now: now}
}

// advance moves the fixture clock forward.
func (f *fixtureIndex) advance(delta time.Duration) {
	f.now = f.now.Add(delta)
	f.index.now = func() time.Time { return f.now }
}

// mustIndex runs a full index.
func (f *fixtureIndex) mustIndex(t *testing.T) ports.GraphSyncResult {
	t.Helper()
	result, err := f.index.Index(context.Background(), "fixture")
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	return result
}

// mustSync runs an incremental sync.
func (f *fixtureIndex) mustSync(t *testing.T, paths ...string) ports.GraphSyncResult {
	t.Helper()
	result, err := f.index.Sync(context.Background(), "fixture", paths)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return result
}

// count runs a scalar count query.
func (f *fixtureIndex) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var value int
	if err := f.index.db.QueryRowContext(context.Background(), query, args...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

// symbolID resolves one symbol id by name and path.
func (f *fixtureIndex) symbolID(t *testing.T, name, file string) string {
	t.Helper()
	var id string
	err := f.index.db.QueryRowContext(context.Background(),
		`SELECT id FROM symbols WHERE name = ? AND path = ? LIMIT 1`, name, file).Scan(&id)
	if err != nil {
		t.Fatalf("symbol %s in %s: %v", name, file, err)
	}
	return id
}

// symbolIDByQual resolves one symbol id by its qualified name.
func (f *fixtureIndex) symbolIDByQual(t *testing.T, qualname string) string {
	t.Helper()
	var id string
	err := f.index.db.QueryRowContext(context.Background(),
		`SELECT id FROM symbols WHERE qualname = ?`, qualname).Scan(&id)
	if err != nil {
		t.Fatalf("symbol %s: %v", qualname, err)
	}
	return id
}

// hasEdge reports whether an edge of a kind exists between two ids.
func (f *fixtureIndex) hasEdge(t *testing.T, source, target, kind string) bool {
	t.Helper()
	var count int
	if err := f.index.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM edges WHERE source = ? AND target = ? AND kind = ?`,
		source, target, kind).Scan(&count); err != nil {
		t.Fatalf("edge query: %v", err)
	}
	return count > 0
}

func TestIndexExtractsSymbolsAndEdges(t *testing.T) {
	fixture := newFixture(t)
	result := fixture.mustIndex(t)
	if result.Changed != len(fixtureFiles)-1 {
		t.Fatalf("changed = %d, want %d (go.mod is not a Go file)", result.Changed, len(fixtureFiles)-1)
	}
	status, err := fixture.index.Status(context.Background(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if status.Engine != Engine || status.IndexState != stateComplete {
		t.Fatalf("status = %+v", status)
	}
	if status.Files != len(fixtureFiles)-1 {
		t.Fatalf("files = %d, want %d", status.Files, len(fixtureFiles)-1)
	}
	if status.Nodes == 0 || status.Edges == 0 {
		t.Fatalf("empty graph: %+v", status)
	}
	if status.NodesByKind["package"] != 3 {
		t.Fatalf("package nodes = %d, want 3 (cache, service, main)", status.NodesByKind["package"])
	}
	for _, kind := range []string{"func", "method", "type", "const", "var"} {
		if status.NodesByKind[kind] == 0 {
			t.Fatalf("no %s symbols: %+v", kind, status.NodesByKind)
		}
	}
	if status.FilesByLanguage["go"] != status.Files {
		t.Fatalf("files by language = %+v", status.FilesByLanguage)
	}
	if status.SizeBytes <= 0 || status.IndexedAt.IsZero() {
		t.Fatalf("missing size or time: %+v", status)
	}
	if !strings.Contains(status.DBPath, filepath.Join("index", "graph")) {
		t.Fatalf("db path %s is not under index/graph", status.DBPath)
	}
	if rel, err := filepath.Rel(fixture.root, status.DBPath); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("db path %s is inside the checkout", status.DBPath)
	}

	// Exact search finds package-level and method definitions.
	for _, name := range []string{"Fetch", "MemoryStore", "DefaultKey", "ErrMiss", "First", "BodyOnly"} {
		hits, err := fixture.index.Search(context.Background(), ports.GraphQuery{Text: name})
		if err != nil {
			t.Fatalf("search %s: %v", name, err)
		}
		if len(hits) == 0 || hits[0].Match != ports.GraphMatchExact || hits[0].Name != name {
			t.Fatalf("search %s = %+v", name, hits)
		}
	}
	method, err := fixture.index.Search(context.Background(), ports.GraphQuery{Text: "cache.MemoryStore.Get"})
	if err != nil {
		t.Fatal(err)
	}
	if len(method) != 1 || method[0].Kind != "method" || method[0].Match != ports.GraphMatchExact {
		t.Fatalf("qualified method = %+v", method)
	}
	generic, err := fixture.index.Search(context.Background(), ports.GraphQuery{Text: "First", Kinds: []string{"func"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(generic) != 1 || !strings.Contains(generic[0].Signature, "[T any]") {
		t.Fatalf("generic signature = %+v", generic)
	}

	fetch := fixture.symbolID(t, "Fetch", "service/service.go")
	storeGet := fixture.symbolIDByQual(t, "cache.Store.Get")
	if !fixture.hasEdge(t, fetch, storeGet, "calls") {
		t.Fatalf("Fetch does not call Store.Get")
	}
	memoryGet := fixture.symbolIDByQual(t, "cache.MemoryStore.Get")
	if memoryGet == storeGet {
		t.Fatalf("interface and concrete Get share an id")
	}
	memoryStore := fixture.symbolID(t, "MemoryStore", "cache/cache.go")
	store := fixture.symbolID(t, "Store", "cache/cache.go")
	if !fixture.hasEdge(t, memoryStore, store, "implements") {
		t.Fatalf("MemoryStore does not implement Store")
	}
	codec := fixture.symbolID(t, "Codec", "cache/codec.go")
	jsonCodec := fixture.symbolID(t, "JSONCodec", "cache/codec.go")
	if !fixture.hasEdge(t, jsonCodec, codec, "implements") {
		t.Fatalf("JSONCodec does not implement Codec")
	}
	cacheType := fixture.symbolID(t, "Cache", "cache/embedded.go")
	if !fixture.hasEdge(t, cacheType, memoryStore, "embeds") {
		t.Fatalf("Cache does not embed MemoryStore")
	}
	if !fixture.hasEdge(t, cacheType, store, "implements") {
		t.Fatalf("promoted method did not satisfy Store")
	}
	cachePkg := fixture.symbolID(t, "cache", "cache/cache.go")
	servicePkg := fixture.symbolID(t, "service", "service/service.go")
	if !fixture.hasEdge(t, servicePkg, cachePkg, "imports") {
		t.Fatalf("service does not import cache")
	}
	newFunc := fixture.symbolID(t, "New", "service/service.go")
	defaultKey := fixture.symbolID(t, "DefaultKey", "cache/cache.go")
	if !fixture.hasEdge(t, newFunc, defaultKey, "references") {
		t.Fatalf("New does not reference DefaultKey")
	}
	testFetch := fixture.symbolID(t, "TestFetch", "service/service_test.go")
	if !fixture.hasEdge(t, testFetch, fetch, "calls") {
		t.Fatalf("TestFetch does not call Fetch")
	}
}

func TestSearchTiersAndAmbiguity(t *testing.T) {
	fixture := newFixture(t)
	fixture.mustIndex(t)
	ctx := context.Background()

	exact, err := fixture.index.Search(ctx, ports.GraphQuery{Text: "Fetch"})
	if err != nil {
		t.Fatal(err)
	}
	if exact[0].Match != ports.GraphMatchExact || exact[0].Confidence != 1.0 {
		t.Fatalf("exact = %+v", exact[0])
	}
	fts, err := fixture.index.Search(ctx, ports.GraphQuery{Text: "Memor"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fts) == 0 || fts[0].Match != ports.GraphMatchFTS || fts[0].Name != "MemoryStore" {
		t.Fatalf("fts = %+v", fts)
	}
	like, err := fixture.index.Search(ctx, ports.GraphQuery{Text: "morySto"})
	if err != nil {
		t.Fatal(err)
	}
	if len(like) == 0 || like[0].Match != ports.GraphMatchLike || like[0].Confidence != 0.5 {
		t.Fatalf("like = %+v", like)
	}
	onlyTypes, err := fixture.index.Search(ctx, ports.GraphQuery{Text: "Store", Kinds: []string{"type"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range onlyTypes {
		if hit.Kind != "type" {
			t.Fatalf("kind filter leaked %s", hit.Kind)
		}
	}
	if _, err := fixture.index.Node(ctx, "fixture", "Get", "", ports.NodeOpts{}); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("Get should be ambiguous, got %v", err)
	}
	if _, err := fixture.index.Search(ctx, ports.GraphQuery{Text: "  "}); err == nil {
		t.Fatal("empty search should fail")
	}
}

func TestNodeCallersCalleesAndSource(t *testing.T) {
	fixture := newFixture(t)
	fixture.mustIndex(t)
	ctx := context.Background()
	view, err := fixture.index.Node(ctx, "fixture", "Fetch", "", ports.NodeOpts{
		IncludeSource:  true,
		IncludeCallers: true,
		IncludeCallees: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.Symbol.Name != "Fetch" || view.Symbol.Kind != "func" {
		t.Fatalf("symbol = %+v", view.Symbol)
	}
	if len(view.Source) == 0 || !strings.Contains(view.Source[0].Text, "func Fetch") {
		t.Fatalf("source = %+v", view.Source)
	}
	if len(view.Callees) == 0 || view.Callees[0].Name != "Get" {
		t.Fatalf("callees = %+v", view.Callees)
	}
	if len(view.Callers) == 0 || view.Callers[0].Name != "TestFetch" {
		t.Fatalf("callers = %+v", view.Callers)
	}
	if !IsTestFile(view.Callers[0].File) {
		t.Fatalf("caller should be a test file: %+v", view.Callers[0])
	}
	// A body edit is disclosed instead of silently mixing two versions.
	path := filepath.Join(fixture.root, "service", "service.go")
	if err := os.WriteFile(path, []byte(strings.Replace(fixtureFiles["service/service.go"], "return value", "return value + \"\"", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := fixture.index.Node(ctx, "fixture", "Fetch", "", ports.NodeOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed.Notes) == 0 || !strings.Contains(strings.Join(changed.Notes, " "), "changed since the index was built") {
		t.Fatalf("stale source note missing: %+v", changed.Notes)
	}
}

func TestExploreAndImpact(t *testing.T) {
	fixture := newFixture(t)
	fixture.mustIndex(t)
	ctx := context.Background()
	view, err := fixture.index.Explore(ctx, "fixture", "Fetch", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Symbols) == 0 || view.Symbols[0].Name != "Fetch" {
		t.Fatalf("symbols = %+v", view.Symbols)
	}
	if len(view.Files) == 0 || view.Files[0].Path != "service/service.go" {
		t.Fatalf("files = %+v", view.Files)
	}
	if view.Bytes <= 0 || view.Bytes > ports.MaxGraphExploreBytes {
		t.Fatalf("bytes = %d", view.Bytes)
	}
	foundSource := false
	for _, file := range view.Files {
		for _, line := range file.Lines {
			if strings.Contains(line.Text, "func Fetch") {
				foundSource = true
			}
		}
	}
	if !foundSource {
		t.Fatalf("explore did not render the Fetch declaration")
	}
	if len(view.BlastRadius) == 0 {
		t.Fatalf("explore has no blast radius")
	}
	pinned, err := fixture.index.Explore(ctx, "fixture", "service/service.go:20-40 Fetch", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pinned.Symbols) == 0 {
		t.Fatalf("pinned explore returned no symbols")
	}

	impact, err := fixture.index.Impact(ctx, "fixture", "Fetch", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(impact.Hits) == 0 {
		t.Fatal("no impact hits")
	}
	foundTest := false
	for _, hit := range impact.Hits {
		if hit.Name == "TestFetch" {
			if !hit.Test {
				t.Fatalf("TestFetch hit is not marked as a test: %+v", hit)
			}
			foundTest = true
		}
	}
	if !foundTest {
		t.Fatalf("TestFetch is not in the blast radius: %+v", impact.Hits)
	}
	if len(impact.Tests) == 0 || impact.Tests[0] != "service/service_test.go" {
		t.Fatalf("tests = %+v", impact.Tests)
	}
	if impact.Depth != 2 {
		t.Fatalf("depth = %d", impact.Depth)
	}
	clamped, err := fixture.index.Impact(ctx, "fixture", "Fetch", "", 99)
	if err != nil {
		t.Fatal(err)
	}
	if clamped.Depth != ports.MaxGraphImpactDepth {
		t.Fatalf("clamped depth = %d", clamped.Depth)
	}
	if len(clamped.Notes) == 0 || !strings.Contains(strings.Join(clamped.Notes, " "), "clamped") {
		t.Fatalf("clamp note missing: %+v", clamped.Notes)
	}
}

func TestExploreBudgetClipsLargeSource(t *testing.T) {
	fixture := newFixture(t)
	fixture.mustIndex(t)
	ctx := context.Background()
	var body strings.Builder
	body.WriteString("package service\n\n// Big is a deliberately large declaration.\nfunc Big() string {\n")
	for index := 0; index < 2000; index++ {
		body.WriteString("\t_ = \"line ")
		body.WriteString(strings.Repeat("x", 24))
		body.WriteString("\"\n")
	}
	body.WriteString("\treturn \"big\"\n}\n")
	path := filepath.Join(fixture.root, "service", "big.go")
	if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.advance(time.Hour)
	fixture.mustSync(t)
	view, err := fixture.index.Explore(ctx, "fixture", "Big", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Symbols) == 0 || view.Symbols[0].Name != "Big" {
		t.Fatalf("symbols = %+v", view.Symbols)
	}
	if view.Bytes > ports.MaxGraphExploreBytes {
		t.Fatalf("bytes = %d over the limit", view.Bytes)
	}
	if !view.Truncated {
		t.Fatal("a 2000-line function was not clipped")
	}
	for _, file := range view.Files {
		if len(file.Lines) >= 2000 {
			t.Fatalf("file %s was not clipped", file.Path)
		}
	}
}

func TestIncrementalSync(t *testing.T) {
	fixture := newFixture(t)
	fixture.mustIndex(t)
	ctx := context.Background()
	var cacheIndexedAt int64
	if err := fixture.index.db.QueryRowContext(ctx,
		`SELECT indexed_at FROM files WHERE path = 'cache/cache.go'`).Scan(&cacheIndexedAt); err != nil {
		t.Fatal(err)
	}
	fixture.advance(time.Hour)

	// A body edit keeps every symbol id; only the edited package is parsed.
	path := filepath.Join(fixture.root, "service", "service.go")
	edited := strings.Replace(fixtureFiles["service/service.go"], `return ""`, `return "-"`, 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	result := fixture.mustSync(t)
	if result.Changed != 1 {
		t.Fatalf("changed = %d, want 1: %+v", result.Changed, result.Notes)
	}
	if strings.Contains(strings.Join(result.Notes, " "), "rebuilding to relink") {
		t.Fatalf("body edit escalated to a rebuild: %+v", result.Notes)
	}
	var newCacheIndexedAt int64
	if err := fixture.index.db.QueryRowContext(ctx,
		`SELECT indexed_at FROM files WHERE path = 'cache/cache.go'`).Scan(&newCacheIndexedAt); err != nil {
		t.Fatal(err)
	}
	if newCacheIndexedAt != cacheIndexedAt {
		t.Fatalf("unchanged package was re-extracted: %d -> %d", cacheIndexedAt, newCacheIndexedAt)
	}

	// A new symbol in an existing file is picked up incrementally.
	edited = strings.Replace(edited, "// BodyOnly", `// FetchAll returns two values.
func FetchAll(store cache.Store, keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, Fetch(store, key))
	}
	return out
}

// BodyOnly`, 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.advance(time.Hour)
	fixture.mustSync(t)
	hits, err := fixture.index.Search(ctx, ports.GraphQuery{Text: "FetchAll"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Match != ports.GraphMatchExact {
		t.Fatalf("FetchAll = %+v", hits)
	}
	fetch := fixture.symbolID(t, "Fetch", "service/service.go")
	fetchAll := fixture.symbolID(t, "FetchAll", "service/service.go")
	if !fixture.hasEdge(t, fetchAll, fetch, "calls") {
		t.Fatalf("FetchAll does not call Fetch")
	}

	// A new file in an existing package is added.
	extra := filepath.Join(fixture.root, "cache", "extra.go")
	if err := os.WriteFile(extra, []byte("package cache\n\n// Extra is new.\nfunc Extra() string { return \"extra\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.advance(time.Hour)
	fixture.mustSync(t)
	if hits, err := fixture.index.Search(ctx, ports.GraphQuery{Text: "Extra"}); err != nil || len(hits) == 0 {
		t.Fatalf("Extra = %+v, %v", hits, err)
	}

	// Deleting a file removes its symbols; the adapter rebuilds so incoming
	// edges from unchanged packages cannot point at removed ids.
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	fixture.advance(time.Hour)
	result = fixture.mustSync(t)
	if !strings.Contains(strings.Join(result.Notes, " "), "rebuilding to relink") {
		t.Fatalf("delete did not escalate to a rebuild: %+v", result.Notes)
	}
	if hits, err := fixture.index.Search(ctx, ports.GraphQuery{Text: "Extra"}); err != nil || len(hits) != 0 {
		t.Fatalf("Extra survived deletion: %+v, %v", hits, err)
	}
	if fixture.count(t, `SELECT count(*) FROM symbols WHERE name = 'Extra'`) != 0 {
		t.Fatal("deleted symbol row remains")
	}
}

func TestSchemaMismatchRebuilds(t *testing.T) {
	fixture := newFixture(t)
	fixture.mustIndex(t)
	fixture.index.mu.Lock()
	if _, err := fixture.index.db.Exec(`UPDATE meta SET value = '99' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	fixture.index.mu.Unlock()
	if err := fixture.index.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), Options{
		Root: fixture.root, DBPath: fixture.dbPath, Project: "fixture", Create: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if !reopened.needsRebuild {
		t.Fatal("a future schema was accepted")
	}
	status, err := reopened.Status(context.Background(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if status.IndexState != "stale-schema" {
		t.Fatalf("state = %q", status.IndexState)
	}
	if _, err := reopened.Search(context.Background(), ports.GraphQuery{Text: "Fetch"}); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("search on a stale schema = %v", err)
	}
	if _, err := reopened.Index(context.Background(), "fixture"); err != nil {
		t.Fatal(err)
	}
	var schema int
	if err := reopened.db.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'schema_version'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if schema != SchemaVersion {
		t.Fatalf("schema = %d, want %d", schema, SchemaVersion)
	}
	if hits, err := reopened.Search(context.Background(), ports.GraphQuery{Text: "Fetch"}); err != nil || len(hits) == 0 {
		t.Fatalf("Fetch after rebuild = %+v, %v", hits, err)
	}
}

func TestStoreKeepsNoSourceCopies(t *testing.T) {
	fixture := newFixture(t)
	fixture.mustIndex(t)
	if err := fixture.index.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(fixture.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytesContains(raw, []byte("BODY_MARKER_9f3a1")) {
		t.Fatal("the store contains a function body marker")
	}
	if bytesContains(raw, []byte("m.values[key] = value")) {
		t.Fatal("the store contains function body text")
	}
	// The declaration text that is allowed to be stored is present.
	if !bytesContains(raw, []byte("func Fetch")) {
		t.Fatal("the store lost the Fetch signature")
	}
	walPath := fixture.dbPath + "-wal"
	if wal, err := os.ReadFile(walPath); err == nil && bytesContains(wal, []byte("BODY_MARKER_9f3a1")) {
		t.Fatal("the WAL contains a function body marker")
	}
}

func TestIndexEmptyStoreIsReadableAsEmpty(t *testing.T) {
	fixture := newFixture(t)
	status, err := fixture.index.Status(context.Background(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if status.IndexState != stateEmpty || status.Files != 0 {
		t.Fatalf("status = %+v", status)
	}
	if _, err := fixture.index.Search(context.Background(), ports.GraphQuery{Text: "Fetch"}); !errors.Is(err, ErrIndexEmpty) {
		t.Fatalf("search on an empty store = %v", err)
	}
	if _, err := fixture.index.Impact(context.Background(), "fixture", "Fetch", "", 2); !errors.Is(err, ErrIndexEmpty) {
		t.Fatalf("impact on an empty store = %v", err)
	}
}

// TestDataIndexIsGitignored keeps the derived store out of version control.
func TestDataIndexIsGitignored(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", ".gitignore"))
	if err != nil {
		t.Skipf("repository .gitignore is unavailable: %v", err)
	}
	if !strings.Contains(string(raw), "/index/") {
		t.Fatal("the repository .gitignore must keep /index/ (and the graph store) out of git")
	}
	if !strings.HasPrefix(filepath.ToSlash(DefaultDBSubdir), "index/") {
		t.Fatalf("DefaultDBSubdir = %q, want it under index/", DefaultDBSubdir)
	}
}

// TestIndexMeasurement reports index time and on-disk size per 1k lines of
// Go source. It is a cheap smoke test, not a benchmark.
func TestIndexMeasurement(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement builds a synthetic module")
	}
	t.Setenv("GOWORK", "off")
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	data := filepath.Join(base, "brain")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/large\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines := 0
	const packages = 12
	for pkg := 0; pkg < packages; pkg++ {
		dir := filepath.Join(root, "pkg"+string(rune('a'+pkg)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		var body strings.Builder
		body.WriteString("package pkg" + string(rune('a'+pkg)) + "\n\n")
		for index := 0; index < 8; index++ {
			body.WriteString("// Fn" + string(rune('A'+index)) + " is generated.\n")
			body.WriteString("func Fn" + string(rune('A'+index)) + "(value int) int {\n")
			body.WriteString("\ttotal := value\n")
			body.WriteString("\tfor i := 0; i < 3; i++ {\n\t\ttotal += i\n\t}\n")
			body.WriteString("\treturn total\n}\n\n")
		}
		body.WriteString("func Use() int {\n\treturn FnA(1) + FnB(2)\n}\n")
		lines += strings.Count(body.String(), "\n")
		if err := os.WriteFile(filepath.Join(dir, "code.go"), []byte(body.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dbPath, err := DefaultDBPath(data, "large", root)
	if err != nil {
		t.Fatal(err)
	}
	index, err := Open(context.Background(), Options{Root: root, DBPath: dbPath, Project: "large", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	started := time.Now()
	result, err := index.Index(context.Background(), "large")
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	symbols := index.symbolCountForReport(t)
	edges := index.edgeCountForReport(t)
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	perThousand := float64(info.Size()) / float64(lines) * 1000
	t.Logf("measurement: %d files, %d LOC, %d symbols, %d edges, index %s, db %d bytes, %.0f bytes/1k LOC",
		packages, lines, symbols, edges, elapsed.Round(time.Millisecond), info.Size(), perThousand)
	if result.Changed != packages {
		t.Fatalf("changed = %d, want %d", result.Changed, packages)
	}
	if perThousand > 400_000 {
		t.Fatalf("store is %.0f bytes per 1k LOC, over the 400k guard", perThousand)
	}
}

// symbolCountForReport is a test-only helper that reads the symbol count.
func (i *Index) symbolCountForReport(t *testing.T) int {
	t.Helper()
	var count int
	if err := i.db.QueryRow(`SELECT count(*) FROM symbols`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// edgeCountForReport is a test-only helper that reads the edge count.
func (i *Index) edgeCountForReport(t *testing.T) int {
	t.Helper()
	var count int
	if err := i.db.QueryRow(`SELECT count(*) FROM edges`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// bytesContains reports whether haystack contains needle.
func bytesContains(haystack, needle []byte) bool {
	return strings.Contains(string(haystack), string(needle))
}

func BenchmarkSearch(b *testing.B) {
	b.ReportAllocs()
	dir := b.TempDir()
	b.Setenv("GOWORK", "off")
	writeFixtureBench(b, dir)
	dbPath, err := DefaultDBPath(b.TempDir(), "fixture", dir)
	if err != nil {
		b.Fatal(err)
	}
	index, err := Open(context.Background(), Options{Root: dir, DBPath: dbPath, Project: "fixture", Create: true})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	if _, err := index.Index(context.Background(), "fixture"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := index.Search(context.Background(), ports.GraphQuery{Text: "Fetch"}); err != nil {
			b.Fatal(err)
		}
	}
}

// writeFixtureBench writes the fixture without a *testing.T.
func writeFixtureBench(b *testing.B, root string) {
	b.Helper()
	for path, content := range fixtureFiles {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			b.Fatal(err)
		}
	}
}
