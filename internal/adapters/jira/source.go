package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
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
	Name                = "jira"
	checkpointStream    = "jira"
	checkpointVersion   = 1
	pageSize            = 50
	maxPagesPerRead     = 10
	maxPagesPerPass     = 10000
	maxCommentsPerIssue = 5
	maxCommentBytes     = 16 << 10
	maxIssueBodyBytes   = 256 << 10
	watermarkOverlap    = 5 * time.Minute
)

var orderByClause = regexp.MustCompile(`(?i)\border\s+by\b`)

// Descriptor describes the native Jira Cloud and Data Center read-only source.
func Descriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider: Name, DisplayName: "Jira",
		Capabilities:   domain.Capabilities{Backfill: true, Incremental: true, AuthModes: []domain.AuthMode{domain.AuthToken}},
		DefaultPublish: domain.PublishShared,
		ConfigFields: []domain.ConfigField{
			{Key: "token", Label: "API token / personal access token", Required: true, Secret: true},
			{Key: "email", Label: "Atlassian account email", Help: "Required for Jira Cloud Basic authentication; unused by Data Center."},
			{Key: "deployment", Label: "Deployment", Help: "cloud (default) or datacenter."},
			{Key: "projects", Label: "Projects", Help: "Optional comma-separated Jira project keys."},
			{Key: "jql", Label: "JQL filter", Help: "Optional additional read-only JQL filter; ordering is enforced by the source."},
		},
		Backends: []domain.Backend{domain.BackendNative},
		Streams:  []string{checkpointStream},
	}
}

// Source reads Jira issues from Cloud or Data Center through read-only REST GETs.
type Source struct {
	connection  domain.Connection
	baseURL     string
	deployment  string
	projects    []string
	jql         string
	credentials ports.CredentialProvider
	http        *connectors.HTTP
}

type cursor struct {
	Watermark time.Time `json:"watermark,omitempty"`
	PassMax   time.Time `json:"passMax,omitempty"`
	ScanSince time.Time `json:"scanSince,omitempty"`
	NextPage  string    `json:"nextPageToken,omitempty"`
	Previous  string    `json:"previousPageToken,omitempty"`
	StartAt   int       `json:"startAt,omitempty"`
	Offset    int       `json:"offset,omitempty"`
	Seen      []string  `json:"seen,omitempty"`
	Pages     int       `json:"pages,omitempty"`
	Completed bool      `json:"completed,omitempty"`
}

// NewSource constructs a Jira source. BaseURL is mandatory and cannot contain
// credentials, query parameters, or fragments. Plain HTTP is limited to loopback.
func NewSource(conn domain.Connection, credentials ports.CredentialProvider, client *http.Client, now func() time.Time) (*Source, error) {
	if err := Descriptor().ValidateConnection(conn); err != nil {
		return nil, err
	}
	if credentials == nil {
		return nil, domain.NewValidationError("jira.credentials", "are required")
	}
	baseURL, err := sourceBaseURL(conn.BaseURL)
	if err != nil {
		return nil, err
	}
	deployment := strings.ToLower(strings.TrimSpace(conn.Settings["deployment"]))
	if deployment == "" {
		deployment = "cloud"
	}
	if deployment != "cloud" && deployment != "datacenter" {
		return nil, domain.NewValidationError("jira.deployment", "must be cloud or datacenter")
	}
	customJQL := strings.TrimSpace(conn.Settings["jql"])
	if customJQL != "" && stripOrderBy(customJQL) == "" {
		return nil, domain.NewValidationError("jira.jql", "must contain a filter before ORDER BY")
	}
	projects, err := parseProjects(conn.Settings["projects"])
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Source{
		connection: conn, baseURL: baseURL, deployment: deployment,
		projects: projects, jql: customJQL,
		credentials: credentials,
		http:        connectors.NewHTTP(connectors.HTTPOptions{Client: client, Now: now}),
	}, nil
}

func sourceBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(raw, "#") || parsed.Opaque != "" {
		return "", domain.NewValidationError("jira.baseUrl", "must be an absolute https URL without credentials, query, or fragment; http is allowed only for loopback hosts")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && !(scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return "", domain.NewValidationError("jira.baseUrl", "must use https; http is allowed only for loopback hosts")
	}
	if parsed.Hostname() == "" {
		return "", domain.NewValidationError("jira.baseUrl", "must contain a host")
	}
	parsed.Scheme = scheme
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.EqualFold(host, "localhost.") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func parseProjects(raw string) ([]string, error) {
	var projects []string
	seen := map[string]bool{}
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		for _, r := range value {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				return nil, domain.NewValidationError("jira.projects", "project keys may contain only letters, digits, underscore, and hyphen")
			}
		}
		value = strings.ToUpper(value)
		if !seen[value] {
			seen[value] = true
			projects = append(projects, value)
		}
	}
	sort.Strings(projects)
	return projects, nil
}

func (s *Source) Descriptor() domain.SourceDescriptor { return Descriptor() }

// Check verifies credentials and the current user's identity with Jira's
// authenticated-user endpoint. It does not read issue data.
func (s *Source) Check(ctx context.Context) error {
	token, err := s.token(ctx)
	if err != nil {
		return fmt.Errorf("jira check: %s", connectors.SanitizeError(err))
	}
	if s.deployment == "cloud" && strings.TrimSpace(s.connection.Settings["email"]) == "" {
		return domain.NewValidationError("jira.email", "is required for Jira Cloud")
	}
	var identity struct {
		AccountID string `json:"accountId"`
		Name      string `json:"name"`
	}
	if err := s.get(ctx, token, s.apiRoot()+"/myself", nil, &identity); err != nil {
		return fmt.Errorf("jira check: %s", connectors.SanitizeError(err, token))
	}
	if strings.TrimSpace(identity.AccountID) == "" && strings.TrimSpace(identity.Name) == "" {
		return errors.New("jira check: authenticated user response is incomplete")
	}
	return nil
}

func (s *Source) apiRoot() string {
	version := "2"
	if s.deployment == "cloud" {
		version = "3"
	}
	return s.baseURL + "/rest/api/" + version
}

// Read returns the next bounded page of Jira issues. The checkpoint carries a
// durable watermark, a page position and the IDs already returned from the
// current page; a shifted page is replayed instead of trusting a stale offset.
//
// Known limitation: Jira DC uses numeric startAt paging. An issue updated while
// an initial backfill is running can move across an already consumed page
// boundary and be missed for that pass; the five-minute watermark overlap
// repairs the usual recent case, but there is no forced full re-backfill. Cloud
// uses opaque nextPageToken, which is less exposed to this race.
func (s *Source) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, err
	}
	if req.Connection.ID != s.connection.ID || req.Connection.Provider != Name {
		return ports.ReadBatch{}, domain.NewValidationError("jira.connection", "does not match this source")
	}
	token, err := s.token(ctx)
	if err != nil {
		return ports.ReadBatch{}, fmt.Errorf("jira credentials: %s", connectors.SanitizeError(err))
	}
	if s.deployment == "cloud" && strings.TrimSpace(s.connection.Settings["email"]) == "" {
		return ports.ReadBatch{}, domain.NewValidationError("jira.email", "is required for Jira Cloud")
	}
	state, err := readCursor(req.Checkpoints)
	if err != nil {
		return ports.ReadBatch{}, err
	}
	if s.deployment == "cloud" && state.StartAt != 0 || s.deployment == "datacenter" && (state.NextPage != "" || state.Previous != "") {
		return ports.ReadBatch{}, domain.NewValidationError("jira.checkpoint", "pagination state does not match the deployment")
	}
	if state.Completed {
		state.NextPage, state.Previous = "", ""
		state.StartAt, state.Offset, state.Pages = 0, 0, 0
		state.Seen = nil
		state.PassMax = time.Time{}
		state.ScanSince = scanBound(state.Watermark, req.Since)
		state.Completed = false
	}
	// Since belongs to this read run. If it changes in either direction from
	// the in-progress query, restart safely at the run's effective bound; this
	// both narrows an older cursor and widens a cursor created by a stricter run.
	if req.Since != nil {
		requestedBound := scanBound(state.Watermark, req.Since)
		if !requestedBound.Equal(state.ScanSince) {
			state.ScanSince = requestedBound
			state.NextPage, state.Previous = "", ""
			state.StartAt, state.Offset, state.Pages = 0, 0, 0
			state.Seen = nil
			state.PassMax = time.Time{}
		}
	}
	if state.ScanSince.IsZero() {
		state.ScanSince = scanBound(state.Watermark, req.Since)
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 25
	}
	if limit > 500 {
		limit = 500
	}
	batch := ports.ReadBatch{}
	pagesRead := 0
	for pagesRead < maxPagesPerRead && len(batch.Items) < limit {
		if err := ctx.Err(); err != nil {
			return ports.ReadBatch{}, err
		}
		if state.Pages >= maxPagesPerPass {
			return ports.ReadBatch{}, errors.New("jira pagination exceeded the per-pass page limit")
		}
		page, err := s.searchPage(ctx, token, state)
		if err != nil {
			return ports.ReadBatch{}, fmt.Errorf("jira read: %s", connectors.SanitizeError(err, token))
		}
		pagesRead++
		pageIDs := make([]string, len(page.Issues))
		for index := range page.Issues {
			pageIDs[index] = page.Issues[index].Key
		}
		if state.Offset > 0 && !samePagePrefix(pageIDs, state.Seen) {
			state.Offset, state.Seen = 0, nil
		}
		if state.Offset > len(page.Issues) {
			return ports.ReadBatch{}, domain.NewValidationError("jira.checkpoint", "offset exceeds provider page")
		}
		pageDone := false
		for state.Offset < len(page.Issues) {
			recordIndex := state.Offset
			issue := page.Issues[recordIndex]
			if issue.Fields.Updated.IsZero() {
				return ports.ReadBatch{}, errors.New("jira read: issue has no valid updated timestamp")
			}
			if !state.ScanSince.IsZero() && issue.Fields.Updated.Before(state.ScanSince) || req.Since != nil && issue.Fields.Updated.Before(req.Since.UTC()) {
				state.Offset++
				if state.Offset > len(state.Seen) {
					state.Seen = append(state.Seen, pageIDs[recordIndex])
				}
				continue
			}
			if len(batch.Items) >= limit {
				break
			}
			item, err := s.issueItem(ctx, token, req.Connection.ID, issue)
			if err != nil {
				return ports.ReadBatch{}, fmt.Errorf("jira read: %s", connectors.SanitizeError(err, token))
			}
			batch.Items = append(batch.Items, item)
			state.Offset++
			if state.Offset > len(state.Seen) {
				state.Seen = append(state.Seen, pageIDs[recordIndex])
			}
		}
		if state.Offset == len(page.Issues) {
			pageDone = true
		}
		if !pageDone {
			break
		}
		state.PassMax = laterTime(state.PassMax, pageMaxUpdated(page.Issues))
		state.Pages++
		state.Offset = 0
		state.Seen = nil
		if s.deployment == "cloud" {
			if page.NextPageToken != "" {
				if page.NextPageToken == state.NextPage || page.NextPageToken == state.Previous {
					return ports.ReadBatch{}, errors.New("jira pagination returned a non-advancing nextPageToken")
				}
				state.Previous, state.NextPage = state.NextPage, page.NextPageToken
			} else {
				state.Completed = true
			}
		} else {
			if page.StartAt+len(page.Issues) < page.Total {
				next := page.StartAt + len(page.Issues)
				if next <= state.StartAt || len(page.Issues) == 0 {
					return ports.ReadBatch{}, errors.New("jira pagination did not advance startAt")
				}
				state.StartAt = next
			} else {
				state.Completed = true
			}
		}
		if state.Completed {
			state.Watermark = laterTime(state.Watermark, state.PassMax)
			state.NextPage, state.Previous = "", ""
			state.StartAt, state.Offset, state.Pages = 0, 0, 0
			break
		}
	}
	batch.More = !state.Completed
	data, err := json.Marshal(state)
	if err != nil {
		return ports.ReadBatch{}, fmt.Errorf("encode jira checkpoint: %w", err)
	}
	batch.Checkpoints = []domain.Checkpoint{{Stream: checkpointStream, Version: checkpointVersion, Data: data}}
	return batch, nil
}

type searchPage struct {
	Issues        []issue `json:"issues"`
	NextPageToken string  `json:"nextPageToken"`
	IsLast        bool    `json:"isLast"`
	StartAt       int     `json:"startAt"`
	Total         int     `json:"total"`
}

func (s *Source) searchPage(ctx context.Context, token string, state cursor) (searchPage, error) {
	query := url.Values{}
	query.Set("jql", s.query(state.ScanSince))
	query.Set("fields", "summary,description,status,labels,assignee,reporter,project,issuetype,created,updated")
	query.Set("maxResults", strconv.Itoa(pageSize))
	path := "/search"
	if s.deployment == "cloud" {
		path = "/search/jql"
		if state.NextPage != "" {
			query.Set("nextPageToken", state.NextPage)
		}
	} else {
		query.Set("startAt", strconv.Itoa(state.StartAt))
	}
	var page searchPage
	if err := s.get(ctx, token, s.apiRoot()+path, query, &page); err != nil {
		return searchPage{}, err
	}
	if len(page.Issues) > pageSize {
		return searchPage{}, errors.New("Jira returned more issues than requested")
	}
	if s.deployment == "cloud" {
		if page.NextPageToken == "" && !page.IsLast {
			return searchPage{}, errors.New("Jira Cloud returned a non-final page without a nextPageToken")
		}
		if page.NextPageToken != "" && page.IsLast {
			return searchPage{}, errors.New("Jira Cloud returned a nextPageToken for a final page")
		}
	} else {
		if page.StartAt != state.StartAt || page.Total < 0 {
			return searchPage{}, errors.New("Jira Data Center returned invalid startAt/total")
		}
		if len(page.Issues) == 0 && page.StartAt < page.Total {
			return searchPage{}, errors.New("Jira Data Center returned an empty page before total")
		}
	}
	return page, nil
}

func (s *Source) query(since time.Time) string {
	parts := make([]string, 0, 3)
	if len(s.projects) == 0 {
		parts = append(parts, "project is not EMPTY")
	} else {
		quoted := make([]string, len(s.projects))
		for i, project := range s.projects {
			quoted[i] = `"` + project + `"`
		}
		parts = append(parts, "project in ("+strings.Join(quoted, ", ")+")")
	}
	if s.jql != "" {
		custom := stripOrderBy(s.jql)
		parts = append(parts, "("+custom+")")
	}
	if !since.IsZero() {
		parts = append(parts, `updated >= "`+since.UTC().Format("2006-01-02 15:04")+`"`)
	}
	return strings.Join(parts, " AND ") + " ORDER BY updated ASC"
}

func stripOrderBy(value string) string {
	if index := orderByClause.FindStringIndex(value); index != nil {
		return strings.TrimSpace(value[:index[0]])
	}
	return strings.TrimSpace(value)
}

func readCursor(checkpoints map[string]domain.Checkpoint) (cursor, error) {
	state := cursor{}
	for stream := range checkpoints {
		if stream != checkpointStream {
			return cursor{}, domain.NewValidationError("jira.checkpoint", "contains an unsupported stream")
		}
	}
	checkpoint, ok := checkpoints[checkpointStream]
	if !ok {
		return state, nil
	}
	if checkpoint.Stream != checkpointStream || checkpoint.Version != checkpointVersion {
		return cursor{}, domain.NewValidationError("jira.checkpoint", "unsupported stream or version")
	}
	if err := json.Unmarshal(checkpoint.Data, &state); err != nil {
		return cursor{}, domain.NewValidationError("jira.checkpoint", "is invalid JSON")
	}
	if len(strings.TrimSpace(string(checkpoint.Data))) == 0 || strings.TrimSpace(string(checkpoint.Data)) == "null" {
		return cursor{}, domain.NewValidationError("jira.checkpoint", "must be a cursor object")
	}
	if state.StartAt < 0 || state.StartAt > maxPagesPerPass*pageSize || len(state.NextPage) > 8192 || len(state.Previous) > 8192 || state.Offset < 0 || state.Offset > pageSize || state.Pages < 0 || state.Pages >= maxPagesPerPass || state.NextPage == state.Previous && state.NextPage != "" || state.Watermark.Before(time.Time{}) || state.PassMax.Before(time.Time{}) || state.ScanSince.Before(time.Time{}) || (state.NextPage != "" || state.Previous != "") && state.StartAt != 0 || (state.Completed && (state.NextPage != "" || state.Previous != "" || state.StartAt != 0 || state.Offset != 0 || state.Pages != 0)) {
		return cursor{}, domain.NewValidationError("jira.checkpoint", "position or timestamp is invalid")
	}
	return state, nil
}

func scanBound(watermark time.Time, since *time.Time) time.Time {
	var bound time.Time
	if !watermark.IsZero() {
		bound = watermark.Add(-watermarkOverlap).UTC()
	}
	if since != nil && (bound.IsZero() || since.UTC().After(bound)) {
		bound = since.UTC()
	}
	return bound
}

func pageMaxUpdated(issues []issue) time.Time {
	var latest time.Time
	for _, value := range issues {
		latest = laterTime(latest, value.Fields.Updated)
	}
	return latest
}

// samePagePrefix reports whether ids starts with the previously returned keys.
// A mismatch means the numbered page shifted and its offset is unsafe.
func samePagePrefix(ids, seen []string) bool {
	if len(seen) == 0 {
		return true
	}
	if len(ids) < len(seen) {
		return false
	}
	for index := range seen {
		if ids[index] != seen[index] {
			return false
		}
	}
	return true
}

func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b.UTC()
	}
	return a
}

func (s *Source) issueItem(ctx context.Context, token, connectionID string, value issue) (domain.Item, error) {
	if strings.TrimSpace(value.Key) == "" || strings.TrimSpace(value.Fields.Project.Key) == "" {
		return domain.Item{}, errors.New("Jira issue is missing its key or project key")
	}
	comments, err := s.comments(ctx, token, value.Key)
	if err != nil {
		return domain.Item{}, err
	}
	var bodyText strings.Builder
	if status := strings.TrimSpace(value.Fields.Status.Name); status != "" {
		bodyText.WriteString("**Status:** " + status + "\n\n")
	}
	if project := strings.TrimSpace(value.Fields.Project.Name); project != "" {
		bodyText.WriteString("**Project:** " + project + "\n\n")
	}
	if issueType := strings.TrimSpace(value.Fields.IssueType.Name); issueType != "" {
		bodyText.WriteString("**Issue type:** " + issueType + "\n\n")
	}
	bodyText.WriteString(value.Fields.Description.markdown())
	body := boundedBody(bodyText.String(), comments)
	title := strings.TrimSpace(value.Fields.Summary)
	if title == "" {
		title = value.Key
	}
	author := strings.TrimSpace(value.Fields.Reporter.DisplayName)
	if author == "" {
		author = strings.TrimSpace(value.Fields.Assignee.DisplayName)
	}
	labels := append([]string(nil), value.Fields.Labels...)
	sort.Strings(labels)
	itemURL := s.baseURL + "/browse/" + url.PathEscape(value.Key)
	return domain.Item{
		Ref:  domain.ItemRef{ConnectionID: connectionID, ExternalID: value.Fields.Project.Key + "/" + value.Key},
		Kind: domain.ItemIssue, Title: title, Body: body, URL: itemURL, Author: author,
		State:  normalizeJiraState(value.Fields.Status.Name, value.Fields.Status.StatusCategory.Key),
		Labels: labels, CreatedAt: value.Fields.Created, UpdatedAt: value.Fields.Updated,
		ProjectID: singleConnectionProject(s.connection.Projects),
	}, nil
}

// normalizeJiraState maps Jira's done category to closed and every other known
// category to open. Older servers omit statusCategory; the status name is then
// the fallback. An empty status yields an empty state.
func normalizeJiraState(name, categoryKey string) domain.ItemState {
	switch category := strings.ToLower(strings.TrimSpace(categoryKey)); category {
	case "done":
		return domain.ItemStateClosed
	case "":
		// Fall through to the name fallback for older servers.
	default:
		return domain.ItemStateOpen
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "done", "closed", "resolved", "fixed", "complete", "completed":
		return domain.ItemStateClosed
	case "":
		return ""
	default:
		return domain.ItemStateOpen
	}
}

func (s *Source) comments(ctx context.Context, token, key string) ([]jiraComment, error) {
	query := url.Values{"maxResults": {strconv.Itoa(maxCommentsPerIssue)}, "orderBy": {"-created"}, "startAt": {"0"}}
	var result struct {
		Comments []jiraComment `json:"comments"`
	}
	if err := s.get(ctx, token, s.apiRoot()+"/issue/"+url.PathEscape(key)+"/comment", query, &result); err != nil {
		return nil, err
	}
	comments := make([]jiraComment, 0, maxCommentsPerIssue)
	for _, comment := range result.Comments {
		if strings.TrimSpace(comment.Body.markdown()) == "" {
			continue
		}
		comments = append(comments, comment)
	}
	sort.SliceStable(comments, func(i, j int) bool {
		if !comments[i].Created.Equal(comments[j].Created) {
			return comments[i].Created.After(comments[j].Created)
		}
		return comments[i].ID > comments[j].ID
	})
	if len(comments) > maxCommentsPerIssue {
		comments = comments[:maxCommentsPerIssue]
	}
	return comments, nil
}

func boundedBody(description string, comments []jiraComment) string {
	var out strings.Builder
	appendBounded(&out, strings.TrimSpace(description), maxIssueBodyBytes)
	if len(comments) > 0 {
		appendBounded(&out, "\n\n### Comments\n", maxIssueBodyBytes)
	}
	for _, comment := range comments {
		text := capUTF8(strings.TrimSpace(comment.Body.markdown()), maxCommentBytes)
		if text == "" {
			continue
		}
		author := strings.TrimSpace(comment.Author.DisplayName)
		if author == "" {
			author = "Jira user"
		}
		entry := "\n- **" + author + "** (" + comment.Created.UTC().Format(time.RFC3339) + "): " + text + "\n"
		entry = capUTF8(entry, maxCommentBytes)
		appendBounded(&out, entry, maxIssueBodyBytes)
		if out.Len() >= maxIssueBodyBytes {
			break
		}
	}
	return out.String()
}

func appendBounded(out *strings.Builder, value string, limit int) {
	remaining := limit - out.Len()
	if remaining > 0 {
		out.WriteString(capUTF8(value, remaining))
	}
}

func capUTF8(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func (s *Source) token(ctx context.Context) (string, error) {
	token, err := s.credentials.Get(ctx, s.connection.ID, "token")
	if err != nil {
		return "", errors.New(connectors.SanitizeError(err, token))
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("token is empty")
	}
	return token, nil
}

func (s *Source) get(ctx context.Context, token, endpoint string, query url.Values, out any) error {
	requestURL := endpoint
	if len(query) != 0 {
		requestURL += "?" + query.Encode()
	}
	header := make(http.Header)
	header.Set("Accept", "application/json")
	if s.deployment == "cloud" {
		header.Set("Authorization", "Basic "+basicAuth(strings.TrimSpace(s.connection.Settings["email"]), token))
	} else {
		header.Set("Authorization", "Bearer "+token)
	}
	return s.http.GetJSON(ctx, requestURL, header, out)
}

func basicAuth(email, token string) string {
	return base64.StdEncoding.EncodeToString([]byte(email + ":" + token))
}

// jiraComment and issue model only the fields consumed by the adapter.
type jiraComment struct {
	ID      string    `json:"id"`
	Author  jiraUser  `json:"author"`
	Created time.Time `json:"created"`
	Body    jiraText  `json:"body"`
}

type jiraUser struct {
	DisplayName string `json:"displayName"`
}

type issue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string    `json:"summary"`
		Description jiraText  `json:"description"`
		Labels      []string  `json:"labels"`
		Created     time.Time `json:"created"`
		Updated     time.Time `json:"updated"`
		Reporter    jiraUser  `json:"reporter"`
		Assignee    jiraUser  `json:"assignee"`
		Status      struct {
			Name           string `json:"name"`
			StatusCategory struct {
				Key string `json:"key"`
			} `json:"statusCategory"`
		} `json:"status"`
		IssueType struct {
			Name string `json:"name"`
		} `json:"issuetype"`
		Project struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"project"`
	} `json:"fields"`
}

func singleConnectionProject(projects []string) string {
	if len(projects) == 1 {
		return projects[0]
	}
	return ""
}
