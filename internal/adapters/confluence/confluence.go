// Package confluence implements a read-only, native Confluence Cloud and Data
// Center source for the item-level ingestion contract.
//
// The source only issues authenticated GET requests. Cloud authenticates with
// HTTP Basic (email + API token); Data Center authenticates with a Bearer
// token. Tokens are resolved from the credential provider at read time and are
// never stored on the Source, placed in a URL or rendered in an error.
//
// Confluence Cloud silently caps a search that expands body.storage at 50
// results. The reader therefore never treats one short page as the end of a
// pass: it keeps advancing the numeric start offset until the provider
// returns an empty page or an explicit totalSize is reached.
package confluence

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// Name is the native provider name.
	Name = "confluence"
	// DefaultUserAgent identifies the engine to Confluence.
	DefaultUserAgent = "lv24-brain"

	checkpointStream  = "confluence"
	checkpointVersion = 1

	// Provider page size. Confluence Cloud truncates a body.storage search at
	// 50 results even when limit=100, so the reader advances start by the
	// number of rows actually returned and confirms completion with an empty
	// page or an explicit totalSize smaller than the remaining range.
	providerPageSize   = 100
	sourceDefaultLimit = 25
	sourceMaxLimit     = 500

	sourceMaxPagesPerRead   = 10
	sourceMaxPagesPerStream = 10_000

	expandFields = "body.storage,version,space,metadata.labels"

	maxSourceBodyBytes    = 512 << 10
	maxCommentsPerItem    = 5
	maxCommentBytes       = 16 << 10
	maxCommentsTotalBytes = 256 << 10

	// A completed pass restarts from the watermark minus this overlap. CQL
	// lastmodified is minute-granular, so a short window turns an update that
	// landed during the previous pass into an idempotent upsert.
	pageOverlap = 5 * time.Minute

	// Confluence CQL accepts a date or date-time; this is the canonical UTC
	// minute form. Local filtering still uses the full-precision bound.
	cqlTimeLayout = "2006-01-02 15:04"

	deploymentCloud      = "cloud"
	deploymentDataCenter = "datacenter"
)

// Descriptor is the static provider description used by the registry.
func Descriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider:    Name,
		DisplayName: "Confluence",
		Capabilities: domain.Capabilities{
			Backfill:    true,
			Incremental: true,
			AuthModes:   []domain.AuthMode{domain.AuthToken},
		},
		DefaultPublish: domain.PublishShared,
		Backends:       []domain.Backend{domain.BackendNative},
		ConfigFields: []domain.ConfigField{
			{Key: "token", Label: "API token", Required: true, Secret: true,
				Help: "Confluence Cloud API token or Data Center personal access token."},
			{Key: "email", Label: "Email", Required: false,
				Help: "Atlassian account email; required for Confluence Cloud Basic authentication."},
			{Key: "deployment", Label: "Deployment", Required: false,
				Help: `"cloud" (default) or "datacenter".`},
			{Key: "spaces", Label: "Space keys", Required: false,
				Help: "Optional comma-separated Confluence space keys to restrict the search."},
			{Key: "cql", Label: "Custom CQL", Required: false,
				Help: "Optional read-only CQL appended to the generated lastmodified/space filter."},
		},
		Streams: []string{checkpointStream},
	}
}

// Source is the native Confluence item source.
type Source struct {
	connection  domain.Connection
	apiBase     string
	siteBase    string
	deployment  string
	email       string
	spaces      []string
	customCQL   string
	credentials ports.CredentialProvider
	http        *connectors.HTTP
	now         func() time.Time

	// orderByDisabled records a Data Center instance that rejected
	// "ORDER BY lastmodified ASC" with HTTP 400. The flag avoids paying for
	// the rejected request on every page of a long backfill.
	orderByDisabled atomic.Bool
}

// NewSource validates a Confluence connection and builds its native source.
//
// conn.BaseURL is required: Cloud looks like https://site.atlassian.net/wiki
// and Data Center like https://host/confluence. The API prefix is appended
// exactly once (/wiki/rest/api for Cloud, /rest/api for Data Center). Userinfo,
// query strings and fragments are rejected, https is required by default and
// plain http is accepted only for loopback hosts.
func NewSource(conn domain.Connection, credentials ports.CredentialProvider, client *http.Client, now func() time.Time) (*Source, error) {
	if credentials == nil {
		return nil, domain.NewValidationError("confluence.credentials", "are required")
	}
	if err := Descriptor().ValidateConnection(conn); err != nil {
		return nil, err
	}
	deployment, err := parseDeployment(conn.Settings["deployment"])
	if err != nil {
		return nil, err
	}
	spaces, err := parseSpaces(conn.Settings["spaces"])
	if err != nil {
		return nil, err
	}
	customCQL, err := normalizeCustomCQL(conn.Settings["cql"])
	if err != nil {
		return nil, err
	}
	apiBase, siteBase, err := normalizeBaseURL(conn.BaseURL, deployment)
	if err != nil {
		return nil, err
	}
	email := strings.TrimSpace(conn.Settings["email"])
	if deployment == deploymentCloud && email == "" {
		return nil, domain.NewValidationError("confluence.email", "is required for Confluence Cloud Basic authentication")
	}
	if now == nil {
		now = time.Now
	}
	return &Source{
		connection:  conn,
		apiBase:     apiBase,
		siteBase:    siteBase,
		deployment:  deployment,
		email:       email,
		spaces:      spaces,
		customCQL:   customCQL,
		credentials: credentials,
		http:        connectors.NewHTTP(connectors.HTTPOptions{Client: client, Now: now}),
		now:         now,
	}, nil
}

// Descriptor returns the source's static provider description.
func (s *Source) Descriptor() domain.SourceDescriptor { return Descriptor() }

// Check validates credentials and configuration with one read-only request to
// the current-user endpoint.
func (s *Source) Check(ctx context.Context) error {
	token, err := s.token(ctx)
	if err != nil {
		return fmt.Errorf("confluence check: %s", connectors.SanitizeError(err))
	}
	var identity userIdentity
	if _, err := s.getJSON(ctx, token, s.apiBase+"/user/current", &identity); err != nil {
		return fmt.Errorf("confluence check: %s", connectors.SanitizeError(err, token))
	}
	if strings.TrimSpace(identity.Username) == "" && strings.TrimSpace(identity.AccountID) == "" {
		return errors.New("confluence check: authenticated user response is incomplete")
	}
	return nil
}

// Read returns the next bounded page of the single "confluence" stream.
//
// Cursor semantics:
//   - one checkpoint carries the durable watermark, the numeric start of the
//     current page, an offset inside that page and the page IDs already
//     returned, so a crash replays at most the current page;
//   - the watermark advances only after a complete pass; the next pass starts
//     at watermark-overlap narrowed by ReadRequest.Since;
//   - More is true exactly while at least one provider page or page offset is
//     still pending;
//   - req.Since is an inclusive lower bound and narrows an existing cursor.
//
// Known limitation: Confluence uses numeric start paging. A page updated while
// an initial backfill is running can shift across an already consumed page
// boundary and be missed for that pass; the five-minute watermark overlap
// repairs the usual recent case, and the current page offset is replay-safe,
// but there is no forced full re-backfill.
//
// Nothing is ever inferred as deleted: an item that disappears from a page is
// simply not returned, and the source does not declare the Deletes capability.
func (s *Source) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, err
	}
	if req.Connection.ID != s.connection.ID || req.Connection.Provider != Name {
		return ports.ReadBatch{}, domain.NewValidationError("confluence.connection", "does not match this source")
	}
	token, err := s.token(ctx)
	if err != nil {
		return ports.ReadBatch{}, fmt.Errorf("confluence credentials: %s", connectors.SanitizeError(err))
	}
	cursor, err := readSourceCursor(req.Checkpoints)
	if err != nil {
		return ports.ReadBatch{}, err
	}
	if cursor.Done {
		// A completed pass starts the next incremental pass. The watermark
		// stays durable; only the page walk restarts.
		cursor.Start, cursor.Offset, cursor.Seen, cursor.Done = 0, 0, nil, false
		cursor.PassMax = time.Time{}
	}
	limit := sourceLimit(req.Limit)
	bound := scanBound(cursor.Watermark, req.Since)
	baseCQL := s.buildCQL(bound)
	items := make([]domain.Item, 0, limit)
	pagesRead := 0
	more := false
	for len(items) < limit && pagesRead < sourceMaxPagesPerRead {
		if err := ctx.Err(); err != nil {
			return ports.ReadBatch{}, err
		}
		if cursor.Start > sourceMaxPagesPerStream*providerPageSize {
			return ports.ReadBatch{}, fmt.Errorf("confluence: pagination exceeded %d pages", sourceMaxPagesPerStream)
		}
		page, err := s.searchPage(ctx, token, baseCQL, cursor.Start)
		if err != nil {
			return ports.ReadBatch{}, fmt.Errorf("confluence read: %s", connectors.SanitizeError(err, token))
		}
		pagesRead++
		results := append([]searchResult(nil), page.Results...)
		// A provider without ORDER BY support may return a page in an
		// arbitrary order. Sorting by update time then ID makes a replay of
		// the same page deterministic.
		sort.SliceStable(results, func(i, j int) bool {
			left, right := results[i].UpdatedAt(), results[j].UpdatedAt()
			if !left.Equal(right) {
				return left.Before(right)
			}
			return results[i].ID < results[j].ID
		})
		pageIDs := make([]string, len(results))
		for index := range results {
			pageIDs[index] = results[index].ID
		}
		// Numbered pages can shift when a page is updated during a pass.
		// Only trust the saved offset when the fetched page starts with the
		// rows already returned; otherwise replay the page (duplicates are
		// idempotent upserts, a skipped row would be lost).
		if cursor.Offset > 0 && !samePagePrefix(pageIDs, cursor.Seen) {
			cursor.Offset, cursor.Seen = 0, nil
		}
		if cursor.Offset > len(results) {
			cursor.Offset, cursor.Seen = 0, nil
		}
		pageNewest := newestUpdated(results)
		for cursor.Offset < len(results) && len(items) < limit {
			result := results[cursor.Offset]
			cursor.Offset++
			if cursor.Offset > len(cursor.Seen) {
				cursor.Seen = append(cursor.Seen, result.ID)
			}
			updated := result.UpdatedAt()
			if bound != nil && (updated.IsZero() || updated.Before(*bound)) {
				continue
			}
			item, err := s.buildItem(ctx, token, req.Connection.ID, result, page.Links.Base)
			if err != nil {
				return ports.ReadBatch{}, fmt.Errorf("confluence read: %s", connectors.SanitizeError(err, token))
			}
			items = append(items, item)
		}
		if cursor.Offset < len(results) {
			// The item limit stopped us inside the page.
			more = true
			break
		}
		// The whole page was returned, so its position may advance.
		cursor.PassMax = laterTime(cursor.PassMax, pageNewest)
		if page.hasMore(cursor.Start) {
			cursor.Start += len(results)
			cursor.Offset, cursor.Seen = 0, nil
			if pagesRead >= sourceMaxPagesPerRead {
				more = true
				break
			}
			continue
		}
		// Complete pass: fold the pass maximum into the durable watermark.
		cursor.Watermark = laterTime(cursor.Watermark, cursor.PassMax)
		cursor.PassMax = time.Time{}
		cursor.Start, cursor.Offset, cursor.Seen, cursor.Done = 0, 0, nil, true
		more = false
		break
	}
	if len(items) >= limit && !cursor.Done {
		more = true
	}
	checkpoint, err := encodeSourceCursor(cursor)
	if err != nil {
		return ports.ReadBatch{}, err
	}
	return ports.ReadBatch{
		Items:       items,
		Checkpoints: []domain.Checkpoint{checkpoint},
		More:        more,
	}, nil
}

// sourceCursor is the JSON payload of the single confluence checkpoint.
type sourceCursor struct {
	// Watermark is the newest provider update time of the last complete pass.
	Watermark time.Time `json:"watermark,omitempty"`
	// PassMax is the newest update time seen in the current pass. It folds
	// into Watermark only when the pass completes.
	PassMax time.Time `json:"passMax,omitempty"`
	// Start is the provider start offset of the current page.
	Start int `json:"start"`
	// Offset is the next row inside the current page.
	Offset int `json:"offset,omitempty"`
	// Seen is the ordered list of page IDs already returned from the current
	// page. It detects a shifted page and resets the offset instead of
	// silently skipping a row.
	Seen []string `json:"seen,omitempty"`
	// Done marks a completed pass; the next Read starts a new pass.
	Done bool `json:"done,omitempty"`
}

func readSourceCursor(checkpoints map[string]domain.Checkpoint) (sourceCursor, error) {
	checkpoint, ok := checkpoints[checkpointStream]
	if !ok {
		return sourceCursor{}, nil
	}
	if checkpoint.Stream != checkpointStream || checkpoint.Version != checkpointVersion {
		return sourceCursor{}, domain.NewValidationError("confluence.checkpoint",
			fmt.Sprintf("stream %q version %d is not supported", checkpoint.Stream, checkpoint.Version))
	}
	var cursor sourceCursor
	if err := json.Unmarshal(checkpoint.Data, &cursor); err != nil {
		return sourceCursor{}, domain.NewValidationError("confluence.checkpoint", "is not valid JSON")
	}
	if cursor.Start < 0 || cursor.Start > sourceMaxPagesPerStream*providerPageSize {
		return sourceCursor{}, domain.NewValidationError("confluence.checkpoint", "start is invalid")
	}
	if cursor.Offset < 0 || cursor.Offset > providerPageSize || len(cursor.Seen) > providerPageSize {
		return sourceCursor{}, domain.NewValidationError("confluence.checkpoint", "offset is invalid")
	}
	if cursor.Offset != len(cursor.Seen) {
		return sourceCursor{}, domain.NewValidationError("confluence.checkpoint", "offset does not match seen page rows")
	}
	if cursor.Done && (cursor.Start != 0 || cursor.Offset != 0 || len(cursor.Seen) != 0 || !cursor.PassMax.IsZero()) {
		return sourceCursor{}, domain.NewValidationError("confluence.checkpoint", "completed pass carries page state")
	}
	// A pass maximum older than the durable watermark can only be a corrupted
	// or non-monotonic cursor: reject it instead of moving time backwards.
	if !cursor.PassMax.IsZero() && !cursor.Watermark.IsZero() && cursor.PassMax.Before(cursor.Watermark) {
		return sourceCursor{}, domain.NewValidationError("confluence.checkpoint", "pass maximum precedes watermark")
	}
	return cursor, nil
}

func encodeSourceCursor(cursor sourceCursor) (domain.Checkpoint, error) {
	data, err := json.Marshal(cursor)
	if err != nil {
		return domain.Checkpoint{}, fmt.Errorf("encode confluence checkpoint: %w", err)
	}
	return domain.Checkpoint{Stream: checkpointStream, Version: checkpointVersion, Data: data}, nil
}

// scanBound is the inclusive lower bound of the current pass: the durable
// watermark minus the overlap window, narrowed by ReadRequest.Since.
func scanBound(watermark time.Time, since *time.Time) *time.Time {
	var bound *time.Time
	if !watermark.IsZero() {
		at := watermark.Add(-pageOverlap).UTC()
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

func laterTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right.UTC()
	}
	return left
}

func newestUpdated(results []searchResult) time.Time {
	var newest time.Time
	for _, result := range results {
		newest = laterTime(newest, result.UpdatedAt())
	}
	return newest
}

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

func sourceLimit(limit int) int {
	if limit <= 0 {
		return sourceDefaultLimit
	}
	if limit > sourceMaxLimit {
		return sourceMaxLimit
	}
	return limit
}

func (s *Source) buildCQL(bound *time.Time) string {
	parts := make([]string, 0, 4)
	parts = append(parts, "type = page")
	if bound != nil {
		parts = append(parts, fmt.Sprintf(`lastmodified >= "%s"`, bound.UTC().Format(cqlTimeLayout)))
	}
	if len(s.spaces) > 0 {
		quoted := make([]string, 0, len(s.spaces))
		for _, space := range s.spaces {
			quoted = append(quoted, `"`+escapeCQLString(space)+`"`)
		}
		parts = append(parts, "space in ("+strings.Join(quoted, ",")+")")
	}
	if s.customCQL != "" {
		parts = append(parts, "("+s.customCQL+")")
	}
	return strings.Join(parts, " AND ")
}

// searchPage fetches one numeric page. A Data Center instance that rejects
// ORDER BY is retried once without it, and the rejection is remembered.
func (s *Source) searchPage(ctx context.Context, token, baseCQL string, start int) (searchResponse, error) {
	ordered := !s.orderByDisabled.Load()
	for {
		cql := baseCQL
		if ordered {
			cql += " ORDER BY lastmodified ASC"
		}
		query := url.Values{}
		query.Set("cql", cql)
		query.Set("start", strconv.Itoa(start))
		query.Set("limit", strconv.Itoa(providerPageSize))
		query.Set("expand", expandFields)
		var page searchResponse
		_, err := s.getJSON(ctx, token, s.apiBase+"/content/search?"+query.Encode(), &page)
		if err == nil {
			return page, nil
		}
		var status *connectors.HTTPStatusError
		if ordered && errors.As(err, &status) && status.StatusCode == http.StatusBadRequest {
			s.orderByDisabled.Store(true)
			ordered = false
			continue
		}
		return searchResponse{}, err
	}
}

func (s *Source) buildItem(ctx context.Context, token, connectionID string, result searchResult, pageBase string) (domain.Item, error) {
	comments, err := s.readComments(ctx, token, result.ID)
	if err != nil {
		return domain.Item{}, err
	}
	title := strings.TrimSpace(result.Title)
	if title == "" {
		title = "Confluence page " + result.ID
	}
	body := sourceBody(storageToMarkdown(result.Body.Storage.Value), comments)
	item := domain.Item{
		Ref:       domain.ItemRef{ConnectionID: connectionID, ExternalID: externalID(result.ID)},
		Kind:      domain.ItemPage,
		Title:     title,
		Body:      body,
		URL:       s.itemURL(result, pageBase),
		Author:    result.Version.By.name(),
		Labels:    result.labelNames(),
		CreatedAt: result.Version.When,
		UpdatedAt: result.Version.When,
	}
	if len(s.connection.Projects) == 1 {
		item.ProjectID = s.connection.Projects[0]
	}
	return item, nil
}

// externalID is stable across renames and moves: it covers the page ID only.
func externalID(id string) string { return "page/" + id }

func (s *Source) itemURL(result searchResult, pageBase string) string {
	webui := strings.TrimSpace(result.Links.WebUI)
	if webui == "" {
		return ""
	}
	if parsed, err := url.Parse(webui); err == nil && parsed.IsAbs() {
		return webui
	}
	base := strings.TrimSpace(result.Links.Base)
	if base == "" {
		base = strings.TrimSpace(pageBase)
	}
	if base == "" {
		base = s.siteBase
	}
	if base == "" {
		return webui
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(webui, "/")
}

// readComments fetches at most maxCommentsPerItem newest comments. One page of
// up to providerPageSize comments is requested; when the API reports more and
// returns comments oldest-first, the tail page is fetched instead of keeping
// the oldest comments.
func (s *Source) readComments(ctx context.Context, token, pageID string) ([]comment, error) {
	first, err := s.commentsPage(ctx, token, pageID, 0)
	if err != nil {
		return nil, err
	}
	results := first.Results
	if first.TotalSize > providerPageSize && len(results) > 0 && commentsAscending(results) {
		start := first.TotalSize - providerPageSize
		if start < 0 {
			start = 0
		}
		tail, err := s.commentsPage(ctx, token, pageID, start)
		if err != nil {
			return nil, err
		}
		results = tail.Results
	}
	sort.SliceStable(results, func(i, j int) bool {
		left, right := results[i].UpdatedAt(), results[j].UpdatedAt()
		if !left.Equal(right) {
			return left.After(right)
		}
		return results[i].ID > results[j].ID
	})
	if len(results) > maxCommentsPerItem {
		results = results[:maxCommentsPerItem]
	}
	// Render chronologically, with the ID as a stable tie-breaker.
	sort.SliceStable(results, func(i, j int) bool {
		left, right := results[i].UpdatedAt(), results[j].UpdatedAt()
		if !left.Equal(right) {
			return left.Before(right)
		}
		return results[i].ID < results[j].ID
	})
	return results, nil
}

func (s *Source) commentsPage(ctx context.Context, token, pageID string, start int) (searchResponse, error) {
	query := url.Values{}
	query.Set("start", strconv.Itoa(start))
	query.Set("limit", strconv.Itoa(providerPageSize))
	query.Set("expand", "body.storage,version")
	requestURL := s.apiBase + "/content/" + url.PathEscape(pageID) + "/child/comment?" + query.Encode()
	var page searchResponse
	if _, err := s.getJSON(ctx, token, requestURL, &page); err != nil {
		return searchResponse{}, err
	}
	return page, nil
}

func commentsAscending(results []searchResult) bool {
	if len(results) < 2 {
		return true
	}
	return !results[len(results)-1].UpdatedAt().Before(results[0].UpdatedAt())
}

func (s *Source) token(ctx context.Context) (string, error) {
	token, err := s.credentials.Get(ctx, s.connection.ID, "token")
	if err != nil {
		return "", fmt.Errorf("read confluence credential: %s", connectors.SanitizeError(err))
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("read confluence credential: token is empty")
	}
	return token, nil
}

func (s *Source) getJSON(ctx context.Context, token, requestURL string, out any) (http.Header, error) {
	return s.http.GetJSONWithHeaders(ctx, requestURL, s.authHeader(token), out)
}

func (s *Source) authHeader(token string) http.Header {
	header := http.Header{}
	if s.deployment == deploymentCloud {
		credentials := base64.StdEncoding.EncodeToString([]byte(s.email + ":" + token))
		header.Set("Authorization", "Basic "+credentials)
	} else {
		header.Set("Authorization", "Bearer "+token)
	}
	header.Set("Accept", "application/json")
	header.Set("User-Agent", DefaultUserAgent)
	return header
}

func parseDeployment(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", deploymentCloud:
		return deploymentCloud, nil
	case deploymentDataCenter:
		return deploymentDataCenter, nil
	default:
		return "", domain.NewValidationError("confluence.deployment", `must be "cloud" or "datacenter"`)
	}
}

func parseSpaces(raw string) ([]string, error) {
	var spaces []string
	seen := make(map[string]struct{})
	for _, entry := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	}) {
		space := strings.TrimSpace(entry)
		if space == "" {
			continue
		}
		if err := validateSpaceKey(space); err != nil {
			return nil, err
		}
		key := strings.ToUpper(space)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		spaces = append(spaces, key)
	}
	sort.Strings(spaces)
	return spaces, nil
}

func validateSpaceKey(space string) error {
	if len(space) > 255 {
		return domain.NewValidationError("confluence.spaces", "space keys must be at most 255 bytes")
	}
	for _, r := range space {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' {
			continue
		}
		return domain.NewValidationError("confluence.spaces", fmt.Sprintf("%q contains an invalid character", space))
	}
	return nil
}

func normalizeCustomCQL(raw string) (string, error) {
	cql := strings.TrimSpace(raw)
	if cql == "" {
		return "", nil
	}
	if len(cql) > 2000 {
		return "", domain.NewValidationError("confluence.cql", "must be at most 2000 bytes")
	}
	if strings.IndexFunc(cql, unicode.IsControl) >= 0 {
		return "", domain.NewValidationError("confluence.cql", "must not contain control characters")
	}
	// The generated query owns the single ORDER BY clause.
	if strings.Contains(strings.ToLower(cql), "order by") {
		return "", domain.NewValidationError("confluence.cql", "must not contain ORDER BY; the source adds it")
	}
	return cql, nil
}

func escapeCQLString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, `"`, `\"`)
}

// isAllowedBaseURL reports whether u may carry the token: https always, http
// only for loopback hosts so local test servers keep working.
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

// normalizeBaseURL appends the API prefix exactly once and returns the API base
// plus the site base used to build page URLs when _links.base is absent.
func normalizeBaseURL(raw, deployment string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", domain.NewValidationError("confluence.baseUrl", "is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !isAllowedBaseURL(parsed) {
		return "", "", domain.NewValidationError("confluence.baseUrl",
			"must be an https URL without credentials, query or fragment; http is allowed only for loopback hosts")
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch deployment {
	case deploymentCloud:
		// Remove an API suffix if present, ensure the wiki context, then add
		// the API suffix exactly once: /wiki/rest/api.
		path = strings.TrimSuffix(path, "/rest/api")
		if !strings.HasSuffix(path, "/wiki") {
			path += "/wiki"
		}
		path += "/rest/api"
	case deploymentDataCenter:
		// Remove an API suffix if present, then add /rest/api exactly once.
		path = strings.TrimSuffix(path, "/rest/api") + "/rest/api"
	default:
		return "", "", domain.NewValidationError("confluence.deployment", "is not supported")
	}
	apiPath := path
	sitePath := strings.TrimSuffix(apiPath, "/rest/api")
	parsed.Path, parsed.RawPath = apiPath, ""
	apiBase := strings.TrimRight(parsed.String(), "/")
	parsed.Path, parsed.RawPath = sitePath, ""
	siteBase := strings.TrimRight(parsed.String(), "/")
	return apiBase, siteBase, nil
}

// userIdentity is the subset of /user/current the source needs.
type userIdentity struct {
	Username  string `json:"username"`
	AccountID string `json:"accountId"`
}

// searchResponse is the Confluence content search / child comment envelope.
type searchResponse struct {
	Results   []searchResult `json:"results"`
	Start     int            `json:"start"`
	Limit     int            `json:"limit"`
	Size      int            `json:"size"`
	TotalSize int            `json:"totalSize"`
	Links     struct {
		Base string `json:"base"`
		Next string `json:"next"`
	} `json:"_links"`
}

// hasMore reports whether a page leaves more provider rows to fetch.
//
// totalSize is authoritative when present, but a Cloud page that returned
// exactly 50 rows for a larger request is always treated as truncated: that is
// the documented silent cap. It costs at most one extra empty request and
// guarantees the first 50 rows are never mistaken for a complete pass.
func (p searchResponse) hasMore(start int) bool {
	if len(p.Results) == 0 {
		return false
	}
	if p.Links.Next != "" {
		return true
	}
	if providerPageSize > 50 && len(p.Results) == 50 {
		return true
	}
	if p.TotalSize > 0 {
		return start+len(p.Results) < p.TotalSize
	}
	return len(p.Results) >= providerPageSize
}

// searchResult is one Confluence content object. Comments use the same shape.
type searchResult struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Title  string `json:"title"`
	Body   struct {
		Storage struct {
			Value          string `json:"value"`
			Representation string `json:"representation"`
		} `json:"storage"`
	} `json:"body"`
	Version struct {
		Number int       `json:"number"`
		When   time.Time `json:"when"`
		By     versionBy `json:"by"`
	} `json:"version"`
	Space struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"space"`
	Metadata struct {
		Labels struct {
			Results []struct {
				Name string `json:"name"`
			} `json:"results"`
		} `json:"labels"`
	} `json:"metadata"`
	Links struct {
		Base  string `json:"base"`
		WebUI string `json:"webui"`
	} `json:"_links"`
}

// versionBy is the Confluence version author object.
type versionBy struct {
	Username    string `json:"username"`
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	PublicName  string `json:"publicName"`
}

func (b versionBy) name() string {
	for _, value := range []string{b.DisplayName, b.PublicName, b.Username, b.AccountID} {
		if name := strings.TrimSpace(value); name != "" {
			return name
		}
	}
	return "unknown"
}

// UpdatedAt is the provider update time used for filtering and watermarks.
func (r searchResult) UpdatedAt() time.Time { return r.Version.When.UTC() }

type comment = searchResult

func (r searchResult) labelNames() []string {
	out := make([]string, 0, len(r.Metadata.Labels.Results))
	seen := make(map[string]struct{}, len(r.Metadata.Labels.Results))
	for _, label := range r.Metadata.Labels.Results {
		name := strings.TrimSpace(label.Name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// sourceBody renders the page body and its newest bounded comments. The final
// body never exceeds the host cap (512 KiB), each comment is capped at 16 KiB
// and all comments together at 256 KiB.
func sourceBody(body string, comments []comment) string {
	var out strings.Builder
	appendBounded(&out, strings.TrimSpace(body), maxSourceBodyBytes)
	if len(comments) == 0 {
		return out.String()
	}
	var block strings.Builder
	block.WriteString("\n\n### Comments\n")
	for _, comment := range comments {
		text := strings.TrimSpace(storageToMarkdown(comment.Body.Storage.Value))
		if text == "" {
			continue
		}
		entry := "\n- **" + authorName(comment) + "** (" +
			connectors.DisplayTime(comment.UpdatedAt(), "unknown") + "): " +
			capUTF8(text, maxCommentBytes) + "\n"
		remaining := maxCommentsTotalBytes - block.Len()
		if remaining <= 0 {
			break
		}
		block.WriteString(capUTF8(entry, remaining))
		if block.Len() >= maxCommentsTotalBytes {
			break
		}
	}
	appendBounded(&out, block.String(), maxSourceBodyBytes)
	return out.String()
}

func authorName(value searchResult) string {
	return value.Version.By.name()
}

func appendBounded(out *strings.Builder, value string, limit int) {
	remaining := limit - out.Len()
	if remaining <= 0 {
		return
	}
	out.WriteString(capUTF8(value, remaining))
}

// capUTF8 truncates value to at most limit bytes without splitting a rune.
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
