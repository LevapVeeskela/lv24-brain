package codeindex

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// syncPolyglotLocked refreshes TypeScript, JavaScript and Python files without
// touching the Go half of the store. A changed file is re-extracted together
// with the files that import it, so an import edge and a cross-file call edge
// are re-resolved instead of pointing at a removed definition.
func (i *Index) syncPolyglotLocked(ctx context.Context, scan scanResult, changed, deleted []string, result ports.GraphSyncResult, started time.Time) (ports.GraphSyncResult, error) {
	reextract := map[string]bool{}
	for _, filePath := range changed {
		reextract[filePath] = true
	}
	dependents, err := i.polyglotImporters(ctx, append(append([]string(nil), changed...), deleted...), scan.Files)
	if err != nil {
		return ports.GraphSyncResult{}, err
	}
	for _, filePath := range dependents {
		reextract[filePath] = true
	}
	states := make(map[string]fileState, len(reextract))
	for filePath := range reextract {
		if state, ok := scan.Files[filePath]; ok {
			states[filePath] = state
		}
	}
	resolver, err := i.loadPolyglotResolver(ctx)
	if err != nil {
		return ports.GraphSyncResult{}, err
	}
	for filePath := range reextract {
		resolver.removeFile(filePath)
	}
	for _, filePath := range deleted {
		resolver.removeFile(filePath)
	}
	batch, err := i.extractPolyglot(ctx, states, resolver)
	if err != nil {
		return ports.GraphSyncResult{}, err
	}
	reextracted := make([]string, 0, len(reextract))
	for filePath := range reextract {
		reextracted = append(reextracted, filePath)
	}
	sort.Strings(reextracted)
	oldIDs, err := i.symbolIDsForFiles(ctx, reextracted)
	if err != nil {
		return ports.GraphSyncResult{}, err
	}
	newFiles := make([]fileState, 0, len(changed))
	for _, filePath := range changed {
		state := scan.Files[filePath]
		newFiles = append(newFiles, state)
	}
	sort.Slice(newFiles, func(left, right int) bool { return newFiles[left].Path < newFiles[right].Path })
	now := i.now()
	if err := i.writePolyglotIncremental(ctx, newFiles, deleted, changed, oldIDs, batch, now.UnixMilli(), scan.Skipped); err != nil {
		return ports.GraphSyncResult{}, err
	}
	result.Notes = append(result.Notes, fmt.Sprintf(
		"re-extracted %d TypeScript/JavaScript/Python file(s) for %d changed and %d removed, plus %d importer(s) relinked in %s",
		len(batch.Files), len(changed), len(deleted), len(dependents), time.Since(started).Round(time.Millisecond)))
	if batch.Partial > 0 {
		result.Notes = append(result.Notes, fmt.Sprintf(
			"%d re-extracted file(s) had syntax errors; declarations in ERROR spans were skipped", batch.Partial))
	}
	result.Notes = append(result.Notes, scan.Notes...)
	return result, nil
}

// polyglotImporters lists indexed files that import one of the given paths.
// Only files still present in the scan are returned; a deleted importer is
// already covered by its own delete.
func (i *Index) polyglotImporters(ctx context.Context, paths []string, states map[string]fileState) ([]string, error) {
	moduleIDs := make([]string, 0, len(paths))
	for _, filePath := range paths {
		moduleIDs = append(moduleIDs, SymbolID(filePath, filePath))
	}
	seen := map[string]bool{}
	for _, chunk := range chunkStrings(uniqueStrings(moduleIDs), sqlChunk) {
		query := `SELECT DISTINCT s.path FROM edges e JOIN symbols s ON s.id = e.source
			WHERE e.target IN (` + placeholders(len(chunk)) + `) AND e.kind = 'imports'`
		rows, err := i.db.QueryContext(ctx, query, stringsToAny(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("codeindex: read script importers: %w", err)
		}
		for rows.Next() {
			var filePath string
			if err := rows.Scan(&filePath); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("codeindex: read script importers: %w", err)
			}
			if _, ok := states[filePath]; ok && IsPolyglotPath(filePath) {
				seen[filePath] = true
			}
		}
		rowErr := rows.Err()
		_ = rows.Close()
		if rowErr != nil {
			return nil, fmt.Errorf("codeindex: read script importers: %w", rowErr)
		}
	}
	out := make([]string, 0, len(seen))
	for filePath := range seen {
		out = append(out, filePath)
	}
	sort.Strings(out)
	return out, nil
}

// loadPolyglotResolver seeds the resolver from the store. Go symbols are
// included in the name map because a path lookup is always qualified; only
// module rows mark a file as a resolvable script module.
func (i *Index) loadPolyglotResolver(ctx context.Context) (*polyglotResolver, error) {
	resolver := newPolyglotResolver()
	rows, err := i.db.QueryContext(ctx,
		`SELECT path, id, kind, name, signature, exported FROM symbols WHERE kind <> 'package'`)
	if err != nil {
		return nil, fmt.Errorf("codeindex: read script symbols: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var path, id, kind, name, signature string
		var exported int
		if err := rows.Scan(&path, &id, &kind, &name, &signature, &exported); err != nil {
			return nil, fmt.Errorf("codeindex: read script symbols: %w", err)
		}
		if kind == "module" {
			resolver.files[path] = true
			continue
		}
		if resolver.byPath[path] == nil {
			resolver.byPath[path] = map[string][]polyglotSymbolRef{}
		}
		ref := polyglotSymbolRef{
			id:        id,
			kind:      kind,
			signature: signature,
			exported:  exported != 0,
			defaulted: strings.HasPrefix(signature, "export default "),
		}
		resolver.byPath[path][name] = append(resolver.byPath[path][name], ref)
		resolver.byID[id] = ref
		resolver.idsByPath[path] = append(resolver.idsByPath[path], id)
	}
	return resolver, rows.Err()
}

// writePolyglotIncremental updates the script half of the store in one
// transaction. A symbol removed from a changed file is not left referenced:
// dangling edges are deleted after the new rows are inserted.
func (i *Index) writePolyglotIncremental(ctx context.Context, files []fileState, deleted, changed, oldIDs []string, batch polyglotBatch, indexedAt int64, skipped int) error {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("codeindex: begin script sync: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, chunk := range chunkStrings(oldIDs, sqlChunk) {
		query := `DELETE FROM edges WHERE source IN (` + placeholders(len(chunk)) + `)`
		if _, err := tx.ExecContext(ctx, query, stringsToAny(chunk)...); err != nil {
			return fmt.Errorf("codeindex: delete stale script edges: %w", err)
		}
	}
	for _, chunk := range chunkStrings(uniqueStrings(append(append([]string(nil), changed...), deleted...)), sqlChunk) {
		query := `DELETE FROM symbols WHERE path IN (` + placeholders(len(chunk)) + `)`
		if _, err := tx.ExecContext(ctx, query, stringsToAny(chunk)...); err != nil {
			return fmt.Errorf("codeindex: delete stale script symbols: %w", err)
		}
	}
	for _, chunk := range chunkStrings(deleted, sqlChunk) {
		query := `DELETE FROM files WHERE path IN (` + placeholders(len(chunk)) + `)`
		if _, err := tx.ExecContext(ctx, query, stringsToAny(chunk)...); err != nil {
			return fmt.Errorf("codeindex: delete removed script files: %w", err)
		}
	}
	if err := insertFiles(ctx, tx, files, batch.Unresolved, indexedAt); err != nil {
		return err
	}
	if err := insertSymbols(ctx, tx, batch.Symbols); err != nil {
		return err
	}
	edges := make([]edgeRow, 0, len(batch.Edges))
	for _, edge := range batch.Edges {
		edges = append(edges, edge)
	}
	if err := insertEdges(ctx, tx, edges); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM edges WHERE source NOT IN (SELECT id FROM symbols) OR target NOT IN (SELECT id FROM symbols)`); err != nil {
		return fmt.Errorf("codeindex: delete dangling script edges: %w", err)
	}
	for filePath, unresolved := range batch.Unresolved {
		if _, err := tx.ExecContext(ctx, `UPDATE files SET unresolved = ? WHERE path = ?`, unresolved, filePath); err != nil {
			return fmt.Errorf("codeindex: update unresolved count for %s: %w", filePath, err)
		}
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
		return fmt.Errorf("codeindex: commit script sync: %w", err)
	}
	return nil
}
