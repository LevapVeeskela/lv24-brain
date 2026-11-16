package codeindex

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"
)

// utf8BOM is stripped before parsing: the grammar would otherwise treat it as
// an unknown token and mark the first declaration as an ERROR span.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// tsFileResult is the syntax-level extraction of one TypeScript, TSX or
// JavaScript file. It contains declaration facts and reference sites only; the
// resolver in polyglot.go turns them into rows and edges.
type tsFileResult struct {
	Symbols    []tsSymbol
	Imports    []tsImport
	Calls      []tsCall
	Heritage   []tsHeritage
	TypeRefs   []tsTypeRef
	TypeParams map[string]bool
	Partial    bool
	Skipped    int
}

// tsSymbol is one declaration.
type tsSymbol struct {
	Kind      string
	Name      string
	Qual      string
	Signature string
	Doc       string
	StartLine int
	EndLine   int
	StartByte uint32
	EndByte   uint32
	Exported  bool
	Default   bool
}

// tsBinding maps one local import name to the name it imports.
type tsBinding struct {
	Local    string
	Imported string
}

// tsImport is one module specifier with the names it binds locally.
type tsImport struct {
	Specifier string
	Line      int
	Bindings  []tsBinding
}

// tsCall is one call or constructor site. Receiver is set for `object.method()`
// and empty for a bare identifier call.
type tsCall struct {
	Name     string
	Receiver string
	Line     int
	Byte     uint32
	New      bool
}

// tsHeritage is an extends or implements clause.
type tsHeritage struct {
	Target string
	Kind   string
	Line   int
	Byte   uint32
}

// tsTypeRef is one type identifier in an annotation or alias value.
type tsTypeRef struct {
	Name string
	Line int
	Byte uint32
}

// tsAncestry is the declaration context of one symbol.
type tsAncestry struct {
	names         []string
	inFunction    bool
	exported      bool
	defaultExport bool
	accessibility string
}

// extractScript parses one file and runs the four extraction queries. The
// engine lock is held for the whole parse: one wasm parser is shared by every
// file of the index run.
func (i *Index) extractScript(spec polyglotSpec, source []byte) (tsFileResult, error) {
	source = bytes.TrimPrefix(source, utf8BOM)
	engine, err := i.polyglotEngine()
	if err != nil {
		return tsFileResult{}, err
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.closed {
		return tsFileResult{}, fmt.Errorf("codeindex: the tree-sitter runtime is closed")
	}
	language, ok := engine.languages[spec.Language]
	if !ok {
		return tsFileResult{}, fmt.Errorf("codeindex: language %q is not loaded", spec.Language)
	}
	parsed, err := engine.parse(language, source)
	if err != nil {
		return tsFileResult{}, err
	}
	defer parsed.close()
	result := tsFileResult{Partial: parsed.hasError}
	if err := parsed.collectMatches(spec, language.all, &result); err != nil {
		return tsFileResult{}, err
	}
	if len(result.TypeParams) > 0 {
		refs := result.TypeRefs[:0]
		for _, reference := range result.TypeRefs {
			if !result.TypeParams[reference.Name] {
				refs = append(refs, reference)
			}
		}
		result.TypeRefs = refs
	}
	return result, nil
}

// collectMatches runs the one extraction query and routes each match to its
// handler. One pass over the tree keeps throughput near the parser limit: four
// independent queries would scan every node four times.
func (p *tsParse) collectMatches(spec polyglotSpec, query *tsQuery, result *tsFileResult) error {
	if err := query.exec(uint64(p.root)); err != nil {
		return err
	}
	bySite := map[string]int{}
	seenImports := map[string]bool{}
	for {
		captures, found, err := query.next()
		if err != nil {
			return err
		}
		if !found {
			break
		}
		switch {
		case hasCapture(captures, "call_site", "method_site", "this_site", "new_site"):
			err = p.collectCall(captures, result)
		case hasCapture(captures, "import", "require"):
			err = p.collectImport(captures, seenImports, result)
		case hasCapture(captures, "heritage", "type_ref", "type_param"):
			err = p.collectReference(captures, result)
		default:
			err = p.collectDeclaration(spec, captures, bySite, result)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// hasCapture reports whether a match carries one of the named captures.
func hasCapture(captures []tsCapture, names ...string) bool {
	for _, capture := range captures {
		for _, name := range names {
			if capture.name == name {
				return true
			}
		}
	}
	return false
}

// collectDeclaration turns the declaration captures of one match into a
// symbol. A declaration that contains an ERROR span is skipped; a local
// declaration (one inside a function) is not a namespace member and is skipped
// as well.
func (p *tsParse) collectDeclaration(spec polyglotSpec, captures []tsCapture, bySite map[string]int, result *tsFileResult) error {
	kindCapture := ""
	var declaration, nameNode tsNode
	for _, capture := range captures {
		if _, ok := spec.DeclarationKinds[capture.name]; ok {
			kindCapture = capture.name
			declaration = capture.node
			continue
		}
		if capture.name == "name" {
			nameNode = capture.node
		}
	}
	if kindCapture == "" || declaration == 0 || nameNode == 0 {
		return nil
	}
	if p.engine.isError(declaration) || p.engine.hasError(declaration) {
		result.Skipped++
		return nil
	}
	name := strings.Trim(strings.TrimSpace(p.engine.nodeText(p.source, nameNode)), `"'`)
	if name == "" {
		return nil
	}
	start, end, err := p.engine.byteRange(declaration)
	if err != nil || start >= end {
		return err
	}
	ancestry, err := p.ancestry(declaration)
	if err != nil {
		return err
	}
	if ancestry.inFunction {
		return nil
	}
	kind := spec.DeclarationKinds[kindCapture]
	qual := name
	if len(ancestry.names) > 0 {
		qual = strings.Join(append(append([]string(nil), ancestry.names...), name), ".")
	}
	exported := ancestry.exported
	if kind == "method" {
		exported = ancestry.accessibility != "private" && ancestry.accessibility != "protected"
	}
	symbol := tsSymbol{
		Kind:      kind,
		Name:      name,
		Qual:      qual,
		Signature: p.signature(declaration, kind),
		Doc:       p.jsDocBefore(p.lineAt(start)),
		StartLine: p.lineAt(start),
		EndLine:   p.lineAt(end),
		StartByte: start,
		EndByte:   end,
		Exported:  exported,
		Default:   ancestry.defaultExport,
	}
	key := fmt.Sprintf("%d\x00%s", start, name)
	if previous, ok := bySite[key]; ok {
		if symbolPriority(symbol.Kind) <= symbolPriority(result.Symbols[previous].Kind) {
			return nil
		}
		result.Symbols[previous] = symbol
		return nil
	}
	bySite[key] = len(result.Symbols)
	result.Symbols = append(result.Symbols, symbol)
	return nil
}

// symbolPriority resolves a duplicate capture site: a function-typed variable
// declarator matches both the function and the const/var pattern, and the
// function is the more useful declaration.
func symbolPriority(kind string) int {
	switch kind {
	case "func":
		return 5
	case "method":
		return 4
	case "type":
		return 3
	case "const":
		return 2
	default:
		return 1
	}
}

// ancestry walks the declaration's parents once. It reports the class and
// namespace names that qualify the symbol, whether a function boundary was
// crossed, and the export/accessibility facts.
func (p *tsParse) ancestry(declaration tsNode) (tsAncestry, error) {
	ancestry := tsAncestry{}
	current, err := p.engine.parent(declaration)
	if err != nil {
		return ancestry, err
	}
	for depth := 0; depth < maxAncestorDepth; depth++ {
		if current == 0 || p.engine.zero(current) {
			if current != 0 {
				p.engine.free(uint32(current))
			}
			return ancestry, nil
		}
		kind, err := p.engine.nodeType(current)
		if err != nil {
			p.engine.free(uint32(current))
			return ancestry, err
		}
		if depth == 0 {
			ancestry.accessibility = p.accessibility(declaration)
			if kind == "export_statement" {
				ancestry.exported = true
				text := strings.TrimSpace(p.engine.nodeText(p.source, current))
				ancestry.defaultExport = strings.HasPrefix(text, "export default")
			}
		}
		switch kind {
		case "function_declaration", "function_signature", "generator_function_declaration",
			"function_definition", "method_definition", "method_signature",
			"abstract_method_signature", "arrow_function", "function_expression",
			"generator_function":
			ancestry.inFunction = true
		case "class_declaration", "abstract_class_declaration", "interface_declaration",
			"enum_declaration", "internal_module", "class":
			nameNode, err := p.engine.childByField(current, "name")
			if err != nil {
				p.engine.free(uint32(current))
				return ancestry, err
			}
			if !p.engine.zero(nameNode) {
				name := strings.Trim(strings.TrimSpace(p.engine.nodeText(p.source, nameNode)), `"'`)
				if name != "" {
					ancestry.names = append([]string{name}, ancestry.names...)
				}
			}
			p.engine.free(uint32(nameNode))
		case "program", "module":
			p.engine.free(uint32(current))
			return ancestry, nil
		}
		next, err := p.engine.parent(current)
		p.engine.free(uint32(current))
		if err != nil {
			return ancestry, err
		}
		current = next
	}
	if current != 0 {
		p.engine.free(uint32(current))
	}
	return ancestry, nil
}

// accessibility reads the public/private/protected modifier of a class member.
func (p *tsParse) accessibility(node tsNode) string {
	count, err := p.engine.call("ts_node_named_child_count", uint64(node))
	if err != nil {
		return ""
	}
	for index := uint32(0); index < uint32(count); index++ {
		child, err := p.engine.namedChild(node, index)
		if err != nil {
			return ""
		}
		kind, _ := p.engine.nodeType(child)
		if kind == "accessibility_modifier" {
			modifier := strings.TrimSpace(p.engine.nodeText(p.source, child))
			p.engine.free(uint32(child))
			return modifier
		}
		p.engine.free(uint32(child))
	}
	return ""
}

// signature renders the declaration text down to the body. The body itself is
// never read into the store.
func (p *tsParse) signature(declaration tsNode, kind string) string {
	start, end, err := p.engine.byteRange(declaration)
	if err != nil || start >= end {
		return ""
	}
	declarationType, _ := p.engine.nodeType(declaration)
	if declarationType == "variable_declarator" {
		parent, err := p.engine.parent(declaration)
		if err == nil && !p.engine.zero(parent) {
			if parentStart, _, rangeErr := p.engine.byteRange(parent); rangeErr == nil && parentStart <= start {
				start = parentStart
			}
			value, valueErr := p.engine.childByField(declaration, "value")
			if valueErr == nil && !p.engine.zero(value) {
				valueType, _ := p.engine.nodeType(value)
				if isFunctionNode(valueType) {
					body, bodyErr := p.engine.childByField(value, "body")
					if bodyErr == nil && !p.engine.zero(body) {
						if bodyStart, _, rangeErr := p.engine.byteRange(body); rangeErr == nil && bodyStart > start {
							end = bodyStart
						}
					}
					p.engine.free(uint32(body))
				}
			}
			p.engine.free(uint32(value))
			p.engine.free(uint32(parent))
		}
	} else {
		body, err := p.engine.childByField(declaration, "body")
		if err == nil && !p.engine.zero(body) {
			if bodyStart, _, rangeErr := p.engine.byteRange(body); rangeErr == nil && bodyStart > start && bodyStart <= end {
				end = bodyStart
			}
		}
		p.engine.free(uint32(body))
	}
	if start > uint32(len(p.source)) {
		return ""
	}
	if end > uint32(len(p.source)) {
		end = uint32(len(p.source))
	}
	return truncateRunes(collapseSpace(sanitizeComment(string(p.source[start:end]))), maxSignatureRunes)
}

// isFunctionNode reports whether the node type carries an executable body that
// must not enter a stored signature.
func isFunctionNode(kind string) bool {
	switch kind {
	case "arrow_function", "function_expression", "generator_function", "function_declaration",
		"generator_function_declaration", "method_definition":
		return true
	}
	return false
}

// jsDocBefore extracts the JSDoc or line-comment block directly above a
// declaration. A blank line ends the block; the text is sanitized and capped
// like a Go doc comment.
func (p *tsParse) jsDocBefore(startLine int) string {
	line := startLine - 1
	if line < 1 {
		return ""
	}
	text := func(number int) string {
		if number < 1 || number > len(p.lineStarts) {
			return ""
		}
		start := p.lineStarts[number-1]
		end := len(p.source)
		if number < len(p.lineStarts) {
			end = p.lineStarts[number]
		}
		return strings.TrimSpace(string(p.source[start:end]))
	}
	previous := text(line)
	if previous == "" {
		return ""
	}
	var lines []string
	if strings.HasSuffix(previous, "*/") {
		for current := line; current >= 1 && line-current < 80; current-- {
			value := text(current)
			if value == "" {
				break
			}
			lines = append([]string{value}, lines...)
			if strings.HasPrefix(value, "/*") {
				break
			}
		}
	} else if strings.HasPrefix(previous, "//") {
		for current := line; current >= 1 && line-current < 80; current-- {
			value := text(current)
			if !strings.HasPrefix(value, "//") {
				break
			}
			lines = append([]string{value}, lines...)
		}
	} else {
		return ""
	}
	if len(lines) == 0 {
		return ""
	}
	return truncateRunes(sanitizeComment(strings.Join(lines, "\n")), maxDocRunes)
}

// collectImport records one ES import, re-export with a source or CommonJS
// require call. The module specifier is kept verbatim; resolution against
// indexed files happens later so a bare package name is never invented as a
// local module.
func (p *tsParse) collectImport(captures []tsCapture, seen map[string]bool, result *tsFileResult) error {
	var statement, requireName tsNode
	specifier := ""
	commonjs := false
	for _, capture := range captures {
		switch capture.name {
		case "import":
			statement = capture.node
		case "require":
			statement = capture.node
			commonjs = true
		case "require_fn":
			requireName = capture.node
		case "source", "require_source":
			specifier = strings.Trim(strings.TrimSpace(p.engine.nodeText(p.source, capture.node)), `"'`)
		}
	}
	if commonjs {
		if strings.TrimSpace(p.engine.nodeText(p.source, requireName)) != "require" {
			return nil
		}
	}
	if statement == 0 || specifier == "" {
		return nil
	}
	start, _, err := p.engine.byteRange(statement)
	if err != nil {
		return err
	}
	line := p.lineAt(start)
	key := fmt.Sprintf("%s\x00%d", specifier, line)
	if seen[key] {
		return nil
	}
	seen[key] = true
	item := tsImport{Specifier: specifier, Line: line}
	if !commonjs {
		raw := strings.TrimSpace(p.engine.nodeText(p.source, statement))
		item.Bindings = parseImportBindings(raw)
	}
	result.Imports = append(result.Imports, item)
	return nil
}

// parseImportBindings reads the local names of one import statement. A
// re-export has no local binding; a side-effect import has none either.
func parseImportBindings(raw string) []tsBinding {
	text := strings.TrimSpace(raw)
	if text == "" || strings.HasPrefix(text, "export") || !strings.HasPrefix(text, "import") {
		return nil
	}
	clause := strings.TrimSpace(strings.TrimPrefix(text, "import"))
	clause = collapseSpace(clause)
	if strings.HasPrefix(clause, "type ") {
		clause = strings.TrimSpace(strings.TrimPrefix(clause, "type"))
	}
	if index := strings.LastIndex(clause, " from "); index >= 0 {
		clause = clause[:index]
	} else if strings.HasPrefix(clause, `"`) || strings.HasPrefix(clause, `'`) {
		return nil
	}
	clause = strings.TrimSpace(clause)
	if clause == "" {
		return nil
	}
	var bindings []tsBinding
	if open := strings.Index(clause, "{"); open >= 0 {
		if close := strings.Index(clause[open:], "}"); close >= 0 {
			for _, entry := range strings.Split(clause[open+1:open+close], ",") {
				entry = strings.TrimSpace(entry)
				entry = strings.TrimSpace(strings.TrimPrefix(entry, "type "))
				if entry == "" {
					continue
				}
				parts := strings.Fields(entry)
				switch {
				case len(parts) == 3 && parts[1] == "as":
					bindings = append(bindings, tsBinding{Local: parts[2], Imported: parts[0]})
				case len(parts) == 1:
					bindings = append(bindings, tsBinding{Local: parts[0], Imported: parts[0]})
				}
			}
		}
		clause = strings.TrimSpace(clause[:open])
	}
	if star := strings.Index(clause, "*"); star >= 0 {
		rest := strings.Fields(strings.TrimSpace(clause[star:]))
		if len(rest) == 3 && rest[0] == "*" && rest[1] == "as" {
			bindings = append(bindings, tsBinding{Local: rest[2], Imported: "*"})
		}
		clause = strings.TrimSpace(clause[:star])
	}
	if fields := strings.Fields(strings.Trim(clause, ",")); len(fields) > 0 {
		name := fields[0]
		if name != "type" && isIdentifier(name) {
			bindings = append(bindings, tsBinding{Local: name, Imported: "default"})
		}
	}
	return bindings
}

// isIdentifier reports whether value is a single JavaScript identifier. It
// accepts Unicode letters because ECMAScript identifiers do.
func isIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index, char := range value {
		switch {
		case char == '_' || char == '$':
		case unicode.IsLetter(char):
		case unicode.IsDigit(char):
			if index == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// collectCall records one call, member-call or constructor site.
func (p *tsParse) collectCall(captures []tsCapture, result *tsFileResult) error {
	var site tsNode
	var name, receiver string
	newExpression := false
	for _, capture := range captures {
		switch capture.name {
		case "call_site", "method_site", "this_site", "new_site":
			site = capture.node
		case "call", "method", "new":
			name = strings.TrimSpace(p.engine.nodeText(p.source, capture.node))
		case "recv":
			receiver = strings.TrimSpace(p.engine.nodeText(p.source, capture.node))
		}
		if capture.name == "this_site" {
			receiver = "this"
		}
		if capture.name == "new_site" {
			newExpression = true
		}
	}
	if site == 0 || name == "" {
		return nil
	}
	start, _, err := p.engine.byteRange(site)
	if err != nil {
		return err
	}
	result.Calls = append(result.Calls, tsCall{
		Name:     name,
		Receiver: receiver,
		Line:     p.lineAt(start),
		Byte:     start,
		New:      newExpression,
	})
	return nil
}

// collectReference records an inheritance clause, a type parameter name or one
// type identifier. Uses that name a parameter are filtered after the whole file
// is read, so declaration order cannot leak a generic parameter as a reference.
func (p *tsParse) collectReference(captures []tsCapture, result *tsFileResult) error {
	var heritage, typeParam, typeRef tsNode
	var target string
	targetKind := ""
	for _, capture := range captures {
		switch capture.name {
		case "heritage":
			heritage = capture.node
		case "extends":
			target = strings.TrimSpace(p.engine.nodeText(p.source, capture.node))
			targetKind = "extends"
		case "implements":
			target = strings.TrimSpace(p.engine.nodeText(p.source, capture.node))
			targetKind = "implements"
		case "type_param":
			typeParam = capture.node
		case "type_ref":
			typeRef = capture.node
		}
	}
	if heritage != 0 && target != "" && targetKind != "" {
		start, _, err := p.engine.byteRange(heritage)
		if err != nil {
			return err
		}
		result.Heritage = append(result.Heritage, tsHeritage{
			Target: target,
			Kind:   targetKind,
			Line:   p.lineAt(start),
			Byte:   start,
		})
		return nil
	}
	if typeParam != 0 {
		name := strings.TrimSpace(p.engine.nodeText(p.source, typeParam))
		if name != "" {
			if result.TypeParams == nil {
				result.TypeParams = map[string]bool{}
			}
			result.TypeParams[name] = true
		}
		return nil
	}
	if typeRef == 0 {
		return nil
	}
	name := strings.TrimSpace(p.engine.nodeText(p.source, typeRef))
	if name == "" || p.typeReferenceExcluded(typeRef) {
		return nil
	}
	start, _, err := p.engine.byteRange(typeRef)
	if err != nil {
		return err
	}
	result.TypeRefs = append(result.TypeRefs, tsTypeRef{Name: name, Line: p.lineAt(start), Byte: start})
	return nil
}

// typeReferenceExcluded skips a type identifier that is not a reference: the
// declaration name's self-edge is already filtered by the resolver, while a
// type parameter and a heritage name are represented by other facts.
func (p *tsParse) typeReferenceExcluded(node tsNode) bool {
	current, err := p.engine.parent(node)
	if err != nil {
		return true
	}
	for depth := 0; depth < 4; depth++ {
		if current == 0 || p.engine.zero(current) {
			if current != 0 {
				p.engine.free(uint32(current))
			}
			return false
		}
		kind, err := p.engine.nodeType(current)
		if err != nil {
			p.engine.free(uint32(current))
			return true
		}
		switch kind {
		case "constraint", "default_type":
			// A generic bound/default is a real type use, not a parameter
			// declaration; stop the exclusion walk here.
			p.engine.free(uint32(current))
			return false
		case "type_parameters", "type_parameter", "class_heritage", "extends_clause",
			"implements_clause", "extends_type_clause":
			p.engine.free(uint32(current))
			return true
		case "program", "module":
			p.engine.free(uint32(current))
			return false
		}
		next, err := p.engine.parent(current)
		p.engine.free(uint32(current))
		if err != nil {
			return true
		}
		current = next
	}
	if current != 0 {
		p.engine.free(uint32(current))
	}
	return false
}
