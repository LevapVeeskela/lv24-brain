package dbschema

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

type staticCredentials map[string]string

func (s staticCredentials) Get(_ context.Context, _ string, field string) (string, error) {
	value, ok := s[field]
	if !ok {
		return "", domain.ErrNotFound
	}
	return value, nil
}

func postgresTestConnection() domain.Connection {
	return domain.Connection{
		ID:       "pg-schema-test",
		Provider: PostgresProvider,
		Settings: map[string]string{
			"host":     "db.example.com",
			"port":     "5433",
			"database": "app",
			"user":     "reader",
			"sslmode":  "verify-full",
		},
	}
}

func mustPostgresSource(t *testing.T, conn domain.Connection) *PostgresSource {
	t.Helper()
	source, err := NewPostgresSource(conn, staticCredentials{"password": "s3cret"}, nil)
	if err != nil {
		t.Fatalf("NewPostgresSource: %v", err)
	}
	return source
}

func TestPostgresDescriptor(t *testing.T) {
	descriptor := PostgresDescriptor()
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("Descriptor.Validate: %v", err)
	}
	if descriptor.Provider != PostgresProvider || descriptor.DisplayName != "PostgreSQL schema" {
		t.Fatalf("identity = %q/%q", descriptor.Provider, descriptor.DisplayName)
	}
	if !descriptor.Capabilities.Backfill || !descriptor.Capabilities.Incremental || descriptor.Capabilities.Deletes {
		t.Fatalf("capabilities = %+v", descriptor.Capabilities)
	}
	if len(descriptor.Capabilities.AuthModes) != 1 || descriptor.Capabilities.AuthModes[0] != domain.AuthToken {
		t.Fatalf("auth modes = %v", descriptor.Capabilities.AuthModes)
	}
	if descriptor.DefaultPublish != domain.PublishShared {
		t.Fatalf("default publish = %q", descriptor.DefaultPublish)
	}
	if len(descriptor.Streams) != 1 || descriptor.Streams[0] != postgresStream {
		t.Fatalf("streams = %v", descriptor.Streams)
	}
	keys := map[string]domain.ConfigField{}
	for _, field := range descriptor.ConfigFields {
		keys[field.Key] = field
	}
	for _, key := range []string{"host", "port", "database", "user", "password", "sslmode", "includesystem"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("missing config field %q", key)
		}
	}
	if !keys["host"].Required || !keys["database"].Required || !keys["user"].Required {
		t.Error("host/database/user must be required")
	}
	if !keys["password"].Secret || !keys["password"].Required {
		t.Error("password must be a required secret")
	}
	if len(descriptor.Backends) != 1 || descriptor.Backends[0] != domain.BackendNative {
		t.Fatalf("backends = %v", descriptor.Backends)
	}
}

func TestPostgresSettingsValidation(t *testing.T) {
	base := postgresTestConnection()
	cases := []struct {
		name     string
		mutate   func(*domain.Connection)
		settings func(*domain.Connection)
	}{
		{name: "missing host", settings: func(c *domain.Connection) { delete(c.Settings, "host") }},
		{name: "host with scheme", settings: func(c *domain.Connection) { c.Settings["host"] = "postgres://db" }},
		{name: "host with path", settings: func(c *domain.Connection) { c.Settings["host"] = "db/extra" }},
		{name: "missing database", settings: func(c *domain.Connection) { delete(c.Settings, "database") }},
		{name: "missing user", settings: func(c *domain.Connection) { delete(c.Settings, "user") }},
		{name: "bad port", settings: func(c *domain.Connection) { c.Settings["port"] = "abc" }},
		{name: "port zero", settings: func(c *domain.Connection) { c.Settings["port"] = "0" }},
		{name: "port too high", settings: func(c *domain.Connection) { c.Settings["port"] = "70000" }},
		{name: "bad sslmode", settings: func(c *domain.Connection) { c.Settings["sslmode"] = "prefer" }},
		{name: "bad includesystem", settings: func(c *domain.Connection) { c.Settings["includesystem"] = "maybe" }},
		{name: "base url userinfo", mutate: func(c *domain.Connection) { c.BaseURL = "postgres://user:pass@db.example.com:5433" }},
		{name: "base url query", mutate: func(c *domain.Connection) { c.BaseURL = "postgres://db.example.com:5433?sslmode=disable" }},
		{name: "base url fragment", mutate: func(c *domain.Connection) { c.BaseURL = "postgres://db.example.com:5433#frag" }},
		{name: "base url path", mutate: func(c *domain.Connection) { c.BaseURL = "postgres://db.example.com:5433/app" }},
		{name: "base url wrong host", mutate: func(c *domain.Connection) { c.BaseURL = "postgres://other.example.com:5433" }},
		{name: "base url wrong port", mutate: func(c *domain.Connection) { c.BaseURL = "postgres://db.example.com:5432" }},
		{name: "base url wrong scheme", mutate: func(c *domain.Connection) { c.BaseURL = "https://db.example.com:5433" }},
	}
	for _, testCase := range cases {
		conn := base
		conn.Settings = map[string]string{}
		for key, value := range base.Settings {
			conn.Settings[key] = value
		}
		if testCase.settings != nil {
			testCase.settings(&conn)
		}
		if testCase.mutate != nil {
			testCase.mutate(&conn)
		}
		if _, err := NewPostgresSource(conn, staticCredentials{"password": "s3cret"}, nil); err == nil {
			t.Errorf("%s: NewPostgresSource succeeded, want an error", testCase.name)
		}
	}
}

func TestPostgresBaseURLValid(t *testing.T) {
	conn := postgresTestConnection()
	conn.BaseURL = "postgres://db.example.com:5433"
	if _, err := NewPostgresSource(conn, staticCredentials{"password": "s3cret"}, nil); err != nil {
		t.Fatalf("valid BaseURL rejected: %v", err)
	}
	conn.BaseURL = "postgresql://DB.EXAMPLE.COM:5433/"
	if _, err := NewPostgresSource(conn, staticCredentials{"password": "s3cret"}, nil); err != nil {
		t.Fatalf("valid BaseURL rejected: %v", err)
	}
}

func TestPostgresCredentialsRequired(t *testing.T) {
	if _, err := NewPostgresSource(postgresTestConnection(), nil, nil); err == nil {
		t.Fatal("NewPostgresSource accepted nil credentials")
	}
}

func TestPostgresConnConfig(t *testing.T) {
	source := mustPostgresSource(t, postgresTestConnection())
	config, err := source.connConfig("s3cret")
	if err != nil {
		t.Fatalf("connConfig: %v", err)
	}
	if config.Host != "db.example.com" || config.Port != 5433 || config.Database != "app" || config.User != "reader" {
		t.Fatalf("config endpoint = %s:%d/%s user=%s", config.Host, config.Port, config.Database, config.User)
	}
	if config.Password != "s3cret" {
		t.Fatal("password was not assigned to the parsed config")
	}
	if config.RuntimeParams["default_transaction_read_only"] != "on" {
		t.Fatalf("default_transaction_read_only = %q", config.RuntimeParams["default_transaction_read_only"])
	}
	if config.RuntimeParams["application_name"] != pgApplicationName {
		t.Fatalf("application_name = %q", config.RuntimeParams["application_name"])
	}
	if config.TLSConfig == nil {
		t.Fatal("verify-full must configure TLS")
	}
	if config.ConnectTimeout == 0 {
		t.Fatal("connect timeout must be bounded")
	}

	conn := postgresTestConnection()
	conn.Settings["sslmode"] = "disable"
	disabled := mustPostgresSource(t, conn)
	config, err = disabled.connConfig("s3cret")
	if err != nil {
		t.Fatalf("connConfig disable: %v", err)
	}
	if config.TLSConfig != nil {
		t.Fatal("sslmode=disable must not configure TLS")
	}
}

func TestPostgresCatalogOnlySQL(t *testing.T) {
	queries := []string{
		pgReadOnlySQL, pgDatabaseSQL, pgVersionSQL, pgTablesSQL, pgColumnsSQL,
		pgConstraintsSQL, pgIndexesSQL, pgViewsSQL, pgSequencesSQL, pgTriggersSQL,
		pgMigrationTablesSQL, pgMigrationColumnsSQL, pgTableStatsSQL, pgStatStatementsSchemaSQL,
	}
	for _, query := range queries {
		lower := strings.ToLower(query)
		for _, forbidden := range []string{"insert ", "update ", "delete ", "copy ", "pg_dump", "create ", "drop ", "alter ", "truncate "} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("catalog query contains forbidden %q: %s", forbidden, query)
			}
		}
		if strings.Contains(lower, "select *") {
			t.Errorf("catalog query uses SELECT *: %s", query)
		}
	}
	allowed := map[string]bool{
		"pg_catalog.pg_attrdef":          true,
		"pg_catalog.pg_attribute":        true,
		"pg_catalog.pg_class":            true,
		"pg_catalog.pg_constraint":       true,
		"pg_catalog.pg_extension":        true,
		"pg_catalog.pg_index":            true,
		"pg_catalog.pg_namespace":        true,
		"pg_catalog.pg_sequence":         true,
		"pg_catalog.pg_stat_user_tables": true,
		"pg_catalog.pg_trigger":          true,
		"information_schema.columns":     true,
	}
	fromTarget := regexp.MustCompile(`(?i)\b(?:from|join)\s+([a-z_][a-z0-9_.]*)`)
	for _, query := range queries {
		for _, match := range fromTarget.FindAllStringSubmatch(query, -1) {
			target := strings.ToLower(match[1])
			if !allowed[target] {
				t.Errorf("catalog query reads an unlisted relation %q: %s", target, query)
			}
		}
	}
	if !strings.Contains(strings.ToLower(pgReadOnlySQL), "transaction_read_only") {
		t.Fatal("read-only verification SQL does not inspect transaction_read_only")
	}
	if !strings.Contains(strings.ToLower(pgReadOnlySQL), "default_transaction_read_only") {
		t.Fatal("read-only verification SQL does not inspect default_transaction_read_only")
	}
}

func TestPostgresSnapshotItems(t *testing.T) {
	conn := domain.Connection{ID: "pg-schema-test", Provider: PostgresProvider, Projects: []string{"proj"}}
	snapshot := &pgSnapshot{
		Database:      "app",
		ServerVersion: "16.4",
		Watermark:     "pg-catalog:12345",
		Tables: []pgTable{{
			Schema: "public", Name: "users", Kind: "r", Comment: "user accounts",
			Columns: []pgColumn{
				{Ordinal: 1, Name: "id", Type: "bigint", NotNull: true, Default: "nextval('users_id_seq'::regclass)"},
				{Ordinal: 2, Name: "email", Type: "text", NotNull: true, Comment: "unique address"},
			},
			Constraints: []pgConstraint{{Name: "users_pkey", Type: "p", Definition: "PRIMARY KEY (id)"}},
		}},
		Views: []pgView{{
			Schema: "public", Name: "active_users", Kind: "v",
			Definition: "SELECT users.id, users.email FROM public.users;",
			Columns:    []pgColumn{{Ordinal: 1, Name: "id", Type: "bigint", NotNull: true}},
		}},
		Indexes: []pgIndex{{
			Schema: "public", Table: "users", Name: "users_email_idx",
			Definition: "CREATE UNIQUE INDEX users_email_idx ON public.users USING btree (email)",
			Unique:     true,
		}},
		Sequences: []pgSequence{{
			Schema: "public", Name: "users_id_seq", Start: 1, Increment: 1, Min: 1, Max: 9223372036854775807, Cache: 1,
		}},
		Triggers: []pgTrigger{{
			Schema: "public", Table: "users", Name: "users_audit",
			Definition: "CREATE TRIGGER users_audit AFTER INSERT ON public.users FOR EACH ROW EXECUTE FUNCTION audit()",
		}},
		Migrations: []migrationLedger{{
			Schema: "public", Table: "schema_migrations",
			Entries: []migrationEntry{{Fields: []migrationField{{Key: "version", Value: "20260101"}, {Key: "dirty", Value: "false"}}}},
		}},
		Stats: pgStats{
			Tables:     []pgTableStat{{Schema: "public", Table: "users", SeqScan: 2, LiveTup: 10}},
			Statements: []pgStatementStat{{QueryID: "42", Calls: 9, MeanExecTime: 1.5, TotalExecTime: 13.5}},
		},
	}
	hash, err := snapshot.digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	again, err := snapshot.digest()
	if err != nil || hash != again {
		t.Fatalf("digest is not deterministic: %q vs %q (%v)", hash, again, err)
	}
	items := snapshot.items(conn, hash)
	byID := itemsByExternalID(items)
	required := []string{
		"postgres-schema/app/database",
		"postgres-schema/app/table:public.users",
		"postgres-schema/app/view:public.active_users",
		"postgres-schema/app/index:public.users_email_idx",
		"postgres-schema/app/sequence:public.users_id_seq",
		"postgres-schema/app/trigger:public.users.users_audit",
		"postgres-schema/app/migrations",
		"postgres-schema/app/stats",
	}
	for _, externalID := range required {
		if _, ok := byID[externalID]; !ok {
			t.Errorf("missing item %q", externalID)
		}
	}
	table := byID["postgres-schema/app/table:public.users"]
	if !strings.Contains(table.Body, `CREATE TABLE "public"."users"`) ||
		!strings.Contains(table.Body, `"id" bigint DEFAULT nextval`) ||
		!strings.Contains(table.Body, `CONSTRAINT "users_pkey" PRIMARY KEY (id)`) {
		t.Errorf("table body lacks DDL-like content: %q", table.Body)
	}
	if table.ProjectID != "proj" {
		t.Errorf("project mapping = %q", table.ProjectID)
	}
	view := byID["postgres-schema/app/view:public.active_users"].Body
	if !strings.Contains(view, `CREATE VIEW "public"."active_users" AS`) {
		t.Errorf("view body lacks CREATE VIEW: %q", view)
	}
	if !strings.Contains(byID["postgres-schema/app/index:public.users_email_idx"].Body, "CREATE UNIQUE INDEX") {
		t.Error("index body lacks its definition")
	}
	if !strings.Contains(byID["postgres-schema/app/sequence:public.users_id_seq"].Body, "CREATE SEQUENCE") {
		t.Error("sequence body lacks its definition")
	}
	if !strings.Contains(byID["postgres-schema/app/trigger:public.users.users_audit"].Body, "CREATE TRIGGER") {
		t.Error("trigger body lacks its definition")
	}
	if !strings.Contains(byID["postgres-schema/app/migrations"].Body, "20260101") {
		t.Error("migration body lacks the ledger row")
	}
	stats := byID["postgres-schema/app/stats"].Body
	if !strings.Contains(stats, "|| 42 |") && !strings.Contains(stats, "| `42` |") {
		t.Errorf("stats body lacks the aggregate row: %q", stats)
	}
	for _, item := range items {
		if err := item.WithHash().Validate(); err != nil {
			t.Fatalf("item %q invalid: %v", item.Ref.ExternalID, err)
		}
	}

	// Stats are volatile and must not invalidate the schema digest.
	snapshot.Stats.Tables[0].SeqScan = 999
	statsIgnored, err := snapshot.digest()
	if err != nil || statsIgnored != hash {
		t.Fatal("statistics changed the schema digest")
	}
	// A schema change must change the digest.
	snapshot.Tables[0].Columns[1].Type = "varchar(320)"
	changed, err := snapshot.digest()
	if err != nil {
		t.Fatalf("digest after change: %v", err)
	}
	if changed == hash {
		t.Fatal("schema change did not change the digest")
	}
}

func TestPostgresCheckpointSharedValidation(t *testing.T) {
	cases := []snapshotCheckpoint{
		{Hash: "", Watermark: "", Offset: 1},
		{Hash: "nope", Watermark: ""},
		{Hash: "sha256:abc", Watermark: ""},
		{Hash: "sha256:" + strings.Repeat("z", 64), Watermark: ""},
		{Hash: "sha256:" + strings.Repeat("0", 64), Watermark: "", Offset: -1},
	}
	for i, checkpoint := range cases {
		if err := checkpoint.validate(); err == nil {
			t.Errorf("case %d: invalid checkpoint accepted", i)
		}
	}
	valid := snapshotCheckpoint{Hash: "sha256:" + strings.Repeat("0", 64), Watermark: "pg-catalog:1"}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid checkpoint rejected: %v", err)
	}
}
