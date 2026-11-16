package codeindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// scriptFixtures is a checkout with no Go module: the indexer must still build
// a useful graph from TypeScript, TSX, JavaScript and JSX.
var scriptFixtures = map[string]string{
	"src/models.ts": `/** A stored entity. */
export interface Entity {
  id: string;
  name: string;
}

export type ID = string | number;

export enum Kind {
  User = "user",
  Team = "team",
}

/** Base class. */
export class Base {
  protected createdAt = 0;

  touch(): void {
    this.createdAt = Date.now();
  }
}

/** A user entity. */
export class User extends Base implements Entity {
  id: string;
  name: string;

  constructor(id: string, name: string) {
    super();
    this.id = id;
    this.name = name;
  }

  rename(next: string): void {
    this.name = next;
    this.touch();
  }

  static create(id: string, name: string): User {
    return new User(id, name);
  }
}
`,
	"src/util.ts": `/** Collapse whitespace. */
export function normalizeName(value: string): string {
  return value.trim().replace(/\s+/g, " ");
}

export const MAX_NAME = 64;

/** Default formatter. */
export default class Formatter {
  format(value: string): string {
    return normalizeName(value);
  }
}
`,
	"src/store.ts": `import { User, type Entity } from "./models";
import { MAX_NAME, normalizeName } from "./util";

export interface StoreLike<T> {
  add(item: T): void;
  find(id: string): T | null;
}

export class Store<T extends Entity> implements StoreLike<T> {
  private items: T[] = [];

  add(item: T): void {
    this.items.push(item);
  }

  find(id: string): T | null {
    return this.items.find((item) => item.id === id) ?? null;
  }
}

export function makeUser(id: string, name: string): User {
  return new User(id, name.trim().slice(0, MAX_NAME));
}

export function label(user: User): string {
  const marker = "BODY_MARKER_7c21";
  return normalizeName(user.name) + marker.slice(0, 0);
}
`,
	"src/app.ts": `import Formatter from "./util";
import * as store from "./store";
import { type User } from "./models";

/** Application entry point. */
export class App {
  private formatter = new Formatter();

  run(user: User): string {
    const named = store.label(user);
    return this.formatter.format(named);
  }
}
`,
	"src/view.tsx": `import { App } from "./app";
import type { User } from "./models";

export interface Props {
  user: User;
}

/** Render one user. */
export function View({ user }: Props): JSX.Element {
  const app = new App();
  return <div className="row">{app.run(user)}</div>;
}
`,
	"src/legacy.jsx": `import { Base } from "./models";

export class Legacy extends Base {
  render() {
    return <span>{this.props.label}</span>;
  }
}
`,
	"src/broken.ts": `export function good(): number {
  return 1;
}

export function broken(: number {
  return 2;
}

export const after = 3;
`,
	"src/missing.ts": `import { gone } from "./nowhere";

export function useMissing(): void {
  gone();
}
`,
	"src/types.d.ts": `export interface Generated {
  value: string;
}
`,
	"src/bundle.min.js": `export function minified(){return 1}
`,
	"dist/generated.ts": `export function generated(): void {}
`,
}

// scriptFixture is a script-only checkout with an open index.
type scriptFixture struct {
	index  *Index
	root   string
	dbPath string
	now    time.Time
}

// newScriptFixture writes the fixture checkout and opens an empty store.
func newScriptFixture(t *testing.T) *scriptFixture {
	t.Helper()
	return newCodeFixture(t, scriptFixtures, "scripts")
}

// newCodeFixture writes a path-to-content fixture checkout and opens an empty
// store. Language fixtures share it so the helper behavior stays identical.
func newCodeFixture(t *testing.T, files map[string]string, project string) *scriptFixture {
	t.Helper()
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeScriptFiles(t, root, files)
	now := time.Unix(1_700_000_000, 0).UTC()
	dbPath, err := DefaultDBPath(filepath.Join(base, "brain"), project, root)
	if err != nil {
		t.Fatal(err)
	}
	index, err := Open(context.Background(), Options{
		Root:    root,
		DBPath:  dbPath,
		Project: project,
		Create:  true,
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return &scriptFixture{index: index, root: root, dbPath: dbPath, now: now}
}

// writeScriptFiles writes a path-to-content map below root.
func writeScriptFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// writeScriptFile replaces one fixture file.
func (f *scriptFixture) writeScriptFile(t *testing.T, name, content string) {
	t.Helper()
	writeScriptFiles(t, f.root, map[string]string{name: content})
}

// advance moves the fixture clock.
func (f *scriptFixture) advance(delta time.Duration) {
	f.now = f.now.Add(delta)
	f.index.now = func() time.Time { return f.now }
}

// mustIndex runs a full index.
func (f *scriptFixture) mustIndex(t *testing.T) ports.GraphSyncResult {
	t.Helper()
	result, err := f.index.Index(context.Background(), "scripts")
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	return result
}

// mustSync runs an incremental sync.
func (f *scriptFixture) mustSync(t *testing.T, paths ...string) ports.GraphSyncResult {
	t.Helper()
	result, err := f.index.Sync(context.Background(), "scripts", paths)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return result
}

// count runs one scalar count.
func (f *scriptFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var value int
	if err := f.index.db.QueryRowContext(context.Background(), query, args...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

// symbolNames lists a file's definitions without the module row.
func (f *scriptFixture) symbolNames(t *testing.T, path string) []string {
	t.Helper()
	rows, err := f.index.db.QueryContext(context.Background(),
		`SELECT name FROM symbols WHERE path = ? AND kind <> 'module' ORDER BY start_line, kind, name`, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// symbolIDByQual resolves one qualified name.
func (f *scriptFixture) symbolIDByQual(t *testing.T, qualname string) string {
	t.Helper()
	var id string
	if err := f.index.db.QueryRowContext(context.Background(),
		`SELECT id FROM symbols WHERE qualname = ?`, qualname).Scan(&id); err != nil {
		t.Fatalf("symbol %s: %v", qualname, err)
	}
	return id
}

// hasEdge reports one edge and returns its confidence.
func (f *scriptFixture) hasEdge(t *testing.T, source, target, kind string) (float64, bool) {
	t.Helper()
	var confidence float64
	err := f.index.db.QueryRowContext(context.Background(),
		`SELECT MAX(confidence) FROM edges WHERE source = ? AND target = ? AND kind = ?`,
		source, target, kind).Scan(&confidence)
	if err != nil {
		return 0, false
	}
	return confidence, true
}

// edgeCount counts edges of one kind.
func (f *scriptFixture) edgeCount(t *testing.T, kind string) int {
	t.Helper()
	return f.count(t, `SELECT count(*) FROM edges WHERE kind = ?`, kind)
}

// TestScriptIndexExtractsSymbolsAndEdges is the golden shape of a mixed script
// checkout: exact declaration sets, import edges, calls, inheritance and
// type-reference confidence.
func TestScriptIndexExtractsSymbolsAndEdges(t *testing.T) {
	fixture := newScriptFixture(t)
	result := fixture.mustIndex(t)
	if !strings.Contains(strings.Join(result.Notes, " "), "TypeScript/JavaScript") {
		t.Fatalf("index notes do not disclose script extraction: %+v", result.Notes)
	}
	status, err := fixture.index.Status(context.Background(), "scripts")
	if err != nil {
		t.Fatal(err)
	}
	if status.Files != 8 {
		t.Fatalf("files = %d, want 8 (three declaration/generated files are skipped): %+v", status.Files, status.FilesByLanguage)
	}
	if status.FilesByLanguage["typescript"] != 6 || status.FilesByLanguage["tsx"] != 1 || status.FilesByLanguage["jsx"] != 1 {
		t.Fatalf("files by language = %+v", status.FilesByLanguage)
	}
	if status.NodesByKind["type"] != 11 || status.NodesByKind["func"] != 6 ||
		status.NodesByKind["method"] != 11 || status.NodesByKind["const"] != 2 ||
		status.NodesByKind["module"] != 8 {
		t.Fatalf("nodes by kind = %+v", status.NodesByKind)
	}

	want := map[string][]string{
		"src/models.ts":  {"Entity", "ID", "Kind", "Base", "touch", "User", "constructor", "rename", "create"},
		"src/util.ts":    {"normalizeName", "MAX_NAME", "Formatter", "format"},
		"src/store.ts":   {"StoreLike", "add", "find", "Store", "add", "find", "makeUser", "label"},
		"src/app.ts":     {"App", "run"},
		"src/view.tsx":   {"Props", "View"},
		"src/legacy.jsx": {"Legacy", "render"},
		"src/broken.ts":  {"good", "after"},
		"src/missing.ts": {"useMissing"},
	}
	for path, names := range want {
		if got := fixture.symbolNames(t, path); !reflect.DeepEqual(got, names) {
			t.Fatalf("%s symbols = %v, want %v", path, got, names)
		}
	}
	for _, skipped := range []string{"src/types.d.ts", "src/bundle.min.js", "dist/generated.ts"} {
		if got := fixture.symbolNames(t, skipped); len(got) != 0 {
			t.Fatalf("%s was indexed: %v", skipped, got)
		}
		if fixture.count(t, `SELECT count(*) FROM files WHERE path = ?`, skipped) != 0 {
			t.Fatalf("%s has a files row", skipped)
		}
	}

	// Stable ids are path + qualname, never a line number.
	if id := fixture.symbolIDByQual(t, "User"); id != SymbolID("src/models.ts", "User") {
		t.Fatalf("User id = %s", id)
	}
	if id := fixture.symbolIDByQual(t, "Store.find"); id != SymbolID("src/store.ts", "Store.find") {
		t.Fatalf("Store.find id = %s", id)
	}
	moduleID := SymbolID("src/store.ts", "src/store.ts")
	if fixture.symbolIDByQual(t, "src/store.ts") != moduleID {
		t.Fatal("the module symbol is not addressable by its path")
	}

	// Imports: one module-to-module edge per resolved specifier, confidence 1.
	if got := fixture.edgeCount(t, "imports"); got != 8 {
		t.Fatalf("import edges = %d, want 8", got)
	}
	for _, target := range []string{"src/models.ts", "src/util.ts"} {
		confidence, ok := fixture.hasEdge(t, moduleID, SymbolID(target, target), "imports")
		if !ok || confidence != 1.0 {
			t.Fatalf("store import of %s = %v, %v", target, confidence, ok)
		}
	}

	user := fixture.symbolIDByQual(t, "User")
	base := fixture.symbolIDByQual(t, "Base")
	entity := fixture.symbolIDByQual(t, "Entity")
	if confidence, ok := fixture.hasEdge(t, user, base, "extends"); !ok || confidence != 0.7 {
		t.Fatalf("User extends Base = %v, %v", confidence, ok)
	}
	if confidence, ok := fixture.hasEdge(t, user, entity, "implements"); !ok || confidence != 0.7 {
		t.Fatalf("User implements Entity = %v, %v", confidence, ok)
	}
	legacy := fixture.symbolIDByQual(t, "Legacy")
	if confidence, ok := fixture.hasEdge(t, legacy, base, "extends"); !ok || confidence != 0.5 {
		t.Fatalf("Legacy extends imported Base = %v, %v", confidence, ok)
	}

	rename := fixture.symbolIDByQual(t, "User.rename")
	touch := fixture.symbolIDByQual(t, "Base.touch")
	if confidence, ok := fixture.hasEdge(t, rename, touch, "calls"); !ok || confidence != 0.6 {
		t.Fatalf("rename -> this.touch = %v, %v", confidence, ok)
	}
	format := fixture.symbolIDByQual(t, "Formatter.format")
	normalize := fixture.symbolIDByQual(t, "normalizeName")
	if confidence, ok := fixture.hasEdge(t, format, normalize, "calls"); !ok || confidence != 0.6 {
		t.Fatalf("format -> normalizeName = %v, %v", confidence, ok)
	}
	label := fixture.symbolIDByQual(t, "label")
	if confidence, ok := fixture.hasEdge(t, label, normalize, "calls"); !ok || confidence != 0.5 {
		t.Fatalf("label -> imported normalizeName = %v, %v", confidence, ok)
	}
	run := fixture.symbolIDByQual(t, "App.run")
	if confidence, ok := fixture.hasEdge(t, run, label, "calls"); !ok || confidence != 0.4 {
		t.Fatalf("run -> namespace store.label = %v, %v", confidence, ok)
	}

	// A constructor site is a reference, never a fake type-resolved call.
	create := fixture.symbolIDByQual(t, "User.create")
	if confidence, ok := fixture.hasEdge(t, create, user, "references"); !ok || confidence != 0.6 {
		t.Fatalf("create -> new User = %v, %v", confidence, ok)
	}
	view := fixture.symbolIDByQual(t, "View")
	appType := fixture.symbolIDByQual(t, "App")
	if confidence, ok := fixture.hasEdge(t, view, appType, "references"); !ok || confidence != 0.4 {
		t.Fatalf("View -> imported App = %v, %v", confidence, ok)
	}

	// Search, Node and Impact report the script language and low-confidence hops.
	hits, err := fixture.index.Search(context.Background(), ports.GraphQuery{Text: "User"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Language != "typescript" || hits[0].Kind != "type" {
		t.Fatalf("User search = %+v", hits)
	}
	node, err := fixture.index.Node(context.Background(), "scripts", "User.rename", "", ports.NodeOpts{IncludeCallees: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(node.Callees) == 0 || node.Callees[0].Name != "touch" || node.Callees[0].Confidence != 0.6 {
		t.Fatalf("rename callees = %+v", node.Callees)
	}
	impact, err := fixture.index.Impact(context.Background(), "scripts", "Base", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	reached := map[string]bool{}
	for _, hit := range impact.Hits {
		reached[hit.Name] = true
	}
	for _, name := range []string{"User", "Legacy"} {
		if !reached[name] {
			t.Fatalf("impact from Base does not reach %s: %+v", name, impact.Hits)
		}
	}
	if len(impact.Notes) == 0 || !strings.Contains(strings.Join(impact.Notes, " "), "heuristics") {
		t.Fatalf("impact does not disclose heuristic hops: %+v", impact.Notes)
	}
}

// TestScriptIncrementalSync edits, adds and deletes script files by hash.
func TestScriptIncrementalSync(t *testing.T) {
	fixture := newScriptFixture(t)
	fixture.mustIndex(t)
	ctx := context.Background()
	userIDBefore := fixture.symbolIDByQual(t, "User")
	var userLineBefore int
	if err := fixture.index.db.QueryRowContext(ctx,
		`SELECT start_line FROM symbols WHERE qualname = 'User'`).Scan(&userLineBefore); err != nil {
		t.Fatal(err)
	}
	var utilIndexedAt, modelsIndexedAt int64
	if err := fixture.index.db.QueryRowContext(ctx,
		`SELECT indexed_at FROM files WHERE path = 'src/util.ts'`).Scan(&utilIndexedAt); err != nil {
		t.Fatal(err)
	}
	if err := fixture.index.db.QueryRowContext(ctx,
		`SELECT indexed_at FROM files WHERE path = 'src/models.ts'`).Scan(&modelsIndexedAt); err != nil {
		t.Fatal(err)
	}
	fixture.advance(time.Hour)

	// A line shift must not change ids: every definition is path + qualname.
	shifted := strings.Repeat("// shifted\n", 6) + scriptFixtures["src/models.ts"]
	fixture.writeScriptFile(t, "src/models.ts", shifted)
	result := fixture.mustSync(t)
	if result.Changed != 1 {
		t.Fatalf("changed = %d, want 1: %+v", result.Changed, result.Notes)
	}
	if strings.Contains(strings.Join(result.Notes, " "), "full rebuild") {
		t.Fatalf("script-only change escalated to a full rebuild: %+v", result.Notes)
	}
	if id := fixture.symbolIDByQual(t, "User"); id != userIDBefore {
		t.Fatalf("User id changed after a line shift: %s -> %s", userIDBefore, id)
	}
	var startLine int
	if err := fixture.index.db.QueryRowContext(ctx,
		`SELECT start_line FROM symbols WHERE qualname = 'User'`).Scan(&startLine); err != nil {
		t.Fatal(err)
	}
	if startLine != userLineBefore+6 {
		t.Fatalf("User start line = %d, want %d after six inserted lines", startLine, userLineBefore+6)
	}
	var modelsIndexedAfter int64
	if err := fixture.index.db.QueryRowContext(ctx,
		`SELECT indexed_at FROM files WHERE path = 'src/models.ts'`).Scan(&modelsIndexedAfter); err != nil {
		t.Fatal(err)
	}
	if modelsIndexedAfter == modelsIndexedAt {
		t.Fatal("the edited models.ts was not rewritten")
	}
	var utilIndexedAfter int64
	if err := fixture.index.db.QueryRowContext(ctx,
		`SELECT indexed_at FROM files WHERE path = 'src/util.ts'`).Scan(&utilIndexedAfter); err != nil {
		t.Fatal(err)
	}
	if utilIndexedAfter != utilIndexedAt {
		t.Fatalf("unchanged util.ts was rewritten: %d -> %d", utilIndexedAt, utilIndexedAfter)
	}
	// The importer was re-extracted, so the inheritance edge survives.
	legacy := fixture.symbolIDByQual(t, "Legacy")
	if _, ok := fixture.hasEdge(t, legacy, fixture.symbolIDByQual(t, "Base"), "extends"); !ok {
		t.Fatal("Legacy extends Base was lost after the models.ts edit")
	}

	// A new function in an existing file is visible immediately.
	added := "export function removeUser(id: string): boolean {\n  return id.length > 0;\n}\n" +
		scriptFixtures["src/store.ts"]
	fixture.writeScriptFile(t, "src/store.ts", added)
	fixture.advance(time.Hour)
	fixture.mustSync(t)
	hits, err := fixture.index.Search(ctx, ports.GraphQuery{Text: "removeUser"})
	if err != nil || len(hits) != 1 {
		t.Fatalf("removeUser = %+v, %v", hits, err)
	}
	if hits[0].Language != "typescript" || hits[0].Kind != "func" {
		t.Fatalf("removeUser hit = %+v", hits[0])
	}

	// Deleting an imported file removes its rows and every edge to it.
	deletedModule := SymbolID("src/missing.ts", "src/missing.ts")
	if err := os.Remove(filepath.Join(fixture.root, "src", "missing.ts")); err != nil {
		t.Fatal(err)
	}
	fixture.advance(time.Hour)
	fixture.mustSync(t)
	if fixture.count(t, `SELECT count(*) FROM files WHERE path = 'src/missing.ts'`) != 0 {
		t.Fatal("the deleted file row survived")
	}
	if fixture.count(t, `SELECT count(*) FROM symbols WHERE path = 'src/missing.ts'`) != 0 {
		t.Fatal("deleted file symbols survived")
	}
	if fixture.count(t, `SELECT count(*) FROM edges WHERE target = ?`, deletedModule) != 0 {
		t.Fatal("edges to the deleted module survived")
	}

	// Deleting a file that others import must relink the importers.
	if err := os.Remove(filepath.Join(fixture.root, "src", "util.ts")); err != nil {
		t.Fatal(err)
	}
	fixture.advance(time.Hour)
	fixture.mustSync(t)
	utilModule := SymbolID("src/util.ts", "src/util.ts")
	if fixture.count(t, `SELECT count(*) FROM symbols WHERE path = 'src/util.ts'`) != 0 {
		t.Fatal("util.ts symbols survived deletion")
	}
	if fixture.count(t, `SELECT count(*) FROM edges WHERE target = ?`, utilModule) != 0 {
		t.Fatal("import edges to the deleted util module survived")
	}
	if fixture.count(t, `SELECT count(*) FROM edges e WHERE NOT EXISTS (SELECT 1 FROM symbols s WHERE s.id = e.source)`+
		` OR NOT EXISTS (SELECT 1 FROM symbols t WHERE t.id = e.target)`) != 0 {
		t.Fatal("dangling edges remain after script deletion")
	}
}

// TestScriptNestedRepoIsSkipped keeps one store bound to one checkout: a
// nested clone is not walked.
func TestScriptNestedRepoIsSkipped(t *testing.T) {
	fixture := newScriptFixture(t)
	nested := filepath.Join(fixture.root, "nested")
	if err := os.MkdirAll(filepath.Join(nested, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "hidden.ts"), []byte("export function hidden(): void {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.mustIndex(t)
	if fixture.count(t, `SELECT count(*) FROM files WHERE path = 'nested/hidden.ts'`) != 0 {
		t.Fatal("a nested Git checkout was indexed")
	}
	if fixture.count(t, `SELECT count(*) FROM symbols WHERE name = 'hidden'`) != 0 {
		t.Fatal("a symbol from a nested Git checkout was indexed")
	}
}

// TestScriptBOMAndCRLF proves a UTF-8 BOM and CRLF line endings do not turn
// the first declaration into a parse error.
func TestScriptBOMAndCRLF(t *testing.T) {
	fixture := newScriptFixture(t)
	source := "\uFEFF" + strings.ReplaceAll(scriptFixtures["src/util.ts"], "\n", "\r\n")
	fixture.writeScriptFile(t, "src/util.ts", source)
	fixture.mustIndex(t)
	if got := fixture.symbolNames(t, "src/util.ts"); !reflect.DeepEqual(got, []string{"normalizeName", "MAX_NAME", "Formatter", "format"}) {
		t.Fatalf("BOM/CRLF symbols = %v", got)
	}
	var lines int
	if err := fixture.index.db.QueryRow(`SELECT end_line - start_line FROM symbols WHERE qualname = 'Formatter'`).Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if lines < 1 {
		t.Fatalf("Formatter range = %d lines", lines)
	}
}

// TestScriptSyntaxErrorsAreSkipped proves a file with a parse error is partial,
// not silently trusted: the clean declarations survive, the broken one does not.
func TestScriptSyntaxErrorsAreSkipped(t *testing.T) {
	fixture := newScriptFixture(t)
	fixture.mustIndex(t)
	if got := fixture.symbolNames(t, "src/broken.ts"); !reflect.DeepEqual(got, []string{"good", "after"}) {
		t.Fatalf("broken.ts symbols = %v", got)
	}
	if fixture.count(t, `SELECT count(*) FROM symbols WHERE path = 'src/broken.ts' AND kind = 'module'`) != 1 {
		t.Fatal("the broken file lost its module node")
	}
	status, err := fixture.index.Status(context.Background(), "scripts")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(status.Notes, " "), "syntax errors") {
		t.Fatalf("status does not disclose the partial file: %+v", status.Notes)
	}
}

// TestScriptIncrementalErrorEdit proves an introduced syntax error drops the
// declarations it covers and clears on the next clean sync.
func TestScriptIncrementalErrorEdit(t *testing.T) {
	fixture := newScriptFixture(t)
	fixture.mustIndex(t)
	broken := `export function normalizeName(value: string): string {
  return value;
}

export function half(: string {
  return value;
}

export const MAX_NAME = 64;
`
	fixture.writeScriptFile(t, "src/util.ts", broken)
	fixture.advance(time.Hour)
	result := fixture.mustSync(t)
	if !strings.Contains(strings.Join(result.Notes, " "), "ERROR spans") {
		t.Fatalf("sync does not disclose skipped ERROR spans: %+v", result.Notes)
	}
	if got := fixture.symbolNames(t, "src/util.ts"); !reflect.DeepEqual(got, []string{"normalizeName", "MAX_NAME"}) {
		t.Fatalf("edited util.ts symbols = %v", got)
	}
	if fixture.count(t, `SELECT count(*) FROM symbols WHERE name = 'half'`) != 0 {
		t.Fatal("a symbol from an ERROR span was indexed")
	}
	fixture.writeScriptFile(t, "src/util.ts", scriptFixtures["src/util.ts"])
	fixture.advance(time.Hour)
	fixture.mustSync(t)
	if got := fixture.symbolNames(t, "src/util.ts"); !reflect.DeepEqual(got, []string{"normalizeName", "MAX_NAME", "Formatter", "format"}) {
		t.Fatalf("restored util.ts symbols = %v", got)
	}
}

// TestScriptDeterministicRebuild indexes the same checkout twice and compares
// every stored identity.
func TestScriptDeterministicRebuild(t *testing.T) {
	fixture := newScriptFixture(t)
	fixture.mustIndex(t)
	firstSymbols, firstEdges := fixture.snapshot(t)
	if _, err := fixture.index.Rebuild(context.Background(), "scripts"); err != nil {
		t.Fatal(err)
	}
	secondSymbols, secondEdges := fixture.snapshot(t)
	if !reflect.DeepEqual(firstSymbols, secondSymbols) {
		t.Fatal("symbol rows are not deterministic across a rebuild")
	}
	if !reflect.DeepEqual(firstEdges, secondEdges) {
		t.Fatal("edge rows are not deterministic across a rebuild")
	}
}

// snapshot renders sorted symbol and edge rows.
func (f *scriptFixture) snapshot(t *testing.T) ([]string, []string) {
	t.Helper()
	ctx := context.Background()
	symbolRows, err := f.index.db.QueryContext(ctx,
		`SELECT id, path, kind, name, qualname, start_line, end_line, signature, exported FROM symbols ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var symbols []string
	for symbolRows.Next() {
		var id, path, kind, name, qualname, signature string
		var start, end, exported int
		if err := symbolRows.Scan(&id, &path, &kind, &name, &qualname, &start, &end, &signature, &exported); err != nil {
			t.Fatal(err)
		}
		symbols = append(symbols, strings.Join([]string{id, path, kind, name, qualname}, "\x00"))
	}
	_ = symbolRows.Close()
	edgeRows, err := f.index.db.QueryContext(ctx,
		`SELECT source, target, kind, line, confidence, provenance FROM edges ORDER BY source, target, kind, line`)
	if err != nil {
		t.Fatal(err)
	}
	var edges []string
	for edgeRows.Next() {
		var source, target, kind, provenance string
		var line int
		var confidence float64
		if err := edgeRows.Scan(&source, &target, &kind, &line, &confidence, &provenance); err != nil {
			t.Fatal(err)
		}
		edges = append(edges, strings.Join([]string{source, target, kind}, "\x00"))
	}
	_ = edgeRows.Close()
	sort.Strings(edges)
	return symbols, edges
}

// TestScriptExtractorSkippedClaims keeps the language registry honest: only the
// four advertised extensions are indexed, and generated/declaration output is
// not.
func TestScriptExtractorSkippedClaims(t *testing.T) {
	for _, path := range []string{"a.ts", "a.tsx", "a.js", "a.jsx", "a.py"} {
		if !IsPolyglotPath(path) || LanguageForPath(path) == "" {
			t.Fatalf("%s is not claimed", path)
		}
	}
	for _, path := range []string{"a.rs", "a.go", "a.d.ts", "a.min.js", "dist/a.ts"} {
		if IsPolyglotPath(path) && polyglotPathIndexable(path) {
			t.Fatalf("%s is claimed by the script extractor", path)
		}
	}
	if LanguageForPath("a.go") != "go" {
		t.Fatal("Go is not reported as the Go language")
	}
}

// TestScriptUnsupportedLanguageIsRefused keeps the native engine honest about
// what it cannot parse.
func TestScriptUnsupportedLanguageIsRefused(t *testing.T) {
	t.Setenv("GOWORK", "off")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.rs"), []byte("fn main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath, err := DefaultDBPath(filepath.Join(t.TempDir(), "brain"), "rust", root)
	if err != nil {
		t.Fatal(err)
	}
	index, err := Open(context.Background(), Options{Root: root, DBPath: dbPath, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	if _, err := index.Index(context.Background(), "rust"); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("Rust-only checkout = %v, want ErrUnsupported", err)
	}
}

// TestScriptExtractorVersionRebuilds proves that the added languages are a new
// extraction version: an older store is rebuilt before it is read.
func TestScriptExtractorVersionRebuilds(t *testing.T) {
	fixture := newScriptFixture(t)
	fixture.mustIndex(t)
	fixture.index.mu.Lock()
	if _, err := fixture.index.db.Exec(`UPDATE meta SET value = '1' WHERE key = 'extractor_version'`); err != nil {
		t.Fatal(err)
	}
	fixture.index.mu.Unlock()
	if err := fixture.index.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), Options{
		Root: fixture.root, DBPath: fixture.dbPath, Project: "scripts", Create: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if !reopened.needsRebuild {
		t.Fatal("an extractor 1 store was accepted by the extractor 2 engine")
	}
	if _, err := reopened.Search(context.Background(), ports.GraphQuery{Text: "User"}); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("read of a stale extractor store = %v", err)
	}
	if _, err := reopened.Index(context.Background(), "scripts"); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := reopened.db.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'extractor_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != ExtractorVersion {
		t.Fatalf("extractor version = %d, want %d", version, ExtractorVersion)
	}
}

// TestScriptRuntimeSmoke extracts one real desktop TypeScript file. It skips on
// a machine without the desktop checkout.
func TestScriptRuntimeSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("runtime smoke parses a real desktop file")
	}
	path := filepath.Join("..", "..", "..", "..", "lv24-brain-desktop", "src", "pages", "connection-detail", "ui", "ConnectionDetailPage.tsx")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("desktop fixture is unavailable: %v", err)
	}
	spec, ok := polyglotSpecForPath(path)
	if !ok {
		t.Fatal("the desktop file is not claimed")
	}
	index := &Index{}
	started := time.Now()
	result, err := index.extractScript(spec, source)
	if err != nil {
		t.Fatal(err)
	}
	cold := time.Since(started)
	defer func() { _ = index.Close() }()
	if len(result.Symbols) < 5 || len(result.Imports) < 5 {
		t.Fatalf("smoke extraction = %d symbols, %d imports", len(result.Symbols), len(result.Imports))
	}
	warmStart := time.Now()
	const runs = 3
	for run := 0; run < runs; run++ {
		if _, err := index.extractScript(spec, source); err != nil {
			t.Fatal(err)
		}
	}
	warm := time.Since(warmStart) / runs
	t.Logf("smoke: %d bytes, %d symbols, %d imports, %d calls, cold %s, warm %s",
		len(source), len(result.Symbols), len(result.Imports), len(result.Calls),
		cold.Round(time.Millisecond), warm.Round(time.Millisecond))
}
