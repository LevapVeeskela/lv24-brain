// Package notion implements a native read-only Notion source.
//
// Behavior contract:
//   - Config settings: "token" is a required secret held only by the
//     credential provider (never in Settings); "databases" is an optional
//     comma or newline separated list of database / data-source IDs;
//     "pages" is an optional comma or newline separated list of root page
//     IDs (share them with the integration); "version" is the optional
//     Notion-Version header, default 2025-09-03.
//   - Pages may contain personal content, so the descriptor marks
//     PersonalData true and DefaultPublish local-only.
//   - One checkpoint stream per configured database ("datasource-<id>"),
//     per configured root page ("page-<id>") or a single "notion-search"
//     stream when neither is configured. Descriptor.Streams stays empty
//     because streams are dynamic. Checkpoint JSON is
//     {"watermark","cursor","offset","seen","done","passMax"} (< 1 KiB).
//     Watermark is the newest provider time of the last completed pass and
//     advances only after a complete pass; the next pass replays from
//     watermark minus 5 minutes (overlapBound) narrowed by ReadRequest.Since.
//     More is true while any stream still has a provider page or offset
//     pending. Malformed checkpoints are rejected, never silently reset.
//   - Databases use POST /v1/data_sources/{id}/query with page_size 100,
//     cursor paging and a last_edited_time on_or_after filter when a bound
//     exists. Root pages fetch GET /v1/pages/{id} once and then walk
//     GET /v1/blocks/{id}/children depth-first (depth <= 3, at most 100
//     blocks per root, 500 KiB cumulative raw response cap). Search (no
//     databases/pages configured) uses POST /v1/search sorted
//     last_edited_time descending; the search index has no updated_since, so
//     paging stops as soon as results are older than the bound minus 5
//     minutes (documented heuristic, see Read).
//   - Block conversion keeps paragraph, headings, lists, code, quote,
//     divider, callout text, toggle text and link-preview text in provider
//     order (depth-first); embeds, files, images, video, pdf and other
//     attachments are skipped. Properties render as sorted frontmatter-like
//     text, then the block Markdown. Bodies are capped at
//     domain.MaxItemBodyBytes on a UTF-8 boundary.
//   - Items are domain.ItemPage with external ID "page/<id>" or
//     "data_source/<db>/<page>". Title prefers the title property, then the
//     first heading, then a stable fallback. URL is the page URL when the
//     provider returns one; author is last_edited_by (name, else id).
//     ProjectID is set only when the connection feeds a single project.
//   - No deletes are inferred or emitted (Deletes capability is false).
//   - Secrets never appear in URLs, logs or errors: the token travels only
//     in the Authorization Bearer header and every returned error is passed
//     through connectors.SanitizeError with the token redacted.
package notion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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
	// Name is the provider name.
	Name = "notion"
	// DefaultBaseURL is the Notion API endpoint.
	DefaultBaseURL = "https://api.notion.com"
	// DefaultVersion is the Notion-Version header when unset.
	DefaultVersion = "2025-09-03"
	// DefaultUserAgent identifies the engine to Notion.
	DefaultUserAgent = "lv24-brain"

	checkpointVersion = 1

	sourcePageSize = 100

	defaultLimit = 50
	maxLimit     = 500

	// maxPagesPerRead bounds the data-source query / search requests of one
	// Read call. Block-children fetches for the emitted items are extra but
	// bounded by Limit (each item walks at most maxBlocksPerPage blocks).
	maxPagesPerRead = 5

	// maxBlocksPerPage caps the block tree of one root page or one item.
	maxBlocksPerPage = 100
	// maxBlocksBytes caps the cumulative raw block-children response per
	// root page or item (500 KiB). Traversal stops early when exceeded.
	maxBlocksBytes = 500 << 10

	// maxDepth caps block-children recursion (root = depth 0).
	maxDepth = 3

	// scanOverlap replays the tail of the previous pass so an edit that
	// raced a checkpoint is still picked up; unchanged replays are no-ops
	// for the host (same Ref + same content = same Hash).
	scanOverlap = 5 * time.Minute
)

// Descriptor returns the static Notion source description.
func Descriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider:    Name,
		DisplayName: "Notion",
		Capabilities: domain.Capabilities{
			Backfill:    true,
			Incremental: true,
			AuthModes:   []domain.AuthMode{domain.AuthToken},
		},
		DefaultPublish: domain.PublishLocalOnly,
		ConfigFields: []domain.ConfigField{
			{Key: "token", Label: "Internal integration token", Required: true, Secret: true, Help: "Notion internal integration token; share pages/databases with the integration."},
			{Key: "databases", Label: "Databases", Help: "Comma or newline separated database / data-source IDs to sync. Empty means search mode."},
			{Key: "pages", Label: "Root pages", Help: "Comma or newline separated root page IDs to sync (share them with the integration). Empty means search mode unless databases are set."},
			{Key: "version", Label: "Notion version", Help: "Notion-Version header (default 2025-09-03)."},
		},
		Backends:     []domain.Backend{domain.BackendNative},
		PersonalData: true,
	}
}

// Source is the native read-only Notion integration.
type Source struct {
	connection  domain.Connection
	baseURL     string
	version     string
	databases   []string
	pages       []string
	credentials ports.CredentialProvider
	client      *http.Client
	now         func() time.Time
}

type streamDef struct {
	kind string // "datasource", "page", "search"
	id   string // database or page ID, empty for search
	name string // checkpoint stream name
}

// streamCursor is the per-stream checkpoint payload.
type streamCursor struct {
	Watermark *time.Time `json:"watermark,omitempty"`
	Cursor    *string    `json:"cursor,omitempty"`
	Offset    int        `json:"offset,omitempty"`
	Seen      int        `json:"seen,omitempty"`
	Done      bool       `json:"done,omitempty"`
	PassMax   *time.Time `json:"passMax,omitempty"`
}

// NewSource validates the connection and builds the native source.
func NewSource(conn domain.Connection, credentials ports.CredentialProvider, client *http.Client, now func() time.Time) (*Source, error) {
	if err := Descriptor().ValidateConnection(conn); err != nil {
		return nil, err
	}
	if credentials == nil {
		return nil, domain.NewValidationError("notion.credentials", "is required")
	}
	databases, err := parseIDList(conn.Settings["databases"], "notion.databases")
	if err != nil {
		return nil, err
	}
	pages, err := parseIDList(conn.Settings["pages"], "notion.pages")
	if err != nil {
		return nil, err
	}
	version := strings.TrimSpace(conn.Settings["version"])
	if version == "" {
		version = DefaultVersion
	}
	if len(version) > 64 || strings.ContainsAny(version, " \t\n\r") {
		return nil, domain.NewValidationError("notion.version", "must be a single token such as 2025-09-03")
	}
	baseURL := strings.TrimSpace(conn.BaseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || !isAllowedBaseURL(parsed) || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return nil, domain.NewValidationError("notion.baseUrl", "must be an https URL without credentials, query or fragment; http is allowed only for loopback hosts")
	}
	if parsed.RawPath != "" {
		_ = parsed.RawPath
	}
	normalized := strings.TrimRight(parsed.String(), "/")
	if now == nil {
		now = time.Now
	}
	httpClient := client
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if httpClient.CheckRedirect == nil {
		cloned := *httpClient
		cloned.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
		httpClient = &cloned
	}
	return &Source{
		connection:  conn,
		baseURL:     normalized,
		version:     version,
		databases:   databases,
		pages:       pages,
		credentials: credentials,
		client:      httpClient,
		now:         now,
	}, nil
}

// Descriptor returns the provider's static description.
func (s *Source) Descriptor() domain.SourceDescriptor { return Descriptor() }

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

func parseIDList(raw, field string) ([]string, error) {
	var out []string
	seen := make(map[string]struct{})
	for _, entry := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		id := strings.TrimSpace(entry)
		if id == "" {
			continue
		}
		if len(id) > 100 || strings.ContainsAny(id, " \t\n\r\x00") {
			return nil, domain.NewValidationError(field, fmt.Sprintf("entry %q is invalid", id))
		}
		for _, r := range id {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
				continue
			}
			return nil, domain.NewValidationError(field, fmt.Sprintf("entry %q must use letters, digits, '-' or '_'", id))
		}
		key := strings.ToLower(id)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func (s *Source) streams() []streamDef {
	if len(s.databases) == 0 && len(s.pages) == 0 {
		return []streamDef{{kind: "search", name: "notion-search"}}
	}
	var defs []streamDef
	for _, id := range s.databases {
		defs = append(defs, streamDef{kind: "datasource", id: id, name: "datasource-" + sanitizeStreamID(id)})
	}
	for _, id := range s.pages {
		defs = append(defs, streamDef{kind: "page", id: id, name: "page-" + sanitizeStreamID(id)})
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].name < defs[j].name })
	return defs
}

func sanitizeStreamID(id string) string {
	lowered := strings.ToLower(strings.TrimSpace(id))
	var b strings.Builder
	for _, r := range lowered {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		sum := sha256.Sum256([]byte(id))
		return hex.EncodeToString(sum[:])[:16]
	}
	if c := out[0]; !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
		out = "id-" + out
	}
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

// Check verifies the token with the cheapest read-only endpoint.
func (s *Source) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	token, err := s.token(ctx)
	if err != nil {
		return fmt.Errorf("notion check: %s", connectors.SanitizeError(err))
	}
	var me struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if _, err := s.doGET(ctx, token, s.baseURL+"/v1/users/me", &me); err != nil {
		return fmt.Errorf("notion check: %s", connectors.SanitizeError(err, token))
	}
	if strings.TrimSpace(me.ID) == "" || strings.TrimSpace(me.Type) == "" {
		return fmt.Errorf("notion check: users/me response is incomplete")
	}
	return nil
}

// Read returns the next bounded batch across all streams.
//
// Cursor semantics: each stream carries {watermark, cursor, offset, seen,
// done} plus an internal passMax. Watermark is the newest provider time of
// the last completed pass and advances only when the stream's provider pages
// are exhausted; the next pass starts from watermark minus 5 minutes,
// narrowed by ReadRequest.Since. More is true while any stream still has a
// provider page or an offset pending. A fully Done checkpoint set starts the
// next incremental pass on the following Read.
//
// Search heuristic: POST /v1/search sorts last_edited_time descending and
// offers no updated_since filter, so paging stops as soon as a result is
// older than the effective bound minus 5 minutes. The 5-minute margin covers
// index lag; an edit older than the margin can surface one pass late but is
// never lost while its time still exceeds a later watermark. Database
// queries use a real last_edited_time on_or_after filter and have no such
// heuristic.
func (s *Source) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, err
	}
	if req.Connection.ID != s.connection.ID || req.Connection.Provider != Name {
		return ports.ReadBatch{}, domain.NewValidationError("notion.connection", "does not match source connection")
	}
	token, err := s.token(ctx)
	if err != nil {
		return ports.ReadBatch{}, fmt.Errorf("notion credentials: %s", connectors.SanitizeError(err))
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	streams := s.streams()
	states := make([]streamCursor, len(streams))
	for i, def := range streams {
		state, err := readCursor(req.Checkpoints, def.name)
		if err != nil {
			return ports.ReadBatch{}, err
		}
		states[i] = state
	}
	allDone := len(streams) > 0
	for _, state := range states {
		allDone = allDone && state.Done
	}
	if allDone {
		for i := range states {
			watermark := states[i].Watermark
			states[i] = streamCursor{Watermark: watermark}
		}
	}
	items := make([]domain.Item, 0, limit)
	pagesRead := 0
outer:
	for i, def := range streams {
		if len(items) >= limit || pagesRead >= maxPagesPerRead {
			break
		}
		if states[i].Done {
			continue
		}
		if err := ctx.Err(); err != nil {
			return ports.ReadBatch{}, err
		}
		switch def.kind {
		case "page":
			if pagesRead >= maxPagesPerRead {
				break outer
			}
			if len(items) >= limit {
				break outer
			}
			pagesRead++
			_, emitted, err := s.readRootPage(ctx, token, def.id, overlapBound(states[i].Watermark, req.Since))
			if err != nil {
				return ports.ReadBatch{}, fmt.Errorf("notion %s: %s", def.name, connectors.SanitizeError(err, token))
			}
			states[i].Seen++
			if emitted != nil {
				items = append(items, *emitted)
				states[i].PassMax = maxTimePtr(states[i].PassMax, emitted.UpdatedAt)
			}
			states[i].Watermark = laterTimePtr(states[i].Watermark, states[i].PassMax)
			states[i].PassMax = nil
			states[i].Done = true
		default:
			for len(items) < limit && pagesRead < maxPagesPerRead && !states[i].Done {
				if err := ctx.Err(); err != nil {
					return ports.ReadBatch{}, err
				}
				consumed, err := s.readStreamPage(ctx, token, def, &states[i], overlapBound(states[i].Watermark, req.Since), &items, limit)
				if err != nil {
					return ports.ReadBatch{}, fmt.Errorf("notion %s: %s", def.name, connectors.SanitizeError(err, token))
				}
				if consumed {
					pagesRead++
				} else {
					break
				}
			}
		}
	}
	checkpoints := make([]domain.Checkpoint, 0, len(streams))
	more := false
	for i, def := range streams {
		if !states[i].Done {
			more = true
		}
		data, err := json.Marshal(states[i])
		if err != nil {
			return ports.ReadBatch{}, fmt.Errorf("encode notion checkpoint: %w", err)
		}
		checkpoints = append(checkpoints, domain.Checkpoint{Stream: def.name, Version: checkpointVersion, Data: data})
	}
	sort.Slice(checkpoints, func(i, j int) bool { return checkpoints[i].Stream < checkpoints[j].Stream })
	return ports.ReadBatch{Items: items, Checkpoints: checkpoints, More: more}, nil
}

func readCursor(checkpoints map[string]domain.Checkpoint, stream string) (streamCursor, error) {
	stored, ok := checkpoints[stream]
	if !ok {
		return streamCursor{}, nil
	}
	if stored.Stream != stream || stored.Version != checkpointVersion {
		return streamCursor{}, domain.NewValidationError("notion.checkpoint", "unsupported stream or version")
	}
	var state streamCursor
	if err := json.Unmarshal(stored.Data, &state); err != nil {
		return streamCursor{}, domain.NewValidationError("notion.checkpoint", "is invalid")
	}
	if state.Offset < 0 || state.Offset > sourcePageSize || state.Seen < 0 || state.Seen > 100000 {
		return streamCursor{}, domain.NewValidationError("notion.checkpoint", "position is invalid")
	}
	if state.Cursor != nil && len(*state.Cursor) > 4096 {
		return streamCursor{}, domain.NewValidationError("notion.checkpoint", "cursor is invalid")
	}
	if state.Watermark != nil {
		utc := state.Watermark.UTC()
		state.Watermark = &utc
	}
	if state.PassMax != nil {
		utc := state.PassMax.UTC()
		state.PassMax = &utc
	}
	return state, nil
}

func overlapBound(watermark *time.Time, since *time.Time) *time.Time {
	var bound *time.Time
	if watermark != nil {
		at := watermark.UTC().Add(-scanOverlap)
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

func maxTimePtr(left *time.Time, right time.Time) *time.Time {
	if right.IsZero() {
		return left
	}
	right = right.UTC()
	if left == nil || right.After(*left) {
		return &right
	}
	return left
}

func laterTimePtr(left, right *time.Time) *time.Time {
	if left == nil {
		return right
	}
	if right != nil && right.After(*left) {
		return right
	}
	return left
}

// readStreamPage fetches one provider page for a database or search stream,
// appends at most the remaining limit of items and advances state. It reports
// whether a provider request was consumed.
func (s *Source) readStreamPage(ctx context.Context, token string, def streamDef, state *streamCursor, bound *time.Time, items *[]domain.Item, limit int) (bool, error) {
	var results []notionPage
	var nextCursor *string
	var hasMore bool
	var err error
	if def.kind == "datasource" {
		results, nextCursor, hasMore, err = s.queryDataSource(ctx, token, def.id, state.Cursor, bound)
	} else {
		results, nextCursor, hasMore, err = s.searchPages(ctx, token, state.Cursor)
	}
	if err != nil {
		return false, err
	}
	if state.Offset > len(results) {
		state.Offset = len(results)
	}
	index := state.Offset
	for index < len(results) && len(*items) < limit {
		page := results[index]
		updated := page.LastEditedTime.UTC()
		if def.kind == "search" && bound != nil && !updated.IsZero() && updated.Before(*bound) {
			// Search heuristic: descending order, so this and every later
			// result is older than the bound (minus overlap already
			// applied by overlapBound). Stop paging this pass.
			state.Watermark = laterTimePtr(state.Watermark, state.PassMax)
			state.PassMax = nil
			state.Cursor = nil
			state.Offset = 0
			state.Done = true
			return true, nil
		}
		if bound != nil && !updated.IsZero() && updated.Before(*bound) {
			index++
			state.Offset = index
			continue
		}
		item, err := s.pageItem(ctx, def, page)
		if err != nil {
			return false, err
		}
		*items = append(*items, item)
		state.PassMax = maxTimePtr(state.PassMax, item.UpdatedAt)
		state.Seen++
		index++
		state.Offset = index
	}
	if state.Offset < len(results) {
		return true, nil
	}
	if hasMore && nextCursor != nil {
		// For search, the bound check already stopped older pages; a page
		// that ends exactly at the bound still continues so the next page
		// can prove it is older.
		state.Cursor = nextCursor
		state.Offset = 0
		return true, nil
	}
	state.Watermark = laterTimePtr(state.Watermark, state.PassMax)
	state.PassMax = nil
	state.Cursor = nil
	state.Offset = 0
	state.Done = true
	return true, nil
}

func (s *Source) readRootPage(ctx context.Context, token, pageID string, bound *time.Time) (item *domain.Item, emitted *domain.Item, err error) {
	page, err := s.getPage(ctx, token, pageID)
	if err != nil {
		return nil, nil, err
	}
	if bound != nil && !page.LastEditedTime.IsZero() && page.LastEditedTime.UTC().Before(*bound) {
		return nil, nil, nil
	}
	built, err := s.pageItem(ctx, streamDef{kind: "page", id: pageID}, page)
	if err != nil {
		return nil, nil, err
	}
	return &built, &built, nil
}

func (s *Source) pageItem(ctx context.Context, def streamDef, page notionPage) (domain.Item, error) {
	markdown, firstHeading, err := s.pageBlocksMarkdown(ctx, page.ID)
	if err != nil {
		return domain.Item{}, err
	}
	title := titleFromProperties(page.Properties)
	if title == "" {
		title = firstHeading
	}
	if strings.TrimSpace(title) == "" {
		title = "Notion page " + shortID(page.ID)
	}
	front := frontmatter(page.Properties)
	var body strings.Builder
	if front != "" {
		body.WriteString(front)
		body.WriteString("\n\n")
	}
	body.WriteString(markdown)
	capped := capUTF8(body.String(), domain.MaxItemBodyBytes)
	externalID := "page/" + normalizeID(page.ID)
	if def.kind == "datasource" {
		externalID = "data_source/" + normalizeID(def.id) + "/" + normalizeID(page.ID)
	}
	item := domain.Item{
		Ref:       domain.ItemRef{ConnectionID: s.connection.ID, ExternalID: externalID},
		Kind:      domain.ItemPage,
		Title:     title,
		Body:      capped,
		URL:       strings.TrimSpace(page.URL),
		Author:    authorName(page.LastEditedBy),
		Labels:    labelsFromProperties(page.Properties),
		CreatedAt: page.CreatedTime.UTC(),
		UpdatedAt: page.LastEditedTime.UTC(),
	}
	if len(s.connection.Projects) == 1 {
		item.ProjectID = s.connection.Projects[0]
	}
	return item, nil
}

func normalizeID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

func shortID(id string) string {
	stripped := strings.ReplaceAll(strings.TrimSpace(id), "-", "")
	if len(stripped) > 8 {
		return strings.ToLower(stripped[:8])
	}
	if stripped == "" {
		return "unknown"
	}
	return strings.ToLower(stripped)
}

func authorName(user *notionUser) string {
	if user == nil {
		return ""
	}
	if strings.TrimSpace(user.Name) != "" {
		return strings.TrimSpace(user.Name)
	}
	return strings.TrimSpace(user.ID)
}

func (s *Source) token(ctx context.Context) (string, error) {
	token, err := s.credentials.Get(ctx, s.connection.ID, "token")
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("token is empty")
	}
	return token, nil
}

func (s *Source) headers(token string) http.Header {
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+token)
	header.Set("Notion-Version", s.version)
	header.Set("Accept", "application/json")
	header.Set("User-Agent", DefaultUserAgent)
	return header
}

func (s *Source) doGET(ctx context.Context, token, requestURL string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header = s.headers(token)
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return len(raw), notionStatusError(resp.StatusCode, resp.Status)
	}
	if out == nil || len(raw) == 0 {
		return len(raw), nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return len(raw), err
	}
	return len(raw), nil
}

func (s *Source) doPOST(ctx context.Context, token, requestURL string, body any, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, &buf)
	if err != nil {
		return err
	}
	header := s.headers(token)
	header.Set("Content-Type", "application/json")
	req.Header = header
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return notionStatusError(resp.StatusCode, resp.Status)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func notionStatusError(code int, status string) error {
	text := strings.TrimSpace(status)
	if text == "" {
		text = http.StatusText(code)
	}
	switch code {
	case http.StatusUnauthorized:
		return fmt.Errorf("http 401 Unauthorized: check the integration token")
	case http.StatusForbidden:
		return fmt.Errorf("http 403 Forbidden: share the pages/databases with the integration")
	default:
		if text == "" {
			text = strconv.Itoa(code)
		}
		return fmt.Errorf("http %d %s", code, text)
	}
}

type notionUser struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type notionPage struct {
	Object         string                    `json:"object"`
	ID             string                    `json:"id"`
	CreatedTime    time.Time                 `json:"created_time"`
	LastEditedTime time.Time                 `json:"last_edited_time"`
	LastEditedBy   *notionUser               `json:"last_edited_by,omitempty"`
	Properties     map[string]notionProperty `json:"properties,omitempty"`
	URL            string                    `json:"url,omitempty"`
}

type notionProperty struct {
	ID          string           `json:"id,omitempty"`
	Type        string           `json:"type,omitempty"`
	Title       []notionRichText `json:"title,omitempty"`
	RichText    []notionRichText `json:"rich_text,omitempty"`
	Select      *notionSelect    `json:"select,omitempty"`
	MultiSelect []notionSelect   `json:"multi_select,omitempty"`
	Status      *notionSelect    `json:"status,omitempty"`
}

type notionRichText struct {
	PlainText string `json:"plain_text"`
	Href      string `json:"href,omitempty"`
}

type notionSelect struct {
	Name string `json:"name,omitempty"`
}

type notionQueryResponse struct {
	Object     string       `json:"object"`
	Results    []notionPage `json:"results"`
	NextCursor *string      `json:"next_cursor"`
	HasMore    bool         `json:"has_more"`
}

type notionSearchResponse struct {
	Object     string       `json:"object"`
	Results    []notionPage `json:"results"`
	NextCursor *string      `json:"next_cursor"`
	HasMore    bool         `json:"has_more"`
}

type notionBlock struct {
	Object             string              `json:"object,omitempty"`
	ID                 string              `json:"id"`
	Type               string              `json:"type"`
	HasChildren        bool                `json:"has_children,omitempty"`
	Paragraph          *notionBlockContent `json:"paragraph,omitempty"`
	Heading1           *notionBlockContent `json:"heading_1,omitempty"`
	Heading2           *notionBlockContent `json:"heading_2,omitempty"`
	Heading3           *notionBlockContent `json:"heading_3,omitempty"`
	BulletedListItem   *notionBlockContent `json:"bulleted_list_item,omitempty"`
	NumberedListItem   *notionBlockContent `json:"numbered_list_item,omitempty"`
	Quote              *notionBlockContent `json:"quote,omitempty"`
	Callout            *notionBlockContent `json:"callout,omitempty"`
	Toggle             *notionBlockContent `json:"toggle,omitempty"`
	ToggleableHeading1 *notionBlockContent `json:"toggleable_heading_1,omitempty"`
	ToggleableHeading2 *notionBlockContent `json:"toggleable_heading_2,omitempty"`
	ToggleableHeading3 *notionBlockContent `json:"toggleable_heading_3,omitempty"`
	Code               *notionCodeBlock    `json:"code,omitempty"`
	LinkPreview        *notionLinkPreview  `json:"link_preview,omitempty"`
}

type notionBlockContent struct {
	RichText []notionRichText `json:"rich_text"`
}

type notionCodeBlock struct {
	RichText []notionRichText `json:"rich_text"`
	Language string           `json:"language,omitempty"`
}

type notionLinkPreview struct {
	URL string `json:"url"`
}

type notionChildrenResponse struct {
	Object     string        `json:"object"`
	Results    []notionBlock `json:"results"`
	NextCursor *string       `json:"next_cursor"`
	HasMore    bool          `json:"has_more"`
}

func (s *Source) queryDataSource(ctx context.Context, token, databaseID string, startCursor *string, bound *time.Time) ([]notionPage, *string, bool, error) {
	body := map[string]any{
		"page_size": sourcePageSize,
		"sorts": []map[string]string{
			{"timestamp": "last_edited_time", "direction": "ascending"},
		},
	}
	if startCursor != nil {
		body["start_cursor"] = *startCursor
	}
	if bound != nil {
		body["filter"] = map[string]any{
			"timestamp":        "last_edited_time",
			"last_edited_time": map[string]string{"on_or_after": bound.UTC().Format(time.RFC3339Nano)},
		}
	}
	var out notionQueryResponse
	requestURL := s.baseURL + "/v1/data_sources/" + url.PathEscape(databaseID) + "/query"
	if err := s.doPOST(ctx, token, requestURL, body, &out); err != nil {
		return nil, nil, false, err
	}
	return out.Results, out.NextCursor, out.HasMore, nil
}

func (s *Source) searchPages(ctx context.Context, token string, startCursor *string) ([]notionPage, *string, bool, error) {
	body := map[string]any{
		"page_size": sourcePageSize,
		"sort":      map[string]string{"direction": "descending", "timestamp": "last_edited_time"},
	}
	if startCursor != nil {
		body["start_cursor"] = *startCursor
	}
	var out notionSearchResponse
	if err := s.doPOST(ctx, token, s.baseURL+"/v1/search", body, &out); err != nil {
		return nil, nil, false, err
	}
	return out.Results, out.NextCursor, out.HasMore, nil
}

func (s *Source) getPage(ctx context.Context, token, pageID string) (notionPage, error) {
	var out notionPage
	_, err := s.doGET(ctx, token, s.baseURL+"/v1/pages/"+url.PathEscape(pageID), &out)
	if err != nil {
		return notionPage{}, err
	}
	return out, nil
}

func (s *Source) pageBlocksMarkdown(ctx context.Context, pageID string) (string, string, error) {
	token, err := s.token(ctx)
	if err != nil {
		return "", "", err
	}
	traversal := &blockTraversal{source: s, token: token}
	if err := traversal.walk(ctx, pageID, 0); err != nil {
		return "", "", err
	}
	return traversal.markdown(), traversal.firstHeading, nil
}

type blockTraversal struct {
	source       *Source
	token        string
	parts        []string
	firstHeading string
	blocks       int
	rawBytes     int
	stopped      bool
}

func (t *blockTraversal) markdown() string {
	return strings.Join(t.parts, "\n\n")
}

func (t *blockTraversal) walk(ctx context.Context, blockID string, depth int) error {
	if t.stopped || depth > maxDepth {
		return nil
	}
	var startCursor *string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if t.blocks >= maxBlocksPerPage || t.rawBytes >= maxBlocksBytes {
			t.stopped = true
			return nil
		}
		requestURL := t.source.baseURL + "/v1/blocks/" + url.PathEscape(blockID) + "/children?page_size=" + strconv.Itoa(sourcePageSize)
		if startCursor != nil {
			requestURL += "&start_cursor=" + url.QueryEscape(*startCursor)
		}
		var out notionChildrenResponse
		rawLen, err := t.source.doGET(ctx, t.token, requestURL, &out)
		if err != nil {
			return err
		}
		t.rawBytes += rawLen
		if t.rawBytes >= maxBlocksBytes {
			t.stopped = true
			return nil
		}
		for _, block := range out.Results {
			if t.blocks >= maxBlocksPerPage {
				t.stopped = true
				return nil
			}
			t.blocks++
			text, include, isHeading := renderBlock(block)
			if include {
				if isHeading && t.firstHeading == "" {
					t.firstHeading = strings.TrimSpace(strings.TrimLeft(text, "#"))
				}
				t.parts = append(t.parts, text)
			}
			if block.HasChildren && depth < maxDepth && !t.stopped {
				if err := t.walk(ctx, block.ID, depth+1); err != nil {
					return err
				}
			}
			if t.stopped {
				return nil
			}
		}
		if !out.HasMore || out.NextCursor == nil {
			return nil
		}
		startCursor = out.NextCursor
	}
}

func renderBlock(block notionBlock) (string, bool, bool) {
	switch block.Type {
	case "paragraph":
		if block.Paragraph == nil {
			return "", false, false
		}
		text := richTextString(block.Paragraph.RichText)
		if strings.TrimSpace(text) == "" {
			return "", false, false
		}
		return text, true, false
	case "heading_1":
		if block.Heading1 == nil {
			return "", false, false
		}
		text := strings.TrimSpace(richTextString(block.Heading1.RichText))
		if text == "" {
			return "", false, false
		}
		return "# " + text, true, true
	case "heading_2":
		if block.Heading2 == nil {
			return "", false, false
		}
		text := strings.TrimSpace(richTextString(block.Heading2.RichText))
		if text == "" {
			return "", false, false
		}
		return "## " + text, true, true
	case "heading_3":
		if block.Heading3 == nil {
			return "", false, false
		}
		text := strings.TrimSpace(richTextString(block.Heading3.RichText))
		if text == "" {
			return "", false, false
		}
		return "### " + text, true, true
	case "bulleted_list_item":
		if block.BulletedListItem == nil {
			return "", false, false
		}
		text := strings.TrimSpace(richTextString(block.BulletedListItem.RichText))
		if text == "" {
			return "", false, false
		}
		return "- " + text, true, false
	case "numbered_list_item":
		if block.NumberedListItem == nil {
			return "", false, false
		}
		text := strings.TrimSpace(richTextString(block.NumberedListItem.RichText))
		if text == "" {
			return "", false, false
		}
		return "1. " + text, true, false
	case "quote":
		if block.Quote == nil {
			return "", false, false
		}
		text := strings.TrimSpace(richTextString(block.Quote.RichText))
		if text == "" {
			return "", false, false
		}
		return "> " + text, true, false
	case "callout":
		if block.Callout == nil {
			return "", false, false
		}
		text := strings.TrimSpace(richTextString(block.Callout.RichText))
		if text == "" {
			return "", false, false
		}
		return text, true, false
	case "toggle":
		if block.Toggle == nil {
			return "", false, false
		}
		text := strings.TrimSpace(richTextString(block.Toggle.RichText))
		if text == "" {
			return "", false, false
		}
		return text, true, false
	case "toggleable_heading_1", "toggleable_heading_2", "toggleable_heading_3":
		var content *notionBlockContent
		prefix := "## "
		switch block.Type {
		case "toggleable_heading_1":
			content, prefix = block.ToggleableHeading1, "# "
		case "toggleable_heading_2":
			content, prefix = block.ToggleableHeading2, "## "
		default:
			content, prefix = block.ToggleableHeading3, "### "
		}
		if content == nil {
			return "", false, false
		}
		text := strings.TrimSpace(richTextString(content.RichText))
		if text == "" {
			return "", false, false
		}
		return prefix + text, true, true
	case "code":
		if block.Code == nil {
			return "", false, false
		}
		code := strings.TrimRight(richTextString(block.Code.RichText), "\n")
		if code == "" {
			return "", false, false
		}
		lang := strings.TrimSpace(block.Code.Language)
		if strings.ContainsAny(lang, " \t\n\r`") {
			lang = ""
		}
		return "```" + lang + "\n" + code + "\n```", true, false
	case "divider":
		return "---", true, false
	case "link_preview":
		if block.LinkPreview == nil || strings.TrimSpace(block.LinkPreview.URL) == "" {
			return "", false, false
		}
		return strings.TrimSpace(block.LinkPreview.URL), true, false
	default:
		return "", false, false
	}
}

func richTextString(parts []notionRichText) string {
	var b strings.Builder
	for _, part := range parts {
		text := part.PlainText
		href := strings.TrimSpace(part.Href)
		if href != "" && href != text && text != "" {
			b.WriteString("[" + text + "](" + href + ")")
		} else {
			b.WriteString(text)
		}
	}
	return b.String()
}

func titleFromProperties(props map[string]notionProperty) string {
	keys := make([]string, 0, len(props))
	for key := range props {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		prop := props[key]
		if prop.Type == "title" && len(prop.Title) > 0 {
			if title := strings.TrimSpace(richTextString(prop.Title)); title != "" {
				return title
			}
		}
	}
	for _, key := range keys {
		prop := props[key]
		if len(prop.Title) > 0 {
			if title := strings.TrimSpace(richTextString(prop.Title)); title != "" {
				return title
			}
		}
	}
	return ""
}

func frontmatter(props map[string]notionProperty) string {
	if len(props) == 0 {
		return ""
	}
	keys := make([]string, 0, len(props))
	for key := range props {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var lines []string
	for _, key := range keys {
		value := propertyString(props[key])
		if value == "" {
			continue
		}
		lines = append(lines, key+": "+value)
	}
	if len(lines) == 0 {
		return ""
	}
	return "---\n" + strings.Join(lines, "\n") + "\n---"
}

func propertyString(prop notionProperty) string {
	switch prop.Type {
	case "title":
		return strings.TrimSpace(richTextString(prop.Title))
	case "rich_text":
		return strings.TrimSpace(richTextString(prop.RichText))
	case "select":
		if prop.Select != nil {
			return strings.TrimSpace(prop.Select.Name)
		}
	case "multi_select":
		var names []string
		for _, option := range prop.MultiSelect {
			if name := strings.TrimSpace(option.Name); name != "" {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		return strings.Join(names, ", ")
	case "status":
		if prop.Status != nil {
			return strings.TrimSpace(prop.Status.Name)
		}
	default:
		if len(prop.Title) > 0 {
			return strings.TrimSpace(richTextString(prop.Title))
		}
		if len(prop.RichText) > 0 {
			return strings.TrimSpace(richTextString(prop.RichText))
		}
		if prop.Select != nil {
			return strings.TrimSpace(prop.Select.Name)
		}
	}
	return ""
}

func labelsFromProperties(props map[string]notionProperty) []string {
	var out []string
	keys := make([]string, 0, len(props))
	for key := range props {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		prop := props[key]
		switch prop.Type {
		case "select":
			if prop.Select != nil {
				if name := strings.TrimSpace(prop.Select.Name); name != "" {
					out = append(out, name)
				}
			}
		case "multi_select":
			for _, option := range prop.MultiSelect {
				if name := strings.TrimSpace(option.Name); name != "" {
					out = append(out, name)
				}
			}
		case "status":
			if prop.Status != nil {
				if name := strings.TrimSpace(prop.Status.Name); name != "" {
					out = append(out, name)
				}
			}
		}
	}
	sort.Strings(out)
	compacted := out[:0]
	for _, label := range out {
		if len(compacted) == 0 || compacted[len(compacted)-1] != label {
			compacted = append(compacted, label)
		}
	}
	return compacted
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

var _ ports.Source = (*Source)(nil)
