package feeds

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
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

// The web source reads sitemaps (urlset and sitemap-index) and fetches the
// listed pages into page items: one dynamic stream per sitemap URL
// (web-<hash>), a bounded page budget per Read, lastmod-aware re-scans and
// optional robots.txt compliance. It never follows redirects, never touches
// hosts the operator did not allow, and stops the run on 401/403.
//
// Setting keys follow the domain's [a-z][a-z0-9] alphabet (no underscores),
// so the conventional names max_pages/allow_hosts/respect_robots are spelled
// "maxpages", "allowhosts" and "respectrobots".

// WebDescriptor returns the static provider description.
func WebDescriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider: NameWeb, DisplayName: "Web pages/sitemaps",
		Capabilities: domain.Capabilities{
			Backfill: true, Incremental: true,
			AuthModes: []domain.AuthMode{domain.AuthNone},
		},
		DefaultPublish: domain.PublishShared,
		ConfigFields: []domain.ConfigField{
			{Key: "sitemaps", Label: "Sitemap URLs", Required: true,
				Help: "Comma or newline separated sitemap or sitemap-index URLs (https, or http on loopback for tests)."},
			{Key: "maxpages", Label: "Max pages per sitemap",
				Help: "Optional page cap per sitemap; 1-500, default 50."},
			{Key: "allowhosts", Label: "Allowed page hosts",
				Help: "Optional comma separated host or host:port list; default is each sitemap's own host and port."},
			{Key: "respectrobots", Label: "Respect robots.txt",
				Help: "Optional true/false; default true."},
		},
	}
}

// webCursor is the checkpoint payload of one sitemap stream:
// {sitemapHash,urlIndex,etag,lastModified,watermark}.
type webCursor struct {
	// SitemapHash is the hash of the sitemap URL owning this cursor; a
	// checkpoint pasted into another stream is rejected.
	SitemapHash string `json:"sitemapHash"`
	// URLIndex is the next page index of the scan in progress; it resets
	// to 0 once a sitemap's pages are fully consumed, and the next pass
	// re-enters through the document validators.
	URLIndex int `json:"urlIndex"`
	// ETag and LastModified are the sitemap document validators, reused for
	// the conditional re-fetch that starts a pass.
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	// Watermark is the newest page lastmod processed so far; re-scans skip
	// pages with lastmod <= watermark.
	Watermark *time.Time `json:"watermark,omitempty"`
}

// WebSource reads configured sitemaps into raw page items.
type WebSource struct {
	connection  domain.Connection
	sitemapURLs []string
	// sitemapHosts is the default host policy when allowhosts is unset:
	// pages and child sitemaps must share a configured sitemap's host.
	sitemapHosts map[string]bool
	// pageLimit caps one sitemap's resolved page list.
	pageLimit int
	// allowHosts restricts page fetches; empty means the sitemap's host.
	allowHosts    map[string]bool
	respectRobots bool
	http          *fetcher
	pageHTTP      *fetcher
	robotsHTTP    *fetcher
}

// NewWeb validates the connection and builds the web source.
func NewWeb(conn domain.Connection, client *http.Client) (*WebSource, error) {
	if err := WebDescriptor().ValidateConnection(conn); err != nil {
		return nil, err
	}
	sitemapURLs, err := parseSitemapURLs(conn.Settings["sitemaps"])
	if err != nil {
		return nil, err
	}
	pageLimit, err := parseCount("connection.settings.maxpages", conn.Settings["maxpages"], defaultMaxPages, maxMaxPages)
	if err != nil {
		return nil, err
	}
	allowHosts, err := parseAllowHosts(conn.Settings["allowhosts"])
	if err != nil {
		return nil, err
	}
	respectRobots := true
	if raw := strings.TrimSpace(conn.Settings["respectrobots"]); raw != "" {
		respectRobots, err = strconv.ParseBool(raw)
		if err != nil {
			return nil, domain.NewValidationError("connection.settings.respectrobots", "must be true or false")
		}
	}
	sitemapHosts := make(map[string]bool, len(sitemapURLs))
	for _, sitemapURL := range sitemapURLs {
		parsed, parseErr := url.Parse(sitemapURL)
		if parseErr != nil {
			return nil, domain.NewValidationError("connection.settings.sitemaps", "contains an invalid URL")
		}
		sitemapHosts[strings.ToLower(parsed.Host)] = true
	}
	return &WebSource{
		connection:    conn,
		sitemapURLs:   sitemapURLs,
		sitemapHosts:  sitemapHosts,
		pageLimit:     pageLimit,
		allowHosts:    allowHosts,
		respectRobots: respectRobots,
		http:          newFetcher(client, maxSitemapBytes),
		pageHTTP:      newFetcher(client, maxPageBytes),
		robotsHTTP:    newFetcher(client, maxRobotsBytes),
	}, nil
}

// parseSitemapURLs normalizes, validates, dedupes and sorts the configured
// sitemap URLs so streams and scan order stay deterministic.
func parseSitemapURLs(raw string) ([]string, error) {
	var sitemapURLs []string
	for _, entry := range parseList(raw) {
		normalized, err := validateSourceURL("connection.settings.sitemaps", entry)
		if err != nil {
			return nil, err
		}
		sitemapURLs = append(sitemapURLs, normalized)
	}
	if len(sitemapURLs) == 0 {
		return nil, domain.NewValidationError("connection.settings.sitemaps", "must contain at least one sitemap URL")
	}
	if len(sitemapURLs) > maxSitemapURLs {
		return nil, domain.NewValidationError("connection.settings.sitemaps", fmt.Sprintf("must contain at most %d sitemaps", maxSitemapURLs))
	}
	return sortedUnique(sitemapURLs), nil
}

// parseAllowHosts lowercases the configured host list.
func parseAllowHosts(raw string) (map[string]bool, error) {
	var allowHosts map[string]bool
	for _, entry := range parseList(raw) {
		if strings.ContainsAny(entry, "/ ") {
			return nil, domain.NewValidationError("connection.settings.allowhosts", fmt.Sprintf("%q is not a host name", entry))
		}
		if allowHosts == nil {
			allowHosts = map[string]bool{}
		}
		allowHosts[strings.ToLower(entry)] = true
	}
	return allowHosts, nil
}

// Descriptor returns the static provider description.
func (s *WebSource) Descriptor() domain.SourceDescriptor { return WebDescriptor() }

// Check fetches each configured sitemap with a bounded response and
// validates that it parses as a urlset or sitemap-index. No credentials
// exist in this mode.
func (s *WebSource) Check(ctx context.Context) error {
	for _, sitemapURL := range s.sitemapURLs {
		stream := streamOf(webStreamPrefix, sitemapURL)
		result, err := s.http.fetch(ctx, sitemapURL, plainHeaders())
		if err != nil {
			return fmt.Errorf("web check %s: %s", stream, connectors.SanitizeError(err))
		}
		if result.Status >= 300 && result.Status < 400 {
			return fmt.Errorf("web check %s: sitemap returned http %d; redirects are not followed", stream, result.Status)
		}
		if result.Status < 200 || result.Status >= 300 {
			return fmt.Errorf("web check %s: http %d", stream, result.Status)
		}
		if _, _, err := parseSitemapDocument(result.Body); err != nil {
			return fmt.Errorf("web check %s: %s", stream, connectors.SanitizeError(err))
		}
	}
	return nil
}

// Read returns one bounded page of site pages with the resumable cursors of
// the sitemaps it touched.
func (s *WebSource) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, err
	}
	if req.Connection.ID != s.connection.ID || req.Connection.Provider != NameWeb {
		return ports.ReadBatch{}, domain.NewValidationError("web.connection", "does not match source connection")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultReadItems
	}
	items := make([]domain.Item, 0, min(limit, 32))
	var checkpoints []domain.Checkpoint
	robots := newRobots()
	pageBudget := pagesPerRead
	for _, sitemapURL := range s.sitemapURLs {
		if err := ctx.Err(); err != nil {
			return ports.ReadBatch{}, err
		}
		if pageBudget <= 0 {
			return ports.ReadBatch{Items: items, Checkpoints: checkpoints, More: true}, nil
		}
		stream := streamOf(webStreamPrefix, sitemapURL)
		cursor, err := loadWebCursor(req.Checkpoints, stream)
		if err != nil {
			return ports.ReadBatch{}, err
		}
		cursor.SitemapHash = strings.TrimPrefix(stream, webStreamPrefix)
		document, err := s.resolve(ctx, sitemapURL, cursor)
		if err != nil {
			return ports.ReadBatch{}, err
		}
		if document.unchanged || document.skipped {
			// Unchanged document (304 against the cursor validators): the
			// watermark already covers the pass, nothing moved. Skipped
			// documents (redirect/error/malformed) keep their cursor too.
			continue
		}
		cursor.ETag, cursor.LastModified = document.validators.etag, document.validators.lastModified
		watermark := cursor.Watermark
		maxSeen := watermark
		pendingIndex := -1
		pages := document.pages
		for index := cursor.URLIndex; index < len(pages); index++ {
			page := pages[index]
			if watermark != nil && page.lastmod != nil && !page.lastmod.After(*watermark) {
				// The sitemap says nothing newer than what was consumed.
				cursor.URLIndex = index + 1
				continue
			}
			if pageBudget <= 0 || len(items) >= limit {
				cursor.URLIndex = index
				pendingIndex = index
				break
			}
			pageBudget--
			item, fetched, stop := s.fetchPage(ctx, page, robots)
			if stop != nil {
				return ports.ReadBatch{}, stop
			}
			cursor.URLIndex = index + 1
			if fetched {
				items = append(items, item)
				maxSeen = laterTime(maxSeen, page.lastmod)
			}
		}
		if pendingIndex < 0 {
			// Sitemap fully consumed: wrap to the top; the next pass
			// re-enters through the document validators and the watermark
			// lastmod rule. Only a complete pass may move the watermark; a
			// pending page is resumed by URLIndex and must keep the old
			// frontier.
			cursor.URLIndex = 0
			watermark = maxSeen
		}
		cursor.Watermark = watermark
		data, err := json.Marshal(cursor)
		if err != nil {
			return ports.ReadBatch{}, fmt.Errorf("web read %s: %w", stream, err)
		}
		checkpoints = append(checkpoints, domain.Checkpoint{Stream: stream, Version: checkpointV1, Data: data})
		if pendingIndex >= 0 {
			return ports.ReadBatch{Items: items, Checkpoints: checkpoints, More: true}, nil
		}
	}
	return ports.ReadBatch{Items: items, Checkpoints: checkpoints, More: false}, nil
}

// webPage is one resolved sitemap URL with its lastmod.
type webPage struct {
	loc     string
	lastmod *time.Time
}

// validators carries a response's revalidation validators.
type validators struct {
	etag         string
	lastModified string
}

// sitemapDocument is the resolved state of one sitemap document for this
// pass.
type sitemapDocument struct {
	// unchanged: the conditional re-fetch answered 304, nothing moved.
	unchanged bool
	// skipped: redirect/error/malformed; the cursor is kept as-is.
	skipped bool
	// pages in document order, truncated to the page cap.
	pages      []webPage
	validators validators
}

// resolve fetches the sitemap document (conditionally when the cursor marks
// a completed pass), expanding a sitemap-index by fetching its children, and
// truncates the aggregate page list to the configured page cap. 401/403 stop
// the run.
func (s *WebSource) resolve(ctx context.Context, sitemapURL string, cursor webCursor) (sitemapDocument, error) {
	document := sitemapDocument{validators: validators{etag: cursor.ETag, lastModified: cursor.LastModified}}
	conditional := cursor.Watermark != nil && cursor.URLIndex == 0
	header := plainHeaders()
	if conditional {
		header = conditionalHeaders(cursor.ETag, cursor.LastModified)
	}
	result, err := s.http.fetch(ctx, sitemapURL, header)
	if err != nil {
		return sitemapDocument{}, fmt.Errorf("web read %s: %s", streamOf(webStreamPrefix, sitemapURL), connectors.SanitizeError(err))
	}
	if result.Status == http.StatusNotModified && !conditional {
		// Without cursor state a 304 cannot be interpreted: re-read fully.
		result, err = s.http.fetch(ctx, sitemapURL, plainHeaders())
		if err != nil {
			return sitemapDocument{}, fmt.Errorf("web read %s: %s", streamOf(webStreamPrefix, sitemapURL), connectors.SanitizeError(err))
		}
	}
	if result.Status == http.StatusNotModified && conditional {
		return sitemapDocument{unchanged: true, validators: document.validators}, nil
	}
	if result.Status >= 300 && result.Status < 400 {
		// Redirects are never followed; the sitemap is skipped this pass.
		return sitemapDocument{skipped: true, validators: document.validators}, nil
	}
	if result.Status == http.StatusUnauthorized || result.Status == http.StatusForbidden {
		return sitemapDocument{}, fmt.Errorf("web read %s: http %d", streamOf(webStreamPrefix, sitemapURL), result.Status)
	}
	if result.Status < 200 || result.Status >= 300 {
		// Other document errors skip the sitemap; the next pass retries.
		return sitemapDocument{skipped: true, validators: document.validators}, nil
	}
	document.validators.etag, document.validators.lastModified = validatorsFrom(result.Header)
	kind, entries, err := parseSitemapDocument(result.Body)
	if err != nil {
		// A malformed document skips the sitemap instead of failing the run.
		return sitemapDocument{skipped: true, validators: document.validators}, nil
	}
	if kind == sitemapKindIndex {
		pages, err := s.expandIndex(ctx, entries)
		if err != nil {
			return sitemapDocument{}, err
		}
		if len(pages) > s.pageLimit {
			pages = pages[:s.pageLimit]
		}
		document.pages = pages
		return document, nil
	}
	pages := make([]webPage, 0, len(entries))
	for _, entry := range entries {
		pages = append(pages, webPage{loc: entry.loc, lastmod: entry.lastmod})
	}
	if len(pages) > s.pageLimit {
		pages = pages[:s.pageLimit]
	}
	document.pages = pages
	return document, nil
}

// expandIndex fetches the child sitemaps of a sitemap-index, sorted for
// determinism, and concatenates their page entries in document order.
func (s *WebSource) expandIndex(ctx context.Context, children []sitemapLoc) ([]webPage, error) {
	locs := make([]string, 0, len(children))
	for _, child := range children {
		loc := strings.TrimSpace(child.loc)
		if loc == "" {
			continue
		}
		if _, err := validateSourceURL("sitemap.index", loc); err != nil {
			// Child sitemaps outside the allowed scheme are never fetched.
			continue
		}
		parsed, err := url.Parse(loc)
		if err != nil || !s.hostAllowed(parsed) {
			// Child sitemaps outside the host policy are never fetched.
			continue
		}
		locs = append(locs, loc)
	}
	if len(locs) > maxSitemapChildren {
		locs = locs[:maxSitemapChildren]
	}
	sort.Strings(locs)
	var pages []webPage
	for _, loc := range locs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := s.http.fetch(ctx, loc, plainHeaders())
		if err != nil {
			return nil, fmt.Errorf("web read: child sitemap %s: %s", streamOf(webStreamPrefix, loc), connectors.SanitizeError(err))
		}
		if result.Status == http.StatusUnauthorized || result.Status == http.StatusForbidden {
			return nil, fmt.Errorf("web read: child sitemap %s: http %d", streamOf(webStreamPrefix, loc), result.Status)
		}
		if result.Status < 200 || result.Status >= 300 {
			// Redirected or failing children are skipped.
			continue
		}
		kind, entries, err := parseSitemapDocument(result.Body)
		if err != nil || kind == sitemapKindIndex {
			continue
		}
		for _, entry := range entries {
			pages = append(pages, webPage{loc: entry.loc, lastmod: entry.lastmod})
		}
	}
	return pages, nil
}

// pageStatus reports what a page fetch produced.
type pageStatus byte

const (
	pageSkipped pageStatus = iota
	pageFetched
)

// fetchPage ingests one page within the host and robots policy: redirects,
// transport failures and out-of-policy URLs never fail the run; 401/403 stop
// it.
func (s *WebSource) fetchPage(ctx context.Context, page webPage, robots *robots) (domain.Item, bool, error) {
	pageURL, err := validateSourceURL("sitemap.page", page.loc)
	if err != nil {
		// The sitemap advertised a URL outside the fetch policy.
		return domain.Item{}, false, nil
	}
	parsed, err := url.Parse(pageURL)
	if err != nil {
		return domain.Item{}, false, nil
	}
	if !s.hostAllowed(parsed) {
		return domain.Item{}, false, nil
	}
	if s.respectRobots && robots.disallows(ctx, s.robotsHTTP, parsed) {
		return domain.Item{}, false, nil
	}
	header := plainHeaders()
	// Page-level conditional GETs would compare If-Modified-Since against the
	// page's own Last-Modified and can answer 304 on the first scan, silently
	// dropping the page. The sitemap's lastmod plus the per-stream watermark
	// is the revalidation contract: a page is fetched exactly when the
	// sitemap advertises a newer lastmod.
	result, err := s.pageHTTP.fetch(ctx, pageURL, header)
	if err != nil {
		// Transport failures skip the page; the next pass retries.
		return domain.Item{}, false, nil
	}
	if result.Status == http.StatusUnauthorized || result.Status == http.StatusForbidden {
		return domain.Item{}, false, fmt.Errorf("web read: page %s http %d", streamOf(webStreamPrefix, pageURL), result.Status)
	}
	if result.Status == http.StatusNotModified {
		return domain.Item{}, false, nil
	}
	if result.Status < 200 || result.Status >= 300 {
		return domain.Item{}, false, nil
	}
	item := domain.Item{Kind: domain.ItemPage}
	if len(s.connection.Projects) == 1 {
		item.ProjectID = s.connection.Projects[0]
	}
	item.Ref = domain.ItemRef{ConnectionID: s.connection.ID, ExternalID: pageExternalID(pageURL)}
	item.Title = pageTitleOf(result.Body, parsed)
	item.Body = capUTF8(htmlToText(string(result.Body)), maxBodyBytes)
	item.URL = pageURL
	if page.lastmod != nil {
		item.CreatedAt = page.lastmod.UTC()
		item.UpdatedAt = page.lastmod.UTC()
	}
	return item, true, nil
}

// hostAllowed reports whether the URL passes allowhosts, or the same-host
// default (a configured sitemap's host and port) over validated scheme/loopback
// URLs.
func (s *WebSource) hostAllowed(parsed *url.URL) bool {
	host := strings.ToLower(parsed.Host)
	if len(s.allowHosts) > 0 {
		return s.allowHosts[host]
	}
	return s.sitemapHosts[host]
}

// pageExternalID keeps the page URL (stable and readable) unless it exceeds
// the external ID bound; long URLs fall back to a full sha256.
func pageExternalID(pageURL string) string {
	if len(pageURL) > maxExternalIDBytes {
		return "sha256-" + sha256Hex(pageURL)
	}
	return pageURL
}

// pageTitleOf picks the decoded <title> or falls back to the URL path.
func pageTitleOf(body []byte, parsed *url.URL) string {
	if title := capUTF8(htmlTitle(string(body)), maxTitleBytes); title != "" {
		return title
	}
	for _, segment := range strings.Split(strings.Trim(parsed.Path, "/"), "/") {
		if segment == "" {
			continue
		}
		if decoded, err := url.PathUnescape(segment); err == nil {
			segment = decoded
		}
		if title := strings.TrimSpace(segment); title != "" {
			return capUTF8(title, maxTitleBytes)
		}
	}
	return capUTF8(parsed.Hostname()+parsed.Path, maxTitleBytes)
}

// loadWebCursor validates the persisted cursor of one sitemap stream.
func loadWebCursor(checkpoints map[string]domain.Checkpoint, stream string) (webCursor, error) {
	cursor := webCursor{}
	checkpoint, ok := checkpoints[stream]
	if !ok {
		return cursor, nil
	}
	if checkpoint.Stream != stream || checkpoint.Version != checkpointV1 {
		return cursor, domain.NewValidationError("web.checkpoint", "unsupported stream or version")
	}
	if err := json.Unmarshal(checkpoint.Data, &cursor); err != nil {
		return cursor, domain.NewValidationError("web.checkpoint", "is not a valid sitemap cursor")
	}
	if cursor.SitemapHash != strings.TrimPrefix(stream, webStreamPrefix) ||
		cursor.URLIndex < 0 || cursor.URLIndex > maxSitemapChildren*maxMaxPages ||
		len(cursor.ETag) > maxURLBytes || len(cursor.LastModified) > maxURLBytes {
		return cursor, domain.NewValidationError("web.checkpoint", "cursor does not belong to this sitemap")
	}
	return cursor, nil
}

// sitemapKind selects the document root type.
type sitemapKind byte

const (
	sitemapKindURL sitemapKind = iota
	sitemapKindIndex
)

// sitemapLoc is one raw <loc> entry of any sitemap document.
type sitemapLoc struct {
	loc     string
	lastmod *time.Time
}

// sitemapChildXML is one <url> or <sitemap> child of a sitemap document.
type sitemapChildXML struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod"`
}

// urlsetXML parses <urlset> documents; the XMLName tag doubles as the root
// check.
type urlsetXML struct {
	XMLName xml.Name          `xml:"urlset"`
	Entries []sitemapChildXML `xml:"url"`
}

// indexXML parses <sitemapindex> documents.
type indexXML struct {
	XMLName xml.Name          `xml:"sitemapindex"`
	Entries []sitemapChildXML `xml:"sitemap"`
}

// parseSitemapDocument validates the XML root and returns the entries of a
// urlset or the children of a sitemap-index.
func parseSitemapDocument(body []byte) (sitemapKind, []sitemapLoc, error) {
	collect := func(raw []sitemapChildXML) []sitemapLoc {
		out := make([]sitemapLoc, 0, len(raw))
		for _, child := range raw {
			loc := strings.TrimSpace(child.Loc)
			if loc == "" {
				continue
			}
			entry := sitemapLoc{loc: loc}
			if lastmod, ok := parseFeedTime(child.LastMod); ok {
				entry.lastmod = timePtr(lastmod)
			}
			out = append(out, entry)
		}
		return out
	}
	var urlset urlsetXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&urlset); err == nil {
		return sitemapKindURL, collect(urlset.Entries), nil
	}
	var index indexXML
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&index); err == nil {
		return sitemapKindIndex, collect(index.Entries), nil
	}
	return 0, nil, fmt.Errorf("malformed sitemap: root is not urlset or sitemapindex")
}

var _ ports.Source = (*WebSource)(nil)
