// Package github implements a read-only GitHub connector. It only issues GET
// requests, resolves the token from ports.SecretStore at run time, and writes
// deterministic Markdown dumps through connectors.MarkdownSink.
package github

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// Name is the connector source name.
	Name = "github"
	// DefaultBaseURL is the public GitHub REST API.
	DefaultBaseURL = "https://api.github.com"
	// DefaultTokenKey is the default secret-store key.
	DefaultTokenKey = "github/token"
	// DefaultUserAgent identifies the engine to GitHub.
	DefaultUserAgent = "lv24-brain"
	// DefaultLimit is the per-collection item limit.
	DefaultLimit = 25
	// MaxLimit is the largest supported per-collection item limit.
	MaxLimit = 1000
)

// Config configures the GitHub connector.
type Config struct {
	BaseURL    string
	TokenKey   string
	UserAgent  string
	Repos      []string
	Secrets    ports.SecretStore
	Sink       connectors.MarkdownSink
	HTTPClient *http.Client
	Retry      connectors.RetryPolicy
	Now        func() time.Time
}

// Connector reads repository metadata, pull requests, issues and recent
// commits from GitHub.
type Connector struct {
	baseURL   string
	tokenKey  string
	userAgent string
	repos     []string
	secrets   ports.SecretStore
	sink      connectors.MarkdownSink
	http      *connectors.HTTP
	now       func() time.Time
}

// isAllowedBaseURL reports whether u may carry the PAT: https always, http only
// when the host is loopback so local test servers keep working.
func isAllowedBaseURL(u *url.URL) bool {
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}

// New validates cfg and builds a connector.
func New(cfg Config) (*Connector, error) {
	if cfg.Secrets == nil {
		return nil, domain.NewValidationError("github.secrets", "is required")
	}
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || !isAllowedBaseURL(parsed) {
		return nil, domain.NewValidationError("github.baseURL", "must be an https URL; http is allowed only for loopback hosts")
	}
	baseURL = strings.TrimRight(parsed.String(), "/")

	tokenKey := strings.TrimSpace(cfg.TokenKey)
	if tokenKey == "" {
		tokenKey = DefaultTokenKey
	}
	userAgent := strings.TrimSpace(cfg.UserAgent)
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}

	repos := make([]string, 0, len(cfg.Repos))
	seen := make(map[string]struct{}, len(cfg.Repos))
	for _, repo := range cfg.Repos {
		normalized, err := normalizeRepo(repo)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		repos = append(repos, normalized)
	}
	if len(repos) == 0 {
		return nil, domain.NewValidationError("github.repos", "must contain at least one owner/repository")
	}
	sort.Strings(repos)

	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	return &Connector{
		baseURL:   baseURL,
		tokenKey:  tokenKey,
		userAgent: userAgent,
		repos:     repos,
		secrets:   cfg.Secrets,
		sink:      cfg.Sink,
		http:      connectors.NewHTTP(connectors.HTTPOptions{Client: cfg.HTTPClient, Retry: cfg.Retry}),
		now:       now,
	}, nil
}

// Name reports the connector source.
func (c *Connector) Name() string { return Name }

// Run collects configured repositories and writes one Markdown dump per repo.
func (c *Connector) Run(ctx context.Context, opts ports.ConnectorOptions) (domain.ConnectorRun, error) {
	started := c.now().UTC()
	run := connectors.NewRun(Name, started)

	token, err := c.secrets.Get(ctx, c.tokenKey)
	if err != nil || strings.TrimSpace(token) == "" {
		if err == nil {
			err = fmt.Errorf("github token is empty")
		}
		runErr := fmt.Errorf("read github token: %w", err)
		run = connectors.FinishRun(run, nil, runErr, started)
		return run, fmt.Errorf("github connector: %s", connectors.SanitizeError(runErr))
	}
	token = strings.TrimSpace(token)

	repos, err := c.selectRepos(opts.Projects)
	if err != nil {
		run = connectors.FinishRun(run, nil, err, started, token)
		return run, fmt.Errorf("github connector: %s", connectors.SanitizeError(err, token))
	}

	limit := normalizeLimit(opts.Limit)
	counts := map[string]int{}
	var failures []string

	for _, repo := range repos {
		dump, err := c.collectRepo(ctx, token, repo, opts.Since, limit)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", repo, connectors.SanitizeError(err, token)))
			continue
		}
		if len(dump.failures) > 0 {
			for _, failure := range dump.failures {
				failures = append(failures, fmt.Sprintf("%s: %s", repo, connectors.SanitizeError(failure, token)))
			}
		}
		if c.sink != nil {
			if err := c.sink.Write(ctx, dumpPath(repo), renderDump(dump, started)); err != nil {
				failures = append(failures, fmt.Sprintf("%s: write markdown: %s", repo, connectors.SanitizeError(err, token)))
				continue
			}
		}
		counts["repos"]++
		counts["pull_requests"] += len(dump.pulls)
		counts["issues"] += len(dump.issues)
		counts["commits"] += len(dump.commits)
	}

	var runErr error
	if len(failures) > 0 {
		runErr = errors.New(strings.Join(failures, "; "))
	}
	run = connectors.FinishRun(run, counts, runErr, c.now().UTC(), token)
	if runErr != nil {
		return run, fmt.Errorf("github connector: %s", connectors.SanitizeError(runErr, token))
	}
	return run, nil
}

type repoDump struct {
	repo     repository
	pulls    []pullRequest
	issues   []issue
	commits  []commit
	failures []error
}

func (c *Connector) collectRepo(ctx context.Context, token, repo string, since *time.Time, limit int) (repoDump, error) {
	metadata, err := c.getRepository(ctx, token, repo)
	if err != nil {
		return repoDump{}, err
	}
	dump := repoDump{repo: metadata}

	pulls, err := c.listPullRequests(ctx, token, repo, since, limit)
	if err != nil {
		dump.failures = append(dump.failures, fmt.Errorf("pull requests: %w", err))
	} else {
		dump.pulls = pulls
	}

	issues, err := c.listIssues(ctx, token, repo, since, limit)
	if err != nil {
		dump.failures = append(dump.failures, fmt.Errorf("issues: %w", err))
	} else {
		dump.issues = issues
	}

	commits, err := c.listCommits(ctx, token, repo, since, limit)
	if err != nil {
		dump.failures = append(dump.failures, fmt.Errorf("commits: %w", err))
	} else {
		dump.commits = commits
	}

	return dump, nil
}

func (c *Connector) selectRepos(requested []string) ([]string, error) {
	if len(requested) == 0 {
		return append([]string(nil), c.repos...), nil
	}
	allowed := make(map[string]struct{}, len(c.repos))
	for _, repo := range c.repos {
		allowed[repo] = struct{}{}
	}
	selected := make([]string, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, repo := range requested {
		normalized, err := normalizeRepo(repo)
		if err != nil {
			return nil, err
		}
		if _, ok := allowed[normalized]; !ok {
			return nil, domain.NewValidationError("connector.projects", fmt.Sprintf("repository %q is not configured", normalized))
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		selected = append(selected, normalized)
	}
	if len(selected) == 0 {
		return nil, domain.NewValidationError("connector.projects", "must not be empty")
	}
	sort.Strings(selected)
	return selected, nil
}

func normalizeRepo(repo string) (string, error) {
	repo = strings.TrimSpace(repo)
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", domain.NewValidationError("github.repo", fmt.Sprintf("%q must look like owner/repository", repo))
	}
	for _, part := range parts {
		if part == "." || part == ".." {
			return "", domain.NewValidationError("github.repo", fmt.Sprintf("%q is not a valid repository", repo))
		}
		for _, r := range part {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
				continue
			}
			return "", domain.NewValidationError("github.repo", fmt.Sprintf("%q contains an invalid character", repo))
		}
	}
	return parts[0] + "/" + parts[1], nil
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}

func repoAPIPath(repo string) string {
	parts := strings.SplitN(repo, "/", 2)
	return "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
}

func dumpPath(repo string) string {
	slug, err := connectors.SlugPath(repo)
	if err != nil {
		slug = connectors.Slug(repo)
	}
	return "github/" + slug + ".md"
}

func (c *Connector) getRepository(ctx context.Context, token, repo string) (repository, error) {
	var out repository
	if err := c.get(ctx, token, repoAPIPath(repo), nil, &out); err != nil {
		return repository{}, err
	}
	return out, nil
}

func (c *Connector) listPullRequests(ctx context.Context, token, repo string, since *time.Time, limit int) ([]pullRequest, error) {
	query := url.Values{}
	query.Set("state", "all")
	query.Set("sort", "updated")
	query.Set("direction", "desc")
	// Results are ordered by updated_at descending, so once a page ends before
	// since, every later page is older too.
	keep := func(item pullRequest) bool {
		return since == nil || !item.UpdatedAt.Before(since.UTC())
	}
	stop := func(page []pullRequest) bool {
		return since != nil && len(page) > 0 && page[len(page)-1].UpdatedAt.Before(since.UTC())
	}
	filtered, err := listPaged(ctx, c, token, repoAPIPath(repo)+"/pulls", query, limit, keep, stop)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if !filtered[i].UpdatedAt.Equal(filtered[j].UpdatedAt) {
			return filtered[i].UpdatedAt.After(filtered[j].UpdatedAt)
		}
		return filtered[i].Number > filtered[j].Number
	})
	return filtered, nil
}

func (c *Connector) listIssues(ctx context.Context, token, repo string, since *time.Time, limit int) ([]issue, error) {
	query := url.Values{}
	query.Set("state", "all")
	query.Set("sort", "updated")
	query.Set("direction", "desc")
	if since != nil {
		query.Set("since", since.UTC().Format(time.RFC3339))
	}
	keep := func(item issue) bool {
		if item.PullRequest != nil {
			return false
		}
		return since == nil || !item.UpdatedAt.Before(since.UTC())
	}
	filtered, err := listPaged(ctx, c, token, repoAPIPath(repo)+"/issues", query, limit, keep, nil)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if !filtered[i].UpdatedAt.Equal(filtered[j].UpdatedAt) {
			return filtered[i].UpdatedAt.After(filtered[j].UpdatedAt)
		}
		return filtered[i].Number > filtered[j].Number
	})
	return filtered, nil
}

func (c *Connector) listCommits(ctx context.Context, token, repo string, since *time.Time, limit int) ([]commit, error) {
	query := url.Values{}
	if since != nil {
		query.Set("since", since.UTC().Format(time.RFC3339))
	}
	out, err := listPaged[commit](ctx, c, token, repoAPIPath(repo)+"/commits", query, limit, nil, nil)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		left := out[i].Commit.Author.Date
		right := out[j].Commit.Author.Date
		if !left.Equal(right) {
			return left.After(right)
		}
		return out[i].SHA < out[j].SHA
	})
	return out, nil
}

func (c *Connector) get(ctx context.Context, token, path string, query url.Values, out any) error {
	requestURL := c.baseURL + path
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}
	_, err := c.getURL(ctx, token, requestURL, out)
	return err
}

// getURL performs an authenticated GET and returns the response headers. The
// token is attached to requestURL, so callers pass only the configured API base
// or a next-page URL already checked by nextPage.
func (c *Connector) getURL(ctx context.Context, token, requestURL string, out any) (http.Header, error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	header.Set("Accept", "application/vnd.github+json")
	header.Set("X-GitHub-Api-Version", "2022-11-28")
	header.Set("User-Agent", c.userAgent)
	return c.http.GetJSONWithHeaders(ctx, requestURL, header, out)
}
