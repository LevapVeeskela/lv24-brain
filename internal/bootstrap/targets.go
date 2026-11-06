package bootstrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
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
