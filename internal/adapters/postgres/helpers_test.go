package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestLoadMigrations(t *testing.T) {
	migrations, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("expected at least one embedded migration")
	}
	if migrations[0].Version != 1 {
		t.Fatalf("first migration version = %d, want 1", migrations[0].Version)
	}
	if last := migrations[len(migrations)-1].Version; last != SchemaVersion {
		t.Fatalf("last migration version = %d, SchemaVersion = %d", last, SchemaVersion)
	}
	for i := 1; i < len(migrations); i++ {
		if migrations[i-1].Version >= migrations[i].Version {
			t.Fatalf("migrations are not strictly sorted at index %d", i)
		}
	}

	sql := migrations[0].SQL
	for _, table := range []string{
		"roles", "users", "projects", "notes", "decisions", "attempts",
		"handoffs", "connector_runs", "revisions", "audit_events",
	} {
		if !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS "+table) {
			t.Errorf("migration SQL is missing table %s", table)
		}
	}
	if len(migrations[0].Checksum) != 64 {
		t.Fatalf("checksum length = %d, want 64", len(migrations[0].Checksum))
	}

	var authSQL, revocationSQL string
	for _, migration := range migrations {
		switch migration.Version {
		case 2:
			authSQL = migration.SQL
		case 3:
			revocationSQL = migration.SQL
		}
	}
	if authSQL == "" {
		t.Fatal("migration 2 (auth tokens) is missing")
	}
	for _, table := range []string{"api_tokens", "refresh_tokens"} {
		if !strings.Contains(authSQL, "CREATE TABLE IF NOT EXISTS "+table) {
			t.Errorf("migration 2 SQL is missing table %s", table)
		}
	}
	if !strings.Contains(authSQL, "api_tokens_hash_uidx") {
		t.Error("migration 2 is missing the unique API-token hash index")
	}

	if revocationSQL == "" {
		t.Fatal("migration 3 (auth revocations) is missing")
	}
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS auth_revocations",
		"PRIMARY KEY (scope_kind, scope_id)",
		"scope_kind IN ('family', 'session')",
	} {
		if !strings.Contains(revocationSQL, fragment) {
			t.Errorf("migration 3 SQL is missing %q", fragment)
		}
	}
}

func TestParseMigrationName(t *testing.T) {
	tests := []struct {
		fileName string
		version  int
		name     string
		wantErr  bool
	}{
		{fileName: "0001_init.sql", version: 1, name: "init"},
		{fileName: "0042_more_stuff.sql", version: 42, name: "more_stuff"},
		{fileName: "1_init.sql", wantErr: true},
		{fileName: "0001_Init.sql", wantErr: true},
		{fileName: "0001_.sql", wantErr: true},
		{fileName: "0000_zero.sql", wantErr: true},
		{fileName: "0001_init.txt", wantErr: true},
		{fileName: "0001_init.sql.bak", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.fileName, func(t *testing.T) {
			version, name, err := parseMigrationName(test.fileName)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected error, got version=%d name=%q", version, name)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if version != test.version || name != test.name {
				t.Fatalf("got (%d, %q), want (%d, %q)", version, name, test.version, test.name)
			}
		})
	}
}

func TestRevisionValue(t *testing.T) {
	value, err := RevisionValue(domain.SyncRevision{Kind: domain.SyncKindServer, Value: "42"})
	if err != nil || value != 42 {
		t.Fatalf("RevisionValue = (%d, %v), want (42, nil)", value, err)
	}
	value, err = RevisionValue(domain.SyncRevision{Kind: domain.SyncKindServer, Value: "0"})
	if err != nil || value != 0 {
		t.Fatalf("RevisionValue zero = (%d, %v), want (0, nil)", value, err)
	}
	for _, revision := range []domain.SyncRevision{
		{Kind: domain.SyncKindGit, Value: "abc"},
		{Kind: domain.SyncKindServer, Value: "abc"},
		{Kind: domain.SyncKindServer, Value: "-1"},
		{Kind: domain.SyncKindServer, Value: ""},
		{Kind: "", Value: "1"},
	} {
		if _, err := RevisionValue(revision); err == nil {
			t.Errorf("RevisionValue(%+v) should fail", revision)
		}
	}

	at := time.Date(2026, 11, 5, 10, 0, 0, 0, time.FixedZone("x", 3600))
	sync := RevisionFromValue(7, at)
	if sync.Kind != domain.SyncKindServer || sync.Value != "7" {
		t.Fatalf("RevisionFromValue = %+v", sync)
	}
	if sync.At.Location() != time.UTC {
		t.Fatalf("revision time is not UTC: %v", sync.At)
	}
}

func TestJSONHelpers(t *testing.T) {
	raw, err := marshalJSON(nil)
	if err != nil || string(raw) != "{}" {
		t.Fatalf("marshalJSON(nil) = (%s, %v)", raw, err)
	}
	var nilCounts map[string]int
	raw, err = marshalJSON(nilCounts)
	if err != nil || string(raw) != "{}" {
		t.Fatalf("marshalJSON(nil map) = (%s, %v)", raw, err)
	}
	raw, err = marshalJSON(map[string]int{"items": 3})
	if err != nil {
		t.Fatalf("marshalJSON: %v", err)
	}
	counts, err := unmarshalJSON(raw, map[string]int{})
	if err != nil || counts["items"] != 3 {
		t.Fatalf("unmarshalJSON = (%v, %v)", counts, err)
	}
	empty, err := unmarshalJSON(nil, map[string]any{"fallback": true})
	if err != nil || empty["fallback"] != true {
		t.Fatalf("unmarshalJSON fallback = (%v, %v)", empty, err)
	}
	if _, err := unmarshalJSON([]byte("{"), map[string]int{}); err == nil {
		t.Fatal("expected decode error for malformed JSON")
	}
}

func TestNewID(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 100; i++ {
		id, err := newID("note")
		if err != nil {
			t.Fatalf("newID: %v", err)
		}
		if !strings.HasPrefix(id, "note_") {
			t.Fatalf("id %q lacks prefix", id)
		}
		if len(id) != len("note_")+24 {
			t.Fatalf("id %q has unexpected length %d", id, len(id))
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}
	bare, err := newID("")
	if err != nil || len(bare) != 24 {
		t.Fatalf("bare newID = (%q, %v)", bare, err)
	}
}

func TestClampLimitAndOffset(t *testing.T) {
	if got := clampLimit(0); got != defaultListLimit {
		t.Fatalf("clampLimit(0) = %d", got)
	}
	if got := clampLimit(-5); got != defaultListLimit {
		t.Fatalf("clampLimit(-5) = %d", got)
	}
	if got := clampLimit(10); got != 10 {
		t.Fatalf("clampLimit(10) = %d", got)
	}
	if got := clampLimit(maxListLimit + 1); got != maxListLimit {
		t.Fatalf("clampLimit(max+1) = %d", got)
	}
	if got := normalizeOffset(-1); got != 0 {
		t.Fatalf("normalizeOffset(-1) = %d", got)
	}
	if got := normalizeOffset(5); got != 5 {
		t.Fatalf("normalizeOffset(5) = %d", got)
	}
}

func TestActorContext(t *testing.T) {
	ctx := context.Background()
	if ActorFromContext(ctx) != "" {
		t.Fatal("empty context should have no actor")
	}
	ctx = WithActor(ctx, "admin")
	if ActorFromContext(ctx) != "admin" {
		t.Fatalf("actor = %q", ActorFromContext(ctx))
	}
	if got := ActorFromContext(WithActor(ctx, "")); got != "admin" {
		t.Fatalf("empty actor should keep previous value, got %q", got)
	}
}

func TestMapPgError(t *testing.T) {
	if err := mapPgError("op", nil); err != nil {
		t.Fatalf("nil error mapped to %v", err)
	}
	if err := mapPgError("get", pgx.ErrNoRows); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ErrNoRows mapped to %v", err)
	}

	unique := &pgconn.PgError{
		Code:           "23505",
		ConstraintName: "users_login_lower_uidx",
		Message:        "duplicate key value violates unique constraint (login)=(sensitive-value)",
	}
	err := mapPgError("create user", unique)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("unique violation mapped to %v", err)
	}
	if strings.Contains(err.Error(), "sensitive-value") {
		t.Fatalf("mapped error leaked PostgreSQL detail: %v", err)
	}
	if !strings.Contains(err.Error(), "users_login_lower_uidx") {
		t.Fatalf("mapped error lost constraint name: %v", err)
	}

	for _, code := range []string{"23503", "23514", "22P02"} {
		err := mapPgError("op", &pgconn.PgError{Code: code})
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("code %s mapped to %v, want validation", code, err)
		}
	}

	sentinel := errors.New("network down")
	if err := mapPgError("ping", sentinel); !errors.Is(err, sentinel) {
		t.Fatalf("generic error not preserved: %v", err)
	}
}

func TestRevisionConflictError(t *testing.T) {
	err := &RevisionConflictError{Entity: "notes", ID: "n1", Expected: 2, Current: 5}
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatal("RevisionConflictError must unwrap to ErrConflict")
	}
	if !strings.Contains(err.Error(), "expected 2, current 5") {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestValidateOIDCPair(t *testing.T) {
	if err := ValidateOIDCPair("", ""); err != nil {
		t.Fatalf("empty pair: %v", err)
	}
	if err := ValidateOIDCPair("https://id.example", "subject"); err != nil {
		t.Fatalf("complete pair: %v", err)
	}
	if err := ValidateOIDCPair("https://id.example", ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("issuer only: %v", err)
	}
	if err := ValidateOIDCPair("", "subject"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("subject only: %v", err)
	}
}

func TestNormalizeUser(t *testing.T) {
	user, err := normalizeNewUser(domain.User{ID: "u1", Login: "  alice  "})
	if err != nil {
		t.Fatalf("normalizeNewUser: %v", err)
	}
	if user.Login != "alice" {
		t.Fatalf("login = %q", user.Login)
	}
	if user.Role != domain.RoleMember {
		t.Fatalf("role = %q", user.Role)
	}
	if user.AuthSource != "local" {
		t.Fatalf("auth source = %q", user.AuthSource)
	}

	if _, err := normalizeUser(domain.User{ID: "u1", Login: "alice"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("update without role must fail: %v", err)
	}
	updated, err := normalizeUser(domain.User{
		ID: "u1", Login: "alice", Role: domain.RoleAdmin, AuthSource: "oidc",
	})
	if err != nil || updated.Role != domain.RoleAdmin || updated.AuthSource != "oidc" {
		t.Fatalf("normalizeUser update = (%+v, %v)", updated, err)
	}
	if _, err := normalizeUser(domain.User{ID: "u1", Login: "alice", Role: "root", AuthSource: "local"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid role: %v", err)
	}
	if _, err := normalizeUser(domain.User{ID: "u1", Login: "   ", Role: domain.RoleMember, AuthSource: "local"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("blank login: %v", err)
	}
	if _, err := normalizeUser(domain.User{Login: "alice", Role: domain.RoleMember, AuthSource: "local"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("missing id: %v", err)
	}
}

func TestNormalizeProject(t *testing.T) {
	project, err := normalizeProject(domain.Project{ID: "p1", Name: "Demo"})
	if err != nil {
		t.Fatalf("normalizeProject: %v", err)
	}
	if project.Kind != domain.ProjectKindOther {
		t.Fatalf("kind = %q", project.Kind)
	}
	if project.Paths == nil || project.Aliases == nil {
		t.Fatal("path slices must be non-nil")
	}
	if _, err := normalizeProject(domain.Project{ID: "p1", Name: "  "}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("blank name: %v", err)
	}
	if _, err := normalizeProject(domain.Project{ID: "p1", Name: "Demo", Kind: "alien"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid kind: %v", err)
	}
}

func TestRevisionKeyColumn(t *testing.T) {
	for table, want := range map[string]string{
		"users": "id", "projects": "id", "project_state": "project_id",
		"notes": "id", "decisions": "id", "attempts": "id", "handoffs": "id",
		"connector_runs": "id",
	} {
		got, err := revisionKeyColumn(table)
		if err != nil || got != want {
			t.Fatalf("revisionKeyColumn(%q) = (%q, %v), want %q", table, got, err, want)
		}
	}
	if _, err := revisionKeyColumn("sql_injection"); err == nil {
		t.Fatal("unknown table must be rejected")
	}
}

func TestKnowledgeListQuery(t *testing.T) {
	query, args := knowledgeListQuery("notes", noteColumns, "updated_at", ListOptions{})
	if strings.Contains(query, "WHERE") {
		t.Fatalf("unfiltered query has WHERE: %s", query)
	}
	if !strings.Contains(query, "ORDER BY updated_at DESC, id") {
		t.Fatalf("query order missing: %s", query)
	}
	if len(args) != 2 || args[0] != defaultListLimit || args[1] != 0 {
		t.Fatalf("args = %#v", args)
	}

	query, args = knowledgeListQuery("notes", noteColumns, "updated_at", ListOptions{ProjectID: "p1", Limit: 10, Offset: 5})
	if !strings.Contains(query, "WHERE project_id = $1") {
		t.Fatalf("project filter missing: %s", query)
	}
	if !strings.Contains(query, "LIMIT $2 OFFSET $3") {
		t.Fatalf("limit/offset placeholders missing: %s", query)
	}
	if len(args) != 3 || args[0] != "p1" || args[1] != 10 || args[2] != 5 {
		t.Fatalf("args = %#v", args)
	}

	_, args = knowledgeListQuery("notes", noteColumns, "updated_at", ListOptions{Limit: maxListLimit + 100, Offset: -1})
	if args[0] != maxListLimit || args[1] != 0 {
		t.Fatalf("clamped args = %#v", args)
	}
}

func TestOpenFromEnvRequiresURL(t *testing.T) {
	t.Setenv(DatabaseURLEnv, "")
	if _, err := OpenFromEnv(context.Background(), Options{}); err == nil {
		t.Fatal("OpenFromEnv must fail when the database URL is unset")
	}
}

func TestUserRecordDomainHidesPasswordHash(t *testing.T) {
	record := UserRecord{
		User:         domain.User{ID: "u1", Login: "alice", Role: domain.RoleMember},
		PasswordHash: "secret-hash",
		OIDCIssuer:   "https://id.example",
		OIDCSubject:  "subject",
		Revision:     3,
	}
	public := record.Domain()
	if public.ID != "u1" || public.Login != "alice" {
		t.Fatalf("domain projection = %+v", public)
	}
	if strings.Contains(public.ID, "secret-hash") {
		t.Fatal("domain projection leaked password hash")
	}

	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal user record: %v", err)
	}
	encoded := string(raw)
	for _, forbidden := range []string{"secret-hash", "id.example", "subject", "passwordHash", "oidc"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("user JSON leaked %q: %s", forbidden, encoded)
		}
	}
	if !strings.Contains(encoded, `"revision":3`) {
		t.Fatalf("user JSON is missing revision: %s", encoded)
	}
}
