package bootstrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/levapveeskela/lv24-brain/internal/adapters/imap"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// MailConfigPath is the legacy-compatible IMAP account list inside the data
// directory. It holds account ids, hosts, usernames and folders; passwords
// live in the OS secret store under imap/<account>/password.
const MailConfigPath = "connectors/mail.json"

// MailAccount is one IMAP account from connectors/mail.json.
type MailAccount struct {
	ID       string   `json:"id"`
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Username string   `json:"username"`
	Folders  []string `json:"folders"`
	// Days bounds the initial window when no --since/--days is given.
	Days int `json:"days"`
}

// LoadMailAccounts reads connectors/mail.json from dataDir. A missing file
// yields no accounts and no error.
func LoadMailAccounts(dataDir string) ([]MailAccount, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(MailConfigPath)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	var file struct {
		Accounts []MailAccount `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", MailConfigPath, err)
	}
	out := make([]MailAccount, 0, len(file.Accounts))
	for _, account := range file.Accounts {
		account.ID = strings.TrimSpace(account.ID)
		if account.ID == "" || strings.TrimSpace(account.Host) == "" || strings.TrimSpace(account.Username) == "" {
			continue
		}
		if account.Port == 0 {
			account.Port = 993
		}
		for i, folder := range account.Folders {
			account.Folders[i] = decodeIMAPUTF7(folder)
		}
		out = append(out, account)
	}
	return out, nil
}

// IMAPAccount converts a mail.json account to connector settings.
func (m MailAccount) IMAPAccount() *connectorbuild.IMAPAccount {
	return &connectorbuild.IMAPAccount{
		Account:  m.ID,
		Address:  net.JoinHostPort(strings.TrimSpace(m.Host), strconv.Itoa(m.Port)),
		Username: strings.TrimSpace(m.Username),
		Folders:  m.Folders,
	}
}

// InitialSince applies the mail.json initial import window only when the CLI
// has not supplied an explicit --since or --days cutoff. Non-positive Days
// preserves the legacy unbounded window. The clock is supplied by the caller.
func (m MailAccount) InitialSince(explicit *time.Time, now time.Time) *time.Time {
	if explicit != nil || m.Days <= 0 {
		return explicit
	}
	cutoff := now.UTC().AddDate(0, 0, -m.Days)
	return &cutoff
}

// ImportSince applies Days only before an account has a persisted cursor.
// Once any folder has been imported, the UID cursor must be allowed to catch
// up even after downtime longer than Days. The legacy run contract has only
// one Since for all folders, so newly added folders then backfill unbounded.
func (m MailAccount) ImportSince(explicit *time.Time, now time.Time, statePath string) (*time.Time, error) {
	if explicit != nil || m.Days <= 0 {
		return explicit, nil
	}
	state, err := imap.NewUIDStateStore(statePath).Load()
	if err != nil {
		return nil, err
	}
	for _, cursor := range state.Accounts[m.ID].Folders {
		if cursor.UIDValidity != 0 || cursor.LastUID != 0 {
			return nil, nil
		}
	}
	return m.InitialSince(nil, now), nil
}

// LegacyConnectorOptions supplies the legacy discovery inputs. Nil Getenv and
// RemoteURL select the process environment and local git remote reads. No
// discovery operation contacts a provider or reads credential values.
type LegacyConnectorOptions struct {
	DataDir   string
	Projects  []domain.Project
	Getenv    func(string) string
	RemoteURL func(ctx context.Context, directory, remote string) (string, error)
}

// DiscoverLegacyConnectors reads registry targets, GitLab checkout remotes,
// mail.json and environment overrides for ConnectionMigrator. Unlike the old
// run helpers, it retains host identity for self-hosted GitLab/GitHub URLs.
// Env repository/project lists replace the respective discovered lists; an
// explicit IMAP account replaces a matching mailbox or adds another mailbox.
func DiscoverLegacyConnectors(ctx context.Context, opts LegacyConnectorOptions) ([]app.LegacyConnection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	remoteURL := opts.RemoteURL
	if remoteURL == nil {
		remoteURL = legacyRemoteURL
	}
	bases := map[string]string{
		"github": strings.TrimSpace(getenv("BRAIN_GITHUB_BASE_URL")),
		"gitlab": strings.TrimSpace(getenv("BRAIN_GITLAB_BASE_URL")),
	}
	var targets []app.LegacyConnection
	addRepo := func(raw, projectID, hint string) error {
		provider, base, path := legacyRepository(raw, hint, bases)
		if provider == "" {
			return nil
		}
		if strings.Contains(path, ",") {
			return domain.NewValidationError("migration.repository", "repository paths containing commas cannot be represented by the legacy descriptor")
		}
		if bases[provider] != "" {
			base = bases[provider]
		}
		field := "repos"
		if provider == "gitlab" {
			field = "projects"
		}
		targets = append(targets, app.LegacyConnection{
			Connection:    domain.Connection{Provider: provider, BaseURL: base, Projects: []string{projectID}, Settings: map[string]string{field: path}},
			LegacySecrets: map[string]string{"token": provider + "/token"},
		})
		return nil
	}
	for _, project := range opts.Projects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := addRepo(project.RepoURL, project.ID, ""); err != nil {
			return nil, err
		}
		for _, directory := range project.Paths {
			if info, err := os.Stat(directory); err != nil || !info.IsDir() {
				continue
			}
			for _, remote := range []string{"gitlab", "origin"} {
				raw, err := remoteURL(ctx, directory, remote)
				if err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					continue // Missing/unavailable checkout remotes are legacy best-effort.
				}
				hint := ""
				if remote == "gitlab" {
					hint = "gitlab"
				}
				provider, _, _ := legacyRepository(raw, hint, bases)
				if provider == "gitlab" {
					if err := addRepo(raw, project.ID, hint); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	for _, provider := range []string{"github", "gitlab"} {
		field, env := "repos", "BRAIN_GITHUB_REPOS"
		if provider == "gitlab" {
			field, env = "projects", "BRAIN_GITLAB_PROJECTS"
		}
		values := legacyTargetList(getenv(env))
		if len(values) == 0 {
			continue
		}
		kept := make([]app.LegacyConnection, 0, len(targets)+1)
		for _, target := range targets {
			if target.Connection.Provider != provider {
				kept = append(kept, target)
			}
		}
		base := bases[provider]
		if base == "" {
			base = legacyProviderBase(provider, "https", provider+".com")
		}
		targets = append(kept, app.LegacyConnection{
			Connection:    domain.Connection{Provider: provider, BaseURL: base, Settings: map[string]string{field: strings.Join(values, ",")}},
			LegacySecrets: map[string]string{"token": provider + "/token"},
		})
	}
	accounts, err := LoadMailAccounts(opts.DataDir)
	if err != nil {
		return nil, err
	}
	envAccount := strings.TrimSpace(getenv("BRAIN_IMAP_ACCOUNT"))
	for _, account := range accounts {
		if envAccount != "" && strings.EqualFold(envAccount, account.ID) {
			continue
		}
		if account.Port < 1 || account.Port > 65535 {
			return nil, domain.NewValidationError("migration.imap.port", "must be between 1 and 65535")
		}
		address := net.JoinHostPort(strings.Trim(strings.TrimSpace(account.Host), "[]"), strconv.Itoa(account.Port))
		target, err := legacyMailConnection(account.ID, address, account.Username, account.Folders, "")
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	if envAccount != "" {
		target, err := legacyMailConnection(envAccount, strings.TrimSpace(getenv("BRAIN_IMAP_ADDRESS")), getenv("BRAIN_IMAP_USERNAME"), legacyTargetList(getenv("BRAIN_IMAP_FOLDERS")), strings.TrimSpace(getenv("BRAIN_IMAP_PASSWORD_KEY")))
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func legacyRemoteURL(ctx context.Context, directory, remote string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(runCtx, "git", "-C", directory, "remote", "get-url", remote).Output()
	return strings.TrimSpace(string(raw)), err
}

// legacyRepository recognises public/vendor-named hosts and explicit base-URL
// hosts. A remote named gitlab also identifies arbitrary self-hosted GitLab.
// SSH users/ports are transport details, not REST account/endpoint identity.
func legacyRepository(raw, hint string, bases map[string]string) (provider, base, path string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", ""
	}
	if !strings.Contains(raw, "://") {
		host, rest, ok := strings.Cut(raw, ":")
		if !ok || !strings.Contains(host, "@") {
			return "", "", ""
		}
		raw = "ssh://" + host + "/" + rest
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", ""
	}
	host := strings.ToLower(parsed.Hostname())
	provider = hint
	if provider == "" {
		for _, candidate := range []string{"github", "gitlab"} {
			override, _ := url.Parse(bases[candidate])
			if host == candidate+".com" || strings.HasPrefix(host, candidate+".") || (override != nil && override.Hostname() != "" && strings.EqualFold(override.Hostname(), host)) {
				provider = candidate
				break
			}
		}
	}
	path = strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git")
	if provider == "" || path == "" {
		return "", "", ""
	}
	scheme, authority := "https", host
	if parsed.Scheme == "https" || parsed.Scheme == "http" {
		scheme, authority = parsed.Scheme, strings.ToLower(parsed.Host)
	}
	return provider, legacyProviderBase(provider, scheme, authority), path
}

func legacyProviderBase(provider, scheme, host string) string {
	if provider == "github" {
		if host == "github.com" {
			return "https://api.github.com"
		}
		return scheme + "://" + host + "/api/v3"
	}
	return scheme + "://" + host + "/api/v4"
}

func legacyTargetList(raw string) []string {
	var result []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func legacyMailConnection(account, address, username string, folders []string, passwordKey string) (app.LegacyConnection, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || strings.TrimSpace(host) == "" {
		return app.LegacyConnection{}, domain.NewValidationError("migration.imap.address", "must be host:port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return app.LegacyConnection{}, domain.NewValidationError("migration.imap.port", "must be between 1 and 65535")
	}
	for _, folder := range folders {
		if strings.Contains(folder, ",") {
			return app.LegacyConnection{}, domain.NewValidationError("migration.imap.folders", "folder names containing commas cannot be represented by the legacy descriptor")
		}
	}
	if len(folders) == 0 {
		folders = []string{"INBOX"}
	}
	if passwordKey == "" {
		passwordKey = "imap/" + connectors.Slug(account) + "/password"
	}
	return app.LegacyConnection{
		Connection:    domain.Connection{Provider: "imap", Account: account, BaseURL: "imaps://" + net.JoinHostPort(strings.ToLower(host), strconv.Itoa(portNumber)), Publish: domain.PublishLocalOnly, Settings: map[string]string{"username": strings.TrimSpace(username), "folders": strings.Join(folders, ",")}},
		LegacySecrets: map[string]string{"password": passwordKey},
	}, nil
}

var (
	githubRepoPattern = regexp.MustCompile(`(?i)github\.com[:/]+([^/\s]+)/([^/\s]+?)(?:\.git)?/?$`)
	gitlabRepoPattern = regexp.MustCompile(`(?i)gitlab\.com[:/]+(.+?)(?:\.git)?/?$`)
)

// GitHubRepos lists unique owner/name repositories from registered projects'
// repo URLs, sorted.
func GitHubRepos(projects []domain.Project) []string {
	seen := map[string]bool{}
	var out []string
	for _, project := range projects {
		match := githubRepoPattern.FindStringSubmatch(strings.TrimSpace(project.RepoURL))
		if match == nil {
			continue
		}
		repo := match[1] + "/" + match[2]
		if key := strings.ToLower(repo); !seen[key] {
			seen[key] = true
			out = append(out, repo)
		}
	}
	sort.Strings(out)
	return out
}

// GitLabProjects lists unique gitlab.com project paths found in registered
// projects' repo URLs and in the "gitlab" and "origin" remotes of their
// checkouts (the legacy tool used the same remotes), sorted.
func GitLabProjects(ctx context.Context, projects []domain.Project) []string {
	seen := map[string]bool{}
	var out []string
	add := func(url string) {
		match := gitlabRepoPattern.FindStringSubmatch(strings.TrimSpace(url))
		if match == nil {
			return
		}
		path := strings.Trim(match[1], "/")
		if key := strings.ToLower(path); path != "" && !seen[key] {
			seen[key] = true
			out = append(out, path)
		}
	}
	for _, project := range projects {
		add(project.RepoURL)
		for _, dir := range project.Paths {
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				continue
			}
			for _, remote := range []string{"gitlab", "origin"} {
				runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				url, err := exec.CommandContext(runCtx, "git", "-C", dir, "remote", "get-url", remote).Output()
				cancel()
				if err == nil {
					add(string(url))
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// ghTokenSecrets falls back to the GitHub CLI's login for github/token when
// the secret store has no value, so a machine already signed in with `gh`
// needs no second copy of the token. The token is read per run and never
// persisted.
type ghTokenSecrets struct {
	ports.SecretStore
	lookPath func(string) (string, error)
	output   func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func withGHTokenFallback(inner ports.SecretStore) ports.SecretStore {
	return ghTokenSecrets{
		SecretStore: inner,
		lookPath:    exec.LookPath,
		output: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		},
	}
}

// githubTokenSource reads github/token (the OS secret, else the gh CLI
// login) for update checks. It runs only when an update request is made, so
// ordinary commands never spawn gh.
func githubTokenSource(store ports.SecretStore) func(context.Context) (string, error) {
	fallback := withGHTokenFallback(store)
	return func(ctx context.Context) (string, error) { return fallback.Get(ctx, "github/token") }
}

func (s ghTokenSecrets) Get(ctx context.Context, key string) (string, error) {
	value, err := s.SecretStore.Get(ctx, key)
	if key != "github/token" || (err == nil && strings.TrimSpace(value) != "") {
		return value, err
	}
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return value, err
	}
	if _, lookErr := s.lookPath("gh"); lookErr != nil {
		return value, err
	}
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, ghErr := s.output(runCtx, "gh", "auth", "token")
	if token := strings.TrimSpace(string(out)); ghErr == nil && token != "" {
		return token, nil
	}
	return value, err
}

// decodeIMAPUTF7 converts an RFC 3501 modified UTF-7 mailbox name (as stored by
// the legacy tool, e.g. "&BBIEQQRP-") to UTF-8; the IMAP library encodes names
// itself, so passing the encoded form would double-encode it. Invalid input is
// returned unchanged.
func decodeIMAPUTF7(name string) string {
	if !strings.Contains(name, "&") {
		return name
	}
	var out strings.Builder
	for i := 0; i < len(name); {
		if name[i] != '&' {
			out.WriteByte(name[i])
			i++
			continue
		}
		end := strings.IndexByte(name[i+1:], '-')
		if end < 0 {
			return name
		}
		chunk := name[i+1 : i+1+end]
		i += end + 2
		if chunk == "" {
			out.WriteByte('&')
			continue
		}
		raw, err := base64.RawStdEncoding.DecodeString(strings.ReplaceAll(chunk, ",", "/"))
		if err != nil || len(raw)%2 != 0 {
			return name
		}
		units := make([]uint16, len(raw)/2)
		for j := range units {
			units[j] = uint16(raw[2*j])<<8 | uint16(raw[2*j+1])
		}
		out.WriteString(string(utf16.Decode(units)))
	}
	return out.String()
}
