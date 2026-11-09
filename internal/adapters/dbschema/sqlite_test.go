package dbschema

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ingestiontest"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const sqliteCanarySecret = "CANARY_DO_NOT_READ_7f3c1a"

func sqliteTestConnection(path string, extra map[string]string) domain.Connection {
	settings := map[string]string{"path": path}
	for key, value := range extra {
		settings[key] = value
	}
	return domain.Connection{ID: "sqlite-schema-test", Provider: SQLiteProvider, Settings: settings}
}

func createSQLiteFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = db.Close() }()
	statements := []string{
		`PRAGMA user_version = 7`,
		`PRAGMA application_id = 4242`,
		`CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			email TEXT UNIQUE DEFAULT 'none'
		)`,
		`CREATE TABLE posts (
			id INTEGER PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			title TEXT
		)`,
		`CREATE INDEX idx_posts_title ON posts(title)`,
		`CREATE VIEW user_posts AS SELECT u.name, p.title FROM users u JOIN posts p ON p.user_id = u.id`,
		`CREATE TRIGGER trg_posts_after AFTER INSERT ON posts
		BEGIN
			UPDATE users SET name = name WHERE id = NEW.user_id;
		END`,
		`CREATE TABLE auto_ids (id INTEGER PRIMARY KEY AUTOINCREMENT, value TEXT)`,
		`CREATE TABLE commented (
			id INTEGER PRIMARY KEY, -- audit: comment-marker
			note TEXT /* inline comment-marker */
		)`,
		`CREATE TABLE canary (secret TEXT)`,
		`INSERT INTO canary (secret) VALUES ('` + sqliteCanarySecret + `')`,
		`CREATE TABLE schema_migrations (version TEXT NOT NULL, dirty BOOLEAN NOT NULL)`,
		`INSERT INTO schema_migrations (version, dirty) VALUES ('20260101', 0), ('20260102', 0)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
	return path
}

func mustSQLiteSource(t *testing.T, conn domain.Connection) *SQLiteSource {
	t.Helper()
	source, err := NewSQLiteSource(conn, nil)
	if err != nil {
		t.Fatalf("NewSQLiteSource: %v", err)
	}
	return source
}

func cloneCheckpoints(in map[string]domain.Checkpoint) map[string]domain.Checkpoint {
	out := make(map[string]domain.Checkpoint, len(in))
	for stream, checkpoint := range in {
		out[stream] = checkpoint.Clone()
	}
	return out
}

func readSQLiteAll(t *testing.T, source *SQLiteSource, conn domain.Connection, checkpoints map[string]domain.Checkpoint, limit int, since *time.Time) (ports.ReadBatch, map[string]domain.Checkpoint) {
	t.Helper()
	checkpoints = cloneCheckpoints(checkpoints)
	var collected []domain.Item
	var last ports.ReadBatch
	for batch := 0; ; batch++ {
		if batch > 1000 {
			t.Fatal("More never cleared")
		}
		next, err := source.Read(context.Background(), ports.ReadRequest{
			Connection: conn, Checkpoints: checkpoints, Limit: limit, Since: since,
		})
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		last = next
		collected = append(collected, next.Items...)
		for _, checkpoint := range next.Checkpoints {
			checkpoints[checkpoint.Stream] = checkpoint
		}
		if !next.More {
			break
		}
	}
	last.Items = collected
	return last, checkpoints
}

func itemsByExternalID(items []domain.Item) map[string]domain.Item {
	out := make(map[string]domain.Item, len(items))
	for _, item := range items {
		out[item.Ref.ExternalID] = item
	}
	return out
}

func decodeSQLiteCheckpoint(t *testing.T, batch ports.ReadBatch) snapshotCheckpoint {
	t.Helper()
	if len(batch.Checkpoints) != 1 {
		t.Fatalf("checkpoints = %d, want 1", len(batch.Checkpoints))
	}
	var checkpoint snapshotCheckpoint
	if err := json.Unmarshal(batch.Checkpoints[0].Data, &checkpoint); err != nil {
		t.Fatalf("decode checkpoint: %v", err)
	}
	return checkpoint
}

func TestSQLiteDescriptor(t *testing.T) {
	descriptor := SQLiteDescriptor()
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("Descriptor.Validate: %v", err)
	}
	if descriptor.Provider != SQLiteProvider || descriptor.DisplayName != "SQLite schema" {
		t.Fatalf("identity = %q/%q", descriptor.Provider, descriptor.DisplayName)
	}
	if !descriptor.Capabilities.Backfill || !descriptor.Capabilities.Incremental || descriptor.Capabilities.Deletes {
		t.Fatalf("capabilities = %+v", descriptor.Capabilities)
	}
	if len(descriptor.Capabilities.AuthModes) != 1 || descriptor.Capabilities.AuthModes[0] != domain.AuthNone {
		t.Fatalf("auth modes = %v", descriptor.Capabilities.AuthModes)
	}
	if descriptor.DefaultPublish != domain.PublishShared {
		t.Fatalf("default publish = %q", descriptor.DefaultPublish)
	}
	if len(descriptor.Streams) != 1 || descriptor.Streams[0] != sqliteStream {
		t.Fatalf("streams = %v", descriptor.Streams)
	}
	keys := map[string]bool{}
	for _, field := range descriptor.ConfigFields {
		keys[field.Key] = true
	}
	if !keys["path"] || !keys["includesystem"] {
		t.Fatalf("config fields = %+v", descriptor.ConfigFields)
	}
	if len(descriptor.Backends) != 1 || descriptor.Backends[0] != domain.BackendNative {
		t.Fatalf("backends = %v", descriptor.Backends)
	}
}

func TestSQLiteNewSourceValidation(t *testing.T) {
	path := createSQLiteFixture(t)
	dir := t.TempDir()
	cases := []struct {
		name     string
		settings map[string]string
	}{
		{"missing path", map[string]string{}},
		{"relative path", map[string]string{"path": "relative.db"}},
		{"UNC path", map[string]string{"path": `\\server\share\db.sqlite`}},
		{"file URI", map[string]string{"path": "file:" + filepath.ToSlash(path)}},
		{"query character", map[string]string{"path": path + "?mode=ro"}},
		{"directory", map[string]string{"path": dir}},
		{"missing file", map[string]string{"path": filepath.Join(t.TempDir(), "missing.db")}},
		{"bad includesystem", map[string]string{"path": path, "includesystem": "sometimes"}},
	}
	for _, testCase := range cases {
		conn := sqliteTestConnection(path, nil)
		conn.Settings = testCase.settings
		if _, err := NewSQLiteSource(conn, nil); err == nil {
			t.Errorf("%s: NewSQLiteSource succeeded, want an error", testCase.name)
		}
	}
}

func TestSQLiteReadSnapshot(t *testing.T) {
	path := createSQLiteFixture(t)
	conn := sqliteTestConnection(path, nil)
	source := mustSQLiteSource(t, conn)
	batch, checkpoints := readSQLiteAll(t, source, conn, nil, 0, nil)
	if batch.More {
		t.Fatal("More = true after a complete read")
	}
	items := itemsByExternalID(batch.Items)
	required := []string{
		"sqlite-schema/main/database",
		"sqlite-schema/main/table:users",
		"sqlite-schema/main/table:posts",
		"sqlite-schema/main/table:schema_migrations",
		"sqlite-schema/main/index:idx_posts_title",
		"sqlite-schema/main/view:user_posts",
		"sqlite-schema/main/trigger:trg_posts_after",
		"sqlite-schema/main/migrations",
	}
	for _, externalID := range required {
		if _, ok := items[externalID]; !ok {
			t.Errorf("missing item %q", externalID)
		}
	}
	if _, ok := items["sqlite-schema/main/table:sqlite_sequence"]; ok {
		t.Fatal("system table surfaced without includesystem")
	}
	posts := items["sqlite-schema/main/table:posts"].Body
	if !strings.Contains(posts, "user_id") || !strings.Contains(posts, "REFERENCES") && !strings.Contains(posts, "Foreign keys") {
		t.Errorf("posts body lacks columns/FK: %q", posts)
	}
	if !strings.Contains(posts, "ON DELETE CASCADE") {
		t.Errorf("posts body lost ON DELETE CASCADE: %q", posts)
	}
	indexBody := items["sqlite-schema/main/index:idx_posts_title"].Body
	if !strings.Contains(indexBody, "idx_posts_title") || !strings.Contains(indexBody, "title") {
		t.Errorf("index body incomplete: %q", indexBody)
	}
	triggerBody := items["sqlite-schema/main/trigger:trg_posts_after"].Body
	if !strings.Contains(triggerBody, "AFTER INSERT") {
		t.Errorf("trigger body incomplete: %q", triggerBody)
	}
	migrations := items["sqlite-schema/main/migrations"].Body
	if !strings.Contains(migrations, "20260101") || !strings.Contains(migrations, "20260102") {
		t.Errorf("migration ledger incomplete: %q", migrations)
	}
	overview := items["sqlite-schema/main/database"].Body
	if !strings.Contains(overview, "PRAGMA user_version: 7") || !strings.Contains(overview, "Schema digest:") {
		t.Errorf("overview incomplete: %q", overview)
	}
	// SQLite has no comment metadata, but it keeps SQL comments in the stored
	// CREATE text; those comments must survive verbatim.
	commented, ok := items["sqlite-schema/main/table:commented"]
	if !ok {
		t.Fatal("missing commented table")
	}
	if !strings.Contains(commented.Body, "-- audit: comment-marker") ||
		!strings.Contains(commented.Body, "/* inline comment-marker */") {
		t.Errorf("stored SQL comments were mangled: %q", commented.Body)
	}
	checkpoint := decodeSQLiteCheckpoint(t, ports.ReadBatch{Checkpoints: batch.Checkpoints})
	if checkpoint.Hash == "" || !strings.HasPrefix(checkpoint.Hash, domain.ItemHashPrefix) {
		t.Fatalf("checkpoint hash = %q", checkpoint.Hash)
	}
	if checkpoint.Watermark == "" {
		t.Fatal("checkpoint watermark is empty")
	}
	if err := checkpoints[sqliteStream].Validate(); err != nil {
		t.Fatalf("stored checkpoint invalid: %v", err)
	}
}

func TestSQLiteDigestStabilityAndChange(t *testing.T) {
	path := createSQLiteFixture(t)
	conn := sqliteTestConnection(path, nil)
	source := mustSQLiteSource(t, conn)
	first, checkpoints := readSQLiteAll(t, source, conn, nil, 0, nil)
	firstCheckpoint := decodeSQLiteCheckpoint(t, first)

	// Unchanged schema: no items, More=false, same digest.
	unchanged, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints})
	if err != nil {
		t.Fatalf("unchanged Read: %v", err)
	}
	if len(unchanged.Items) != 0 || unchanged.More {
		t.Fatalf("unchanged schema returned %d items, More=%v", len(unchanged.Items), unchanged.More)
	}
	unchangedCheckpoint := decodeSQLiteCheckpoint(t, unchanged)
	if unchangedCheckpoint.Hash != firstCheckpoint.Hash {
		t.Fatalf("unchanged hash moved: %q vs %q", unchangedCheckpoint.Hash, firstCheckpoint.Hash)
	}

	// req.Since forces a snapshot even when the digest is unchanged.
	now := time.Now().UTC()
	forced, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints, Since: &now})
	if err != nil {
		t.Fatalf("forced Read: %v", err)
	}
	if len(forced.Items) == 0 {
		t.Fatal("req.Since did not force a snapshot")
	}

	// A DDL change produces a new digest and items.
	writer, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	if _, err := writer.Exec(`ALTER TABLE users ADD COLUMN created_at TEXT`); err != nil {
		writer.Close()
		t.Fatalf("alter: %v", err)
	}
	writer.Close()
	changed, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints, Limit: 5})
	if err != nil {
		t.Fatalf("changed Read: %v", err)
	}
	if len(changed.Items) == 0 || !changed.More || len(changed.Checkpoints) != 1 {
		t.Fatalf("changed schema returned %d items, More=%v", len(changed.Items), changed.More)
	}
	changedCheckpoint := decodeSQLiteCheckpoint(t, changed)
	if changedCheckpoint.Hash == firstCheckpoint.Hash {
		t.Fatal("schema digest did not change after DDL")
	}
	if changedCheckpoint.Offset == 0 {
		t.Fatal("paged changed snapshot must carry a resumable offset")
	}
}

func TestSQLitePagingMatchesUnpaged(t *testing.T) {
	path := createSQLiteFixture(t)
	conn := sqliteTestConnection(path, nil)
	source := mustSQLiteSource(t, conn)
	baseline, _ := readSQLiteAll(t, source, conn, nil, 0, nil)
	paged, _ := readSQLiteAll(t, source, conn, nil, 2, nil)
	if len(paged.Items) != len(baseline.Items) {
		t.Fatalf("paged read returned %d items, baseline %d", len(paged.Items), len(baseline.Items))
	}
	baselineItems := itemsByExternalID(baseline.Items)
	for _, item := range paged.Items {
		other, ok := baselineItems[item.Ref.ExternalID]
		if !ok {
			t.Fatalf("paged item %q missing from baseline", item.Ref.ExternalID)
		}
		if item.ComputeHash() != other.ComputeHash() {
			t.Fatalf("paged item %q diverged from baseline", item.Ref.ExternalID)
		}
	}
}

func TestSQLiteReadOnlyEnforcement(t *testing.T) {
	path := createSQLiteFixture(t)
	ctx := context.Background()
	db, err := openSQLiteReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("openSQLiteReadOnly: %v", err)
	}
	defer func() { _ = db.Close() }()
	var queryOnly int
	if err := db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		t.Fatalf("query_only: %v", err)
	}
	if queryOnly != 1 {
		t.Fatalf("query_only = %d, want 1", queryOnly)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO canary (secret) VALUES ('written')"); err == nil {
		t.Fatal("INSERT succeeded on a read-only connection")
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE should_not_exist (x INTEGER)"); err == nil {
		t.Fatal("CREATE TABLE succeeded on a read-only connection")
	}
}

func TestSQLiteNoRowsRead(t *testing.T) {
	path := createSQLiteFixture(t)
	conn := sqliteTestConnection(path, nil)
	source := mustSQLiteSource(t, conn)
	batch, checkpoints := readSQLiteAll(t, source, conn, nil, 0, nil)
	for _, item := range batch.Items {
		if strings.Contains(item.Title, sqliteCanarySecret) || strings.Contains(item.Body, sqliteCanarySecret) {
			t.Fatalf("canary row value leaked into item %q", item.Ref.ExternalID)
		}
	}
	for stream, checkpoint := range checkpoints {
		if strings.Contains(string(checkpoint.Data), sqliteCanarySecret) {
			t.Fatalf("canary row value leaked into checkpoint %q", stream)
		}
	}
	if err := batch.Items[0].Validate(); err != nil {
		t.Fatalf("item invalid: %v", err)
	}
}

func TestSQLiteIncludeSystem(t *testing.T) {
	path := createSQLiteFixture(t)
	conn := sqliteTestConnection(path, map[string]string{"includesystem": "true"})
	source := mustSQLiteSource(t, conn)
	batch, _ := readSQLiteAll(t, source, conn, nil, 0, nil)
	items := itemsByExternalID(batch.Items)
	if _, ok := items["sqlite-schema/main/table:sqlite_sequence"]; !ok {
		t.Fatal("includesystem did not include sqlite_sequence")
	}
}

func TestSQLiteCheckpointMalformed(t *testing.T) {
	path := createSQLiteFixture(t)
	conn := sqliteTestConnection(path, nil)
	source := mustSQLiteSource(t, conn)
	cases := []domain.Checkpoint{
		{Stream: sqliteStream, Version: 99, Data: []byte(`{"hash":"","watermark":""}`)},
		{Stream: "other", Version: checkpointVersion, Data: []byte(`{"hash":"","watermark":""}`)},
		{Stream: sqliteStream, Version: checkpointVersion, Data: []byte(`not json`)},
		{Stream: sqliteStream, Version: checkpointVersion, Data: []byte(`{"hash":"sha256:zz","watermark":""}`)},
		{Stream: sqliteStream, Version: checkpointVersion, Data: []byte(`{"hash":"","watermark":"","offset":-1}`)},
		{Stream: sqliteStream, Version: checkpointVersion, Data: []byte(`{"hash":"","watermark":"","offset":3}`)},
	}
	for i, checkpoint := range cases {
		_, err := source.Read(context.Background(), ports.ReadRequest{
			Connection: conn, Checkpoints: map[string]domain.Checkpoint{sqliteStream: checkpoint},
		})
		if err == nil {
			t.Errorf("case %d: malformed checkpoint accepted", i)
		}
	}
}

func TestSQLiteBounds(t *testing.T) {
	long := strings.Repeat("x", domain.MaxItemBodyBytes+4096)
	bounded := boundedBody(long)
	if len(bounded) > domain.MaxItemBodyBytes {
		t.Fatalf("bounded body is %d bytes, over the cap", len(bounded))
	}
	if !strings.Contains(bounded, "truncated") {
		t.Fatal("bounded body lacks the truncation note")
	}

	path := createSQLiteFixture(t)
	conn := sqliteTestConnection(path, nil)
	source := mustSQLiteSource(t, conn)
	batch, _ := readSQLiteAll(t, source, conn, nil, 0, nil)
	for _, item := range batch.Items {
		if len(item.Body) > domain.MaxItemBodyBytes {
			t.Fatalf("item %q body is %d bytes", item.Ref.ExternalID, len(item.Body))
		}
		if err := item.WithHash().Validate(); err != nil {
			t.Fatalf("item %q invalid: %v", item.Ref.ExternalID, err)
		}
	}
}

func TestSQLiteCheck(t *testing.T) {
	path := createSQLiteFixture(t)
	source := mustSQLiteSource(t, sqliteTestConnection(path, nil))
	if err := source.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := source.Check(context.Background()); err == nil {
		t.Fatal("Check on a removed database succeeded")
	}
}

func TestSourceContract(t *testing.T) {
	path := createSQLiteFixture(t)
	conn := sqliteTestConnection(path, nil)
	var mu sync.Mutex
	counter := 0
	nextName := func() string {
		mu.Lock()
		defer mu.Unlock()
		counter++
		return fmt.Sprintf("generated_%03d", counter)
	}
	addOne := func() {
		name := nextName()
		writer, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatalf("open writer: %v", err)
		}
		defer func() { _ = writer.Close() }()
		if _, err := writer.Exec("CREATE TABLE " + quoteIdent(name) + " (id INTEGER PRIMARY KEY, value TEXT)"); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	harness := ingestiontest.Harness{
		Connection: conn,
		PageSize:   3,
		NewSource: func(c domain.Connection) ports.Source {
			source, err := NewSQLiteSource(c, nil)
			if err != nil {
				t.Fatalf("NewSQLiteSource: %v", err)
			}
			return source
		},
		AddItem: addOne,
		UpdateItem: func() {
			writer, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)")
			if err != nil {
				t.Fatalf("open writer: %v", err)
			}
			defer func() { _ = writer.Close() }()
			name := fmt.Sprintf("contract_column_%d", counter+1)
			if _, err := writer.Exec("ALTER TABLE users ADD COLUMN " + quoteIdent(name) + " TEXT"); err != nil {
				t.Fatalf("alter users: %v", err)
			}
		},
		AddMany: func(n int) {
			for range n {
				addOne()
			}
		},
	}
	ingestiontest.RunSourceSuite(t, harness)
}
