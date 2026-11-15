package codegraph

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// fixture builds a small SQLite index with the CodeGraph schema subset this
// adapter reads, plus the checkout the source is read from. It is a test-only
// double: it keeps the column names and the FTS external-content wiring, not
// every table of a real index.
type fixture struct {
	t           *testing.T
	root        string
	files       map[string][]string
	nodes       []ports.GraphSymbol
	edges       []fixtureEdge
	schema      int
	extraction  int
	indexedWith string
	indexState  string
	dirtyPaths  string
	indexedMS   int64
	extraMeta   map[string]string
}

type fixtureEdge struct {
	from       string
	to         string
	kind       string
	line       int
	provenance string
	confidence float64
	resolvedBy string
}

// fixtureIndexAgeMillis is how old the fixture index is when a test reads it.
const fixtureIndexAgeMillis = int64(3600 * 1000)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t:           t,
		root:        t.TempDir(),
		files:       map[string][]string{},
		schema:      SupportedSchemaVersion,
		extraction:  SupportedExtractionVersion,
		indexedWith: VerifiedIndexedWithVersion,
		indexState:  "complete",
		extraMeta:   map[string]string{},
	}
	f.buildDefaultRepo()
	return f
}

// buildDefaultRepo writes the source files and the node/edge rows every test
// can rely on:
//
//	EventCache <- hashKey <- ListKeys <- Handler <- handler_test.go
//	EventCache <- cache_test.go
func (f *fixture) buildDefaultRepo() {
	f.addFile("internal/cache/cache.go", `package cache

// EventCache stores an event under key and returns the key.
func EventCache(key string) string {
	return key
}

func hashKey(key string) string {
	return EventCache(key)
}`)
	f.addNode(ports.GraphSymbol{
		ID: "function:event-cache", Kind: "function", Name: "EventCache", QualName: "EventCache",
		File: "internal/cache/cache.go", Language: "go", StartLine: 4, EndLine: 6,
		Signature: "(key string) string", Docstring: "EventCache stores an event under key and returns the key.",
		Visibility: "public", Exported: true,
	})
	f.addNode(ports.GraphSymbol{
		ID: "function:hash-key", Kind: "function", Name: "hashKey", QualName: "hashKey",
		File: "internal/cache/cache.go", Language: "go", StartLine: 8, EndLine: 10,
		Signature: "(key string) string",
	})

	f.addFile("internal/cache/keys.go", `package cache

func ListKeys() []string {
	return []string{hashKey("a")}
}`)
	f.addNode(ports.GraphSymbol{
		ID: "function:list-keys", Kind: "function", Name: "ListKeys", QualName: "ListKeys",
		File: "internal/cache/keys.go", Language: "go", StartLine: 3, EndLine: 5,
		Signature: "() []string", Exported: true,
	})

	f.addFile("internal/api/handler.go", `package api

import "example.com/cache"

func Handler() string {
	return cache.ListKeys()[0]
}`)
	f.addNode(ports.GraphSymbol{
		ID: "function:handler", Kind: "function", Name: "Handler", QualName: "api.Handler",
		File: "internal/api/handler.go", Language: "go", StartLine: 5, EndLine: 7,
		Signature: "() string", Exported: true,
	})

	f.addFile("internal/cache/cache_test.go", `package cache

import "testing"

func TestEventCache(t *testing.T) {
	if EventCache("k") != "k" {
		t.Fatal("bad")
	}
}`)
	f.addNode(ports.GraphSymbol{
		ID: "file:internal/cache/cache_test.go", Kind: "file", Name: "cache_test.go",
		QualName: "internal/cache/cache_test.go", File: "internal/cache/cache_test.go",
		Language: "go", StartLine: 1, EndLine: 9,
	})
	f.addFile("internal/api/handler_test.go", `package api

import "testing"

func TestHandler(t *testing.T) {
	if Handler() != "ok" {
		t.Fatal("bad")
	}
}`)
	f.addNode(ports.GraphSymbol{
		ID: "file:internal/api/handler_test.go", Kind: "file", Name: "handler_test.go",
		QualName: "internal/api/handler_test.go", File: "internal/api/handler_test.go",
		Language: "go", StartLine: 1, EndLine: 8,
	})

	f.addEdge("function:hash-key", "function:event-cache", "calls", 9, "", 0.9, "exact-match")
	f.addEdge("function:list-keys", "function:hash-key", "calls", 4, "", 0.9, "exact-match")
	f.addEdge("function:handler", "function:list-keys", "calls", 6, "", 0.9, "import")
	f.addEdge("file:internal/cache/cache_test.go", "function:event-cache", "calls", 6, "", 0.9, "import")
	f.addEdge("file:internal/api/handler_test.go", "function:handler", "calls", 6, "", 0.9, "import")
}

// addFile writes one checkout file and records its lines.
func (f *fixture) addFile(path, content string) {
	f.t.Helper()
	content = strings.TrimSuffix(content, "\n")
	f.files[path] = strings.Split(content, "\n")
}

// addNode records one indexed definition.
func (f *fixture) addNode(symbol ports.GraphSymbol) {
	if symbol.Language == "" {
		symbol.Language = "go"
	}
	f.nodes = append(f.nodes, symbol)
}

func (f *fixture) addEdge(from, to, kind string, line int, provenance string, confidence float64, resolvedBy string) {
	f.edges = append(f.edges, fixtureEdge{
		from: from, to: to, kind: kind, line: line,
		provenance: provenance, confidence: confidence, resolvedBy: resolvedBy,
	})
}

// addRadiusFiles adds count files, each with one function whose body has
// bodyLines statements. It gives explore more source than one answer may hold.
func (f *fixture) addRadiusFiles(count, bodyLines int) {
	f.t.Helper()
	for fileIndex := 0; fileIndex < count; fileIndex++ {
		lines := []string{
			"package radius",
			"",
			fmt.Sprintf("// Radius%d fans a value out across the pipeline.", fileIndex),
			fmt.Sprintf("func Radius%d(input string) string {", fileIndex),
		}
		for line := 0; line < bodyLines; line++ {
			lines = append(lines, fmt.Sprintf("\tresult%d := input + %q", line, strings.Repeat("x", 18)))
		}
		lines = append(lines, "\treturn input", "}")
		path := fmt.Sprintf("internal/radius/radius%d.go", fileIndex)
		f.addFile(path, strings.Join(lines, "\n"))
		f.addNode(ports.GraphSymbol{
			ID: fmt.Sprintf("function:radius-%d", fileIndex), Kind: "function",
			Name: fmt.Sprintf("Radius%d", fileIndex), QualName: fmt.Sprintf("Radius%d", fileIndex),
			File: path, Language: "go", StartLine: 4, EndLine: len(lines),
			Signature: "(input string) string", Exported: true,
		})
	}
}

// build writes the checkout files and the SQLite index, returning both paths.
func (f *fixture) build() (string, string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Join(f.root, ".codegraph"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	for path, lines := range f.files {
		full := filepath.Join(f.root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	dbPath := filepath.Join(f.root, filepath.FromSlash(DefaultDBRelPath))
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			f.t.Fatal(err)
		}
	}()
	if _, err := db.Exec(fixtureSchema); err != nil {
		f.t.Fatalf("create fixture schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_versions(version, applied_at, description) VALUES (1, 0, 'Initial schema')`); err != nil {
		f.t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_versions(version, applied_at, description) VALUES (?, 0, 'fixture')`, f.schema); err != nil {
		f.t.Fatal(err)
	}
	f.indexedMS = nowMillis() - fixtureIndexAgeMillis
	metadata := map[string]string{
		"indexed_with_version":            f.indexedWith,
		"indexed_with_extraction_version": fmt.Sprintf("%d", f.extraction),
		"index_state":                     f.indexState,
		"index_files_discovered":          fmt.Sprintf("%d", len(f.files)),
		"indexed_at_commit":               "0123456789abcdef0123456789abcdef01234567",
	}
	if f.dirtyPaths != "" {
		metadata["indexed_dirty_paths"] = f.dirtyPaths
	}
	for key, value := range f.extraMeta {
		metadata[key] = value
	}
	for key, value := range metadata {
		if _, err := db.Exec(`INSERT INTO project_metadata(key, value, updated_at) VALUES (?, ?, 0)`, key, value); err != nil {
			f.t.Fatal(err)
		}
	}
	for path := range f.files {
		info, err := os.Stat(filepath.Join(f.root, filepath.FromSlash(path)))
		if err != nil {
			f.t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO files(path, content_hash, language, size, modified_at, indexed_at, node_count, errors, generated)
			VALUES (?, '', 'go', ?, ?, ?, 0, NULL, 0)`, path, info.Size(), info.ModTime().UnixMilli(), f.indexedMS); err != nil {
			f.t.Fatal(err)
		}
	}
	for _, node := range f.nodes {
		exported := 0
		if node.Exported {
			exported = 1
		}
		if _, err := db.Exec(`INSERT INTO nodes(id, kind, name, qualified_name, file_path, language,
			start_line, end_line, start_column, end_column, docstring, signature, visibility, is_exported, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?, ?, ?, 0)`,
			node.ID, node.Kind, node.Name, node.QualName, node.File, node.Language,
			node.StartLine, node.EndLine, nullable(node.Docstring), nullable(node.Signature), nullable(node.Visibility), exported); err != nil {
			f.t.Fatal(err)
		}
	}
	for _, edge := range f.edges {
		metadata := ""
		if edge.confidence > 0 || edge.resolvedBy != "" {
			raw, err := json.Marshal(map[string]any{"confidence": edge.confidence, "resolvedBy": edge.resolvedBy})
			if err != nil {
				f.t.Fatal(err)
			}
			metadata = string(raw)
		}
		if _, err := db.Exec(`INSERT INTO edges(source, target, kind, metadata, line, provenance)
			VALUES (?, ?, ?, ?, ?, ?)`,
			edge.from, edge.to, edge.kind, nullable(metadata), edge.line, nullable(edge.provenance)); err != nil {
			f.t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO unresolved_refs(from_node_id, reference_name, reference_kind, line, col, status)
		VALUES ('function:handler', 'missing', 'call', 99, 0, 'failed')`); err != nil {
		f.t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO nodes_fts(nodes_fts) VALUES('rebuild')`); err != nil {
		f.t.Fatalf("rebuild fts: %v", err)
	}
	return f.root, dbPath
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nowMillis() int64 {
	return time.Now().UnixMilli()
}

// fixtureSchema is the CodeGraph schema subset the adapter reads, with the
// same column names and the external-content FTS mirror.
const fixtureSchema = `
CREATE TABLE nodes (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, name TEXT NOT NULL,
  qualified_name TEXT NOT NULL, file_path TEXT NOT NULL, language TEXT NOT NULL,
  start_line INTEGER NOT NULL, end_line INTEGER NOT NULL, start_column INTEGER NOT NULL,
  end_column INTEGER NOT NULL, docstring TEXT, signature TEXT, visibility TEXT,
  is_exported INTEGER DEFAULT 0, is_async INTEGER DEFAULT 0, is_static INTEGER DEFAULT 0,
  is_abstract INTEGER DEFAULT 0, decorators TEXT, type_parameters TEXT,
  return_type TEXT, updated_at INTEGER NOT NULL);
CREATE TABLE edges (
  id INTEGER PRIMARY KEY AUTOINCREMENT, source TEXT NOT NULL, target TEXT NOT NULL,
  kind TEXT NOT NULL, metadata TEXT, line INTEGER, col INTEGER, provenance TEXT);
CREATE TABLE files (
  path TEXT PRIMARY KEY, content_hash TEXT NOT NULL, language TEXT NOT NULL,
  size INTEGER NOT NULL, modified_at INTEGER NOT NULL, indexed_at INTEGER NOT NULL,
  node_count INTEGER DEFAULT 0, errors TEXT, generated INTEGER NOT NULL DEFAULT 0);
CREATE TABLE unresolved_refs (
  id INTEGER PRIMARY KEY AUTOINCREMENT, from_node_id TEXT NOT NULL, reference_name TEXT NOT NULL,
  reference_kind TEXT NOT NULL, line INTEGER NOT NULL, col INTEGER NOT NULL, candidates TEXT,
  file_path TEXT NOT NULL DEFAULT '', language TEXT NOT NULL DEFAULT 'unknown',
  status TEXT NOT NULL DEFAULT 'pending', name_tail TEXT NOT NULL DEFAULT '');
CREATE TABLE project_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE schema_versions (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL, description TEXT);
CREATE VIRTUAL TABLE nodes_fts USING fts5(id, name, qualified_name, docstring, signature,
  content='nodes', content_rowid='rowid');
CREATE INDEX idx_nodes_name ON nodes(name);
CREATE INDEX idx_nodes_lower_name ON nodes(lower(name));
CREATE INDEX idx_nodes_qualified_name ON nodes(qualified_name);
CREATE INDEX idx_nodes_file_line ON nodes(file_path, start_line);
CREATE INDEX idx_edges_target_kind ON edges(target, kind);
CREATE INDEX idx_edges_source_kind ON edges(source, kind);
`
