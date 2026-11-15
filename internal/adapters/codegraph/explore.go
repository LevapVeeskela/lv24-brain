package codegraph

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// exploreReserveBytes keeps room for the flow, blast radius, pointers and
	// notes after the source sections are allocated.
	exploreReserveBytes = 3 << 10
	// exploreMinFileBytes is the smallest source allowance one file receives.
	exploreMinFileBytes = 480
	// exploreMaxSymbols bounds the symbol list of one answer.
	exploreMaxSymbols = 12
	// exploreSearchLimit is how many candidates the search tier supplies
	// before per-file ranking.
	exploreSearchLimit = 40
	// explorePathSymbols bounds the definitions taken from a pinned path.
	explorePathSymbols = 4
	// exploreMergeGap joins two line ranges separated by at most this many
	// lines, so a small interleaving gap does not cost a second header.
	exploreMergeGap = 4
	// exploreFlowNodes bounds a flow including its first step.
	exploreFlowNodes = 7
	// exploreFlowCandidates bounds the callees inspected per flow step.
	exploreFlowCandidates = 30
	// exploreBlastDepth is the reverse-BFS depth of the preview.
	exploreBlastDepth = 2
	// exploreBlastHits and exploreBlastFiles bound the preview.
	exploreBlastHits  = 16
	exploreBlastFiles = 8
)

// namedKinds can appear in a call flow. A file or import node is a carrier, not
// a step an agent can read.
var namedKinds = map[string]bool{
	"function": true, "method": true, "class": true, "struct": true,
	"interface": true, "trait": true, "route": true, "component": true,
}

// Explore answers one question with symbol source, the call flow between the
// matches and the blast radius, under MaxGraphExploreBytes.
func (i *Index) Explore(ctx context.Context, project, query string, maxFiles int) (ports.ExploreView, error) {
	original := strings.TrimSpace(query)
	if original == "" {
		return ports.ExploreView{}, domain.NewValidationError("graph.query", "is required")
	}
	if maxFiles <= 0 {
		maxFiles = ports.DefaultGraphExploreFiles
	}
	if maxFiles > ports.MaxGraphExploreFiles {
		maxFiles = ports.MaxGraphExploreFiles
	}
	cleaned, pins := extractPins(original)
	view := ports.ExploreView{Query: original, Symbols: []ports.SymbolHit{}}

	searchText := cleaned
	if searchText == "" && len(pins) > 0 {
		searchText = pins[0].Path
	}
	if searchText != "" {
		hits, err := i.Search(ctx, ports.GraphQuery{Text: searchText, Limit: exploreSearchLimit})
		if err != nil {
			return ports.ExploreView{}, err
		}
		view.Symbols = append(view.Symbols, hits...)
	}
	pinned := map[string]bool{}
	for _, pin := range pins {
		repoPath, ok := i.repoPath(pin.Path)
		if !ok {
			view.Notes = append(view.Notes, fmt.Sprintf("pinned path %s is outside the checkout", pin.Path))
			continue
		}
		pin.Path = repoPath
		hits, err := i.symbolsInFile(ctx, pin.Path, defaultKinds, explorePathSymbols)
		if err != nil {
			return ports.ExploreView{}, err
		}
		if len(hits) == 0 {
			view.Notes = append(view.Notes, fmt.Sprintf("pinned path %s is not in the index", pin.Path))
			continue
		}
		if pin.Start > 0 {
			overlapping := make([]ports.SymbolHit, 0, len(hits))
			for _, hit := range hits {
				if overlapsWindow(hit.GraphSymbol, pin.Start, pin.End) {
					overlapping = append(overlapping, hit)
				}
			}
			if len(overlapping) > 0 {
				hits = overlapping
			} else {
				view.Notes = append(view.Notes, fmt.Sprintf(
					"no definition in %s overlaps lines %d-%d; showing the file's definitions instead", pin.Path, pin.Start, pin.End))
			}
		}
		pinned[pin.Path] = true
		view.Symbols = append(view.Symbols, hits...)
	}
	view.Symbols = rankExploreHits(view.Symbols, maxFiles, pinned)
	if len(view.Symbols) == 0 {
		view.Notes = append(view.Notes, "no indexed symbol matched; try a shorter name, an exact identifier or a repository path")
		view.Bytes = estimateExplore(&view)
		return view, nil
	}
	if !hasExactHit(view.Symbols) {
		view.Notes = append(view.Notes, "no exact name matched; the results are full-text or substring candidates")
	}
	if ambiguous := ambiguousNames(view.Symbols); len(ambiguous) > 0 {
		view.Notes = append(view.Notes, fmt.Sprintf("%d name(s) are defined in several files: %s; pass a file to choose one",
			len(ambiguous), strings.Join(ambiguous, ", ")))
	}

	groups := groupByFile(view.Symbols)
	paths := make([]string, 0, len(groups))
	for _, group := range groups {
		paths = append(paths, group.path)
	}
	reader, err := i.newSourceReader(ctx, paths)
	if err != nil {
		return ports.ExploreView{}, err
	}

	// Allocate the source budget proportionally to rank: the best matching
	// file first, then the remaining budget shared the same way. A file that
	// needs less than its share returns the rest to the pool.
	hard := ports.MaxGraphExploreBytes
	budgetLeft := hard - exploreReserveBytes - estimateExplore(&view)
	if budgetLeft < exploreMinFileBytes {
		budgetLeft = exploreMinFileBytes
	}
	weights := make([]float64, len(groups))
	weightLeft := 0.0
	for index := range groups {
		weights[index] = 1 / float64(index+1)
		weightLeft += weights[index]
	}
	for index, group := range groups {
		if index >= maxFiles {
			for _, rest := range groups[index:] {
				view.Pointers = append(view.Pointers, fmt.Sprintf("%s: %d matching symbol(s), not rendered (file limit %d)", rest.path, len(rest.hits), maxFiles))
			}
			view.Truncated = true
			break
		}
		if weightLeft <= 0 || budgetLeft <= 0 {
			view.Pointers = append(view.Pointers, fmt.Sprintf("%s: not rendered (byte budget reached)", group.path))
			view.Truncated = true
			break
		}
		share := int(float64(budgetLeft) * (weights[index] / weightLeft))
		if share < exploreMinFileBytes {
			share = exploreMinFileBytes
		}
		if share > budgetLeft {
			share = budgetLeft
		}
		rendered, used := renderFile(reader, group, share)
		view.Files = append(view.Files, rendered)
		budgetLeft -= used
		weightLeft -= weights[index]
		if rendered.Truncated {
			view.Truncated = true
			view.Pointers = append(view.Pointers, fmt.Sprintf("%s is clipped; read one symbol with brain code node <name> --file %s", rendered.Path, rendered.Path))
		}
	}

	// Flow: the longest call chain of named symbols starting at one of the
	// best hits. It answers "how does X reach Y" without reading every file.
	if root, ok := firstNamed(view.Symbols); ok {
		hitIDs := make(map[string]bool, len(view.Symbols))
		for _, symbol := range view.Symbols {
			hitIDs[symbol.ID] = true
		}
		best := i.flowFrom(ctx, root, hitIDs)
		for _, candidate := range view.Symbols[:minInt(3, len(view.Symbols))] {
			if !namedKinds[candidate.Kind] || candidate.ID == root.ID {
				continue
			}
			if chain := i.flowFrom(ctx, candidate.GraphSymbol, hitIDs); len(chain) > len(best) {
				best = chain
			}
		}
		if len(best) > 1 {
			view.Flow = best
		}

		blast, err := i.impactFromSymbol(ctx, root, exploreBlastDepth)
		if err != nil {
			view.Notes = append(view.Notes, "blast radius unavailable: "+err.Error())
		} else {
			view.BlastRadius = trimBlast(blast.Hits, exploreBlastHits, exploreBlastFiles)
			for _, note := range blast.Notes {
				view.Notes = append(view.Notes, "blast radius: "+note)
			}
			if len(blast.Tests) > 0 {
				view.Notes = append(view.Notes, fmt.Sprintf("%d test file(s) reach %s: %s",
					len(blast.Tests), root.Name, strings.Join(firstStrings(blast.Tests, 4), ", ")))
			}
			if blast.Truncated {
				view.Notes = append(view.Notes, "blast radius preview is capped; run brain code impact for the full reverse walk")
			}
		}
	}

	for _, file := range view.Files {
		if changed, note := reader.changed(file.Path); changed {
			view.Notes = append(view.Notes, note)
		}
	}
	fitExplore(&view, ports.MaxGraphExploreBytes)
	view.Bytes = estimateExplore(&view)
	return view, nil
}

// exploreGroup is the per-file view of the matching symbols.
type exploreGroup struct {
	path string
	hits []ports.SymbolHit
}

// groupByFile orders files by their best hit and keeps matches in source
// order inside each file.
func groupByFile(hits []ports.SymbolHit) []exploreGroup {
	order := make([]string, 0, len(hits))
	byPath := map[string][]ports.SymbolHit{}
	for _, hit := range hits {
		if _, ok := byPath[hit.File]; !ok {
			order = append(order, hit.File)
		}
		byPath[hit.File] = append(byPath[hit.File], hit)
	}
	groups := make([]exploreGroup, 0, len(order))
	for _, path := range order {
		fileHits := byPath[path]
		sort.SliceStable(fileHits, func(left, right int) bool {
			return fileHits[left].StartLine < fileHits[right].StartLine
		})
		groups = append(groups, exploreGroup{path: path, hits: fileHits})
	}
	return groups
}

// rankExploreHits applies generated/test penalties, drops duplicates and caps
// the symbol list. A pinned path outranks a name match from another file.
func rankExploreHits(hits []ports.SymbolHit, maxFiles int, pinned map[string]bool) []ports.SymbolHit {
	seen := map[string]bool{}
	unique := make([]ports.SymbolHit, 0, len(hits))
	for _, hit := range hits {
		if seen[hit.ID] {
			continue
		}
		seen[hit.ID] = true
		hit.Score = exploreWeight(hit, pinned[hit.File])
		unique = append(unique, hit)
	}
	sort.SliceStable(unique, func(left, right int) bool {
		if unique[left].Score != unique[right].Score {
			return unique[left].Score > unique[right].Score
		}
		if unique[left].File != unique[right].File {
			return unique[left].File < unique[right].File
		}
		return unique[left].StartLine < unique[right].StartLine
	})
	// Keep at most four hits per file so one large file cannot crowd out the
	// rest of the answer.
	perFile := map[string]int{}
	out := make([]ports.SymbolHit, 0, minInt(len(unique), exploreMaxSymbols))
	for _, hit := range unique {
		if perFile[hit.File] >= 4 || len(out) >= exploreMaxSymbols {
			continue
		}
		perFile[hit.File]++
		out = append(out, hit)
	}
	return out
}

// exploreWeight ranks one candidate. Exact names and pinned files win;
// generated bundles and vendored trees lose most of their weight, and tests
// lose some, because they describe code instead of defining it.
func exploreWeight(hit ports.SymbolHit, pinned bool) float64 {
	score := hit.Score
	switch hit.Match {
	case ports.GraphMatchExact:
		score = 1000
	case ports.GraphMatchPath:
		score = 900
	case ports.GraphMatchFTS:
		score = 500 + score
	}
	if hit.Exported {
		score += 5
	}
	lower := strings.ToLower(hit.File)
	for _, marker := range []string{"generated", "/dist/", "/build/", "/vendor/", "/node_modules/", ".min.js", ".min.css", "/l10n/"} {
		if strings.Contains(lower, marker) {
			score *= 0.35
			break
		}
	}
	if IsTestFile(hit.File) {
		score *= 0.7
	}
	return score
}

func hasExactHit(hits []ports.SymbolHit) bool {
	for _, hit := range hits {
		if hit.Match == ports.GraphMatchExact || hit.Match == ports.GraphMatchPath {
			return true
		}
	}
	return false
}

// ambiguousNames lists names defined in more than one file among the hits.
func ambiguousNames(hits []ports.SymbolHit) []string {
	files := map[string]map[string]bool{}
	for _, hit := range hits {
		if files[hit.Name] == nil {
			files[hit.Name] = map[string]bool{}
		}
		files[hit.Name][hit.File] = true
	}
	out := make([]string, 0, 4)
	for name, paths := range files {
		if len(paths) > 1 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// firstNamed returns the best hit that can start a flow.
func firstNamed(hits []ports.SymbolHit) (ports.GraphSymbol, bool) {
	for _, hit := range hits {
		if namedKinds[hit.Kind] {
			return hit.GraphSymbol, true
		}
	}
	return ports.GraphSymbol{}, false
}

// flowFrom walks callees from start, preferring symbols that also matched the
// query, and returns at most exploreFlowNodes steps. The walk is a v1 flow: a
// single call chain, not a full call graph.
func (i *Index) flowFrom(ctx context.Context, start ports.GraphSymbol, hitIDs map[string]bool) []ports.FlowStep {
	steps := []ports.FlowStep{{GraphSymbol: start, Confidence: 1}}
	visited := map[string]bool{start.ID: true}
	current := start
	for len(steps) < exploreFlowNodes {
		callees, _, err := i.neighbors(ctx, current.ID, "callees", exploreFlowCandidates)
		if err != nil {
			break
		}
		var chosen *ports.GraphNeighbor
		priority := -1
		for index := range callees {
			candidate := &callees[index]
			if candidate.Edge != "calls" || !namedKinds[candidate.Kind] || visited[candidate.ID] {
				continue
			}
			score := 0
			if hitIDs[candidate.ID] {
				score = 2
			}
			if score > priority {
				priority = score
				chosen = candidate
			}
			if score == 2 {
				break
			}
		}
		if chosen == nil {
			break
		}
		visited[chosen.ID] = true
		steps = append(steps, ports.FlowStep{
			GraphSymbol: chosen.GraphSymbol,
			Via:         chosen.Edge,
			Confidence:  chosen.Confidence,
		})
		current = chosen.GraphSymbol
	}
	return steps
}

// renderFile renders the symbol ranges of one file within share bytes. It
// returns the rendered file and the bytes it used, so the caller can hand the
// remainder to the next file.
func renderFile(reader *sourceReader, group exploreGroup, share int) (ports.ExploreFile, int) {
	file := ports.ExploreFile{Path: group.path}
	names := make([]string, 0, len(group.hits))
	for _, hit := range group.hits {
		names = append(names, hit.Name)
		if file.Language == "" {
			file.Language = hit.Language
		}
	}
	file.Symbols = names
	used := len(group.path) + len(strings.Join(names, ", ")) + 16
	ranges := mergeRanges(group.hits)
	for _, lineRange := range ranges {
		lines, clipped, err := reader.lines(group.path, lineRange.start, lineRange.end)
		if err != nil {
			file.Note = "source unavailable: " + err.Error()
			return file, used
		}
		if clipped {
			file.Truncated = true
		}
		for _, line := range lines {
			cost := len(line.Text) + len(strconv.Itoa(line.Number)) + 3
			if used+cost > share {
				file.Truncated = true
				return file, used
			}
			used += cost
			file.Lines = append(file.Lines, line)
		}
	}
	return file, used
}

// overlapsWindow reports whether a definition's line range touches
// [start, end]. A pin without an end is one line.
func overlapsWindow(symbol ports.GraphSymbol, start, end int) bool {
	if end < start {
		end = start
	}
	return symbol.StartLine <= end && symbol.EndLine >= start
}

// lineRange is an inclusive 1-based line interval.
type lineRange struct {
	start, end int
}

// mergeRanges turns symbol ranges into a small set of disjoint intervals.
func mergeRanges(hits []ports.SymbolHit) []lineRange {
	ranges := make([]lineRange, 0, len(hits))
	for _, hit := range hits {
		end := hit.EndLine
		if end < hit.StartLine {
			end = hit.StartLine
		}
		ranges = append(ranges, lineRange{start: hit.StartLine, end: end})
	}
	sort.Slice(ranges, func(left, right int) bool {
		if ranges[left].start != ranges[right].start {
			return ranges[left].start < ranges[right].start
		}
		return ranges[left].end < ranges[right].end
	})
	merged := make([]lineRange, 0, len(ranges))
	for _, current := range ranges {
		if current.start < 1 {
			current.start = 1
		}
		if len(merged) > 0 && current.start <= merged[len(merged)-1].end+exploreMergeGap {
			if current.end > merged[len(merged)-1].end {
				merged[len(merged)-1].end = current.end
			}
			continue
		}
		merged = append(merged, current)
	}
	return merged
}

// trimBlast keeps the nearest hits and at most maxFiles distinct files.
func trimBlast(hits []ports.ImpactHit, maxHits, maxFiles int) []ports.ImpactHit {
	seen := map[string]bool{}
	out := make([]ports.ImpactHit, 0, len(hits))
	for _, hit := range hits {
		if len(out) >= maxHits || len(seen) >= maxFiles && !seen[hit.File] {
			continue
		}
		seen[hit.File] = true
		out = append(out, hit)
	}
	return out
}

// fitExplore drops the least important sections until the answer fits limit
// bytes. Source lines go last: a caller losing a note is cheaper than losing
// the code the query asked about.
func fitExplore(view *ports.ExploreView, limit int) {
	for iterations := 0; estimateExplore(view) > limit && iterations < 4000; iterations++ {
		switch {
		case len(view.BlastRadius) > 0:
			view.BlastRadius = view.BlastRadius[:len(view.BlastRadius)-1]
		case len(view.Flow) > 1:
			view.Flow = view.Flow[:len(view.Flow)-1]
		case len(view.Pointers) > 2:
			view.Pointers = view.Pointers[:len(view.Pointers)-1]
		case len(view.Notes) > 2:
			view.Notes = view.Notes[:len(view.Notes)-1]
		case view.Truncated:
			view.Truncated = false
		case trimLastFileLines(view):
		case len(view.Symbols) > 1:
			view.Symbols = view.Symbols[:len(view.Symbols)-1]
		default:
			shrinkExploreSymbols(view)
			return
		}
	}
}

// shrinkExploreSymbols is the last guard of the byte ceiling. It trims the
// declaration text of the remaining symbols; a docstring is never worth more
// than the source lines the reader needs.
func shrinkExploreSymbols(view *ports.ExploreView) {
	for index := range view.Symbols {
		view.Symbols[index].Signature = truncateRunes(view.Symbols[index].Signature, 1024)
		view.Symbols[index].Docstring = truncateRunes(view.Symbols[index].Docstring, 1024)
	}
}

// truncateRunes shortens s to at most limit runes without splitting one.
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

// trimLastFileLines drops the last rendered line of the last file with lines.
func trimLastFileLines(view *ports.ExploreView) bool {
	for fileIndex := len(view.Files) - 1; fileIndex >= 0; fileIndex-- {
		file := &view.Files[fileIndex]
		if len(file.Lines) == 0 {
			continue
		}
		file.Lines = file.Lines[:len(file.Lines)-1]
		file.Truncated = true
		return true
	}
	return false
}

// estimateExplore measures the answer the way a renderer would emit it: the
// text of every field plus a small per-line overhead. It is deterministic, so a
// test can assert the byte ceiling.
func estimateExplore(view *ports.ExploreView) int {
	size := 256 + len(view.Query)
	addSymbol := func(symbol ports.GraphSymbol) {
		size += len(symbol.ID) + len(symbol.Kind) + len(symbol.Name) + len(symbol.QualName) +
			len(symbol.File) + len(symbol.Language) + len(symbol.Signature) + len(symbol.Docstring) + 16
	}
	for _, hit := range view.Symbols {
		addSymbol(hit.GraphSymbol)
		size += 24
	}
	for _, file := range view.Files {
		size += len(file.Path) + len(file.Language) + len(file.Note) + len(strings.Join(file.Symbols, ", ")) + 24
		for _, line := range file.Lines {
			size += len(line.Text) + len(strconv.Itoa(line.Number)) + 4
		}
	}
	for _, step := range view.Flow {
		addSymbol(step.GraphSymbol)
		size += len(step.Via) + 16
	}
	for _, hit := range view.BlastRadius {
		addSymbol(hit.GraphSymbol)
		size += len(hit.Via) + 48
	}
	for _, pointer := range view.Pointers {
		size += len(pointer) + 2
	}
	for _, note := range view.Notes {
		size += len(note) + 2
	}
	return size
}

// pathPin is a repository path a query pinned, with an optional line window.
type pathPin struct {
	Path  string
	Start int
	End   int
}

// extractPins removes file anchors such as "internal/app/search.go#L10" or
// "src/auth.ts:20-40" from a query. Agents write paths that way; pinning them
// means the path proves identity even when the words do not.
func extractPins(query string) (string, []pathPin) {
	fields := strings.Fields(query)
	kept := make([]string, 0, len(fields))
	pins := make([]pathPin, 0, 2)
	for _, field := range fields {
		pin, ok := parsePin(field)
		if !ok {
			kept = append(kept, field)
			continue
		}
		pins = append(pins, pin)
	}
	return strings.Join(kept, " "), pins
}

// parsePin parses one path token with an optional #L or :line window.
func parsePin(token string) (pathPin, bool) {
	cleaned := strings.TrimSpace(strings.Trim(token, "`'\""))
	if cleaned == "" {
		return pathPin{}, false
	}
	spec := cleaned
	lineSpec := ""
	if index := strings.Index(spec, "#L"); index > 0 {
		spec, lineSpec = spec[:index], spec[index+2:]
	} else if index := strings.LastIndex(spec, ":"); index > 0 && !strings.Contains(spec[index:], "/") {
		tail := spec[index+1:]
		if isLineSpec(tail) {
			spec, lineSpec = spec[:index], tail
		}
	}
	if !looksLikeSourcePath(spec) {
		return pathPin{}, false
	}
	pin := pathPin{Path: normalizeRepoPath(spec)}
	if lineSpec != "" {
		parts := strings.SplitN(strings.TrimPrefix(lineSpec, "L"), "-", 2)
		pin.Start = atoi(parts[0])
		pin.End = pin.Start
		if len(parts) == 2 {
			pin.End = atoi(strings.TrimPrefix(parts[1], "L"))
		}
	}
	return pin, true
}

// isLineSpec reports whether value is "N" or "N-M".
func isLineSpec(value string) bool {
	if value == "" {
		return false
	}
	for _, part := range strings.SplitN(value, "-", 2) {
		if part == "" {
			return false
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return false
			}
		}
	}
	return true
}

// looksLikeSourcePath requires a separator and a short extension so a dotted
// identifier is never mistaken for a path.
func looksLikeSourcePath(value string) bool {
	if !strings.ContainsAny(value, `/\`) {
		return false
	}
	extension := path.Ext(strings.ReplaceAll(value, `\`, "/"))
	if len(extension) < 2 || len(extension) > 10 {
		return false
	}
	for _, char := range extension[1:] {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}

// normalizeRepoPath turns a Windows or "./"-prefixed path into the POSIX form
// the index uses.
func normalizeRepoPath(value string) string {
	normalized := strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")
	normalized = strings.TrimPrefix(normalized, "./")
	return normalized
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

// firstStrings returns up to count values.
func firstStrings(values []string, count int) []string {
	if len(values) <= count {
		return values
	}
	return values[:count]
}
