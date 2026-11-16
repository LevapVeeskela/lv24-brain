package codeindex

import (
	"fmt"
	"strings"
	"unicode"
)

// pythonAncestry is the declaration context of one Python symbol.
type pythonAncestry struct {
	names      []string
	inFunction bool
}

// collectPythonMatches runs the one Python extraction query and routes each
// match to its handler. The dispatch keys are the capture names of py_query.go.
func (p *tsParse) collectPythonMatches(spec polyglotSpec, query *tsQuery, result *syntaxResult) error {
	if err := query.exec(uint64(p.root)); err != nil {
		return err
	}
	bySite := map[string]int{}
	for {
		captures, found, err := query.next()
		if err != nil {
			return err
		}
		if !found {
			break
		}
		switch {
		case hasCapture(captures, "call_site", "method_site"):
			err = p.collectCall(captures, result)
		case hasCapture(captures, "import_statement", "import_from", "import_from_wildcard"):
			err = p.collectPythonImport(captures, result)
		case hasCapture(captures, "heritage", "heritage_dotted", "type_ref", "type_string"):
			err = p.collectPythonReference(captures, result)
		default:
			err = p.collectPythonDeclaration(spec, captures, bySite, result)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// collectPythonDeclaration turns one class, function, type alias or assignment
// capture into a symbol. A decorated declaration takes the range of its
// decorators; a declaration inside a function is local and skipped.
func (p *tsParse) collectPythonDeclaration(spec polyglotSpec, captures []tsCapture, bySite map[string]int, result *syntaxResult) error {
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
	name := strings.TrimSpace(p.engine.nodeText(p.source, nameNode))
	if name == "" {
		return nil
	}
	ancestry, err := p.pythonAncestry(declaration)
	if err != nil {
		return err
	}
	if ancestry.inFunction {
		return nil
	}
	outer := p.pythonDecorated(declaration)
	rangeNode := declaration
	if outer != 0 {
		rangeNode = outer
		defer p.engine.free(uint32(outer))
	}
	start, end, err := p.engine.byteRange(rangeNode)
	if err != nil || start >= end {
		return err
	}
	kind := spec.DeclarationKinds[kindCapture]
	if kindCapture == "decl_assign" && pythonConstantName(name) {
		kind = "const"
	}
	if kind == "func" && len(ancestry.names) > 0 {
		kind = "method"
	}
	qual := name
	if len(ancestry.names) > 0 {
		qual = strings.Join(append(append([]string(nil), ancestry.names...), name), ".")
	}
	startLine := p.lineAt(start)
	doc := p.pythonDocstring(declaration)
	if doc == "" {
		doc = p.pythonCommentBefore(startLine)
	}
	symbol := syntaxSymbol{
		Kind:      kind,
		Name:      name,
		Qual:      qual,
		Signature: p.pythonSignature(declaration, rangeNode),
		Doc:       doc,
		StartLine: startLine,
		EndLine:   p.lineAt(end),
		StartByte: start,
		EndByte:   end,
		Exported:  pythonExported(name),
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

// pythonDecorated returns the enclosing decorated_definition of a declaration,
// or zero when the declaration is not decorated. The caller frees a non-zero
// node.
func (p *tsParse) pythonDecorated(declaration tsNode) tsNode {
	parent, err := p.engine.parent(declaration)
	if err != nil || parent == 0 {
		if parent != 0 {
			p.engine.free(uint32(parent))
		}
		return 0
	}
	kind, err := p.engine.nodeType(parent)
	if err != nil || kind != "decorated_definition" {
		p.engine.free(uint32(parent))
		return 0
	}
	return parent
}

// pythonAncestry collects the enclosing class names and reports whether the
// declaration sits inside a function. A decorated definition is transparent: it
// wraps the declaration, not a function body.
func (p *tsParse) pythonAncestry(declaration tsNode) (pythonAncestry, error) {
	ancestry := pythonAncestry{}
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
		switch kind {
		case "function_definition", "lambda":
			ancestry.inFunction = true
		case "class_definition":
			nameNode, err := p.engine.childByField(current, "name")
			if err != nil {
				p.engine.free(uint32(current))
				return ancestry, err
			}
			if !p.engine.zero(nameNode) {
				name := strings.TrimSpace(p.engine.nodeText(p.source, nameNode))
				if name != "" {
					ancestry.names = append([]string{name}, ancestry.names...)
				}
			}
			p.engine.free(uint32(nameNode))
		case "module":
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

// pythonSignature renders the declaration header: decorators, the def/class
// line through its body colon, or the name and annotation of an assignment. The
// body is never read into the store.
func (p *tsParse) pythonSignature(declaration, rangeNode tsNode) string {
	start, end, err := p.engine.byteRange(rangeNode)
	if err != nil || start >= end || start >= uint32(len(p.source)) {
		return ""
	}
	kind, _ := p.engine.nodeType(declaration)
	switch kind {
	case "function_definition", "class_definition":
		body, bodyErr := p.engine.childByField(declaration, "body")
		if bodyErr == nil && !p.engine.zero(body) {
			if bodyStart, _, rangeErr := p.engine.byteRange(body); rangeErr == nil && bodyStart > start && bodyStart < end {
				end = bodyStart
			}
		}
		p.engine.free(uint32(body))
	case "assignment":
		right, rightErr := p.engine.childByField(declaration, "right")
		if rightErr == nil && !p.engine.zero(right) {
			if rightStart, _, rangeErr := p.engine.byteRange(right); rangeErr == nil && rightStart > start && rightStart < end {
				end = rightStart
			}
		}
		p.engine.free(uint32(right))
	}
	if end > uint32(len(p.source)) {
		end = uint32(len(p.source))
	}
	text := strings.TrimSpace(sanitizeComment(string(p.source[start:end])))
	text = strings.TrimSpace(strings.TrimSuffix(text, "="))
	return truncateRunes(collapseSpace(text), maxSignatureRunes)
}

// pythonDocstring reads the first statement of a body when it is a string.
func (p *tsParse) pythonDocstring(declaration tsNode) string {
	body, err := p.engine.childByField(declaration, "body")
	if err != nil || p.engine.zero(body) {
		p.engine.free(uint32(body))
		return ""
	}
	defer p.engine.free(uint32(body))
	first, err := p.engine.namedChild(body, 0)
	if err != nil || p.engine.zero(first) {
		p.engine.free(uint32(first))
		return ""
	}
	defer p.engine.free(uint32(first))
	kind, _ := p.engine.nodeType(first)
	if kind != "expression_statement" {
		return ""
	}
	inner, err := p.engine.namedChild(first, 0)
	if err != nil || p.engine.zero(inner) {
		p.engine.free(uint32(inner))
		return ""
	}
	defer p.engine.free(uint32(inner))
	if kind, _ := p.engine.nodeType(inner); kind != "string" {
		return ""
	}
	text := pythonStringContent(p.engine.nodeText(p.source, inner))
	if text == "" {
		return ""
	}
	return truncateRunes(sanitizeComment(text), maxDocRunes)
}

// pythonStringContent strips the optional prefix and the quotes of one string
// literal. It is only called on a `string` node, never on arbitrary source.
func pythonStringContent(raw string) string {
	text := strings.TrimSpace(raw)
	for len(text) > 0 && strings.ContainsRune("rRbBuUfF", rune(text[0])) {
		text = strings.TrimSpace(text[1:])
	}
	for _, quote := range []string{`"""`, `'''`, `"`, `'`} {
		if len(text) >= 2*len(quote) && strings.HasPrefix(text, quote) && strings.HasSuffix(text, quote) {
			return strings.TrimSpace(text[len(quote) : len(text)-len(quote)])
		}
	}
	return text
}

// pythonCommentBefore reads the contiguous `#` comment block directly above a
// declaration. A blank line or code line ends the block.
func (p *tsParse) pythonCommentBefore(startLine int) string {
	line := startLine - 1
	var lines []string
	for current := line; current >= 1 && line-current < 80; current-- {
		text := p.sourceLine(current)
		if !strings.HasPrefix(text, "#") {
			break
		}
		lines = append([]string{text}, lines...)
	}
	if len(lines) == 0 {
		return ""
	}
	return truncateRunes(sanitizeComment(strings.Join(lines, "\n")), maxDocRunes)
}

// sourceLine returns one trimmed 1-based line of the parsed source.
func (p *tsParse) sourceLine(number int) string {
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

// collectPythonImport records `import` and `from ... import` statements. The
// module specifier is kept verbatim; resolution happens later so a standard
// library or third-party name is never invented as a local module.
func (p *tsParse) collectPythonImport(captures []tsCapture, result *syntaxResult) error {
	var statement tsNode
	module := ""
	fromForm := false
	var items []string
	for _, capture := range captures {
		switch capture.name {
		case "import_statement", "import_from", "import_from_wildcard":
			statement = capture.node
			fromForm = capture.name != "import_statement"
		case "from_module":
			module = strings.TrimSpace(p.engine.nodeText(p.source, capture.node))
		case "import_item", "from_item":
			items = append(items, strings.TrimSpace(p.engine.nodeText(p.source, capture.node)))
		}
	}
	if statement == 0 {
		return nil
	}
	start, _, err := p.engine.byteRange(statement)
	if err != nil {
		return err
	}
	line := p.lineAt(start)
	if !fromForm {
		for _, item := range items {
			name, alias := splitPythonAlias(item)
			if name == "" {
				continue
			}
			item := syntaxImport{Specifier: name, Line: line}
			local := alias
			if local == "" {
				// `import pkg.mod` binds only the root package; a bare
				// multi-part import is not bound to its leaf module.
				if !strings.Contains(name, ".") {
					local = name
				}
			}
			if local != "" {
				item.Bindings = append(item.Bindings, syntaxBinding{Local: local, Imported: "*"})
			}
			result.Imports = append(result.Imports, item)
		}
		return nil
	}
	if module == "" {
		return nil
	}
	item := syntaxImport{Specifier: module, Line: line}
	for _, raw := range items {
		name, alias := splitPythonAlias(raw)
		if name == "" {
			continue
		}
		local := alias
		if local == "" {
			local = name
		}
		item.Bindings = append(item.Bindings, syntaxBinding{Local: local, Imported: name})
	}
	result.Imports = append(result.Imports, item)
	return nil
}

// splitPythonAlias splits `name as local` inside one import item.
func splitPythonAlias(text string) (string, string) {
	fields := strings.Fields(text)
	if len(fields) >= 3 && fields[len(fields)-2] == "as" {
		return strings.Join(fields[:len(fields)-2], ""), fields[len(fields)-1]
	}
	return strings.Join(fields, ""), ""
}

// collectPythonReference records a base class or one type annotation.
func (p *tsParse) collectPythonReference(captures []tsCapture, result *syntaxResult) error {
	var heritage, typeNode tsNode
	var target string
	for _, capture := range captures {
		switch capture.name {
		case "heritage", "heritage_dotted":
			heritage = capture.node
		case "extends", "extends_dotted":
			target = strings.TrimSpace(p.engine.nodeText(p.source, capture.node))
		case "type_ref":
			typeNode = capture.node
		case "type_string":
			name := pythonStringContent(p.engine.nodeText(p.source, capture.node))
			if pythonDottedName(name) {
				typeNode = capture.node
				target = name
			}
		}
	}
	if heritage != 0 && target != "" {
		start, _, err := p.engine.byteRange(heritage)
		if err != nil {
			return err
		}
		result.Heritage = append(result.Heritage, syntaxHeritage{
			Target: target,
			Kind:   "extends",
			Line:   p.lineAt(start),
			Byte:   start,
		})
		return nil
	}
	if typeNode == 0 {
		return nil
	}
	if target == "" {
		target = strings.TrimSpace(p.engine.nodeText(p.source, typeNode))
	}
	if !pythonDottedName(target) || p.pythonTypeReferenceExcluded(typeNode) {
		return nil
	}
	start, _, err := p.engine.byteRange(typeNode)
	if err != nil {
		return err
	}
	result.TypeRefs = append(result.TypeRefs, syntaxTypeRef{Name: target, Line: p.lineAt(start), Byte: start})
	return nil
}

// pythonTypeReferenceExcluded skips the identifier of a PEP 695 type parameter:
// it declares a name instead of using one.
func (p *tsParse) pythonTypeReferenceExcluded(node tsNode) bool {
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
		case "type_parameter":
			p.engine.free(uint32(current))
			return true
		case "class_definition", "function_definition", "module":
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

// pythonExported applies the naming convention: a leading underscore marks a
// non-public definition, except for dunder methods and module attributes.
func pythonExported(name string) bool {
	if name == "" {
		return false
	}
	if !strings.HasPrefix(name, "_") {
		return true
	}
	return len(name) > 4 && strings.HasPrefix(name, "__") && strings.HasSuffix(name, "__")
}

// pythonConstantName reports the conventional constant spelling: upper-case
// letters, digits and underscores with at least one letter.
func pythonConstantName(name string) bool {
	letters := 0
	for _, char := range name {
		switch {
		case char == '_' || unicode.IsDigit(char):
		case unicode.IsLetter(char):
			if !unicode.IsUpper(char) {
				return false
			}
			letters++
		default:
			return false
		}
	}
	return letters > 0
}

// pythonDottedName reports whether value is one identifier or a dotted chain of
// identifiers, the only annotation shapes this extractor resolves.
func pythonDottedName(value string) bool {
	if value == "" {
		return false
	}
	for _, part := range strings.Split(value, ".") {
		if !pythonIdentifier(part) {
			return false
		}
	}
	return true
}

// pythonIdentifier reports whether value is one Python identifier.
func pythonIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index, char := range value {
		switch {
		case char == '_':
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
