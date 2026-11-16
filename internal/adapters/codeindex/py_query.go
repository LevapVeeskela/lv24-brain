package codeindex

// The Python query set below is the extraction contract with the pinned
// tree-sitter-python grammar (v0.25.0, ABI 15, see wasm/PROVENANCE.md). Queries
// compile once at engine start, so a grammar upgrade that renames a node fails
// the index with the exact query position instead of silently dropping symbols.

// pythonDeclarationsQuery captures classes, functions, PEP 695 type aliases and
// assignments. The ancestor walk decides the final kind: a function in a class
// body is a method, a declaration inside a function is local and skipped, and a
// module-level assignment is a const only when its name is upper-case.
const pythonDeclarationsQuery = `
(class_definition name: (identifier) @name) @decl_class
(function_definition name: (identifier) @name) @decl_func
(type_alias_statement left: (type (generic_type (identifier) @name))) @decl_type
(type_alias_statement left: (type (identifier) @name)) @decl_type
(assignment left: (identifier) @name) @decl_assign
`

// pythonImportsQuery captures both import forms. One match carries one imported
// name, so `from pkg import A, B` binds A and B separately while the import edge
// is emitted once per statement line.
const pythonImportsQuery = `
(import_statement name: (_) @import_item) @import_statement
(import_from_statement module_name: (_) @from_module name: (_) @from_item) @import_from
(import_from_statement module_name: (_) @from_module (wildcard_import) @from_wildcard) @import_from_wildcard
`

// pythonCallsQuery captures bare calls and simple member calls. A dotted
// receiver such as `self.db.query()` or `a.b.c()` is deliberately not captured:
// resolving its type would need inference the syntax-only index must not fake.
const pythonCallsQuery = `
(call function: (identifier) @call) @call_site
(call function: (attribute object: (identifier) @recv attribute: (identifier) @method)) @method_site
`

// pythonReferencesQuery captures base classes and type annotations. Type names
// are plain identifiers in Python, so the query anchors on the annotation node
// (`type`) instead of every identifier; the declaration collector separately
// rejects names inside type-parameter declarations.
const pythonReferencesQuery = `
(class_definition superclasses: (argument_list (identifier) @extends)) @heritage
(class_definition superclasses: (argument_list (attribute) @extends_dotted)) @heritage_dotted
(type (identifier) @type_ref)
(type (string) @type_string)
`
