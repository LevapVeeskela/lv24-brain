package postgres

import (
	"errors"
	"slices"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestProjectAccessMigrationIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	migrations, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	var accessSQL string
	for _, migration := range migrations {
		if migration.Version == 8 {
			accessSQL = migration.SQL
		}
	}
	if accessSQL == "" {
		t.Fatal("migration 8 (project access) is missing")
	}
	for run := 0; run < 2; run++ {
		if _, err := store.Pool().Exec(ctx, accessSQL); err != nil {
			t.Fatalf("re-run migration 8 (pass %d): %v", run+1, err)
		}
	}
	// Unknown scopes are rejected by the database too.
	user, _, err := store.CreateUser(ctx, domain.User{Login: "scoped", Role: domain.RoleMember, AuthSource: "local"}, "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_, err = store.CreateAPIToken(ctx, APITokenRecord{
		UserID: user.ID, Name: "bad", Role: domain.RoleMember, TokenHash: "hash-bad", Scopes: []string{"root"},
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("CreateAPIToken with unknown scope = %v, want validation error", err)
	}
	token, err := store.CreateAPIToken(ctx, APITokenRecord{
		UserID: user.ID, Name: "ok", Role: domain.RoleMember, TokenHash: "hash-ok",
		Scopes: []string{"read"}, ProjectIDs: []string{"p1"},
	})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	found, err := store.FindAPITokenByHash(ctx, "hash-ok", store.nowUTC())
	if err != nil || !slices.Equal(found.Scopes, []string{"read"}) || !slices.Equal(found.ProjectIDs, []string{"p1"}) || found.ID != token.ID {
		t.Fatalf("FindAPITokenByHash = (%+v, %v), want scopes and project ids", found, err)
	}
	legacy, err := store.CreateAPIToken(ctx, APITokenRecord{UserID: user.ID, Name: "legacy", Role: domain.RoleMember, TokenHash: "hash-legacy"})
	if err != nil || len(legacy.Scopes) != 0 || len(legacy.ProjectIDs) != 0 {
		t.Fatalf("unscoped token = (%+v, %v), want empty scopes", legacy, err)
	}
}

func TestProjectMembershipQueries(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	open := mustProject(t, ctx, store, "acl_open")
	secret := mustProject(t, ctx, store, "acl_secret")
	alice, _, err := store.CreateUser(ctx, domain.User{Login: "alice", Role: domain.RoleMember, AuthSource: "local"}, "")
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	bob, _, err := store.CreateUser(ctx, domain.User{Login: "Bob", Role: domain.RoleMember, AuthSource: "local"}, "")
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}

	restrictions, err := store.ProjectRestrictions(ctx, alice.ID)
	if err != nil || len(restrictions) != 0 {
		t.Fatalf("initial restrictions = (%v, %v), want none", restrictions, err)
	}

	stored, previous, err := store.PutProjectMember(ctx, domain.ProjectMember{ProjectID: secret.ID, UserID: alice.ID, Role: domain.ProjectRoleMaintainer})
	if err != nil || previous != "" || stored.Login != "alice" || stored.Role != domain.ProjectRoleMaintainer {
		t.Fatalf("PutProjectMember = (%+v, %q, %v)", stored, previous, err)
	}
	if _, previous, err = store.PutProjectMember(ctx, domain.ProjectMember{ProjectID: secret.ID, UserID: alice.ID, Role: domain.ProjectRoleViewer}); err != nil || previous != domain.ProjectRoleMaintainer {
		t.Fatalf("update member previous = %q, %v", previous, err)
	}
	if _, _, err := store.PutProjectMember(ctx, domain.ProjectMember{ProjectID: secret.ID, UserID: bob.ID, Role: domain.ProjectRoleMember}); err != nil {
		t.Fatalf("PutProjectMember bob: %v", err)
	}
	if _, _, err := store.PutProjectMember(ctx, domain.ProjectMember{ProjectID: "missing", UserID: bob.ID, Role: domain.ProjectRoleMember}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing project = %v, want not found", err)
	}
	if _, _, err := store.PutProjectMember(ctx, domain.ProjectMember{ProjectID: secret.ID, UserID: "user_missing", Role: domain.ProjectRoleMember}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing user = %v, want not found", err)
	}
	if _, _, err := store.PutProjectMember(ctx, domain.ProjectMember{ProjectID: secret.ID, UserID: bob.ID, Role: "owner"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad role = %v, want validation", err)
	}

	restrictions, err = store.ProjectRestrictions(ctx, alice.ID)
	if err != nil || len(restrictions) != 1 || restrictions[secret.ID] != domain.ProjectRoleViewer {
		t.Fatalf("alice restrictions = (%v, %v)", restrictions, err)
	}
	if _, ok := restrictions[open.ID]; ok {
		t.Fatal("open project reported as restricted")
	}
	access, err := store.GetProjectAccess(ctx, secret.ID)
	if err != nil || !access.Restricted || len(access.Members) != 2 || access.Members[0].Login != "alice" || access.Members[1].Login != "Bob" {
		t.Fatalf("GetProjectAccess = (%+v, %v)", access, err)
	}
	if _, err := store.GetProjectAccess(ctx, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetProjectAccess missing = %v", err)
	}

	// Reopening with members conflicts; removing them keeps the restriction.
	if _, err := store.SetProjectRestricted(ctx, secret.ID, false); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("reopen with members = %v, want conflict", err)
	}
	if role, err := store.DeleteProjectMember(ctx, secret.ID, alice.ID); err != nil || role != domain.ProjectRoleViewer {
		t.Fatalf("DeleteProjectMember = %q, %v", role, err)
	}
	if _, err := store.DeleteProjectMember(ctx, secret.ID, alice.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second delete = %v, want not found", err)
	}
	// A deleted user's membership cascades; the project stays restricted.
	if _, err := store.DeleteUser(ctx, bob.ID, 0); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	restrictions, err = store.ProjectRestrictions(ctx, alice.ID)
	if err != nil || len(restrictions) != 1 || restrictions[secret.ID] != "" {
		t.Fatalf("restrictions after removals = (%v, %v), want secret without role", restrictions, err)
	}
	if changed, err := store.SetProjectRestricted(ctx, secret.ID, false); err != nil || !changed {
		t.Fatalf("reopen = %v, %v", changed, err)
	}
	if changed, err := store.SetProjectRestricted(ctx, open.ID, true); err != nil || !changed {
		t.Fatalf("restrict = %v, %v", changed, err)
	}
	if changed, err := store.SetProjectRestricted(ctx, open.ID, true); err != nil || changed {
		t.Fatalf("restrict again = %v, %v, want unchanged", changed, err)
	}
	// Deleting a project cascades its restriction.
	if _, err := store.DeleteProject(ctx, open.ID, 0); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	restrictions, err = store.ProjectRestrictions(ctx, alice.ID)
	if err != nil || len(restrictions) != 0 {
		t.Fatalf("restrictions after project delete = (%v, %v)", restrictions, err)
	}
}

func TestKnowledgeRevisionsCarryProjectID(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	project := mustProject(t, ctx, store, "acl_rev")
	note, _, err := store.CreateNote(ctx, domain.Note{ProjectID: project.ID, Body: "b"})
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	note.Body = "c"
	if _, _, err := store.UpdateNote(ctx, note.Note, 0); err != nil {
		t.Fatalf("UpdateNote: %v", err)
	}
	if _, err := store.DeleteNote(ctx, note.ID, 0); err != nil {
		t.Fatalf("DeleteNote: %v", err)
	}
	revisions, err := store.ListRevisions(ctx, RevisionFilter{EntityKind: "note", EntityID: note.ID})
	if err != nil || len(revisions) != 3 {
		t.Fatalf("ListRevisions = (%d, %v), want 3", len(revisions), err)
	}
	for _, revision := range revisions {
		if revision.Detail["project_id"] != project.ID {
			t.Fatalf("revision %d detail = %v, want project_id", revision.ID, revision.Detail)
		}
	}
}
