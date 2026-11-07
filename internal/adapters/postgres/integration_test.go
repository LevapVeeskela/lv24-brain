package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// newTestStore builds an isolated PostgreSQL schema per test. Tests skip when
// TestDatabaseURLEnv is unset so the default `go test` run stays offline.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	databaseURL := os.Getenv(TestDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set; skipping PostgreSQL integration test", TestDatabaseURLEnv)
	}

	adminCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminPool, err := pgxpool.New(adminCtx, databaseURL)
	if err != nil {
		t.Fatal("open test admin pool failed")
	}
	if err := adminPool.Ping(adminCtx); err != nil {
		adminPool.Close()
		t.Fatalf("ping test database: %v", err)
	}

	schema := fmt.Sprintf("brain_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(adminCtx, "CREATE SCHEMA "+quotedSchema); err != nil {
		adminPool.Close()
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := adminPool.Exec(ctx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Logf("drop test schema %s: %v", schema, err)
		}
		adminPool.Close()
	})

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal("parse test database url failed")
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(adminCtx, cfg)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	store := New(pool)
	t.Cleanup(store.Close)

	if _, err := store.Migrate(adminCtx); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}
	return store
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func revisionNumber(t *testing.T, revision domain.SyncRevision) int64 {
	t.Helper()
	value, err := RevisionValue(revision)
	if err != nil {
		t.Fatalf("revision %+v: %v", revision, err)
	}
	return value
}

func mustProject(t *testing.T, ctx context.Context, store *Store, id string) ProjectRecord {
	t.Helper()
	record, _, err := store.CreateProject(ctx, domain.Project{
		ID: id, Name: "Project " + id, Kind: domain.ProjectKindApp,
	})
	if err != nil {
		t.Fatalf("create project %s: %v", id, err)
	}
	return record
}

func TestMigrateIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)

	result, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(result.Applied) != 0 {
		t.Fatalf("second Migrate applied %v, want none", result.Applied)
	}
	if result.Version != SchemaVersion {
		t.Fatalf("version = %d, want %d", result.Version, SchemaVersion)
	}
	migrations, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	var recorded int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM brain_schema_migrations`).Scan(&recorded); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if recorded != len(migrations) {
		t.Fatalf("recorded migrations = %d, embedded = %d", recorded, len(migrations))
	}

	var roleCount int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM roles`).Scan(&roleCount); err != nil {
		t.Fatalf("count roles: %v", err)
	}
	if roleCount != 3 {
		t.Fatalf("role count = %d, want 3", roleCount)
	}
	if _, err := store.Pool().Exec(ctx, `INSERT INTO roles (name) VALUES ('root')`); err == nil {
		t.Fatal("roles table accepted an invalid role")
	}

	var tableCount int
	err = store.Pool().QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema()
		  AND table_name IN (
		    'roles', 'users', 'projects', 'notes', 'decisions', 'attempts',
		    'handoffs', 'connector_runs', 'revisions', 'audit_events', 'project_state'
		  )`).Scan(&tableCount)
	if err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tableCount != 11 {
		t.Fatalf("table count = %d, want 11", tableCount)
	}
}

func TestMigrateDetectsChecksumDrift(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)

	if _, err := store.Pool().Exec(ctx, `UPDATE brain_schema_migrations SET checksum = 'drifted' WHERE version = 1`); err != nil {
		t.Fatalf("tamper checksum: %v", err)
	}
	if _, err := store.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Migrate drift error = %v, want checksum mismatch", err)
	}
}

func TestMigrateRejectsNewerSchema(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)

	if _, err := store.Pool().Exec(
		ctx,
		`INSERT INTO brain_schema_migrations (version, name, checksum) VALUES (99, 'future', 'future')`,
	); err != nil {
		t.Fatalf("insert future migration: %v", err)
	}
	if _, err := store.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "newer than this engine") {
		t.Fatalf("Migrate newer-schema error = %v", err)
	}
}

func TestUsersCRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := WithActor(testContext(t), "admin")

	record, revision, err := store.CreateUser(ctx, domain.User{ID: "user_1", Login: "alice"}, "test-hash-not-a-secret")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if record.Role != domain.RoleMember {
		t.Fatalf("role = %q, want member", record.Role)
	}
	if record.AuthSource != "local" {
		t.Fatalf("auth source = %q", record.AuthSource)
	}
	if record.PasswordHash != "test-hash-not-a-secret" {
		t.Fatalf("password hash = %q", record.PasswordHash)
	}
	if record.Revision == 0 || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() {
		t.Fatalf("record not fully populated: %+v", record)
	}
	if revisionNumber(t, revision) != record.Revision {
		t.Fatalf("returned revision does not match record revision")
	}

	stored, err := store.GetUser(ctx, "user_1")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if stored.Login != "alice" {
		t.Fatalf("login = %q", stored.Login)
	}
	byLogin, err := store.GetUserByLogin(ctx, "ALICE")
	if err != nil || byLogin.ID != "user_1" {
		t.Fatalf("GetUserByLogin = (%+v, %v)", byLogin, err)
	}

	if _, _, err := store.CreateUser(ctx, domain.User{ID: "user_2", Login: "Alice"}, ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate login error = %v, want conflict", err)
	}
	if _, _, err := store.CreateUser(ctx, domain.User{ID: "user_3", Login: "bob", Role: "root"}, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid role error = %v, want validation", err)
	}
	if _, _, err := store.CreateUser(ctx, domain.User{ID: "user_3", Login: "   "}, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("blank login error = %v, want validation", err)
	}

	oidcRevision, err := store.SetUserOIDC(ctx, "user_1", "https://id.example", "subject-1", record.Revision)
	if err != nil {
		t.Fatalf("SetUserOIDC: %v", err)
	}
	oidcValue := revisionNumber(t, oidcRevision)
	byOIDC, err := store.GetUserByOIDC(ctx, "https://id.example", "subject-1")
	if err != nil || byOIDC.ID != "user_1" {
		t.Fatalf("GetUserByOIDC = (%+v, %v)", byOIDC, err)
	}
	if _, err := store.SetUserOIDC(ctx, "user_1", "", "subject-2", oidcValue); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("partial OIDC pair error = %v, want validation", err)
	}

	_, _, err = store.UpdateUserRole(ctx, "user_1", domain.RoleAdmin, record.Revision)
	var conflict *RevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("stale role update error = %v, want RevisionConflictError", err)
	}
	if conflict.Current != oidcValue {
		t.Fatalf("conflict current = %d, want %d", conflict.Current, oidcValue)
	}
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatal("conflict must unwrap to domain.ErrConflict")
	}

	updated, roleRevision, err := store.UpdateUserRole(ctx, "user_1", domain.RoleAdmin, oidcValue)
	if err != nil {
		t.Fatalf("UpdateUserRole: %v", err)
	}
	if updated.Role != domain.RoleAdmin {
		t.Fatalf("role = %q", updated.Role)
	}
	roleValue := revisionNumber(t, roleRevision)

	passwordRevision, err := store.UpdateUserPasswordHash(ctx, "user_1", "rotated-hash", roleValue)
	if err != nil {
		t.Fatalf("UpdateUserPasswordHash: %v", err)
	}
	passwordValue := revisionNumber(t, passwordRevision)
	stored, err = store.GetUser(ctx, "user_1")
	if err != nil {
		t.Fatalf("GetUser after password: %v", err)
	}
	if stored.PasswordHash != "rotated-hash" {
		t.Fatalf("password hash = %q", stored.PasswordHash)
	}

	profile := stored.User
	profile.DisplayName = "Alice Example"
	profile.Role = domain.RoleViewer
	_, profileRevision, err := store.UpdateUser(ctx, profile, passwordValue)
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	profileValue := revisionNumber(t, profileRevision)

	users, err := store.ListUsers(ctx)
	if err != nil || len(users) != 1 {
		t.Fatalf("ListUsers = (%d, %v)", len(users), err)
	}
	roles, err := store.ListRoles(ctx)
	if err != nil || len(roles) != 3 {
		t.Fatalf("ListRoles = (%d, %v)", len(roles), err)
	}

	if _, err := store.DeleteUser(ctx, "user_1", passwordValue); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale delete error = %v, want conflict", err)
	}
	if _, err := store.DeleteUser(ctx, "user_1", profileValue); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := store.GetUser(ctx, "user_1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetUser after delete = %v, want not found", err)
	}
	if _, err := store.GetUserByLogin(ctx, "alice"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetUserByLogin after delete = %v, want not found", err)
	}
}

func TestProjectsOptimisticRevision(t *testing.T) {
	store := newTestStore(t)
	ctx := WithActor(testContext(t), "member")

	record, revision, err := store.CreateProject(ctx, domain.Project{ID: "p1", Name: "Alpha"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if record.Kind != domain.ProjectKindOther {
		t.Fatalf("kind = %q", record.Kind)
	}
	if revisionNumber(t, revision) != record.Revision {
		t.Fatal("revision mismatch")
	}

	project, err := store.GetProject(ctx, "p1")
	if err != nil || project.Name != "Alpha" {
		t.Fatalf("GetProject = (%+v, %v)", project, err)
	}

	record.Name = "Alpha 2"
	updated, updateRevision, err := store.UpdateProject(ctx, record.Project, record.Revision)
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	updateValue := revisionNumber(t, updateRevision)
	if updated.Name != "Alpha 2" || updated.Revision != updateValue {
		t.Fatalf("updated = %+v", updated)
	}

	_, _, err = store.UpdateProject(ctx, record.Project, record.Revision)
	var conflict *RevisionConflictError
	if !errors.As(err, &conflict) || conflict.Current != updateValue {
		t.Fatalf("stale update error = %v, want conflict at %d", err, updateValue)
	}

	projects, err := store.ListProjects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("ListProjects = (%d, %v)", len(projects), err)
	}
	records, err := store.ListProjectRecords(ctx)
	if err != nil || len(records) != 1 || records[0].Revision != updateValue {
		t.Fatalf("ListProjectRecords = (%+v, %v)", records, err)
	}

	record.Name = "Alpha 3"
	if err := store.PutProject(ctx, record.Project); err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	afterPut, err := store.GetProjectRecord(ctx, "p1")
	if err != nil || afterPut.Name != "Alpha 3" {
		t.Fatalf("GetProjectRecord after put = (%+v, %v)", afterPut, err)
	}
	if afterPut.Revision <= updateValue {
		t.Fatalf("PutProject revision %d did not advance past %d", afterPut.Revision, updateValue)
	}

	before := afterPut.Revision
	if _, _, err := store.CreateProject(ctx, domain.Project{ID: "p1", Name: "Duplicate"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate project error = %v, want conflict", err)
	}
	current, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision: %v", err)
	}
	if revisionNumber(t, current) != before {
		t.Fatalf("failed create advanced revision to %s, want %d", current.Value, before)
	}

	if _, _, err := store.CreateProject(ctx, domain.Project{ID: "p2", Name: "   "}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("blank name error = %v, want validation", err)
	}

	if _, err := store.DeleteProject(ctx, "p1", updateValue); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale delete error = %v, want conflict", err)
	}
	if _, err := store.DeleteProject(ctx, "p1", before); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := store.GetProject(ctx, "p1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetProject after delete = %v, want not found", err)
	}
}

func TestNotesCRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := WithActor(testContext(t), "member")
	project := mustProject(t, ctx, store, "p1")

	note, revision, err := store.CreateNote(ctx, domain.Note{
		ProjectID: project.ID, Title: "First", Body: "Body one", Tags: nil,
	})
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if note.ID == "" || note.Tags == nil {
		t.Fatalf("note not normalized: %+v", note)
	}
	if revisionNumber(t, revision) != note.Revision {
		t.Fatal("revision mismatch")
	}

	if _, err := store.AppendNote(ctx, domain.Note{ID: "note_port", ProjectID: project.ID, Body: "Body two"}); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	portNote, err := store.GetNote(ctx, "note_port")
	if err != nil || portNote.Body != "Body two" {
		t.Fatalf("GetNote(port) = (%+v, %v)", portNote, err)
	}

	notes, err := store.ListNotes(ctx, ListOptions{ProjectID: project.ID})
	if err != nil || len(notes) != 2 {
		t.Fatalf("ListNotes = (%d, %v)", len(notes), err)
	}
	other, err := store.ListNotes(ctx, ListOptions{ProjectID: "other"})
	if err != nil || len(other) != 0 {
		t.Fatalf("ListNotes(other) = (%d, %v)", len(other), err)
	}

	note.Title = "First edited"
	note.Body = "Body one edited"
	note.Tags = []string{"tag"}
	_, _, err = store.UpdateNote(ctx, note.Note, note.Revision-1)
	var conflict *RevisionConflictError
	if !errors.As(err, &conflict) || conflict.Current != note.Revision {
		t.Fatalf("stale note update error = %v", err)
	}

	updated, updateRevision, err := store.UpdateNote(ctx, note.Note, note.Revision)
	if err != nil {
		t.Fatalf("UpdateNote: %v", err)
	}
	updateValue := revisionNumber(t, updateRevision)
	if updated.Title != "First edited" || len(updated.Tags) != 1 {
		t.Fatalf("updated note = %+v", updated)
	}
	if !updated.CreatedAt.Equal(note.CreatedAt) {
		t.Fatalf("created_at changed: %v -> %v", note.CreatedAt, updated.CreatedAt)
	}

	before, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision: %v", err)
	}
	if _, _, err := store.CreateNote(ctx, domain.Note{ProjectID: project.ID, Body: "  "}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid note error = %v, want validation", err)
	}
	after, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision: %v", err)
	}
	if before.Value != after.Value {
		t.Fatalf("failed note create advanced revision %s -> %s", before.Value, after.Value)
	}

	if _, err := store.DeleteNote(ctx, note.ID, note.Revision); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale delete error = %v, want conflict", err)
	}
	if _, err := store.DeleteNote(ctx, note.ID, updateValue); err != nil {
		t.Fatalf("DeleteNote: %v", err)
	}
	if _, err := store.GetNote(ctx, note.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetNote after delete = %v, want not found", err)
	}
}

func TestDecisionsAttemptsHandoffs(t *testing.T) {
	store := newTestStore(t)
	ctx := WithActor(testContext(t), "member")
	project := mustProject(t, ctx, store, "p1")

	decision, revision, err := store.CreateDecision(ctx, domain.Decision{
		ProjectID: project.ID, Title: "Choose storage",
		Alternatives: []string{"postgres", "sqlite"},
	})
	if err != nil {
		t.Fatalf("CreateDecision: %v", err)
	}
	if decision.Status != domain.DecisionProposed {
		t.Fatalf("status = %q", decision.Status)
	}
	decisionValue := revisionNumber(t, revision)
	decision.Status = domain.DecisionAccepted
	decision.Context = "Server mode needs shared storage"
	updatedDecision, decisionRevision, err := store.UpdateDecision(ctx, decision.Decision, decisionValue)
	if err != nil {
		t.Fatalf("UpdateDecision: %v", err)
	}
	decisionValue = revisionNumber(t, decisionRevision)
	if updatedDecision.Status != domain.DecisionAccepted {
		t.Fatalf("status = %q", updatedDecision.Status)
	}
	if _, err := store.AppendDecision(ctx, domain.Decision{ID: "decision_port", ProjectID: project.ID, Title: "Port"}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	decisions, err := store.ListDecisions(ctx, ListOptions{ProjectID: project.ID})
	if err != nil || len(decisions) != 2 {
		t.Fatalf("ListDecisions = (%d, %v)", len(decisions), err)
	}
	if _, _, err := store.CreateDecision(ctx, domain.Decision{ProjectID: project.ID}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("blank decision title error = %v, want validation", err)
	}

	attempt, attemptRevision, err := store.CreateAttempt(ctx, domain.Attempt{
		ProjectID: project.ID, Title: "Try cache", Hypothesis: "Redis helps",
	})
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	attemptValue := revisionNumber(t, attemptRevision)
	attempt.Result = "faster"
	updatedAttempt, attemptRevision, err := store.UpdateAttempt(ctx, attempt.Attempt, attemptValue)
	if err != nil {
		t.Fatalf("UpdateAttempt: %v", err)
	}
	attemptValue = revisionNumber(t, attemptRevision)
	if updatedAttempt.Result != "faster" {
		t.Fatalf("attempt result = %q", updatedAttempt.Result)
	}
	if _, err := store.AppendAttempt(ctx, domain.Attempt{ID: "attempt_port", ProjectID: project.ID, Title: "Port"}); err != nil {
		t.Fatalf("AppendAttempt: %v", err)
	}
	attempts, err := store.ListAttempts(ctx, ListOptions{ProjectID: project.ID})
	if err != nil || len(attempts) != 2 {
		t.Fatalf("ListAttempts = (%d, %v)", len(attempts), err)
	}

	handoff, handoffRevision, err := store.CreateHandoff(ctx, domain.Handoff{
		ProjectID: project.ID, Summary: "Continue WP4", NextAction: "Wire HTTP",
	})
	if err != nil {
		t.Fatalf("CreateHandoff: %v", err)
	}
	handoffValue := revisionNumber(t, handoffRevision)
	if _, err := store.AppendHandoff(ctx, domain.Handoff{ID: "handoff_port", ProjectID: project.ID, Summary: "Port"}); err != nil {
		t.Fatalf("AppendHandoff: %v", err)
	}
	handoffs, err := store.ListHandoffs(ctx, ListOptions{ProjectID: project.ID})
	if err != nil || len(handoffs) != 2 {
		t.Fatalf("ListHandoffs = (%d, %v)", len(handoffs), err)
	}
	if _, _, err := store.CreateHandoff(ctx, domain.Handoff{ProjectID: project.ID}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("blank handoff summary error = %v, want validation", err)
	}

	if _, err := store.DeleteDecision(ctx, decision.ID, decisionValue); err != nil {
		t.Fatalf("DeleteDecision: %v", err)
	}
	if _, err := store.DeleteAttempt(ctx, attempt.ID, attemptValue); err != nil {
		t.Fatalf("DeleteAttempt: %v", err)
	}
	if _, err := store.DeleteHandoff(ctx, handoff.ID, handoffValue); err != nil {
		t.Fatalf("DeleteHandoff: %v", err)
	}
}

func TestStateLifecycleAndCascade(t *testing.T) {
	store := newTestStore(t)
	ctx := WithActor(testContext(t), "member")
	project := mustProject(t, ctx, store, "p1")
	if _, _, err := store.CreateNote(ctx, domain.Note{ID: "note_1", ProjectID: project.ID, Body: "cascade"}); err != nil {
		t.Fatalf("CreateNote: %v", err)
	}

	if _, err := store.GetState(ctx, project.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetState before put = %v, want not found", err)
	}

	freshUntil := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	state := domain.State{
		ProjectID: project.ID, Goal: "Ship WP4", Active: []string{"persistence"},
		NextAction: "write tests", FreshUntil: freshUntil,
	}
	revision, err := store.PutState(ctx, state)
	if err != nil {
		t.Fatalf("PutState: %v", err)
	}
	stateValue := revisionNumber(t, revision)
	stored, err := store.GetState(ctx, project.ID)
	if err != nil || stored.Goal != "Ship WP4" || len(stored.Active) != 1 {
		t.Fatalf("GetState = (%+v, %v)", stored, err)
	}
	if !stored.FreshUntil.Equal(freshUntil) {
		t.Fatalf("fresh until = %v, want %v", stored.FreshUntil, freshUntil)
	}

	state.Goal = "Ship WP4 and WP5"
	updated, stateRevision, err := store.PutStateAt(ctx, state, stateValue)
	if err != nil {
		t.Fatalf("PutStateAt: %v", err)
	}
	stateValue = revisionNumber(t, stateRevision)
	if updated.Goal != state.Goal {
		t.Fatalf("goal = %q", updated.Goal)
	}
	if _, _, err := store.PutStateAt(ctx, state, stateValue-1); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale state error = %v, want conflict", err)
	}

	if _, err := store.DeleteProject(ctx, project.ID, project.Revision); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := store.GetState(ctx, project.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetState after cascade = %v, want not found", err)
	}
	notes, err := store.ListNotes(ctx, ListOptions{ProjectID: project.ID})
	if err != nil || len(notes) != 0 {
		t.Fatalf("ListNotes after cascade = (%d, %v)", len(notes), err)
	}
}

func TestConnectorRuns(t *testing.T) {
	store := newTestStore(t)
	ctx := WithActor(testContext(t), "admin")

	run, revision, err := store.CreateConnectorRun(ctx, domain.ConnectorRun{Source: "github"})
	if err != nil {
		t.Fatalf("CreateConnectorRun: %v", err)
	}
	if run.Status != domain.ConnectorRunning || run.ID == "" || run.StartedAt.IsZero() {
		t.Fatalf("run = %+v", run)
	}
	if run.Counts == nil {
		t.Fatal("counts must be non-nil")
	}
	runValue := revisionNumber(t, revision)

	if _, _, err := store.FinishConnectorRun(ctx, run.ID, domain.ConnectorSuccess, nil, "", runValue+10); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale finish error = %v, want conflict", err)
	}

	finished, finishRevision, err := store.FinishConnectorRun(
		ctx, run.ID, domain.ConnectorSuccess, map[string]int{"repos": 4}, "", runValue,
	)
	if err != nil {
		t.Fatalf("FinishConnectorRun: %v", err)
	}
	finishValue := revisionNumber(t, finishRevision)
	if finished.Status != domain.ConnectorSuccess || finished.FinishedAt == nil {
		t.Fatalf("finished = %+v", finished)
	}
	if finished.Counts["repos"] != 4 {
		t.Fatalf("counts = %v", finished.Counts)
	}

	if _, _, err := store.CreateConnectorRun(ctx, domain.ConnectorRun{ID: "run_2", Source: "gitlab", Status: domain.ConnectorFailed}); err != nil {
		t.Fatalf("CreateConnectorRun second: %v", err)
	}
	runs, err := store.ListConnectorRuns(ctx, ConnectorRunFilter{Source: "github"})
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListConnectorRuns(github) = (%d, %v)", len(runs), err)
	}
	runs, err = store.ListConnectorRuns(ctx, ConnectorRunFilter{Status: domain.ConnectorFailed})
	if err != nil || len(runs) != 1 || runs[0].ID != "run_2" {
		t.Fatalf("ListConnectorRuns(failed) = (%+v, %v)", runs, err)
	}
	if _, err := store.ListConnectorRuns(ctx, ConnectorRunFilter{Status: "done"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid filter status error = %v, want validation", err)
	}
	if _, _, err := store.FinishConnectorRun(ctx, run.ID, "done", nil, "", finishValue); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid finish status error = %v, want validation", err)
	}

	if _, err := store.DeleteConnectorRun(ctx, run.ID, runValue); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale run delete error = %v, want conflict", err)
	}
	if _, err := store.DeleteConnectorRun(ctx, run.ID, finishValue); err != nil {
		t.Fatalf("DeleteConnectorRun: %v", err)
	}
	if _, err := store.GetConnectorRun(ctx, run.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetConnectorRun after delete = %v, want not found", err)
	}
}

func TestAuditAndRevisionTrail(t *testing.T) {
	store := newTestStore(t)
	ctx := WithActor(testContext(t), "auditor")

	project := mustProject(t, ctx, store, "p1")
	user, _, err := store.CreateUser(ctx, domain.User{ID: "user_1", Login: "audit-user"}, "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	note, _, err := store.CreateNote(ctx, domain.Note{ID: "note_1", ProjectID: project.ID, Body: "audited"})
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}

	current, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision: %v", err)
	}
	currentValue := revisionNumber(t, current)
	if currentValue < note.Revision {
		t.Fatalf("current revision %d < note revision %d", currentValue, note.Revision)
	}

	revisions, err := store.ListRevisions(ctx, RevisionFilter{EntityKind: "note", EntityID: note.ID})
	if err != nil || len(revisions) == 0 {
		t.Fatalf("ListRevisions(note) = (%d, %v)", len(revisions), err)
	}
	if revisions[0].Actor != "auditor" || revisions[0].EntityID != note.ID {
		t.Fatalf("revision = %+v", revisions[0])
	}
	single, err := store.GetRevision(ctx, revisions[0].ID)
	if err != nil || single.Kind != domain.SyncKindServer {
		t.Fatalf("GetRevision = (%+v, %v)", single, err)
	}

	events, err := store.ListAuditEvents(ctx, AuditFilter{Actor: "auditor", EntityKind: "note", EntityID: note.ID})
	if err != nil || len(events) != 1 {
		t.Fatalf("ListAuditEvents(note) = (%d, %v)", len(events), err)
	}
	if events[0].Action != "note.create" {
		t.Fatalf("action = %q", events[0].Action)
	}
	singleEvent, err := store.GetAuditEvent(ctx, events[0].ID)
	if err != nil || singleEvent.Revision != note.Revision {
		t.Fatalf("GetAuditEvent = (%+v, %v)", singleEvent, err)
	}

	userEvents, err := store.ListAuditEvents(ctx, AuditFilter{EntityKind: "user", EntityID: user.ID})
	if err != nil || len(userEvents) != 1 {
		t.Fatalf("ListAuditEvents(user) = (%d, %v)", len(userEvents), err)
	}
	if _, leaked := userEvents[0].Detail["passwordHash"]; leaked {
		t.Fatal("audit detail contains password hash")
	}
	if _, leaked := userEvents[0].Detail["password_hash"]; leaked {
		t.Fatal("audit detail contains password hash")
	}

	recorded, auditRevision, err := store.RecordAuditEvent(ctx, "backup.create", "backup", "b1", map[string]any{"size": 4})
	if err != nil {
		t.Fatalf("RecordAuditEvent: %v", err)
	}
	if recorded.Action != "backup.create" || recorded.Actor != "auditor" {
		t.Fatalf("recorded = %+v", recorded)
	}
	if revisionNumber(t, auditRevision) != recorded.Revision {
		t.Fatal("audit revision mismatch")
	}

	future := time.Now().Add(time.Hour)
	events, err = store.ListAuditEvents(ctx, AuditFilter{Since: &future})
	if err != nil || len(events) != 0 {
		t.Fatalf("future filter = (%d, %v)", len(events), err)
	}
}

func TestPortsStoreContract(t *testing.T) {
	store := newTestStore(t)
	ctx := WithActor(testContext(t), "cli")
	var portStore ports.Store = store

	project := domain.Project{ID: "p1", Name: "Ports"}
	if err := portStore.PutProject(ctx, project); err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	if _, err := portStore.GetProject(ctx, "p1"); err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	projects, err := portStore.ListProjects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("ListProjects = (%d, %v)", len(projects), err)
	}

	stateRevision, err := portStore.PutState(ctx, domain.State{ProjectID: "p1", Goal: "ports"})
	if err != nil {
		t.Fatalf("PutState: %v", err)
	}
	if _, err := portStore.GetState(ctx, "p1"); err != nil {
		t.Fatalf("GetState: %v", err)
	}

	noteRevision, err := portStore.AppendNote(ctx, domain.Note{ID: "note_1", ProjectID: "p1", Body: "ports"})
	if err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	decisionRevision, err := portStore.AppendDecision(ctx, domain.Decision{ID: "decision_1", ProjectID: "p1", Title: "ports"})
	if err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	attemptRevision, err := portStore.AppendAttempt(ctx, domain.Attempt{ID: "attempt_1", ProjectID: "p1", Title: "ports"})
	if err != nil {
		t.Fatalf("AppendAttempt: %v", err)
	}
	handoffRevision, err := portStore.AppendHandoff(ctx, domain.Handoff{ID: "handoff_1", ProjectID: "p1", Summary: "ports"})
	if err != nil {
		t.Fatalf("AppendHandoff: %v", err)
	}

	values := []int64{
		revisionNumber(t, stateRevision), revisionNumber(t, noteRevision),
		revisionNumber(t, decisionRevision), revisionNumber(t, attemptRevision),
		revisionNumber(t, handoffRevision),
	}
	current, err := portStore.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision: %v", err)
	}
	currentValue := revisionNumber(t, current)
	for _, value := range values {
		if value > currentValue {
			t.Fatalf("recorded revision %d exceeds current %d", value, currentValue)
		}
	}
	if currentValue != values[len(values)-1] {
		t.Fatalf("current revision = %d, want %d", currentValue, values[len(values)-1])
	}
}

func TestAtomicityAndForeignKeys(t *testing.T) {
	store := newTestStore(t)
	ctx := WithActor(testContext(t), "member")
	project := mustProject(t, ctx, store, "p1")

	baseline, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision: %v", err)
	}
	baselineValue := revisionNumber(t, baseline)

	if _, _, err := store.CreateNote(ctx, domain.Note{ProjectID: "missing", Body: "orphan"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("missing project error = %v, want validation", err)
	}
	after, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision: %v", err)
	}
	if revisionNumber(t, after) != baselineValue {
		t.Fatalf("failed FK write advanced revision from %d to %s", baselineValue, after.Value)
	}

	if _, _, err := store.CreateNote(ctx, domain.Note{ID: "note_1", ProjectID: project.ID, Body: "cascade"}); err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if _, _, err := store.CreateDecision(ctx, domain.Decision{ID: "decision_1", ProjectID: project.ID, Title: "cascade"}); err != nil {
		t.Fatalf("CreateDecision: %v", err)
	}
	if _, _, err := store.CreateAttempt(ctx, domain.Attempt{ID: "attempt_1", ProjectID: project.ID, Title: "cascade"}); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	if _, _, err := store.CreateHandoff(ctx, domain.Handoff{ID: "handoff_1", ProjectID: project.ID, Summary: "cascade"}); err != nil {
		t.Fatalf("CreateHandoff: %v", err)
	}

	if _, err := store.DeleteProject(ctx, project.ID, project.Revision); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	for table, countQuery := range map[string]string{
		"notes":     `SELECT count(*) FROM notes`,
		"decisions": `SELECT count(*) FROM decisions`,
		"attempts":  `SELECT count(*) FROM attempts`,
		"handoffs":  `SELECT count(*) FROM handoffs`,
	} {
		var count int
		if err := store.Pool().QueryRow(ctx, countQuery).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s count after cascade = %d", table, count)
		}
	}
}

func TestOpenRejectsInvalidURL(t *testing.T) {
	if _, err := Open(context.Background(), "", Options{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty URL error = %v, want validation", err)
	}
	_, err := Open(context.Background(), "not-a-valid-dsn:", Options{})
	if err == nil {
		t.Fatal("expected invalid URL error")
	}
	if strings.Contains(err.Error(), "not-a-valid-dsn") {
		t.Fatalf("error leaked database URL: %v", err)
	}
}

func TestAPITokensAndRefreshTokens(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	user, _, err := store.CreateUser(ctx, domain.User{ID: "user_tokens", Login: "token-user"}, "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	token, err := store.CreateAPIToken(ctx, APITokenRecord{
		UserID: user.ID, Name: "cli", Role: domain.RoleMember, Prefix: "brain_abc", TokenHash: "hash-a",
	})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if token.ID == "" || token.CreatedAt.IsZero() {
		t.Fatalf("token = %+v", token)
	}
	if _, err := store.CreateAPIToken(ctx, APITokenRecord{
		UserID: user.ID, Name: "dup", Role: domain.RoleMember, TokenHash: "hash-a",
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate hash error = %v, want conflict", err)
	}
	if _, err := store.CreateAPIToken(ctx, APITokenRecord{
		UserID: "missing", Name: "ghost", Role: domain.RoleMember, TokenHash: "hash-b",
	}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown owner error = %v, want not found", err)
	}

	found, err := store.FindAPITokenByHash(ctx, "hash-a", time.Now())
	if err != nil || found.LastUsedAt == nil {
		t.Fatalf("FindAPITokenByHash = (%+v, %v)", found, err)
	}
	tokens, err := store.ListAPITokens(ctx, user.ID)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("ListAPITokens = (%d, %v)", len(tokens), err)
	}
	if err := store.RevokeAPIToken(ctx, token.ID, time.Now()); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	if err := store.RevokeAPIToken(ctx, token.ID, time.Now()); err != nil {
		t.Fatalf("repeated RevokeAPIToken: %v", err)
	}
	if _, err := store.FindAPITokenByHash(ctx, "hash-a", time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("FindAPITokenByHash after revoke = %v", err)
	}

	now := time.Now().UTC()
	refresh := auth.RefreshRecord{
		SessionID: "sess_1", FamilyID: "fam_1", UserID: user.ID,
		TokenHash: "refresh-1", IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := store.CreateRefresh(ctx, refresh); err != nil {
		t.Fatalf("CreateRefresh: %v", err)
	}
	if err := store.CreateRefresh(ctx, refresh); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate CreateRefresh = %v", err)
	}
	consumed, err := store.ConsumeRefresh(ctx, "refresh-1", now)
	if err != nil || consumed.SessionID != "sess_1" {
		t.Fatalf("ConsumeRefresh = (%+v, %v)", consumed, err)
	}
	if _, err := store.ConsumeRefresh(ctx, "refresh-1", now); !errors.Is(err, auth.ErrRefreshReused) {
		t.Fatalf("reused ConsumeRefresh = %v", err)
	}

	second := auth.RefreshRecord{
		SessionID: "sess_1", FamilyID: "fam_1", UserID: user.ID,
		TokenHash: "refresh-2", IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := store.CreateRefresh(ctx, second); err != nil {
		t.Fatalf("CreateRefresh(second): %v", err)
	}
	if err := store.RevokeSession(ctx, "sess_1", now); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if _, err := store.ConsumeRefresh(ctx, "refresh-2", now); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("ConsumeRefresh after session revoke = %v", err)
	}

	expired := auth.RefreshRecord{
		SessionID: "sess_2", FamilyID: "fam_2", UserID: user.ID,
		TokenHash: "refresh-3", IssuedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour),
	}
	if err := store.CreateRefresh(ctx, expired); err != nil {
		t.Fatalf("CreateRefresh(expired): %v", err)
	}
	if _, err := store.ConsumeRefresh(ctx, "refresh-3", now); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("ConsumeRefresh expired = %v", err)
	}
	if _, err := store.ConsumeRefresh(ctx, "unknown", now); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("ConsumeRefresh unknown = %v", err)
	}
}

func TestCreateRefreshRejectsRevokedFamilyAndSession(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	user, _, err := store.CreateUser(ctx, domain.User{ID: "user_revoke", Login: "revoke-user"}, "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now().UTC()
	newRecord := func(hash, sessionID, familyID string) auth.RefreshRecord {
		return auth.RefreshRecord{
			SessionID: sessionID, FamilyID: familyID, UserID: user.ID,
			TokenHash: hash, IssuedAt: now, ExpiresAt: now.Add(time.Hour),
		}
	}

	// Normal rotation within a live family still succeeds.
	if err := store.CreateRefresh(ctx, newRecord("rotate-1", "sess_rotate", "fam_rotate")); err != nil {
		t.Fatalf("CreateRefresh(first): %v", err)
	}
	if err := store.CreateRefresh(ctx, newRecord("rotate-2", "sess_rotate", "fam_rotate")); err != nil {
		t.Fatalf("CreateRefresh(rotation): %v", err)
	}

	// A revoked family is a durable marker: no new rotation may be issued.
	if err := store.CreateRefresh(ctx, newRecord("family-first", "sess_family", "fam_revoked")); err != nil {
		t.Fatalf("CreateRefresh(family): %v", err)
	}
	if err := store.RevokeFamily(ctx, "fam_revoked", now); err != nil {
		t.Fatalf("RevokeFamily: %v", err)
	}
	if err := store.CreateRefresh(ctx, newRecord("family-second", "sess_family", "fam_revoked")); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("CreateRefresh after RevokeFamily = %v, want ErrInvalidRefresh", err)
	}

	// A revoked session is likewise durable.
	if err := store.CreateRefresh(ctx, newRecord("session-first", "sess_revoked", "fam_session")); err != nil {
		t.Fatalf("CreateRefresh(session): %v", err)
	}
	if err := store.RevokeSession(ctx, "sess_revoked", now); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if err := store.CreateRefresh(ctx, newRecord("session-second", "sess_revoked", "fam_session")); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("CreateRefresh after RevokeSession = %v, want ErrInvalidRefresh", err)
	}

	// A different live session of the same user is unaffected.
	if err := store.CreateRefresh(ctx, newRecord("other-session", "sess_other", "fam_other")); err != nil {
		t.Fatalf("CreateRefresh(other session): %v", err)
	}
}

// insertExpiredRefreshRow writes a token that is already expired according to
// the database clock, independent of any client clock skew.
func insertExpiredRefreshRow(t *testing.T, ctx context.Context, store *Store, hash, sessionID, familyID, userID string) {
	t.Helper()
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO refresh_tokens (token_hash, session_id, family_id, user_id, issued_at, expires_at)
		VALUES ($1, $2, $3, $4, now() - interval '2 hours', now() - interval '1 hour')`,
		hash, sessionID, familyID, userID); err != nil {
		t.Fatalf("insert expired refresh row %s: %v", hash, err)
	}
}

func TestCreateRefreshPrunesOnlyItsOwnScope(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	user, _, err := store.CreateUser(ctx, domain.User{ID: "user_prune", Login: "prune-user"}, "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now().UTC()

	insertExpiredRefreshRow(t, ctx, store, "prune-stale", "sess_prune", "fam_prune", user.ID)
	insertExpiredRefreshRow(t, ctx, store, "keep-stale", "sess_keep", "fam_keep", user.ID)

	// Rotating in one scope still succeeds and prunes that scope's expired row.
	if err := store.CreateRefresh(ctx, auth.RefreshRecord{
		SessionID: "sess_prune", FamilyID: "fam_prune", UserID: user.ID,
		TokenHash: "prune-fresh", IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateRefresh: %v", err)
	}
	if _, err := store.ConsumeRefresh(ctx, "prune-fresh", now); err != nil {
		t.Fatalf("ConsumeRefresh(rotated): %v", err)
	}

	countRows := func(hash string) int {
		t.Helper()
		var count int
		if err := store.Pool().QueryRow(ctx,
			`SELECT count(*) FROM refresh_tokens WHERE token_hash = $1`, hash).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", hash, err)
		}
		return count
	}
	if count := countRows("prune-stale"); count != 0 {
		t.Fatalf("expired row in the rotated scope survived: count = %d", count)
	}
	if count := countRows("keep-stale"); count != 1 {
		t.Fatalf("expired row in an unrelated scope was pruned: count = %d", count)
	}
}

func TestCreateRefreshDoesNotWaitOnUnrelatedRevocation(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	user, _, err := store.CreateUser(ctx, domain.User{ID: "user_contention", Login: "contention-user"}, "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now().UTC()
	insertExpiredRefreshRow(t, ctx, store, "held-expired", "sess_held", "fam_held", user.ID)

	// Connection A models an in-flight RevokeFamily for an unrelated scope: it
	// holds the family advisory lock and the row lock on an expired row.
	conn, err := store.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection A: %v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin connection A: %v", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "fam_held"); err != nil {
		t.Fatalf("lock unrelated family: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE family_id = $1`, "fam_held"); err != nil {
		t.Fatalf("revoke unrelated family: %v", err)
	}

	// A global prune would block on connection A's row lock. The scoped prune
	// must complete while the unrelated transaction is still open.
	rotationCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- store.CreateRefresh(rotationCtx, auth.RefreshRecord{
			SessionID: "sess_free", FamilyID: "fam_free", UserID: user.ID,
			TokenHash: "free-rotation", IssuedAt: now, ExpiresAt: now.Add(time.Hour),
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("CreateRefresh blocked by an unrelated revocation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CreateRefresh blocked on an unrelated revoked scope")
	}
}

func TestRevokeScopeIDsAreTrimmed(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	user, _, err := store.CreateUser(ctx, domain.User{ID: "user_trim", Login: "trim-user"}, "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now().UTC()
	if err := store.CreateRefresh(ctx, auth.RefreshRecord{
		SessionID: "sess_trim", FamilyID: "fam_trim", UserID: user.ID,
		TokenHash: "trim-token", IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateRefresh: %v", err)
	}

	if err := store.RevokeFamily(ctx, "  fam_trim\t", now); err != nil {
		t.Fatalf("RevokeFamily: %v", err)
	}
	if err := store.RevokeSession(ctx, "\tsess_trim \n", now); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}

	for _, want := range []struct{ kind, id string }{
		{"family", "fam_trim"},
		{"session", "sess_trim"},
	} {
		var storedID string
		if err := store.Pool().QueryRow(ctx,
			`SELECT scope_id FROM auth_revocations WHERE scope_kind = $1`, want.kind).Scan(&storedID); err != nil {
			t.Fatalf("read %s tombstone: %v", want.kind, err)
		}
		if storedID != want.id {
			t.Fatalf("%s tombstone scope_id = %q, want %q", want.kind, storedID, want.id)
		}
	}

	var revoked bool
	if err := store.Pool().QueryRow(ctx,
		`SELECT revoked_at IS NOT NULL FROM refresh_tokens WHERE token_hash = 'trim-token'`).Scan(&revoked); err != nil {
		t.Fatalf("read trimmed token: %v", err)
	}
	if !revoked {
		t.Fatal("token for the trimmed family was not revoked")
	}
}

// insertRefreshRow writes a token directly, bypassing CreateRefresh, to model a
// rotation that slipped through before the revocation serialization was added.
func insertRefreshRow(t *testing.T, ctx context.Context, store *Store, hash, sessionID, familyID, userID string, expiresAt time.Time) {
	t.Helper()
	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO refresh_tokens (token_hash, session_id, family_id, user_id, issued_at, expires_at)
		VALUES ($1, $2, $3, $4, now() - interval '1 hour', $5)`, hash, sessionID, familyID, userID, expiresAt); err != nil {
		t.Fatalf("insert refresh row %s: %v", hash, err)
	}
}

func TestRefreshRevocationsOutliveTokenRows(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	user, _, err := store.CreateUser(ctx, domain.User{ID: "user_tombstone", Login: "tombstone-user"}, "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now().UTC()
	record := func(hash, sessionID, familyID string) auth.RefreshRecord {
		return auth.RefreshRecord{
			SessionID: sessionID, FamilyID: familyID, UserID: user.ID,
			TokenHash: hash, IssuedAt: now, ExpiresAt: now.Add(time.Hour),
		}
	}

	// A family tombstone survives pruning of every marker row.
	if err := store.CreateRefresh(ctx, record("family-marker", "sess_marker", "fam_marker")); err != nil {
		t.Fatalf("CreateRefresh(family marker): %v", err)
	}
	if err := store.RevokeFamily(ctx, "fam_marker", now); err != nil {
		t.Fatalf("RevokeFamily: %v", err)
	}
	if err := store.RevokeFamily(ctx, "fam_marker", now); err != nil {
		t.Fatalf("repeated RevokeFamily: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `DELETE FROM refresh_tokens WHERE family_id = $1`, "fam_marker"); err != nil {
		t.Fatalf("prune family marker rows: %v", err)
	}
	if err := store.CreateRefresh(ctx, record("family-after", "sess_marker", "fam_marker")); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("CreateRefresh after pruned RevokeFamily = %v, want ErrInvalidRefresh", err)
	}
	insertRefreshRow(t, ctx, store, "family-slipped", "sess_marker", "fam_marker", user.ID, now.Add(time.Hour))
	if _, err := store.ConsumeRefresh(ctx, "family-slipped", now); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("ConsumeRefresh of slipped family token = %v, want ErrInvalidRefresh", err)
	}

	// A session tombstone has the same guarantees, including for a different
	// family issued to the same revoked session.
	if err := store.CreateRefresh(ctx, record("session-marker", "sess_marker_revoked", "fam_session_marker")); err != nil {
		t.Fatalf("CreateRefresh(session marker): %v", err)
	}
	if err := store.RevokeSession(ctx, "sess_marker_revoked", now); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `DELETE FROM refresh_tokens WHERE session_id = $1`, "sess_marker_revoked"); err != nil {
		t.Fatalf("prune session marker rows: %v", err)
	}
	if err := store.CreateRefresh(ctx, record("session-after", "sess_marker_revoked", "fam_new_for_session")); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("CreateRefresh after pruned RevokeSession = %v, want ErrInvalidRefresh", err)
	}
	insertRefreshRow(t, ctx, store, "session-slipped", "sess_marker_revoked", "fam_new_for_session", user.ID, now.Add(time.Hour))
	if _, err := store.ConsumeRefresh(ctx, "session-slipped", now); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("ConsumeRefresh of slipped session token = %v, want ErrInvalidRefresh", err)
	}

	var tombstones int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM auth_revocations`).Scan(&tombstones); err != nil {
		t.Fatalf("count revocation tombstones: %v", err)
	}
	if tombstones != 2 {
		t.Fatalf("revocation tombstones = %d, want 2", tombstones)
	}

	// Expiry is decided by the database clock, not the caller's now value.
	insertRefreshRow(t, ctx, store, "clock-skew", "sess_clock", "fam_clock", user.ID, time.Now().UTC().Add(-time.Minute))
	if _, err := store.ConsumeRefresh(ctx, "clock-skew", time.Now().UTC().Add(-time.Hour)); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("ConsumeRefresh with stale caller clock = %v, want ErrInvalidRefresh", err)
	}

	// Normal rotation in a live family still works.
	if err := store.CreateRefresh(ctx, record("rotate-live-1", "sess_live", "fam_live")); err != nil {
		t.Fatalf("CreateRefresh(live first): %v", err)
	}
	if _, err := store.ConsumeRefresh(ctx, "rotate-live-1", now); err != nil {
		t.Fatalf("ConsumeRefresh(live first): %v", err)
	}
	if err := store.CreateRefresh(ctx, record("rotate-live-2", "sess_live", "fam_live")); err != nil {
		t.Fatalf("CreateRefresh(live rotation): %v", err)
	}
	if _, err := store.ConsumeRefresh(ctx, "rotate-live-2", now); err != nil {
		t.Fatalf("ConsumeRefresh(live rotation): %v", err)
	}
}

func waitForRefreshLockWaiter(t *testing.T, ctx context.Context, store *Store, scopeID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		err := store.Pool().QueryRow(ctx, `
			SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory'
			  AND NOT granted
			  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			  AND classid::bigint = ((hashtextextended($1, 0) >> 32) & 4294967295)
			  AND objid::bigint = (hashtextextended($1, 0) & 4294967295)`,
			scopeID).Scan(&waiting)
		if err != nil {
			t.Fatalf("inspect pg_locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("RevokeFamily did not wait on the advisory lock for %q", scopeID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRevokeFamilySerializesWithInFlightCreate(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	user, _, err := store.CreateUser(ctx, domain.User{ID: "user_race", Login: "race-user"}, "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now().UTC()
	const familyID = "fam_race"
	const sessionID = "sess_race"
	if err := store.CreateRefresh(ctx, auth.RefreshRecord{
		SessionID: sessionID, FamilyID: familyID, UserID: user.ID,
		TokenHash: "race-initial", IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateRefresh(initial): %v", err)
	}

	// Connection A models an in-flight rotation: it holds the family advisory
	// lock and inserts a token inside an uncommitted transaction.
	conn, err := store.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection A: %v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin connection A: %v", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, familyID); err != nil {
		t.Fatalf("lock family on connection A: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO refresh_tokens (token_hash, session_id, family_id, user_id, issued_at, expires_at)
		VALUES ('race-inflight', $1, $2, $3, now(), $4)`,
		sessionID, familyID, user.ID, now.Add(time.Hour)); err != nil {
		t.Fatalf("insert in-flight token: %v", err)
	}

	revoked := make(chan error, 1)
	go func() {
		revoked <- store.RevokeFamily(context.Background(), familyID, time.Now().UTC())
	}()

	// RevokeFamily must wait for the advisory lock before writing the tombstone.
	waitForRefreshLockWaiter(t, ctx, store, familyID)
	select {
	case err := <-revoked:
		t.Fatalf("RevokeFamily completed while the family lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit connection A: %v", err)
	}
	select {
	case err := <-revoked:
		if err != nil {
			t.Fatalf("RevokeFamily after lock release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RevokeFamily did not complete after the family lock was released")
	}

	var unrevoked int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM refresh_tokens WHERE family_id = $1 AND revoked_at IS NULL`, familyID).Scan(&unrevoked); err != nil {
		t.Fatalf("count unrevoked family tokens: %v", err)
	}
	if unrevoked != 0 {
		t.Fatalf("family has %d unrevoked tokens after RevokeFamily", unrevoked)
	}
	if _, err := store.ConsumeRefresh(ctx, "race-inflight", time.Now().UTC()); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("ConsumeRefresh of in-flight token after RevokeFamily = %v, want ErrInvalidRefresh", err)
	}
	if err := store.CreateRefresh(ctx, auth.RefreshRecord{
		SessionID: sessionID, FamilyID: familyID, UserID: user.ID,
		TokenHash: "race-successor", IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("CreateRefresh after racing RevokeFamily = %v, want ErrInvalidRefresh", err)
	}
}

func TestFindAPITokenIgnoresLastUsedUpdateFailure(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	user, _, err := store.CreateUser(ctx, domain.User{ID: "user_usage", Login: "usage-user"}, "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := store.CreateAPIToken(ctx, APITokenRecord{
		UserID: user.ID, Name: "cli", Role: domain.RoleMember, TokenHash: "hash-usage",
	}); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `
		CREATE FUNCTION reject_last_used() RETURNS trigger AS $$
		BEGIN
			IF NEW.last_used_at IS DISTINCT FROM OLD.last_used_at THEN
				RAISE EXCEPTION 'last_used_at rejected';
			END IF;
			RETURN NEW;
		END
		$$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	if _, err := store.Pool().Exec(ctx, `
		CREATE TRIGGER reject_last_used_update BEFORE UPDATE ON api_tokens
		FOR EACH ROW EXECUTE FUNCTION reject_last_used()`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	found, err := store.FindAPITokenByHash(ctx, "hash-usage", time.Now())
	if err != nil {
		t.Fatalf("FindAPITokenByHash with a failing usage update = %v", err)
	}
	if found.ID == "" || found.TokenHash != "hash-usage" {
		t.Fatalf("token = %+v", found)
	}
}

func TestSearchFullTextMigrationIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	migrations, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	var searchSQL string
	for _, migration := range migrations {
		if migration.Version == 7 {
			searchSQL = migration.SQL
		}
	}
	if searchSQL == "" {
		t.Fatal("migration 7 (full-text search) is missing")
	}

	// Running the file again must succeed: every statement is IF NOT EXISTS
	// or OR REPLACE, so a partially applied schema can be repaired safely.
	for run := 0; run < 2; run++ {
		if _, err := store.Pool().Exec(ctx, searchSQL); err != nil {
			t.Fatalf("re-run migration 7 (pass %d): %v", run+1, err)
		}
	}

	project := mustProject(t, ctx, store, "fts")
	if _, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID: project.ID, Title: "Индекс", Body: "generated vector",
	}); err != nil {
		t.Fatalf("create note: %v", err)
	}
	var matches bool
	if err := store.Pool().QueryRow(ctx,
		`SELECT search_tsv @@ websearch_to_tsquery('simple', 'индекс') FROM notes WHERE project_id = $1`,
		project.ID,
	).Scan(&matches); err != nil {
		t.Fatalf("query generated tsvector: %v", err)
	}
	if !matches {
		t.Fatal("generated search_tsv does not match the note title")
	}
}
