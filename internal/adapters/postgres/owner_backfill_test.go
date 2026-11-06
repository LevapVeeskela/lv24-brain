package postgres

import (
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// TestOwnerBackfillFromAudit replays migration 0005 (its SQL is idempotent)
// over rows whose owner is unknown and checks that the creator is recovered
// from the "<kind>.create" audit event, while rows without a usable event
// stay unowned.
func TestOwnerBackfillFromAudit(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	mustProject(t, ctx, store, "owned")
	user, _, err := store.CreateUser(ctx, domain.User{Login: "creator@example.test", Role: domain.RoleMember, AuthSource: "local"}, "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Created with the creator's login as audit actor, but without an owner
	// id, as rows written before ownership tracking were.
	withLogin := WithActor(ctx, "creator@example.test")
	known, _, err := store.CreateNote(withLogin, domain.Note{ProjectID: "owned", Body: "known creator"})
	if err != nil {
		t.Fatalf("create note: %v", err)
	}
	decision, _, err := store.CreateDecision(withLogin, domain.Decision{ProjectID: "owned", Title: "known", Status: domain.DecisionProposed})
	if err != nil {
		t.Fatalf("create decision: %v", err)
	}
	unknown, _, err := store.CreateNote(ctx, domain.Note{ProjectID: "owned", Body: "no actor"})
	if err != nil {
		t.Fatalf("create anonymous note: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE notes SET created_by = ''`); err != nil {
		t.Fatalf("reset notes owner: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE decisions SET created_by = ''`); err != nil {
		t.Fatalf("reset decisions owner: %v", err)
	}

	migrations, err := LoadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	var backfill string
	for _, migration := range migrations {
		if migration.Version == 5 {
			backfill = migration.SQL
		}
	}
	if backfill == "" {
		t.Fatal("migration 0005 not found")
	}
	if _, err := store.pool.Exec(ctx, backfill); err != nil {
		t.Fatalf("replay migration 0005: %v", err)
	}

	for _, check := range []struct {
		table, id, want string
	}{
		{"notes", known.ID, user.ID},
		{"decisions", decision.ID, user.ID},
		{"notes", unknown.ID, ""},
	} {
		var got string
		if err := store.pool.QueryRow(ctx, `SELECT created_by FROM `+check.table+` WHERE id = $1`, check.id).Scan(&got); err != nil {
			t.Fatalf("read %s %s: %v", check.table, check.id, err)
		}
		if got != check.want {
			t.Fatalf("%s %s created_by = %q, want %q", check.table, check.id, got, check.want)
		}
	}
}
