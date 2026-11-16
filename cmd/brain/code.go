package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// runCode dispatches the code-graph commands.
func runCode(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printCodeUsage(stderr)
		return 2
	}
	command, rest := args[0], args[1:]
	switch command {
	case "help", "-h", "--help":
		printCodeUsage(stdout)
		return 0
	case "index":
		return runCodeIndex(rest, stdout, stderr)
	case "sync":
		return runCodeSync(rest, stdout, stderr)
	case "status":
		return runCodeStatus(rest, stdout, stderr)
	case "search":
		return runCodeSearch(rest, stdout, stderr)
	case "node":
		return runCodeNode(rest, stdout, stderr)
	case "explore":
		return runCodeExplore(rest, stdout, stderr)
	case "impact":
		return runCodeImpact(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "brain code: unknown subcommand %q\n\n", command)
		printCodeUsage(stderr)
		return 2
	}
}

func printCodeUsage(w io.Writer) {
	fmt.Fprint(w, `Build and query a code graph.

The native engine indexes Go with go/ast and go/types, and TypeScript, TSX,
JavaScript and JSX with an embedded tree-sitter runtime. It keeps a derived
SQLite store under the brain data directory. The codegraph engine reads an
external CodeGraph index in place and cannot write it.

Usage:
  brain code index   [--project p] [--engine native] [--rebuild] [--tags t] [--json]
  brain code sync    [path...] [--project p] [--engine native|codegraph] [--tags t] [--json]
  brain code status  [--project p] [--engine native|codegraph] [--json]
  brain code search  <query> [--kind k]... [--file f] [--limit n] [--json]
  brain code node    <symbol> [--file f] [--code] [--callers=false] [--callees=false] [--json]
  brain code explore <query> [--max-files n] [--json]
  brain code impact  <symbol> [--file f] [--depth n] [--json]

Flags:
  --project <id>    project id or alias (default: the working directory)
  --engine <name>   native, codegraph or off (default: graph.engine in brain.yaml)
  --db <path>       index file or its directory (default: graph.path, then the
                    engine default)
  --root <dir>      checkout source is read from (default: the project path)
  --tags <list>     extra Go build tags for the native indexer (repeatable)
  --rebuild         drop the native store before indexing
  --json            print the typed JSON answer
  --config <path>   path to brain.yaml

The code graph is off by default. Enable the native indexer with:

  graph:
    engine: native

Then run: brain code index
Exit codes: 0 success, 1 runtime error, 2 usage error.
`)
}

// codeFlags are the flags every `brain code` subcommand shares.
type codeFlags struct {
	project *string
	engine  *string
	db      *string
	root    *string
	asJSON  *bool
	config  *string
	rebuild *bool
	tags    *stringList
}

func addCodeFlags(fs *flag.FlagSet) *codeFlags {
	return &codeFlags{
		project: fs.String("project", "", "project id or alias"),
		engine:  fs.String("engine", "", "graph engine: native, codegraph or off (default: graph.engine)"),
		db:      fs.String("db", "", "index file or its directory"),
		root:    fs.String("root", "", "checkout root to read source from"),
		asJSON:  fs.Bool("json", false, "print JSON"),
		config:  configFlag(fs),
	}
}

// addCodeWriteFlags adds the flags only the native writer understands.
func addCodeWriteFlags(fs *flag.FlagSet, flags *codeFlags) {
	flags.rebuild = fs.Bool("rebuild", false, "drop and rebuild the native store")
	flags.tags = &stringList{}
	fs.Var(flags.tags, "tags", "extra Go build tags (repeatable or comma-separated)")
}

// withCodeRuntime resolves the engine, project, checkout and index, then runs
// fn. writable creates an empty native store; a read command leaves a missing
// store missing and reports the index command instead.
func withCodeRuntime(ctx context.Context, flags *codeFlags, command string, stderr io.Writer, writable bool, fn func(domain.Project, *bootstrap.GraphRuntime) int) int {
	cfg, err := loadRequiredConfig(*flags.config)
	if err != nil {
		return reportError(stderr, command, err)
	}
	if err := cfg.Graph.Validate(); err != nil {
		return reportError(stderr, command, err)
	}
	engine := strings.ToLower(strings.TrimSpace(*flags.engine))
	if engine == "" {
		engine = cfg.Graph.EffectiveEngine()
	}
	switch engine {
	case ports.GraphEngineOff:
		fmt.Fprintf(stderr, "brain %s: the code graph is off; set graph.engine: native in brain.yaml or pass --engine native\n", command)
		return 1
	case ports.GraphEngineNative, ports.GraphEngineCodeGraph:
	default:
		fmt.Fprintf(stderr, "brain %s: unknown graph engine %q; use native, codegraph or off\n", command, engine)
		return 1
	}
	if cfg.Mode == config.ModeServer {
		fmt.Fprintf(stderr, "brain %s: server mode keeps no local checkout; run this command on a local or git brain\n", command)
		return 1
	}
	useCase, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, command, err)
	}
	defer func() { _ = useCase.Close() }()
	target, err := resolveTarget(ctx, useCase.App, *flags.project, "", stderr)
	if err != nil {
		return reportError(stderr, command, err)
	}
	root := codeRoot(*flags.root, target)
	if root == "" {
		fmt.Fprintf(stderr, "brain %s: project %s has no local checkout; register one with brain project set %s --add-path <dir>\n",
			command, target.Project.ID, target.Project.ID)
		return 1
	}
	dbPath, err := bootstrap.GraphDBPath(cfg, engine, root, target.Project.ID, *flags.db)
	if err != nil {
		return reportError(stderr, command, err)
	}
	if !writable {
		if _, statErr := os.Stat(dbPath); statErr != nil {
			if engine == ports.GraphEngineCodeGraph {
				fmt.Fprintf(stderr, "brain %s: no codegraph index at %s; create one with `codegraph index` (or brain graphs --init) in %s, or set graph.path\n",
					command, dbPath, root)
			} else {
				fmt.Fprintf(stderr, "brain %s: no native graph index at %s; run brain code index --project %s\n",
					command, dbPath, target.Project.ID)
			}
			return 1
		}
	}
	runtime, err := bootstrap.OpenGraph(ctx, bootstrap.GraphOptions{
		Engine:  engine,
		Root:    root,
		Project: target.Project.ID,
		DBPath:  dbPath,
		Create:  writable,
		Tags:    flags.tags.values(),
	})
	if err != nil {
		return reportError(stderr, command, err)
	}
	defer func() { _ = runtime.Close() }()
	return fn(target.Project, runtime)
}

// codeRoot picks the checkout: the explicit flag, the resolved path, then the
// first registered local path.
func codeRoot(flagRoot string, target app.Resolved) string {
	if value := strings.TrimSpace(flagRoot); value != "" {
		if info, err := os.Stat(value); err == nil && info.IsDir() {
			return filepath.Clean(value)
		}
		return ""
	}
	if target.Path != "" {
		if info, err := os.Stat(target.Path); err == nil && info.IsDir() {
			return filepath.Clean(target.Path)
		}
	}
	for _, path := range app.LocalPaths(target.Project.Paths) {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return filepath.Clean(path)
		}
	}
	return ""
}

// codeDBPath is intentionally not duplicated here: bootstrap.GraphDBPath owns
// the engine-specific default and the graph.path override.

func runCodeIndex(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("code index", stderr)
	flags := addCodeFlags(fs)
	addCodeWriteFlags(fs, flags)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("code index", positional, 0, "no arguments", stderr) {
		return 2
	}
	ctx := context.Background()
	return withCodeRuntime(ctx, flags, "code index", stderr, true, func(project domain.Project, runtime *bootstrap.GraphRuntime) int {
		if runtime.Native == nil {
			fmt.Fprintln(stderr, "brain code index: the codegraph engine is read-only; run `codegraph index` in the checkout and use brain code status")
			return 1
		}
		var result ports.GraphSyncResult
		var err error
		if *flags.rebuild {
			result, err = runtime.Native.Rebuild(ctx, project.ID)
		} else {
			result, err = runtime.Native.Index(ctx, project.ID)
		}
		if err != nil {
			return reportError(stderr, "code index", err)
		}
		if *flags.asJSON {
			return writeJSONReport(stdout, stderr, "code index", result)
		}
		printCodeSync(stdout, "index", result)
		return 0
	})
}

func runCodeSync(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("code sync", stderr)
	flags := addCodeFlags(fs)
	addCodeWriteFlags(fs, flags)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	ctx := context.Background()
	return withCodeRuntime(ctx, flags, "code sync", stderr, true, func(project domain.Project, runtime *bootstrap.GraphRuntime) int {
		var result ports.GraphSyncResult
		var err error
		if runtime.Native != nil {
			result, err = runtime.Native.Sync(ctx, project.ID, positional)
		} else {
			result, err = runtime.Index.Sync(ctx, project.ID, positional)
		}
		if err != nil {
			return reportError(stderr, "code sync", err)
		}
		if *flags.asJSON {
			return writeJSONReport(stdout, stderr, "code sync", result)
		}
		printCodeSync(stdout, "sync", result)
		return 0
	})
}

func runCodeStatus(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("code status", stderr)
	flags := addCodeFlags(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("code status", positional, 0, "no arguments", stderr) {
		return 2
	}
	ctx := context.Background()
	return withCodeRuntime(ctx, flags, "code status", stderr, false, func(project domain.Project, runtime *bootstrap.GraphRuntime) int {
		status, err := runtime.Index.Status(ctx, project.ID)
		if err != nil {
			return reportError(stderr, "code status", err)
		}
		if *flags.asJSON {
			return writeJSONReport(stdout, stderr, "code status", status)
		}
		printCodeStatus(stdout, status)
		return 0
	})
}

func runCodeSearch(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("code search", stderr)
	flags := addCodeFlags(fs)
	limit := fs.Int("limit", ports.DefaultGraphSearchLimit, "maximum hits")
	file := fs.String("file", "", "keep only symbols defined in that path")
	var kinds stringList
	fs.Var(&kinds, "kind", "keep only that node kind (repeatable)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	query := strings.TrimSpace(strings.Join(positional, " "))
	if query == "" {
		fmt.Fprintln(stderr, "brain code search: a query is required")
		return 2
	}
	if *limit < 1 || *limit > ports.MaxGraphSearchLimit {
		fmt.Fprintf(stderr, "brain code search: --limit must be between 1 and %d\n", ports.MaxGraphSearchLimit)
		return 2
	}
	ctx := context.Background()
	return withCodeRuntime(ctx, flags, "code search", stderr, false, func(project domain.Project, runtime *bootstrap.GraphRuntime) int {
		hits, err := runtime.Index.Search(ctx, ports.GraphQuery{Text: query, Kinds: kinds, File: *file, Limit: *limit})
		if err != nil {
			return reportError(stderr, "code search", err)
		}
		if *flags.asJSON {
			return writeJSONReport(stdout, stderr, "code search", hits)
		}
		printCodeSearch(stdout, query, hits)
		return 0
	})
}

func runCodeNode(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("code node", stderr)
	flags := addCodeFlags(fs)
	file := fs.String("file", "", "file that contains the symbol")
	includeSource := fs.Bool("code", false, "print the declaration with line numbers")
	includeCallers := fs.Bool("callers", true, "list direct callers")
	includeCallees := fs.Bool("callees", true, "list direct callees")
	limit := fs.Int("limit", 20, "maximum neighbours per side")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	symbol := strings.TrimSpace(strings.Join(positional, " "))
	if symbol == "" {
		fmt.Fprintln(stderr, "brain code node: a symbol is required")
		return 2
	}
	ctx := context.Background()
	return withCodeRuntime(ctx, flags, "code node", stderr, false, func(project domain.Project, runtime *bootstrap.GraphRuntime) int {
		view, err := runtime.Index.Node(ctx, project.ID, symbol, *file, ports.NodeOpts{
			IncludeSource:  *includeSource,
			IncludeCallers: *includeCallers,
			IncludeCallees: *includeCallees,
			Limit:          *limit,
		})
		if err != nil {
			return reportError(stderr, "code node", err)
		}
		if *flags.asJSON {
			return writeJSONReport(stdout, stderr, "code node", view)
		}
		printCodeNode(stdout, view, *includeCallers, *includeCallees)
		return 0
	})
}

func runCodeExplore(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("code explore", stderr)
	flags := addCodeFlags(fs)
	maxFiles := fs.Int("max-files", ports.DefaultGraphExploreFiles, "maximum files rendered")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	query := strings.TrimSpace(strings.Join(positional, " "))
	if query == "" {
		fmt.Fprintln(stderr, "brain code explore: a query is required")
		return 2
	}
	if *maxFiles < 1 || *maxFiles > ports.MaxGraphExploreFiles {
		fmt.Fprintf(stderr, "brain code explore: --max-files must be between 1 and %d\n", ports.MaxGraphExploreFiles)
		return 2
	}
	ctx := context.Background()
	return withCodeRuntime(ctx, flags, "code explore", stderr, false, func(project domain.Project, runtime *bootstrap.GraphRuntime) int {
		view, err := runtime.Index.Explore(ctx, project.ID, query, *maxFiles)
		if err != nil {
			return reportError(stderr, "code explore", err)
		}
		if *flags.asJSON {
			return writeJSONReport(stdout, stderr, "code explore", view)
		}
		printCodeExplore(stdout, view)
		return 0
	})
}

func runCodeImpact(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("code impact", stderr)
	flags := addCodeFlags(fs)
	file := fs.String("file", "", "file that contains the symbol")
	depth := fs.Int("depth", ports.DefaultGraphImpactDepth, "reverse BFS depth")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	symbol := strings.TrimSpace(strings.Join(positional, " "))
	if symbol == "" {
		fmt.Fprintln(stderr, "brain code impact: a symbol is required")
		return 2
	}
	if *depth < 1 || *depth > ports.MaxGraphImpactDepth {
		fmt.Fprintf(stderr, "brain code impact: --depth must be between 1 and %d\n", ports.MaxGraphImpactDepth)
		return 2
	}
	ctx := context.Background()
	return withCodeRuntime(ctx, flags, "code impact", stderr, false, func(project domain.Project, runtime *bootstrap.GraphRuntime) int {
		view, err := runtime.Index.Impact(ctx, project.ID, symbol, *file, *depth)
		if err != nil {
			return reportError(stderr, "code impact", err)
		}
		if *flags.asJSON {
			return writeJSONReport(stdout, stderr, "code impact", view)
		}
		printCodeImpact(stdout, view)
		return 0
	})
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("value must not be empty")
	}
	*l = append(*l, value)
	return nil
}

// values splits comma-separated occurrences into individual tags.
func (l *stringList) values() []string {
	if l == nil {
		return nil
	}
	out := make([]string, 0, len(*l))
	for _, value := range *l {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// printCodeSync renders the result of index or sync.
func printCodeSync(w io.Writer, command string, result ports.GraphSyncResult) {
	fmt.Fprintf(w, "%s: %s (project %s)\n", command, result.Engine, result.Project)
	if result.Unsupported {
		fmt.Fprintln(w, "status: unsupported by this engine")
	} else {
		fmt.Fprintf(w, "changed files: %d\n", result.Changed)
	}
	printNotes(w, result.Notes)
}

func printCodeStatus(w io.Writer, status ports.GraphStatus) {
	fmt.Fprintf(w, "project:       %s\n", status.Project)
	with := status.IndexedWithVersion
	if with == "" {
		with = "-"
	}
	fmt.Fprintf(w, "engine:        %s (schema %d, extraction %d, indexed with %s)\n",
		status.Engine, status.SchemaVersion, status.ExtractionVersion, with)
	fmt.Fprintf(w, "checkout:      %s\n", status.Root)
	fmt.Fprintf(w, "index:         %s (%s)\n", status.DBPath, formatBytes(status.SizeBytes))
	indexed := "unknown"
	if !status.IndexedAt.IsZero() {
		indexed = fmt.Sprintf("%s (%s ago)", status.IndexedAt.Format(time.RFC3339), formatAge(status.AgeSeconds))
	}
	fmt.Fprintf(w, "indexed:       %s\n", indexed)
	if status.Commit != "" {
		state := ""
		if status.Dirty {
			state = ", worktree dirty at index time"
		}
		fmt.Fprintf(w, "commit:        %s%s\n", shortCommit(status.Commit), state)
	}
	if status.IndexState != "" {
		fmt.Fprintf(w, "state:         %s\n", status.IndexState)
	}
	fmt.Fprintf(w, "files:         %d\n", status.Files)
	fmt.Fprintf(w, "nodes:         %d\n", status.Nodes)
	fmt.Fprintf(w, "edges:         %d\n", status.Edges)
	fmt.Fprintf(w, "unresolved:    %d\n", status.UnresolvedRefs)
	if status.Engine == ports.GraphEngineNative {
		fmt.Fprintf(w, "writes:        brain code index | brain code sync\n")
	} else {
		fmt.Fprintf(w, "read mode:     read-only (no statement can write)\n")
	}
	printCounts(w, "nodes by kind", status.NodesByKind, 8)
	printCounts(w, "files by language", status.FilesByLanguage, 6)
	printNotes(w, status.Notes)
}

func printCounts(w io.Writer, label string, counts map[string]int, limit int) {
	if len(counts) == 0 {
		return
	}
	type entry struct {
		name  string
		count int
	}
	entries := make([]entry, 0, len(counts))
	for name, count := range counts {
		entries = append(entries, entry{name: name, count: count})
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].count != entries[right].count {
			return entries[left].count > entries[right].count
		}
		return entries[left].name < entries[right].name
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	parts := make([]string, 0, len(entries))
	for _, item := range entries {
		parts = append(parts, fmt.Sprintf("%s %d", item.name, item.count))
	}
	fmt.Fprintf(w, "%-14s %s\n", label+":", strings.Join(parts, ", "))
}

func printCodeSearch(w io.Writer, query string, hits []ports.SymbolHit) {
	if len(hits) == 0 {
		fmt.Fprintf(w, "no symbols matched %q\n", query)
		return
	}
	fmt.Fprintf(w, "%d hit(s) for %q\n", len(hits), query)
	for _, hit := range hits {
		fmt.Fprintf(w, "  %-9s %s %s  %s\n", hit.Match, hit.Kind, hit.Name, codeLocation(hit.GraphSymbol))
		if hit.Signature != "" {
			fmt.Fprintf(w, "            %s\n", firstLine(hit.Signature))
		}
		if hit.Docstring != "" {
			fmt.Fprintf(w, "            %s\n", firstLine(hit.Docstring))
		}
	}
}

func printCodeNode(w io.Writer, view ports.NodeView, withCallers, withCallees bool) {
	fmt.Fprintf(w, "%s %s  %s\n", view.Symbol.Kind, view.Symbol.QualName, codeLocation(view.Symbol))
	if view.Symbol.Signature != "" {
		fmt.Fprintf(w, "signature: %s\n", firstLine(view.Symbol.Signature))
	}
	if view.Symbol.Docstring != "" {
		fmt.Fprintf(w, "docstring: %s\n", firstLine(view.Symbol.Docstring))
	}
	if len(view.Source) > 0 {
		fmt.Fprintln(w, "source:")
		printNumberedLines(w, view.Source)
		if view.SourceTruncated {
			fmt.Fprintln(w, "  … truncated")
		}
	}
	if withCallers {
		fmt.Fprintf(w, "callers (%d):\n", len(view.Callers))
		printNeighbors(w, view.Callers)
	}
	if withCallees {
		fmt.Fprintf(w, "callees (%d):\n", len(view.Callees))
		printNeighbors(w, view.Callees)
	}
	printNotes(w, view.Notes)
}

func printNeighbors(w io.Writer, neighbors []ports.GraphNeighbor) {
	if len(neighbors) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	for _, neighbor := range neighbors {
		where := ""
		if neighbor.Line > 0 {
			where = fmt.Sprintf(" line %d", neighbor.Line)
		}
		count := ""
		if neighbor.Count > 1 {
			count = fmt.Sprintf(" ×%d", neighbor.Count)
		}
		fmt.Fprintf(w, "  %-12s %s%s  %s%s (confidence %.2f)\n",
			neighbor.Edge, neighbor.Name, count, codeLocation(neighbor.GraphSymbol), where, neighbor.Confidence)
	}
}

func printCodeExplore(w io.Writer, view ports.ExploreView) {
	fmt.Fprintf(w, "query: %s\n", view.Query)
	fmt.Fprintf(w, "symbols (%d):\n", len(view.Symbols))
	for _, symbol := range view.Symbols {
		fmt.Fprintf(w, "  %-9s %s %s  %s\n", symbol.Match, symbol.Kind, symbol.Name, codeLocation(symbol.GraphSymbol))
	}
	for _, file := range view.Files {
		fmt.Fprintf(w, "\n%s", file.Path)
		if file.Language != "" {
			fmt.Fprintf(w, " (%s)", file.Language)
		}
		fmt.Fprintln(w)
		printNumberedLines(w, file.Lines)
		if file.Truncated {
			fmt.Fprintln(w, "  … clipped at the byte budget")
		}
		if file.Note != "" {
			fmt.Fprintf(w, "  note: %s\n", file.Note)
		}
	}
	if len(view.Flow) > 1 {
		fmt.Fprintln(w, "\nflow:")
		fmt.Fprintf(w, "  %s", view.Flow[0].Name)
		for _, step := range view.Flow[1:] {
			fmt.Fprintf(w, " --%s--> %s", step.Via, step.Name)
		}
		fmt.Fprintln(w)
	}
	if len(view.BlastRadius) > 0 {
		fmt.Fprintf(w, "\nblast radius (%d):\n", len(view.BlastRadius))
		for _, hit := range view.BlastRadius {
			test := ""
			if hit.Test {
				test = " [test]"
			}
			fmt.Fprintf(w, "  d%d %-12s %s  %s%s\n", hit.Distance, hit.Via, hit.Name, codeLocation(hit.GraphSymbol), test)
		}
	}
	if len(view.Pointers) > 0 {
		fmt.Fprintln(w, "\npointers:")
		for _, pointer := range view.Pointers {
			fmt.Fprintf(w, "  - %s\n", pointer)
		}
	}
	printNotes(w, view.Notes)
	fmt.Fprintf(w, "\nbytes: %d of %d\n", view.Bytes, ports.MaxGraphExploreBytes)
}

func printCodeImpact(w io.Writer, view ports.ImpactView) {
	fmt.Fprintf(w, "root:  %s %s  %s\n", view.Symbol.Kind, view.Symbol.QualName, codeLocation(view.Symbol))
	fmt.Fprintf(w, "depth: %d\n", view.Depth)
	fmt.Fprintf(w, "hits (%d):\n", len(view.Hits))
	if len(view.Hits) == 0 {
		fmt.Fprintln(w, "  (none: nothing in the index depends on this definition)")
	}
	for _, hit := range view.Hits {
		test := ""
		if hit.Test {
			test = " [test]"
		}
		provenance := ""
		if hit.Provenance != "" {
			provenance = ", " + hit.Provenance
		}
		fmt.Fprintf(w, "  d%-2d %-12s %s  %s%s (confidence %.2f%s)\n",
			hit.Distance, hit.Via, hit.Name, codeLocation(hit.GraphSymbol), test, hit.Confidence, provenance)
	}
	if len(view.Tests) > 0 {
		fmt.Fprintf(w, "tests (%d): %s\n", len(view.Tests), strings.Join(view.Tests, ", "))
	}
	if view.Truncated {
		fmt.Fprintln(w, "truncated: the walk stopped at the node cap")
	}
	printNotes(w, view.Notes)
}

// printNumberedLines renders source with a gap marker when the numbers jump,
// so a clipped answer never looks like contiguous code.
func printNumberedLines(w io.Writer, lines []ports.SourceLine) {
	last := 0
	for _, line := range lines {
		if last != 0 && line.Number > last+1 {
			fmt.Fprintf(w, "%5s\t…\n", "")
		}
		fmt.Fprintf(w, "%5d\t%s\n", line.Number, line.Text)
		last = line.Number
	}
}

func printNotes(w io.Writer, notes []string) {
	if len(notes) == 0 {
		return
	}
	fmt.Fprintln(w, "notes:")
	for _, note := range notes {
		fmt.Fprintf(w, "  - %s\n", note)
	}
}

func codeLocation(symbol ports.GraphSymbol) string {
	if symbol.EndLine > symbol.StartLine {
		return fmt.Sprintf("%s:%d-%d", symbol.File, symbol.StartLine, symbol.EndLine)
	}
	return fmt.Sprintf("%s:%d", symbol.File, symbol.StartLine)
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(line)
}

func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

func formatBytes(size int64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
	)
	switch {
	case size >= gib:
		return fmt.Sprintf("%.1f GiB", float64(size)/gib)
	case size >= mib:
		return fmt.Sprintf("%.1f MiB", float64(size)/mib)
	case size >= kib:
		return fmt.Sprintf("%.1f KiB", float64(size)/kib)
	default:
		return fmt.Sprintf("%d B", size)
	}
}

func formatAge(seconds int64) string {
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	if seconds < 3600 {
		return fmt.Sprintf("%dm", seconds/60)
	}
	if seconds < 86400 {
		return fmt.Sprintf("%dh%02dm", seconds/3600, (seconds%3600)/60)
	}
	return fmt.Sprintf("%dd%02dh", seconds/86400, (seconds%86400)/3600)
}
