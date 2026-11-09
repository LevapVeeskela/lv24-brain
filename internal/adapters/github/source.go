package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	checkpointStream      = "github"
	checkpointVersion     = 1
	maxSourcePage         = 100
	maxSourcePagesPerRead = 10
	maxCommentsPerItem    = 20
	maxCommentBytes       = 8 << 10
	maxSourceBodyBytes    = 256 << 10
	// Numbered pages can shift during a run; overlapping the last five
	// minutes replays updated items that moved across a page boundary.
	pageOverlap = 5 * time.Minute
)

// Descriptor is the static GitHub source description used by the registry.
func Descriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider: Name, DisplayName: "GitHub",
		Capabilities:   domain.Capabilities{Backfill: true, Incremental: true, AuthModes: []domain.AuthMode{domain.AuthToken}},
		DefaultPublish: domain.PublishShared,
		ConfigFields: []domain.ConfigField{
			{Key: "repos", Label: "Repositories", Required: true, Help: "Comma or newline separated owner/repository names."},
			{Key: "token", Label: "Personal access token", Required: true, Secret: true},
		},
		Streams: []string{checkpointStream},
	}
}

// Source reads repository issues, pull requests and commits into raw items.
// It only uses authenticated GET requests and never stores credentials.
type Source struct {
	connection  domain.Connection
	baseURL     string
	userAgent   string
	repos       []string
	credentials ports.CredentialProvider
	http        *connectors.HTTP
	now         func() time.Time
}

type sourceCursor struct {
	Collection int          `json:"collection"`
	Page       int          `json:"page"`
	Offset     int          `json:"offset"`
	ScanSince  []*time.Time `json:"scanSince,omitempty"`
	Watermarks []*time.Time `json:"watermarks,omitempty"`
}

type sourceCollection struct {
	repo string
	kind string
}

// NewSource constructs the native source using credentials under conn/token.
func NewSource(conn domain.Connection, credentials ports.CredentialProvider, client *http.Client, now func() time.Time) (*Source, error) {
	if err := Descriptor().ValidateConnection(conn); err != nil {
		return nil, err
	}
	if credentials == nil {
		return nil, domain.NewValidationError("github.credentials", "is required")
	}
	repos, err := parseRepos(conn.Settings["repos"])
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimSpace(conn.BaseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || !isAllowedBaseURL(parsed) || parsed.User != nil || parsed.Fragment != "" {
		return nil, domain.NewValidationError("github.baseUrl", "must be an https URL; http is allowed only for loopback hosts")
	}
	if now == nil {
		now = time.Now
	}
	return &Source{
		connection:  conn,
		baseURL:     strings.TrimRight(parsed.String(), "/"),
		userAgent:   DefaultUserAgent,
		repos:       repos,
		credentials: credentials,
		http:        connectors.NewHTTP(connectors.HTTPOptions{Client: client}),
		now:         now,
	}, nil
}

func parseRepos(raw string) ([]string, error) {
	var repos []string
	for _, entry := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		repo, err := normalizeRepo(entry)
		if err != nil {
			return nil, err
		}
		if len(repo) > 200 {
			return nil, domain.NewValidationError("github.repos", "repository names must be at most 200 bytes")
		}
		repos = append(repos, repo)
	}
	if len(repos) == 0 {
		return nil, domain.NewValidationError("github.repos", "must contain at least one owner/repository")
	}
	sort.Strings(repos)
	repos = slices.Compact(repos)
	return repos, nil
}

// Descriptor returns the provider's static description.
func (s *Source) Descriptor() domain.SourceDescriptor { return Descriptor() }

// Check verifies credentials and access with one repository metadata request.
func (s *Source) Check(ctx context.Context) error {
	token, err := s.token(ctx)
	if err != nil {
		return err
	}
	var repo repository
	if err := s.get(ctx, token, repoAPIPath(s.repos[0]), nil, &repo); err != nil {
		return fmt.Errorf("github check: %s", connectors.SanitizeError(err, token))
	}
	return nil
}

// Read returns one bounded page of repository items and its resumable cursor.
func (s *Source) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, err
	}
	if req.Connection.ID != s.connection.ID || req.Connection.Provider != Name {
		return ports.ReadBatch{}, domain.NewValidationError("github.connection", "does not match source connection")
	}
	token, err := s.token(ctx)
	if err != nil {
		return ports.ReadBatch{}, err
	}
	collections := s.collections()
	cursor := newSourceCursor(len(collections), req.Since)
	if checkpoint, ok := req.Checkpoints[checkpointStream]; ok {
		if checkpoint.Stream != checkpointStream || checkpoint.Version != checkpointVersion {
			return ports.ReadBatch{}, domain.NewValidationError("github.checkpoint", "unsupported stream or version")
		}
		if err := json.Unmarshal(checkpoint.Data, &cursor); err != nil {
			return ports.ReadBatch{}, domain.NewValidationError("github.checkpoint", "is invalid")
		}
		if cursor.Page < 1 || cursor.Offset < 0 {
			return ports.ReadBatch{}, domain.NewValidationError("github.checkpoint", "position is invalid")
		}
		if cursor.Collection < 0 || cursor.Collection >= len(collections) || cursor.Offset > maxSourcePage || len(cursor.ScanSince) != len(collections) || len(cursor.Watermarks) != len(collections) {
			return ports.ReadBatch{}, domain.NewValidationError("github.checkpoint", "collection state is invalid")
		}
		// ReadRequest.Since is a run-level lower bound and must narrow an
		// existing checkpoint without discarding its durable page position.
		if req.Since != nil {
			for i := range cursor.ScanSince {
				cursor.ScanSince[i] = laterTime(cursor.ScanSince[i], req.Since)
			}
		}
	}
	limit := normalizeLimit(req.Limit)
	items := make([]domain.Item, 0, limit)
	pagesRead := 0
	for cursor.Collection < len(collections) && len(items) < limit && pagesRead < maxSourcePagesPerRead {
		if err := ctx.Err(); err != nil {
			return ports.ReadBatch{}, err
		}
		collection := collections[cursor.Collection]
		since := cursor.ScanSince[cursor.Collection]
		raw, pageFull, err := s.listCollectionPage(ctx, token, collection, since, cursor.Page)
		if err != nil {
			return ports.ReadBatch{}, fmt.Errorf("github read: %s", connectors.SanitizeError(err, token))
		}
		pagesRead++
		if cursor.Offset > len(raw) {
			return ports.ReadBatch{}, domain.NewValidationError("github.checkpoint", "offset exceeds provider page")
		}
		for cursor.Offset < len(raw) && len(items) < limit {
			obj := raw[cursor.Offset]
			cursor.Offset++
			item, err := s.toItem(ctx, token, req.Connection.ID, collection, obj)
			if err != nil {
				return ports.ReadBatch{}, fmt.Errorf("github read: %s", connectors.SanitizeError(err, token))
			}
			if item != nil && (since == nil || !item.UpdatedAt.Before(since.UTC())) {
				items = append(items, *item)
				watermark := cursor.Watermarks[cursor.Collection]
				if watermark == nil || item.UpdatedAt.After(*watermark) {
					cursor.Watermarks[cursor.Collection] = timePtr(item.UpdatedAt.UTC())
				}
			}
		}
		if cursor.Offset >= len(raw) {
			if pageFull {
				cursor.Page++
				cursor.Offset = 0
			} else {
				cursor.Collection++
				cursor.Page = 1
				cursor.Offset = 0
			}
		}
	}
	more := cursor.Collection < len(collections)
	if !more {
		cursor.Collection, cursor.Page, cursor.Offset = 0, 1, 0
		for i, watermark := range cursor.Watermarks {
			if watermark != nil {
				overlap := timePtr(watermark.UTC().Add(-pageOverlap))
				cursor.ScanSince[i] = laterTime(overlap, req.Since)
			} else {
				cursor.ScanSince[i] = cloneTime(req.Since)
			}
		}
	}
	data, err := json.Marshal(cursor)
	if err != nil {
		return ports.ReadBatch{}, fmt.Errorf("encode github checkpoint: %w", err)
	}
	return ports.ReadBatch{
		Items:       items,
		Checkpoints: []domain.Checkpoint{{Stream: checkpointStream, Version: checkpointVersion, Data: data}},
		More:        more,
	}, nil
}

func newSourceCursor(collectionCount int, since *time.Time) sourceCursor {
	cursor := sourceCursor{
		Page:       1,
		ScanSince:  make([]*time.Time, collectionCount),
		Watermarks: make([]*time.Time, collectionCount),
	}
	for i := 0; i < collectionCount; i++ {
		cursor.ScanSince[i] = cloneTime(since)
		cursor.Watermarks[i] = cloneTime(since)
	}
	return cursor
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	return timePtr(value.UTC())
}

func laterTime(left, right *time.Time) *time.Time {
	if left == nil {
		return cloneTime(right)
	}
	if right != nil && right.After(*left) {
		return cloneTime(right)
	}
	return cloneTime(left)
}

func (s *Source) collections() []sourceCollection {
	collections := make([]sourceCollection, 0, len(s.repos)*3)
	for _, repo := range s.repos {
		collections = append(collections,
			sourceCollection{repo: repo, kind: "issue"},
			sourceCollection{repo: repo, kind: "pull-request"},
			sourceCollection{repo: repo, kind: "commit"},
		)
	}
	return collections
}

type sourceObject struct {
	issue  *issue
	pull   *pullRequest
	commit *commit
}

func (s *Source) listCollectionPage(ctx context.Context, token string, collection sourceCollection, since *time.Time, page int) ([]sourceObject, bool, error) {
	query := url.Values{"page": {strconv.Itoa(page)}, "per_page": {strconv.Itoa(maxSourcePage)}}
	path := repoAPIPath(collection.repo)
	switch collection.kind {
	case "issue":
		path += "/issues"
		query.Set("state", "all")
		query.Set("sort", "updated")
		query.Set("direction", "asc")
	case "pull-request":
		path += "/pulls"
		query.Set("state", "all")
		query.Set("sort", "updated")
		query.Set("direction", "asc")
	case "commit":
		path += "/commits"
	default:
		return nil, false, fmt.Errorf("unknown collection %q", collection.kind)
	}
	if since != nil && collection.kind != "pull-request" {
		query.Set("since", since.UTC().Format(time.RFC3339))
	}
	var objects []sourceObject
	switch collection.kind {
	case "issue":
		var values []issue
		if err := s.get(ctx, token, path, query, &values); err != nil {
			return nil, false, err
		}
		pageFull := len(values) == maxSourcePage
		for i := range values {
			if values[i].PullRequest == nil {
				objects = append(objects, sourceObject{issue: &values[i]})
			}
		}
		return objects, pageFull, nil
	case "pull-request":
		var values []pullRequest
		if err := s.get(ctx, token, path, query, &values); err != nil {
			return nil, false, err
		}
		pageFull := len(values) == maxSourcePage
		for i := range values {
			objects = append(objects, sourceObject{pull: &values[i]})
		}
		return objects, pageFull, nil
	case "commit":
		var values []commit
		if err := s.get(ctx, token, path, query, &values); err != nil {
			return nil, false, err
		}
		pageFull := len(values) == maxSourcePage
		for i := range values {
			objects = append(objects, sourceObject{commit: &values[i]})
		}
		return objects, pageFull, nil
	}
	return objects, false, nil
}

func (s *Source) toItem(ctx context.Context, token, connectionID string, collection sourceCollection, obj sourceObject) (*domain.Item, error) {
	item := domain.Item{Ref: domain.ItemRef{ConnectionID: connectionID}}
	if len(s.connection.Projects) == 1 {
		item.ProjectID = s.connection.Projects[0]
	}
	var commentPaths []string
	switch {
	case obj.issue != nil:
		x := obj.issue
		item.Ref.ExternalID = fmt.Sprintf("%s#issue-%d", collection.repo, x.Number)
		item.Kind, item.Title, item.URL, item.Author = domain.ItemIssue, x.Title, x.HTMLURL, x.User.Login
		item.CreatedAt, item.UpdatedAt = x.CreatedAt, x.UpdatedAt
		item.Labels = labelNames(x.Labels)
		item.Body = x.Body
		commentPaths = []string{repoAPIPath(collection.repo) + "/issues/" + strconv.Itoa(x.Number) + "/comments"}
	case obj.pull != nil:
		x := obj.pull
		item.Ref.ExternalID = fmt.Sprintf("%s#pull-%d", collection.repo, x.Number)
		item.Kind, item.Title, item.URL, item.Author = domain.ItemPullRequest, x.Title, x.HTMLURL, x.User.Login
		item.CreatedAt, item.UpdatedAt = x.CreatedAt, x.UpdatedAt
		item.Labels = labelNames(x.Labels)
		item.Body = x.Body
		commentPaths = []string{
			repoAPIPath(collection.repo) + "/issues/" + strconv.Itoa(x.Number) + "/comments",
			repoAPIPath(collection.repo) + "/pulls/" + strconv.Itoa(x.Number) + "/comments",
		}
	case obj.commit != nil:
		x := obj.commit
		item.Ref.ExternalID = fmt.Sprintf("%s#commit-%s", collection.repo, x.SHA)
		item.Kind, item.Title, item.URL = domain.ItemCommit, sourceFirstLine(x.Commit.Message), x.HTMLURL
		item.Body, item.CreatedAt, item.UpdatedAt = x.Commit.Message, x.Commit.Author.Date, x.Commit.Author.Date
		if x.Author != nil {
			item.Author = x.Author.Login
		} else {
			item.Author = x.Commit.Author.Name
		}
		commentPaths = []string{repoAPIPath(collection.repo) + "/commits/" + url.PathEscape(x.SHA) + "/comments"}
	default:
		return nil, fmt.Errorf("empty GitHub object")
	}
	var comments []sourceComment
	for _, commentPath := range commentPaths {
		remaining := maxCommentsPerItem - len(comments)
		if remaining <= 0 {
			break
		}
		page, err := s.listComments(ctx, token, commentPath, remaining)
		if err != nil {
			return nil, err
		}
		comments = append(comments, page...)
	}
	item.Body = boundedBody(item.Body, comments)
	return &item, nil
}

type sourceComment struct {
	Body      string    `json:"body"`
	User      user      `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Source) listComments(ctx context.Context, token, path string, limit int) ([]sourceComment, error) {
	query := url.Values{"per_page": {strconv.Itoa(limit)}, "page": {"1"}}
	var comments []sourceComment
	if err := s.get(ctx, token, path, query, &comments); err != nil {
		return nil, err
	}
	if len(comments) > limit {
		comments = comments[:limit]
	}
	return comments, nil
}

func boundedBody(body string, comments []sourceComment) string {
	var out strings.Builder
	appendBounded(&out, strings.TrimSpace(body), maxSourceBodyBytes)
	for _, comment := range comments {
		entry := "\n\n---\n\n" + comment.User.Login + " · " + comment.CreatedAt.UTC().Format(time.RFC3339) + "\n\n" + capUTF8(strings.TrimSpace(comment.Body), maxCommentBytes)
		appendBounded(&out, entry, maxSourceBodyBytes)
		if out.Len() >= maxSourceBodyBytes {
			break
		}
	}
	return out.String()
}

func appendBounded(out *strings.Builder, value string, limit int) {
	remaining := limit - out.Len()
	if remaining <= 0 {
		return
	}
	out.WriteString(capUTF8(value, remaining))
}

func capUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit]
}

func labelNames(labels []label) []string {
	out := make([]string, 0, len(labels))
	for _, value := range labels {
		out = append(out, value.Name)
	}
	sort.Strings(out)
	return out
}

func sourceFirstLine(value string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(value), "\n")
	if line == "" {
		return "GitHub commit"
	}
	return line
}

func timePtr(t time.Time) *time.Time { return &t }

func (s *Source) token(ctx context.Context) (string, error) {
	token, err := s.credentials.Get(ctx, s.connection.ID, "token")
	if err != nil {
		return "", fmt.Errorf("read github credential: %s", connectors.SanitizeError(err))
	}
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("read github credential: token is empty")
	}
	return strings.TrimSpace(token), nil
}

func (s *Source) get(ctx context.Context, token, path string, query url.Values, out any) error {
	requestURL := s.baseURL + path
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Accept", "application/vnd.github+json")
	headers.Set("X-GitHub-Api-Version", "2022-11-28")
	headers.Set("User-Agent", s.userAgent)
	_, err := s.http.GetJSONWithHeaders(ctx, requestURL, headers, out)
	return err
}

var _ ports.Source = (*Source)(nil)
