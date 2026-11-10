package fsstore

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Raw zone layout (contracts-v1, WP0.2). Every connection owns one directory:
//
//	connectors/<connectionID>/.gitignore                 publish policy (see SetPublishPolicy)
//	connectors/<connectionID>/items/<40 hex>.md          one item or tombstone
//	connectors/<connectionID>/items.lock                 cross-process write lock
//	connectors/<connectionID>/checkpoints/<stream>.json  one checkpoint per stream
//
// The item file name is the hash part of Item.Key(), so the key is the path:
// "<connectionID>/<hex>" -> connectors/<connectionID>/items/<hex>.md. Files
// are YAML frontmatter (provenance, double-quoted so no value can break the
// "---" delimiter) plus the Markdown body; a tombstone keeps only ref,
// deleted, observed_at and hash, and has no body. connectors/ is excluded
// from CurrentRevision (see isExcludedRootDir), so item churn never changes
// the curated revision.
const (
	itemsDir          = "items"
	itemFileExt       = ".md"
	itemSchemaVersion = 1
	itemKeyHexLength  = 40

	defaultItemPageSize = 100
	maxItemPageSize     = 1000

	connectionIgnoreFile = ".gitignore"
	itemCursorPrefix     = "i1:"
)

// localOnlyIgnore is written to connectors/<id>/.gitignore for a local-only
// connection: "*" ignores everything in the directory, the file included, so
// `git add --all` in brain sync never stages its items or checkpoints.
const localOnlyIgnore = "# lv24-brain: local-only connection; nothing in this directory is committed.\n*\n"

// sharedIgnore is the default for other connections: items and checkpoints
// are shared, transient lock and temporary files are not.
const sharedIgnore = "# lv24-brain: shared connection; only lock and temporary files are ignored.\n*.lock\n.tmp-*\n"

// reservedConnectionIDs are connectors/ entries used by other features: a
// connection with such an ID would mix its items with them.
var reservedConnectionIDs = map[string]struct{}{connectorRunsDir: {}}

// ItemStore is the filesystem raw zone (ports.ItemStore). Writes of one
// connection serialize on connectors/<id>/items.lock; every file is replaced
// atomically (temporary file + rename, with Windows retries). Apply is not
// transactional: items are written in order and an error stops the batch
// midway, which the host contract tolerates (the checkpoint is not advanced,
// so the batch is replayed and already-written items come back unchanged).
type ItemStore struct {
	store *Store
}

var _ ports.ItemStore = (*ItemStore)(nil)

// Items returns the raw-zone item store of this data directory.
func (s *Store) Items() *ItemStore { return &ItemStore{store: s} }

// ItemZone returns Items() as the port; the indexer discovers it through this
// method so it does not depend on the concrete adapter.
func (s *Store) ItemZone() ports.ItemStore { return s.Items() }

// ItemConnections lists the connection IDs that have an items directory.
func (s *Store) ItemConnections(ctx context.Context) ([]string, error) {
	return s.Items().Connections(ctx)
}

// ItemByKey returns the item or tombstone stored under an Item.Key().
func (s *Store) ItemByKey(ctx context.Context, key string) (domain.Item, error) {
	return s.Items().GetByKey(ctx, key)
}

// itemFrontmatter is the on-disk provenance of one item. Times are RFC 3339
// strings with nanoseconds so the stored hash stays reproducible.
type itemFrontmatter struct {
	SchemaVersion int                `yaml:"schema_version"`
	Ref           itemRefFrontmatter `yaml:"ref"`
	Kind          string             `yaml:"kind,omitempty"`
	Title         string             `yaml:"title,omitempty"`
	URL           string             `yaml:"url,omitempty"`
	Author        string             `yaml:"author,omitempty"`
	State         string             `yaml:"state,omitempty"`
	Labels        []string           `yaml:"labels,omitempty"`
	ACL           []string           `yaml:"acl,omitempty"`
	Project       string             `yaml:"project,omitempty"`
	CreatedAt     string             `yaml:"created_at,omitempty"`
	UpdatedAt     string             `yaml:"updated_at,omitempty"`
	ObservedAt    string             `yaml:"observed_at,omitempty"`
	Hash          string             `yaml:"hash"`
	Deleted       bool               `yaml:"deleted"`
}

type itemRefFrontmatter struct {
	ConnectionID string `yaml:"connection_id"`
	ExternalID   string `yaml:"external_id"`
}

// Upsert stores a live item. A missing Hash is computed; an existing live
// item with the same Hash is left untouched (UpsertUnchanged). Upserting a
// tombstoned or unknown key reports UpsertCreated. Tombstones go through
// Delete or Apply.
func (s *ItemStore) Upsert(ctx context.Context, item domain.Item) (ports.UpsertOutcome, error) {
	if err := contextError(ctx); err != nil {
		return "", err
	}
	if item.Deleted {
		return "", domain.NewValidationError("item.deleted", "use Delete or Apply for tombstones")
	}
	prepared, err := prepareItem(item)
	if err != nil {
		return "", err
	}
	var outcome ports.UpsertOutcome
	err = s.withConnectionLock(ctx, prepared.Ref.ConnectionID, func() error {
		var writeErr error
		outcome, writeErr = s.upsertLocked(prepared)
		return writeErr
	})
	return outcome, err
}

// Delete replaces the item with a tombstone observed at at. An existing
// tombstone is not rewritten. It reports whether a live item existed.
func (s *ItemStore) Delete(ctx context.Context, ref domain.ItemRef, at time.Time) (bool, error) {
	if err := contextError(ctx); err != nil {
		return false, err
	}
	if err := ref.Validate(); err != nil {
		return false, err
	}
	var existed bool
	err := s.withConnectionLock(ctx, ref.ConnectionID, func() error {
		var writeErr error
		existed, _, writeErr = s.deleteLocked(ref, at)
		return writeErr
	})
	return existed, err
}

// Get returns the item or tombstone for ref, or domain.ErrNotFound.
func (s *ItemStore) Get(ctx context.Context, ref domain.ItemRef) (domain.Item, error) {
	if err := contextError(ctx); err != nil {
		return domain.Item{}, err
	}
	if err := ref.Validate(); err != nil {
		return domain.Item{}, err
	}
	return s.GetByKey(ctx, ref.Key())
}

// GetByKey returns the item or tombstone stored under key ("<conn>/<hex>"),
// or domain.ErrNotFound.
func (s *ItemStore) GetByKey(ctx context.Context, key string) (domain.Item, error) {
	if err := contextError(ctx); err != nil {
		return domain.Item{}, err
	}
	connectionID, hexPart, err := parseItemKey(key)
	if err != nil {
		return domain.Item{}, err
	}
	path, err := s.itemPath(connectionID, hexPart)
	if err != nil {
		return domain.Item{}, err
	}
	item, err := readItemFile(path, key)
	if errors.Is(err, fs.ErrNotExist) {
		return domain.Item{}, notFound("item", key)
	}
	return item, err
}

// List pages one connection's items ordered by Key. The cursor is the last
// returned key, so paging is stable under concurrent writes: items before the
// cursor are never repeated and existing items after it are never skipped.
// Files that do not parse or do not match their name are skipped.
func (s *ItemStore) List(ctx context.Context, connectionID string, opts ports.ItemListOptions) (ports.ItemPage, error) {
	if err := contextError(ctx); err != nil {
		return ports.ItemPage{}, err
	}
	dir, err := s.itemsDirFor(connectionID)
	if err != nil {
		return ports.ItemPage{}, err
	}
	limit, err := itemPageLimit(opts.Limit)
	if err != nil {
		return ports.ItemPage{}, err
	}
	after, err := decodeItemCursor(connectionID, opts.Cursor)
	if err != nil {
		return ports.ItemPage{}, err
	}
	names, err := itemFileHexes(dir)
	if err != nil {
		return ports.ItemPage{}, err
	}
	page := ports.ItemPage{Items: []domain.Item{}}
	for _, hexPart := range names {
		if hexPart <= after {
			continue
		}
		if err := contextError(ctx); err != nil {
			return ports.ItemPage{}, err
		}
		item, readErr := readItemFile(filepath.Join(dir, hexPart+itemFileExt), connectionID+"/"+hexPart)
		if readErr != nil || (item.Deleted && !opts.IncludeDeleted) {
			continue
		}
		if len(page.Items) == limit {
			page.NextCursor = encodeItemCursor(page.Items[limit-1].Key())
			break
		}
		page.Items = append(page.Items, item)
	}
	return page, nil
}

// Purge removes every item and tombstone of a connection. The items
// directory is first renamed aside, so a failed removal never leaves a
// half-purged directory visible; checkpoints and the publish policy stay.
func (s *ItemStore) Purge(ctx context.Context, connectionID string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	dir, err := s.itemsDirFor(connectionID)
	if err != nil {
		return err
	}
	return s.withConnectionLock(ctx, connectionID, func() error {
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		aside, err := os.MkdirTemp(filepath.Dir(dir), tempFilePrefix+"purge-*")
		if err != nil {
			return fmt.Errorf("fsstore: purge %s: %w", connectionID, err)
		}
		target := filepath.Join(aside, itemsDir)
		if err := renameFile(dir, target); err != nil {
			_ = os.Remove(aside)
			return fmt.Errorf("fsstore: purge %s: %w", connectionID, err)
		}
		if err := os.RemoveAll(aside); err != nil {
			return fmt.Errorf("fsstore: purge %s: %w", connectionID, err)
		}
		return nil
	})
}

// Apply stores a batch in order: live items are upserted, Deleted ones become
// tombstones at Item.ObservedAt. Every item is validated before anything is
// written. The batch is not atomic (see ItemStore). Outcomes align with
// items; a tombstone reports UpsertUpdated when it replaced a live item and
// UpsertUnchanged otherwise.
func (s *ItemStore) Apply(ctx context.Context, items []domain.Item) ([]ports.UpsertOutcome, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	prepared := make([]domain.Item, len(items))
	for index, item := range items {
		if item.Deleted {
			if err := item.Ref.Validate(); err != nil {
				return nil, fmt.Errorf("item %d: %w", index, err)
			}
			prepared[index] = item
			continue
		}
		ready, err := prepareItem(item)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", index, err)
		}
		prepared[index] = ready
	}
	outcomes := make([]ports.UpsertOutcome, 0, len(prepared))
	for start := 0; start < len(prepared); {
		connectionID := prepared[start].Ref.ConnectionID
		end := start
		for end < len(prepared) && prepared[end].Ref.ConnectionID == connectionID {
			end++
		}
		group := prepared[start:end]
		err := s.withConnectionLock(ctx, connectionID, func() error {
			for _, item := range group {
				if err := contextError(ctx); err != nil {
					return err
				}
				outcome, err := s.applyLocked(item)
				if err != nil {
					return err
				}
				outcomes = append(outcomes, outcome)
			}
			return nil
		})
		if err != nil {
			return outcomes, err
		}
		start = end
	}
	return outcomes, nil
}

// Connections lists connection IDs that have an items directory, sorted.
func (s *ItemStore) Connections(ctx context.Context) ([]string, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.store.root, connectorsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fsstore: list connections: %w", err)
	}
	ids := []string{}
	for _, entry := range entries {
		if !entry.IsDir() || validateItemConnectionID(entry.Name()) != nil {
			continue
		}
		info, err := os.Stat(filepath.Join(s.store.root, connectorsDir, entry.Name(), itemsDir))
		if err == nil && info.IsDir() {
			ids = append(ids, entry.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// SetPublishPolicy records where a connection's raw data may travel by
// writing connectors/<id>/.gitignore: local-only ignores the whole directory
// (items and checkpoints are never committed by brain sync); shared ignores
// only lock and temporary files. The host calls it before a connection's
// first run and whenever the policy changes. Files committed while the
// connection was shared stay tracked until removed from the index
// (git rm --cached); this store never runs git.
func (s *ItemStore) SetPublishPolicy(ctx context.Context, connectionID string, policy domain.PublishPolicy) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if !policy.Valid() {
		return domain.NewValidationError("connection.publish", fmt.Sprintf("unknown policy %q", policy))
	}
	dir, err := s.store.connectionDir(connectionID)
	if err != nil {
		return err
	}
	content := sharedIgnore
	if policy == domain.PublishLocalOnly {
		content = localOnlyIgnore
	}
	return writeFileAtomic(filepath.Join(dir, connectionIgnoreFile), []byte(content))
}

// PublishPolicy reads the policy SetPublishPolicy recorded. A directory
// without the local-only marker is shared.
func (s *ItemStore) PublishPolicy(ctx context.Context, connectionID string) (domain.PublishPolicy, error) {
	if err := contextError(ctx); err != nil {
		return "", err
	}
	dir, err := s.store.connectionDir(connectionID)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(dir, connectionIgnoreFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("fsstore: read publish policy of %s: %w", connectionID, err)
	}
	if string(raw) == localOnlyIgnore {
		return domain.PublishLocalOnly, nil
	}
	return domain.PublishShared, nil
}

func (s *ItemStore) upsertLocked(item domain.Item) (ports.UpsertOutcome, error) {
	path, err := s.itemPathForKey(item.Key())
	if err != nil {
		return "", err
	}
	outcome := ports.UpsertCreated
	existing, readErr := readItemFile(path, item.Key())
	switch {
	case readErr == nil && !existing.Deleted && existing.Hash == item.Hash:
		return ports.UpsertUnchanged, nil
	case readErr == nil && !existing.Deleted:
		outcome = ports.UpsertUpdated
	case readErr != nil && !errors.Is(readErr, fs.ErrNotExist):
		// A damaged file is repaired by the new content.
		outcome = ports.UpsertUpdated
	}
	if err := s.writeItem(path, item); err != nil {
		return "", err
	}
	return outcome, nil
}

// deleteLocked writes a tombstone unless one is already stored. It reports
// whether a live item existed and whether anything was written.
func (s *ItemStore) deleteLocked(ref domain.ItemRef, at time.Time) (bool, bool, error) {
	path, err := s.itemPathForKey(ref.Key())
	if err != nil {
		return false, false, err
	}
	existing, readErr := readItemFile(path, ref.Key())
	if readErr == nil && existing.Deleted {
		return false, false, nil
	}
	tombstone := domain.Item{Ref: ref, Deleted: true, ObservedAt: at.UTC()}.WithHash()
	if err := s.writeItem(path, tombstone); err != nil {
		return false, false, err
	}
	return readErr == nil, true, nil
}

func (s *ItemStore) applyLocked(item domain.Item) (ports.UpsertOutcome, error) {
	if !item.Deleted {
		return s.upsertLocked(item)
	}
	existed, _, err := s.deleteLocked(item.Ref, item.ObservedAt)
	if err != nil {
		return "", err
	}
	if existed {
		return ports.UpsertUpdated, nil
	}
	return ports.UpsertUnchanged, nil
}

func (s *ItemStore) writeItem(path string, item domain.Item) error {
	raw, err := encodeItem(item)
	if err != nil {
		return err
	}
	if err := s.ensureConnectionIgnore(item.Ref.ConnectionID); err != nil {
		return err
	}
	return writeFileAtomic(path, raw)
}

// ensureConnectionIgnore writes the shared default .gitignore when the
// connection has none yet, so lock and temporary files are never committed.
// An existing file (for example the local-only marker) is kept.
func (s *ItemStore) ensureConnectionIgnore(connectionID string) error {
	dir, err := s.store.connectionDir(connectionID)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, connectionIgnoreFile)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("fsstore: check %s: %w", path, err)
	}
	return writeFileAtomic(path, []byte(sharedIgnore))
}

func (s *ItemStore) withConnectionLock(ctx context.Context, connectionID string, fn func() error) error {
	dir, err := s.itemsDirFor(connectionID)
	if err != nil {
		return err
	}
	return withFileLock(ctx, dir, fn)
}

func (s *ItemStore) itemsDirFor(connectionID string) (string, error) {
	dir, err := s.store.connectionDir(connectionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, itemsDir), nil
}

func (s *ItemStore) itemPathForKey(key string) (string, error) {
	connectionID, hexPart, err := parseItemKey(key)
	if err != nil {
		return "", err
	}
	return s.itemPath(connectionID, hexPart)
}

func (s *ItemStore) itemPath(connectionID, hexPart string) (string, error) {
	dir, err := s.itemsDirFor(connectionID)
	if err != nil {
		return "", err
	}
	return s.store.ensureInsideRoot(filepath.Join(dir, hexPart+itemFileExt))
}

// connectionDir returns connectors/<id> after validating the ID and checking
// the path stays inside the data directory.
func (s *Store) connectionDir(connectionID string) (string, error) {
	if err := validateItemConnectionID(connectionID); err != nil {
		return "", err
	}
	return s.ensureInsideRoot(filepath.Join(s.root, connectorsDir, connectionID))
}

func validateItemConnectionID(connectionID string) error {
	if err := domain.ValidateConnectionID("connection.id", connectionID); err != nil {
		return err
	}
	if _, reserved := reservedConnectionIDs[connectionID]; reserved {
		return domain.NewValidationError("connection.id", fmt.Sprintf("%q is reserved under connectors/", connectionID))
	}
	if isWindowsReservedName(connectionID) {
		return domain.NewValidationError("connection.id", "is a reserved file name")
	}
	return nil
}

// prepareItem validates a live item and fills a missing Hash.
func prepareItem(item domain.Item) (domain.Item, error) {
	if item.Hash == "" {
		item = item.WithHash()
	}
	if err := item.Validate(); err != nil {
		return domain.Item{}, err
	}
	return item, nil
}

func parseItemKey(key string) (string, string, error) {
	connectionID, hexPart, ok := strings.Cut(key, "/")
	if !ok || !isLowerHex(hexPart, itemKeyHexLength) {
		return "", "", domain.NewValidationError("item.key", "must be <connectionID>/<40 hex>")
	}
	if err := domain.ValidateConnectionID("item.key", connectionID); err != nil {
		return "", "", err
	}
	return connectionID, hexPart, nil
}

func isLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func itemPageLimit(limit int) (int, error) {
	switch {
	case limit < 0:
		return 0, domain.NewValidationError("items.limit", "must not be negative")
	case limit == 0:
		return defaultItemPageSize, nil
	case limit > maxItemPageSize:
		return maxItemPageSize, nil
	default:
		return limit, nil
	}
}

// encodeItemCursor makes the last key of a page an opaque cursor.
func encodeItemCursor(key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(itemCursorPrefix + key))
}

// decodeItemCursor returns the hash part after which listing resumes; empty
// starts from the beginning.
func decodeItemCursor(connectionID, cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", domain.NewValidationError("items.cursor", "is not a valid cursor")
	}
	key, ok := strings.CutPrefix(string(raw), itemCursorPrefix)
	if !ok {
		return "", domain.NewValidationError("items.cursor", "is not a valid cursor")
	}
	cursorConnection, hexPart, err := parseItemKey(key)
	if err != nil || cursorConnection != connectionID {
		return "", domain.NewValidationError("items.cursor", "belongs to another connection")
	}
	return hexPart, nil
}

// itemFileHexes returns the hash parts of the item files in dir, sorted.
func itemFileHexes(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fsstore: list items: %w", err)
	}
	hexes := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		hexPart, ok := strings.CutSuffix(name, itemFileExt)
		if !entry.Type().IsRegular() || !ok || !isLowerHex(hexPart, itemKeyHexLength) {
			continue
		}
		hexes = append(hexes, hexPart)
	}
	sort.Strings(hexes)
	return hexes, nil
}

func encodeItem(item domain.Item) ([]byte, error) {
	front := itemFrontmatter{
		SchemaVersion: itemSchemaVersion,
		Ref:           itemRefFrontmatter{ConnectionID: item.Ref.ConnectionID, ExternalID: item.Ref.ExternalID},
		ObservedAt:    formatItemTime(item.ObservedAt),
		Hash:          item.Hash,
		Deleted:       item.Deleted,
	}
	body := ""
	if !item.Deleted {
		front.Kind = string(item.Kind)
		front.Title = item.Title
		front.URL = item.URL
		front.Author = item.Author
		front.State = string(item.State)
		front.Labels = item.Labels
		front.ACL = item.ACL
		front.Project = item.ProjectID
		front.CreatedAt = formatItemTime(item.CreatedAt)
		front.UpdatedAt = formatItemTime(item.UpdatedAt)
		body = item.Body
	}
	var node yaml.Node
	if err := node.Encode(front); err != nil {
		return nil, fmt.Errorf("fsstore: encode item %s: %w", item.Key(), err)
	}
	quoteStringValues(&node)
	return markdown.Render(&node, body)
}

// quoteStringValues forces double-quoted style on every string value, so a
// newline or a "---" line inside a title can never end the frontmatter.
func quoteStringValues(node *yaml.Node) {
	switch node.Kind {
	case yaml.MappingNode:
		for index := 1; index < len(node.Content); index += 2 {
			quoteStringValues(node.Content[index])
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, child := range node.Content {
			quoteStringValues(child)
		}
	case yaml.ScalarNode:
		if node.Tag == "!!str" {
			node.Style = yaml.DoubleQuotedStyle
		}
	}
}

// readItemFile parses one item file and checks it belongs to key. A missing
// file returns an error wrapping fs.ErrNotExist.
func readItemFile(path, key string) (domain.Item, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.Item{}, err
	}
	var front itemFrontmatter
	body, err := markdown.Parse(raw, &front)
	if err != nil {
		return domain.Item{}, fmt.Errorf("fsstore: item %s: %w", key, err)
	}
	if front.SchemaVersion > itemSchemaVersion {
		return domain.Item{}, domain.NewValidationError("schema_version",
			fmt.Sprintf("item %s: schema %d requires a newer engine", key, front.SchemaVersion))
	}
	item := domain.Item{
		Ref:       domain.ItemRef{ConnectionID: front.Ref.ConnectionID, ExternalID: front.Ref.ExternalID},
		Kind:      domain.ItemKind(front.Kind),
		Title:     front.Title,
		Body:      strings.TrimSuffix(body, "\n"),
		URL:       front.URL,
		Author:    front.Author,
		State:     domain.ItemState(front.State),
		Labels:    front.Labels,
		ACL:       front.ACL,
		Hash:      front.Hash,
		Deleted:   front.Deleted,
		ProjectID: front.Project,
	}
	if item.Deleted {
		item.Body = ""
	}
	if item.Key() != key {
		return domain.Item{}, domain.NewValidationError("item.ref", fmt.Sprintf("file for %s holds another item", key))
	}
	for _, field := range []struct {
		raw    string
		target *time.Time
	}{
		{front.CreatedAt, &item.CreatedAt},
		{front.UpdatedAt, &item.UpdatedAt},
		{front.ObservedAt, &item.ObservedAt},
	} {
		parsed, err := parseItemTime(field.raw)
		if err != nil {
			return domain.Item{}, fmt.Errorf("fsstore: item %s: %w", key, err)
		}
		*field.target = parsed
	}
	return item, nil
}

func formatItemTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

func parseItemTime(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, domain.NewValidationError("item.time", fmt.Sprintf("%q is not RFC 3339", raw))
	}
	return parsed.UTC(), nil
}
