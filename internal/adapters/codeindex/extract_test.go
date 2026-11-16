package codeindex

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestSyntacticFallbackConfidence proves the two lower confidence tiers of the
// extractor: a call resolved by package name is 0.8, a call matched by a unique
// method name is 0.5.
func TestSyntacticFallbackConfidence(t *testing.T) {
	const source = `package broken

func A() {}

func M() {}

func B() {
	A()
	value.M()
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "broken.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	x := newExtractor(t.TempDir(), "example.com/broken", map[string]bool{})
	x.pkgPaths["example.com/broken"] = true
	idA := SymbolID("example.com/broken", "broken.A")
	idM := SymbolID("example.com/broken", "broken.M")
	idB := SymbolID("example.com/broken", "broken.B")
	x.newIDs[idA], x.newIDs[idM], x.newIDs[idB] = true, true, true
	state := &pkgSymbols{
		pkg:          &packages.Package{Fset: fset, Syntax: []*ast.File{file}},
		path:         "example.com/broken",
		name:         "broken",
		files:        map[string]*ast.File{"broken.go": file},
		fileImports:  map[string]map[string]string{"broken.go": {}},
		ranges:       map[string][]symbolRange{},
		names:        map[string]string{"A": idA},
		methodByName: map[string][]string{"M": {idM}},
		typeLines:    map[string]int{},
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		id := SymbolID("example.com/broken", "broken."+fn.Name.Name)
		state.ranges["broken.go"] = append(state.ranges["broken.go"], symbolRange{
			start: fset.Position(fn.Pos()).Line,
			end:   fset.Position(fn.End()).Line,
			id:    id,
		})
	}
	x.collectSyntacticEdges(state)
	byTarget := map[string]edgeRow{}
	for _, edge := range x.edges {
		byTarget[edge.Target] = edge
	}
	if edge, ok := byTarget[idA]; !ok || edge.Kind != "calls" || edge.Confidence != 0.8 || edge.Provenance != "syntax" {
		t.Fatalf("package call edge = %+v", edge)
	}
	if edge, ok := byTarget[idM]; !ok || edge.Kind != "calls" || edge.Confidence != 0.5 {
		t.Fatalf("method-name edge = %+v", edge)
	}
	for _, edge := range x.edges {
		if edge.Source != idB {
			t.Fatalf("edge source = %s, want B (%s)", edge.Source, idB)
		}
	}
}
