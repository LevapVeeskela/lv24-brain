package serverstore

import (
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// TestSearchScopedFiltersProjectsBeforeLimit checks the SQL project filter:
// hidden projects never appear and do not consume the result limit.
func TestSearchScopedFiltersProjectsBeforeLimit(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	open := mustProject(t, ctx, store, "scope_open")
	secret := mustProject(t, ctx, store, "scope_secret")
	for i := 0; i < 3; i++ {
		if _, _, err := store.CreateNote(ctx, domain.Note{ProjectID: secret.ID, Title: "zebra zebra", Body: "zebra"}); err != nil {
			t.Fatalf("create secret note: %v", err)
		}
	}
	if _, _, err := store.CreateNote(ctx, domain.Note{ProjectID: open.ID, Title: "other", Body: "zebra"}); err != nil {
		t.Fatalf("create open note: %v", err)
	}
	query := ports.Query{Text: "zebra", Limit: 1}
	for name, scope := range map[string]httpapi.ProjectScope{
		"exclude": {Exclude: []string{secret.ID}},
		"only":    {Only: []string{open.ID}},
	} {
		hits, err := store.SearchScoped(ctx, query, scope)
		if err != nil || len(hits) != 1 || hits[0].ProjectID != open.ID {
			t.Fatalf("%s: SearchScoped = (%+v, %v), want the open note", name, hits, err)
		}
	}
	hits, err := store.SearchScoped(ctx, ports.Query{Text: "zebra"}, httpapi.ProjectScope{})
	if err != nil || len(hits) != 4 {
		t.Fatalf("unscoped SearchScoped = (%d, %v), want 4", len(hits), err)
	}
}

func TestProjectAccessPassthrough(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	project := mustProject(t, ctx, store, "pass_acl")
	user, _, err := store.CreateUser(ctx, domain.User{Login: "member", Role: domain.RoleMember, AuthSource: "local"}, "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, _, err := store.PutProjectMember(ctx, domain.ProjectMember{ProjectID: project.ID, UserID: user.ID, Role: domain.ProjectRoleMember}); err != nil {
		t.Fatalf("PutProjectMember: %v", err)
	}
	access, err := store.GetProjectAccess(ctx, project.ID)
	if err != nil || !access.Restricted || len(access.Members) != 1 || access.Members[0].Login != "member" {
		t.Fatalf("GetProjectAccess = (%+v, %v)", access, err)
	}
	restrictions, err := store.ProjectRestrictions(ctx, user.ID)
	if err != nil || restrictions[project.ID] != domain.ProjectRoleMember {
		t.Fatalf("ProjectRestrictions = (%v, %v)", restrictions, err)
	}
	token, err := store.CreateAPIToken(ctx, httpapi.APITokenRecord{
		UserID: user.ID, Name: "scoped", Role: domain.RoleMember, TokenHash: "hash-pass",
		Scopes: []domain.TokenScope{domain.TokenScopeRead}, ProjectIDs: []string{project.ID},
	})
	if err != nil || len(token.Scopes) != 1 || token.Scopes[0] != domain.TokenScopeRead || len(token.ProjectIDs) != 1 {
		t.Fatalf("CreateAPIToken = (%+v, %v)", token, err)
	}
}
