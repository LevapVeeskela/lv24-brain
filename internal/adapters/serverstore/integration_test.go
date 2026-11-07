package serverstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/adapters/postgres"
	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// newTestStore builds an isolated PostgreSQL schema per test. Tests skip when
// BRAIN_TEST_DATABASE_URL is unset so the default run stays offline.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	databaseURL := os.Getenv(postgres.TestDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s is not set; skipping PostgreSQL integration test", postgres.TestDatabaseURLEnv)
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

	schema := fmt.Sprintf("brain_serverstore_%d_%d", os.Getpid(), time.Now().UnixNano())
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
	pg := postgres.New(pool)
	t.Cleanup(pg.Close)
	if _, err := pg.Migrate(adminCtx); err != nil {
		t.Fatalf("migrate test schema: %v", err)
	}

	store, err := New(pg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mustProject(t *testing.T, ctx context.Context, store *Store, id string) httpapi.ProjectRecord {
	t.Helper()
	record, _, err := store.CreateProject(ctx, domain.Project{
		ID: id, Name: "Project " + id, Kind: domain.ProjectKindApp,
	})
	if err != nil {
		t.Fatalf("create project %s: %v", id, err)
	}
	return record
}

func TestConnectorRunLifecycle(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)

	record, revision, err := store.CreateConnectorRun(ctx, domain.ConnectorRun{
		Source:    "github",
		Status:    domain.ConnectorRunning,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateConnectorRun: %v", err)
	}
	if record.ID == "" || record.Revision == 0 || revision.Value == "" {
		t.Fatalf("created = %+v revision = %+v", record, revision)
	}

	finished, finishRevision, err := store.FinishConnectorRun(ctx, record.ID, domain.ConnectorSuccess, map[string]int{"repos": 1}, "", 0)
	if err != nil {
		t.Fatalf("FinishConnectorRun: %v", err)
	}
	if finished.Status != domain.ConnectorSuccess || finished.Counts["repos"] != 1 {
		t.Fatalf("finished = %+v", finished)
	}
	if finishRevision.Value == "" || finished.Revision <= record.Revision {
		t.Fatalf("finish revision = %+v record revision = %d", finishRevision, record.Revision)
	}

	got, err := store.GetConnectorRun(ctx, record.ID)
	if err != nil {
		t.Fatalf("GetConnectorRun: %v", err)
	}
	if got.ID != record.ID || got.Status != domain.ConnectorSuccess {
		t.Fatalf("got = %+v", got)
	}

	list, err := store.ListConnectorRuns(ctx, httpapi.ConnectorRunFilter{Source: "github", Limit: 10})
	if err != nil {
		t.Fatalf("ListConnectorRuns: %v", err)
	}
	if len(list) != 1 || list[0].ID != record.ID {
		t.Fatalf("list = %+v", list)
	}

	if _, _, err := store.FinishConnectorRun(ctx, record.ID, domain.ConnectorSuccess, nil, "", 1); err == nil {
		t.Fatal("expected a revision conflict")
	} else {
		var conflict *httpapi.ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("conflict error = %v", err)
		}
	}

	if _, err := store.DeleteConnectorRun(ctx, record.ID, 0); err != nil {
		t.Fatalf("DeleteConnectorRun: %v", err)
	}
	if _, err := store.GetConnectorRun(ctx, record.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
}

func TestStorePing(t *testing.T) {
	store := newTestStore(t)
	if err := store.Ping(testContext(t)); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestStoreProjectsAndConflictMapping(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)

	record := mustProject(t, ctx, store, "p1")
	if record.Kind != domain.ProjectKindApp || record.Revision == 0 {
		t.Fatalf("project = %+v", record)
	}
	loaded, err := store.GetProject(ctx, "p1")
	if err != nil || loaded.ID != "p1" || loaded.Revision != record.Revision {
		t.Fatalf("GetProject = (%+v, %v)", loaded, err)
	}
	projects, err := store.ListProjects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("ListProjects = (%d, %v)", len(projects), err)
	}

	record.Name = "Renamed"
	updated, revision, err := store.UpdateProject(ctx, record.Project, record.Revision)
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	if updated.Name != "Renamed" || revision.Value == "" {
		t.Fatalf("updated = %+v revision = %+v", updated, revision)
	}

	_, _, err = store.UpdateProject(ctx, record.Project, record.Revision)
	var conflict *httpapi.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("stale update error = %T (%v), want *httpapi.ConflictError", err, err)
	}
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatal("conflict must unwrap to domain.ErrConflict")
	}
	if conflict.Current != updated.Revision || conflict.Entity != "projects" {
		t.Fatalf("conflict = %+v", conflict)
	}

	if _, err := store.DeleteProject(ctx, "p1", updated.Revision); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := store.GetProject(ctx, "p1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetProject after delete = %v", err)
	}
}

func TestStoreState(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	project := mustProject(t, ctx, store, "p1")

	if _, err := store.GetState(ctx, project.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetState before put = %v", err)
	}
	freshUntil := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	record, revision, err := store.PutState(ctx, domain.State{
		ProjectID: project.ID, Goal: "ship", Active: []string{"wp8"}, FreshUntil: freshUntil,
	}, 0)
	if err != nil {
		t.Fatalf("PutState: %v", err)
	}
	if record.Goal != "ship" || record.Revision == 0 {
		t.Fatalf("state = %+v", record)
	}
	if _, _, err := store.PutState(ctx, domain.State{ProjectID: project.ID, Goal: "other"}, revisionValue(t, revision)+99); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale PutState error = %v", err)
	}
	loaded, err := store.GetState(ctx, project.ID)
	if err != nil || loaded.Goal != "ship" {
		t.Fatalf("GetState = (%+v, %v)", loaded, err)
	}
}

func TestStoreUsersAndOIDC(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)

	record, _, err := store.CreateUser(ctx, domain.User{ID: "user_1", Login: "alice"}, "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if record.Role != domain.RoleMember || record.PasswordHash != "hash" {
		t.Fatalf("user = %+v", record)
	}
	if _, err := store.GetUserByLogin(ctx, "ALICE"); err != nil {
		t.Fatalf("GetUserByLogin: %v", err)
	}
	if _, err := store.SetUserOIDC(ctx, "user_1", "https://id.example", "subject-1"); err != nil {
		t.Fatalf("SetUserOIDC: %v", err)
	}
	byOIDC, err := store.GetUserByOIDC(ctx, "https://id.example", "subject-1")
	if err != nil || byOIDC.ID != "user_1" {
		t.Fatalf("GetUserByOIDC = (%+v, %v)", byOIDC, err)
	}

	user := byOIDC.User
	user.DisplayName = "Alice"
	user.Role = domain.RoleAdmin
	updated, _, err := store.UpdateUser(ctx, user, 0)
	if err != nil || updated.Role != domain.RoleAdmin {
		t.Fatalf("UpdateUser = (%+v, %v)", updated, err)
	}
	users, err := store.ListUsers(ctx)
	if err != nil || len(users) != 1 {
		t.Fatalf("ListUsers = (%d, %v)", len(users), err)
	}
	roles, err := store.ListRoles(ctx)
	if err != nil || len(roles) != 3 {
		t.Fatalf("ListRoles = (%d, %v)", len(roles), err)
	}
	// Deleting the only admin is refused by the last-admin guard, so a second
	// admin is seeded before user_1 is removed.
	if _, _, err := store.CreateUser(ctx, domain.User{ID: "user_2", Login: "bob", Role: domain.RoleAdmin, AuthSource: "local"}, ""); err != nil {
		t.Fatalf("CreateUser user_2: %v", err)
	}
	if _, err := store.DeleteUser(ctx, "user_1", 0); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
}

func TestStoreKnowledge(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	project := mustProject(t, ctx, store, "p1")

	note, noteRevision, err := store.CreateNote(ctx, domain.Note{ProjectID: project.ID, Title: "Note", Body: "body"})
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if note.Revision == 0 || revisionValue(t, noteRevision) != note.Revision {
		t.Fatalf("note revision mismatch: %+v %+v", note, noteRevision)
	}
	if _, err := store.GetNote(ctx, note.ID); err != nil {
		t.Fatalf("GetNote: %v", err)
	}
	notes, err := store.ListNotes(ctx, httpapi.ListOptions{ProjectID: project.ID})
	if err != nil || len(notes) != 1 {
		t.Fatalf("ListNotes = (%d, %v)", len(notes), err)
	}
	note.Title = "Note 2"
	updated, _, err := store.UpdateNote(ctx, note.Note, note.Revision)
	if err != nil || updated.Title != "Note 2" {
		t.Fatalf("UpdateNote = (%+v, %v)", updated, err)
	}
	if _, err := store.DeleteNote(ctx, note.ID, 0); err != nil {
		t.Fatalf("DeleteNote: %v", err)
	}

	decision, decisionRevision, err := store.CreateDecision(ctx, domain.Decision{ProjectID: project.ID, Title: "Decision"})
	if err != nil {
		t.Fatalf("CreateDecision: %v", err)
	}
	if decision.Status != domain.DecisionProposed {
		t.Fatalf("decision status = %q", decision.Status)
	}
	if _, err := store.GetDecision(ctx, decision.ID); err != nil {
		t.Fatalf("GetDecision: %v", err)
	}
	if _, _, err := store.UpdateDecision(ctx, decision.Decision, revisionValue(t, decisionRevision)); err != nil {
		t.Fatalf("UpdateDecision: %v", err)
	}
	if _, err := store.ListDecisions(ctx, httpapi.ListOptions{ProjectID: project.ID}); err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}

	attempt, attemptRevision, err := store.CreateAttempt(ctx, domain.Attempt{ProjectID: project.ID, Title: "Attempt"})
	if err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}
	if _, err := store.GetAttempt(ctx, attempt.ID); err != nil {
		t.Fatalf("GetAttempt: %v", err)
	}
	if _, _, err := store.UpdateAttempt(ctx, attempt.Attempt, revisionValue(t, attemptRevision)); err != nil {
		t.Fatalf("UpdateAttempt: %v", err)
	}
	if _, err := store.ListAttempts(ctx, httpapi.ListOptions{ProjectID: project.ID}); err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}

	handoff, handoffRevision, err := store.CreateHandoff(ctx, domain.Handoff{ProjectID: project.ID, Summary: "Handoff"})
	if err != nil {
		t.Fatalf("CreateHandoff: %v", err)
	}
	if _, err := store.GetHandoff(ctx, handoff.ID); err != nil {
		t.Fatalf("GetHandoff: %v", err)
	}
	if _, err := store.ListHandoffs(ctx, httpapi.ListOptions{ProjectID: project.ID}); err != nil {
		t.Fatalf("ListHandoffs: %v", err)
	}
	if _, err := store.DeleteHandoff(ctx, handoff.ID, revisionValue(t, handoffRevision)); err != nil {
		t.Fatalf("DeleteHandoff: %v", err)
	}

	if _, err := store.DeleteDecision(ctx, decision.ID, 0); err != nil {
		t.Fatalf("DeleteDecision: %v", err)
	}
	if _, err := store.DeleteAttempt(ctx, attempt.ID, 0); err != nil {
		t.Fatalf("DeleteAttempt: %v", err)
	}
}

func TestStoreSearch(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	first := mustProject(t, ctx, store, "p1")
	second := mustProject(t, ctx, store, "p2")

	if _, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID: first.ID, Title: "Offline mode", Body: "sqlite indexing strategy for offline mode",
	}); err != nil {
		t.Fatalf("create note: %v", err)
	}
	if _, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID: first.ID, Title: "Coverage", Body: "100% coverage in CI",
	}); err != nil {
		t.Fatalf("create note: %v", err)
	}
	if _, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID: first.ID, Title: "Volume", Body: "1000 items processed nightly",
	}); err != nil {
		t.Fatalf("create note: %v", err)
	}
	if _, _, err := store.CreateDecision(ctx, domain.Decision{
		ProjectID: second.ID, Title: "Search engine choice", Context: "postgres full text search",
	}); err != nil {
		t.Fatalf("create decision: %v", err)
	}
	if _, _, err := store.CreateAttempt(ctx, domain.Attempt{
		ProjectID: second.ID, Title: "Cache layer", Hypothesis: "redis helps",
	}); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	if _, _, err := store.CreateHandoff(ctx, domain.Handoff{
		ProjectID: first.ID, Summary: "Continue WP8 wiring",
	}); err != nil {
		t.Fatalf("create handoff: %v", err)
	}

	hits, err := store.Search(ctx, ports.Query{Text: "sqlite"})
	if err != nil || len(hits) != 1 || hits[0].Kind != "note" || hits[0].ProjectID != first.ID {
		t.Fatalf("Search(sqlite) = (%+v, %v)", hits, err)
	}
	if hits[0].Snippet == "" {
		t.Fatal("search hit has no snippet")
	}

	hits, err = store.Search(ctx, ports.Query{Text: "ENGINE"})
	if err != nil || len(hits) != 1 || hits[0].Kind != "decision" {
		t.Fatalf("Search(ENGINE) = (%+v, %v)", hits, err)
	}
	if hits[0].Score <= 0 {
		t.Fatalf("title match score = %v, want > 0", hits[0].Score)
	}

	hits, err = store.Search(ctx, ports.Query{Text: "100%"})
	if err != nil || len(hits) != 1 || hits[0].Title != "Coverage" {
		t.Fatalf("Search(100%%) = (%+v, %v)", hits, err)
	}

	hits, err = store.Search(ctx, ports.Query{Text: "sqlite", ProjectID: second.ID})
	if err != nil || len(hits) != 0 {
		t.Fatalf("Search scoped = (%+v, %v)", hits, err)
	}

	if _, err := store.Search(ctx, ports.Query{Text: "  "}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty search error = %v", err)
	}
}

func TestStoreSearchRanksTitleMatchFirst(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	project := mustProject(t, ctx, store, "rank")

	// Body-only match is created last so recency cannot explain the order.
	if _, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID: project.ID, Title: "Misc", Body: "deploy steps",
	}); err != nil {
		t.Fatalf("create body note: %v", err)
	}
	if _, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID: project.ID, Title: "Deploy", Body: "unrelated words",
	}); err != nil {
		t.Fatalf("create title note: %v", err)
	}

	hits, err := store.Search(ctx, ports.Query{Text: "deploy", ProjectID: project.ID})
	if err != nil || len(hits) != 2 {
		t.Fatalf("Search(deploy) = (%+v, %v)", hits, err)
	}
	if hits[0].Title != "Deploy" || hits[0].Score <= hits[1].Score {
		t.Fatalf("title match is not ranked first: %+v", hits)
	}
}

func TestStoreSearchLimitAppliesAfterRanking(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	project := mustProject(t, ctx, store, "limit")

	// Many weak matches are created first; the strong title match is newest.
	for i := 0; i < 12; i++ {
		if _, _, err := store.CreateNote(ctx, domain.Note{
			ProjectID: project.ID, Title: fmt.Sprintf("Filler %d", i), Body: "cache mentioned once",
		}); err != nil {
			t.Fatalf("create filler note: %v", err)
		}
	}
	if _, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID: project.ID, Title: "Cache", Body: "layer",
	}); err != nil {
		t.Fatalf("create title note: %v", err)
	}

	hits, err := store.Search(ctx, ports.Query{Text: "cache", ProjectID: project.ID, Limit: 1})
	if err != nil || len(hits) != 1 || hits[0].Title != "Cache" {
		t.Fatalf("Search(limit 1) = (%+v, %v), want the title match", hits, err)
	}
}

func TestStoreSearchKindFilters(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	project := mustProject(t, ctx, store, "kinds")
	if _, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID: project.ID, Title: "Rollout", Body: "rollout plan",
	}); err != nil {
		t.Fatalf("create note: %v", err)
	}
	if _, _, err := store.CreateDecision(ctx, domain.Decision{
		ProjectID: project.ID, Title: "Rollout strategy", Context: "staged",
	}); err != nil {
		t.Fatalf("create decision: %v", err)
	}
	if _, _, err := store.CreateHandoff(ctx, domain.Handoff{
		ProjectID: project.ID, Summary: "rollout pending",
	}); err != nil {
		t.Fatalf("create handoff: %v", err)
	}

	cases := []struct {
		name  string
		query ports.Query
		want  []string
	}{
		{"all kinds", ports.Query{}, []string{"note", "decision", "handoff"}},
		{"kind keeps one", ports.Query{Kinds: []string{"decision"}}, []string{"decision"}},
		{"kind keeps two", ports.Query{Kinds: []string{"note", "handoff"}}, []string{"note", "handoff"}},
		{"exclude drops one", ports.Query{ExcludeKinds: []string{"note"}}, []string{"decision", "handoff"}},
		{"kind then exclude", ports.Query{Kinds: []string{"note", "decision"}, ExcludeKinds: []string{"note"}}, []string{"decision"}},
		{"unindexed kind finds nothing", ports.Query{Kinds: []string{"dump"}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.query.Text = "rollout"
			tc.query.ProjectID = project.ID
			hits, err := store.Search(ctx, tc.query)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			got := make([]string, 0, len(hits))
			for _, hit := range hits {
				got = append(got, hit.Kind)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("kinds = %v, want %v", got, tc.want)
			}
			seen := map[string]bool{}
			for _, kind := range got {
				seen[kind] = true
			}
			for _, kind := range tc.want {
				if !seen[kind] {
					t.Fatalf("kinds = %v, missing %q", got, kind)
				}
			}
		})
	}
}

func TestStoreSearchRussianText(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	project := mustProject(t, ctx, store, "ru")
	if _, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID: project.ID, Title: "Столица", Body: "Минск — столица Беларуси, сборка OK",
	}); err != nil {
		t.Fatalf("create note: %v", err)
	}

	for _, text := range []string{"столица", "Беларуси", "минск OK"} {
		hits, err := store.Search(ctx, ports.Query{Text: text, ProjectID: project.ID})
		if err != nil || len(hits) != 1 {
			t.Fatalf("Search(%q) = (%+v, %v), want one hit", text, hits, err)
		}
		if hits[0].Snippet == "" {
			t.Fatalf("Search(%q) returned an empty snippet", text)
		}
	}
}

func TestStoreSearchFallsBackWhenQueryHasNoLexemes(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	project := mustProject(t, ctx, store, "fallback")
	if _, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID: project.ID, Title: "Alert", Body: "build failed!!! retry",
	}); err != nil {
		t.Fatalf("create note: %v", err)
	}

	hits, err := store.Search(ctx, ports.Query{Text: "!!!", ProjectID: project.ID})
	if err != nil || len(hits) != 1 || hits[0].Title != "Alert" {
		t.Fatalf("Search(!!!) = (%+v, %v), want the punctuation match", hits, err)
	}
	hits, err = store.Search(ctx, ports.Query{Text: "%", ProjectID: project.ID})
	if err != nil || len(hits) != 0 {
		t.Fatalf("Search(%%) = (%+v, %v), want no hits", hits, err)
	}
}

func TestStoreRevisionsAscending(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	project := mustProject(t, ctx, store, "p1")
	if _, _, err := store.CreateNote(ctx, domain.Note{ID: "note_1", ProjectID: project.ID, Body: "one"}); err != nil {
		t.Fatalf("create note: %v", err)
	}
	if _, _, err := store.CreateNote(ctx, domain.Note{ID: "note_2", ProjectID: project.ID, Body: "two"}); err != nil {
		t.Fatalf("create note: %v", err)
	}

	ascending, err := store.ListRevisions(ctx, httpapi.RevisionFilter{Ascending: true, Limit: 100})
	if err != nil {
		t.Fatalf("ListRevisions ascending: %v", err)
	}
	if len(ascending) < 3 {
		t.Fatalf("revision count = %d, want at least 3", len(ascending))
	}
	for i := 1; i < len(ascending); i++ {
		if ascending[i-1].ID >= ascending[i].ID {
			t.Fatalf("ascending order violated at %d: %d >= %d", i, ascending[i-1].ID, ascending[i].ID)
		}
	}
	after := ascending[1].ID
	tail, err := store.ListRevisions(ctx, httpapi.RevisionFilter{Ascending: true, AfterID: after, Limit: 100})
	if err != nil {
		t.Fatalf("ListRevisions after: %v", err)
	}
	for _, record := range tail {
		if record.ID <= after {
			t.Fatalf("revision %d not after %d", record.ID, after)
		}
	}

	descending, err := store.ListRevisions(ctx, httpapi.RevisionFilter{Limit: 100})
	if err != nil {
		t.Fatalf("ListRevisions descending: %v", err)
	}
	if len(descending) == 0 || descending[0].ID != ascending[len(ascending)-1].ID {
		t.Fatalf("descending head = %+v, ascending tail = %+v", descending, ascending[len(ascending)-1])
	}

	notes, err := store.ListRevisions(ctx, httpapi.RevisionFilter{EntityKind: "note", Limit: 100})
	if err != nil || len(notes) < 2 {
		t.Fatalf("ListRevisions(note) = (%d, %v)", len(notes), err)
	}
	for _, record := range notes {
		if record.EntityKind != "note" {
			t.Fatalf("unexpected entity kind %q", record.EntityKind)
		}
	}
}

func TestStoreAPITokensAndRefresh(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	if _, _, err := store.CreateUser(ctx, domain.User{ID: "user_1", Login: "alice"}, ""); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	token, err := store.CreateAPIToken(ctx, httpapi.APITokenRecord{
		UserID: "user_1", Name: "cli", Role: domain.RoleMember, Prefix: "brain_abc", TokenHash: "hash-1",
	})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if token.ID == "" || token.CreatedAt.IsZero() {
		t.Fatalf("token = %+v", token)
	}
	if _, err := store.GetAPIToken(ctx, token.ID); err != nil {
		t.Fatalf("GetAPIToken: %v", err)
	}
	found, err := store.FindAPITokenByHash(ctx, "hash-1", time.Now())
	if err != nil || found.LastUsedAt == nil {
		t.Fatalf("FindAPITokenByHash = (%+v, %v)", found, err)
	}
	tokens, err := store.ListAPITokens(ctx, "user_1")
	if err != nil || len(tokens) != 1 {
		t.Fatalf("ListAPITokens = (%d, %v)", len(tokens), err)
	}
	if _, err := store.CreateAPIToken(ctx, httpapi.APITokenRecord{
		UserID: "missing", Name: "cli", Role: domain.RoleMember, TokenHash: "hash-2",
	}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("CreateAPIToken unknown user = %v", err)
	}
	if err := store.RevokeAPIToken(ctx, token.ID, time.Now()); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	if _, err := store.FindAPITokenByHash(ctx, "hash-1", time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("FindAPITokenByHash after revoke = %v", err)
	}

	now := time.Now().UTC()
	record := auth.RefreshRecord{
		SessionID: "sess_1", FamilyID: "fam_1", UserID: "user_1",
		TokenHash: "refresh-1", IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := store.CreateRefresh(ctx, record); err != nil {
		t.Fatalf("CreateRefresh: %v", err)
	}
	if err := store.CreateRefresh(ctx, record); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate CreateRefresh = %v", err)
	}
	consumed, err := store.ConsumeRefresh(ctx, "refresh-1", now)
	if err != nil || consumed.SessionID != "sess_1" {
		t.Fatalf("ConsumeRefresh = (%+v, %v)", consumed, err)
	}
	if _, err := store.ConsumeRefresh(ctx, "refresh-1", now); !errors.Is(err, auth.ErrRefreshReused) {
		t.Fatalf("reused ConsumeRefresh = %v", err)
	}
	if err := store.RevokeFamily(ctx, "fam_1", now); err != nil {
		t.Fatalf("RevokeFamily: %v", err)
	}
	if _, err := store.ConsumeRefresh(ctx, "refresh-1", now); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("ConsumeRefresh after revoke = %v", err)
	}
}

func revisionValue(t *testing.T, revision domain.SyncRevision) int64 {
	t.Helper()
	value, err := postgres.RevisionValue(revision)
	if err != nil {
		t.Fatalf("revision %+v: %v", revision, err)
	}
	return value
}
