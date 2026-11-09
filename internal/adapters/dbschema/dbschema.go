// Package dbschema implements two read-only ports.Source adapters that
// snapshot database structure: postgres-schema (PostgreSQL) and sqlite-schema
// (SQLite). It never reads rows from user tables; the only row-reading
// exception is a bounded read of a recognised migration ledger
// (schema_migrations, flyway_schema_history, databasechangelog).
//
// # Item granularity
//
// Both sources emit one domain.ItemSchema item per object plus one database
// overview item, one optional migration-ledger item and, for PostgreSQL, one
// optional statistics item:
//
//	postgres-schema/<database>/database
//	postgres-schema/<database>/table:<schema>.<name>
//	postgres-schema/<database>/view:<schema>.<name>
//	postgres-schema/<database>/index:<schema>.<name>
//	postgres-schema/<database>/sequence:<schema>.<name>
//	postgres-schema/<database>/trigger:<schema>.<table>.<name>
//	postgres-schema/<database>/migrations
//	postgres-schema/<database>/stats
//
//	sqlite-schema/main/database
//	sqlite-schema/main/table:<name>
//	sqlite-schema/main/view:<name>
//	sqlite-schema/main/index:<name>
//	sqlite-schema/main/trigger:<name>
//	sqlite-schema/main/migrations
//
// "main" is SQLite's name for the primary database file. Object keys longer
// than the external-ID budget are replaced by a SHA-256 prefix; the item title
// remains readable. Item bodies are bounded to domain.MaxItemBodyBytes (512
// KiB) on a UTF-8 boundary.
//
// # Checkpoints
//
// One fixed stream per provider ("postgres-schema" / "sqlite-schema"), version
// 1. The checkpoint payload is:
//
//	{"hash":"sha256:<64 hex>","watermark":"<catalog token>","offset":<n>}
//
// hash is the deterministic schema digest. watermark is a deterministic
// catalog token (PostgreSQL: database OID plus maximum pg_class OID; SQLite:
// PRAGMA schema_version). offset is present only while a changed snapshot is
// being paged out; it is omitted once the snapshot is complete.
//
// Read computes the digest and compares it with the stored hash. An unchanged
// schema returns no items and More=false (unless req.Since forces a fresh
// snapshot, or a previous snapshot was interrupted with offset>0). A changed
// schema returns items in deterministic order, honouring req.Limit with
// More=true and a resumable offset. Removed objects are not tombstoned: the
// sources declare Deletes=false, so the host never infers a deletion from a
// missing item.
//
// # Read-only guarantees
//
// PostgreSQL connections start with default_transaction_read_only=on and every
// snapshot runs in a read-only transaction; the source verifies both
// transaction_read_only and default_transaction_read_only and fails closed if
// either is not "on". Only pg_catalog, information_schema and pg_stat_* views
// are queried, plus the bounded migration-ledger exception; never pg_dump and
// never credentials.
//
// SQLite files are opened with mode=ro and PRAGMA query_only=1. immutable=1 is
// deliberately not used: a WAL or hot-journal database needs its side files,
// and immutable would silently ignore them. Only sqlite_schema, PRAGMA
// functions and the bounded migration-ledger exception are queried; no user
// row is selected.
//
// SQLite has no table or column comment storage. Any SQL comments embedded in
// the stored CREATE text are preserved verbatim, but no comment metadata is
// invented.
package dbschema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// PostgresProvider is the provider name of the PostgreSQL schema source.
	PostgresProvider = "postgres-schema"
	// SQLiteProvider is the provider name of the SQLite schema source.
	SQLiteProvider = "sqlite-schema"

	checkpointVersion = 1
	postgresStream    = PostgresProvider
	sqliteStream      = SQLiteProvider

	defaultReadLimit = 100
	maxReadLimit     = 500

	maxExternalIDLength = 1024
	maxObjectKeyLength  = 400
)

// snapshotCheckpoint is the opaque checkpoint payload. Offset is zero and
// omitted once a snapshot is complete.
type snapshotCheckpoint struct {
	Hash      string `json:"hash"`
	Watermark string `json:"watermark"`
	Offset    int    `json:"offset,omitempty"`
}

// readSnapshotCheckpoint decodes and validates the stored checkpoint for
// stream. ok is false when no checkpoint is stored.
func readSnapshotCheckpoint(req ports.ReadRequest, stream string) (snapshotCheckpoint, bool, error) {
	stored, ok := req.Checkpoints[stream]
	if !ok {
		return snapshotCheckpoint{}, false, nil
	}
	if stored.Stream != stream || stored.Version != checkpointVersion {
		return snapshotCheckpoint{}, true, domain.NewValidationError("dbschema.checkpoint", "unsupported stream or version")
	}
	var checkpoint snapshotCheckpoint
	if err := json.Unmarshal(stored.Data, &checkpoint); err != nil {
		return snapshotCheckpoint{}, true, domain.NewValidationError("dbschema.checkpoint", "is invalid")
	}
	if err := checkpoint.validate(); err != nil {
		return snapshotCheckpoint{}, true, err
	}
	return checkpoint, true, nil
}

func (c snapshotCheckpoint) validate() error {
	if c.Offset < 0 {
		return domain.NewValidationError("dbschema.checkpoint.offset", "must not be negative")
	}
	if c.Hash == "" {
		if c.Offset != 0 {
			return domain.NewValidationError("dbschema.checkpoint.offset", "requires a hash")
		}
		return nil
	}
	if !strings.HasPrefix(c.Hash, domain.ItemHashPrefix) {
		return domain.NewValidationError("dbschema.checkpoint.hash", "must start with "+domain.ItemHashPrefix)
	}
	hexPart := strings.TrimPrefix(c.Hash, domain.ItemHashPrefix)
	if len(hexPart) != sha256.Size*2 {
		return domain.NewValidationError("dbschema.checkpoint.hash", "has an invalid length")
	}
	if _, err := hex.DecodeString(hexPart); err != nil {
		return domain.NewValidationError("dbschema.checkpoint.hash", "is not hexadecimal")
	}
	return nil
}

// makeCheckpoint encodes one checkpoint. offset>0 keeps a partially emitted
// snapshot resumable; offset==0 marks the snapshot complete.
func makeCheckpoint(stream, hash, watermark string, offset int) (domain.Checkpoint, error) {
	data, err := json.Marshal(snapshotCheckpoint{Hash: hash, Watermark: watermark, Offset: offset})
	if err != nil {
		return domain.Checkpoint{}, fmt.Errorf("dbschema: encode checkpoint: %w", err)
	}
	checkpoint := domain.Checkpoint{Stream: stream, Version: checkpointVersion, Data: data}
	if err := checkpoint.Validate(); err != nil {
		return domain.Checkpoint{}, err
	}
	return checkpoint, nil
}

// snapshotOffset decides where a read starts and whether the schema is
// unchanged. A stored offset continues an interrupted paged snapshot even
// when req.Since is set, so a forced multi-batch snapshot cannot restart from
// zero on every Read call.
func snapshotOffset(stored snapshotCheckpoint, hasStored bool, hash string, force bool, count int) (offset int, unchanged bool, err error) {
	if !hasStored || stored.Hash != hash {
		return 0, false, nil
	}
	if stored.Offset > 0 {
		if stored.Offset > count {
			return 0, false, domain.NewValidationError("dbschema.checkpoint.offset", "exceeds the current snapshot")
		}
		return stored.Offset, false, nil
	}
	if force {
		return 0, false, nil
	}
	return 0, true, nil
}

// paginate applies the batch limit to a deterministic item slice.
func paginate(items []domain.Item, offset, limit int) (page []domain.Item, nextOffset int, more bool) {
	if limit <= 0 {
		limit = defaultReadLimit
	}
	if limit > maxReadLimit {
		limit = maxReadLimit
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(items) {
		offset = len(items)
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	more = end < len(items)
	if more {
		nextOffset = end
	}
	return items[offset:end:end], nextOffset, more
}

// digestJSON returns the domain hash of a canonical JSON payload.
func digestJSON(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("dbschema: encode digest: %w", err)
	}
	sum := sha256.Sum256(payload)
	return domain.ItemHashPrefix + hex.EncodeToString(sum[:]), nil
}

// externalIDFor builds provider/<database>/<object> within the ItemRef budget,
// falling back to a stable hash for overlong object keys.
func externalIDFor(provider, database, object string) string {
	database = safeText(database)
	object = safeText(object)
	id := provider + "/" + database + "/" + object
	if len(id) <= maxExternalIDLength {
		return id
	}
	return provider + "/" + database + "/" + shortHash(database+"\x00"+object)
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:40]
}

func safeText(value string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(value, "\x00", "\uFFFD"), "\uFFFD")
}

// itemFor builds a schema item with a bounded body and deterministic labels.
func itemFor(connection domain.Connection, provider, database, object, title, kindLabel, body string) domain.Item {
	item := domain.Item{
		Ref:    domain.ItemRef{ConnectionID: connection.ID, ExternalID: externalIDFor(provider, database, object)},
		Kind:   domain.ItemSchema,
		Title:  itemTitle(title, object),
		Body:   boundedBody(body),
		Labels: []string{"schema", kindLabel},
	}
	if len(connection.Projects) == 1 {
		item.ProjectID = connection.Projects[0]
	}
	return item
}

func itemTitle(title, fallback string) string {
	title = strings.TrimSpace(safeText(title))
	if title == "" {
		title = strings.TrimSpace(safeText(fallback))
	}
	if title == "" {
		title = "Schema object"
	}
	return cutUTF8(title, 300)
}

func boundedBody(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	body = strings.ToValidUTF8(body, "\uFFFD")
	if len(body) <= domain.MaxItemBodyBytes {
		return body
	}
	const note = "\n\n_(truncated at the 512 KiB item body cap)_\n"
	limit := domain.MaxItemBodyBytes - len(note)
	if limit < 0 {
		limit = 0
	}
	return cutUTF8(body, limit) + note
}

func cutUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func qualified(schema, name string) string {
	if schema == "" {
		return quoteIdent(name)
	}
	return quoteIdent(schema) + "." + quoteIdent(name)
}

func objectKey(kind string, parts ...string) string {
	key := kind + ":" + strings.Join(parts, ".")
	key = safeText(key)
	if len(key) > maxObjectKeyLength {
		return kind + ":" + shortHash(strings.Join(parts, "\x00"))
	}
	return key
}

// boolSetting parses an optional boolean setting. Missing and blank values are
// false.
func boolSetting(settings map[string]string, key string) (bool, error) {
	raw, ok := settings[key]
	if !ok || strings.TrimSpace(raw) == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, domain.NewValidationError("dbschema."+key, "must be true or false")
	}
	return value, nil
}

func requiredText(settings map[string]string, key, label string) (string, error) {
	value := strings.TrimSpace(settings[key])
	if value == "" {
		return "", domain.NewValidationError("dbschema."+key, "is required")
	}
	if strings.ContainsRune(value, 0) {
		return "", domain.NewValidationError("dbschema."+key, "must not contain NUL")
	}
	return value, nil
}

// migrationEntry is one migration-ledger row, rendered as ordered fields.
type migrationEntry struct {
	Fields []migrationField `json:"fields"`
}

type migrationField struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// formatDBValue formats a catalog value without exposing row data beyond the
// migration-ledger exception.
func formatDBValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []byte:
		return string(typed)
	case bool:
		return strconv.FormatBool(typed)
	case int:
		return strconv.Itoa(typed)
	case int8:
		return strconv.FormatInt(int64(typed), 10)
	case int16:
		return strconv.FormatInt(int64(typed), 10)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case int64:
		return strconv.FormatInt(typed, 10)
	case uint:
		return strconv.FormatUint(uint64(typed), 10)
	case uint64:
		return strconv.FormatUint(typed, 10)
	case float32:
		return strconv.FormatFloat(float64(typed), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(typed)
	}
}

func sortMigrations(ledgers []migrationLedger) []migrationLedger {
	sort.Slice(ledgers, func(i, j int) bool {
		if ledgers[i].Schema != ledgers[j].Schema {
			return ledgers[i].Schema < ledgers[j].Schema
		}
		return ledgers[i].Table < ledgers[j].Table
	})
	for i := range ledgers {
		sort.Slice(ledgers[i].Entries, func(a, b int) bool {
			return migrationSortKey(ledgers[i].Entries[a]) < migrationSortKey(ledgers[i].Entries[b])
		})
	}
	return ledgers
}

func migrationSortKey(entry migrationEntry) string {
	var builder strings.Builder
	for _, field := range entry.Fields {
		builder.WriteString(field.Key)
		builder.WriteByte('=')
		builder.WriteString(field.Value)
		builder.WriteByte(0)
	}
	return builder.String()
}

type migrationLedger struct {
	Schema  string           `json:"schema,omitempty"`
	Table   string           `json:"table"`
	Entries []migrationEntry `json:"entries"`
}

func markdownCell(value string) string {
	value = safeText(value)
	value = strings.ReplaceAll(value, "|", `\|`)
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

func codeBlock(sql string) string {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return "_No stored SQL._"
	}
	fence := "```"
	if strings.Contains(sql, "```") {
		fence = "````"
	}
	return fence + "sql\n" + sql + "\n" + fence
}
