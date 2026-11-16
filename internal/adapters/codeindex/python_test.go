package codeindex

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// pythonFixtures is a Python-only checkout: a package, relative and absolute
// imports, inheritance, decorators, type aliases, a docstring and a file with a
// syntax error.
var pythonFixtures = map[string]string{
	"pkg/__init__.py": `"""Fixtures package."""
from .models import Widget, helper

__all__ = ["Widget", "helper"]
`,
	"pkg/models.py": `"""Model declarations."""
import os
from . import util
from .shared import Mixin

MAX_ITEMS = 32
_secret = 1
type ItemID = int

class Base:
    """Base model."""

    def __init__(self, name: str) -> None:
        self.name = name

    def label(self) -> str:
        return util.slug(self.name)

class Widget(Base, Mixin):
    """A widget."""

    count = 0

    def __init__(self, name: str, ident: ItemID) -> None:
        super().__init__(name)
        self.ident = ident

    @property
    def label(self) -> str:
        return self.name

    @staticmethod
    def build(name: str) -> "Widget":
        return Widget(name, 1)

    def clone(self) -> "Widget":
        return Widget.build(self.name)

def helper(value: int) -> int:
    return value + 1

def run() -> int:
    def inner(value: int) -> int:
        return value
    return helper(inner(1))

def _private() -> None:
    return None
`,
	"pkg/util.py": `"""Utility functions."""

def slug(value: str) -> str:
    return value.strip().lower()

def unused() -> None:
    pass
`,
	"pkg/shared.py": `class Mixin:
    """Mixin marker."""
`,
	"pkg/_private.py": `def hidden() -> int:
    return 0
`,
	"pkg/alias.py": `import pkg.util as u

def call_alias() -> str:
    return u.slug("x")
`,
	"pkg/broken.py": `def good() -> int:
    return 1

def broken( -> int:
    return 2

def after() -> int:
    return 3
`,
	"pkg/__pycache__/cached.py": `def cached() -> None:
    pass
`,
	"venv/lib.py": `def vendored() -> None:
    pass
`,
	"main.py": `from pkg.models import Widget, helper
from pkg import util

def main() -> int:
    widget = Widget("x", 1)
    util.slug(widget.name)
    return helper(2)
`,
}

// newPythonFixture opens the Python fixture checkout.
func newPythonFixture(t *testing.T) *scriptFixture {
	t.Helper()
	return newCodeFixture(t, pythonFixtures, "python")
}

// TestPythonLanguageRegistration keeps the registry honest: the vendored
// reactor exposes the Python grammar the spec names, and generated Python
// directories are not indexed.
func TestPythonLanguageRegistration(t *testing.T) {
	spec, ok := polyglotSpecForPath("pkg/mod.py")
	if !ok {
		t.Fatal("a Python file is not claimed")
	}
	if spec.Language != pythonLanguage || spec.Grammar != "tree_sitter_python" || spec.ABI != 15 {
		t.Fatalf("Python spec = %+v", spec)
	}
	if LanguageForPath("pkg/mod.py") != pythonLanguage {
		t.Fatalf("LanguageForPath = %q", LanguageForPath("pkg/mod.py"))
	}
	for _, path := range []string{"venv/lib.py", "__pycache__/cached.py", "x/site-packages/a.py"} {
		if polyglotPathIndexable(path) {
			t.Fatalf("%s is indexable", path)
		}
	}
}

// TestPythonIndexExtractsSymbolsAndEdges is the golden shape of a Python
// checkout: exact declaration sets, imports, calls, inheritance and type
// references at syntax-only confidence.
func TestPythonIndexExtractsSymbolsAndEdges(t *testing.T) {
	fixture := newPythonFixture(t)
	result := fixture.mustIndex(t)
	if !strings.Contains(strings.Join(result.Notes, " "), "TypeScript/JavaScript/Python") {
		t.Fatalf("index notes do not disclose Python extraction: %+v", result.Notes)
	}
	status, err := fixture.index.Status(context.Background(), "python")
	if err != nil {
		t.Fatal(err)
	}
	if status.Files != 8 || status.FilesByLanguage["python"] != 8 {
		t.Fatalf("files = %d, languages = %+v", status.Files, status.FilesByLanguage)
	}
	wantKinds := map[string]int{"module": 8, "type": 4, "method": 6, "func": 10, "const": 1, "var": 3}
	for kind, count := range wantKinds {
		if status.NodesByKind[kind] != count {
			t.Fatalf("nodes by kind = %+v, want %s=%d", status.NodesByKind, kind, count)
		}
	}

	want := map[string][]string{
		"pkg/__init__.py": {"__all__"},
		"pkg/models.py": {
			"MAX_ITEMS", "_secret", "ItemID", "Base", "__init__", "label",
			"Widget", "count", "__init__", "label", "build", "clone",
			"helper", "run", "_private",
		},
		"pkg/util.py":     {"slug", "unused"},
		"pkg/shared.py":   {"Mixin"},
		"pkg/_private.py": {"hidden"},
		"pkg/alias.py":    {"call_alias"},
		"pkg/broken.py":   {"good", "after"},
		"main.py":         {"main"},
	}
	for path, names := range want {
		if got := fixture.symbolNames(t, path); !reflect.DeepEqual(got, names) {
			t.Fatalf("%s symbols = %v, want %v", path, got, names)
		}
	}
	if fixture.count(t, `SELECT count(*) FROM symbols WHERE name = 'inner'`) != 0 {
		t.Fatal("a nested function was indexed")
	}
	if fixture.count(t, `SELECT count(*) FROM symbols WHERE name = 'cached' OR name = 'vendored'`) != 0 {
		t.Fatal("a generated Python directory was indexed")
	}
	if fixture.count(t, `SELECT count(*) FROM files WHERE path LIKE '%__pycache__%' OR path LIKE 'venv/%'`) != 0 {
		t.Fatal("a skipped Python file has a files row")
	}

	// Stable ids are path + qualname, never a line number.
	if id := fixture.symbolIDByQual(t, "Widget"); id != SymbolID("pkg/models.py", "Widget") {
		t.Fatalf("Widget id = %s", id)
	}
	if id := fixture.symbolIDByQual(t, "Widget.build"); id != SymbolID("pkg/models.py", "Widget.build") {
		t.Fatalf("Widget.build id = %s", id)
	}
	moduleID := SymbolID("pkg/models.py", "pkg/models.py")

	// Export follows the Python naming convention, including dunders.
	var exported int
	if err := fixture.index.db.QueryRow(`SELECT exported FROM symbols WHERE qualname = '_private'`).Scan(&exported); err != nil {
		t.Fatal(err)
	}
	if exported != 0 {
		t.Fatal("_private is marked exported")
	}
	if err := fixture.index.db.QueryRow(`SELECT exported FROM symbols WHERE qualname = 'Widget.__init__'`).Scan(&exported); err != nil {
		t.Fatal(err)
	}
	if exported != 1 {
		t.Fatal("Widget.__init__ is not marked exported")
	}
	if doc := fixture.symbolDoc(t, "Base"); doc != "Base model." {
		t.Fatalf("Base doc = %q", doc)
	}
	if signature := fixture.symbolSignature(t, "Widget.label"); !strings.HasPrefix(signature, "@property def label") {
		t.Fatalf("decorated method signature = %q", signature)
	}

	// Imports: module-to-module, confidence 1.0. `from . import util` imports
	// both the package and the submodule.
	if got := fixture.edgeCount(t, "imports"); got != 8 {
		t.Fatalf("import edges = %d, want 8", got)
	}
	utilModule := SymbolID("pkg/util.py", "pkg/util.py")
	if confidence, ok := fixture.hasEdge(t, moduleID, utilModule, "imports"); !ok || confidence != 1.0 {
		t.Fatalf("models imports util = %v, %v", confidence, ok)
	}
	aliasModule := SymbolID("pkg/alias.py", "pkg/alias.py")
	if confidence, ok := fixture.hasEdge(t, aliasModule, utilModule, "imports"); !ok || confidence != 1.0 {
		t.Fatalf("alias imports pkg.util = %v, %v", confidence, ok)
	}
	mainModule := SymbolID("main.py", "main.py")
	if confidence, ok := fixture.hasEdge(t, mainModule, moduleID, "imports"); !ok || confidence != 1.0 {
		t.Fatalf("main imports models = %v, %v", confidence, ok)
	}
	if confidence, ok := fixture.hasEdge(t, mainModule, utilModule, "imports"); !ok || confidence != 1.0 {
		t.Fatalf("main imports the util submodule = %v, %v", confidence, ok)
	}

	// Inheritance: local 0.7, imported 0.5, multiple bases are separate edges.
	widget := fixture.symbolIDByQual(t, "Widget")
	base := fixture.symbolIDByQual(t, "Base")
	mixin := fixture.symbolIDByQual(t, "Mixin")
	if confidence, ok := fixture.hasEdge(t, widget, base, "extends"); !ok || confidence != 0.7 {
		t.Fatalf("Widget extends Base = %v, %v", confidence, ok)
	}
	if confidence, ok := fixture.hasEdge(t, widget, mixin, "extends"); !ok || confidence != 0.5 {
		t.Fatalf("Widget extends Mixin = %v, %v", confidence, ok)
	}

	// Calls: same-file 0.6, imported function 0.5, imported module 0.4.
	label := fixture.symbolIDByQual(t, "Base.label")
	slug := fixture.symbolIDByQual(t, "slug")
	if confidence, ok := fixture.hasEdge(t, label, slug, "calls"); !ok || confidence != 0.4 {
		t.Fatalf("Base.label -> util.slug = %v, %v", confidence, ok)
	}
	helper := fixture.symbolIDByQual(t, "helper")
	run := fixture.symbolIDByQual(t, "run")
	if confidence, ok := fixture.hasEdge(t, run, helper, "calls"); !ok || confidence != 0.6 {
		t.Fatalf("run -> helper = %v, %v", confidence, ok)
	}
	main := fixture.symbolIDByQual(t, "main")
	if confidence, ok := fixture.hasEdge(t, main, helper, "calls"); !ok || confidence != 0.5 {
		t.Fatalf("main -> imported helper = %v, %v", confidence, ok)
	}
	if confidence, ok := fixture.hasEdge(t, main, slug, "calls"); !ok || confidence != 0.4 {
		t.Fatalf("main -> util.slug = %v, %v", confidence, ok)
	}
	clone := fixture.symbolIDByQual(t, "Widget.clone")
	build := fixture.symbolIDByQual(t, "Widget.build")
	if confidence, ok := fixture.hasEdge(t, clone, build, "calls"); !ok || confidence != 0.6 {
		t.Fatalf("clone -> Widget.build = %v, %v", confidence, ok)
	}
	callAlias := fixture.symbolIDByQual(t, "call_alias")
	if confidence, ok := fixture.hasEdge(t, callAlias, slug, "calls"); !ok || confidence != 0.4 {
		t.Fatalf("call_alias -> u.slug = %v, %v", confidence, ok)
	}

	// Constructors are references, never type-resolved call edges.
	if confidence, ok := fixture.hasEdge(t, build, widget, "references"); !ok || confidence != 0.6 {
		t.Fatalf("build -> new Widget = %v, %v", confidence, ok)
	}
	if confidence, ok := fixture.hasEdge(t, main, widget, "references"); !ok || confidence != 0.4 {
		t.Fatalf("main -> imported Widget = %v, %v", confidence, ok)
	}
	itemID := fixture.symbolIDByQual(t, "ItemID")
	initMethod := fixture.symbolIDByQual(t, "Widget.__init__")
	if confidence, ok := fixture.hasEdge(t, initMethod, itemID, "references"); !ok || confidence != 0.5 {
		t.Fatalf("Widget.__init__ -> ItemID = %v, %v", confidence, ok)
	}
	if status.UnresolvedRefs != 0 {
		t.Fatalf("unresolved references = %d, want 0", status.UnresolvedRefs)
	}
}

// symbolDoc returns one stored doc comment.
func (f *scriptFixture) symbolDoc(t *testing.T, qualname string) string {
	t.Helper()
	var doc string
	if err := f.index.db.QueryRow(`SELECT doc FROM symbols WHERE qualname = ?`, qualname).Scan(&doc); err != nil {
		t.Fatalf("symbol %s: %v", qualname, err)
	}
	return doc
}

// symbolSignature returns one stored signature.
func (f *scriptFixture) symbolSignature(t *testing.T, qualname string) string {
	t.Helper()
	var signature string
	if err := f.index.db.QueryRow(`SELECT signature FROM symbols WHERE qualname = ?`, qualname).Scan(&signature); err != nil {
		t.Fatalf("symbol %s: %v", qualname, err)
	}
	return signature
}

// TestPythonSyntaxErrorsAreSkipped proves a partial Python file keeps its clean
// declarations and drops the ones inside an ERROR span.
func TestPythonSyntaxErrorsAreSkipped(t *testing.T) {
	fixture := newPythonFixture(t)
	fixture.mustIndex(t)
	if got := fixture.symbolNames(t, "pkg/broken.py"); !reflect.DeepEqual(got, []string{"good", "after"}) {
		t.Fatalf("broken.py symbols = %v", got)
	}
	if fixture.count(t, `SELECT count(*) FROM symbols WHERE name = 'broken'`) != 0 {
		t.Fatal("a symbol from an ERROR span was indexed")
	}
	if fixture.count(t, `SELECT count(*) FROM symbols WHERE path = 'pkg/broken.py' AND kind = 'module'`) != 1 {
		t.Fatal("the broken file lost its module node")
	}
	status, err := fixture.index.Status(context.Background(), "python")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(status.Notes, " "), "syntax errors") {
		t.Fatalf("status does not disclose the partial file: %+v", status.Notes)
	}
}

// TestPythonBOMAndCRLF proves a BOM and CRLF line endings do not break the
// first declaration.
func TestPythonBOMAndCRLF(t *testing.T) {
	fixture := newPythonFixture(t)
	source := "\uFEFF" + strings.ReplaceAll(pythonFixtures["pkg/util.py"], "\n", "\r\n")
	fixture.writeScriptFile(t, "pkg/util.py", source)
	fixture.mustIndex(t)
	if got := fixture.symbolNames(t, "pkg/util.py"); !reflect.DeepEqual(got, []string{"slug", "unused"}) {
		t.Fatalf("BOM/CRLF symbols = %v", got)
	}
}

// TestPythonIncrementalSync edits, adds and deletes Python files by hash. A
// line shift keeps ids; an imported file deletion relinks its importer.
func TestPythonIncrementalSync(t *testing.T) {
	fixture := newPythonFixture(t)
	fixture.mustIndex(t)
	ctx := context.Background()
	widgetID := fixture.symbolIDByQual(t, "Widget")
	var widgetLineBefore int
	if err := fixture.index.db.QueryRowContext(ctx,
		`SELECT start_line FROM symbols WHERE qualname = 'Widget'`).Scan(&widgetLineBefore); err != nil {
		t.Fatal(err)
	}
	readIndexedAt := func(path string) int64 {
		t.Helper()
		var value int64
		if err := fixture.index.db.QueryRowContext(ctx, `SELECT indexed_at FROM files WHERE path = ?`, path).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	modelsBefore := readIndexedAt("pkg/models.py")
	utilBefore := readIndexedAt("pkg/util.py")
	fixture.advance(time.Hour)

	shifted := strings.Repeat("# shifted\n", 4) + pythonFixtures["pkg/models.py"]
	fixture.writeScriptFile(t, "pkg/models.py", shifted)
	result := fixture.mustSync(t)
	if result.Changed != 1 {
		t.Fatalf("changed = %d, want 1: %+v", result.Changed, result.Notes)
	}
	if strings.Contains(strings.Join(result.Notes, " "), "full rebuild") {
		t.Fatalf("Python-only change escalated to a full rebuild: %+v", result.Notes)
	}
	if id := fixture.symbolIDByQual(t, "Widget"); id != widgetID {
		t.Fatalf("Widget id changed after a line shift: %s -> %s", widgetID, id)
	}
	var widgetLineAfter int
	if err := fixture.index.db.QueryRowContext(ctx,
		`SELECT start_line FROM symbols WHERE qualname = 'Widget'`).Scan(&widgetLineAfter); err != nil {
		t.Fatal(err)
	}
	if widgetLineAfter != widgetLineBefore+4 {
		t.Fatalf("Widget start line = %d, want %d", widgetLineAfter, widgetLineBefore+4)
	}
	if readIndexedAt("pkg/models.py") == modelsBefore {
		t.Fatal("the edited models.py was not rewritten")
	}
	if readIndexedAt("pkg/util.py") != utilBefore {
		t.Fatal("unchanged util.py was rewritten")
	}
	// The importer is re-extracted, so its import edge is relinked even though
	// its own files row keeps the old indexed_at.
	mainModule := SymbolID("main.py", "main.py")
	modelsModule := SymbolID("pkg/models.py", "pkg/models.py")
	if _, ok := fixture.hasEdge(t, mainModule, modelsModule, "imports"); !ok {
		t.Fatal("main imports models was lost after the models.py edit")
	}
	base := fixture.symbolIDByQual(t, "Base")
	if _, ok := fixture.hasEdge(t, widgetID, base, "extends"); !ok {
		t.Fatal("Widget extends Base was lost after the models.py edit")
	}

	// A new function is visible immediately.
	added := "def added() -> int:\n    return 0\n\n" + pythonFixtures["pkg/models.py"]
	fixture.writeScriptFile(t, "pkg/models.py", added)
	fixture.advance(time.Hour)
	fixture.mustSync(t)
	hits, err := fixture.index.Search(ctx, ports.GraphQuery{Text: "added"})
	if err != nil || len(hits) != 1 {
		t.Fatalf("added = %+v, %v", hits, err)
	}
	if hits[0].Language != pythonLanguage || hits[0].Kind != "func" {
		t.Fatalf("added hit = %+v", hits[0])
	}

	// Deleting an imported file removes its rows and every edge to it.
	label := fixture.symbolIDByQual(t, "Base.label")
	slug := fixture.symbolIDByQual(t, "slug")
	if err := os.Remove(filepath.Join(fixture.root, "pkg", "util.py")); err != nil {
		t.Fatal(err)
	}
	fixture.advance(time.Hour)
	fixture.mustSync(t)
	utilModule := SymbolID("pkg/util.py", "pkg/util.py")
	if fixture.count(t, `SELECT count(*) FROM files WHERE path = 'pkg/util.py'`) != 0 {
		t.Fatal("the deleted file row survived")
	}
	if fixture.count(t, `SELECT count(*) FROM symbols WHERE path = 'pkg/util.py'`) != 0 {
		t.Fatal("deleted file symbols survived")
	}
	if fixture.count(t, `SELECT count(*) FROM edges WHERE target = ?`, utilModule) != 0 {
		t.Fatal("edges to the deleted module survived")
	}
	if _, ok := fixture.hasEdge(t, label, slug, "calls"); ok {
		t.Fatal("the call edge to the deleted util.slug survived")
	}
	if fixture.count(t, `SELECT count(*) FROM edges e WHERE NOT EXISTS (SELECT 1 FROM symbols s WHERE s.id = e.source)`+
		` OR NOT EXISTS (SELECT 1 FROM symbols t WHERE t.id = e.target)`) != 0 {
		t.Fatal("dangling edges remain after Python deletion")
	}
}

// TestPythonDeterministicRebuild indexes the same checkout twice and compares
// every stored identity.
func TestPythonDeterministicRebuild(t *testing.T) {
	fixture := newPythonFixture(t)
	fixture.mustIndex(t)
	firstSymbols, firstEdges := fixture.snapshot(t)
	if _, err := fixture.index.Rebuild(context.Background(), "python"); err != nil {
		t.Fatal(err)
	}
	secondSymbols, secondEdges := fixture.snapshot(t)
	if !reflect.DeepEqual(firstSymbols, secondSymbols) {
		t.Fatal("Python symbol rows are not deterministic across a rebuild")
	}
	if !reflect.DeepEqual(firstEdges, secondEdges) {
		t.Fatal("Python edge rows are not deterministic across a rebuild")
	}
}

// TestPythonMixedCheckout indexes Go, TypeScript and Python in one checkout:
// each language keeps its own extractor and the Go type-checked edges survive.
func TestPythonMixedCheckout(t *testing.T) {
	fixture := newCodeFixture(t, map[string]string{
		"go.mod": "module example.com/mixed\n\ngo 1.24\n",
		"main.go": `package main

func main() {
	Helper()
}

func Helper() {}
`,
		"src/app.ts": `export function app(): number {
  return 1;
}
`,
		"pkg/mod.py": `def mod() -> int:
    return 2
`,
	}, "mixed")
	fixture.mustIndex(t)
	status, err := fixture.index.Status(context.Background(), "mixed")
	if err != nil {
		t.Fatal(err)
	}
	if status.Files != 3 {
		t.Fatalf("files = %d: %+v", status.Files, status.FilesByLanguage)
	}
	if status.FilesByLanguage["go"] != 1 || status.FilesByLanguage["typescript"] != 1 || status.FilesByLanguage["python"] != 1 {
		t.Fatalf("files by language = %+v", status.FilesByLanguage)
	}
	for text, language := range map[string]string{"Helper": "go", "app": "typescript", "mod": "python"} {
		hits, err := fixture.index.Search(context.Background(), ports.GraphQuery{Text: text})
		if err != nil || len(hits) == 0 {
			t.Fatalf("search %s = %+v, %v", text, hits, err)
		}
		if hits[0].Language != language {
			t.Fatalf("search %s language = %s, want %s", text, hits[0].Language, language)
		}
	}
}
