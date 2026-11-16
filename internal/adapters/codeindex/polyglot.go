package codeindex

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// polyglotBatch is the extracted TypeScript/JavaScript half of one index run.
type polyglotBatch struct {
	Symbols    []symbolRow
	Edges      map[string]edgeRow
	Unresolved map[string]int
	Files      []*polyglotFile
	Partial    int
}

// polyglotFile is one parsed script file and its resolved lookup tables.
type polyglotFile struct {
	path       string
	moduleID   string
	lines      int
	python     bool
	symbols    []symbolRow
	symbolByID map[string]symbolRow
	bindings   map[string]polyglotBinding
	ranges     []byteSpan
	result     syntaxResult
}

// polyglotBinding is one local import name bound to an indexed module.
type polyglotBinding struct {
	Target   string
	Imported string
}

// byteSpan is one symbol's byte range inside one file.
type byteSpan struct {
	start uint32
	end   uint32
	id    string
}

// polyglotSymbolRef is the resolution surface of one indexed definition.
type polyglotSymbolRef struct {
	id        string
	kind      string
	signature string
	exported  bool
	defaulted bool
}

// polyglotResolver maps a repository path to the definitions another file can
// import. A full index fills it from the current batch; an incremental sync
// seeds it from the store and replaces the re-extracted files. byID answers an
// exact qualified-name lookup for Python `self.method()` sites, which need the
// enclosing class rather than a bare method name.
type polyglotResolver struct {
	files     map[string]bool
	byPath    map[string]map[string][]polyglotSymbolRef
	byID      map[string]polyglotSymbolRef
	idsByPath map[string][]string
}

// newPolyglotResolver returns an empty resolver.
func newPolyglotResolver() *polyglotResolver {
	return &polyglotResolver{
		files:     map[string]bool{},
		byPath:    map[string]map[string][]polyglotSymbolRef{},
		byID:      map[string]polyglotSymbolRef{},
		idsByPath: map[string][]string{},
	}
}

// addFile replaces every definition known for one path.
func (r *polyglotResolver) addFile(file *polyglotFile) {
	r.removeFile(file.path)
	r.files[file.path] = true
	names := map[string][]polyglotSymbolRef{}
	for _, symbol := range file.symbols {
		if symbol.Kind == "module" {
			continue
		}
		ref := polyglotSymbolRef{
			id:        symbol.ID,
			kind:      symbol.Kind,
			signature: symbol.Signature,
			exported:  symbol.Exported,
			defaulted: strings.HasPrefix(symbol.Signature, "export default "),
		}
		names[symbol.Name] = append(names[symbol.Name], ref)
		r.byID[symbol.ID] = ref
		r.idsByPath[file.path] = append(r.idsByPath[file.path], symbol.ID)
	}
	r.byPath[file.path] = names
}

// removeFile forgets every definition of one path before it is re-extracted.
func (r *polyglotResolver) removeFile(filePath string) {
	delete(r.files, filePath)
	delete(r.byPath, filePath)
	for _, id := range r.idsByPath[filePath] {
		delete(r.byID, id)
	}
	delete(r.idsByPath, filePath)
}

// addBatch replaces the definitions of every file in the batch.
func (r *polyglotResolver) addBatch(files []*polyglotFile) {
	for _, file := range files {
		r.addFile(file)
	}
}

// findLocal resolves one name in one file, filtered by the expected kinds.
func (r *polyglotResolver) findLocal(file, name string, kinds ...string) (string, bool) {
	refs := r.byPath[file][name]
	var found polyglotSymbolRef
	count := 0
	for _, ref := range refs {
		if !kindMatches(ref.kind, kinds) {
			continue
		}
		found = ref
		count++
	}
	return found.id, count == 1
}

// findExported resolves one imported name in a target file. Only exported
// definitions are candidates, so an unexported implementation detail is never
// linked across a module boundary.
func (r *polyglotResolver) findExported(file, name string, kinds ...string) (string, bool) {
	refs := r.byPath[file][name]
	var found polyglotSymbolRef
	count := 0
	for _, ref := range refs {
		if !ref.exported || !kindMatches(ref.kind, kinds) {
			continue
		}
		found = ref
		count++
	}
	return found.id, count == 1
}

// findDefault resolves the single default export of a target file.
func (r *polyglotResolver) findDefault(file string, kinds ...string) (string, bool) {
	var found polyglotSymbolRef
	count := 0
	for _, refs := range r.byPath[file] {
		for _, ref := range refs {
			if !ref.defaulted || !kindMatches(ref.kind, kinds) {
				continue
			}
			found = ref
			count++
		}
	}
	return found.id, count == 1
}

// findQualified resolves one exact qualified name in one file. Python uses it
// for `self.method()` and `Class.method()` sites, where a bare-name match could
// pick a same-named method of an unrelated class.
func (r *polyglotResolver) findQualified(file, qual string, kinds ...string) (string, bool) {
	ref, ok := r.byID[SymbolID(file, qual)]
	if !ok || !kindMatches(ref.kind, kinds) {
		return "", false
	}
	return ref.id, true
}

// kindMatches reports whether a definition kind is in the allowed set.
func kindMatches(kind string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if kind == candidate {
			return true
		}
	}
	return false
}

// resolveSpecifier maps a relative module specifier to an indexed file. Bare
// package names, tsconfig aliases and symlinked paths are deliberately not
// guessed; an unresolved relative specifier becomes a file-level count.
func (r *polyglotResolver) resolveSpecifier(from, specifier string) (string, bool) {
	if !strings.HasPrefix(specifier, "./") && !strings.HasPrefix(specifier, "../") {
		return "", false
	}
	base := path.Clean(path.Join(path.Dir(from), specifier))
	if base == ".." || strings.HasPrefix(base, "../") || strings.HasPrefix(base, "/") {
		return "", false
	}
	if r.files[base] {
		return base, true
	}
	extensions := []string{".ts", ".tsx", ".js", ".jsx"}
	if extension := path.Ext(base); extension != "" {
		stem := strings.TrimSuffix(base, extension)
		for _, candidate := range extensions {
			if r.files[stem+candidate] {
				return stem + candidate, true
			}
		}
		return "", false
	}
	for _, extension := range extensions {
		if r.files[base+extension] {
			return base + extension, true
		}
	}
	for _, extension := range extensions {
		index := path.Join(base, "index"+extension)
		if r.files[index] {
			return index, true
		}
	}
	return "", false
}

// extractPolyglot parses every supported script file of one batch and resolves
// the edges that can be stated from syntax alone. The resolver must already
// contain the files this batch does not re-extract.
func (i *Index) extractPolyglot(ctx context.Context, states map[string]fileState, resolver *polyglotResolver) (polyglotBatch, error) {
	batch := polyglotBatch{Edges: map[string]edgeRow{}, Unresolved: map[string]int{}}
	paths := make([]string, 0, len(states))
	for filePath := range states {
		if polyglotPathIndexable(filePath) && IsPolyglotPath(filePath) {
			paths = append(paths, filePath)
		}
	}
	sort.Strings(paths)
	if resolver == nil {
		resolver = newPolyglotResolver()
	}
	for _, filePath := range paths {
		if err := ctx.Err(); err != nil {
			return polyglotBatch{}, err
		}
		spec, ok := polyglotSpecForPath(filePath)
		if !ok {
			continue
		}
		full := filepath.Join(i.root, filepath.FromSlash(filePath))
		source, err := os.ReadFile(full)
		if err != nil {
			return polyglotBatch{}, fmt.Errorf("codeindex: read %s: %w", filePath, err)
		}
		result, err := i.extractScript(spec, source)
		if err != nil {
			return polyglotBatch{}, fmt.Errorf("codeindex: extract %s: %w", filePath, err)
		}
		file := buildPolyglotFile(filePath, spec, result, source)
		batch.Files = append(batch.Files, file)
		batch.Symbols = append(batch.Symbols, file.symbols...)
		if result.Partial {
			batch.Partial++
		}
	}
	resolver.addBatch(batch.Files)
	for _, file := range batch.Files {
		i.resolvePolyglotFile(file, resolver, &batch)
	}
	sortPolyglotBatch(&batch)
	return batch, nil
}

// buildPolyglotFile converts syntax-level declarations into symbol rows and
// keeps the byte ranges that locate the symbol enclosing a reference.
func buildPolyglotFile(filePath string, spec polyglotSpec, result syntaxResult, source []byte) *polyglotFile {
	lines := strings.Count(string(source), "\n") + 1
	moduleID := SymbolID(filePath, filePath)
	file := &polyglotFile{
		path:       filePath,
		moduleID:   moduleID,
		lines:      lines,
		python:     spec.Language == pythonLanguage,
		bindings:   map[string]polyglotBinding{},
		symbolByID: map[string]symbolRow{},
		result:     result,
	}
	file.symbols = append(file.symbols, symbolRow{
		ID:        moduleID,
		Path:      filePath,
		Package:   polyglotDir(filePath),
		Kind:      "module",
		Name:      filePath,
		QualName:  filePath,
		StartLine: 1,
		EndLine:   lines,
		Signature: "module " + filePath,
	})
	byID := map[string]int{}
	spans := map[string]byteSpan{}
	for _, symbol := range result.Symbols {
		id := SymbolID(filePath, symbol.Qual)
		row := polyglotSymbolRow(filePath, symbol)
		span := byteSpan{start: symbol.StartByte, end: symbol.EndByte, id: id}
		if existing, ok := byID[id]; ok {
			previous := file.symbols[existing]
			replace := symbolPriority(row.Kind) > symbolPriority(previous.Kind)
			if symbolPriority(row.Kind) == symbolPriority(previous.Kind) {
				previousSpan := spans[id]
				replace = span.end-span.start > previousSpan.end-previousSpan.start
			}
			if replace {
				file.symbols[existing] = row
				spans[id] = span
			}
			continue
		}
		byID[id] = len(file.symbols)
		file.symbols = append(file.symbols, row)
		spans[id] = span
	}
	for _, symbol := range file.symbols {
		if symbol.Kind == "module" {
			continue
		}
		span := spans[symbol.ID]
		file.ranges = append(file.ranges, span)
		file.symbolByID[symbol.ID] = symbol
	}
	sort.Slice(file.ranges, func(left, right int) bool {
		if file.ranges[left].start != file.ranges[right].start {
			return file.ranges[left].start < file.ranges[right].start
		}
		return file.ranges[left].end < file.ranges[right].end
	})
	return file
}

// polyglotSymbolRow renders one declaration row. A default export is marked in
// the stored signature so an incremental resolver can find it without a new
// schema column.
func polyglotSymbolRow(filePath string, symbol syntaxSymbol) symbolRow {
	signature := symbol.Signature
	if symbol.Default {
		signature = "export default " + signature
	}
	return symbolRow{
		ID:        SymbolID(filePath, symbol.Qual),
		Path:      filePath,
		Package:   polyglotDir(filePath),
		Kind:      symbol.Kind,
		Name:      symbol.Name,
		QualName:  symbol.Qual,
		StartLine: symbol.StartLine,
		EndLine:   symbol.EndLine,
		Signature: truncateRunes(signature, maxSignatureRunes),
		Doc:       truncateRunes(symbol.Doc, maxDocRunes),
		Exported:  symbol.Exported,
	}
}

// resolvePolyglotFile turns the syntax facts of one file into edges.
func (i *Index) resolvePolyglotFile(file *polyglotFile, resolver *polyglotResolver, batch *polyglotBatch) {
	i.resolvePolyglotImports(file, resolver, batch)
	if file.python {
		i.resolvePythonFile(file, resolver, batch)
		return
	}
	for _, call := range file.result.Calls {
		source := file.enclosing(call.Byte)
		if call.New {
			if target, local, ok := file.resolveType(call.Name, resolver); ok && target != source {
				confidence := 0.4
				if local {
					confidence = 0.6
				}
				batch.addEdge(source, target, "references", call.Line, confidence)
			}
			continue
		}
		if call.Receiver != "" {
			if call.Receiver == "this" {
				if target, ok := resolver.findLocal(file.path, call.Name, "method"); ok && target != source {
					batch.addEdge(source, target, "calls", call.Line, 0.6)
				}
				continue
			}
			binding, imported := file.bindings[call.Receiver]
			if imported && binding.Imported == "*" {
				if target, ok := resolver.findExported(binding.Target, call.Name, "func"); ok {
					if target != source {
						batch.addEdge(source, target, "calls", call.Line, 0.4)
					}
				} else {
					batch.Unresolved[file.path]++
				}
			}
			continue
		}
		if binding, ok := file.bindings[call.Name]; ok {
			target, found := file.resolveBinding(binding, "func", resolver)
			if !found {
				batch.Unresolved[file.path]++
				continue
			}
			if target != source {
				batch.addEdge(source, target, "calls", call.Line, 0.5)
			}
			continue
		}
		if target, ok := resolver.findLocal(file.path, call.Name, "func"); ok && target != source {
			batch.addEdge(source, target, "calls", call.Line, 0.6)
		}
	}
	for _, heritage := range file.result.Heritage {
		source := file.enclosing(heritage.Byte)
		target, local, ok := file.resolveType(heritage.Target, resolver)
		if !ok || target == source {
			continue
		}
		confidence := 0.5
		if local {
			confidence = 0.7
		}
		batch.addEdge(source, target, heritage.Kind, heritage.Line, confidence)
	}
	seenRefs := map[string]bool{}
	for _, reference := range file.result.TypeRefs {
		source := file.enclosing(reference.Byte)
		target, local, ok := file.resolveType(reference.Name, resolver)
		if !ok || target == source {
			continue
		}
		key := source + "\x00" + target
		if seenRefs[key] {
			continue
		}
		seenRefs[key] = true
		confidence := 0.4
		if local {
			confidence = 0.5
		}
		batch.addEdge(source, target, "references", reference.Line, confidence)
	}
}

// resolvePolyglotImports turns import statements into module edges and local
// bindings. TypeScript/JavaScript resolve relative specifiers; Python resolves
// dotted and relative module paths and rebinds `from pkg import submodule` to
// the submodule file.
func (i *Index) resolvePolyglotImports(file *polyglotFile, resolver *polyglotResolver, batch *polyglotBatch) {
	for _, item := range file.result.Imports {
		if file.python {
			i.resolvePythonImport(file, item, resolver, batch)
			continue
		}
		target, ok := resolver.resolveSpecifier(file.path, item.Specifier)
		if !ok {
			if strings.HasPrefix(item.Specifier, "./") || strings.HasPrefix(item.Specifier, "../") {
				batch.Unresolved[file.path]++
			}
			continue
		}
		batch.addEdge(file.moduleID, SymbolID(target, target), "imports", item.Line, 1.0)
		for _, binding := range item.Bindings {
			if binding.Local == "" || binding.Imported == "" {
				continue
			}
			file.bindings[binding.Local] = polyglotBinding{Target: target, Imported: binding.Imported}
		}
	}
}

// resolveBinding maps one import binding to a target definition.
func (file *polyglotFile) resolveBinding(binding polyglotBinding, kind string, resolver *polyglotResolver) (string, bool) {
	if binding.Imported == "default" {
		return resolver.findDefault(binding.Target, kind)
	}
	if binding.Imported == "*" {
		return "", false
	}
	return resolver.findExported(binding.Target, binding.Imported, kind)
}

// resolveType resolves a type name to a local or imported type. The boolean
// says whether the definition is in the same file, which is what separates the
// higher same-file confidence from a cross-file import.
func (file *polyglotFile) resolveType(name string, resolver *polyglotResolver) (string, bool, bool) {
	if target, ok := resolver.findLocal(file.path, name, "type"); ok {
		return target, true, true
	}
	if binding, ok := file.bindings[name]; ok {
		target, found := file.resolveBinding(binding, "type", resolver)
		if found {
			return target, false, true
		}
	}
	return "", false, false
}

// enclosing returns the smallest indexed declaration containing a byte
// offset, or the module node of the file.
func (file *polyglotFile) enclosing(offset uint32) string {
	best := file.enclosingID(offset)
	if best != "" {
		return best
	}
	return file.moduleID
}

// enclosingSymbol returns the symbol row of the enclosing declaration. Python
// call resolution needs its qualified name to resolve `self.method()`.
func (file *polyglotFile) enclosingSymbol(offset uint32) (symbolRow, bool) {
	id := file.enclosingID(offset)
	if id == "" {
		return symbolRow{}, false
	}
	row, ok := file.symbolByID[id]
	return row, ok
}

// enclosingID returns the id of the smallest indexed declaration containing a
// byte offset, or an empty string when the offset is at module level.
func (file *polyglotFile) enclosingID(offset uint32) string {
	index := sort.Search(len(file.ranges), func(index int) bool {
		return file.ranges[index].start > offset
	}) - 1
	best := ""
	bestSpan := uint32(0)
	for current := index; current >= 0 && current >= index-128; current-- {
		candidate := file.ranges[current]
		if candidate.end < offset {
			if best != "" {
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
	return best
}

// addEdge records one deduplicated edge. The highest confidence at a site wins.
func (b *polyglotBatch) addEdge(source, target, kind string, line int, confidence float64) {
	if source == "" || target == "" || source == target {
		return
	}
	key := source + "\x00" + target + "\x00" + kind + "\x00" + strconv.Itoa(line)
	if existing, ok := b.Edges[key]; ok {
		if confidence > existing.Confidence {
			existing.Confidence = confidence
			b.Edges[key] = existing
		}
		return
	}
	b.Edges[key] = edgeRow{
		Source:     source,
		Target:     target,
		Kind:       kind,
		Line:       line,
		Confidence: confidence,
		Provenance: "tree-sitter",
	}
}

// sortPolyglotBatch orders symbols deterministically. Edges are keyed by their
// full identity and are sorted again when a batch is written.
func sortPolyglotBatch(batch *polyglotBatch) {
	sort.Slice(batch.Symbols, func(left, right int) bool {
		a, b := batch.Symbols[left], batch.Symbols[right]
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
}

// result converts one script batch into the shared write shape.
func (b polyglotBatch) result() batchResult {
	edges := make([]edgeRow, 0, len(b.Edges))
	for _, edge := range b.Edges {
		edges = append(edges, edge)
	}
	sort.Slice(edges, func(left, right int) bool {
		a, c := edges[left], edges[right]
		if a.Source != c.Source {
			return a.Source < c.Source
		}
		if a.Target != c.Target {
			return a.Target < c.Target
		}
		if a.Kind != c.Kind {
			return a.Kind < c.Kind
		}
		return a.Line < c.Line
	})
	return batchResult{Symbols: b.Symbols, Edges: edges, Unresolved: b.Unresolved, Files: map[string]string{}}
}

// splitByLanguage separates Go paths from script paths so a sync can route a
// change to the package extractor or the file-scoped script extractor.
func splitByLanguage(paths []string) (goPaths, scriptPaths []string) {
	for _, filePath := range paths {
		if IsPolyglotPath(filePath) {
			scriptPaths = append(scriptPaths, filePath)
		} else {
			goPaths = append(goPaths, filePath)
		}
	}
	return goPaths, scriptPaths
}

// mergeBatches combines the Go and script halves of one index run.
func mergeBatches(parts ...batchResult) batchResult {
	out := batchResult{Unresolved: map[string]int{}, Files: map[string]string{}}
	for _, part := range parts {
		out.Symbols = append(out.Symbols, part.Symbols...)
		out.Edges = append(out.Edges, part.Edges...)
		for filePath, count := range part.Unresolved {
			out.Unresolved[filePath] += count
		}
		for filePath, pkgPath := range part.Files {
			out.Files[filePath] = pkgPath
		}
	}
	sort.Slice(out.Symbols, func(left, right int) bool {
		a, b := out.Symbols[left], out.Symbols[right]
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
	sort.Slice(out.Edges, func(left, right int) bool {
		a, b := out.Edges[left], out.Edges[right]
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
	return out
}

// polyglotEngine lazily builds the process-wide tree-sitter runtime.
func (i *Index) polyglotEngine() (*tsEngine, error) {
	if i.ts != nil {
		return i.ts, nil
	}
	engine, err := newTSEngine()
	if err != nil {
		return nil, err
	}
	i.ts = engine
	return engine, nil
}
