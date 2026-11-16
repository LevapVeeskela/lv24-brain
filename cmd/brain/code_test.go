package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"gopkg.in/yaml.v3"
)

// setupCodeBrain creates a local brain with project demo registered on a
// temporary checkout that holds a two-symbol CodeGraph index.
func setupCodeBrain(t *testing.T) (cfgPath, root string) {
	t.Helper()
	t.Setenv("BRAIN_CONFIG", "")
	base := t.TempDir()
	dataDir := filepath.Join(base, "brain")
	root = filepath.Join(base, "repo")
	cfgPath = filepath.Join(dataDir, "brain.yaml")

	steps := [][]string{
		{"init", "--mode", "local", "--data-dir", dataDir},
		{"project", "set", "demo", "--name", "Demo", "--add-path", root, "--config", cfgPath},
	}
	for _, args := range steps {
		if code, _, stderr := runForTest(t, args...); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, stderr)
		}
	}
	writeCodeCheckout(t, root)
	writeCodeIndex(t, root)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Graph = config.GraphConfig{Engine: ports.GraphEngineCodeGraph}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	return cfgPath, root
}

func writeCodeCheckout(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"internal/cache/cache.go": `package cache

// EventCache caches one event.
func EventCache(key string) string {
	return key
}
`,
		"internal/cache/cache_test.go": `package cache

import "testing"

func TestEventCache(t *testing.T) {
	_ = EventCache("k")
}
`,
	}
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// writeCodeIndex writes the CodeGraph schema subset the adapter reads.
func writeCodeIndex(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".codegraph"), 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, ".codegraph", "codegraph.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if _, err := db.Exec(codeFixtureSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	now := time.Now().UnixMilli()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO schema_versions(version, applied_at, description) VALUES (1, 0, 'Initial schema')`, nil},
		{`INSERT INTO schema_versions(version, applied_at, description) VALUES (11, 0, 'fixture')`, nil},
		{`INSERT INTO project_metadata(key, value, updated_at) VALUES ('indexed_with_version', '1.6.2', 0)`, nil},
		{`INSERT INTO project_metadata(key, value, updated_at) VALUES ('indexed_with_extraction_version', '27', 0)`, nil},
		{`INSERT INTO project_metadata(key, value, updated_at) VALUES ('index_state', 'complete', 0)`, nil},
		{`INSERT INTO project_metadata(key, value, updated_at) VALUES ('indexed_at_commit', '0123456789abcdef0123456789abcdef01234567', 0)`, nil},
		{`INSERT INTO files(path, content_hash, language, size, modified_at, indexed_at) VALUES (?, '', 'go', 90, ?, ?)`,
			[]any{"internal/cache/cache.go", now - 3600_000, now - 3600_000}},
		{`INSERT INTO files(path, content_hash, language, size, modified_at, indexed_at) VALUES (?, '', 'go', 110, ?, ?)`,
			[]any{"internal/cache/cache_test.go", now - 3600_000, now - 3600_000}},
		{`INSERT INTO nodes(id, kind, name, qualified_name, file_path, language, start_line, end_line,
			start_column, end_column, docstring, signature, visibility, is_exported, updated_at)
			VALUES ('function:event-cache', 'function', 'EventCache', 'EventCache', 'internal/cache/cache.go',
			'go', 4, 6, 0, 0, 'EventCache caches one event.', '(key string) string', 'public', 1, 0)`, nil},
		{`INSERT INTO nodes(id, kind, name, qualified_name, file_path, language, start_line, end_line,
			start_column, end_column, docstring, signature, visibility, is_exported, updated_at)
			VALUES ('file:internal/cache/cache_test.go', 'file', 'cache_test.go',
			'internal/cache/cache_test.go', 'internal/cache/cache_test.go', 'go', 1, 7, 0, 0, NULL, NULL, NULL, 0, 0)`, nil},
		{`INSERT INTO edges(source, target, kind, metadata, line, provenance)
			VALUES ('file:internal/cache/cache_test.go', 'function:event-cache', 'calls',
			'{"confidence":0.9,"resolvedBy":"import"}', 6, NULL)`, nil},
		{`INSERT INTO nodes_fts(nodes_fts) VALUES('rebuild')`, nil},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("fixture statement %q: %v", statement.query, err)
		}
	}
}

const codeFixtureSchema = `
CREATE TABLE nodes (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, name TEXT NOT NULL, qualified_name TEXT NOT NULL,
  file_path TEXT NOT NULL, language TEXT NOT NULL, start_line INTEGER NOT NULL, end_line INTEGER NOT NULL,
  start_column INTEGER NOT NULL, end_column INTEGER NOT NULL, docstring TEXT, signature TEXT,
  visibility TEXT, is_exported INTEGER DEFAULT 0, is_async INTEGER DEFAULT 0, is_static INTEGER DEFAULT 0,
  is_abstract INTEGER DEFAULT 0, decorators TEXT, type_parameters TEXT, return_type TEXT,
  updated_at INTEGER NOT NULL);
CREATE TABLE edges (
  id INTEGER PRIMARY KEY AUTOINCREMENT, source TEXT NOT NULL, target TEXT NOT NULL, kind TEXT NOT NULL,
  metadata TEXT, line INTEGER, col INTEGER, provenance TEXT);
CREATE TABLE files (
  path TEXT PRIMARY KEY, content_hash TEXT NOT NULL, language TEXT NOT NULL, size INTEGER NOT NULL,
  modified_at INTEGER NOT NULL, indexed_at INTEGER NOT NULL, node_count INTEGER DEFAULT 0,
  errors TEXT, generated INTEGER NOT NULL DEFAULT 0);
CREATE TABLE unresolved_refs (
  id INTEGER PRIMARY KEY AUTOINCREMENT, from_node_id TEXT NOT NULL, reference_name TEXT NOT NULL,
  reference_kind TEXT NOT NULL, line INTEGER NOT NULL, col INTEGER NOT NULL, candidates TEXT,
  file_path TEXT NOT NULL DEFAULT '', language TEXT NOT NULL DEFAULT 'unknown',
  status TEXT NOT NULL DEFAULT 'pending', name_tail TEXT NOT NULL DEFAULT '');
CREATE TABLE project_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE schema_versions (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL, description TEXT);
CREATE VIRTUAL TABLE nodes_fts USING fts5(id, name, qualified_name, docstring, signature,
  content='nodes', content_rowid='rowid');
`

// decodeCodeJSON unwraps the shared {schema, data} envelope.
func decodeCodeJSON[T any](t *testing.T, stdout string) T {
	t.Helper()
	var envelope struct {
		Schema int             `json:"schema"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode envelope: %v in %q", err, stdout)
	}
	if envelope.Schema != jsonSchemaVersion {
		t.Fatalf("schema = %d", envelope.Schema)
	}
	var value T
	if err := json.Unmarshal(envelope.Data, &value); err != nil {
		t.Fatalf("decode data: %v in %q", err, envelope.Data)
	}
	return value
}

func TestCodeStatusAndSearchJSON(t *testing.T) {
	cfgPath, _ := setupCodeBrain(t)

	// Act
	code, stdout, stderr := runForTest(t, "code", "status", "--project", "demo", "--config", cfgPath, "--json")

	// Assert
	if code != 0 {
		t.Fatalf("status: exit %d: %s", code, stderr)
	}
	status := decodeCodeJSON[ports.GraphStatus](t, stdout)
	if status.Project != "demo" || status.Engine != ports.GraphEngineCodeGraph {
		t.Fatalf("status = %+v", status)
	}
	if status.SchemaVersion != 11 || status.Nodes != 2 || status.Files != 2 || status.Edges != 1 {
		t.Fatalf("counts = %+v", status)
	}

	// Act
	code, stdout, stderr = runForTest(t, "code", "search", "EventCache", "--project", "demo", "--config", cfgPath, "--json")

	// Assert
	if code != 0 {
		t.Fatalf("search: exit %d: %s", code, stderr)
	}
	hits := decodeCodeJSON[[]ports.SymbolHit](t, stdout)
	if len(hits) != 1 || hits[0].Name != "EventCache" || hits[0].Match != ports.GraphMatchExact {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].File != "internal/cache/cache.go" || hits[0].StartLine != 4 || hits[0].Signature != "(key string) string" {
		t.Fatalf("hit = %+v", hits[0])
	}

	// Act: text output stays human-readable.
	code, stdout, stderr = runForTest(t, "code", "status", "--project", "demo", "--config", cfgPath)

	// Assert
	if code != 0 {
		t.Fatalf("status text: exit %d: %s", code, stderr)
	}
	for _, want := range []string{"project:", "codegraph", "files:", "nodes:", "read-only"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("status output misses %q:\n%s", want, stdout)
		}
	}
}

func TestCodeNodeExploreImpactText(t *testing.T) {
	cfgPath, _ := setupCodeBrain(t)
	common := []string{"--project", "demo", "--config", cfgPath}

	// Act
	code, stdout, stderr := runForTest(t, append([]string{"code", "node", "EventCache", "--code"}, common...)...)

	// Assert
	if code != 0 {
		t.Fatalf("node: exit %d: %s", code, stderr)
	}
	for _, want := range []string{"function EventCache", "internal/cache/cache.go:4-6", "func EventCache", "callers (1)", "calls", "cache_test.go"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("node output misses %q:\n%s", want, stdout)
		}
	}

	// Act
	code, stdout, stderr = runForTest(t, append([]string{"code", "explore", "EventCache"}, common...)...)

	// Assert
	if code != 0 {
		t.Fatalf("explore: exit %d: %s", code, stderr)
	}
	for _, want := range []string{"query: EventCache", "symbols (", "internal/cache/cache.go", "bytes:"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("explore output misses %q:\n%s", want, stdout)
		}
	}

	// Act
	code, stdout, stderr = runForTest(t, append([]string{"code", "impact", "EventCache", "--depth", "2"}, common...)...)

	// Assert
	if code != 0 {
		t.Fatalf("impact: exit %d: %s", code, stderr)
	}
	for _, want := range []string{"root:", "depth: 2", "hits (", "cache_test.go"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("impact output misses %q:\n%s", want, stdout)
		}
	}
}

func TestCodeRefusesOffAndMissingIndex(t *testing.T) {
	cfgPath, root := setupCodeBrain(t)
	common := []string{"--project", "demo", "--config", cfgPath}

	// Act: engine off.
	code, _, stderr := runForTest(t, append([]string{"code", "status", "--engine", "off"}, common...)...)

	// Assert
	if code != 1 || !strings.Contains(stderr, "code graph is off") {
		t.Fatalf("off: exit %d: %s", code, stderr)
	}

	// Act: the native engine has no store yet.
	code, _, stderr = runForTest(t, append([]string{"code", "status", "--engine", "native"}, common...)...)

	// Assert
	if code != 1 || !strings.Contains(stderr, "no native graph index") {
		t.Fatalf("native: exit %d: %s", code, stderr)
	}

	// Act: unknown engine.
	code, _, stderr = runForTest(t, append([]string{"code", "status", "--engine", "wat"}, common...)...)
	if code != 1 || !strings.Contains(stderr, "unknown graph engine") {
		t.Fatalf("unknown engine: exit %d: %s", code, stderr)
	}

	// Act: no index on disk.
	if err := os.Remove(filepath.Join(root, ".codegraph", "codegraph.db")); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runForTest(t, append([]string{"code", "status"}, common...)...)

	// Assert
	if code != 1 || !strings.Contains(stderr, "no codegraph index at") {
		t.Fatalf("missing index: exit %d: %s", code, stderr)
	}
}

func TestCodeRefusesForeignSchema(t *testing.T) {
	cfgPath, root := setupCodeBrain(t)
	dbPath := filepath.Join(root, ".codegraph", "codegraph.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_versions(version, applied_at, description) VALUES (12, 0, 'future')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runForTest(t, "code", "status", "--project", "demo", "--config", cfgPath)

	if code != 1 || !strings.Contains(stderr, "refusing index") || !strings.Contains(stderr, "schema 12") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

func TestCodeRejectsInvalidGraphConfig(t *testing.T) {
	cfgPath, _ := setupCodeBrain(t)
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["graph"] = map[string]any{"engine": "future"}
	encoded, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, encoded, 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runForTest(t, "code", "status", "--project", "demo", "--config", cfgPath)

	if code != 1 || !strings.Contains(stderr, "unknown engine") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}

func TestCodeUsage(t *testing.T) {
	if code, _, stderr := runForTest(t, "code"); code != 2 || !strings.Contains(stderr, "brain code status") {
		t.Fatalf("no subcommand: exit %d: %s", code, stderr)
	}
	if code, stdout, _ := runForTest(t, "code", "help"); code != 0 || !strings.Contains(stdout, "brain code explore") {
		t.Fatalf("help: exit %d: %s", code, stdout)
	}
	if code, _, stderr := runForTest(t, "code", "bogus"); code != 2 || !strings.Contains(stderr, "unknown subcommand") {
		t.Fatalf("unknown: exit %d: %s", code, stderr)
	}
	cfgPath, _ := setupCodeBrain(t)
	if code, _, stderr := runForTest(t, "code", "search", "--project", "demo", "--config", cfgPath); code != 2 || !strings.Contains(stderr, "query is required") {
		t.Fatalf("empty search: exit %d: %s", code, stderr)
	}
	if code, _, stderr := runForTest(t, "code", "impact", "EventCache", "--depth", "42", "--project", "demo", "--config", cfgPath); code != 2 || !strings.Contains(stderr, "--depth") {
		t.Fatalf("bad depth: exit %d: %s", code, stderr)
	}
}

func TestCodeJSONExploreKeepsTheByteCeiling(t *testing.T) {
	cfgPath, _ := setupCodeBrain(t)

	code, stdout, stderr := runForTest(t, "code", "explore", "EventCache", "--project", "demo", "--config", cfgPath, "--json")

	if code != 0 {
		t.Fatalf("explore: exit %d: %s", code, stderr)
	}
	view := decodeCodeJSON[ports.ExploreView](t, stdout)
	if view.Bytes > ports.MaxGraphExploreBytes {
		t.Fatalf("bytes = %d", view.Bytes)
	}
	if view.Query != "EventCache" || len(view.Symbols) == 0 {
		t.Fatalf("view = %+v", view)
	}
}
