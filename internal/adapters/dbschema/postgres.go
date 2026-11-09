package dbschema

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	pgStatementTimeout = 30 * time.Second
	pgApplicationName  = "lv24-brain-dbschema"
	pgMigrationLimit   = 200
	pgStatsLimit       = 50
)

// PostgresDescriptor is the static PostgreSQL schema source description.
func PostgresDescriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider:    PostgresProvider,
		DisplayName: "PostgreSQL schema",
		Capabilities: domain.Capabilities{
			Backfill:    true,
			Incremental: true,
			AuthModes:   []domain.AuthMode{domain.AuthToken},
		},
		DefaultPublish: domain.PublishShared,
		ConfigFields: []domain.ConfigField{
			{Key: "host", Label: "Host", Required: true, Help: "PostgreSQL host name or IP address (TCP only)."},
			{Key: "port", Label: "Port", Help: "TCP port; default 5432."},
			{Key: "database", Label: "Database", Required: true},
			{Key: "user", Label: "User", Required: true, Help: "Role used for the snapshot; it should have only read access."},
			{Key: "password", Label: "Password", Required: true, Secret: true},
			{Key: "sslmode", Label: "SSL mode", Help: "verify-full (default), verify-ca, require or disable. Use disable/require only for local or test servers."},
			{Key: "includesystem", Label: "Include system schemas", Help: "Include pg_catalog/information_schema objects (default false). The contracts-v1 key alphabet does not allow underscores."},
		},
		Backends: []domain.Backend{domain.BackendNative},
		Streams:  []string{postgresStream},
	}
}

// PostgresSource snapshots PostgreSQL structure through a verified read-only
// session. It keeps no connection open between calls.
type PostgresSource struct {
	connection  domain.Connection
	settings    postgresSettings
	credentials ports.CredentialProvider
	now         func() time.Time
}

var _ ports.Source = (*PostgresSource)(nil)

type postgresSettings struct {
	host          string
	port          int
	database      string
	user          string
	sslMode       string
	includeSystem bool
}

// NewPostgresSource validates conn and binds the native source. The password
// is resolved from the credential store under conn/<id>/password.
func NewPostgresSource(conn domain.Connection, credentials ports.CredentialProvider, now func() time.Time) (*PostgresSource, error) {
	if err := PostgresDescriptor().ValidateConnection(conn); err != nil {
		return nil, err
	}
	if credentials == nil {
		return nil, domain.NewValidationError("postgres-schema.credentials", "is required")
	}
	settings, err := parsePostgresSettings(conn.Settings)
	if err != nil {
		return nil, err
	}
	if err := validatePostgresBaseURL(conn.BaseURL, settings.host, settings.port); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &PostgresSource{connection: conn, settings: settings, credentials: credentials, now: now}, nil
}

func parsePostgresSettings(settings map[string]string) (postgresSettings, error) {
	host, err := cleanPostgresHost(settings["host"])
	if err != nil {
		return postgresSettings{}, err
	}
	port := 5432
	if raw := strings.TrimSpace(settings["port"]); raw != "" {
		port, err = strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return postgresSettings{}, domain.NewValidationError("dbschema.port", "must be an integer between 1 and 65535")
		}
	}
	database, err := requiredText(settings, "database", "database")
	if err != nil {
		return postgresSettings{}, err
	}
	user, err := requiredText(settings, "user", "user")
	if err != nil {
		return postgresSettings{}, err
	}
	sslMode := strings.ToLower(strings.TrimSpace(settings["sslmode"]))
	if sslMode == "" {
		sslMode = "verify-full"
	}
	switch sslMode {
	case "disable", "require", "verify-ca", "verify-full":
	default:
		return postgresSettings{}, domain.NewValidationError("dbschema.sslmode", "must be disable, require, verify-ca or verify-full")
	}
	includeSystem, err := boolSetting(settings, "includesystem")
	if err != nil {
		return postgresSettings{}, err
	}
	return postgresSettings{
		host:          host,
		port:          port,
		database:      database,
		user:          user,
		sslMode:       sslMode,
		includeSystem: includeSystem,
	}, nil
}

func cleanPostgresHost(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	if host == "" {
		return "", domain.NewValidationError("dbschema.host", "is required")
	}
	if strings.ContainsRune(host, 0) || strings.ContainsAny(host, " \t\r\n/?#@") {
		return "", domain.NewValidationError("dbschema.host", "must be a host name or IP address without a scheme, path or credentials")
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if host == "" {
		return "", domain.NewValidationError("dbschema.host", "is required")
	}
	if strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return "", domain.NewValidationError("dbschema.host", "must not include a port; use the port setting")
	}
	return host, nil
}

// validatePostgresBaseURL lets BaseURL name the same endpoint for display and
// identity, while refusing userinfo (a password must never live there), query
// strings and fragments.
func validatePostgresBaseURL(raw, host string, port int) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return domain.NewValidationError("postgres-schema.baseUrl", "must be an absolute postgres://host[:port] URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "postgres" && scheme != "postgresql" {
		return domain.NewValidationError("postgres-schema.baseUrl", "must use the postgres or postgresql scheme")
	}
	if parsed.User != nil {
		return domain.NewValidationError("postgres-schema.baseUrl", "must not contain credentials")
	}
	if parsed.RawQuery != "" {
		return domain.NewValidationError("postgres-schema.baseUrl", "must not contain a query string")
	}
	if parsed.Fragment != "" {
		return domain.NewValidationError("postgres-schema.baseUrl", "must not contain a fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return domain.NewValidationError("postgres-schema.baseUrl", "must not contain a path or database name")
	}
	if normalizeHost(parsed.Hostname()) != normalizeHost(host) {
		return domain.NewValidationError("postgres-schema.baseUrl", "host does not match the host setting")
	}
	if parsed.Port() != "" {
		parsedPort, err := strconv.Atoi(parsed.Port())
		if err != nil || parsedPort != port {
			return domain.NewValidationError("postgres-schema.baseUrl", "port does not match the port setting")
		}
	}
	return nil
}

func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return strings.TrimSuffix(host, ".")
}

// Descriptor returns the provider's static description.
func (s *PostgresSource) Descriptor() domain.SourceDescriptor { return PostgresDescriptor() }

// Check verifies connectivity and that the session is read-only, without
// reading any schema rows.
func (s *PostgresSource) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, err := s.connect(ctx)
	if err != nil {
		return fmt.Errorf("postgres-schema check: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	tx, err := s.beginReadOnly(ctx, conn)
	if err != nil {
		return fmt.Errorf("postgres-schema check: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var one int
	if err := tx.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("postgres-schema check: %w", err)
	}
	return nil
}

// Read returns one bounded page of a schema snapshot.
func (s *PostgresSource) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, err
	}
	if req.Connection.ID != s.connection.ID || req.Connection.Provider != PostgresProvider {
		return ports.ReadBatch{}, domain.NewValidationError("postgres-schema.connection", "does not match source connection")
	}
	stored, hasStored, err := readSnapshotCheckpoint(req, postgresStream)
	if err != nil {
		return ports.ReadBatch{}, err
	}
	conn, err := s.connect(ctx)
	if err != nil {
		return ports.ReadBatch{}, fmt.Errorf("postgres-schema read: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	tx, err := s.beginReadOnly(ctx, conn)
	if err != nil {
		return ports.ReadBatch{}, fmt.Errorf("postgres-schema read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	snapshot, err := loadPostgresSnapshot(ctx, tx, s.settings.includeSystem)
	if err != nil {
		return ports.ReadBatch{}, fmt.Errorf("postgres-schema read: %w", err)
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
		checkpoint, err := makeCheckpoint(postgresStream, hash, snapshot.Watermark, 0)
		if err != nil {
			return ports.ReadBatch{}, err
		}
		return ports.ReadBatch{Checkpoints: []domain.Checkpoint{checkpoint}}, nil
	}
	page, next, more := paginate(items, offset, req.Limit)
	checkpoint, err := makeCheckpoint(postgresStream, hash, snapshot.Watermark, next)
	if err != nil {
		return ports.ReadBatch{}, err
	}
	return ports.ReadBatch{Items: page, Checkpoints: []domain.Checkpoint{checkpoint}, More: more}, nil
}

func (s *PostgresSource) connect(ctx context.Context) (*pgx.Conn, error) {
	password, err := s.credentials.Get(ctx, s.connection.ID, "password")
	if err != nil {
		return nil, fmt.Errorf("read password credential: %w", err)
	}
	config, err := s.connConfig(password)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, redactError(err, password)
	}
	return conn, nil
}

// connConfig builds a pgx config with no password in the URL itself. The
// password is assigned to the parsed config and never logged.
func (s *PostgresSource) connConfig(password string) (*pgx.ConnConfig, error) {
	endpoint := url.URL{Scheme: "postgres", Host: net.JoinHostPort(s.settings.host, strconv.Itoa(s.settings.port)), Path: "/"}
	query := url.Values{}
	query.Set("sslmode", s.settings.sslMode)
	endpoint.RawQuery = query.Encode()
	config, err := pgx.ParseConfig(endpoint.String())
	if err != nil {
		return nil, domain.NewValidationError("postgres-schema.connection", "cannot build a PostgreSQL connection")
	}
	config.Database = s.settings.database
	config.User = s.settings.user
	config.Password = password
	config.ConnectTimeout = 10 * time.Second
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	config.RuntimeParams["default_transaction_read_only"] = "on"
	config.RuntimeParams["application_name"] = pgApplicationName
	config.RuntimeParams["statement_timeout"] = strconv.FormatInt(pgStatementTimeout.Milliseconds(), 10)
	return config, nil
}

func (s *PostgresSource) beginReadOnly(ctx context.Context, conn *pgx.Conn) (pgx.Tx, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+strconv.FormatInt(pgStatementTimeout.Milliseconds(), 10)+"ms'"); err != nil {
		_ = tx.Rollback(context.Background())
		return nil, err
	}
	var transactionReadOnly, defaultReadOnly string
	if err := tx.QueryRow(ctx, pgReadOnlySQL).Scan(&transactionReadOnly, &defaultReadOnly); err != nil {
		_ = tx.Rollback(context.Background())
		return nil, err
	}
	if transactionReadOnly != "on" || defaultReadOnly != "on" {
		_ = tx.Rollback(context.Background())
		return nil, domain.NewValidationError("postgres-schema.readOnly", "refusing a writable PostgreSQL session")
	}
	return tx, nil
}

func redactError(err error, secret string) error {
	if err == nil {
		return nil
	}
	if secret == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "[redacted]"))
}

const (
	pgReadOnlySQL = `SELECT current_setting('transaction_read_only'), current_setting('default_transaction_read_only')`

	pgDatabaseSQL = `SELECT current_database(), COALESCE(MAX(oid), 0)::text FROM pg_catalog.pg_class`
	pgVersionSQL  = `SELECT current_setting('server_version')`

	// pgSchemaFilter is the shared include-system filter. $1 is the boolean
	// includesystem flag.
	pgSchemaFilter = `($1 OR (n.nspname NOT IN ('pg_catalog', 'information_schema')` +
		` AND n.nspname NOT LIKE 'pg_toast%'` +
		` AND n.nspname NOT LIKE 'pg_temp_%'` +
		` AND n.nspname NOT LIKE 'pg_toast_temp_%'))`

	pgTablesSQL = `SELECT n.nspname, c.relname, c.relkind::text, COALESCE(obj_description(c.oid, 'pg_class'), '')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'f') AND ` + pgSchemaFilter + `
ORDER BY n.nspname, c.relname`

	pgColumnsSQL = `SELECT n.nspname, c.relname, a.attnum, a.attname,
       pg_catalog.format_type(a.atttypid, a.atttypmod),
       a.attnotnull,
       COALESCE(pg_catalog.pg_get_expr(d.adbin, d.adrelid), ''),
       COALESCE(pg_catalog.col_description(c.oid, a.attnum), ''),
       a.attidentity::text, a.attgenerated::text
FROM pg_catalog.pg_attribute a
JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE a.attnum > 0 AND NOT a.attisdropped
  AND c.relkind IN ('r', 'p', 'f', 'v', 'm') AND ` + pgSchemaFilter + `
ORDER BY n.nspname, c.relname, a.attnum`

	pgConstraintsSQL = `SELECT n.nspname, c.relname, con.conname, con.contype::text,
       pg_catalog.pg_get_constraintdef(con.oid, true),
       COALESCE(obj_description(con.oid, 'pg_constraint'), '')
FROM pg_catalog.pg_constraint con
JOIN pg_catalog.pg_class c ON c.oid = con.conrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE con.contype IN ('p', 'u', 'f', 'c', 'x') AND ` + pgSchemaFilter + `
ORDER BY n.nspname, c.relname, con.conname`

	pgIndexesSQL = `SELECT n.nspname, tbl.relname, idx.relname,
       pg_catalog.pg_get_indexdef(i.indexrelid),
       i.indisunique, i.indisprimary,
       COALESCE(obj_description(idx.oid, 'pg_class'), '')
FROM pg_catalog.pg_index i
JOIN pg_catalog.pg_class idx ON idx.oid = i.indexrelid
JOIN pg_catalog.pg_class tbl ON tbl.oid = i.indrelid
JOIN pg_catalog.pg_namespace n ON n.oid = tbl.relnamespace
WHERE ` + pgSchemaFilter + `
ORDER BY n.nspname, tbl.relname, idx.relname`

	pgViewsSQL = `SELECT n.nspname, c.relname, c.relkind::text,
       pg_catalog.pg_get_viewdef(c.oid, true),
       COALESCE(obj_description(c.oid, 'pg_class'), '')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('v', 'm') AND ` + pgSchemaFilter + `
ORDER BY n.nspname, c.relname`

	pgSequencesSQL = `SELECT n.nspname, c.relname,
       s.seqstart, s.seqincrement, s.seqmin, s.seqmax, s.seqcycle, s.seqcache,
       COALESCE(obj_description(c.oid, 'pg_class'), '')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_sequence s ON s.seqrelid = c.oid
WHERE c.relkind = 'S' AND ` + pgSchemaFilter + `
ORDER BY n.nspname, c.relname`

	pgTriggersSQL = `SELECT n.nspname, tbl.relname, t.tgname, pg_catalog.pg_get_triggerdef(t.oid, true)
FROM pg_catalog.pg_trigger t
JOIN pg_catalog.pg_class tbl ON tbl.oid = t.tgrelid
JOIN pg_catalog.pg_namespace n ON n.oid = tbl.relnamespace
WHERE NOT t.tgisinternal AND ` + pgSchemaFilter + `
ORDER BY n.nspname, tbl.relname, t.tgname`

	pgMigrationTablesSQL = `SELECT n.nspname, c.relname
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'r'
  AND c.relname IN ('schema_migrations', 'flyway_schema_history', 'databasechangelog')
  AND ` + pgSchemaFilter + `
ORDER BY n.nspname, c.relname`

	pgMigrationColumnsSQL = `SELECT column_name
FROM information_schema.columns
WHERE table_schema = $1 AND table_name = $2
ORDER BY ordinal_position`

	pgTableStatsSQL = `SELECT schemaname, relname,
       COALESCE(seq_scan, 0), COALESCE(seq_tup_read, 0), COALESCE(idx_scan, 0),
       COALESCE(n_live_tup, 0), COALESCE(n_dead_tup, 0),
       COALESCE(last_vacuum::text, ''), COALESCE(last_analyze::text, '')
FROM pg_catalog.pg_stat_user_tables
ORDER BY schemaname, relname`

	pgStatStatementsSchemaSQL = `SELECT n.nspname
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_extension e ON e.extname = 'pg_stat_statements' AND e.extnamespace = n.oid
WHERE c.relname = 'pg_stat_statements' AND c.relkind = 'v'
ORDER BY n.nspname
LIMIT 1`
)

var migrationColumns = map[string][]string{
	"schema_migrations":     {"version", "dirty"},
	"flyway_schema_history": {"installed_rank", "version", "description", "installed_on", "success"},
	"databasechangelog":     {"id", "author", "filename", "dateexecuted", "orderexecuted", "exectype"},
}

type pgColumn struct {
	Ordinal   int16  `json:"ordinal"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	NotNull   bool   `json:"notNull"`
	Default   string `json:"default,omitempty"`
	Comment   string `json:"comment,omitempty"`
	Identity  string `json:"identity,omitempty"`
	Generated string `json:"generated,omitempty"`
}

type pgConstraint struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Definition string `json:"definition"`
	Comment    string `json:"comment,omitempty"`
}

type pgIndex struct {
	Schema     string `json:"schema"`
	Table      string `json:"table"`
	Name       string `json:"name"`
	Definition string `json:"definition"`
	Unique     bool   `json:"unique"`
	Primary    bool   `json:"primary"`
	Comment    string `json:"comment,omitempty"`
}

type pgTable struct {
	Schema      string         `json:"schema"`
	Name        string         `json:"name"`
	Kind        string         `json:"kind"`
	Comment     string         `json:"comment,omitempty"`
	Columns     []pgColumn     `json:"columns"`
	Constraints []pgConstraint `json:"constraints"`
}

type pgView struct {
	Schema     string     `json:"schema"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	Definition string     `json:"definition"`
	Comment    string     `json:"comment,omitempty"`
	Columns    []pgColumn `json:"columns"`
}

type pgSequence struct {
	Schema    string `json:"schema"`
	Name      string `json:"name"`
	Start     int64  `json:"start"`
	Increment int64  `json:"increment"`
	Min       int64  `json:"min"`
	Max       int64  `json:"max"`
	Cycle     bool   `json:"cycle"`
	Cache     int64  `json:"cache"`
	Comment   string `json:"comment,omitempty"`
}

type pgTrigger struct {
	Schema     string `json:"schema"`
	Table      string `json:"table"`
	Name       string `json:"name"`
	Definition string `json:"definition"`
}

type pgTableStat struct {
	Schema      string `json:"schema"`
	Table       string `json:"table"`
	SeqScan     int64  `json:"seqScan"`
	SeqTupRead  int64  `json:"seqTupRead"`
	IdxScan     int64  `json:"idxScan"`
	LiveTup     int64  `json:"liveTup"`
	DeadTup     int64  `json:"deadTup"`
	LastVacuum  string `json:"lastVacuum,omitempty"`
	LastAnalyze string `json:"lastAnalyze,omitempty"`
}

type pgStatementStat struct {
	QueryID       string  `json:"queryId"`
	Calls         int64   `json:"calls"`
	MeanExecTime  float64 `json:"meanExecTimeMs"`
	TotalExecTime float64 `json:"totalExecTimeMs"`
}

type pgStats struct {
	Tables     []pgTableStat     `json:"tables"`
	Statements []pgStatementStat `json:"statements"`
}

type pgSnapshot struct {
	Database      string            `json:"database"`
	ServerVersion string            `json:"serverVersion"`
	Watermark     string            `json:"watermark"`
	IncludeSystem bool              `json:"includeSystem"`
	Tables        []pgTable         `json:"tables"`
	Views         []pgView          `json:"views"`
	Indexes       []pgIndex         `json:"indexes"`
	Sequences     []pgSequence      `json:"sequences"`
	Triggers      []pgTrigger       `json:"triggers"`
	Migrations    []migrationLedger `json:"migrations"`
	Stats         pgStats           `json:"stats"`
}

type pgDigestPayload struct {
	IncludeSystem bool              `json:"includeSystem"`
	Tables        []pgTable         `json:"tables"`
	Views         []pgView          `json:"views"`
	Indexes       []pgIndex         `json:"indexes"`
	Sequences     []pgSequence      `json:"sequences"`
	Triggers      []pgTrigger       `json:"triggers"`
	Migrations    []migrationLedger `json:"migrations"`
}

func (s *pgSnapshot) digest() (string, error) {
	return digestJSON(pgDigestPayload{
		IncludeSystem: s.IncludeSystem,
		Tables:        s.Tables,
		Views:         s.Views,
		Indexes:       s.Indexes,
		Sequences:     s.Sequences,
		Triggers:      s.Triggers,
		Migrations:    s.Migrations,
	})
}

func loadPostgresSnapshot(ctx context.Context, tx pgx.Tx, includeSystem bool) (*pgSnapshot, error) {
	snapshot := &pgSnapshot{IncludeSystem: includeSystem}
	var maxOID string
	if err := tx.QueryRow(ctx, pgDatabaseSQL).Scan(&snapshot.Database, &maxOID); err != nil {
		return nil, err
	}
	snapshot.Watermark = "pg-catalog:" + maxOID
	if err := tx.QueryRow(ctx, pgVersionSQL).Scan(&snapshot.ServerVersion); err != nil {
		return nil, err
	}

	tables, tableByName, err := loadPGTables(ctx, tx, includeSystem)
	if err != nil {
		return nil, err
	}
	snapshot.Tables = tables
	views, viewByName, err := loadPGViews(ctx, tx, includeSystem)
	if err != nil {
		return nil, err
	}
	snapshot.Views = views
	if err := loadPGColumns(ctx, tx, includeSystem, tableByName, viewByName); err != nil {
		return nil, err
	}
	if err := loadPGConstraints(ctx, tx, includeSystem, tableByName); err != nil {
		return nil, err
	}
	indexes, err := loadPGIndexes(ctx, tx, includeSystem)
	if err != nil {
		return nil, err
	}
	snapshot.Indexes = indexes
	sequences, err := loadPGSequences(ctx, tx, includeSystem)
	if err != nil {
		return nil, err
	}
	snapshot.Sequences = sequences
	triggers, err := loadPGTriggers(ctx, tx, includeSystem)
	if err != nil {
		return nil, err
	}
	snapshot.Triggers = triggers
	migrations, err := loadPGMigrations(ctx, tx, includeSystem)
	if err != nil {
		return nil, err
	}
	snapshot.Migrations = migrations
	snapshot.Stats = loadPGStats(ctx, tx)
	snapshot.sort()
	return snapshot, nil
}

func pgObjectMapKey(schema, name string) string { return schema + "\x00" + name }

func loadPGTables(ctx context.Context, tx pgx.Tx, includeSystem bool) ([]pgTable, map[string]*pgTable, error) {
	rows, err := tx.Query(ctx, pgTablesSQL, includeSystem)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var tables []pgTable
	byName := map[string]*pgTable{}
	for rows.Next() {
		var table pgTable
		if err := rows.Scan(&table.Schema, &table.Name, &table.Kind, &table.Comment); err != nil {
			return nil, nil, err
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for i := range tables {
		byName[pgObjectMapKey(tables[i].Schema, tables[i].Name)] = &tables[i]
	}
	return tables, byName, nil
}

func loadPGViews(ctx context.Context, tx pgx.Tx, includeSystem bool) ([]pgView, map[string]*pgView, error) {
	rows, err := tx.Query(ctx, pgViewsSQL, includeSystem)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var views []pgView
	byName := map[string]*pgView{}
	for rows.Next() {
		var view pgView
		if err := rows.Scan(&view.Schema, &view.Name, &view.Kind, &view.Definition, &view.Comment); err != nil {
			return nil, nil, err
		}
		views = append(views, view)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for i := range views {
		byName[pgObjectMapKey(views[i].Schema, views[i].Name)] = &views[i]
	}
	return views, byName, nil
}

func loadPGColumns(ctx context.Context, tx pgx.Tx, includeSystem bool, tables map[string]*pgTable, views map[string]*pgView) error {
	rows, err := tx.Query(ctx, pgColumnsSQL, includeSystem)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var schema, table string
		var column pgColumn
		if err := rows.Scan(&schema, &table, &column.Ordinal, &column.Name, &column.Type, &column.NotNull,
			&column.Default, &column.Comment, &column.Identity, &column.Generated); err != nil {
			return err
		}
		key := pgObjectMapKey(schema, table)
		if target, ok := tables[key]; ok {
			target.Columns = append(target.Columns, column)
			continue
		}
		if target, ok := views[key]; ok {
			target.Columns = append(target.Columns, column)
		}
	}
	return rows.Err()
}

func loadPGConstraints(ctx context.Context, tx pgx.Tx, includeSystem bool, tables map[string]*pgTable) error {
	rows, err := tx.Query(ctx, pgConstraintsSQL, includeSystem)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var schema, table string
		var constraint pgConstraint
		if err := rows.Scan(&schema, &table, &constraint.Name, &constraint.Type, &constraint.Definition, &constraint.Comment); err != nil {
			return err
		}
		if target, ok := tables[pgObjectMapKey(schema, table)]; ok {
			target.Constraints = append(target.Constraints, constraint)
		}
	}
	return rows.Err()
}

func loadPGIndexes(ctx context.Context, tx pgx.Tx, includeSystem bool) ([]pgIndex, error) {
	rows, err := tx.Query(ctx, pgIndexesSQL, includeSystem)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var indexes []pgIndex
	for rows.Next() {
		var index pgIndex
		if err := rows.Scan(&index.Schema, &index.Table, &index.Name, &index.Definition, &index.Unique, &index.Primary, &index.Comment); err != nil {
			return nil, err
		}
		indexes = append(indexes, index)
	}
	return indexes, rows.Err()
}

func loadPGSequences(ctx context.Context, tx pgx.Tx, includeSystem bool) ([]pgSequence, error) {
	rows, err := tx.Query(ctx, pgSequencesSQL, includeSystem)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sequences []pgSequence
	for rows.Next() {
		var sequence pgSequence
		if err := rows.Scan(&sequence.Schema, &sequence.Name, &sequence.Start, &sequence.Increment,
			&sequence.Min, &sequence.Max, &sequence.Cycle, &sequence.Cache, &sequence.Comment); err != nil {
			return nil, err
		}
		sequences = append(sequences, sequence)
	}
	return sequences, rows.Err()
}

func loadPGTriggers(ctx context.Context, tx pgx.Tx, includeSystem bool) ([]pgTrigger, error) {
	rows, err := tx.Query(ctx, pgTriggersSQL, includeSystem)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var triggers []pgTrigger
	for rows.Next() {
		var trigger pgTrigger
		if err := rows.Scan(&trigger.Schema, &trigger.Table, &trigger.Name, &trigger.Definition); err != nil {
			return nil, err
		}
		triggers = append(triggers, trigger)
	}
	return triggers, rows.Err()
}

func loadPGMigrations(ctx context.Context, tx pgx.Tx, includeSystem bool) ([]migrationLedger, error) {
	rows, err := tx.Query(ctx, pgMigrationTablesSQL, includeSystem)
	if err != nil {
		return nil, err
	}
	type ledgerRef struct{ schema, table string }
	var refs []ledgerRef
	for rows.Next() {
		var ref ledgerRef
		if err := rows.Scan(&ref.schema, &ref.table); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	ledgers := make([]migrationLedger, 0, len(refs))
	for _, ref := range refs {
		selected, err := migrationColumnsPresent(ctx, tx, ref.schema, ref.table)
		if err != nil {
			return nil, err
		}
		if len(selected) == 0 {
			continue
		}
		entries, err := readMigrationRows(ctx, tx, ref.schema, ref.table, selected)
		if err != nil {
			return nil, err
		}
		ledgers = append(ledgers, migrationLedger{Schema: ref.schema, Table: ref.table, Entries: entries})
	}
	return sortMigrations(ledgers), nil
}

func migrationColumnsPresent(ctx context.Context, tx pgx.Tx, schema, table string) ([]string, error) {
	rows, err := tx.Query(ctx, pgMigrationColumnsSQL, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	known, ok := migrationColumns[table]
	if !ok {
		return nil, nil
	}
	selected := make([]string, 0, len(known))
	for _, name := range known {
		if present[name] {
			selected = append(selected, name)
		}
	}
	return selected, nil
}

func readMigrationRows(ctx context.Context, tx pgx.Tx, schema, table string, columns []string) ([]migrationEntry, error) {
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = quoteIdent(column)
	}
	sql := "SELECT " + strings.Join(quoted, ", ") + " FROM " + qualified(schema, table) +
		" ORDER BY " + quoteIdent(columns[0]) + " LIMIT " + strconv.Itoa(pgMigrationLimit)
	rows, err := tx.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []migrationEntry
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, err
		}
		entry := migrationEntry{Fields: make([]migrationField, 0, len(columns))}
		for i, column := range columns {
			entry.Fields = append(entry.Fields, migrationField{Key: column, Value: formatDBValue(values[i])})
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func loadPGStats(ctx context.Context, tx pgx.Tx) pgStats {
	var stats pgStats
	rows, err := tx.Query(ctx, pgTableStatsSQL)
	if err == nil {
		for rows.Next() {
			var entry pgTableStat
			if scanErr := rows.Scan(&entry.Schema, &entry.Table, &entry.SeqScan, &entry.SeqTupRead, &entry.IdxScan,
				&entry.LiveTup, &entry.DeadTup, &entry.LastVacuum, &entry.LastAnalyze); scanErr != nil {
				break
			}
			stats.Tables = append(stats.Tables, entry)
		}
		rows.Close()
	}
	var schema string
	if err := tx.QueryRow(ctx, pgStatStatementsSchemaSQL).Scan(&schema); err != nil || schema == "" {
		return stats
	}
	statementSQL := "SELECT COALESCE(queryid::text, ''), COALESCE(calls, 0), COALESCE(mean_exec_time, 0), COALESCE(total_exec_time, 0)" +
		" FROM " + qualified(schema, "pg_stat_statements") +
		" ORDER BY total_exec_time DESC NULLS LAST LIMIT " + strconv.Itoa(pgStatsLimit)
	statementRows, err := tx.Query(ctx, statementSQL)
	if err != nil {
		return stats
	}
	defer statementRows.Close()
	for statementRows.Next() {
		var entry pgStatementStat
		if err := statementRows.Scan(&entry.QueryID, &entry.Calls, &entry.MeanExecTime, &entry.TotalExecTime); err != nil {
			break
		}
		stats.Statements = append(stats.Statements, entry)
	}
	return stats
}

func (s *pgSnapshot) sort() {
	sort.Slice(s.Tables, func(i, j int) bool {
		if s.Tables[i].Schema != s.Tables[j].Schema {
			return s.Tables[i].Schema < s.Tables[j].Schema
		}
		return s.Tables[i].Name < s.Tables[j].Name
	})
	for i := range s.Tables {
		sort.Slice(s.Tables[i].Columns, func(a, b int) bool { return s.Tables[i].Columns[a].Ordinal < s.Tables[i].Columns[b].Ordinal })
		sort.Slice(s.Tables[i].Constraints, func(a, b int) bool { return s.Tables[i].Constraints[a].Name < s.Tables[i].Constraints[b].Name })
	}
	sort.Slice(s.Views, func(i, j int) bool {
		if s.Views[i].Schema != s.Views[j].Schema {
			return s.Views[i].Schema < s.Views[j].Schema
		}
		return s.Views[i].Name < s.Views[j].Name
	})
	for i := range s.Views {
		sort.Slice(s.Views[i].Columns, func(a, b int) bool { return s.Views[i].Columns[a].Ordinal < s.Views[i].Columns[b].Ordinal })
	}
	sort.Slice(s.Indexes, func(i, j int) bool {
		if s.Indexes[i].Schema != s.Indexes[j].Schema {
			return s.Indexes[i].Schema < s.Indexes[j].Schema
		}
		if s.Indexes[i].Table != s.Indexes[j].Table {
			return s.Indexes[i].Table < s.Indexes[j].Table
		}
		return s.Indexes[i].Name < s.Indexes[j].Name
	})
	sort.Slice(s.Sequences, func(i, j int) bool {
		if s.Sequences[i].Schema != s.Sequences[j].Schema {
			return s.Sequences[i].Schema < s.Sequences[j].Schema
		}
		return s.Sequences[i].Name < s.Sequences[j].Name
	})
	sort.Slice(s.Triggers, func(i, j int) bool {
		if s.Triggers[i].Schema != s.Triggers[j].Schema {
			return s.Triggers[i].Schema < s.Triggers[j].Schema
		}
		if s.Triggers[i].Table != s.Triggers[j].Table {
			return s.Triggers[i].Table < s.Triggers[j].Table
		}
		return s.Triggers[i].Name < s.Triggers[j].Name
	})
	s.Migrations = sortMigrations(s.Migrations)
}

func (s *pgSnapshot) items(connection domain.Connection, hash string) []domain.Item {
	items := make([]domain.Item, 0, len(s.Tables)+len(s.Views)+len(s.Indexes)+len(s.Sequences)+len(s.Triggers)+len(s.Migrations)+2)
	items = append(items, itemFor(connection, PostgresProvider, s.Database, "database",
		"Database "+s.Database, "database", s.renderOverview(hash)))
	for _, table := range s.Tables {
		items = append(items, itemFor(connection, PostgresProvider, s.Database,
			objectKey("table", table.Schema, table.Name),
			"Table "+table.Schema+"."+table.Name, "table", s.renderTable(table)))
	}
	for _, view := range s.Views {
		items = append(items, itemFor(connection, PostgresProvider, s.Database,
			objectKey("view", view.Schema, view.Name),
			"View "+view.Schema+"."+view.Name, "view", s.renderView(view)))
	}
	for _, index := range s.Indexes {
		items = append(items, itemFor(connection, PostgresProvider, s.Database,
			objectKey("index", index.Schema, index.Name),
			"Index "+index.Schema+"."+index.Name, "index", s.renderIndex(index)))
	}
	for _, sequence := range s.Sequences {
		items = append(items, itemFor(connection, PostgresProvider, s.Database,
			objectKey("sequence", sequence.Schema, sequence.Name),
			"Sequence "+sequence.Schema+"."+sequence.Name, "sequence", s.renderSequence(sequence)))
	}
	for _, trigger := range s.Triggers {
		items = append(items, itemFor(connection, PostgresProvider, s.Database,
			objectKey("trigger", trigger.Schema, trigger.Table, trigger.Name),
			"Trigger "+trigger.Schema+"."+trigger.Table+"."+trigger.Name, "trigger", s.renderTrigger(trigger)))
	}
	if len(s.Migrations) > 0 {
		items = append(items, itemFor(connection, PostgresProvider, s.Database,
			"migrations", "Migration ledger "+s.Database, "migrations", s.renderMigrations()))
	}
	items = append(items, itemFor(connection, PostgresProvider, s.Database,
		"stats", "Statistics "+s.Database, "stats", s.renderStats()))
	sort.Slice(items, func(i, j int) bool { return items[i].Ref.ExternalID < items[j].Ref.ExternalID })
	return items
}

func (s *pgSnapshot) renderOverview(hash string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Database %s\n\n", quoteIdent(s.Database))
	fmt.Fprintf(&builder, "- Schema digest: `%s`\n", hash)
	fmt.Fprintf(&builder, "- Catalog watermark: `%s`\n", s.Watermark)
	fmt.Fprintf(&builder, "- Server version: `%s`\n", markdownCell(s.ServerVersion))
	fmt.Fprintf(&builder, "- Include system schemas: %t\n", s.IncludeSystem)
	fmt.Fprintf(&builder, "- Tables: %d\n", len(s.Tables))
	fmt.Fprintf(&builder, "- Views and materialized views: %d\n", len(s.Views))
	fmt.Fprintf(&builder, "- Indexes: %d\n", len(s.Indexes))
	fmt.Fprintf(&builder, "- Sequences: %d\n", len(s.Sequences))
	fmt.Fprintf(&builder, "- Triggers: %d\n", len(s.Triggers))
	fmt.Fprintf(&builder, "- Migration ledgers: %d\n", len(s.Migrations))
	return builder.String()
}

func (s *pgSnapshot) indexesFor(schema, table string) []pgIndex {
	var out []pgIndex
	for _, index := range s.Indexes {
		if index.Schema == schema && index.Table == table {
			out = append(out, index)
		}
	}
	return out
}

func (s *pgSnapshot) renderTable(table pgTable) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Table %s\n\n", qualified(table.Schema, table.Name))
	if table.Comment != "" {
		fmt.Fprintf(&builder, "%s\n\n", markdownCell(table.Comment))
	}
	fmt.Fprintf(&builder, "Kind: `%s`\n\n", markdownCell(table.Kind))
	builder.WriteString("## Definition\n\n")
	builder.WriteString(codeBlock(pgTableDDL(table)))
	builder.WriteString("\n\n## Columns\n\n")
	writePGColumns(&builder, table.Columns)
	if len(table.Constraints) > 0 {
		builder.WriteString("\n## Constraints\n\n")
		builder.WriteString("| Name | Type | Definition | Comment |\n|---|---|---|---|\n")
		for _, constraint := range table.Constraints {
			fmt.Fprintf(&builder, "| `%s` | `%s` | `%s` | %s |\n",
				markdownCell(constraint.Name), markdownCell(constraint.Type),
				markdownCell(constraint.Definition), markdownCell(constraint.Comment))
		}
	}
	indexes := s.indexesFor(table.Schema, table.Name)
	if len(indexes) > 0 {
		builder.WriteString("\n## Indexes\n\n")
		for _, index := range indexes {
			fmt.Fprintf(&builder, "- `%s`: `%s`\n", markdownCell(index.Name), markdownCell(index.Definition))
		}
	}
	return builder.String()
}

func pgTableDDL(table pgTable) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "CREATE TABLE %s (\n", qualified(table.Schema, table.Name))
	var lines []string
	for _, column := range table.Columns {
		line := "  " + quoteIdent(column.Name) + " " + column.Type
		switch {
		case column.Generated == "s":
			line += " GENERATED ALWAYS AS (" + strings.TrimSpace(column.Default) + ") STORED"
		case column.Identity == "a":
			line += " GENERATED ALWAYS AS IDENTITY"
		case column.Identity == "d":
			line += " GENERATED BY DEFAULT AS IDENTITY"
		case strings.TrimSpace(column.Default) != "":
			line += " DEFAULT " + column.Default
		}
		if column.NotNull {
			line += " NOT NULL"
		}
		lines = append(lines, line)
	}
	for _, constraint := range table.Constraints {
		lines = append(lines, "  CONSTRAINT "+quoteIdent(constraint.Name)+" "+constraint.Definition)
	}
	builder.WriteString(strings.Join(lines, ",\n"))
	builder.WriteString("\n);")
	return builder.String()
}

func writePGColumns(builder *strings.Builder, columns []pgColumn) {
	if len(columns) == 0 {
		builder.WriteString("_No columns._\n")
		return
	}
	builder.WriteString("| # | Column | Type | Nullable | Default | Identity | Generated | Comment |\n")
	builder.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, column := range columns {
		nullable := "no"
		if !column.NotNull {
			nullable = "yes"
		}
		fmt.Fprintf(builder, "| %d | `%s` | `%s` | %s | `%s` | `%s` | `%s` | %s |\n",
			column.Ordinal, markdownCell(column.Name), markdownCell(column.Type), nullable,
			markdownCell(column.Default), markdownCell(column.Identity), markdownCell(column.Generated),
			markdownCell(column.Comment))
	}
}

func (s *pgSnapshot) renderView(view pgView) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# View %s\n\n", qualified(view.Schema, view.Name))
	if view.Comment != "" {
		fmt.Fprintf(&builder, "%s\n\n", markdownCell(view.Comment))
	}
	fmt.Fprintf(&builder, "Kind: `%s`\n\n", markdownCell(view.Kind))
	builder.WriteString("## Definition\n\n")
	kind := "CREATE VIEW"
	if view.Kind == "m" {
		kind = "CREATE MATERIALIZED VIEW"
	}
	definition := strings.TrimRight(strings.TrimSpace(view.Definition), ";")
	builder.WriteString(codeBlock(kind + " " + qualified(view.Schema, view.Name) + " AS\n" + definition + ";"))
	builder.WriteString("\n\n## Columns\n\n")
	writePGColumns(&builder, view.Columns)
	return builder.String()
}

func (s *pgSnapshot) renderIndex(index pgIndex) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Index %s\n\n", qualified(index.Schema, index.Name))
	fmt.Fprintf(&builder, "- Table: %s\n", qualified(index.Schema, index.Table))
	fmt.Fprintf(&builder, "- Unique: %t\n", index.Unique)
	fmt.Fprintf(&builder, "- Primary: %t\n", index.Primary)
	if index.Comment != "" {
		fmt.Fprintf(&builder, "- Comment: %s\n", markdownCell(index.Comment))
	}
	builder.WriteString("\n## Definition\n\n")
	builder.WriteString(codeBlock(index.Definition))
	return builder.String()
}

func (s *pgSnapshot) renderSequence(sequence pgSequence) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Sequence %s\n\n", qualified(sequence.Schema, sequence.Name))
	if sequence.Comment != "" {
		fmt.Fprintf(&builder, "%s\n\n", markdownCell(sequence.Comment))
	}
	cycle := ""
	if sequence.Cycle {
		cycle = " CYCLE"
	}
	ddl := fmt.Sprintf("CREATE SEQUENCE %s\n  START WITH %d\n  INCREMENT BY %d\n  MINVALUE %d\n  MAXVALUE %d\n  CACHE %d%s;",
		qualified(sequence.Schema, sequence.Name), sequence.Start, sequence.Increment,
		sequence.Min, sequence.Max, sequence.Cache, cycle)
	builder.WriteString(codeBlock(ddl))
	return builder.String()
}

func (s *pgSnapshot) renderTrigger(trigger pgTrigger) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "# Trigger %s.%s.%s\n\n", quoteIdent(trigger.Schema), quoteIdent(trigger.Table), quoteIdent(trigger.Name))
	builder.WriteString(codeBlock(trigger.Definition))
	return builder.String()
}

func (s *pgSnapshot) renderMigrations() string {
	var builder strings.Builder
	builder.WriteString("# Migration ledgers\n")
	for _, ledger := range s.Migrations {
		fmt.Fprintf(&builder, "\n## %s\n\n", qualified(ledger.Schema, ledger.Table))
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

func (s *pgSnapshot) renderStats() string {
	var builder strings.Builder
	builder.WriteString("# Database statistics\n\n")
	builder.WriteString("Aggregate counters only; no SQL text and no parameter values.\n\n")
	builder.WriteString("## Tables\n\n")
	if len(s.Stats.Tables) == 0 {
		builder.WriteString("_No table statistics available._\n")
	} else {
		builder.WriteString("| Schema | Table | Seq scans | Seq tuples | Index scans | Live tuples | Dead tuples | Last vacuum | Last analyze |\n")
		builder.WriteString("|---|---|---|---|---|---|---|---|---|\n")
		for _, entry := range s.Stats.Tables {
			fmt.Fprintf(&builder, "| %s | %s | %d | %d | %d | %d | %d | %s | %s |\n",
				markdownCell(entry.Schema), markdownCell(entry.Table), entry.SeqScan, entry.SeqTupRead,
				entry.IdxScan, entry.LiveTup, entry.DeadTup, markdownCell(entry.LastVacuum), markdownCell(entry.LastAnalyze))
		}
	}
	builder.WriteString("\n## Statements\n\n")
	if len(s.Stats.Statements) == 0 {
		builder.WriteString("_No pg_stat_statements aggregate available._\n")
	} else {
		builder.WriteString("| Query ID | Calls | Mean ms | Total ms |\n|---|---|---|---|\n")
		for _, entry := range s.Stats.Statements {
			fmt.Fprintf(&builder, "| `%s` | %d | %.3f | %.3f |\n",
				markdownCell(entry.QueryID), entry.Calls, entry.MeanExecTime, entry.TotalExecTime)
		}
	}
	return builder.String()
}
