package codeindex

import (
	"path"
	"strings"
)

// resolvePythonImport resolves one Python import statement. A relative
// specifier that does not match an indexed file is counted as unresolved: it is
// a local module the index failed to find. An absolute specifier that matches
// nothing is left alone because it is normally the standard library or a
// third-party package, not a broken repository import.
func (i *Index) resolvePythonImport(file *polyglotFile, item syntaxImport, resolver *polyglotResolver, batch *polyglotBatch) {
	target, ok := resolver.resolvePythonSpecifier(file.path, item.Specifier)
	if !ok {
		if strings.HasPrefix(item.Specifier, ".") {
			batch.Unresolved[file.path]++
		}
		return
	}
	batch.addEdge(file.moduleID, SymbolID(target, target), "imports", item.Line, 1.0)
	for _, binding := range item.Bindings {
		if binding.Local == "" || binding.Imported == "" {
			continue
		}
		if binding.Imported == "*" {
			file.bindings[binding.Local] = polyglotBinding{Target: target, Imported: "*"}
			continue
		}
		// `from pkg import submodule` imports the submodule file, not a
		// definition in the package __init__; bind the local name to it so a
		// later `submodule.func()` call is resolvable.
		if submodule, found := resolver.resolvePythonSubmodule(target, binding.Imported); found {
			batch.addEdge(file.moduleID, SymbolID(submodule, submodule), "imports", item.Line, 1.0)
			file.bindings[binding.Local] = polyglotBinding{Target: submodule, Imported: "*"}
			continue
		}
		file.bindings[binding.Local] = polyglotBinding{Target: target, Imported: binding.Imported}
	}
}

// resolvePythonSpecifier maps a dotted or relative Python module specifier to
// an indexed file. Only repository-root and file-relative package layouts are
// resolved; a modified sys.path, src-layout alias or namespace package is not
// guessed.
func (r *polyglotResolver) resolvePythonSpecifier(from, specifier string) (string, bool) {
	specifier = strings.TrimSpace(specifier)
	if specifier == "" {
		return "", false
	}
	dots := 0
	for dots < len(specifier) && specifier[dots] == '.' {
		dots++
	}
	rest := specifier[dots:]
	base := ""
	if dots > 0 {
		base = path.Dir(from)
		if base == "." {
			base = ""
		}
		for step := 1; step < dots; step++ {
			if base == "" {
				return "", false
			}
			base = path.Dir(base)
			if base == "." {
				base = ""
			}
		}
	}
	return r.resolvePythonModulePath(base, strings.ReplaceAll(rest, ".", "/"))
}

// resolvePythonModulePath tries the two shapes of one importable module: a
// `<name>.py` file and a `<name>/__init__.py` package.
func (r *polyglotResolver) resolvePythonModulePath(base, relative string) (string, bool) {
	joined := path.Clean(path.Join(base, relative))
	if joined == "." {
		joined = ""
	}
	if joined == "" || joined == ".." || strings.HasPrefix(joined, "../") || strings.HasPrefix(joined, "/") {
		return "", false
	}
	for _, candidate := range []string{joined + ".py", path.Join(joined, "__init__.py")} {
		if r.files[candidate] {
			return candidate, true
		}
	}
	return "", false
}

// resolvePythonSubmodule resolves `from pkg import name` to `pkg/name.py` or
// `pkg/name/__init__.py`. It only applies when the resolved target is a package
// initializer; a plain module cannot contain a submodule.
func (r *polyglotResolver) resolvePythonSubmodule(target, name string) (string, bool) {
	if path.Base(target) != "__init__.py" {
		return "", false
	}
	base := path.Dir(target)
	if base == "." {
		base = ""
	}
	return r.resolvePythonModulePath(base, strings.ReplaceAll(name, ".", "/"))
}

// resolvePythonFile turns the syntax facts of one Python file into edges. Every
// edge states only what the syntax makes clear: no return-type inference, no
// MRO walk, no metaclass or dynamic-import resolution.
func (i *Index) resolvePythonFile(file *polyglotFile, resolver *polyglotResolver, batch *polyglotBatch) {
	for _, call := range file.result.Calls {
		source := file.enclosing(call.Byte)
		if call.Receiver == "" {
			i.resolvePythonBareCall(file, resolver, batch, call, source)
			continue
		}
		i.resolvePythonMemberCall(file, resolver, batch, call, source)
	}
	for _, heritage := range file.result.Heritage {
		source := file.enclosing(heritage.Byte)
		target, local, ok := file.resolvePythonType(heritage.Target, resolver)
		if !ok || target == source {
			continue
		}
		confidence := 0.5
		if local {
			confidence = 0.7
		}
		batch.addEdge(source, target, heritage.Kind, heritage.Line, confidence)
	}
	seen := map[string]bool{}
	for _, reference := range file.result.TypeRefs {
		source := file.enclosing(reference.Byte)
		target, local, ok := file.resolvePythonType(reference.Name, resolver)
		if !ok || target == source {
			continue
		}
		key := source + "\x00" + target
		if seen[key] {
			continue
		}
		seen[key] = true
		confidence := 0.4
		if local {
			confidence = 0.5
		}
		batch.addEdge(source, target, "references", reference.Line, confidence)
	}
}

// resolvePythonBareCall resolves `name(...)`: an imported function is called at
// 0.5, a unique same-file function at 0.6. A bare call to a class is a
// constructor reference, never a type-resolved call edge.
func (i *Index) resolvePythonBareCall(file *polyglotFile, resolver *polyglotResolver, batch *polyglotBatch, call syntaxCall, source string) {
	if binding, ok := file.bindings[call.Name]; ok {
		target, kind, found := file.resolvePythonBinding(binding, resolver)
		if !found {
			if binding.Imported != "*" {
				batch.Unresolved[file.path]++
			}
			return
		}
		if target == source {
			return
		}
		if kind == "type" {
			batch.addEdge(source, target, "references", call.Line, 0.4)
			return
		}
		batch.addEdge(source, target, "calls", call.Line, 0.5)
		return
	}
	if target, ok := resolver.findLocal(file.path, call.Name, "func"); ok {
		if target != source {
			batch.addEdge(source, target, "calls", call.Line, 0.6)
		}
		return
	}
	if target, ok := resolver.findLocal(file.path, call.Name, "type"); ok && target != source {
		batch.addEdge(source, target, "references", call.Line, 0.6)
	}
}

// resolvePythonMemberCall resolves `receiver.name(...)` for three clear cases:
// `self`/`cls` in a class, an imported module namespace, and a class receiver.
func (i *Index) resolvePythonMemberCall(file *polyglotFile, resolver *polyglotResolver, batch *polyglotBatch, call syntaxCall, source string) {
	if call.Receiver == "self" || call.Receiver == "cls" {
		row, ok := file.enclosingSymbol(call.Byte)
		if !ok || row.Kind != "method" {
			return
		}
		class := pythonEnclosingClass(row.QualName)
		if class == "" {
			return
		}
		if target, found := resolver.findQualified(file.path, class+"."+call.Name, "method"); found && target != source {
			batch.addEdge(source, target, "calls", call.Line, 0.6)
		}
		return
	}
	binding, imported := file.bindings[call.Receiver]
	if imported && binding.Imported == "*" {
		if target, ok := resolver.findExported(binding.Target, call.Name, "func"); ok {
			if target != source {
				batch.addEdge(source, target, "calls", call.Line, 0.4)
			}
			return
		}
		if target, ok := resolver.findExported(binding.Target, call.Name, "type"); ok {
			if target != source {
				batch.addEdge(source, target, "references", call.Line, 0.4)
			}
			return
		}
		batch.Unresolved[file.path]++
		return
	}
	if target, ok := resolver.findQualified(file.path, call.Receiver+"."+call.Name, "method"); ok && target != source {
		batch.addEdge(source, target, "calls", call.Line, 0.6)
		return
	}
	if imported {
		if _, ok := resolver.findExported(binding.Target, binding.Imported, "type"); ok {
			if target, found := resolver.findQualified(binding.Target, binding.Imported+"."+call.Name, "method"); found && target != source {
				batch.addEdge(source, target, "calls", call.Line, 0.5)
			}
		}
	}
}

// resolvePythonBinding resolves one imported name to a function or class.
func (file *polyglotFile) resolvePythonBinding(binding polyglotBinding, resolver *polyglotResolver) (string, string, bool) {
	if binding.Imported == "*" || binding.Imported == "default" {
		return "", "", false
	}
	for _, kind := range []string{"func", "type"} {
		if target, ok := resolver.findExported(binding.Target, binding.Imported, kind); ok {
			return target, kind, true
		}
	}
	return "", "", false
}

// resolvePythonType resolves a base class or annotation name. A dotted name is
// resolved only when its first segment is an imported module; a bare name uses
// the same local/imported type lookup as the TypeScript resolver.
func (file *polyglotFile) resolvePythonType(name string, resolver *polyglotResolver) (string, bool, bool) {
	if index := strings.LastIndex(name, "."); index > 0 {
		qualifier, last := name[:index], name[index+1:]
		if binding, ok := file.bindings[qualifier]; ok && binding.Imported == "*" {
			if target, found := resolver.findExported(binding.Target, last, "type"); found {
				return target, false, true
			}
		}
		return "", false, false
	}
	return file.resolveType(name, resolver)
}

// pythonEnclosingClass strips the method name from a qualified method name. A
// nested class keeps its dotted prefix, so `Outer.Inner.run` resolves inside
// `Outer.Inner`.
func pythonEnclosingClass(qual string) string {
	if index := strings.LastIndex(qual, "."); index > 0 {
		return qual[:index]
	}
	return ""
}
