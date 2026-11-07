package postgres

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// insertRevisionAt seeds a revision row with an explicit creation time.
func insertRevisionAt(t *testing.T, store *Store, at time.Time) int64 {
	t.Helper()
	ctx := testContext(t)
	var id int64
	if err := store.Pool().QueryRow(ctx,
		`INSERT INTO revisions (kind, actor, entity_kind, entity_id, detail, created_at)
		 VALUES ('server', 'test', 'test', 'seed', '{}', $1) RETURNING id`, at,
	).Scan(&id); err != nil {
		t.Fatalf("insert revision: %v", err)
	}
	return id
}

func countRows(t *testing.T, store *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := store.Pool().QueryRow(testContext(t), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func TestPruneRetainedDeletesOnlyExpiredRows(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	now := time.Now().UTC()
	old := now.Add(-200 * 24 * time.Hour)
	recent := now.Add(-24 * time.Hour)

	user, _, err := store.CreateUser(ctx, domain.User{
		Login: "retention-owner@example.test", Role: domain.RoleAdmin, AuthSource: "local",
	}, "hash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	project := mustProject(t, ctx, store, "retention-project")

	// Revisions: an orphan, one referenced only by an old audit event, one
	// referenced by a kept audit event, one that is an entity's current
	// revision, and a recent one.
	orphan := insertRevisionAt(t, store, now.Add(-120*24*time.Hour))
	onlyOldAudit := insertRevisionAt(t, store, old)
	keptByAudit := insertRevisionAt(t, store, now.Add(-100*24*time.Hour))
	current := insertRevisionAt(t, store, now.Add(-120*24*time.Hour))
	recentRevision := insertRevisionAt(t, store, recent)
	if _, err := store.Pool().Exec(ctx, `UPDATE projects SET revision = $1 WHERE id = $2`, current, project.ID); err != nil {
		t.Fatalf("point project at current revision: %v", err)
	}

	insertAudit := func(at time.Time, revision *int64, action string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx,
			`INSERT INTO audit_events (revision, actor, action, entity_kind, entity_id, detail, created_at)
			 VALUES ($1, 'test', $2, 'test', 'x', '{}', $3)`, revision, action, at); err != nil {
			t.Fatalf("insert audit %s: %v", action, err)
		}
	}
	insertAudit(old, nil, "test.old-unlinked")
	insertAudit(old, &onlyOldAudit, "test.old-linked")
	insertAudit(now.Add(-100*24*time.Hour), &keptByAudit, "test.kept-linked")
	insertAudit(recent, nil, "test.recent")

	insertRefresh := func(hash string, expires time.Time) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx,
			`INSERT INTO refresh_tokens (token_hash, session_id, family_id, user_id, issued_at, expires_at)
			 VALUES ($1, 'session-1', 'family-1', $2, $3, $4)`,
			hash, user.ID, expires.Add(-time.Hour), expires); err != nil {
			t.Fatalf("insert refresh %s: %v", hash, err)
		}
	}
	insertRefresh("expired-refresh", now.Add(-24*time.Hour))
	insertRefresh("live-refresh", now.Add(24*time.Hour))

	insertRevocation := func(kind, scope string, at time.Time) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx,
			`INSERT INTO auth_revocations (scope_kind, scope_id, revoked_at) VALUES ($1, $2, $3)`,
			kind, scope, at); err != nil {
			t.Fatalf("insert revocation %s: %v", scope, err)
		}
	}
	insertRevocation("family", "old-family", now.Add(-40*24*time.Hour))
	insertRevocation("session", "recent-session", now.Add(-5*24*time.Hour))

	insertAPIToken := func(id string, expires *time.Time) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx,
			`INSERT INTO api_tokens (id, user_id, name, prefix, token_hash, role, expires_at)
			 VALUES ($1, $2, $1, 'brain_', $1, 'member', $3)`, id, user.ID, expires); err != nil {
			t.Fatalf("insert api token %s: %v", id, err)
		}
	}
	longExpired := now.Add(-60 * 24 * time.Hour)
	recentlyExpired := now.Add(-10 * 24 * time.Hour)
	future := now.Add(30 * 24 * time.Hour)
	insertAPIToken("token-long-expired", &longExpired)
	insertAPIToken("token-recently-expired", &recentlyExpired)
	insertAPIToken("token-no-expiry", nil)
	insertAPIToken("token-future", &future)

	result, err := store.PruneRetained(ctx, RetentionPolicy{
		Audit:         now.Add(-180 * 24 * time.Hour),
		Revisions:     now.Add(-90 * 24 * time.Hour),
		Revocations:   now.Add(-30 * 24 * time.Hour),
		RefreshTokens: now,
		APITokens:     now.Add(-30 * 24 * time.Hour),
		// A batch of 2 forces several DELETE rounds in every sweep.
		BatchSize: 2,
	})
	if err != nil {
		t.Fatalf("PruneRetained: %v", err)
	}

	want := RetentionResult{AuditEvents: 2, Revisions: 2, Revocations: 1, RefreshTokens: 1, APITokens: 1}
	if result != want {
		t.Fatalf("result = %+v, want %+v", result, want)
	}
	for _, check := range []struct {
		name  string
		query string
		args  []any
		want  int
	}{
		{"audit kept: linked to kept revision", `SELECT count(*) FROM audit_events WHERE action = 'test.kept-linked'`, nil, 1},
		{"audit kept: recent", `SELECT count(*) FROM audit_events WHERE action = 'test.recent'`, nil, 1},
		{"audit deleted: old unlinked", `SELECT count(*) FROM audit_events WHERE action = 'test.old-unlinked'`, nil, 0},
		{"audit deleted: old linked", `SELECT count(*) FROM audit_events WHERE action = 'test.old-linked'`, nil, 0},
		{"revision deleted: orphan", `SELECT count(*) FROM revisions WHERE id = $1`, []any{orphan}, 0},
		{"revision deleted: released by deleted audit", `SELECT count(*) FROM revisions WHERE id = $1`, []any{onlyOldAudit}, 0},
		{"revision kept: referenced by kept audit", `SELECT count(*) FROM revisions WHERE id = $1`, []any{keptByAudit}, 1},
		{"revision kept: entity current revision", `SELECT count(*) FROM revisions WHERE id = $1`, []any{current}, 1},
		{"revision kept: recent", `SELECT count(*) FROM revisions WHERE id = $1`, []any{recentRevision}, 1},
		{"refresh kept: live", `SELECT count(*) FROM refresh_tokens WHERE token_hash = 'live-refresh'`, nil, 1},
		{"refresh deleted: expired", `SELECT count(*) FROM refresh_tokens WHERE token_hash = 'expired-refresh'`, nil, 0},
		{"revocation kept: recent", `SELECT count(*) FROM auth_revocations WHERE scope_id = 'recent-session'`, nil, 1},
		{"revocation deleted: beyond lifetime", `SELECT count(*) FROM auth_revocations WHERE scope_id = 'old-family'`, nil, 0},
		{"api token deleted: long expired", `SELECT count(*) FROM api_tokens WHERE id = 'token-long-expired'`, nil, 0},
		{"api token kept: within grace", `SELECT count(*) FROM api_tokens WHERE id = 'token-recently-expired'`, nil, 1},
		{"api token kept: no expiry", `SELECT count(*) FROM api_tokens WHERE id = 'token-no-expiry'`, nil, 1},
		{"api token kept: future", `SELECT count(*) FROM api_tokens WHERE id = 'token-future'`, nil, 1},
		{"project survives", `SELECT count(*) FROM projects WHERE id = $1`, []any{project.ID}, 1},
	} {
		if got := countRows(t, store, check.query, check.args...); got != check.want {
			t.Errorf("%s: count = %d, want %d", check.name, got, check.want)
		}
	}
}

func TestPruneRetainedSkipsWhenLockHeldElsewhere(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)

	holder, err := store.Pool().Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire holder: %v", err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock(hashtext('lv24_brain_retention'))`); err != nil {
		t.Fatalf("hold retention lock: %v", err)
	}

	_, err = store.PruneRetained(ctx, RetentionPolicy{Audit: time.Now()})
	if !errors.Is(err, ErrRetentionBusy) {
		t.Fatalf("PruneRetained while locked: err = %v, want ErrRetentionBusy", err)
	}

	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('lv24_brain_retention'))`); err != nil {
		t.Fatalf("release retention lock: %v", err)
	}
	if _, err := store.PruneRetained(ctx, RetentionPolicy{Audit: time.Now()}); err != nil {
		t.Fatalf("PruneRetained after release: %v", err)
	}
}

func TestOpenAppliesPoolSizeAndStatementTimeout(t *testing.T) {
	databaseURL := os.Getenv(TestDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set; skipping PostgreSQL integration test", TestDatabaseURLEnv)
	}
	ctx := testContext(t)
	store, err := Open(ctx, databaseURL, Options{MaxConns: 3, StatementTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(store.Close)

	if got := store.Pool().Config().MaxConns; got != 3 {
		t.Fatalf("max conns = %d, want 3", got)
	}
	var timeout string
	if err := store.Pool().QueryRow(ctx, `SHOW statement_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("SHOW statement_timeout: %v", err)
	}
	if timeout != "2s" {
		t.Fatalf("statement_timeout = %q, want 2s", timeout)
	}
}
