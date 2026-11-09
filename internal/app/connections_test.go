package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

type migrationSecrets struct {
	values map[string]string
	reads  int
	fail   bool
}

func (s *migrationSecrets) Get(_ context.Context, key string) (string, error) {
	s.reads++
	if s.fail {
		return "", errors.New("backend error containing private-legacy-value")
	}
	if value, ok := s.values[key]; ok {
		return value, nil
	}
	return "", domain.ErrNotFound
}
func (s *migrationSecrets) Set(_ context.Context, key, value string) error {
	s.values[key] = value
	return nil
}
func (s *migrationSecrets) Delete(_ context.Context, key string) error {
	delete(s.values, key)
	return nil
}
func (s *migrationSecrets) Names(context.Context) ([]string, error) { return nil, nil }

func migrationFixture(t *testing.T, env map[string]string) (bootstrap.LegacyConnectorOptions, *fsstore.ConnectionStore) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "connectors"), 0o755); err != nil {
		t.Fatal(err)
	}
	mail := `{"accounts":[{"id":"Work.Mail","host":"imap.example.test","username":"work@example.test","folders":["INBOX","Sent"],"days":14},{"id":"home","host":"imap.home.test","port":1993,"username":"home","folders":["INBOX"]}]}`
	if err := os.WriteFile(filepath.Join(root, "connectors", "mail.json"), []byte(mail), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := fsstore.NewConnectionStore(root)
	if err != nil {
		t.Fatal(err)
	}
	return bootstrap.LegacyConnectorOptions{
		DataDir: root,
		Projects: []domain.Project{
			{ID: "gh", RepoURL: "https://github.com/Acme/one.git"},
			{ID: "gh-two", RepoURL: "git@github.com:acme/two.git"},
			{ID: "gl", RepoURL: "https://gitlab.com/group/public.git"},
			{ID: "private", RepoURL: "ssh://git@gitlab.internal.test:2222/team/nested/private.git"},
			{ID: "checkout", Paths: []string{root}},
		},
		Getenv: func(key string) string { return env[key] },
		RemoteURL: func(_ context.Context, _, remote string) (string, error) {
			if remote == "gitlab" {
				return "git@code.internal.test:other/from-checkout.git", nil
			}
			return "", errors.New("no remote")
		},
	}, store
}

func TestMigrateLegacyConnectorsFixturesAndIdempotency(t *testing.T) {
	ctx := context.Background()
	discovery, store := migrationFixture(t, nil)
	legacy := &migrationSecrets{values: map[string]string{"github/token": "private-legacy-value"}}
	migrator := app.ConnectionMigrator{Connections: store, LegacySecrets: legacy, Discover: func(ctx context.Context) ([]app.LegacyConnection, error) {
		return bootstrap.DiscoverLegacyConnectors(ctx, discovery)
	}}
	report, err := migrator.MigrateLegacyConnectors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) != 6 || legacy.reads != 0 || len(report.Credentials) != 6 {
		t.Fatalf("created=%d credentials=%d reads=%d", len(report.Created), len(report.Credentials), legacy.reads)
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]domain.Connection{}
	for _, conn := range list {
		byID[conn.ID] = conn
		entry, err := connectorbuild.Builtin().Lookup(conn.Provider)
		if err != nil {
			t.Fatal(err)
		}
		if err := entry.Descriptor.ValidateConnection(conn); err != nil {
			t.Fatal(err)
		}
	}
	if got := byID["github"]; got.Settings["repos"] != "Acme/one,acme/two" || !reflect.DeepEqual(got.Projects, []string{"gh", "gh-two"}) || got.BaseURL != "https://api.github.com" {
		t.Fatalf("github = %+v", got)
	}
	if got := byID["gitlab-gitlab-internal-test"]; got.Settings["projects"] != "team/nested/private" || got.BaseURL != "https://gitlab.internal.test/api/v4" {
		t.Fatalf("gitlab = %+v", got)
	}
	if got := byID["gitlab-code-internal-test"]; got.Settings["projects"] != "other/from-checkout" {
		t.Fatalf("checkout = %+v", got)
	}
	if got := byID["imap-work-mail"]; got.Account != "Work.Mail" || got.BaseURL != "imaps://imap.example.test:993" || got.Settings["username"] != "work@example.test" || got.Settings["folders"] != "INBOX,Sent" || got.Publish != domain.PublishLocalOnly {
		t.Fatalf("mail = %+v", got)
	}
	before, err := os.ReadFile(filepath.Join(discovery.DataDir, "connectors", "connections.json"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := migrator.MigrateLegacyConnectors(ctx)
	if err != nil || len(again.Created) != 0 || len(again.Existing) != 6 {
		t.Fatalf("rerun: %+v, %v", again, err)
	}
	after, err := os.ReadFile(filepath.Join(discovery.DataDir, "connectors", "connections.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("rerun rewrote file")
	}
}

func TestMigrateLegacyConnectorsEnvironmentOverrides(t *testing.T) {
	discovery, store := migrationFixture(t, map[string]string{
		"BRAIN_GITHUB_REPOS": "env/only,env/only", "BRAIN_GITHUB_BASE_URL": "https://github.enterprise.test/api/v3",
		"BRAIN_GITLAB_PROJECTS": "42,team/env", "BRAIN_GITLAB_BASE_URL": "https://code.example.test/api/v4",
		"BRAIN_IMAP_ACCOUNT": "home", "BRAIN_IMAP_ADDRESS": "[::1]:2993", "BRAIN_IMAP_USERNAME": "env-user", "BRAIN_IMAP_FOLDERS": "Archive,INBOX", "BRAIN_IMAP_PASSWORD_KEY": "mail/custom/password",
	})
	migrator := app.ConnectionMigrator{Connections: store, Discover: func(ctx context.Context) ([]app.LegacyConnection, error) {
		return bootstrap.DiscoverLegacyConnectors(ctx, discovery)
	}}
	report, err := migrator.MigrateLegacyConnectors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) != 4 {
		t.Fatalf("created=%d", len(report.Created))
	}
	for _, conn := range report.Created {
		switch conn.Provider {
		case "github":
			if conn.Settings["repos"] != "env/only" || conn.BaseURL != "https://github.enterprise.test/api/v3" {
				t.Fatalf("github = %+v", conn)
			}
		case "gitlab":
			if conn.Settings["projects"] != "42,team/env" || conn.BaseURL != "https://code.example.test/api/v4" {
				t.Fatalf("gitlab = %+v", conn)
			}
		case "imap":
			if conn.Account == "home" && (conn.BaseURL != "imaps://[::1]:2993" || conn.Settings["username"] != "env-user" || conn.Settings["folders"] != "Archive,INBOX") {
				t.Fatalf("env mail = %+v", conn)
			}
		}
	}
	found := false
	for _, ref := range report.Credentials {
		if ref.ConnectionID == "imap-home" && ref.LegacyKey == "mail/custom/password" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing custom legacy password key")
	}
}

func TestMigrateLegacyConnectorsCopySecretsAndResume(t *testing.T) {
	discovery, store := migrationFixture(t, nil)
	legacy := &migrationSecrets{values: map[string]string{"github/token": "private-legacy-value", "gitlab/token": "private-legacy-value", "imap/work-mail/password": "private-legacy-value"}}
	newSecrets := &migrationSecrets{values: map[string]string{}}
	migrator := app.ConnectionMigrator{Connections: store, LegacySecrets: legacy, Credentials: connectorbuild.SecretCredentials{Store: newSecrets}, Discover: func(ctx context.Context) ([]app.LegacyConnection, error) {
		return bootstrap.DiscoverLegacyConnectors(ctx, discovery)
	}}
	if _, err := migrator.MigrateLegacyConnectors(context.Background()); err != nil {
		t.Fatal(err)
	}
	migrator.CopyLegacySecrets = true
	report, err := migrator.MigrateLegacyConnectors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Created) != 0 || len(newSecrets.values) != 5 {
		t.Fatalf("created=%d copied=%d", len(report.Created), len(newSecrets.values))
	}
	if newSecrets.values["conn/github/token"] != "private-legacy-value" {
		t.Fatal("missing connection token")
	}
	data, err := json.Marshal(report)
	if err != nil || strings.Contains(string(data), "private-legacy-value") || strings.Contains(fmt.Sprintf("%+v", report), "private-legacy-value") {
		t.Fatal("report leaked credentials")
	}
	newSecrets.values["conn/github/token"] = "newer-value"
	if _, err := migrator.MigrateLegacyConnectors(context.Background()); err != nil {
		t.Fatal(err)
	}
	if newSecrets.values["conn/github/token"] != "newer-value" {
		t.Fatal("overwrote existing credential")
	}
	legacy.fail = true
	delete(newSecrets.values, "conn/github/token")
	if _, err := migrator.MigrateLegacyConnectors(context.Background()); err == nil || strings.Contains(err.Error(), "private-legacy-value") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestMigrateLegacyConnectorsIDCollisionsAndExistingIdentity(t *testing.T) {
	store, err := fsstore.NewConnectionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), domain.Connection{ID: "github", Provider: "other"}); err != nil {
		t.Fatal(err)
	}
	seeds := []app.LegacyConnection{
		{Connection: domain.Connection{Provider: "github", BaseURL: "https://api.github.com", Settings: map[string]string{"repos": "a/b"}}},
		{Connection: domain.Connection{Provider: "imap", Account: "a.b", BaseURL: "imaps://one:993", Settings: map[string]string{"username": "a"}}},
		{Connection: domain.Connection{Provider: "imap", Account: "a-b", BaseURL: "imaps://two:993", Settings: map[string]string{"username": "b"}}},
		{Connection: domain.Connection{Provider: "imap", Account: strings.Repeat("x", 100), BaseURL: "imaps://three:993", Settings: map[string]string{"username": "c"}}},
	}
	migrator := app.ConnectionMigrator{Connections: store, Discover: func(context.Context) ([]app.LegacyConnection, error) { return seeds, nil }}
	first, err := migrator.MigrateLegacyConnectors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, conn := range first.Created {
		ids[conn.ID] = true
		if len(conn.ID) > 64 {
			t.Fatal("ID too long")
		}
	}
	if !ids["github-2"] || !ids["imap-a-b"] || !ids["imap-a-b-2"] {
		t.Fatalf("IDs=%v", ids)
	}
	conn, err := store.Get(context.Background(), "github-2")
	if err != nil {
		t.Fatal(err)
	}
	conn.Settings["repos"] = "user/edited"
	if err := store.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	again, err := migrator.MigrateLegacyConnectors(context.Background())
	if err != nil || len(again.Created) != 0 {
		t.Fatalf("rerun=%+v, %v", again, err)
	}
	got, err := store.Get(context.Background(), "github-2")
	if err != nil || got.Settings["repos"] != "user/edited" {
		t.Fatal("modified existing identity")
	}
}

func TestMigrateLegacyConnectorsValidatesWholeInputBeforeWriting(t *testing.T) {
	store, err := fsstore.NewConnectionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	migrator := app.ConnectionMigrator{Connections: store, Discover: func(context.Context) ([]app.LegacyConnection, error) {
		return []app.LegacyConnection{{Connection: domain.Connection{Provider: "github", Settings: map[string]string{"repos": "a/b"}}}, {Connection: domain.Connection{Provider: "imap", Account: "bad", BaseURL: "imaps://host:993", Settings: map[string]string{"username": "bad", "password": "private-legacy-value"}}}}, nil
	}}
	if _, err := migrator.MigrateLegacyConnectors(context.Background()); err == nil || strings.Contains(err.Error(), "private-legacy-value") {
		t.Fatalf("validation=%v", err)
	}
	got, err := store.List(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("partial writes=%v, %v", got, err)
	}
}

func TestMigrateLegacyMailUsesExistingPasswordSlug(t *testing.T) {
	for _, account := range []string{"Work Mail", "Work..Mail"} {
		t.Run(account, func(t *testing.T) {
			discovery, store := migrationFixture(t, map[string]string{
				"BRAIN_IMAP_ACCOUNT": account, "BRAIN_IMAP_ADDRESS": "imap.work.test:993", "BRAIN_IMAP_USERNAME": "user",
			})
			legacy := &migrationSecrets{values: map[string]string{"imap/work-mail/password": "private-legacy-value"}}
			destination := &migrationSecrets{values: map[string]string{}}
			migrator := app.ConnectionMigrator{Connections: store, Credentials: connectorbuild.SecretCredentials{Store: destination}, LegacySecrets: legacy, CopyLegacySecrets: true, Discover: func(ctx context.Context) ([]app.LegacyConnection, error) {
				return bootstrap.DiscoverLegacyConnectors(ctx, discovery)
			}}
			if _, err := migrator.MigrateLegacyConnectors(context.Background()); err != nil {
				t.Fatal(err)
			}
			if destination.values["conn/imap-work-mail/password"] != "private-legacy-value" {
				t.Fatal("did not read legacy slug password")
			}
		})
	}
}

func TestMailAccountDaysInitialWindowAndExplicitOverride(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	mail := bootstrap.MailAccount{Days: 14}
	got := mail.InitialSince(nil, now)
	if got == nil || !got.Equal(now.AddDate(0, 0, -14)) {
		t.Fatalf("initial window=%v", got)
	}
	explicit := now.AddDate(0, 0, -60)
	if got := mail.InitialSince(&explicit, now); got != &explicit {
		t.Fatal("explicit --since/--days lost priority")
	}
	for _, days := range []int{0, -1} {
		if got := (bootstrap.MailAccount{Days: days}).InitialSince(nil, now); got != nil {
			t.Fatalf("days=%d unexpectedly bounded", days)
		}
	}
}

func TestMailAccountDaysDoesNotNarrowResumedImports(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	mail := bootstrap.MailAccount{ID: "work", Days: 7}
	path := filepath.Join(t.TempDir(), "mail-state.json")
	got, err := mail.ImportSince(nil, now, path)
	if err != nil || got == nil || !got.Equal(now.AddDate(0, 0, -7)) {
		t.Fatalf("first run=%v, %v", got, err)
	}
	state := `{"version":1,"accounts":{"work":{"folders":{"INBOX":{"uid_validity":1,"last_uid":42,"updated_at":"2026-01-01T00:00:00Z"}}}}}`
	if err := os.WriteFile(path, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = mail.ImportSince(nil, now, path)
	if err != nil || got != nil {
		t.Fatalf("resumed run narrowed after long offline period: %v, %v", got, err)
	}
	other := bootstrap.MailAccount{ID: "other", Days: 7}
	if got, err := other.ImportSince(nil, now, path); err != nil || got == nil {
		t.Fatalf("other account initial=%v, %v", got, err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mail.ImportSince(nil, now, path); err == nil {
		t.Fatal("ignored corrupted cursor file")
	}
	explicit := now.AddDate(0, 0, -30)
	if got, err := mail.ImportSince(&explicit, now, path); err != nil || got != &explicit {
		t.Fatalf("explicit cutoff=%v, %v", got, err)
	}
}
