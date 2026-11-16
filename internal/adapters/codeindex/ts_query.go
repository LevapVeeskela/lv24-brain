package codeindex

// The query set below is the extraction contract with the embedded grammars.
// Every query is compiled once at engine start; a grammar or query drift fails
// the index with the exact query position instead of silently returning fewer
// symbols. Node names are pinned by the grammar versions in wasm/PROVENANCE.md.

// typescriptDeclarationsQuery captures every declaration the indexer keeps.
// Nested and local declarations are filtered by the ancestor walk, not by the
// query: a query cannot know whether the enclosing function is local.
const typescriptDeclarationsQuery = `
(function_declaration name: (identifier) @name) @decl_func
(function_signature name: (identifier) @name) @decl_func
(generator_function_declaration name: (identifier) @name) @decl_func
(class_declaration name: (type_identifier) @name) @decl_class
(abstract_class_declaration name: (type_identifier) @name) @decl_class
(interface_declaration name: (type_identifier) @name) @decl_class
(type_alias_declaration name: (type_identifier) @name) @decl_class
(enum_declaration name: (identifier) @name) @decl_class
(method_definition name: (property_identifier) @name) @decl_method
(method_signature name: (property_identifier) @name) @decl_method
(abstract_method_signature name: (property_identifier) @name) @decl_method
(internal_module name: (identifier) @name) @decl_namespace
(variable_declarator name: (identifier) @name value: (arrow_function)) @decl_func
(variable_declarator name: (identifier) @name value: (function_expression)) @decl_func
(lexical_declaration kind: "const" (variable_declarator name: (identifier) @name)) @decl_const
(lexical_declaration kind: "let" (variable_declarator name: (identifier) @name)) @decl_var
(variable_declaration (variable_declarator name: (identifier) @name)) @decl_var
`

// javascriptDeclarationsQuery is the JavaScript subset: there are no
// interfaces, type aliases, enums or ambient signatures in the grammar.
const javascriptDeclarationsQuery = `
(function_declaration name: (identifier) @name) @decl_func
(generator_function_declaration name: (identifier) @name) @decl_func
(class_declaration name: (identifier) @name) @decl_class
(method_definition name: (property_identifier) @name) @decl_method
(variable_declarator name: (identifier) @name value: (arrow_function)) @decl_func
(variable_declarator name: (identifier) @name value: (function_expression)) @decl_func
(lexical_declaration kind: "const" (variable_declarator name: (identifier) @name)) @decl_const
(lexical_declaration kind: "let" (variable_declarator name: (identifier) @name)) @decl_var
(variable_declaration (variable_declarator name: (identifier) @name)) @decl_var
`

// scriptImportsQuery captures ES imports, re-exports with a source and
// CommonJS require calls. A bare `export { x }` has no source and no import
// edge; a dynamic import() is deliberately not guessed.
const scriptImportsQuery = `
(import_statement source: (string) @source) @import
(export_statement source: (string) @source) @import
(call_expression function: (identifier) @require_fn arguments: (arguments (string) @require_source)) @require
`

// scriptCallsQuery captures calls and constructor sites. A member call keeps
// its receiver so resolution can require a namespace import instead of
// matching a method name across unrelated objects.
const scriptCallsQuery = `
(call_expression function: (identifier) @call) @call_site
(call_expression function: (member_expression object: (identifier) @recv property: (property_identifier) @method)) @method_site
(call_expression function: (member_expression object: (this) property: (property_identifier) @method)) @this_site
(new_expression constructor: (identifier) @new) @new_site
`

// typescriptReferencesQuery captures inheritance and type annotations.
// Heritage is a declaration fact; a type annotation is resolved only when the
// name maps to exactly one indexed definition. The annotation subtree walk
// stops at type parameters, so a generic parameter is never a reference.
const typescriptReferencesQuery = `
(class_declaration (class_heritage (extends_clause value: (identifier) @extends))) @heritage
(abstract_class_declaration (class_heritage (extends_clause value: (identifier) @extends))) @heritage
(class_declaration (class_heritage (implements_clause (type_identifier) @implements))) @heritage
(abstract_class_declaration (class_heritage (implements_clause (type_identifier) @implements))) @heritage
(class_declaration (class_heritage (implements_clause (generic_type name: (type_identifier) @implements)))) @heritage
(abstract_class_declaration (class_heritage (implements_clause (generic_type name: (type_identifier) @implements)))) @heritage
(interface_declaration (extends_type_clause (type_identifier) @implements)) @heritage
(interface_declaration (extends_type_clause (generic_type name: (type_identifier) @implements))) @heritage
(type_parameter name: (type_identifier) @type_param)
(type_identifier) @type_ref
`

// javascriptReferencesQuery captures class inheritance only. JavaScript has no
// type annotations, so there is no second syntactic source of references.
const javascriptReferencesQuery = `
(class_declaration (class_heritage (identifier) @extends)) @heritage
`
