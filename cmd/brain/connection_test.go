package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/connectionlock"
	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sourceoauth"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/mcpserver"
	"github.com/levapveeskela/lv24-brain/internal/oauthlogin"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"gopkg.in/yaml.v3"
)

func connectionConfig(t *testing.T, mode string) (string, string) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.Mode, cfg.DataDir = mode, filepath.Join(root, "data")
	if mode == config.ModeServer {
		cfg.DataDir, cfg.ServerURL = "", "http://127.0.0.1:1"
	}
	path := filepath.Join(root, "brain.yaml")
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path, cfg.DataDir
}

func connectionJSON[T any](t *testing.T, out string) T {
	t.Helper()
	var envelope struct {
		Schema int `json:"schema"`
		Data   T   `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil || envelope.Schema != jsonSchemaVersion {
		t.Fatalf("invalid envelope: %v, %s", err, out)
	}
	return envelope.Data
}

func TestConnectionAddListRemove(t *testing.T) {
	for _, mode := range []string{config.ModeLocal, config.ModeGit} {
		for _, asJSON := range []bool{false, true} {
			t.Run(mode+"/json="+map[bool]string{false: "false", true: "true"}[asJSON], func(t *testing.T) {
				// Arrange: no git repository or index is needed for the catalogue.
				cfg, dataDir := connectionConfig(t, mode)
				runConn := func(args ...string) string {
					t.Helper()
					args = append([]string{"connection"}, args...)
					args = append(args, "--config", cfg)
					if asJSON {
						args = append(args, "--json")
					}
					code, out, errOut := runForTest(t, args...)
					if code != 0 {
						t.Fatalf("%v: exit %d: %s", args, code, errOut)
					}
					return out
				}
				out := runConn("list")
				if asJSON && len(connectionJSON[[]domain.Connection](t, out)) != 0 {
					t.Fatal(out)
				}

				// Act: flags are deliberately interspersed with the provider.
				out = runConn("add", "github", "--id", "work", "--account", "team", "--base-url", "https://api.github.com", "--project", "demo", "--project", "other", "--schedule", "@daily", "--publish", "local-only", "--setting", "repos=acme/widgets")
				if asJSON {
					conn := connectionJSON[domain.Connection](t, out)
					if conn.ID != "work" || conn.Publish != domain.PublishLocalOnly || conn.Backend != domain.BackendNative {
						t.Fatalf("add: %+v", conn)
					}
				} else if !strings.Contains(out, "brain connection secret set work token") {
					t.Fatalf("missing secret instructions: %s", out)
				}
				out = runConn("list")

				// Assert: list shows effective policy/backend and all requested metadata.
				if asJSON {
					list := connectionJSON[[]domain.Connection](t, out)
					if len(list) != 1 || list[0].Account != "team" || list[0].Schedule != "@daily" || !reflect.DeepEqual(list[0].Projects, []string{"demo", "other"}) {
						t.Fatalf("list: %+v", list)
					}
				} else {
					for _, want := range []string{"work", "github", "team", "https://api.github.com", "demo", "other", "@daily", "local-only", "native"} {
						if !strings.Contains(out, want) {
							t.Errorf("list missing %q: %s", want, out)
						}
					}
				}
				itemPath := filepath.Join(dataDir, "connectors", "work", "items", "keep.md")
				if err := os.MkdirAll(filepath.Dir(itemPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(itemPath, []byte("kept"), 0600); err != nil {
					t.Fatal(err)
				}
				code, _, errOut := runForTest(t, "connection", "remove", "work", "--config", cfg)
				if code != 2 || !strings.Contains(errOut, "--yes") {
					t.Fatalf("unconfirmed remove: %d %s", code, errOut)
				}
				out = runConn("remove", "work", "--yes")
				if asJSON {
					_ = connectionJSON[json.RawMessage](t, out)
				} else if !strings.Contains(out, filepath.Join("connectors", "work")) || !strings.Contains(out, "purge") {
					t.Fatal(out)
				}
				if raw, err := os.ReadFile(itemPath); err != nil || string(raw) != "kept" {
					t.Fatalf("items removed: %v", err)
				}
				out = runConn("list")
				if asJSON && len(connectionJSON[[]domain.Connection](t, out)) != 0 {
					t.Fatal(out)
				}
			})
		}
	}
}

func TestConnectionRemoveRefusesWhileRunHoldsLock(t *testing.T) {
	cfg, dataDir := connectionConfig(t, config.ModeLocal)
	code, _, errOut := runForTest(t, "connection", "add", "github", "--id", "work", "--setting", "repos=acme/widgets", "--config", cfg)
	if code != 0 {
		t.Fatalf("add: %d %s", code, errOut)
	}
	locker, err := connectionlock.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	release, err := locker.TryLock(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	code, _, errOut = runForTest(t, "connection", "remove", "work", "--yes", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "busy") {
		t.Fatalf("remove while locked = %d %s", code, errOut)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = runForTest(t, "connection", "remove", "work", "--yes", "--config", cfg)
	if code != 0 {
		t.Fatalf("remove after release = %d %s", code, errOut)
	}
}

func TestAgentStatusAndStopWithoutRunningAgent(t *testing.T) {
	cfg, _ := connectionConfig(t, config.ModeLocal)
	code, out, errOut := runForTest(t, "agent", "status", "--config", cfg)
	if code != 0 || !strings.Contains(out, "not running") {
		t.Fatalf("agent status = %d %q %q", code, out, errOut)
	}
	code, out, errOut = runForTest(t, "agent", "status", "--json", "--config", cfg)
	if code != 0 || !strings.Contains(out, `"running": false`) {
		t.Fatalf("agent status json = %d %q %q", code, out, errOut)
	}
	code, _, errOut = runForTest(t, "agent", "stop", "--config", cfg)
	if code != 0 {
		t.Fatalf("agent stop without runtime = %d %q", code, errOut)
	}
}

func TestConnectionValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"provider", []string{"add", "unknown", "--id", "work"}, "unknown provider"},
		{"secret", []string{"add", "github", "--id", "work", "--setting", "repos=acme/widgets", "--setting", "token=never-print-me"}, "brain connection secret set work token"},
		{"unknown setting", []string{"add", "github", "--id", "work", "--setting", "nope=never-print-me"}, "nope"},
		{"bad id", []string{"add", "github", "--id", "../bad", "--setting", "repos=acme/widgets"}, "id"},
		{"reserved id", []string{"add", "github", "--id", "runs", "--setting", "repos=acme/widgets"}, "reserved"},
		{"missing setting", []string{"add", "github", "--id", "work"}, "repos"},
		{"bad setting", []string{"add", "github", "--id", "work", "--setting", "no-equals"}, "key=value"},
		{"duplicate setting", []string{"add", "github", "--id", "work", "--setting", "repos=a/b", "--setting", "repos=c/d"}, "duplicate setting"},
		{"bad publish", []string{"add", "github", "--id", "work", "--setting", "repos=acme/widgets", "--publish", "public"}, "publish"},
		{"bad schedule", []string{"add", "github", "--id", "work", "--setting", "repos=acme/widgets", "--schedule", "@every 1s"}, "schedule"},
		{"extra argument", []string{"list", "extra"}, "arguments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, dataDir := connectionConfig(t, config.ModeLocal)
			args := append([]string{"connection"}, tc.args...)
			args = append(args, "--config", cfg)
			code, out, errOut := runForTest(t, args...)
			if code != 2 || !strings.Contains(errOut, tc.want) || strings.Contains(out+errOut, "never-print-me") {
				t.Fatalf("exit=%d out=%s err=%s", code, out, errOut)
			}
			store, err := fsstore.NewConnectionStore(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			list, err := store.List(context.Background())
			if err != nil || len(list) != 0 {
				t.Fatalf("invalid add wrote catalogue: %+v %v", list, err)
			}
		})
	}
}

func TestConnectionProviders(t *testing.T) {
	cfg, _ := connectionConfig(t, config.ModeLocal)
	for _, asJSON := range []bool{false, true} {
		args := []string{"connection", "providers", "--config", cfg}
		if asJSON {
			args = append(args, "--json")
		}
		code, out, errOut := runForTest(t, args...)
		if code != 0 {
			t.Fatalf("providers: %d %s", code, errOut)
		}
		if asJSON {
			got := connectionJSON[[]domain.SourceDescriptor](t, out)
			if !reflect.DeepEqual(got, connectorbuild.Builtin().Descriptors()) {
				t.Fatalf("descriptors: %+v", got)
			}
		} else {
			for _, want := range []string{"github", "GitHub", "gitlab", "imap", "auth=token", "required", "secret", "local-only", "shared"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q: %s", want, out)
				}
			}
		}
	}
}

func TestConnectionProviderDefaultsAndSafeOutput(t *testing.T) {
	cfg, dataDir := connectionConfig(t, config.ModeLocal)
	cases := []struct {
		provider, id string
		settings     []string
		publish      domain.PublishPolicy
	}{
		{"github", "gh", []string{"repos=a/b"}, domain.PublishShared},
		{"gitlab", "gl", []string{"projects=a/b"}, domain.PublishShared},
		{"imap", "mail", []string{"username=reader", "folders=INBOX"}, domain.PublishLocalOnly},
	}
	for _, tc := range cases {
		args := []string{"connection", "add", tc.provider, "--id", tc.id, "--json", "--config", cfg}
		for _, setting := range tc.settings {
			args = append(args, "--setting", setting)
		}
		code, out, errOut := runForTest(t, args...)
		if code != 0 {
			t.Fatal(errOut)
		}
		view := connectionJSON[domain.Connection](t, out)
		if view.Publish != tc.publish || view.Backend != domain.BackendNative || view.Schedule != "manual" {
			t.Fatalf("defaults: %+v", view)
		}
		entry, err := connectorbuild.Builtin().Lookup(tc.provider)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range entry.Descriptor.ConfigFields {
			if field.Secret && !strings.Contains(errOut, "brain connection secret set "+tc.id+" "+field.Key) {
				t.Fatalf("JSON must keep hints on stderr: %s", errOut)
			}
		}
	}
	// A hand-edited catalogue cannot make list print secret provider settings.
	store, err := fsstore.NewConnectionStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := store.Get(context.Background(), "gh")
	if err != nil {
		t.Fatal(err)
	}
	conn.Settings["token"] = "sensitive-value"
	if err := store.Update(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
	for _, asJSON := range []bool{false, true} {
		args := []string{"connection", "list", "--config", cfg}
		if asJSON {
			args = append(args, "--json")
		}
		code, out, errOut := runForTest(t, args...)
		if code != 0 || strings.Contains(out+errOut, "sensitive-value") {
			t.Fatalf("list: %d %s %s", code, out, errOut)
		}
	}
}

func TestConnectionMCPItemDescription(t *testing.T) {
	// tools/list needs no backend, filesystem or network.
	server := mcpserver.New(nil, "test")
	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				InputSchema struct {
					Properties map[string]struct {
						Description string `json:"description"`
					} `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	for _, tool := range response.Result.Tools {
		if tool.Name != "brain_search" {
			continue
		}
		if !strings.Contains(tool.Description, "Raw-zone external items are excluded unless kinds contains \"item\"") || !strings.Contains(tool.InputSchema.Properties["kinds"].Description, "item") {
			t.Fatalf("search descriptions: %+v", tool)
		}
		return
	}
	t.Fatal("brain_search tool missing")
}

func TestConnectionServerModeRefusesEveryCommand(t *testing.T) {
	cfg, _ := connectionConfig(t, config.ModeServer)
	for _, args := range [][]string{{"list"}, {"providers"}, {"add", "github", "--id", "work", "--setting", "repos=a/b"}, {"remove", "work", "--yes"}, {"migrate"}, {"secret", "set", "work", "token"}} {
		args = append(append([]string{"connection"}, args...), "--config", cfg)
		code, out, errOut := runWithInputForTest(t, "never-print-me\n", args...)
		if code != 2 || !strings.Contains(errOut, "not available in server mode yet") || strings.Contains(out+errOut, "never-print-me") {
			t.Errorf("%v: %d %s %s", args, code, out, errOut)
		}
	}
}

func TestConnectionCompletion(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			code, out, errOut := runForTest(t, "completion", shell)
			if code != 0 {
				t.Fatal(errOut)
			}
			for _, want := range []string{"connection", "providers", "migrate", "copy-legacy-secrets", "connection secret set", "token", "password"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q", want)
				}
			}
			for _, provider := range connectorbuild.Builtin().Providers() {
				if !strings.Contains(out, provider) {
					t.Errorf("missing %s", provider)
				}
			}
		})
	}
}

func TestConnectionCompletionNestedPositions(t *testing.T) {
	// Arrange: providers and secret field choices come from the descriptors.
	providers := connectorbuild.Builtin().Providers()
	fields := connectionSecretFields()
	leaves := map[string]completionLeaf{}
	for _, leaf := range completionLeaves() {
		leaves[leaf.key()] = leaf
	}

	// Act/Assert: the third command word is preserved and argument offsets match.
	add, set := leaves["connection add"], leaves["connection secret set"]
	if add.pos == nil || add.pos.index != 1 || !reflect.DeepEqual(add.pos.values, providers) {
		t.Fatalf("provider completion: %+v", add)
	}
	if set.pos == nil || len(set.path) != 3 || set.pos.index != 2 || !reflect.DeepEqual(set.pos.values, fields) {
		t.Fatalf("field completion: %+v", set)
	}
	if !strings.Contains(renderZsh(), "'5:field:("+strings.Join(fields, " ")+")'") {
		t.Fatal("zsh field position must follow connection secret set and the id")
	}
	settings := connectionSettingChoices()
	for _, descriptor := range connectorbuild.Builtin().Descriptors() {
		for _, field := range descriptor.ConfigFields {
			if containsString(settings, field.Key+"=") == field.Secret {
				t.Errorf("incorrect --setting choice for %s", field.Key)
			}
		}
	}
}

func TestConnectionPowerShellCompletion(t *testing.T) {
	ps, err := exec.LookPath("powershell")
	if err != nil {
		t.Skip("powershell is not on PATH")
	}
	path := filepath.Join(t.TempDir(), "completion.ps1")
	script := renderPowerShell() + `
$cases = @{
    'brain connection ' = @('add', 'list', 'remove', 'migrate', 'providers', 'secret')
    'brain connection secret ' = @('set')
    'brain connection add g' = @('github', 'gitlab')
    'brain connection secret set work t' = @('token')
    'brain connection add github --setting r' = @('repos=')
}
foreach ($line in $cases.Keys) {
    $result = @(TabExpansion2 -inputScript $line -cursorColumn $line.Length)
    $actual = @($result.CompletionMatches | ForEach-Object { $_.CompletionText })
    foreach ($want in $cases[$line]) { if ($actual -notcontains $want) { throw "Missing $want for $line; got $actual" } }
}
`
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path).CombinedOutput(); err != nil {
		t.Fatalf("nested completion: %v\n%s", err, out)
	}
}

func TestConnectionItemsSearchAndIndex(t *testing.T) {
	cfg, dataDir := connectionConfig(t, config.ModeLocal)
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.PutProject(ctx, domain.Project{ID: "demo", Name: "Demo"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	item := domain.Item{Ref: domain.ItemRef{ConnectionID: "work", ExternalID: "1"}, Kind: domain.ItemIssue, Title: "uniquerawitem", Body: "uniquerawitem", ProjectID: "demo", UpdatedAt: now, ObservedAt: now}.WithHash()
	if _, err := store.Items().Upsert(ctx, item); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runForTest(t, "index", "--config", cfg)
	if code != 0 || !strings.Contains(out, "items: 1\n") {
		t.Fatalf("index: %d %s %s", code, out, errOut)
	}
	code, out, errOut = runForTest(t, "search", "uniquerawitem", "--kind", "item", "--json", "--config", cfg)
	if code != 0 || !strings.Contains(out, `"kind": "item"`) {
		t.Fatalf("search: %d %s %s", code, out, errOut)
	}
	code, out, errOut = runForTest(t, "search", "uniquerawitem", "--json", "--config", cfg)
	if code != 0 || strings.Contains(out, `"kind": "item"`) {
		t.Fatalf("default search: %d %s %s", code, out, errOut)
	}
}

func connectionSecrets(t *testing.T, store ports.SecretStore) {
	t.Helper()
	previous := buildConnections
	buildConnections = func(cfg config.BrainConfig, opts bootstrap.Options) (*bootstrap.ConnectionRuntime, error) {
		opts.Secrets = store
		return bootstrap.BuildConnections(cfg, opts)
	}
	t.Cleanup(func() { buildConnections = previous })
}

func TestConnectionSecretSet(t *testing.T) {
	cfg, dataDir := connectionConfig(t, config.ModeLocal)
	secrets := newCLISecretStore()
	connectionSecrets(t, secrets)
	code, _, errOut := runForTest(t, "connection", "add", "github", "--id", "work", "--setting", "repos=a/b", "--config", cfg)
	if code != 0 {
		t.Fatal(errOut)
	}
	for _, value := range []string{"sensitive-value\r\n", "sensitive-value"} {
		code, out, errOut := runWithInputForTest(t, value, "connection", "secret", "set", "work", "token", "--config", cfg)
		if code != 0 || strings.Contains(out+errOut, "sensitive-value") || !strings.Contains(out, "conn/work/token") {
			t.Fatalf("secret: %d %s %s", code, out, errOut)
		}
		if got, err := secrets.Get(context.Background(), "conn/work/token"); err != nil || got != "sensitive-value" {
			t.Fatalf("stored: %q %v", got, err)
		}
	}
	for _, field := range []string{"repos", "unknown"} {
		code, out, errOut := runWithInputForTest(t, "sensitive-value\n", "connection", "secret", "set", "work", field, "--config", cfg)
		if code != 2 || !strings.Contains(errOut, "Secret config field") || strings.Contains(out+errOut, "sensitive-value") {
			t.Errorf("field %s: %d %s %s", field, code, out, errOut)
		}
	}
	code, _, _ = runWithInputForTest(t, "\n", "connection", "secret", "set", "work", "token", "--config", cfg)
	if code != 1 {
		t.Fatalf("empty stdin exit=%d", code)
	}
	if raw, err := os.ReadFile(filepath.Join(dataDir, "connectors", "connections.json")); err != nil || strings.Contains(string(raw), "sensitive-value") {
		t.Fatalf("catalogue leaked credential: %v", err)
	}
	connectionSecrets(t, connectionSecretError{SecretStore: secrets})
	code, out, errOut := runWithInputForTest(t, "sensitive-value\n", "connection", "secret", "set", "work", "token", "--config", cfg)
	if code != 1 || strings.Contains(out+errOut, "sensitive-value") {
		t.Fatalf("backend error leaked: %d %s %s", code, out, errOut)
	}
}

type connectionSecretError struct{ ports.SecretStore }

func (s connectionSecretError) Set(context.Context, string, string) error {
	return errors.New("backend echoed sensitive-value")
}

func TestConnectionMigrate(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[asJSON], func(t *testing.T) {
			cfg, dataDir := connectionConfig(t, config.ModeLocal)
			for _, key := range []string{"BRAIN_GITHUB_REPOS", "BRAIN_GITHUB_BASE_URL", "BRAIN_GITLAB_PROJECTS", "BRAIN_GITLAB_BASE_URL", "BRAIN_IMAP_ACCOUNT", "BRAIN_IMAP_ADDRESS", "BRAIN_IMAP_USERNAME", "BRAIN_IMAP_PASSWORD_KEY", "BRAIN_IMAP_FOLDERS"} {
				t.Setenv(key, "")
			}
			store, err := fsstore.New(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.PutProject(context.Background(), domain.Project{ID: "demo", Name: "Demo", RepoURL: "https://github.com/acme/widgets"}); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dataDir, "connectors"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dataDir, "connectors", "mail.json"), []byte(`{"accounts":[{"id":"work","host":"mail.example.test","port":993,"username":"reader","folders":["INBOX"]}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			secrets := newCLISecretStore()
			if err := secrets.Set(context.Background(), "github/token", "legacy-value"); err != nil {
				t.Fatal(err)
			}
			if err := secrets.Set(context.Background(), "imap/work/password", "legacy-mail-value"); err != nil {
				t.Fatal(err)
			}
			connectionSecrets(t, secrets)
			runMigration := func(copySecrets bool, created, skipped int, status string) {
				t.Helper()
				args := []string{"connection", "migrate", "--config", cfg}
				if asJSON {
					args = append(args, "--json")
				}
				if copySecrets {
					args = append(args, "--copy-legacy-secrets")
				}
				code, out, errOut := runForTest(t, args...)
				if code != 0 || strings.Contains(out+errOut, "legacy-value") || strings.Contains(out+errOut, "legacy-mail-value") || strings.Contains(out+errOut, "sensitive-value") {
					t.Fatalf("migrate: %d %s %s", code, out, errOut)
				}
				if asJSON {
					report := connectionJSON[struct {
						Created     []domain.Connection             `json:"created"`
						Skipped     []domain.Connection             `json:"skipped"`
						Credentials []app.LegacyCredentialMigration `json:"credentials"`
					}](t, out)
					if len(report.Created) != created || len(report.Skipped) != skipped || len(report.Credentials) != 2 {
						t.Fatalf("report: %+v", report)
					}
					for _, credential := range report.Credentials {
						if credential.Status != status {
							t.Errorf("credential: %+v", credential)
						}
					}
				} else {
					for _, want := range []string{fmt.Sprintf("created: %d", created), fmt.Sprintf("skipped: %d", skipped), "github/token", "imap/work/password", status} {
						if !strings.Contains(out, want) {
							t.Errorf("missing %q: %s", want, out)
						}
					}
				}
			}
			runMigration(false, 2, 0, "not-copied")
			connections, err := fsstore.NewConnectionStore(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			existing, err := connections.Get(context.Background(), "github")
			if err != nil {
				t.Fatal(err)
			}
			existing.Settings["token"] = "sensitive-value"
			if err := connections.Update(context.Background(), existing); err != nil {
				t.Fatal(err)
			}
			if _, err := secrets.Get(context.Background(), "conn/github/token"); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("copied without opt-in: %v", err)
			}
			runMigration(true, 0, 2, "copied")
			if got, err := secrets.Get(context.Background(), "conn/github/token"); err != nil || got != "legacy-value" {
				t.Fatalf("copy: %q %v", got, err)
			}
			runMigration(true, 0, 2, "already-set")
			if got, err := secrets.Get(context.Background(), "github/token"); err != nil || got != "legacy-value" {
				t.Fatalf("legacy changed: %q %v", got, err)
			}
		})
	}
}

func TestConnectionAuthRemoveAndUnsupportedProvider(t *testing.T) {
	cfg, _ := connectionConfig(t, config.ModeLocal)
	secrets := newCLISecretStore()
	connectionSecrets(t, secrets)
	if code, _, stderr := runForTest(t, "connection", "add", "github", "--id", "work", "--setting", "repos=a/b", "--config", cfg); code != 0 {
		t.Fatalf("add github: %s", stderr)
	}
	if code, _, stderr := runForTest(t, "connection", "add", "folders", "--id", "notes", "--setting", "paths="+t.TempDir(), "--config", cfg); code != 0 {
		t.Fatalf("add folders: %s", stderr)
	}
	store := &oauthlogin.SessionStore{Secrets: secrets}
	ctx := context.Background()
	if err := store.Save(ctx, "work", oauthlogin.Session{
		Provider: "github", ClientID: "client-id",
		Bundle: sourceoauth.Bundle{AccessToken: "access-token"},
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	code, out, stderr := runForTest(t, "connection", "auth", "work", "--remove", "--config", cfg)
	if code != 0 || !strings.Contains(out, "OAuth session removed") {
		t.Fatalf("auth remove = %d %q %s", code, out, stderr)
	}
	for _, key := range []string{"conn/work/oauth", "conn/work/oauthmeta"} {
		if _, err := secrets.Get(ctx, key); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("key %s was not removed: %v", key, err)
		}
	}
	code, _, stderr = runForTest(t, "connection", "auth", "notes", "--client-id", "client", "--config", cfg)
	if code == 0 || !strings.Contains(stderr, "OAuth") {
		t.Fatalf("unsupported provider auth = %d %s", code, stderr)
	}
}
