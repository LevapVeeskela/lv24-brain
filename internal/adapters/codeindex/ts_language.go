package codeindex

import (
	"path"
	"path/filepath"
	"strings"
)

// Polyglot languages are the non-Go source languages the native indexer
// extracts with the embedded tree-sitter reactor. Every entry names the
// grammar export of the wasm module, the language ABI version the grammar
// reports, and the queries that extract declarations, imports, calls and
// syntactically clear type references.
type polyglotSpec struct {
	Language         string
	Grammar          string
	ABI              uint32
	Declarations     string
	Imports          string
	Calls            string
	References       string
	Extensions       []string
	DeclarationKinds map[string]string
}

const (
	// typescriptLanguage is the display name of TypeScript files.
	typescriptLanguage = "typescript"
	// tsxLanguage is the display name of TSX files.
	tsxLanguage = "tsx"
	// jsLanguage is the display name of JavaScript files.
	jsLanguage = "javascript"
	// jsxLanguage is the display name of JSX files.
	jsxLanguage = "jsx"
	// pythonLanguage is the display name of Python files.
	pythonLanguage = "python"
)

// polyglotSpecs is the language registry of the native extractor. The order is
// deterministic and each extension appears once.
var polyglotSpecs = []polyglotSpec{
	{
		Language:         typescriptLanguage,
		Grammar:          "tree_sitter_typescript",
		ABI:              14,
		Declarations:     typescriptDeclarationsQuery,
		Imports:          scriptImportsQuery,
		Calls:            scriptCallsQuery,
		References:       typescriptReferencesQuery,
		Extensions:       []string{".ts"},
		DeclarationKinds: typeScriptDeclarationKinds,
	},
	{
		Language:         tsxLanguage,
		Grammar:          "tree_sitter_tsx",
		ABI:              14,
		Declarations:     typescriptDeclarationsQuery,
		Imports:          scriptImportsQuery,
		Calls:            scriptCallsQuery,
		References:       typescriptReferencesQuery,
		Extensions:       []string{".tsx"},
		DeclarationKinds: typeScriptDeclarationKinds,
	},
	{
		Language:         jsLanguage,
		Grammar:          "tree_sitter_javascript",
		ABI:              15,
		Declarations:     javascriptDeclarationsQuery,
		Imports:          scriptImportsQuery,
		Calls:            scriptCallsQuery,
		References:       javascriptReferencesQuery,
		Extensions:       []string{".js"},
		DeclarationKinds: javascriptDeclarationKinds,
	},
	{
		Language:         jsxLanguage,
		Grammar:          "tree_sitter_javascript",
		ABI:              15,
		Declarations:     javascriptDeclarationsQuery,
		Imports:          scriptImportsQuery,
		Calls:            scriptCallsQuery,
		References:       javascriptReferencesQuery,
		Extensions:       []string{".jsx"},
		DeclarationKinds: javascriptDeclarationKinds,
	},
	{
		Language:         pythonLanguage,
		Grammar:          "tree_sitter_python",
		ABI:              15,
		Declarations:     pythonDeclarationsQuery,
		Imports:          pythonImportsQuery,
		Calls:            pythonCallsQuery,
		References:       pythonReferencesQuery,
		Extensions:       []string{".py"},
		DeclarationKinds: pythonDeclarationKinds,
	},
}

// typeScriptDeclarationKinds maps the capture suffix of the TypeScript
// declaration query to the symbol kind stored in the graph.
var typeScriptDeclarationKinds = map[string]string{
	"decl_func":      "func",
	"decl_class":     "type",
	"decl_method":    "method",
	"decl_var":       "var",
	"decl_const":     "const",
	"decl_namespace": "type",
}

// javascriptDeclarationKinds maps the JavaScript capture suffix. JavaScript has
// no class, interface or type-alias declarations, so a class is still a type.
var javascriptDeclarationKinds = map[string]string{
	"decl_func":   "func",
	"decl_class":  "type",
	"decl_method": "method",
	"decl_var":    "var",
	"decl_const":  "const",
}

// pythonDeclarationKinds maps the Python capture suffix. The final kind of a
// function is decided after the ancestor walk: a function in a class body is a
// method, not a module function. A module-level assignment is a const only when
// its name is upper-case.
var pythonDeclarationKinds = map[string]string{
	"decl_func":   "func",
	"decl_class":  "type",
	"decl_type":   "type",
	"decl_assign": "var",
}

// polyglotSpecForPath resolves a repository-relative path to its language
// spec. It reports false for Go and for every unsupported language, so a
// caller never claims a language it cannot extract.
func polyglotSpecForPath(name string) (polyglotSpec, bool) {
	extension := strings.ToLower(filepath.Ext(name))
	if extension == "" {
		return polyglotSpec{}, false
	}
	for _, spec := range polyglotSpecs {
		for _, candidate := range spec.Extensions {
			if extension == candidate {
				return spec, true
			}
		}
	}
	return polyglotSpec{}, false
}

// LanguageForPath reports the indexed language of one repository path. Go is
// the only language with a type-checked extractor; TypeScript, TSX, JavaScript,
// JSX and Python are syntax-only. An unsupported extension returns an empty
// string, which is what keeps such files out of the store.
func LanguageForPath(name string) string {
	if strings.EqualFold(filepath.Ext(name), ".go") {
		return Language
	}
	if spec, ok := polyglotSpecForPath(name); ok {
		return spec.Language
	}
	return ""
}

// IsPolyglotPath reports whether the native extractor can parse one path with
// tree-sitter.
func IsPolyglotPath(name string) bool {
	_, ok := polyglotSpecForPath(name)
	return ok
}

// polyglotPathIndexable applies the repository policy for generated and
// declaration-only JavaScript. A skipped path is not hashed, so a tree of
// bundled output does not dominate an index.
func polyglotPathIndexable(rel string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(rel, `\`, "/"))
	base := path.Base(normalized)
	if strings.HasSuffix(base, ".d.ts") || strings.HasSuffix(base, ".d.tsx") {
		return false
	}
	if strings.Contains(base, ".min.") || strings.Contains(base, ".generated.") {
		return false
	}
	for _, segment := range strings.Split(normalized, "/") {
		switch segment {
		case "node_modules", "dist", "build", "out", "coverage", ".next", ".turbo",
			"venv", "__pycache__", "site-packages":
			return false
		}
	}
	return true
}

// polyglotDir is the package-like grouping of a polyglot file: its repository
// directory. The files.package column is not a Go import path here.
func polyglotDir(rel string) string {
	dir := path.Dir(strings.ReplaceAll(rel, `\`, "/"))
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// symbolLanguage reports the language of a symbol path for query answers. An
// unknown path keeps the historical Go label instead of inventing one.
func symbolLanguage(rel string) string {
	if language := LanguageForPath(rel); language != "" {
		return language
	}
	return Language
}
