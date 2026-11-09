package dbschema

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"modernc.org/sqlite"
)

// SQLiteDescriptor is the static SQLite schema source description.
func SQLiteDescriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider:    SQLiteProvider,
		DisplayName: "SQLite schema",
		Capabilities: domain.Capabilities{
			Backfill:    true,
			Incremental: true,
			AuthModes:   []domain.AuthMode{domain.AuthNone},
		},
		DefaultPublish: domain.PublishShared,
		ConfigFields: []domain.ConfigField{
			{Key: "path", Label: "Database file", Required: true, Help: "Absolute path to a SQLite database file. Relative, UNC and file: URI values are rejected, as are paths containing ? or #."},
			{Key: "includesystem", Label: "Include system objects", Help: "Include sqlite_* system objects (default false). The contracts-v1 key alphabet does not allow underscores."},
		},
		Backends: []domain.Backend{domain.BackendNative},
		Streams:  []string{sqliteStream},
	}
}

// SQLiteSource snapshots a SQLite database file through a read-only
// connection. It keeps no connection open between calls.
type SQLiteSource struct {
	connection    domain.Connection
	path          string
	includeSystem bool
	now           func() time.Time
}

var _ ports.Source = (*SQLiteSource)(nil)

// NewSQLiteSource validates conn and binds the native source. path must name
// an existing absolute regular file.
func NewSQLiteSource(conn domain.Connection, now func() time.Time) (*SQLiteSource, error) {
	if err := SQLiteDescriptor().ValidateConnection(conn); err != nil {
		return nil, err
	}
	path, err := sqlitePath(conn.Settings)
	if err != nil {
		return nil, err
	}
	includeSystem, err := boolSetting(conn.Settings, "includesystem")
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &SQLiteSource{connection: conn, path: path, includeSystem: includeSystem, now: now}, nil
}

func sqlitePath(settings map[string]string) (string, error) {
	raw := strings.TrimSpace(settings["path"])
	if raw == "" {
		return "", domain.NewValidationError("dbschema.path", "is required")
	}
	if strings.ContainsRune(raw, 0) {
		return "", domain.NewValidationError("dbschema.path", "must not contain NUL")
	}
	if strings.HasPrefix(raw, "file:") {
		return "", domain.NewValidationError("dbschema.path", "must be a filesystem path, not a file: URI")
	}
	if strings.ContainsAny(raw, "?#") {
		return "", domain.NewValidationError("dbschema.path", "must not contain ? or #")
	}
	if strings.HasPrefix(raw, `\\`) || strings.HasPrefix(raw, "//") {
		return "", domain.NewValidationError("dbschema.path", "must not be a UNC path")
	}
	if !filepath.IsAbs(raw) {
		return "", domain.NewValidationError("dbschema.path", "must be an absolute path")
	}
	cleaned := filepath.Clean(raw)
	info, err := os.Stat(cleaned)
	if err != nil {
		return "", domain.NewValidationError("dbschema.path", "is not accessible: "+err.Error())
	}
	if !info.Mode().IsRegular() {
		return "", domain.NewValidationError("dbschema.path", "must name a regular file")
	}
	return cleaned, nil
}

func sqliteDSN(path string) string {
	return "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=query_only(1)"
}

// openSQLiteReadOnly opens a database with mode=ro and verifies query_only.
// immutable=1 is not used because a WAL or hot-journal database needs its side
// files; immutable would silently ignore them.
func openSQLiteReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	connector, err := sqlite.NewConnector(sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open read-only database: %w", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open read-only database: %w", err)
	}
	var queryOnly int
	if err := db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read query_only: %w", err)
	}
	if queryOnly != 1 {
		if _, err := db.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("set query_only: %w", err)
		}
		if err := db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("read query_only: %w", err)
		}
	}
	if queryOnly != 1 {
		_ = db.Close()
		return nil, domain.NewValidationError("sqlite-schema.queryOnly", "refusing a writable SQLite connection")
	}
	return db, nil
}

// Descriptor returns the provider's static description.
func (s *SQLiteSource) Descriptor() domain.SourceDescriptor { return SQLiteDescriptor() }

// Check verifies that the file opens read-only and reports its schema version,
// without reading any user rows.
func (s *SQLiteSource) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	db, err := openSQLiteReadOnly(ctx, s.path)
	if err != nil {
		return fmt.Errorf("sqlite-schema check: %w", err)
	}
	defer func() { _ = db.Close() }()
	var version int64
	if err := db.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&version); err != nil {
		return fmt.Errorf("sqlite-schema check: %w", err)
	}
	return nil
}

// Read returns one bounded page of a schema snapshot.
func (s *SQLiteSource) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, err
	}
	if req.Connection.ID != s.connection.ID || req.Connection.Provider != SQLiteProvider {
		return ports.ReadBatch{}, domain.NewValidationError("sqlite-schema.connection", "does not match source connection")
	}
	stored, hasStored, err := readSnapshotCheckpoint(req, sqliteStream)
	if err != nil {
		return ports.ReadBatch{}, err
	}
	db, err := openSQLiteReadOnly(ctx, s.path)
	if err != nil {
		return ports.ReadBatch{}, fmt.Errorf("sqlite-schema read: %w", err)
	}
	defer func() { _ = db.Close() }()
	snapshot, err := loadSQLiteSnapshot(ctx, db, s.includeSystem)
	if err != nil {
		return ports.ReadBatch{}, fmt.Errorf("sqlite-schema read: %w", err)
	}
	hash, err := snapshot.digest()
	if err != nil {
		return ports.ReadBatch{}, err
	}
	items := snapshot.items(s.connection, hash)
	offset, unchanged, err := snapshotOffset(stored, hasStored, hash, req.Since != nil, len(items))
	if err != nil {
		return ports.ReadBatch{}, err
	}
	if unchanged {
		checkpoint, err := makeCheckpoint(sqliteStream, hash, snapshot.watermark(), 0)
		if err != nil {
			return ports.ReadBatch{}, err
		}
		return ports.ReadBatch{Checkpoints: []domain.Checkpoint{checkpoint}}, nil
	}
	page, next, more := paginate(items, offset, req.Limit)
	checkpoint, err := makeCheckpoint(sqliteStream, hash, snapshot.watermark(), next)
	if err != nil {
		return ports.ReadBatch{}, err
	}
	return ports.ReadBatch{Items: page, Checkpoints: []domain.Checkpoint{checkpoint}, More: more}, nil
}

type sqliteColumn struct {
	CID     int    `json:"cid"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	NotNull bool   `json:"notNull"`
	Default string `json:"default,omitempty"`
	PK      int    `json:"pk,omitempty"`
}

type sqliteForeignKey struct {
	ID       int    `json:"id"`
	Seq      int    `json:"seq"`
	Table    string `json:"table"`
	From     string `json:"from"`
	To       string `json:"to,omitempty"`
	OnUpdate string `json:"onUpdate"`
	OnDelete string `json:"onDelete"`
	Match    string `json:"match"`
}

type sqliteIndex struct {
	Table   string   `json:"table"`
	Name    string   `json:"name"`
	Unique  bool     `json:"unique"`
	Origin  string   `json:"origin"`
	Partial bool     `json:"partial"`
	Columns []string `json:"columns"`
	SQL     string   `json:"sql,omitempty"`
}

type sqliteTable struct {
	Name        string             `json:"name"`
	SQL         string             `json:"sql,omitempty"`
	Columns     []sqliteColumn     `json:"columns"`
	ForeignKeys []sqliteForeignKey `json:"foreignKeys"`
	Indexes     []string           `json:"indexes"`
}

type sqliteView struct {
	Name    string         `json:"name"`
	SQL     string         `json:"sql,omitempty"`
	Columns []sqliteColumn `json:"columns"`
}

type sqliteTrigger struct {
	Name  string `json:"name"`
	Table string `json:"table"`
	SQL   string `json:"sql,omitempty"`
}

type sqliteSnapshot struct {
	SchemaVersion int64             `json:"schemaVersion"`
	UserVersion   int64             `json:"userVersion"`
	ApplicationID int64             `json:"applicationId"`
	Encoding      string            `json:"encoding"`
	PageSize      int64             `json:"pageSize"`
	IncludeSystem bool              `json:"includeSystem"`
	Tables        []sqliteTable     `json:"tables"`
	Views         []sqliteView      `json:"views"`
	Indexes       []sqliteIndex     `json:"indexes"`
	Triggers      []sqliteTrigger   `json:"triggers"`
	Migrations    []migrationLedger `json:"migrations"`
}

func (s *sqliteSnapshot) watermark() string {
	return "sqlite-schema-version:" + strconv.FormatInt(s.SchemaVersion, 10)
}

func (s *sqliteSnapshot) digest() (string, error) {
	return digestJSON(s)
}

type sqliteSchemaRow struct {
	typ   string
	name  string
	table string
	sql   string
}

const sqliteSchemaSQL = `SELECT type, name, tbl_name, COALESCE(sql, '')
FROM sqlite_schema
WHERE type IN ('table', 'index', 'view', 'trigger')
  AND (? = 1 OR name NOT GLOB 'sqlite_*')
ORDER BY type, name`

func loadSQLiteSnapshot(ctx context.Context, db *sql.DB, includeSystem bool) (*sqliteSnapshot, error) {
	snapshot := &sqliteSnapshot{IncludeSystem: includeSystem}
	var err error
	if snapshot.SchemaVersion, err = sqlitePragmaInt(ctx, db, "schema_version"); err != nil {
		return nil, err
	}
	if snapshot.UserVersion, err = sqlitePragmaInt(ctx, db, "user_version"); err != nil {
		return nil, err
	}
	if snapshot.ApplicationID, err = sqlitePragmaInt(ctx, db, "application_id"); err != nil {
		return nil, err
	}
	if snapshot.PageSize, err = sqlitePragmaInt(ctx, db, "page_size"); err != nil {
		return nil, err
	}
	if snapshot.Encoding, err = sqlitePragmaText(ctx, db, "encoding"); err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, sqliteSchemaSQL, boolInt(includeSystem))
	if err != nil {
		return nil, err
	}
	var objects []sqliteSchemaRow
	for rows.Next() {
		var row sqliteSchemaRow
		if err := rows.Scan(&row.typ, &row.name, &row.table, &row.sql); err != nil {
			rows.Close()
			return nil, err
		}
		objects = append(objects, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	indexSQL := map[string]string{}
	var migrationTables []string
	for _, row := range objects {
		if row.typ == "index" {
			indexSQL[row.name] = row.sql
		}
	}

	for _, row := range objects {
		switch row.typ {
		case "table":
			table := sqliteTable{Name: row.name, SQL: row.sql}
			if table.Columns, err = sqliteColumns(ctx, db, row.name); err != nil {
				return nil, err
			}
			if table.ForeignKeys, err = sqliteForeignKeys(ctx, db, row.name); err != nil {
				return nil, err
			}
			indexes, err := sqliteIndexesForTable(ctx, db, row.name, indexSQL, includeSystem)
			if err != nil {
				return nil, err
			}
			snapshot.Indexes = append(snapshot.Indexes, indexes...)
			for _, index := range indexes {
				table.Indexes = append(table.Indexes, index.Name)
			}
			snapshot.Tables = append(snapshot.Tables, table)
			if _, ok := migrationColumns[row.name]; ok {
				migrationTables = append(migrationTables, row.name)
			}
		case "view":
			view := sqliteView{Name: row.name, SQL: row.sql}
			if view.Columns, err = sqliteColumns(ctx, db, row.name); err != nil {
				return nil, err
			}
			snapshot.Views = append(snapshot.Views, view)
		case "trigger":
			snapshot.Triggers = append(snapshot.Triggers, sqliteTrigger{Name: row.name, Table: row.table, SQL: row.sql})
		}
	}

	for _, table := range migrationTables {
		ledger, err := readSQLiteMigration(ctx, db, table)
		if err != nil {
			return nil, err
		}
		snapshot.Migrations = append(snapshot.Migrations, ledger)
	}
	snapshot.sort()
	return snapshot, nil
}

func sqlitePragmaInt(ctx context.Context, db *sql.DB, name string) (int64, error) {
	var value int64
	if err := db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&value); err != nil {
		return 0, fmt.Errorf("read PRAGMA %s: %w", name, err)
	}
	return value, nil
}

func sqlitePragmaText(ctx context.Context, db *sql.DB, name string) (string, error) {
	var value string
	if err := db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&value); err != nil {
		return "", fmt.Errorf("read PRAGMA %s: %w", name, err)
	}
	return value, nil
}

func sqliteColumns(ctx context.Context, db *sql.DB, table string) ([]sqliteColumn, error) {
	rows, err := db.QueryContext(ctx, `SELECT cid, name, type, "notnull", COALESCE(dflt_value, ''), pk
FROM pragma_table_info(?)
ORDER BY cid`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []sqliteColumn
	for rows.Next() {
		var column sqliteColumn
		if err := rows.Scan(&column.CID, &column.Name, &column.Type, &column.NotNull, &column.Default, &column.PK); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func sqliteForeignKeys(ctx context.Context, db *sql.DB, table string) ([]sqliteForeignKey, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, seq, "table", "from", COALESCE("to", ''), on_update, on_delete, match
FROM pragma_foreign_key_list(?)
ORDER BY id, seq`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []sqliteForeignKey
	for rows.Next() {
		var key sqliteForeignKey
		if err := rows.Scan(&key.ID, &key.Seq, &key.Table, &key.From, &key.To, &key.OnUpdate, &key.OnDelete, &key.Match); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func sqliteIndexesForTable(ctx context.Context, db *sql.DB, table string, indexSQL map[string]string, includeSystem bool) ([]sqliteIndex, error) {
	rows, err := db.QueryContext(ctx, `SELECT seq, name, "unique", origin, partial
FROM pragma_index_list(?)
ORDER BY seq`, table)
	if err != nil {
		return nil, err
	}
	type indexMeta struct {
		name    string
		unique  bool
		origin  string
		partial bool
	}
	var metas []indexMeta
	for rows.Next() {
		var seq int
		var meta indexMeta
		if err := rows.Scan(&seq, &meta.name, &meta.unique, &meta.origin, &meta.partial); err != nil {
			rows.Close()
			return nil, err
		}
		metas = append(metas, meta)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	var indexes []sqliteIndex
	for _, meta := range metas {
		if !includeSystem && strings.HasPrefix(meta.name, "sqlite_") {
			continue
		}
		columns, err := sqliteIndexColumns(ctx, db, meta.name)
		if err != nil {
			return nil, err
		}
		indexes = append(indexes, sqliteIndex{
			Table: table, Name: meta.name, Unique: meta.unique, Origin: meta.origin,
			Partial: meta.partial, Columns: columns, SQL: indexSQL[meta.name],
		})
	}
	return indexes, nil
}

func sqliteIndexColumns(ctx context.Context, db *sql.DB, index string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT seqno, COALESCE(name, '<expression>')
FROM pragma_index_info(?)
ORDER BY seqno`, index)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var seq int
		var name string
		if err := rows.Scan(&seq, &name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

func readSQLiteMigration(ctx context.Context, db *sql.DB, table string) (migrationLedger, error) {
	columns, err := sqliteColumns(ctx, db, table)
	if err != nil {
		return migrationLedger{}, err
	}
	present := map[string]bool{}
	for _, column := range columns {
		present[column.Name] = true
	}
	known := migrationColumns[table]
	selected := make([]string, 0, len(known))
	for _, name := range known {
		if present[name] {
			selected = append(selected, name)
		}
	}
	ledger := migrationLedger{Table: table}
	if len(selected) == 0 {
		return ledger, nil
	}
	quoted := make([]string, len(selected))
	for i, name := range selected {
		quoted[i] = quoteIdent(name)
	}
	query := "SELECT " + strings.Join(quoted, ", ") + " FROM " + quoteIdent(table) +
		" ORDER BY " + quoteIdent(selected[0]) + " LIMIT " + strconv.Itoa(pgMigrationLimit)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return migrationLedger{}, err
	}
	defer rows.Close()
	for rows.Next() {
		values := make([]any, len(selected))
		pointers := make([]any, len(selected))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return migrationLedger{}, err
		}
		entry := migrationEntry{Fields: make([]migrationField, 0, len(selected))}
		for i, name := range selected {
			entry.Fields = append(entry.Fields, migrationField{Key: name, Value: formatDBValue(values[i])})
		}
		ledger.Entries = append(ledger.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return migrationLedger{}, err
	}
	sort.Slice(ledger.Entries, func(i, j int) bool {
		return migrationSortKey(ledger.Entries[i]) < migrationSortKey(ledger.Entries[j])
	})
	return ledger, nil
}

func (s *sqliteSnapshot) sort() {
	sort.Slice(s.Tables, func(i, j int) bool { return s.Tables[i].Name < s.Tables[j].Name })
	for i := range s.Tables {
		sort.Slice(s.Tables[i].Columns, func(a, b int) bool { return s.Tables[i].Columns[a].CID < s.Tables[i].Columns[b].CID })
		sort.Slice(s.Tables[i].ForeignKeys, func(a, b int) bool {
			if s.Tables[i].ForeignKeys[a].ID != s.Tables[i].ForeignKeys[b].ID {
				return s.Tables[i].ForeignKeys[a].ID < s.Tables[i].ForeignKeys[b].ID
			}
			return s.Tables[i].ForeignKeys[a].Seq < s.Tables[i].ForeignKeys[b].Seq
		})
		sort.Strings(s.Tables[i].Indexes)
	}
	sort.Slice(s.Views, func(i, j int) bool { return s.Views[i].Name < s.Views[j].Name })
	for i := range s.Views {
		sort.Slice(s.Views[i].Columns, func(a, b int) bool { return s.Views[i].Columns[a].CID < s.Views[i].Columns[b].CID })
	}
	sort.Slice(s.Indexes, func(i, j int) bool {
		if s.Indexes[i].Table != s.Indexes[j].Table {
			return s.Indexes[i].Table < s.Indexes[j].Table
		}
		return s.Indexes[i].Name < s.Indexes[j].Name
	})
	sort.Slice(s.Triggers, func(i, j int) bool {
		if s.Triggers[i].Table != s.Triggers[j].Table {
			return s.Triggers[i].Table < s.Triggers[j].Table
		}
		return s.Triggers[i].Name < s.Triggers[j].Name
	})
	s.Migrations = sortMigrations(s.Migrations)
}

func (s *sqliteSnapshot) items(connection domain.Connection, hash string) []domain.Item {
	items := make([]domain.Item, 0, len(s.Tables)+len(s.Views)+len(s.Indexes)+len(s.Triggers)+len(s.Migrations)+1)
	items = append(items, itemFor(connection, SQLiteProvider, "main", "database",
		"Database main", "database", s.renderOverview(hash)))
	for _, table := range s.Tables {
		items = append(items, itemFor(connection, SQLiteProvider, "main",
			objectKey("table", table.Name), "Table "+table.Name, "table", s.renderTable(table)))
	}
	for _, view := range s.Views {
		items = append(items, itemFor(connection, SQLiteProvider, "main",
			objectKey("view", view.Name), "View "+view.Name, "view", s.renderView(view)))
	}
	for _, index := range s.Indexes {
		items = append(items, itemFor(connection, SQLiteProvider, "main",
			objectKey("index", index.Name), "Index "+index.Name, "index", s.renderIndex(index)))
	}
	for _, trigger := range s.Triggers {
		items = append(items, itemFor(connection, SQLiteProvider, "main",
			objectKey("trigger", trigger.Name), "Trigger "+trigger.Name, "trigger", s.renderTrigger(trigger)))
	}
	if len(s.Migrations) > 0 {
		items = append(items, itemFor(connection, SQLiteProvider, "main",
			"migrations", "Migration ledger main", "migrations", s.renderMigrations()))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Ref.ExternalID < items[j].Ref.ExternalID })
	return items
}

func (s *sqliteSnapshot) renderOverview(hash string) string {
	var builder strings.Builder
	builder.WriteString("# Database main\n\n")
	fmt.Fprintf(&builder, "- Schema digest: `%s`\n", hash)
	fmt.Fprintf(&builder, "- Watermark: `%s`\n", s.watermark())
	fmt.Fprintf(&builder, "- PRAGMA schema_version: %d\n", s.SchemaVersion)
	fmt.Fprintf(&builder, "- PRAGMA user_version: %d\n", s.UserVersion)
	fmt.Fprintf(&builder, "- PRAGMA application_id: %d\n", s.ApplicationID)
	fmt.Fprintf(&builder, "- Encoding: `%s`\n", markdownCell(s.Encoding))
	fmt.Fprintf(&builder, "- Page size: %d\n", s.PageSize)
	fmt.Fprintf(&builder, "- Include system objects: %t\n", s.IncludeSystem)
	fmt.Fprintf(&builder, "- Tables: %d\n", len(s.Tables))
	fmt.Fprintf(&builder, "- Views: %d\n", len(s.Views))
	fmt.Fprintf(&builder, "- Indexes: %d\n", len(s.Indexes))
	fmt.Fprintf(&builder, "- Triggers: %d\n", len(s.Triggers))
	fmt.Fprintf(&builder, "- Migration ledgers: %d\n", len(s.Migrations))
	return builder.String()
}

func (s *sqliteSnapshot) renderTable(table sqliteTable) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Table %s\n\n", quoteIdent(table.Name))
	builder.WriteString("_SQLite stores no table or column comments; stored SQL is preserved verbatim._\n\n")
	if table.SQL != "" {
		builder.WriteString("## Definition\n\n")
		builder.WriteString(codeBlock(table.SQL))
		builder.WriteString("\n\n")
	}
	builder.WriteString("## Columns\n\n")
	writeSQLiteColumns(&builder, table.Columns)
	if len(table.ForeignKeys) > 0 {
		builder.WriteString("\n## Foreign keys\n\n")
		builder.WriteString("| ID | Seq | Table | From | To | On update | On delete | Match |\n|---|---|---|---|---|---|---|---|\n")
		for _, key := range table.ForeignKeys {
			fmt.Fprintf(&builder, "| %d | %d | `%s` | `%s` | `%s` | %s | %s | %s |\n",
				key.ID, key.Seq, markdownCell(key.Table), markdownCell(key.From), markdownCell(key.To),
				markdownCell(key.OnUpdate), markdownCell(key.OnDelete), markdownCell(key.Match))
		}
	}
	if len(table.Indexes) > 0 {
		builder.WriteString("\n## Indexes\n\n")
		for _, name := range table.Indexes {
			fmt.Fprintf(&builder, "- `%s`\n", markdownCell(name))
		}
	}
	return builder.String()
}

func writeSQLiteColumns(builder *strings.Builder, columns []sqliteColumn) {
	if len(columns) == 0 {
		builder.WriteString("_No columns._\n")
		return
	}
	builder.WriteString("| # | Column | Type | Not null | Default | PK |\n|---|---|---|---|---|---|\n")
	for _, column := range columns {
		fmt.Fprintf(builder, "| %d | `%s` | `%s` | %t | `%s` | %d |\n",
			column.CID, markdownCell(column.Name), markdownCell(column.Type), column.NotNull,
			markdownCell(column.Default), column.PK)
	}
}

func (s *sqliteSnapshot) renderView(view sqliteView) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# View %s\n\n", quoteIdent(view.Name))
	if view.SQL != "" {
		builder.WriteString("## Definition\n\n")
		builder.WriteString(codeBlock(view.SQL))
		builder.WriteString("\n\n")
	}
	builder.WriteString("## Columns\n\n")
	writeSQLiteColumns(&builder, view.Columns)
	return builder.String()
}

func (s *sqliteSnapshot) renderIndex(index sqliteIndex) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Index %s\n\n", quoteIdent(index.Name))
	fmt.Fprintf(&builder, "- Table: `%s`\n", markdownCell(index.Table))
	fmt.Fprintf(&builder, "- Unique: %t\n", index.Unique)
	fmt.Fprintf(&builder, "- Origin: `%s`\n", markdownCell(index.Origin))
	fmt.Fprintf(&builder, "- Partial: %t\n", index.Partial)
	if len(index.Columns) > 0 {
		fmt.Fprintf(&builder, "- Columns: %s\n", markdownCell(strings.Join(index.Columns, ", ")))
	}
	if index.SQL != "" {
		builder.WriteString("\n## Definition\n\n")
		builder.WriteString(codeBlock(index.SQL))
	}
	return builder.String()
}

func (s *sqliteSnapshot) renderTrigger(trigger sqliteTrigger) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Trigger %s\n\n", quoteIdent(trigger.Name))
	fmt.Fprintf(&builder, "- Table: `%s`\n\n", markdownCell(trigger.Table))
	builder.WriteString(codeBlock(trigger.SQL))
	return builder.String()
}

func (s *sqliteSnapshot) renderMigrations() string {
	var builder strings.Builder
	builder.WriteString("# Migration ledgers\n")
	for _, ledger := range s.Migrations {
		fmt.Fprintf(&builder, "\n## %s\n\n", quoteIdent(ledger.Table))
		if len(ledger.Entries) == 0 {
			builder.WriteString("_No rows._\n")
			continue
		}
		fields := ledger.Entries[0].Fields
		builder.WriteString("|")
		for _, field := range fields {
			fmt.Fprintf(&builder, " %s |", markdownCell(field.Key))
		}
		builder.WriteString("\n|")
		for range fields {
			builder.WriteString("---|")
		}
		builder.WriteString("\n")
		for _, entry := range ledger.Entries {
			builder.WriteString("|")
			for _, field := range entry.Fields {
				fmt.Fprintf(&builder, " %s |", markdownCell(field.Value))
			}
			builder.WriteString("\n")
		}
	}
	return builder.String()
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
