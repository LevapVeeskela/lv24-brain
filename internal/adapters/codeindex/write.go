package codeindex

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"golang.org/x/tools/go/packages"
)

const sqlChunk = 400

// Index rebuilds the store from scratch. It is the safe path after a schema
// change, a module move or a symbol-set change that incremental Sync cannot
// relink.
func (i *Index) Index(ctx context.Context, project string) (ports.GraphSyncResult, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.indexLocked(ctx, project, "full rebuild")
}

// Rebuild drops the schema and rebuilds it, then indexes every package. It
// recovers a store whose files were truncated or written by an incompatible
// engine.
func (i *Index) Rebuild(ctx context.Context, project string) (ports.GraphSyncResult, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.needsRebuild = true
	return i.indexLocked(ctx, project, "requested rebuild")
}

// indexLocked runs a full rebuild while the caller holds the write lock.
func (i *Index) indexLocked(ctx context.Context, project string, reason string) (ports.GraphSyncResult, error) {
	started := time.Now()
	i.module = readModulePath(i.root)
	if err := i.ensureSchema(ctx); err != nil {
		return ports.GraphSyncResult{}, err
	}
	scan, err := i.scan(ctx, nil)
	if err != nil {
		return ports.GraphSyncResult{}, err
	}
	if len(scan.Files) == 0 {
		return ports.GraphSyncResult{}, fmt.Errorf(
			"%w: the native engine indexes Go, TypeScript, TSX, JavaScript and JSX; no supported source file was found under %s",
			domain.ErrUnsupported, i.root)
	}
	goFiles := 0
	for _, state := range scan.Files {
		if state.Language == Language {
			goFiles++
		}
	}
	var pkgs []*packages.Package
	packageErrors := 0
	var loadNotes []string
	if goFiles > 0 {
		pkgs, packageErrors, loadNotes, err = i.loadPackages(ctx, nil, i.patterns)
		if err != nil {
			return ports.GraphSyncResult{}, err
		}
	}
	goBatch, err := i.extractPackages(ctx, pkgs, nil)
	if err != nil {
		return ports.GraphSyncResult{}, err
	}
	polyBatch, err := i.extractPolyglot(ctx, scan.Files, nil)
	if err != nil {
		return ports.GraphSyncResult{}, err
	}
	batch := mergeBatches(goBatch, polyBatch.result())
	unresolved := 0
	for _, count := range batch.Unresolved {
		unresolved += count
	}
	now := i.now()
	result := ports.GraphSyncResult{
		Engine:  Engine,
		Project: i.label(project),
		Changed: len(scan.Files),
		Notes:   []string{},
	}
	files := make([]fileState, 0, len(scan.Files))
	for path, state := range scan.Files {
		if pkgPath := batch.Files[path]; pkgPath != "" {
			state.Package = pkgPath
		}
		files = append(files, state)
	}
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	meta := map[string]string{
		metaSchema:     fmt.Sprint(SchemaVersion),
		metaExtractor:  fmt.Sprint(ExtractorVersion),
		metaEngine:     Engine,
		metaIndexedAt:  fmt.Sprint(now.UnixMilli()),
		metaState:      stateComplete,
		metaUnresolved: fmt.Sprint(unresolved),
		metaSkipped:    fmt.Sprint(scan.Skipped),
		metaPkgErrors:  fmt.Sprint(packageErrors),
		metaPartial:    fmt.Sprint(polyBatch.Partial),
		metaGitSHA:     gitHead(i.root),
	}
	if gitDirty(i.root) {
		meta[metaGitDirty] = "1"
	} else {
		meta[metaGitDirty] = "0"
	}
	if i.module != "" {
		meta[metaModule] = i.module
	}
	if label := i.label(project); label != "" {
		meta[metaProject] = label
	}
	err = i.writeAll(ctx, files, batch, meta, now.UnixMilli())
	if err != nil {
		return ports.GraphSyncResult{}, err
	}
	result.Changed = len(files)
	result.Notes = append(result.Notes, fmt.Sprintf(
		"indexed %d file(s), %d symbol(s), %d edge(s) in %s",
		len(files), len(batch.Symbols), len(batch.Edges), time.Since(started).Round(time.Millisecond)))
	if len(polyBatch.Files) > 0 {
		note := fmt.Sprintf("%d TypeScript/JavaScript file(s) extracted with tree-sitter", len(polyBatch.Files))
		if polyBatch.Partial > 0 {
			note += fmt.Sprintf("; %d had syntax errors and their ERROR-span declarations were skipped", polyBatch.Partial)
		}
		result.Notes = append(result.Notes, note)
	}
	if reason != "" {
		result.Notes = append(result.Notes, reason)
	}
	result.Notes = append(result.Notes, scan.Notes...)
	result.Notes = append(result.Notes, loadNotes...)
	if i.module == "" && goFiles > 0 {
		result.Notes = append(result.Notes, "no go.mod was found above the checkout; qualified names keep the full package path")
	}
	if goFiles > 0 && len(pkgs) == 0 {
		result.Notes = append(result.Notes, "no Go packages were loaded; the store holds files but no Go symbols")
	}
	i.needsRebuild = false
	i.notes = nil
	return result, nil
}

// Sync re-extracts only the packages whose files changed. A change that
// removes or renames symbols escalates to a full rebuild: incoming edges from
// unchanged packages would otherwise point at ids that no longer exist.
func (i *Index) Sync(ctx context.Context, project string, paths []string) (ports.GraphSyncResult, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	started := time.Now()
	if i.needsRebuild {
		return i.indexLocked(ctx, project, "schema or extractor version changed; full rebuild")
	}
	if err := i.ensureSchemaIfNeeded(ctx); err != nil {
		return ports.GraphSyncResult{}, err
	}
	if current := readModulePath(i.root); current != i.module {
		i.module = current
		return i.indexLocked(ctx, project, "the module path changed; full rebuild")
	}
	previous, err := i.fileStates(ctx)
	if err != nil {
		return ports.GraphSyncResult{}, err
	}
	filter := normalizeFilter(paths)
	if len(previous) == 0 && len(filter) == 0 {
		return i.indexLocked(ctx, project, "empty store; full index")
	}
	scan, err := i.scan(ctx, previous)
	if err != nil {
		return ports.GraphSyncResult{}, err
	}
	changed := make([]string, 0, 16)
	for path, state := range scan.Files {
		if len(filter) > 0 && !pathMatchesAny(path, filter) {
			continue
		}
		old, ok := previous[path]
		if !ok || old.SHA256 != state.SHA256 || old.Size != state.Size {
			changed = append(changed, path)
		}
	}
	deleted := make([]string, 0, 8)
	for path := range previous {
		if len(filter) > 0 && !pathMatchesAny(path, filter) {
			continue
		}
		if _, ok := scan.Files[path]; !ok {
			deleted = append(deleted, path)
		}
	}
	sort.Strings(changed)
	sort.Strings(deleted)
	result := ports.GraphSyncResult{
		Engine:  Engine,
		Project: i.label(project),
		Changed: len(changed) + len(deleted),
		Notes:   []string{},
	}
	if len(changed) == 0 && len(deleted) == 0 {
		result.Notes = append(result.Notes, "no indexed file changed")
		return result, nil
	}
	goChanged, polyChanged := splitByLanguage(changed)
	goDeleted, polyDeleted := splitByLanguage(deleted)
	if len(polyChanged)+len(polyDeleted) > 0 {
		if len(goChanged)+len(goDeleted) > 0 {
			result.Notes = append(result.Notes, "Go and TypeScript/JavaScript files changed together; full rebuild")
			rebuilt, rebuildErr := i.indexLocked(ctx, project, "")
			if rebuildErr != nil {
				return ports.GraphSyncResult{}, rebuildErr
			}
			rebuilt.Changed = result.Changed
			rebuilt.Notes = append(result.Notes, rebuilt.Notes...)
			return rebuilt, nil
		}
		return i.syncPolyglotLocked(ctx, scan, polyChanged, polyDeleted, result, started)
	}
	oldIDs, err := i.symbolIDsForFiles(ctx, append(append([]string(nil), changed...), deleted...))
	if err != nil {
		return ports.GraphSyncResult{}, err
	}
	known, err := i.knownSymbolIDs(ctx)
	if err != nil {
		return ports.GraphSyncResult{}, err
	}
	// The old versions of changed files must not satisfy an edge: the new
	// symbol set is the only valid target set for those files.
	for _, id := range oldIDs {
		delete(known, id)
	}
	dirs := packageDirs(changed)
	var pkgs []*packages.Package
	packageErrors := 0
	var loadNotes []string
	if len(dirs) > 0 {
		var loadErr error
		pkgs, packageErrors, loadNotes, loadErr = i.loadPackages(ctx, dirs, nil)
		if loadErr != nil {
			return ports.GraphSyncResult{}, loadErr
		}
	}
	batch, err := i.extractPackages(ctx, pkgs, known)
	if err != nil {
		return ports.GraphSyncResult{}, err
	}
	removed := make([]string, 0, 8)
	batchIDs := make(map[string]bool, len(batch.Symbols))
	for _, symbol := range batch.Symbols {
		batchIDs[symbol.ID] = true
	}
	for _, id := range oldIDs {
		if !batchIDs[id] {
			removed = append(removed, id)
		}
	}
	if len(removed) > 0 {
		result.Notes = append(result.Notes, fmt.Sprintf(
			"%d symbol(s) were removed or renamed; rebuilding to relink incoming edges", len(removed)))
		rebuilt, rebuildErr := i.indexLocked(ctx, project, "")
		if rebuildErr != nil {
			return ports.GraphSyncResult{}, rebuildErr
		}
		rebuilt.Changed = result.Changed
		rebuilt.Notes = append(result.Notes, rebuilt.Notes...)
		return rebuilt, nil
	}
	now := i.now().UnixMilli()
	newFiles := make([]fileState, 0, len(changed))
	for _, path := range changed {
		state := scan.Files[path]
		if pkgPath := batch.Files[path]; pkgPath != "" {
			state.Package = pkgPath
		}
		newFiles = append(newFiles, state)
	}
	packages := map[string]bool{}
	for _, path := range changed {
		if old, ok := previous[path]; ok && old.Package != "" {
			packages[old.Package] = true
		}
		if pkgPath := batch.Files[path]; pkgPath != "" {
			packages[pkgPath] = true
		}
	}
	err = i.writeIncremental(ctx, newFiles, deleted, changed, oldIDs, sortedStringSet(packages), batch, now, packageErrors, scan.Skipped)
	if err != nil {
		return ports.GraphSyncResult{}, err
	}
	result.Notes = append(result.Notes, fmt.Sprintf(
		"re-extracted %d package(s) for %d changed and %d removed file(s) in %s",
		len(pkgs), len(changed), len(deleted), time.Since(started).Round(time.Millisecond)))
	result.Notes = append(result.Notes, scan.Notes...)
	result.Notes = append(result.Notes, loadNotes...)
	return result, nil
}

// fileStates reads the indexed file rows.
func (i *Index) fileStates(ctx context.Context) (map[string]fileState, error) {
	rows, err := i.db.QueryContext(ctx, `SELECT path, sha256, size, mtime_ns, package, language FROM files`)
	if err != nil {
		return nil, fmt.Errorf("codeindex: read indexed files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]fileState{}
	for rows.Next() {
		var state fileState
		if err := rows.Scan(&state.Path, &state.SHA256, &state.Size, &state.MTimeNS, &state.Package, &state.Language); err != nil {
			return nil, fmt.Errorf("codeindex: read indexed files: %w", err)
		}
		out[state.Path] = state
	}
	return out, rows.Err()
}

// knownSymbolIDs reads every symbol id in the store.
func (i *Index) knownSymbolIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := i.db.QueryContext(ctx, `SELECT id FROM symbols`)
	if err != nil {
		return nil, fmt.Errorf("codeindex: read symbol ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("codeindex: read symbol ids: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// symbolIDsForFiles reads the ids of the symbols defined in the given paths.
func (i *Index) symbolIDsForFiles(ctx context.Context, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(paths))
	for _, chunk := range chunkStrings(uniqueStrings(paths), sqlChunk) {
		query := `SELECT id FROM symbols WHERE path IN (` + placeholders(len(chunk)) + `)`
		rows, err := i.db.QueryContext(ctx, query, stringsToAny(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("codeindex: read symbol ids for changed files: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("codeindex: read symbol ids for changed files: %w", err)
			}
			out = append(out, id)
		}
		rowErr := rows.Err()
		_ = rows.Close()
		if rowErr != nil {
			return nil, fmt.Errorf("codeindex: read symbol ids for changed files: %w", rowErr)
		}
	}
	sort.Strings(out)
	return out, nil
}

// writeAll replaces the store contents in one transaction.
func (i *Index) writeAll(ctx context.Context, files []fileState, batch batchResult, meta map[string]string, indexedAt int64) error {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("codeindex: begin rebuild: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{`DELETE FROM edges`, `DELETE FROM symbols`, `DELETE FROM files`, `DELETE FROM meta`} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("codeindex: clear store: %w", err)
		}
	}
	if err := insertFiles(ctx, tx, files, batch.Unresolved, indexedAt); err != nil {
		return err
	}
	if err := insertSymbols(ctx, tx, batch.Symbols); err != nil {
		return err
	}
	if err := insertEdges(ctx, tx, batch.Edges); err != nil {
		return err
	}
	communities, err := rebuildCommunities(ctx, tx)
	if err != nil {
		return err
	}
	meta[metaCommunities] = fmt.Sprint(communities)
	meta[metaCommunityAlgo] = CommunityAlgorithm
	if err := rebuildFTS(ctx, tx); err != nil {
		return err
	}
	if err := setMeta(ctx, tx, meta); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("codeindex: commit rebuild: %w", err)
	}
	return nil
}

// writeIncremental updates only the changed and removed files.
func (i *Index) writeIncremental(ctx context.Context, files []fileState, deleted, changed, oldIDs, packages []string, batch batchResult, indexedAt int64, packageErrors, skipped int) error {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("codeindex: begin sync: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, chunk := range chunkStrings(oldIDs, sqlChunk) {
		query := `DELETE FROM edges WHERE source IN (` + placeholders(len(chunk)) + `)`
		if _, err := tx.ExecContext(ctx, query, stringsToAny(chunk)...); err != nil {
			return fmt.Errorf("codeindex: delete stale edges: %w", err)
		}
	}
	// Import edges hang off the package node, which is not tied to one
	// changed file: delete them by package so a removed import disappears.
	for _, pkgPath := range packages {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM edges WHERE source = ? AND kind = 'imports'`, PackageID(pkgPath)); err != nil {
			return fmt.Errorf("codeindex: delete stale imports: %w", err)
		}
	}
	for _, chunk := range chunkStrings(uniqueStrings(append(append([]string(nil), changed...), deleted...)), sqlChunk) {
		query := `DELETE FROM symbols WHERE path IN (` + placeholders(len(chunk)) + `)`
		if _, err := tx.ExecContext(ctx, query, stringsToAny(chunk)...); err != nil {
			return fmt.Errorf("codeindex: delete stale symbols: %w", err)
		}
	}
	if len(deleted) > 0 {
		for _, chunk := range chunkStrings(deleted, sqlChunk) {
			query := `DELETE FROM files WHERE path IN (` + placeholders(len(chunk)) + `)`
			if _, err := tx.ExecContext(ctx, query, stringsToAny(chunk)...); err != nil {
				return fmt.Errorf("codeindex: delete removed files: %w", err)
			}
		}
		// A package that lost its last file must not keep an orphan node.
		if _, err := tx.ExecContext(ctx, `DELETE FROM symbols
			WHERE kind = 'package' AND NOT EXISTS (SELECT 1 FROM files WHERE files.package = symbols.package)`); err != nil {
			return fmt.Errorf("codeindex: delete orphan packages: %w", err)
		}
	}
	if err := insertFiles(ctx, tx, files, batch.Unresolved, indexedAt); err != nil {
		return err
	}
	if err := insertSymbols(ctx, tx, batch.Symbols); err != nil {
		return err
	}
	if err := insertEdges(ctx, tx, batch.Edges); err != nil {
		return err
	}
	communities, err := rebuildCommunities(ctx, tx)
	if err != nil {
		return err
	}
	if err := rebuildFTS(ctx, tx); err != nil {
		return err
	}
	var unresolved int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(unresolved), 0) FROM files`).Scan(&unresolved); err != nil {
		return fmt.Errorf("codeindex: count unresolved references: %w", err)
	}
	meta := map[string]string{
		metaIndexedAt:     fmt.Sprint(indexedAt),
		metaState:         stateComplete,
		metaUnresolved:    fmt.Sprint(unresolved),
		metaPkgErrors:     fmt.Sprint(packageErrors),
		metaSkipped:       fmt.Sprint(skipped),
		metaCommunities:   fmt.Sprint(communities),
		metaCommunityAlgo: CommunityAlgorithm,
	}
	if sha := gitHead(i.root); sha != "" {
		meta[metaGitSHA] = sha
	}
	if gitDirty(i.root) {
		meta[metaGitDirty] = "1"
	} else {
		meta[metaGitDirty] = "0"
	}
	if err := setMeta(ctx, tx, meta); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("codeindex: commit sync: %w", err)
	}
	return nil
}

// insertFiles upserts file rows with their unresolved-reference counts.
func insertFiles(ctx context.Context, tx *sql.Tx, files []fileState, unresolved map[string]int, indexedAt int64) error {
	statement, err := tx.PrepareContext(ctx, `INSERT INTO files(path, sha256, size, mtime_ns, package, language, indexed_at, unresolved)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			sha256 = excluded.sha256, size = excluded.size, mtime_ns = excluded.mtime_ns,
			package = excluded.package, language = excluded.language,
			indexed_at = excluded.indexed_at, unresolved = excluded.unresolved`)
	if err != nil {
		return fmt.Errorf("codeindex: prepare file insert: %w", err)
	}
	defer func() { _ = statement.Close() }()
	for _, file := range files {
		language := file.Language
		if language == "" {
			language = Language
		}
		if _, err := statement.ExecContext(ctx, file.Path, file.SHA256, file.Size, file.MTimeNS,
			file.Package, language, indexedAt, unresolved[file.Path]); err != nil {
			return fmt.Errorf("codeindex: write file %s: %w", file.Path, err)
		}
	}
	return nil
}

// insertSymbols writes symbol rows in one prepared statement.
func insertSymbols(ctx context.Context, tx *sql.Tx, symbols []symbolRow) error {
	if len(symbols) == 0 {
		return nil
	}
	statement, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO symbols(
		id, path, package, kind, name, qualname, start_line, end_line, signature, doc, exported)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("codeindex: prepare symbol insert: %w", err)
	}
	defer func() { _ = statement.Close() }()
	for _, symbol := range symbols {
		if _, err := statement.ExecContext(ctx, symbol.ID, symbol.Path, symbol.Package, symbol.Kind,
			symbol.Name, symbol.QualName, symbol.StartLine, symbol.EndLine, symbol.Signature,
			symbol.Doc, boolInt(symbol.Exported)); err != nil {
			return fmt.Errorf("codeindex: write symbol %s: %w", symbol.QualName, err)
		}
	}
	return nil
}

// insertEdges writes edge rows; a duplicate primary key is ignored.
func insertEdges(ctx context.Context, tx *sql.Tx, edges []edgeRow) error {
	if len(edges) == 0 {
		return nil
	}
	statement, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO edges(
		source, target, kind, line, confidence, provenance) VALUES(?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("codeindex: prepare edge insert: %w", err)
	}
	defer func() { _ = statement.Close() }()
	for _, edge := range edges {
		if _, err := statement.ExecContext(ctx, edge.Source, edge.Target, edge.Kind, edge.Line,
			edge.Confidence, edge.Provenance); err != nil {
			return fmt.Errorf("codeindex: write edge %s: %w", edge.Kind, err)
		}
	}
	return nil
}

// rebuildFTS mirrors the symbols table into the full-text index. A rebuild is
// cheaper than per-row trigger maintenance during a batch.
func rebuildFTS(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO symbols_fts(symbols_fts) VALUES('rebuild')`); err != nil {
		return fmt.Errorf("codeindex: rebuild symbol index: %w", err)
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// chunkStrings splits values into deterministic batches.
func chunkStrings(values []string, size int) [][]string {
	if size <= 0 {
		size = len(values)
	}
	out := make([][]string, 0, (len(values)+size-1)/size)
	for start := 0; start < len(values); start += size {
		end := start + size
		if end > len(values) {
			end = len(values)
		}
		out = append(out, values[start:end])
	}
	return out
}

// uniqueStrings drops duplicates while keeping the first occurrence.
func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// sortedStringSet returns the sorted keys of a set.
func sortedStringSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
