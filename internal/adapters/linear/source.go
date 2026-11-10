package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	checkpointStream  = "linear"
	checkpointVersion = 1

	// pageMaxFirst caps the GraphQL `first` argument of one request.
	pageMaxFirst       = 100
	sourceDefaultLimit = 25
	sourceMaxLimit     = 500

	// maxPagesPerRead bounds the provider requests of one Read call, so a
	// batch never turns into an unbounded crawl. The cursor keeps the Relay
	// position, so the next batch continues without re-reading finished
	// pages.
	maxPagesPerRead = 10

	// maxCommentCount, maxCommentBytes and maxSourceBodyBytes bound the
	// comments embedded in one item body: at most five comments, 16 KiB
	// each, and 256 KiB for the whole body (domain.MaxItemBodyBytes is
	// 512 KiB).
	maxCommentCount    = 5
	maxCommentBytes    = 16 << 10
	maxSourceBodyBytes = 256 << 10

	maxResponseBytes = 8 << 20

	// pageOverlap replays a short window before the durable watermark on the
	// next pass. An issue updated while a pass is running can move across a
	// Relay page boundary; the overlap re-reads the newest window and turns
	// a possible missed update into an idempotent upsert.
	pageOverlap = 5 * time.Minute
)

// Descriptor is the static Linear source description used by the registry.
//
// The team filter and auth scheme are non-secret connection settings; the
// token lives only in the credential store under conn/<id>/token.
//
// authscheme accepts:
//   - "api_key" (default): the token is sent as-is in the Authorization
//     header, which is what Linear personal API keys require.
//   - "oauth": the token is sent as "Bearer <token>", which is what Linear
//     OAuth access tokens require.
func Descriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider: Name, DisplayName: "Linear",
		Capabilities:   domain.Capabilities{Backfill: true, Incremental: true, AuthModes: []domain.AuthMode{domain.AuthToken}},
		DefaultPublish: domain.PublishShared,
		ConfigFields: []domain.ConfigField{
			{Key: "token", Label: "API token", Required: true, Secret: true, Help: "Linear personal API key (authscheme api_key) or OAuth access token (authscheme oauth)."},
			{Key: "teams", Label: "Teams", Help: "Comma or newline separated team keys or IDs; empty reads all teams."},
			{Key: "authscheme", Label: "Auth scheme", Help: "api_key (default): send the token as-is in Authorization; oauth: send it as Bearer. Linear personal API keys use api_key, OAuth access tokens use oauth."},
		},
		Backends: []domain.Backend{domain.BackendNative},
		Streams:  []string{checkpointStream},
	}
}

// Source is the item-level read-only Linear integration. It only issues
// POST requests against the configured GraphQL endpoint.
type Source struct {
	connection  domain.Connection
	baseURL     string
	teams       []string
	authScheme  string
	credentials ports.CredentialProvider
	http        *http.Client
}

// linearCursor is the checkpoint payload of the single "linear" stream:
// the Relay position (after/offset) of the current pass plus the durable
// watermark of the last completed pass.
type linearCursor struct {
	// Watermark is the newest provider timestamp of the last completed pass.
	Watermark time.Time `json:"watermark,omitempty"`
	// PassMax is the newest timestamp returned in the current pass. It is
	// folded into Watermark when the pass completes.
	PassMax time.Time `json:"passMax,omitempty"`
	// After is the Relay endCursor of the last fully consumed page.
	After *string `json:"after,omitempty"`
	// Offset is the number of nodes already returned from the current page.
	Offset int `json:"offset,omitempty"`
}

// NewSource constructs the native source using credentials under conn/token.
// conn.Settings["teams"] accepts comma or newline separated team keys or
// IDs; conn.Settings["authscheme"] is "api_key" (default) or "oauth".
func NewSource(conn domain.Connection, credentials ports.CredentialProvider, client *http.Client, now func() time.Time) (*Source, error) {
	if err := Descriptor().ValidateConnection(conn); err != nil {
		return nil, err
	}
	if credentials == nil {
		return nil, domain.NewValidationError("linear.credentials", "is required")
	}
	teams, err := parseTeams(conn.Settings["teams"])
	if err != nil {
		return nil, err
	}
	authScheme, err := parseAuthScheme(conn.Settings["authscheme"])
	if err != nil {
		return nil, err
	}
	baseURL, err := normalizeBaseURL(conn.BaseURL)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// Never follow redirects with the token attached: a redirect target
	// would receive the Authorization header.
	cloned := *client
	cloned.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	_ = now // Accepted for signature parity with the other native sources; the scan bound derives from stored watermarks, not the clock.
	return &Source{
		connection:  conn,
		baseURL:     baseURL,
		teams:       teams,
		authScheme:  authScheme,
		credentials: credentials,
		http:        &cloned,
	}, nil
}

func parseTeams(raw string) ([]string, error) {
	var teams []string
	seen := make(map[string]struct{})
	for _, value := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		team := strings.TrimSpace(value)
		if team == "" {
			continue
		}
		if len(team) > 128 {
			return nil, domain.NewValidationError("linear.teams", "team keys or IDs must be at most 128 characters")
		}
		for _, r := range team {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
				continue
			}
			return nil, domain.NewValidationError("linear.teams", fmt.Sprintf("team %q contains an invalid character", team))
		}
		if _, ok := seen[team]; ok {
			continue
		}
		seen[team] = struct{}{}
		teams = append(teams, team)
	}
	sort.Strings(teams)
	return teams, nil
}

func parseAuthScheme(raw string) (string, error) {
	scheme := strings.TrimSpace(raw)
	if scheme == "" {
		return "api_key", nil
	}
	if scheme != "api_key" && scheme != "oauth" {
		return "", domain.NewValidationError("linear.authscheme", `must be "api_key" or "oauth"`)
	}
	return scheme, nil
}

// normalizeBaseURL keeps the configured GraphQL endpoint as-is: the default
// is the public Linear endpoint, while a self-hosted or test URL supplies
// its own full endpoint path. Userinfo, query strings and fragments are
// rejected so a token can never be smuggled in through the URL.
func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = DefaultBaseURL
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || !isAllowedBaseURL(parsed) || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return "", domain.NewValidationError("linear.baseUrl", "must be an https GraphQL endpoint URL without credentials or query; http is allowed only for loopback hosts")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// Descriptor returns the provider's static description.
func (s *Source) Descriptor() domain.SourceDescriptor { return Descriptor() }

// Check verifies the token with the cheapest Linear call: the authenticated
// viewer. It reads no issues and writes nothing.
func (s *Source) Check(ctx context.Context) error {
	token, err := s.token(ctx)
	if err != nil {
		return fmt.Errorf("linear check: %s", connectors.SanitizeError(err))
	}
	var response gqlResponse
	if err := s.postGraphQL(ctx, token, gqlRequest{Query: viewerQuery}, &response); err != nil {
		return fmt.Errorf("linear check: %s", connectors.SanitizeError(err, token))
	}
	viewer := response.DataViewer()
	if viewer == nil || strings.TrimSpace(viewer.ID) == "" || strings.TrimSpace(viewer.Name) == "" {
		return errors.New("linear check: viewer response is incomplete")
	}
	return nil
}

// Read returns the next bounded page of issues and its resumable cursor.
//
// Cursor semantics:
//   - the single "linear" stream carries a Relay position (after/offset)
//     plus the durable watermark of the last completed pass;
//   - the watermark advances only after a complete pass (a page with
//     hasNextPage false is fully consumed); until then the position keeps
//     the in-progress page, so a crash replays at most the current page;
//   - the next pass starts from the watermark minus a five-minute overlap;
//   - More is true exactly while the pass is incomplete (item limit hit,
//     page budget exhausted with a next page pending, or a next page
//     announced).
//
// Time bounds:
//   - ReadRequest.Since and the durable watermark are inclusive lower bounds
//     on issue update time, sent as the GraphQL updatedAt gte filter; the
//     source additionally keeps only items at or after the bound, and never
//     reports an item missing from a filtered read as deleted (this source
//     never emits tombstones).
func (s *Source) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, err
	}
	if req.Connection.ID != s.connection.ID || req.Connection.Provider != Name {
		return ports.ReadBatch{}, domain.NewValidationError("linear.connection", "does not match this source")
	}
	token, err := s.token(ctx)
	if err != nil {
		return ports.ReadBatch{}, fmt.Errorf("linear credentials: %s", connectors.SanitizeError(err))
	}
	limit := normalizeLimit(req.Limit)
	cursor, err := decodeCursor(req.Checkpoints)
	if err != nil {
		return ports.ReadBatch{}, err
	}
	bound := scanBound(cursor, req.Since)
	items := make([]domain.Item, 0, limit)
	after := cursor.After
	offset := cursor.Offset
	passMax := cursor.PassMax
	pagesRead := 0
	complete := false
	for len(items) < limit && pagesRead < maxPagesPerRead {
		if err := ctx.Err(); err != nil {
			return ports.ReadBatch{}, err
		}
		first := min(limit-len(items), pageMaxFirst)
		connection, err := s.queryIssues(ctx, token, first, after, bound)
		if err != nil {
			return ports.ReadBatch{}, fmt.Errorf("linear read: %s", connectors.SanitizeError(err, token))
		}
		pagesRead++
		nodes := connection.Nodes
		if offset > len(nodes) {
			return ports.ReadBatch{}, domain.NewValidationError("linear.checkpoint", "offset exceeds provider page")
		}
		rest := nodes[offset:]
		consumed := offset
		for i := range rest {
			if len(items) >= limit {
				break
			}
			consumed++
			item := toItem(s.connection.ID, s.itemProjectID(), rest[i])
			if bound != nil && (item.UpdatedAt.IsZero() || item.UpdatedAt.Before(*bound)) {
				continue
			}
			items = append(items, item)
			if item.UpdatedAt.After(passMax) {
				passMax = item.UpdatedAt.UTC()
			}
		}
		if len(items) >= limit {
			if consumed < len(nodes) || connection.PageInfo.HasNextPage {
				// Stopped early, or exactly at a page end with more pages:
				// resume inside/after this page.
				offset = consumed
				if consumed >= len(nodes) && connection.PageInfo.HasNextPage {
					next, ok := pageEndCursor(connection.PageInfo)
					if !ok {
						return ports.ReadBatch{}, errors.New("linear read: page announces a next page without a cursor")
					}
					after, offset = next, 0
				}
			} else {
				complete = true
				after, offset = nil, 0
			}
			break
		}
		if connection.PageInfo.HasNextPage {
			next, ok := pageEndCursor(connection.PageInfo)
			if !ok {
				return ports.ReadBatch{}, errors.New("linear read: page announces a next page without a cursor")
			}
			after, offset = next, 0
			continue
		}
		complete = true
		after, offset = nil, 0
		break
	}
	more := !complete
	next := linearCursor{Watermark: cursor.Watermark, PassMax: passMax, After: after, Offset: offset}
	if complete {
		if !passMax.IsZero() && passMax.After(next.Watermark) {
			next.Watermark = passMax.UTC()
		}
		next.PassMax = time.Time{}
	}
	data, err := json.Marshal(next)
	if err != nil {
		return ports.ReadBatch{}, fmt.Errorf("encode linear checkpoint: %w", err)
	}
	return ports.ReadBatch{
		Items:       items,
		Checkpoints: []domain.Checkpoint{{Stream: checkpointStream, Version: checkpointVersion, Data: data}},
		More:        more,
	}, nil
}

// decodeCursor reads the single-stream checkpoint. Any malformed or
// unsupported cursor is rejected explicitly: silently restarting a stream
// would re-read history and could hide corruption.
func decodeCursor(checkpoints map[string]domain.Checkpoint) (linearCursor, error) {
	checkpoint, ok := checkpoints[checkpointStream]
	if !ok {
		return linearCursor{}, nil
	}
	if checkpoint.Stream != checkpointStream || checkpoint.Version != checkpointVersion {
		return linearCursor{}, domain.NewValidationError("linear.checkpoint",
			fmt.Sprintf("stream %q version %d is not supported", checkpoint.Stream, checkpoint.Version))
	}
	var cursor linearCursor
	if err := json.Unmarshal(checkpoint.Data, &cursor); err != nil {
		return linearCursor{}, domain.NewValidationError("linear.checkpoint", "is not valid JSON")
	}
	if cursor.Offset < 0 || cursor.Offset > pageMaxFirst {
		return linearCursor{}, domain.NewValidationError("linear.checkpoint", "position is invalid")
	}
	return cursor, nil
}

// scanBound is the inclusive lower bound of the current pass: the durable
// watermark minus the overlap window, narrowed by ReadRequest.Since.
func scanBound(cursor linearCursor, since *time.Time) *time.Time {
	var bound *time.Time
	if !cursor.Watermark.IsZero() {
		at := cursor.Watermark.Add(-pageOverlap).UTC()
		bound = &at
	}
	if since != nil {
		at := since.UTC()
		if bound == nil || at.After(*bound) {
			bound = &at
		}
	}
	return bound
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return sourceDefaultLimit
	}
	if limit > sourceMaxLimit {
		return sourceMaxLimit
	}
	return limit
}

func pageEndCursor(info gqlPageInfo) (*string, bool) {
	if info.EndCursor == nil || strings.TrimSpace(*info.EndCursor) == "" {
		return nil, false
	}
	cursor := strings.TrimSpace(*info.EndCursor)
	return &cursor, true
}

// itemProjectID maps items into the brain project only when the connection
// feeds exactly one project; with several projects the mapping is ambiguous
// and the host policy decides.
func (s *Source) itemProjectID() string {
	if len(s.connection.Projects) == 1 {
		return s.connection.Projects[0]
	}
	return ""
}

func (s *Source) token(ctx context.Context) (string, error) {
	token, err := s.credentials.Get(ctx, s.connection.ID, "token")
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("token is empty")
	}
	return token, nil
}

func (s *Source) authHeader(token string) string {
	if s.authScheme == "oauth" {
		return "Bearer " + token
	}
	return token
}

const viewerQuery = `query LinearViewer { viewer { id name } }`

const issuesQuery = `query LinearIssues($first: Int, $after: String, $filter: IssueFilter, $orderBy: PaginationOrderBy) {
  issues(first: $first, after: $after, filter: $filter, orderBy: $orderBy) {
    nodes {
      id identifier title description url createdAt updatedAt
      state { name type }
      team { key }
      labels(first: 50) { nodes { name } }
      assignee { name }
      comments(last: 5, orderBy: createdAt) { nodes { id body createdAt updatedAt user { name } } }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type gqlResponse struct {
	Data   *gqlData   `json:"data"`
	Errors []gqlError `json:"errors,omitempty"`
}

type gqlError struct {
	Message string `json:"message,omitempty"`
}

type gqlData struct {
	Viewer *gqlViewer          `json:"viewer,omitempty"`
	Issues *gqlIssueConnection `json:"issues,omitempty"`
}

// DataViewer returns the viewer payload, if any.
func (r gqlResponse) DataViewer() *gqlViewer {
	if r.Data == nil {
		return nil
	}
	return r.Data.Viewer
}

type gqlViewer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type gqlIssueConnection struct {
	Nodes    []gqlIssue  `json:"nodes"`
	PageInfo gqlPageInfo `json:"pageInfo"`
}

type gqlPageInfo struct {
	HasNextPage bool    `json:"hasNextPage"`
	EndCursor   *string `json:"endCursor"`
}

type gqlName struct {
	Name string `json:"name"`
	// Type is Linear's state category (backlog, unstarted, started,
	// completed, canceled); empty for names that are not workflow states.
	Type string `json:"type,omitempty"`
}

type gqlTeam struct {
	Key string `json:"key"`
}

type gqlLabelConnection struct {
	Nodes []gqlName `json:"nodes"`
}

type gqlCommentConnection struct {
	Nodes []gqlComment `json:"nodes"`
}

type gqlComment struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	User      *gqlName  `json:"user"`
}

type gqlIssue struct {
	ID          string                `json:"id"`
	Identifier  string                `json:"identifier"`
	Title       string                `json:"title"`
	Description string                `json:"description"`
	URL         string                `json:"url"`
	CreatedAt   time.Time             `json:"createdAt"`
	UpdatedAt   time.Time             `json:"updatedAt"`
	State       *gqlName              `json:"state"`
	Team        *gqlTeam              `json:"team"`
	Labels      *gqlLabelConnection   `json:"labels"`
	Assignee    *gqlName              `json:"assignee"`
	Comments    *gqlCommentConnection `json:"comments"`
}

// queryIssues fetches one Relay page of issues ordered by updatedAt. The
// bound is an inclusive lower bound sent as the updatedAt gte filter; teams
// narrow the page with filter.team.key.in. Comments ride along in the same
// query so one page costs exactly one request (no N+1).
func (s *Source) queryIssues(ctx context.Context, token string, first int, after *string, bound *time.Time) (gqlIssueConnection, error) {
	filter := make(map[string]any)
	if bound != nil {
		filter["updatedAt"] = map[string]any{"gte": bound.UTC().Format(time.RFC3339Nano)}
	}
	if len(s.teams) > 0 {
		filter["team"] = map[string]any{"key": map[string]any{"in": s.teams}}
	}
	variables := map[string]any{"first": first, "filter": filter, "orderBy": "updatedAt"}
	if after != nil {
		variables["after"] = *after
	}
	var response gqlResponse
	if err := s.postGraphQL(ctx, token, gqlRequest{Query: issuesQuery, Variables: variables}, &response); err != nil {
		return gqlIssueConnection{}, err
	}
	if response.Data == nil || response.Data.Issues == nil {
		return gqlIssueConnection{}, errors.New("linear: issues response has no data")
	}
	return *response.Data.Issues, nil
}

// postGraphQL sends one GraphQL request. Non-2xx responses and GraphQL
// error payloads fail closed with errors that carry neither the token nor
// the response body.
func (s *Source) postGraphQL(ctx context.Context, token string, payload gqlRequest, out *gqlResponse) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(payload); err != nil {
		return fmt.Errorf("encode linear request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL, &body)
	if err != nil {
		return fmt.Errorf("build linear request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", DefaultUserAgent)
	request.Header.Set("Authorization", s.authHeader(token))
	response, err := s.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("linear request failed: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read linear response: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("linear response exceeds %d bytes", maxResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// The body is deliberately not rendered: an upstream error page may
		// echo request details. Only the status travels outward.
		return fmt.Errorf("linear: http %d %s", response.StatusCode, http.StatusText(response.StatusCode))
	}
	var decoded gqlResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Errorf("decode linear response: %w", err)
	}
	if len(decoded.Errors) > 0 {
		// GraphQL error messages may quote the query or upstream state, so
		// only the fact that errors were returned travels outward.
		return errors.New("linear: graphql response contains errors")
	}
	*out = decoded
	return nil
}

// toItem converts one Linear issue into a raw-zone item. The Linear
// description is already Markdown and is kept as-is; comments ride along in
// the body, newest first-bounded and byte-capped.
func toItem(connectionID, projectID string, issue gqlIssue) domain.Item {
	externalID := strings.TrimSpace(issue.Identifier)
	if externalID == "" {
		externalID = strings.TrimSpace(issue.ID)
	}
	title := strings.TrimSpace(issue.Title)
	if title == "" {
		title = externalID
	}
	var author string
	if issue.Assignee != nil {
		author = strings.TrimSpace(issue.Assignee.Name)
	}
	return domain.Item{
		Ref:       domain.ItemRef{ConnectionID: connectionID, ExternalID: externalID},
		Kind:      domain.ItemIssue,
		Title:     title,
		Body:      issueBody(issue.Description, issueComments(issue.Comments)),
		URL:       strings.TrimSpace(issue.URL),
		Author:    author,
		State:     normalizeLinearState(issue.State),
		Labels:    issueLabels(issue.Labels),
		CreatedAt: issue.CreatedAt,
		UpdatedAt: issue.UpdatedAt,
		ProjectID: projectID,
	}
}

// normalizeLinearState maps Linear workflow state types to normalized item
// states. completed and canceled are closed; started, unstarted and backlog
// are open. The state name is the fallback when an older API omits type.
func normalizeLinearState(state *gqlName) domain.ItemState {
	if state == nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(state.Type)) {
	case "completed", "canceled", "cancelled":
		return domain.ItemStateClosed
	case "started", "unstarted", "backlog", "planned", "triage":
		return domain.ItemStateOpen
	}
	switch strings.ToLower(strings.TrimSpace(state.Name)) {
	case "done", "completed", "canceled", "cancelled":
		return domain.ItemStateClosed
	case "":
		return ""
	default:
		return domain.ItemStateOpen
	}
}

func issueLabels(labels *gqlLabelConnection) []string {
	if labels == nil {
		return nil
	}
	out := make([]string, 0, len(labels.Nodes))
	for _, label := range labels.Nodes {
		if text := strings.TrimSpace(label.Name); text != "" {
			out = append(out, text)
		}
	}
	sort.Strings(out)
	return out
}

// issueComments returns at most the five newest non-empty comments in
// chronological order with a stable tie-breaker, so identical upstream data
// always produces identical text.
func issueComments(connection *gqlCommentConnection) []gqlComment {
	if connection == nil {
		return nil
	}
	kept := make([]gqlComment, 0, len(connection.Nodes))
	for _, comment := range connection.Nodes {
		if strings.TrimSpace(comment.Body) == "" {
			continue
		}
		kept = append(kept, comment)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if !kept[i].CreatedAt.Equal(kept[j].CreatedAt) {
			return kept[i].CreatedAt.Before(kept[j].CreatedAt)
		}
		return kept[i].ID < kept[j].ID
	})
	if len(kept) > maxCommentCount {
		kept = kept[len(kept)-maxCommentCount:]
	}
	return kept
}

func issueBody(description string, comments []gqlComment) string {
	var body strings.Builder
	appendBounded(&body, strings.TrimSpace(description), maxSourceBodyBytes)
	if len(comments) == 0 {
		return body.String()
	}
	var rendered strings.Builder
	rendered.WriteString("\n\n### Comments\n")
	for _, comment := range comments {
		author := "unknown"
		if comment.User != nil && strings.TrimSpace(comment.User.Name) != "" {
			author = strings.TrimSpace(comment.User.Name)
		}
		header := "\n- **" + author + "** (" + connectors.FormatTime(comment.CreatedAt) + "): "
		rendered.WriteString(header)
		rendered.WriteString(capUTF8(strings.TrimSpace(comment.Body), maxCommentBytes))
		rendered.WriteString("\n")
	}
	appendBounded(&body, rendered.String(), maxSourceBodyBytes)
	return body.String()
}

func appendBounded(out *strings.Builder, value string, limit int) {
	remaining := limit - out.Len()
	if remaining <= 0 {
		return
	}
	out.WriteString(capUTF8(value, remaining))
}

// capUTF8 truncates value to at most limit bytes without splitting a rune, so
// a truncated body stays valid UTF-8 and still passes domain.Item.Validate.
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

var _ ports.Source = (*Source)(nil)
