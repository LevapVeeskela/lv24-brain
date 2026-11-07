// Package gitlab implements a read-only GitLab connector. It only issues GET
// requests with a personal access token from ports.SecretStore and writes
// deterministic Markdown dumps through connectors.MarkdownSink.
package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// Name is the connector source name.
	Name = "gitlab"
	// DefaultBaseURL is the public GitLab REST API.
	DefaultBaseURL = "https://gitlab.com/api/v4"
	// DefaultTokenKey is the default secret-store key.
	DefaultTokenKey = "gitlab/token"
	// DefaultUserAgent identifies the engine to GitLab.
	DefaultUserAgent = "lv24-brain"
	// DefaultLimit is the per-collection item limit.
	DefaultLimit = 25
	// MaxLimit is the largest supported per-collection item limit.
	MaxLimit = 1000
)

// Config configures the GitLab connector.
type Config struct {
	BaseURL    string
	TokenKey   string
	UserAgent  string
	Projects   []string
	Secrets    ports.SecretStore
	Sink       connectors.MarkdownSink
	HTTPClient *http.Client
	Retry      connectors.RetryPolicy
	Now        func() time.Time
}

// Connector reads project metadata, merge requests, issues and recent commits.
type Connector struct {
	baseURL   string
	tokenKey  string
	userAgent string
	projects  []string
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
		return nil, domain.NewValidationError("gitlab.secrets", "is required")
	}
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || !isAllowedBaseURL(parsed) {
		return nil, domain.NewValidationError("gitlab.baseURL", "must be an https URL; http is allowed only for loopback hosts")
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

	projects := make([]string, 0, len(cfg.Projects))
	seen := make(map[string]struct{}, len(cfg.Projects))
	for _, project := range cfg.Projects {
		normalized, err := normalizeProject(project)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		projects = append(projects, normalized)
	}
	if len(projects) == 0 {
		return nil, domain.NewValidationError("gitlab.projects", "must contain at least one project id or path")
	}
	sort.Strings(projects)

	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	return &Connector{
		baseURL:   baseURL,
		tokenKey:  tokenKey,
		userAgent: userAgent,
		projects:  projects,
		secrets:   cfg.Secrets,
		sink:      cfg.Sink,
		http:      connectors.NewHTTP(connectors.HTTPOptions{Client: cfg.HTTPClient, Retry: cfg.Retry}),
		now:       now,
	}, nil
}

// Name reports the connector source.
func (c *Connector) Name() string { return Name }

// Run collects configured projects and writes one Markdown dump per project.
func (c *Connector) Run(ctx context.Context, opts ports.ConnectorOptions) (domain.ConnectorRun, error) {
	started := c.now().UTC()
	run := connectors.NewRun(Name, started)

	token, err := c.secrets.Get(ctx, c.tokenKey)
	if err != nil || strings.TrimSpace(token) == "" {
		if err == nil {
			err = fmt.Errorf("gitlab token is empty")
		}
		runErr := fmt.Errorf("read gitlab token: %w", err)
		run = connectors.FinishRun(run, nil, runErr, started)
		return run, fmt.Errorf("gitlab connector: %s", connectors.SanitizeError(runErr))
	}
	token = strings.TrimSpace(token)

	projects, err := c.selectProjects(opts.Projects)
	if err != nil {
		run = connectors.FinishRun(run, nil, err, started, token)
		return run, fmt.Errorf("gitlab connector: %s", connectors.SanitizeError(err, token))
	}

	limit := normalizeLimit(opts.Limit)
	counts := map[string]int{}
	var failures []string

	for _, project := range projects {
		dump, err := c.collectProject(ctx, token, project, opts.Since, limit)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", project, connectors.SanitizeError(err, token)))
			continue
		}
		if len(dump.failures) > 0 {
			for _, failure := range dump.failures {
				failures = append(failures, fmt.Sprintf("%s: %s", project, connectors.SanitizeError(failure, token)))
			}
		}
		if c.sink != nil {
			if err := c.sink.Write(ctx, dumpPath(project), renderDump(dump, started)); err != nil {
				failures = append(failures, fmt.Sprintf("%s: write markdown: %s", project, connectors.SanitizeError(err, token)))
				continue
			}
		}
		counts["projects"]++
		counts["merge_requests"] += len(dump.mergeRequests)
		counts["issues"] += len(dump.issues)
		counts["commits"] += len(dump.commits)
	}

	var runErr error
	if len(failures) > 0 {
		runErr = errors.New(strings.Join(failures, "; "))
	}
	run = connectors.FinishRun(run, counts, runErr, c.now().UTC(), token)
	if runErr != nil {
		return run, fmt.Errorf("gitlab connector: %s", connectors.SanitizeError(runErr, token))
	}
	return run, nil
}

type projectDump struct {
	project       project
	mergeRequests []mergeRequest
	issues        []issue
	commits       []commit
	failures      []error
}

func (c *Connector) collectProject(ctx context.Context, token, projectPath string, since *time.Time, limit int) (projectDump, error) {
	metadata, err := c.getProject(ctx, token, projectPath)
	if err != nil {
		return projectDump{}, err
	}
	dump := projectDump{project: metadata}

	projectRef := projectPath
	if metadata.ID != 0 {
		projectRef = strconv.Itoa(metadata.ID)
	}

	mergeRequests, err := c.listMergeRequests(ctx, token, projectRef, since, limit)
	if err != nil {
		dump.failures = append(dump.failures, fmt.Errorf("merge requests: %w", err))
	} else {
		dump.mergeRequests = mergeRequests
	}

	issues, err := c.listIssues(ctx, token, projectRef, since, limit)
	if err != nil {
		dump.failures = append(dump.failures, fmt.Errorf("issues: %w", err))
	} else {
		dump.issues = issues
	}

	commits, err := c.listCommits(ctx, token, projectRef, since, limit)
	if err != nil {
		dump.failures = append(dump.failures, fmt.Errorf("commits: %w", err))
	} else {
		dump.commits = commits
	}

	return dump, nil
}

func (c *Connector) selectProjects(requested []string) ([]string, error) {
	if len(requested) == 0 {
		return append([]string(nil), c.projects...), nil
	}
	allowed := make(map[string]struct{}, len(c.projects))
	for _, project := range c.projects {
		allowed[project] = struct{}{}
	}
	selected := make([]string, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, project := range requested {
		normalized, err := normalizeProject(project)
		if err != nil {
			return nil, err
		}
		if _, ok := allowed[normalized]; !ok {
			return nil, domain.NewValidationError("connector.projects", fmt.Sprintf("project %q is not configured", normalized))
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

func normalizeProject(project string) (string, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return "", domain.NewValidationError("gitlab.project", "is required")
	}
	if project == "." || project == ".." || strings.Contains(project, "..") {
		return "", domain.NewValidationError("gitlab.project", fmt.Sprintf("%q is not a valid project", project))
	}
	for _, r := range project {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == '/' {
			continue
		}
		return "", domain.NewValidationError("gitlab.project", fmt.Sprintf("%q contains an invalid character", project))
	}
	return strings.Trim(project, "/"), nil
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

func projectAPIPath(project string) string {
	return "/projects/" + url.PathEscape(project)
}

func dumpPath(project string) string {
	slug, err := connectors.SlugPath(project)
	if err != nil {
		slug = connectors.Slug(project)
	}
	return "gitlab/" + slug + ".md"
}

func (c *Connector) getProject(ctx context.Context, token, projectPath string) (project, error) {
	var out project
	if err := c.get(ctx, token, projectAPIPath(projectPath), nil, &out); err != nil {
		return project{}, err
	}
	return out, nil
}

func (c *Connector) listMergeRequests(ctx context.Context, token, project string, since *time.Time, limit int) ([]mergeRequest, error) {
	query := url.Values{}
	query.Set("state", "all")
	query.Set("order_by", "updated_at")
	query.Set("sort", "desc")
	if since != nil {
		query.Set("updated_after", since.UTC().Format(time.RFC3339))
	}
	keep := func(item mergeRequest) bool {
		return since == nil || !item.UpdatedAt.Before(since.UTC())
	}
	filtered, err := listPaged(ctx, c, token, projectAPIPath(project)+"/merge_requests", query, limit, keep, nil)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if !filtered[i].UpdatedAt.Equal(filtered[j].UpdatedAt) {
			return filtered[i].UpdatedAt.After(filtered[j].UpdatedAt)
		}
		return filtered[i].IID > filtered[j].IID
	})
	return filtered, nil
}

func (c *Connector) listIssues(ctx context.Context, token, project string, since *time.Time, limit int) ([]issue, error) {
	query := url.Values{}
	query.Set("state", "all")
	query.Set("order_by", "updated_at")
	query.Set("sort", "desc")
	if since != nil {
		query.Set("updated_after", since.UTC().Format(time.RFC3339))
	}
	keep := func(item issue) bool {
		return since == nil || !item.UpdatedAt.Before(since.UTC())
	}
	filtered, err := listPaged(ctx, c, token, projectAPIPath(project)+"/issues", query, limit, keep, nil)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if !filtered[i].UpdatedAt.Equal(filtered[j].UpdatedAt) {
			return filtered[i].UpdatedAt.After(filtered[j].UpdatedAt)
		}
		return filtered[i].IID > filtered[j].IID
	})
	return filtered, nil
}

func (c *Connector) listCommits(ctx context.Context, token, project string, since *time.Time, limit int) ([]commit, error) {
	query := url.Values{}
	if since != nil {
		query.Set("since", since.UTC().Format(time.RFC3339))
	}
	out, err := listPaged[commit](ctx, c, token, projectAPIPath(project)+"/repository/commits", query, limit, nil, nil)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		left := out[i].AuthoredDate
		right := out[j].AuthoredDate
		if !left.Equal(right) {
			return left.After(right)
		}
		return out[i].ID < out[j].ID
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
// or a next-page URL derived from it by nextPage.
func (c *Connector) getURL(ctx context.Context, token, requestURL string, out any) (http.Header, error) {
	header := http.Header{}
	header.Set("PRIVATE-TOKEN", token)
	header.Set("Accept", "application/json")
	header.Set("User-Agent", c.userAgent)
	return c.http.GetJSONWithHeaders(ctx, requestURL, header, out)
}
