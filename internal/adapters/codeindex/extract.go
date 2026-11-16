package codeindex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

// symbolRow is one row of the symbols table.
type symbolRow struct {
	ID        string
	Path      string
	Package   string
	Kind      string
	Name      string
	QualName  string
	StartLine int
	EndLine   int
	Signature string
	Doc       string
	Exported  bool
}

// edgeRow is one row of the edges table.
type edgeRow struct {
	Source     string
	Target     string
	Kind       string
	Line       int
	Confidence float64
	Provenance string
}

// symbolRange is one symbol's line span inside one file. It is used to find
// the definition a reference belongs to.
type symbolRange struct {
	start int
	end   int
	id    string
}

// pkgSymbols is the extraction state of one package.
type pkgSymbols struct {
	pkg          *packages.Package
	path         string
	name         string
	packageID    string
	packageSym   symbolRow
	files        map[string]*ast.File
	fileImports  map[string]map[string]string
	ranges       map[string][]symbolRange
	names        map[string]string
	methodByName map[string][]string
	typeLines    map[string]int
}

// extractor holds the state of one extraction batch. Symbols are collected for
// every package before any edge is resolved, so a cross-package edge can name
// a symbol that was added in the same batch.
type extractor struct {
	root     string
	module   string
	known    map[string]bool
	newIDs   map[string]bool
	pkgPaths map[string]bool
	symbols  []symbolRow
	edges    map[string]edgeRow
	unres    map[string]int
	ifaces   *interfaceIndex
	states   []*pkgSymbols
}

// newExtractor prepares a batch that resolves against known symbol ids.
func newExtractor(root, module string, known map[string]bool) *extractor {
	return &extractor{
		root:     root,
		module:   module,
		known:    known,
		newIDs:   map[string]bool{},
		pkgPaths: map[string]bool{},
		edges:    map[string]edgeRow{},
		unres:    map[string]int{},
	}
}

// loadPackages loads the module packages of the given directories. Tests are
// included so test functions and their calls are indexed. Dependencies are
// loaded for type information but only packages whose files live under the
// checkout become roots.
func (i *Index) loadPackages(ctx context.Context, dirs, patterns []string) ([]*packages.Package, int, []string, error) {
	if len(patterns) == 0 {
		if len(dirs) == 0 {
			patterns = []string{DefaultIndexPattern}
		} else {
			patterns = make([]string, 0, len(dirs))
			for _, dir := range dirs {
				patterns = append(patterns, dirPattern(dir))
			}
		}
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo |
			packages.NeedTypesSizes | packages.NeedImports | packages.NeedDeps |
			packages.NeedModule,
		Dir:   i.root,
		Tests: true,
	}
	if len(i.tags) > 0 {
		cfg.BuildFlags = []string{"-tags=" + strings.Join(i.tags, ",")}
	}
	loaded, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("codeindex: load Go packages in %s: %w", i.root, err)
	}
	seen := map[string]int{}
	packageErrors := 0
	notes := make([]string, 0, 4)
	roots := make([]*packages.Package, 0, len(loaded))
	for _, pkg := range loaded {
		if pkg == nil || pkg.PkgPath == "" {
			continue
		}
		if pkg.Name == "main" && strings.HasSuffix(pkg.PkgPath, ".test") {
			continue // the generated test main is not source
		}
		if pkg.Dir == "" {
			continue
		}
		if _, ok := i.relPath(pkg.Dir); !ok {
			continue
		}
		if len(pkg.Errors) > 0 {
			packageErrors++
			if len(notes) < 8 {
				notes = append(notes, fmt.Sprintf("%s: %s", pkg.PkgPath, pkg.Errors[0].Msg))
			}
		}
		// Tests:true yields both the plain and the test-augmented variant of
		// one package. Keep the variant with the most syntax trees: it is a
		// superset of the other and avoids duplicate ids.
		if previous, ok := seen[pkg.PkgPath]; ok && previous >= len(pkg.Syntax) {
			continue
		}
		seen[pkg.PkgPath] = len(pkg.Syntax)
		roots = append(roots, pkg)
	}
	if packageErrors > 0 {
		notes = append(notes, fmt.Sprintf("%d package(s) reported load or type errors; extraction fell back to syntax where needed", packageErrors))
	}
	// Drop roots that lost the dedupe comparison.
	byPath := map[string]*packages.Package{}
	for _, pkg := range roots {
		if current, ok := byPath[pkg.PkgPath]; !ok || len(pkg.Syntax) > len(current.Syntax) {
			byPath[pkg.PkgPath] = pkg
		}
	}
	roots = roots[:0]
	for _, pkg := range byPath {
		roots = append(roots, pkg)
	}
	sort.Slice(roots, func(left, right int) bool {
		if roots[left].PkgPath != roots[right].PkgPath {
			return roots[left].PkgPath < roots[right].PkgPath
		}
		return roots[left].Dir < roots[right].Dir
	})
	return roots, packageErrors, notes, nil
}

// extractPackages turns loaded packages into symbol, edge and file rows.
func (i *Index) extractPackages(ctx context.Context, pkgs []*packages.Package, known map[string]bool) (batchResult, error) {
	x := newExtractor(i.root, i.module, known)
	batch := batchResult{Unresolved: map[string]int{}, Files: map[string]string{}}
	states := make([]*pkgSymbols, 0, len(pkgs))
	for _, pkg := range pkgs {
		if err := ctx.Err(); err != nil {
			return batchResult{}, err
		}
		state, ok := x.collectSymbols(pkg)
		if !ok {
			continue
		}
		x.pkgPaths[state.path] = true
		states = append(states, state)
		for rel := range state.files {
			batch.Files[rel] = state.path
		}
	}
	x.states = states
	for _, state := range states {
		x.collectEdges(state)
	}
	for _, state := range states {
		x.collectEdges(state)
	}
	sort.Slice(x.symbols, func(left, right int) bool {
		a, b := x.symbols[left], x.symbols[right]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.StartLine != b.StartLine {
			return a.StartLine < b.StartLine
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
	edges := make([]edgeRow, 0, len(x.edges))
	for _, edge := range x.edges {
		edges = append(edges, edge)
	}
	sort.Slice(edges, func(left, right int) bool {
		a, b := edges[left], edges[right]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Line < b.Line
	})
	batch.Symbols = x.symbols
	batch.Edges = edges
	batch.Unresolved = x.unres
	return batch, nil
}

// batchResult is one extraction batch ready to be written.
type batchResult struct {
	Symbols    []symbolRow
	Edges      []edgeRow
	Unresolved map[string]int
	Files      map[string]string
}

// collectSymbols walks the declarations of one package.
func (x *extractor) collectSymbols(pkg *packages.Package) (*pkgSymbols, bool) {
	state := &pkgSymbols{
		pkg:          pkg,
		path:         pkg.PkgPath,
		name:         pkg.Name,
		packageID:    PackageID(pkg.PkgPath),
		files:        map[string]*ast.File{},
		fileImports:  map[string]map[string]string{},
		ranges:       map[string][]symbolRange{},
		names:        map[string]string{},
		methodByName: map[string][]string{},
		typeLines:    map[string]int{},
	}
	paths := make([]string, 0, len(pkg.Syntax))
	for _, file := range pkg.Syntax {
		rel, ok := x.relPath(pkg.Fset.Position(file.Pos()).Filename)
		if !ok {
			continue
		}
		state.files[rel] = file
		paths = append(paths, rel)
	}
	if len(paths) == 0 {
		return nil, false
	}
	sort.Strings(paths)
	state.packageSym = symbolRow{
		ID:        state.packageID,
		Path:      paths[0],
		Package:   pkg.PkgPath,
		Kind:      "package",
		Name:      pkg.Name,
		QualName:  x.displayName(pkg.PkgPath, pkg.Name),
		StartLine: 1,
		EndLine:   1,
		Signature: "package " + pkg.Name,
	}
	x.addSymbol(state, state.packageSym)
	for _, rel := range paths {
		x.collectFileSymbols(state, pkg, rel, state.files[rel])
	}
	for rel := range state.ranges {
		sort.Slice(state.ranges[rel], func(left, right int) bool {
			return state.ranges[rel][left].start < state.ranges[rel][right].start
		})
	}
	return state, true
}

// collectFileSymbols adds the declarations of one file.
func (x *extractor) collectFileSymbols(state *pkgSymbols, pkg *packages.Package, rel string, file *ast.File) {
	state.fileImports[rel] = packageImports(file)
	for _, decl := range file.Decls {
		switch node := decl.(type) {
		case *ast.FuncDecl:
			x.collectFunc(state, rel, node)
		case *ast.GenDecl:
			x.collectGenDecl(state, rel, node)
		}
	}
}

// collectFunc adds a function or method declaration.
func (x *extractor) collectFunc(state *pkgSymbols, rel string, decl *ast.FuncDecl) {
	if decl.Name == nil || decl.Name.Name == "_" {
		return
	}
	kind := "func"
	typeName := ""
	if decl.Recv != nil && len(decl.Recv.List) > 0 {
		kind = "method"
		typeName = receiverBaseName(decl.Recv.List[0].Type)
	}
	name := decl.Name.Name
	if kind == "func" && name == "init" {
		name, typeName = x.uniqueInitName(state), ""
	}
	row := symbolRow{
		ID:        SymbolID(state.path, x.qual(state.path, state.name, typeName, name)),
		Path:      rel,
		Package:   state.path,
		Kind:      kind,
		Name:      name,
		QualName:  x.qual(state.path, state.name, typeName, name),
		StartLine: x.line(pkgFset(state), decl.Pos()),
		EndLine:   x.line(pkgFset(state), decl.End()),
		Signature: truncateRunes(collapseSpace(funcSignature(pkgFset(state), decl)), maxSignatureRunes),
		Doc:       docText(decl.Doc),
		Exported:  ast.IsExported(decl.Name.Name),
	}
	x.addSymbol(state, row)
}

// uniqueInitName gives each init function of a package a stable qualname.
func (x *extractor) uniqueInitName(state *pkgSymbols) string {
	base := "init"
	for suffix := 1; ; suffix++ {
		candidate := base
		if suffix > 1 {
			candidate = fmt.Sprintf("init#%d", suffix)
		}
		if _, exists := state.names[candidate]; !exists {
			return candidate
		}
	}
}

// collectGenDecl adds type, const and var declarations.
func (x *extractor) collectGenDecl(state *pkgSymbols, rel string, decl *ast.GenDecl) {
	fset := pkgFset(state)
	for _, spec := range decl.Specs {
		switch node := spec.(type) {
		case *ast.TypeSpec:
			if node.Name == nil || node.Name.Name == "_" {
				continue
			}
			name := node.Name.Name
			row := symbolRow{
				ID:        SymbolID(state.path, x.qual(state.path, state.name, "", name)),
				Path:      rel,
				Package:   state.path,
				Kind:      "type",
				Name:      name,
				QualName:  x.qual(state.path, state.name, "", name),
				StartLine: x.line(fset, node.Pos()),
				EndLine:   x.line(fset, node.End()),
				Signature: truncateRunes("type "+name+" "+printNode(fset, node.Type), maxSignatureRunes),
				Doc:       docText(firstDoc(decl.Doc, node.Doc)),
				Exported:  node.Name.IsExported(),
			}
			x.addSymbol(state, row)
			state.typeLines[name] = row.StartLine
			x.collectInterfaceMethods(state, rel, node, row)
		case *ast.ValueSpec:
			kind := "var"
			if decl.Tok == token.CONST {
				kind = "const"
			}
			x.collectValueSpec(state, rel, decl, node, kind)
		}
	}
}

// collectValueSpec adds each named constant or variable. A blank identifier
// and a local declaration have no symbol.
func (x *extractor) collectValueSpec(state *pkgSymbols, rel string, decl *ast.GenDecl, spec *ast.ValueSpec, kind string) {
	fset := pkgFset(state)
	for _, ident := range spec.Names {
		if ident == nil || ident.Name == "_" {
			continue
		}
		signature := kind + " " + ident.Name
		if spec.Type != nil {
			signature += " " + printNode(fset, spec.Type)
		} else if len(spec.Values) > 0 {
			values := make([]string, 0, len(spec.Values))
			for _, value := range spec.Values {
				values = append(values, printNode(fset, value))
			}
			signature += " = " + strings.Join(values, ", ")
		}
		row := symbolRow{
			ID:        SymbolID(state.path, x.qual(state.path, state.name, "", ident.Name)),
			Path:      rel,
			Package:   state.path,
			Kind:      kind,
			Name:      ident.Name,
			QualName:  x.qual(state.path, state.name, "", ident.Name),
			StartLine: x.line(fset, ident.Pos()),
			EndLine:   x.line(fset, spec.End()),
			Signature: truncateRunes(collapseSpace(signature), maxSignatureRunes),
			Doc:       docText(firstDoc(decl.Doc, spec.Doc)),
			Exported:  ast.IsExported(ident.Name),
		}
		x.addSymbol(state, row)
	}
}

// collectInterfaceMethods adds the explicit methods of an interface type so a
// call through the interface resolves to a real symbol.
func (x *extractor) collectInterfaceMethods(state *pkgSymbols, rel string, spec *ast.TypeSpec, typeRow symbolRow) {
	ifaceType, ok := spec.Type.(*ast.InterfaceType)
	if !ok {
		return
	}
	info := state.pkg.TypesInfo
	if info == nil {
		return
	}
	object, ok := info.Defs[spec.Name]
	if !ok {
		return
	}
	typeName, ok := object.(*types.TypeName)
	if !ok {
		return
	}
	iface, ok := typeName.Type().Underlying().(*types.Interface)
	if !ok {
		return
	}
	iface.Complete()
	fset := pkgFset(state)
	fields := map[string]*ast.Field{}
	if ifaceType.Methods != nil {
		for _, field := range ifaceType.Methods.List {
			for _, name := range field.Names {
				fields[name.Name] = field
			}
		}
	}
	for index := 0; index < iface.NumExplicitMethods(); index++ {
		method := iface.ExplicitMethod(index)
		if method == nil {
			continue
		}
		field := fields[method.Name()]
		start, end := typeRow.StartLine, typeRow.StartLine
		doc := ""
		if field != nil {
			start = x.line(fset, field.Pos())
			end = x.line(fset, field.End())
			doc = docText(field.Doc)
		}
		signatureType, _ := method.Type().(*types.Signature)
		signature := method.Name()
		if signatureType != nil {
			plain := types.NewSignatureType(nil, nil, nil, signatureType.Params(), signatureType.Results(), signatureType.Variadic())
			signature = strings.TrimPrefix(types.TypeString(plain, packageQualifier), "func")
		}
		row := symbolRow{
			ID:        SymbolID(state.path, x.qual(state.path, state.name, spec.Name.Name, method.Name())),
			Path:      rel,
			Package:   state.path,
			Kind:      "method",
			Name:      method.Name(),
			QualName:  x.qual(state.path, state.name, spec.Name.Name, method.Name()),
			StartLine: start,
			EndLine:   end,
			Signature: truncateRunes(collapseSpace("func ("+spec.Name.Name+") "+signature), maxSignatureRunes),
			Doc:       doc,
			Exported:  method.Exported(),
		}
		x.addSymbol(state, row)
	}
}

// addSymbol records one symbol and its lookup entries.
func (x *extractor) addSymbol(state *pkgSymbols, row symbolRow) {
	if row.ID == "" {
		return
	}
	if x.newIDs[row.ID] {
		return
	}
	row.Signature = sanitizeComment(row.Signature)
	row.Doc = sanitizeComment(row.Doc)
	x.newIDs[row.ID] = true
	x.symbols = append(x.symbols, row)
	if row.Path == "" || row.Kind == "package" {
		return
	}
	state.ranges[row.Path] = append(state.ranges[row.Path], symbolRange{
		start: row.StartLine,
		end:   row.EndLine,
		id:    row.ID,
	})
	if row.Kind == "method" {
		state.methodByName[row.Name] = append(state.methodByName[row.Name], row.ID)
		return
	}
	if _, exists := state.names[row.Name]; !exists {
		state.names[row.Name] = row.ID
	}
}

// collectEdges resolves every dependency edge of one package.
func (x *extractor) collectEdges(state *pkgSymbols) {
	x.collectImports(state)
	x.collectEmbeds(state)
	if state.pkg.TypesInfo != nil && state.pkg.Types != nil {
		x.collectTypeEdges(state)
		x.collectImplements(state)
		return
	}
	x.collectSyntacticEdges(state)
}

// collectImports records package-to-package imports.
func (x *extractor) collectImports(state *pkgSymbols) {
	fset := pkgFset(state)
	for _, file := range state.files {
		for _, spec := range file.Imports {
			path := importPath(spec)
			if path == "" || !x.shouldIndexPkg(path) {
				continue
			}
			target := PackageID(path)
			if !x.hasSymbol(target) {
				continue
			}
			x.addEdge(state.packageID, target, "imports", x.line(fset, spec.Pos()), 1.0, "types")
		}
	}
}

// collectEmbeds records struct and interface embedding.
func (x *extractor) collectEmbeds(state *pkgSymbols) {
	info := state.pkg.TypesInfo
	if info == nil {
		return
	}
	fset := pkgFset(state)
	for _, file := range state.files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				from := SymbolID(state.path, x.qual(state.path, state.name, "", typeSpec.Name.Name))
				if !x.hasSymbol(from) {
					continue
				}
				switch node := typeSpec.Type.(type) {
				case *ast.StructType:
					for _, field := range node.Fields.List {
						if len(field.Names) != 0 {
							continue
						}
						target, ok := x.embeddedTarget(info, field.Type)
						if !ok {
							continue
						}
						x.addEdge(from, target, "embeds", x.line(fset, field.Pos()), 1.0, "types")
					}
				case *ast.InterfaceType:
					targets := x.interfaceEmbeds(typeSpec, info)
					for _, target := range targets {
						x.addEdge(from, target, "embeds", x.line(fset, typeSpec.Pos()), 1.0, "types")
					}
				}
			}
		}
	}
}

// interfaceEmbeds resolves the embedded types of one interface declaration.
func (x *extractor) interfaceEmbeds(spec *ast.TypeSpec, info *types.Info) []string {
	object, ok := info.Defs[spec.Name]
	if !ok {
		return nil
	}
	typeName, ok := object.(*types.TypeName)
	if !ok {
		return nil
	}
	iface, ok := typeName.Type().Underlying().(*types.Interface)
	if !ok {
		return nil
	}
	iface.Complete()
	out := make([]string, 0, 2)
	for index := 0; index < iface.NumEmbeddeds(); index++ {
		out = append(out, x.embeddedTypeIDs(iface.EmbeddedType(index))...)
	}
	return out
}

// embeddedTarget resolves a struct field's type expression.
func (x *extractor) embeddedTarget(info *types.Info, expr ast.Expr) (string, bool) {
	for {
		switch node := expr.(type) {
		case *ast.StarExpr:
			expr = node.X
		case *ast.ParenExpr:
			expr = node.X
		case *ast.IndexExpr:
			expr = node.X
		case *ast.IndexListExpr:
			expr = node.X
		default:
			goto resolved
		}
	}
resolved:
	var ident *ast.Ident
	switch node := expr.(type) {
	case *ast.Ident:
		ident = node
	case *ast.SelectorExpr:
		ident = node.Sel
	default:
		return "", false
	}
	object, ok := info.Uses[ident]
	if !ok {
		return "", false
	}
	return x.objectSymbol(object)
}

// embeddedTypeIDs resolves an embedded interface term to indexed type ids.
func (x *extractor) embeddedTypeIDs(embedded types.Type) []string {
	embedded = types.Unalias(embedded)
	switch node := embedded.(type) {
	case *types.Named:
		if id, ok := x.objectSymbol(node.Obj()); ok {
			return []string{id}
		}
	case *types.Pointer:
		return x.embeddedTypeIDs(node.Elem())
	case *types.Union:
		out := make([]string, 0, node.Len())
		for index := 0; index < node.Len(); index++ {
			out = append(out, x.embeddedTypeIDs(node.Term(index).Type())...)
		}
		return out
	}
	return nil
}

// collectTypeEdges resolves every identifier use through go/types.
func (x *extractor) collectTypeEdges(state *pkgSymbols) {
	info := state.pkg.TypesInfo
	fset := pkgFset(state)
	callMarks := map[*ast.Ident]bool{}
	for _, file := range state.files {
		markCalls(file, callMarks)
	}
	for ident, object := range info.Uses {
		if object == nil || ident == nil {
			continue
		}
		position := fset.Position(ident.Pos())
		rel, ok := x.relPath(position.Filename)
		if !ok {
			continue
		}
		if _, ok := state.files[rel]; !ok {
			continue
		}
		from := state.enclosing(rel, position.Line)
		if from == "" {
			continue
		}
		target, ok := x.objectSymbol(object)
		if !ok {
			if x.indexableObject(object) {
				x.unres[rel]++
			}
			continue
		}
		kind := "references"
		if callMarks[ident] {
			if _, isFunc := object.(*types.Func); isFunc {
				kind = "calls"
			}
		}
		if target == from {
			continue
		}
		x.addEdge(from, target, kind, position.Line, 1.0, "types")
	}
}

// collectSyntacticEdges is the fallback for a package whose types did not
// load. Calls are resolved by package name with lower confidence.
func (x *extractor) collectSyntacticEdges(state *pkgSymbols) {
	fset := pkgFset(state)
	for rel, file := range state.files {
		imports := state.fileImports[rel]
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			line := x.line(fset, call.Pos())
			from := state.enclosing(rel, line)
			if from == "" {
				return true
			}
			switch fun := unwrapTypeExpr(call.Fun).(type) {
			case *ast.Ident:
				if target, ok := state.names[fun.Name]; ok && target != from {
					x.addEdge(from, target, "calls", line, 0.8, "syntax")
					return true
				}
				x.unres[rel]++
			case *ast.SelectorExpr:
				if base, ok := fun.X.(*ast.Ident); ok {
					if path := imports[base.Name]; path != "" && x.shouldIndexPkg(path) {
						if target, ok := x.syntacticTarget(path, fun.Sel.Name); ok && target != from {
							x.addEdge(from, target, "calls", line, 0.8, "syntax")
							return true
						}
					}
				}
				if ids := state.methodByName[fun.Sel.Name]; len(ids) == 1 && ids[0] != from {
					x.addEdge(from, ids[0], "calls", line, 0.5, "syntax")
				}
			}
			return true
		})
	}
}

// syntacticTarget builds the deterministic id of a cross-package call.
func (x *extractor) syntacticTarget(pkgPath, name string) (string, bool) {
	pkgName := pathBase(pkgPath)
	id := SymbolID(pkgPath, x.qual(pkgPath, pkgName, "", name))
	return id, x.hasSymbol(id)
}

// collectImplements records concrete types satisfying interfaces. Candidate
// interfaces come from the package and its transitive imports; only exported
// interfaces of a dependency can be implemented outside it, so the unexported
// surface of a dependency is not scanned.
func (x *extractor) collectImplements(state *pkgSymbols) {
	info := state.pkg.TypesInfo
	if info == nil || state.pkg.Types == nil || state.pkg.Types.Scope() == nil {
		return
	}
	if x.ifaces == nil {
		x.ifaces = x.buildInterfaceIndex()
	}
	if x.ifaces.empty() {
		return
	}
	scope := state.pkg.Types.Scope()
	for _, name := range scope.Names() {
		object, ok := scope.Lookup(name).(*types.TypeName)
		if !ok || object.IsAlias() {
			continue
		}
		named, ok := object.Type().(*types.Named)
		if !ok || named.TypeParams().Len() > 0 {
			continue
		}
		if _, isInterface := named.Underlying().(*types.Interface); isInterface {
			continue
		}
		from := SymbolID(state.path, x.qual(state.path, state.name, "", name))
		if !x.hasSymbol(from) {
			continue
		}
		line := state.typeLines[name]
		for _, candidate := range x.ifaces.candidates(methodSetNames(named)) {
			if types.Implements(named, candidate.iface) || types.Implements(types.NewPointer(named), candidate.iface) {
				x.addEdge(from, candidate.id, "implements", line, 1.0, "types")
			}
		}
	}
}

// interfaceRef is one candidate interface of the implements check.
type interfaceRef struct {
	id      string
	iface   *types.Interface
	methods map[string]bool
	count   int
}

// interfaceIndex buckets interfaces by their first method name. A concrete
// type can satisfy an interface only if it has that method, so the full
// types.Implements check runs on a small candidate set instead of every
// interface of every dependency.
type interfaceIndex struct {
	byMethod map[string][]interfaceRef
	total    int
}

func (x *interfaceIndex) empty() bool { return x == nil || x.total == 0 }

// candidates returns the interfaces whose method-name set may be satisfied by
// the given method set.
func (x *interfaceIndex) candidates(methods map[string]bool) []interfaceRef {
	if x == nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]interfaceRef, 0, 8)
	for name := range methods {
		for _, candidate := range x.byMethod[name] {
			if seen[candidate.id] {
				continue
			}
			seen[candidate.id] = true
			out = append(out, candidate)
		}
	}
	return out
}

// buildInterfaceIndex collects interfaces from every package in the batch and
// its transitive imports.
func (x *extractor) buildInterfaceIndex() *interfaceIndex {
	index := &interfaceIndex{byMethod: map[string][]interfaceRef{}}
	visited := map[string]bool{}
	var addPackage func(pkg *types.Package, exportedOnly bool)
	addPackage = func(pkg *types.Package, exportedOnly bool) {
		if pkg == nil || visited[pkg.Path()] {
			return
		}
		visited[pkg.Path()] = true
		if scope := pkg.Scope(); scope != nil {
			for _, name := range scope.Names() {
				object, ok := scope.Lookup(name).(*types.TypeName)
				if !ok || object.IsAlias() {
					continue
				}
				if exportedOnly && !object.Exported() {
					continue
				}
				if !x.shouldIndexPkg(pkg.Path()) {
					continue
				}
				iface, ok := object.Type().Underlying().(*types.Interface)
				if !ok {
					continue
				}
				iface.Complete()
				count := iface.NumMethods()
				if count == 0 || count > maxInterfaceMethods {
					continue
				}
				qual := x.qual(pkg.Path(), pkg.Name(), "", name)
				id := SymbolID(pkg.Path(), qual)
				if !x.hasSymbol(id) {
					continue
				}
				methods := make(map[string]bool, count)
				first := ""
				for methodIndex := 0; methodIndex < count; methodIndex++ {
					methodName := iface.Method(methodIndex).Name()
					methods[methodName] = true
					if first == "" || methodName < first {
						first = methodName
					}
				}
				index.byMethod[first] = append(index.byMethod[first], interfaceRef{
					id:      id,
					iface:   iface,
					methods: methods,
					count:   count,
				})
				index.total++
			}
		}
		for _, imported := range pkg.Imports() {
			addPackage(imported, true)
		}
	}
	for _, state := range x.states {
		addPackage(state.pkg.Types, false)
	}
	return index
}

// maxInterfaceMethods bounds the interfaces considered for implements edges.
// A wider interface is usually a marker or a generated port; checking every
// type against it costs more than the edge is worth.
const maxInterfaceMethods = 12

// addEdge records one deduplicated edge. A repeated site keeps the highest
// confidence.
func (x *extractor) addEdge(source, target, kind string, line int, confidence float64, provenance string) {
	if source == "" || target == "" || source == target {
		return
	}
	key := source + "\x00" + target + "\x00" + kind + "\x00" + strconv.Itoa(line)
	if existing, ok := x.edges[key]; ok {
		if confidence > existing.Confidence {
			existing.Confidence = confidence
			existing.Provenance = provenance
			x.edges[key] = existing
		}
		return
	}
	x.edges[key] = edgeRow{
		Source:     source,
		Target:     target,
		Kind:       kind,
		Line:       line,
		Confidence: confidence,
		Provenance: provenance,
	}
}

// objectSymbol maps a go/types object to the id of its indexed definition.
func (x *extractor) objectSymbol(object types.Object) (string, bool) {
	if object == nil || object.Pkg() == nil {
		return "", false
	}
	pkgPath := object.Pkg().Path()
	if !x.shouldIndexPkg(pkgPath) {
		return "", false
	}
	pkgName := object.Pkg().Name()
	switch node := object.(type) {
	case *types.Func:
		fn := node
		if origin := node.Origin(); origin != nil {
			fn = origin
		}
		signature, _ := fn.Type().(*types.Signature)
		typeName := ""
		if signature != nil && signature.Recv() != nil {
			typeName = receiverName(signature.Recv().Type())
		}
		if typeName == "" && fn.Parent() != fn.Pkg().Scope() {
			return "", false
		}
		id := SymbolID(pkgPath, x.qual(pkgPath, pkgName, typeName, fn.Name()))
		return id, x.hasSymbol(id)
	case *types.TypeName:
		if node.Parent() != node.Pkg().Scope() {
			return "", false
		}
		id := SymbolID(pkgPath, x.qual(pkgPath, pkgName, "", node.Name()))
		return id, x.hasSymbol(id)
	case *types.Const:
		if node.Parent() != node.Pkg().Scope() {
			return "", false
		}
		id := SymbolID(pkgPath, x.qual(pkgPath, pkgName, "", node.Name()))
		return id, x.hasSymbol(id)
	case *types.Var:
		if node.Parent() != node.Pkg().Scope() {
			return "", false
		}
		id := SymbolID(pkgPath, x.qual(pkgPath, pkgName, "", node.Name()))
		return id, x.hasSymbol(id)
	default:
		return "", false
	}
}

// shouldIndexPkg reports whether a package path belongs to the indexed module.
func (x *extractor) shouldIndexPkg(path string) bool {
	if path == "" {
		return false
	}
	if x.module != "" {
		return path == x.module || strings.HasPrefix(path, x.module+"/")
	}
	return x.pkgPaths[path]
}

// indexableObject reports whether an object should have become a symbol. A
// local variable, a parameter and a struct field are not indexed, so their
// uses are not unresolved references.
func (x *extractor) indexableObject(object types.Object) bool {
	if object == nil || object.Pkg() == nil || !x.shouldIndexPkg(object.Pkg().Path()) {
		return false
	}
	switch node := object.(type) {
	case *types.Func:
		if node.Parent() == nil {
			return true // a method; a local function is not a Go declaration
		}
		return node.Parent() == node.Pkg().Scope()
	case *types.TypeName:
		return node.Parent() == node.Pkg().Scope()
	case *types.Const:
		return node.Parent() == node.Pkg().Scope()
	case *types.Var:
		return node.Parent() == node.Pkg().Scope()
	default:
		return false
	}
}

// hasSymbol reports whether an id exists in the store or this batch.
func (x *extractor) hasSymbol(id string) bool {
	return x.newIDs[id] || x.known[id]
}

// displayName trims the module prefix from a package path.
func (x *extractor) displayName(pkgPath, pkgName string) string {
	if x.module != "" {
		if pkgPath == x.module {
			return pkgName
		}
		if strings.HasPrefix(pkgPath, x.module+"/") {
			return strings.TrimPrefix(pkgPath, x.module+"/")
		}
	}
	return pkgPath
}

// qual builds the qualified name of one symbol.
func (x *extractor) qual(pkgPath, pkgName, typeName, name string) string {
	prefix := x.displayName(pkgPath, pkgName)
	if typeName != "" {
		return prefix + "." + typeName + "." + name
	}
	return prefix + "." + name
}

func (x *extractor) line(fset *token.FileSet, pos token.Pos) int {
	if fset == nil || !pos.IsValid() {
		return 0
	}
	return fset.Position(pos).Line
}

func (x *extractor) relPath(absolute string) (string, bool) {
	rel, ok := relUnder(x.root, absolute)
	return rel, ok
}

// relUnder converts an absolute file path to a checkout-relative POSIX path.
func relUnder(root, absolute string) (string, bool) {
	if absolute == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, filepath.Clean(absolute))
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// packageImports maps the local import name of one file to its package path.
func packageImports(file *ast.File) map[string]string {
	out := make(map[string]string, len(file.Imports))
	for _, spec := range file.Imports {
		path := importPath(spec)
		if path == "" {
			continue
		}
		name := pathBase(path)
		if spec.Name != nil && spec.Name.Name != "" {
			name = spec.Name.Name
		}
		if name == "_" || name == "." {
			continue
		}
		out[name] = path
	}
	return out
}

// importPath unquotes an import spec path.
func importPath(spec *ast.ImportSpec) string {
	if spec == nil || spec.Path == nil {
		return ""
	}
	value, err := strconv.Unquote(spec.Path.Value)
	if err != nil {
		return ""
	}
	return value
}

// pathBase is the last segment of an import path.
func pathBase(path string) string {
	if index := strings.LastIndex(path, "/"); index >= 0 {
		return path[index+1:]
	}
	return path
}

// receiverBaseName returns the base type name of a receiver or embedded term.
func receiverBaseName(expr ast.Expr) string {
	switch node := expr.(type) {
	case *ast.Ident:
		return node.Name
	case *ast.StarExpr:
		return receiverBaseName(node.X)
	case *ast.IndexExpr:
		return receiverBaseName(node.X)
	case *ast.IndexListExpr:
		return receiverBaseName(node.X)
	case *ast.ParenExpr:
		return receiverBaseName(node.X)
	}
	return ""
}

// receiverName returns the named base of a go/types receiver type.
func receiverName(t types.Type) string {
	t = types.Unalias(t)
	switch node := t.(type) {
	case *types.Pointer:
		return receiverName(node.Elem())
	case *types.Named:
		return node.Obj().Name()
	case *types.TypeParam:
		return node.Obj().Name()
	}
	return ""
}

// methodSetNames returns the union of the value and pointer method names.
func methodSetNames(named *types.Named) map[string]bool {
	out := map[string]bool{}
	add := func(set *types.MethodSet) {
		for index := 0; index < set.Len(); index++ {
			out[set.At(index).Obj().Name()] = true
		}
	}
	add(types.NewMethodSet(named))
	add(types.NewMethodSet(types.NewPointer(named)))
	return out
}

// markCalls records every identifier that is the callee of a call expression.
func markCalls(file *ast.File, marks map[*ast.Ident]bool) {
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := unwrapTypeExpr(call.Fun).(type) {
		case *ast.Ident:
			marks[fun] = true
		case *ast.SelectorExpr:
			marks[fun.Sel] = true
		}
		return true
	})
}

// unwrapTypeExpr removes parentheses and generic instantiation around an
// expression.
func unwrapTypeExpr(expr ast.Expr) ast.Expr {
	for {
		switch node := expr.(type) {
		case *ast.ParenExpr:
			expr = node.X
		case *ast.IndexExpr:
			expr = node.X
		case *ast.IndexListExpr:
			expr = node.X
		default:
			return expr
		}
	}
}

// printNode renders one AST node on a single line.
func printNode(fset *token.FileSet, node ast.Node) string {
	if node == nil {
		return ""
	}
	var buffer bytes.Buffer
	if err := printer.Fprint(&buffer, fset, node); err != nil {
		return ""
	}
	return collapseSpace(buffer.String())
}

// funcSignature renders a function declaration without its body.
func funcSignature(fset *token.FileSet, decl *ast.FuncDecl) string {
	receiver := ""
	if decl.Recv != nil && len(decl.Recv.List) > 0 {
		receiver = " (" + printNode(fset, decl.Recv.List[0].Type) + ")"
	}
	rest := strings.TrimPrefix(printNode(fset, decl.Type), "func")
	return "func" + receiver + " " + decl.Name.Name + rest
}

// collapseSpace joins whitespace-separated fields with one space.
func collapseSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// docText normalizes a doc comment and caps its stored size.
func docText(group *ast.CommentGroup) string {
	if group == nil {
		return ""
	}
	return truncateRunes(strings.TrimSpace(group.Text()), maxDocRunes)
}

// firstDoc returns the first non-nil doc group.
func firstDoc(groups ...*ast.CommentGroup) *ast.CommentGroup {
	for _, group := range groups {
		if group != nil {
			return group
		}
	}
	return nil
}

// packageQualifier renders type names with their package name.
func packageQualifier(pkg *types.Package) string {
	if pkg == nil {
		return ""
	}
	return pkg.Name()
}

// PackageID is the deterministic id of one package node.
func PackageID(pkgPath string) string {
	digest := sha256.Sum256([]byte(pkgPath))
	return "pkg_" + hex.EncodeToString(digest[:])[:16]
}

// SymbolID is the deterministic id of one symbol. It is derived from the
// package path and the qualified name, never from a line number, so an edit
// that only shifts lines keeps every incoming edge valid.
func SymbolID(pkgPath, qualname string) string {
	digest := sha256.Sum256([]byte(pkgPath + "\x00" + qualname))
	return "sym_" + hex.EncodeToString(digest[:])[:20]
}

// enclosing returns the innermost symbol of a file that contains line.
func (s *pkgSymbols) enclosing(rel string, line int) string {
	ranges := s.ranges[rel]
	if len(ranges) == 0 {
		return s.packageID
	}
	// Binary search for the last range that starts at or before line, then
	// walk back a bounded distance. Declarations do not nest deeply, so the
	// containing range is found immediately in practice.
	low, high := 0, len(ranges)
	for low < high {
		mid := (low + high) / 2
		if ranges[mid].start <= line {
			low = mid + 1
		} else {
			high = mid
		}
	}
	best := ""
	bestSpan := 0
	for index := low - 1; index >= 0 && index >= low-64; index-- {
		candidate := ranges[index]
		if candidate.end < line || candidate.start > line {
			if candidate.end < line && best != "" {
				break
			}
			continue
		}
		span := candidate.end - candidate.start
		if best == "" || span < bestSpan {
			best = candidate.id
			bestSpan = span
		}
	}
	if best != "" {
		return best
	}
	return s.packageID
}

// pkgFset returns the package file set.
func pkgFset(state *pkgSymbols) *token.FileSet {
	if state == nil || state.pkg == nil {
		return nil
	}
	return state.pkg.Fset
}
