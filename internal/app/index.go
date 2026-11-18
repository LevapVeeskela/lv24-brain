package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/gitinfo"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// IndexOptions controls a local index refresh.
type IndexOptions struct {
	// Rebuild records whether the caller reset the SQLite database first.
	Rebuild bool
}

// IndexResult reports how many records were indexed.
type IndexResult struct {
	Projects  int
	States    int
	Notes     int
	Decisions int
	Attempts  int
	Handoffs  int
	Dumps     int
	// Items counts live raw-zone items (connectors/<id>/items).
	Items     int
	Documents int
	// Pruned counts documents removed because their source is gone.
	Pruned  int
	Rebuild bool
	// Warnings reports ambiguous registry data the indexer worked around
	// instead of guessing, for example one repository claimed by two projects.
	Warnings []string
}

// Reindex rebuilds the local search projection from Markdown files. The caller
// is responsible for deleting an existing database when Rebuild is true; this
// use case only upserts documents.
func (a *App) Reindex(ctx context.Context, opts IndexOptions) (IndexResult, error) {
	if a.cfg.Mode == config.ModeServer {
		return IndexResult{}, fmt.Errorf("%w: index is local-only in server mode", domain.ErrUnsupported)
	}
	if a.deps.Store == nil {
		return IndexResult{}, fmt.Errorf("%w: brain data store is not available; check data_dir or run brain init", domain.ErrUnsupported)
	}
	if a.deps.Index == nil {
		return IndexResult{}, fmt.Errorf("%w: local search index is not available", domain.ErrUnsupported)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	result := IndexResult{Rebuild: opts.Rebuild}
	seen := map[string]struct{}{}
	// digest accumulates the projection of every written document, so the
	// generation names the exact content the index holds, not just its size.
	digest := make([]string, 0, 1024)
	// A reindex writes every document, so the writes go in chunks: one
	// transaction per document made a corpus rebuild pay a commit and a WAL
	// checkpoint per record instead of per few hundred records.
	batch := newIndexWriteBatch(ctx, a.deps.Index)
	upsert := func(doc ports.Document) error {
		if _, duplicate := seen[doc.ID]; duplicate {
			// A legacy duplicate id would make search pick an arbitrary
			// record. The listing returns records newest first, so keeping the
			// first document makes the projection deterministic; doctor
			// reports the duplicate as duplicate-id.
			return nil
		}
		seen[doc.ID] = struct{}{}
		projection := projectDocument(doc)
		digest = append(digest, doc.ID+"="+projection.ContentHash+"="+strconv.FormatInt(doc.UpdatedAt.UnixNano(), 10))
		return batch.add(doc)
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return result, err
	}
	result.Projects = len(projects)
	for _, project := range projects {
		state, stateErr := a.deps.Store.GetState(ctx, project.ID)
		switch {
		case stateErr == nil:
			if err := upsert(stateDocument(project, state)); err != nil {
				return result, fmt.Errorf("index state for %s: %w", project.ID, err)
			}
			result.States++
			result.Documents++
		case errors.Is(stateErr, domain.ErrNotFound):
		default:
			return result, stateErr
		}

		notes, err := a.listNotes(ctx, project.ID)
		if err != nil {
			return result, err
		}
		for _, note := range notes {
			if err := upsert(noteDocument(note)); err != nil {
				return result, fmt.Errorf("index note %s: %w", note.ID, err)
			}
			result.Notes++
			result.Documents++
		}

		decisions, err := a.listDecisions(ctx, project.ID)
		if err != nil {
			return result, err
		}
		for _, decision := range decisions {
			if err := upsert(decisionDocument(decision)); err != nil {
				return result, fmt.Errorf("index decision %s: %w", decision.ID, err)
			}
			result.Decisions++
			result.Documents++
		}

		attempts, err := a.listAttempts(ctx, project.ID)
		if err != nil {
			return result, err
		}
		for _, attempt := range attempts {
			if err := upsert(attemptDocument(attempt)); err != nil {
				return result, fmt.Errorf("index attempt %s: %w", attempt.ID, err)
			}
			result.Attempts++
			result.Documents++
		}

		handoffs, err := a.listHandoffs(ctx, project.ID)
		if err != nil {
			return result, err
		}
		for _, handoff := range handoffs {
			if err := upsert(handoffDocument(handoff)); err != nil {
				return result, fmt.Errorf("index handoff %s: %w", handoff.ID, err)
			}
			result.Handoffs++
			result.Documents++
		}
	}
	dumps, warnings, err := a.indexDumps(ctx, projects, upsert)
	result.Dumps = dumps
	result.Warnings = append(result.Warnings, warnings...)
	result.Documents += dumps
	if err != nil {
		return result, err
	}
	items, err := a.indexAllItems(ctx, upsert)
	result.Items = items
	result.Documents += items
	if err != nil {
		return result, err
	}
	if err := batch.flush(); err != nil {
		return result, err
	}
	pruned, err := a.pruneIndex(ctx, seen)
	result.Pruned = pruned
	if err != nil {
		return result, err
	}
	// The run left a fragmented full-text index and a large WAL; compacting
	// them is part of the rebuild, not of the next query.
	if compactor, ok := a.deps.Index.(indexCompactor); ok {
		if err := compactor.Optimize(ctx); err != nil {
			return result, fmt.Errorf("compact the search index: %w", err)
		}
	}
	if err := a.recordIndexGeneration(ctx, digest); err != nil {
		return result, err
	}
	a.clearIndexDirty()
	return result, nil
}

// Index metadata keys shared with sqliteindex. They are literals here because
// the use case layer does not import an adapter.
const (
	indexMetaGeneration    = "index_generation"
	indexMetaIndexedFrom   = "indexed_from"
	indexMetaSchemaVersion = "schema_version"
)

// gitDirtySuffix marks an indexed_from value taken while the data directory had
// uncommitted changes. It is part of the comparison: a commit without a
// reindex must report the index as stale even when HEAD itself did not move.
const gitDirtySuffix = "+dirty"

// indexMetaReader is the optional index capability that exposes the metadata
// written by the last completed reindex.
type indexMetaReader interface {
	Meta(ctx context.Context) (map[string]string, error)
}

// indexMetaWriter is the optional index capability that stores index metadata.
type indexMetaWriter interface {
	SetMeta(ctx context.Context, values map[string]string) error
}

// recordIndexGeneration stores the content digest of a completed reindex and
// the git state it was built from. An adapter without the meta capability
// keeps the reindex working; only the status cannot report freshness.
func (a *App) recordIndexGeneration(ctx context.Context, digest []string) error {
	writer, ok := a.deps.Index.(indexMetaWriter)
	if !ok {
		return nil
	}
	head, dirty := a.dataDirGitMarker(ctx)
	if err := writer.SetMeta(ctx, map[string]string{
		indexMetaGeneration:  projectionGeneration(digest),
		indexMetaIndexedFrom: gitMarker(head, dirty),
	}); err != nil {
		return fmt.Errorf("record the index generation: %w", err)
	}
	return nil
}

// projectionGeneration is the order-independent digest of the indexed
// documents. The same corpus produces the same value on every machine, so a
// generation mismatch means the content differs, not the run.
func projectionGeneration(entries []string) string {
	sorted := append([]string(nil), entries...)
	sort.Strings(sorted)
	hasher := sha256.New()
	for _, entry := range sorted {
		_, _ = hasher.Write([]byte(entry))
		_, _ = hasher.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hasher.Sum(nil))[:16]
}

// IndexStatus reports whether the search index still describes the data
// directory. The check is read-only: it compares the indexed_from marker with
// the current git HEAD and dirty flag, and never reindexes.
type IndexStatus struct {
	// Stale is set when the index was built from another HEAD (or dirty
	// state), when it has no generation, or when a schema upgrade emptied it.
	Stale bool
	// IndexedFrom is the marker recorded by the last completed reindex:
	// a short HEAD sha, with a "+dirty" suffix when the tree was dirty.
	IndexedFrom string
	// Head and Dirty describe the data directory right now.
	Head  string
	Dirty bool
	// Generation is the content digest of the indexed corpus.
	Generation string
	// SchemaVersion is the index schema version the database was opened with.
	SchemaVersion string
}

// IndexStatus compares the index metadata with the current git state of the
// data directory. It returns an error when no local index is available; a
// non-git data directory has no HEAD to compare, so the marker is only equal
// when both sides are empty.
func (a *App) IndexStatus(ctx context.Context) (IndexStatus, error) {
	if a.cfg.Mode == config.ModeServer {
		return IndexStatus{}, fmt.Errorf("%w: index is local-only in server mode", domain.ErrUnsupported)
	}
	if a.deps.Index == nil {
		return IndexStatus{}, fmt.Errorf("%w: local search index is not available", domain.ErrUnsupported)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var status IndexStatus
	if reader, ok := a.deps.Index.(indexMetaReader); ok {
		meta, err := reader.Meta(ctx)
		if err != nil {
			return IndexStatus{}, err
		}
		status.IndexedFrom = meta[indexMetaIndexedFrom]
		status.Generation = meta[indexMetaGeneration]
		status.SchemaVersion = meta[indexMetaSchemaVersion]
	}
	head, dirty := a.dataDirGitMarker(ctx)
	status.Head = head
	status.Dirty = dirty
	// A schema rebuild drops the meta table, so a missing generation is the
	// "index was recreated empty" signal; no adapter flag is needed and a
	// completed reindex clears it by writing the generation.
	status.Stale = status.Generation == "" || status.IndexedFrom != gitMarker(head, dirty)
	return status, nil
}

// dataDirGitMarker reads the data directory's short HEAD and dirty flag with
// the read-only git helper. A directory that is not a repository (or a missing
// git binary) yields empty values.
func (a *App) dataDirGitMarker(ctx context.Context) (string, bool) {
	dir := strings.TrimSpace(a.cfg.DataDir)
	if dir == "" {
		return "", false
	}
	summary := gitinfo.Read(ctx, dir)
	if !summary.IsRepo {
		return "", false
	}
	return summary.Head, summary.Dirty > 0
}

// gitMarker encodes a repository state as the indexed_from value. The empty
// string means "no repository here".
func gitMarker(head string, dirty bool) string {
	head = strings.TrimSpace(head)
	if head == "" {
		return ""
	}
	if dirty {
		return head + gitDirtySuffix
	}
	return head
}

// indexBatchSize bounds one reindex transaction: a few hundred documents
// commit together, so a corpus rebuild costs tens of transactions instead of one
// per record, without holding the write lock for the whole run.
const indexBatchSize = 500

// indexBatchWriter is the optional batch capability of the search index.
type indexBatchWriter interface {
	UpsertBatch(ctx context.Context, docs []ports.Document) error
}

// indexCompactor is the optional compaction capability of the search index: it
// merges the full-text segments and truncates the write-ahead log a reindex
// leaves behind.
type indexCompactor interface {
	Optimize(ctx context.Context) error
}

// indexWriteBatch buffers reindex documents and writes them in chunks. An index
// without the batch capability keeps the per-document path, so the reindex stays
// correct for any implementation.
type indexWriteBatch struct {
	ctx     context.Context
	index   ports.Index
	batcher indexBatchWriter
	pending []ports.Document
}

func newIndexWriteBatch(ctx context.Context, index ports.Index) *indexWriteBatch {
	batch := &indexWriteBatch{ctx: ctx, index: index}
	batch.batcher, _ = index.(indexBatchWriter)
	return batch
}

// add buffers one document and writes the chunk when it is full.
func (b *indexWriteBatch) add(doc ports.Document) error {
	b.pending = append(b.pending, doc)
	if len(b.pending) < indexBatchSize {
		return nil
	}
	return b.flush()
}

// flush writes every buffered document.
func (b *indexWriteBatch) flush() error {
	for len(b.pending) > 0 {
		size := minInt(len(b.pending), indexBatchSize)
		if b.batcher == nil {
			size = 1
		}
		if err := b.write(b.pending[:size]); err != nil {
			return err
		}
		b.pending = b.pending[size:]
	}
	return nil
}

// write persists one chunk in one transaction when the index supports batches.
func (b *indexWriteBatch) write(chunk []ports.Document) error {
	if writer, ok := b.batcher.(projectionBatchWriter); ok {
		contentHashes, stubs := projectionColumns(chunk)
		if err := writer.UpsertProjectionBatch(b.ctx, chunk, contentHashes, stubs); err != nil {
			return fmt.Errorf("index %d documents: %w", len(chunk), err)
		}
		return nil
	}
	if b.batcher != nil {
		if err := b.batcher.UpsertBatch(b.ctx, chunk); err != nil {
			return fmt.Errorf("index %d documents: %w", len(chunk), err)
		}
		return nil
	}
	for _, doc := range chunk {
		if err := upsertProjected(b.ctx, b.index, doc); err != nil {
			return err
		}
	}
	return nil
}

// pruneIndex deletes documents a complete reindex did not produce: records
// removed, renamed or quarantined since they were indexed.
func (a *App) pruneIndex(ctx context.Context, seen map[string]struct{}) (int, error) {
	lister, ok := a.deps.Index.(ports.IndexLister)
	if !ok {
		return 0, nil
	}
	ids, err := lister.IDs(ctx)
	if err != nil {
		return 0, err
	}
	pruned := 0
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		if err := a.deps.Index.Delete(ctx, id); err != nil {
			return pruned, fmt.Errorf("prune index %s: %w", id, err)
		}
		pruned++
	}
	return pruned, nil
}

// IndexDumps refreshes the connector dumps in the search index, for example
// after a connector run. It returns the number of indexed dump files.
func (a *App) IndexDumps(ctx context.Context) (int, error) {
	if a.deps.Store == nil || a.deps.Index == nil {
		return 0, nil
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	upsert := func(doc ports.Document) error { return a.upsertProjected(ctx, doc) }
	count, _, err := a.indexDumps(ctx, projects, upsert)
	if err != nil {
		a.markIndexDirty()
	}
	return count, err
}

// indexDumps indexes connector dumps and returns the ambiguous repository
// mappings it refused to guess. Reindex reports them; a connector-triggered
// refresh looks only at its own dumps, so it discards them.
func (a *App) indexDumps(ctx context.Context, projects []domain.Project, upsert func(ports.Document) error) (int, []string, error) {
	lister, ok := a.deps.Store.(ports.DumpLister)
	if !ok {
		return 0, nil, nil
	}
	dumps, err := lister.ListDumps(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("list connector dumps: %w", err)
	}
	byRepo, warnings := projectsByRepo(projects)
	for i, dump := range dumps {
		if err := upsert(dumpDocument(dump, byRepo)); err != nil {
			return i, warnings, fmt.Errorf("index dump %s: %w", dump.Path, err)
		}
	}
	return len(dumps), warnings, nil
}

// dumpDocument indexes a dump under the registered project whose repository
// it mirrors ("github:owner/repo" matches a repo_url ending in owner/repo),
// else under the dump's own project id ("mail:account").
func dumpDocument(dump ports.Dump, byRepo map[string]string) ports.Document {
	projectID := dump.ProjectID
	if _, repo, ok := strings.Cut(dump.ProjectID, ":"); ok {
		if id, found := byRepo[strings.ToLower(repo)]; found {
			projectID = id
		}
	}
	return ports.Document{
		ID:        "dump:" + dump.Path,
		ProjectID: projectID,
		Kind:      KindDump,
		Title:     dump.Title,
		Body:      dump.Body,
		UpdatedAt: dump.UpdatedAt,
	}
}

// KindItem is the index kind of raw-zone items. Like dumps, items are left
// out of curated search unless dumps are requested or kinds names "item".
const KindItem = "item"

// itemDocumentPrefix starts the index id of every item: "item:<Item.Key()>".
const itemDocumentPrefix = "item:"

// itemPageSize is the page size used when the indexer walks a connection.
const itemPageSize = 500

// ItemZone is implemented by stores with a local raw zone (fsstore). The
// indexer discovers it by type assertion on Dependencies.Store, so stores
// without items (server mode) simply index none.
type ItemZone interface {
	// ItemConnections lists connection IDs that have stored items.
	ItemConnections(ctx context.Context) ([]string, error)
	// ItemZone returns the item store.
	ItemZone() ports.ItemStore
	// ItemByKey returns the item or tombstone stored under an Item.Key(),
	// or domain.ErrNotFound.
	ItemByKey(ctx context.Context, key string) (domain.Item, error)
}

// ItemIndexResult reports an incremental item index refresh.
type ItemIndexResult struct {
	// Indexed counts live items written to the index.
	Indexed int
	// Removed counts documents deleted for tombstoned, missing or (on a
	// whole-connection refresh) no longer stored items.
	Removed int
}

// IndexItems refreshes the index for items of one connection after a run.
// With keys, only those items are touched: live items are upserted,
// tombstones and unknown keys are removed from the index. Keys not belonging
// to connectionID are rejected. Without keys, the whole connection is
// re-indexed and documents of items no longer stored (after Purge) are
// pruned. Items absent from a partial read are never removed: only an
// explicit tombstone or a missing file deletes a document. Server mode and
// stores without a raw zone index nothing.
func (a *App) IndexItems(ctx context.Context, connectionID string, keys []string) (ItemIndexResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	zone, ok := a.deps.Store.(ItemZone)
	if !ok || a.deps.Index == nil || a.cfg.Mode == config.ModeServer {
		return ItemIndexResult{}, nil
	}
	if err := domain.ValidateConnectionID("connection.id", connectionID); err != nil {
		return ItemIndexResult{}, err
	}
	var (
		result ItemIndexResult
		err    error
	)
	if len(keys) == 0 {
		result, err = a.reindexConnection(ctx, zone, connectionID)
	} else {
		result, err = a.indexItemKeys(ctx, zone, connectionID, keys)
	}
	if err != nil {
		a.markIndexDirty()
	}
	return result, err
}

func (a *App) indexItemKeys(ctx context.Context, zone ItemZone, connectionID string, keys []string) (ItemIndexResult, error) {
	var result ItemIndexResult
	prefix := connectionID + "/"
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			return result, domain.NewValidationError("item.key", fmt.Sprintf("%q does not belong to connection %q", key, connectionID))
		}
	}
	for _, key := range keys {
		item, err := zone.ItemByKey(ctx, key)
		switch {
		case err == nil && !item.Deleted:
			if err := a.upsertProjected(ctx, itemDocument(item)); err != nil {
				return result, fmt.Errorf("index item %s: %w", key, err)
			}
			result.Indexed++
		case err == nil || errors.Is(err, domain.ErrNotFound):
			if err := a.deps.Index.Delete(ctx, itemDocumentPrefix+key); err != nil {
				return result, fmt.Errorf("remove item %s: %w", key, err)
			}
			result.Removed++
		default:
			return result, fmt.Errorf("read item %s: %w", key, err)
		}
	}
	return result, nil
}

func (a *App) reindexConnection(ctx context.Context, zone ItemZone, connectionID string) (ItemIndexResult, error) {
	var result ItemIndexResult
	seen := map[string]struct{}{}
	count, err := indexConnectionItems(ctx, zone.ItemZone(), connectionID, func(doc ports.Document) error {
		seen[doc.ID] = struct{}{}
		return a.upsertProjected(ctx, doc)
	})
	result.Indexed = count
	if err != nil {
		return result, err
	}
	lister, ok := a.deps.Index.(ports.IndexLister)
	if !ok {
		return result, nil
	}
	ids, err := lister.IDs(ctx)
	if err != nil {
		return result, err
	}
	prefix := itemDocumentPrefix + connectionID + "/"
	for _, id := range ids {
		if _, kept := seen[id]; kept || !strings.HasPrefix(id, prefix) {
			continue
		}
		if err := a.deps.Index.Delete(ctx, id); err != nil {
			return result, fmt.Errorf("prune index %s: %w", id, err)
		}
		result.Removed++
	}
	return result, nil
}

// indexAllItems upserts every live item of every connection (full reindex);
// the caller's prune removes documents of tombstoned or purged items.
func (a *App) indexAllItems(ctx context.Context, upsert func(ports.Document) error) (int, error) {
	zone, ok := a.deps.Store.(ItemZone)
	if !ok {
		return 0, nil
	}
	connections, err := zone.ItemConnections(ctx)
	if err != nil {
		return 0, fmt.Errorf("list item connections: %w", err)
	}
	total := 0
	for _, connectionID := range connections {
		count, err := indexConnectionItems(ctx, zone.ItemZone(), connectionID, upsert)
		total += count
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func indexConnectionItems(ctx context.Context, store ports.ItemStore, connectionID string, upsert func(ports.Document) error) (int, error) {
	count := 0
	cursor := ""
	for {
		page, err := store.List(ctx, connectionID, ports.ItemListOptions{Cursor: cursor, Limit: itemPageSize})
		if err != nil {
			return count, fmt.Errorf("list items of %s: %w", connectionID, err)
		}
		for _, item := range page.Items {
			if item.Deleted {
				continue
			}
			if err := upsert(itemDocument(item)); err != nil {
				return count, fmt.Errorf("index item %s: %w", item.Key(), err)
			}
			count++
		}
		if page.NextCursor == "" {
			return count, nil
		}
		cursor = page.NextCursor
	}
}

// itemDocument projects a live item. Title and body are searchable together
// with URL, author and labels; an unmapped item is filed under the synthetic
// project "connection:<id>", like unmapped dumps keep "mail:<account>".
func itemDocument(item domain.Item) ports.Document {
	projectID := strings.TrimSpace(item.ProjectID)
	if projectID == "" {
		projectID = "connection:" + item.Ref.ConnectionID
	}
	updated := item.UpdatedAt
	if updated.IsZero() {
		updated = updatedOrCreated(item.ObservedAt, item.CreatedAt)
	}
	return ports.Document{
		ID:        itemDocumentPrefix + item.Key(),
		ProjectID: projectID,
		Kind:      KindItem,
		Title:     fallbackTitle(item.Title, item.Body),
		Body:      joinText(item.Body, item.URL, item.Author, strings.Join(item.Labels, " ")),
		UpdatedAt: updated.UTC(),
	}
}

// projectsByRepo maps "owner/repo" (lowercase, from repo_url) to project id.
// A repository claimed by several projects is ambiguous: it maps to no project
// and a warning names the candidates in id order, so a dump is never
// attributed to whichever project the registry returned last. The dump then
// stays under its connector project ("github:owner/repo") until the owner
// merges the registry entries.
func projectsByRepo(projects []domain.Project) (map[string]string, []string) {
	owners := make(map[string][]string, len(projects))
	for _, project := range projects {
		if repo := repoPath(project.RepoURL); repo != "" {
			owners[repo] = append(owners[repo], project.ID)
		}
	}
	byRepo := make(map[string]string, len(owners))
	var warnings []string
	for repo, ids := range owners {
		sort.Strings(ids)
		if len(ids) > 1 {
			warnings = append(warnings, fmt.Sprintf(
				"repository %s is claimed by %s; dumps stay under their connector project until one owner is chosen",
				repo, strings.Join(ids, ", ")))
			continue
		}
		byRepo[repo] = ids[0]
	}
	sort.Strings(warnings)
	return byRepo, warnings
}

// repoPath extracts "owner/repo" from https or ssh remote URLs.
func repoPath(remote string) string {
	remote = strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	if remote == "" {
		return ""
	}
	if _, rest, ok := strings.Cut(remote, "://"); ok {
		remote = rest
		if _, path, found := strings.Cut(remote, "/"); found {
			return strings.ToLower(strings.Trim(path, "/"))
		}
		return ""
	}
	if _, path, ok := strings.Cut(remote, ":"); ok { // git@host:owner/repo
		return strings.ToLower(strings.Trim(path, "/"))
	}
	return ""
}

func stateDocument(project domain.Project, state domain.State) ports.Document {
	title := strings.TrimSpace(project.Name)
	if title == "" {
		title = project.ID
	}
	return ports.Document{
		ID:        "state:" + project.ID,
		ProjectID: project.ID,
		Kind:      "state",
		Title:     title + " state",
		Body: joinText(
			state.Goal,
			strings.Join(state.Active, "\n"),
			strings.Join(state.Blocked, "\n"),
			state.NextAction,
			strings.Join(state.OpenQuestions, "\n"),
		),
		UpdatedAt: updatedOrCreated(state.UpdatedAt, project.UpdatedAt),
	}
}

// indexKey is the search index id of a record. Record ids are unique only
// within their project (two projects may both have note-2026-01-01-setup), so
// the index key is scoped by the project. Project ids never contain "/".
// sqliteindex strips the scope again when it returns hits.
func indexKey(projectID, recordID string) string {
	return projectID + "/" + recordID
}

// MarkIndexDirty asks the next search to reindex the data directory. Bootstrap
// uses it after the index was recreated empty by a schema change.
func MarkIndexDirty(dataDir string) error {
	path := filepath.Join(dataDir, filepath.FromSlash(indexDirtyMarker))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("schema upgrade\n"), 0o644)
}
