package bootstrap

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestGitHubReposFromRegistry(t *testing.T) {
	projects := []domain.Project{
		{ID: "a", RepoURL: "https://github.com/Acme/widgets.git"},
		{ID: "b", RepoURL: "https://github.com/acme/WIDGETS"},
		{ID: "c", RepoURL: "git@github.com:acme/tools.git"},
		{ID: "d", RepoURL: "https://gitlab.com/acme/x.git"},
		{ID: "e"},
	}
	if got, want := GitHubRepos(projects), []string{"Acme/widgets", "acme/tools"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("GitHubRepos = %v, want %v", got, want)
	}
}

func TestGitLabProjectsFromRemotes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", "https://github.com/acme/app.git"},
		{"remote", "add", "gitlab", "git@gitlab.com:acme-group/sub/app.git"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	projects := []domain.Project{
		{ID: "app", Paths: []string{repo, filepath.Join(repo, "missing")}},
		{ID: "other", RepoURL: "https://gitlab.com/acme-group/other.git"},
	}
	got := GitLabProjects(context.Background(), projects)
	if want := []string{"acme-group/other", "acme-group/sub/app"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("GitLabProjects = %v, want %v", got, want)
	}
}

func TestLoadMailAccounts(t *testing.T) {
	dir := t.TempDir()
	if accounts, err := LoadMailAccounts(dir); err != nil || accounts != nil {
		t.Fatalf("missing file = %v, %v", accounts, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "connectors"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := "\xef\xbb\xbf" + `{"accounts":[
		{"id":"box","host":"imap.example.test","username":"u","folders":["INBOX"],"secret":"legacy"},
		{"id":"","host":"h","username":"u"},
		{"id":"nohost","username":"u"}]}`
	if err := os.WriteFile(filepath.Join(dir, "connectors", "mail.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	accounts, err := LoadMailAccounts(dir)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts = %+v, %v", accounts, err)
	}
	imap := accounts[0].IMAPAccount()
	if imap.Account != "box" || imap.Address != "imap.example.test:993" || imap.Username != "u" || len(imap.Folders) != 1 {
		t.Fatalf("imap account = %+v", imap)
	}
}

type mapSecrets map[string]string

func (m mapSecrets) Get(_ context.Context, key string) (string, error) {
	if value, ok := m[key]; ok {
		return value, nil
	}
	return "", domain.ErrNotFound
}
func (m mapSecrets) Set(context.Context, string, string) error { return nil }
func (m mapSecrets) Delete(context.Context, string) error      { return nil }
func (m mapSecrets) Names(context.Context) ([]string, error)   { return nil, nil }

func TestGHTokenFallback(t *testing.T) {
	calls := 0
	store := ghTokenSecrets{
		SecretStore: mapSecrets{"gitlab/token": "gl"},
		lookPath:    func(string) (string, error) { return "gh", nil },
		output: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls++
			if name != "gh" || !reflect.DeepEqual(args, []string{"auth", "token"}) {
				t.Fatalf("unexpected command %s %v", name, args)
			}
			return []byte("gho_fromcli\n"), nil
		},
	}
	ctx := context.Background()
	if value, err := store.Get(ctx, "github/token"); err != nil || value != "gho_fromcli" {
		t.Fatalf("fallback = %q, %v", value, err)
	}
	if value, err := store.Get(ctx, "gitlab/token"); err != nil || value != "gl" || calls != 1 {
		t.Fatalf("other key = %q, %v (gh calls %d)", value, err, calls)
	}
	// A stored token wins and gh is not consulted.
	store.SecretStore = mapSecrets{"github/token": "stored"}
	if value, _ := store.Get(ctx, "github/token"); value != "stored" || calls != 1 {
		t.Fatalf("stored token = %q (gh calls %d)", value, calls)
	}
	// Without gh the original not-found error is returned.
	store.SecretStore = mapSecrets{}
	store.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	if _, err := store.Get(ctx, "github/token"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("no gh error = %v", err)
	}
}

func TestDecodeIMAPUTF7(t *testing.T) {
	for in, want := range map[string]string{
		"INBOX":                               "INBOX",
		"[Gmail]/&BBIEQQRP- &BD8EPgRHBEIEMA-": "[Gmail]/Вся почта",
		"&BB4EQgQ,BEAEMAQyBDsENQQ9BD0ESwQ1-":  "Отправленные",
		"A&-B":                                "A&B",
		"broken&abc":                          "broken&abc",
	} {
		if got := decodeIMAPUTF7(in); got != want {
			t.Errorf("decodeIMAPUTF7(%q) = %q, want %q", in, got, want)
		}
	}
}
