// Package feeds implements two native read-only sources: RSS/Atom feeds and
// web pages discovered through sitemaps.
//
// Both sources issue only GET requests (no credentials are read, stored or
// attached), bound every response through the shared retrying HTTP client
// with a bounded body reader and a request timeout, and never follow
// redirects, so a redirect to a foreign host can never be fetched. https URLs
// are always allowed; http is accepted only for loopback hosts so local test
// servers and self-hosted feeds keep working.
//
// The sources follow the ports.Source contract: deterministic batches per
// checkpoint, resumable per-stream cursors, no tombstones (removal cannot be
// inferred from a feed or a sitemap, so the descriptors never declare the
// Deletes capability).
package feeds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const (
	// NameRSS is the RSS/Atom source provider name.
	NameRSS = "rss"
	// NameWeb is the web/sitemaps source provider name.
	NameWeb = "web"
	// UserAgent identifies the engine to feed, page and robots servers.
	UserAgent = "lv24-brain"
)

const (
	// maxFeedBytes bounds one RSS/Atom feed response.
	maxFeedBytes = int64(8 << 20)
	// maxSitemapBytes bounds one sitemap document response. The sitemap spec
	// allows 50 MB files; larger sitemaps must use a sitemap-index, so we
	// read at most 5 MiB per document.
	maxSitemapBytes = int64(5 << 20)
	// maxPageBytes bounds one page response.
	maxPageBytes = int64(2 << 20)
	// maxRobotsBytes bounds a robots.txt response.
	maxRobotsBytes = int64(256 << 10)

	// entryOverlap replays entries updated within the last five minutes
	// when a feed scan restarts, so entries that moved while a run was
	// consuming the feed are re-read instead of silently skipped.
	entryOverlap = 5 * time.Minute

	// defaultReadItems is the per-Read item budget when the host does not
	// pass an explicit limit.
	defaultReadItems = 100

	// defaultFeedEntries is the per-feed entry cap when the limit setting is
	// unset; maxFeedEntries is the largest accepted value.
	defaultFeedEntries = 100
	maxFeedEntries     = 500

	// maxFeedURLs and maxSitemapURLs bound the configured URL lists so a
	// checkpoint can never exceed domain.MaxCheckpointBytes.
	maxFeedURLs    = 50
	maxSitemapURLs = 20

	// maxURLBytes bounds one configured URL.
	maxURLBytes = 2048

	// maxSitemapChildren bounds the child sitemaps read per sitemap-index.
	maxSitemapChildren = 20

	// defaultMaxPages is the per-sitemap page cap when maxpages is unset;
	// maxMaxPages is the largest accepted value.
	defaultMaxPages = 50
	maxMaxPages     = 500

	// pagesPerRead bounds page fetches per Read of the web source,
	// independent of the host's item limit.
	pagesPerRead = 8

	// maxBodyBytes caps every produced item body at the domain limit.
	maxBodyBytes = domain.MaxItemBodyBytes

	// maxTitleBytes bounds item titles.
	maxTitleBytes = 512

	// maxExternalIDBytes bounds item external IDs; longer values fall back
	// to a full sha256 URL hash.
	maxExternalIDBytes = 1000
)

// Stream prefixes build the dynamic stream names
// "<prefix><16 hex chars of sha256(url)>"; the sources declare no static
// streams, each feed/sitemap gets an independent cursor.
const (
	rssStreamPrefix = "rss-"
	webStreamPrefix = "web-"

	checkpointV1 = 1
)

// hash16 returns the first 16 hex characters of the sha256 of raw: a stable
// and path-safe identifier for one URL (stream suffix, external ID prefix).
func hash16(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8])
}

// sha256Hex returns the full hex sha256 of raw.
func sha256Hex(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// validateSourceURL accepts only https URLs (http for loopback hosts so test
// servers and self-hosted feeds keep working) without credentials or
// fragments, and returns the normalized URL string.
func validateSourceURL(field, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", domain.NewValidationError(field, "is required")
	}
	if len(raw) > maxURLBytes {
		return "", domain.NewValidationError(field, fmt.Sprintf("must be at most %d bytes", maxURLBytes))
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", domain.NewValidationError(field, fmt.Sprintf("%q must be an absolute URL", raw))
	}
	if parsed.User != nil {
		return "", domain.NewValidationError(field, fmt.Sprintf("%q must not contain credentials", raw))
	}
	if parsed.Fragment != "" {
		return "", domain.NewValidationError(field, fmt.Sprintf("%q must not contain a fragment", raw))
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		host := parsed.Hostname()
		if strings.EqualFold(host, "localhost") {
			break
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", domain.NewValidationError(field,
				fmt.Sprintf("%q must be https; http is allowed only for loopback hosts", raw))
		}
	default:
		return "", domain.NewValidationError(field,
			fmt.Sprintf("%q must use the https scheme; http is allowed only for loopback hosts", raw))
	}
	return parsed.String(), nil
}

// parseList splits a comma or newline separated settings value and trims the
// entries.
func parseList(raw string) []string {
	var out []string
	for _, entry := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

// parseCount parses an optional counting setting: empty selects the default,
// other values must lie between 1 and max.
func parseCount(field, raw string, fallback, max int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > max {
		return 0, domain.NewValidationError(field, fmt.Sprintf("must be an integer between 1 and %d", max))
	}
	return value, nil
}

// fetchResult is one bounded GET response. Body is set only for 2xx
// responses. Redirects are never followed (the shared client answers them
// with http.ErrUseLastResponse), so the redirect target is never fetched and
// callers decide the policy per resource type from the status alone.
type fetchResult struct {
	Status int
	Body   []byte
	Header http.Header
}

// fetcher performs bounded GET requests through an http.Client with a 30s
// request timeout (the same default connectors.NewHTTP applies) and redirect
// following disabled, so a redirect is reported instead of fetched.
type fetcher struct {
	client       *http.Client
	maxBodyBytes int64
}

// newFetcher builds a fetcher; a nil client selects the shared 30s-timeout
// client.
func newFetcher(client *http.Client, maxBodyBytes int64) *fetcher {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if client.CheckRedirect == nil {
		// Never follow redirects: the response answer lets callers decide,
		// and a redirect to a foreign host is never fetched.
		cloned := *client
		cloned.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		client = &cloned
	}
	return &fetcher{client: client, maxBodyBytes: maxBodyBytes}
}

// readBounded reads at most max bytes.
func readBounded(body io.Reader, max int64) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > max {
		return nil, fmt.Errorf("response exceeds %d bytes", max)
	}
	return value, nil
}

// fetch issues one bounded GET.
func (f *fetcher) fetch(ctx context.Context, rawURL string, header http.Header) (fetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fetchResult{}, fmt.Errorf("build request: %s", connectors.SanitizeError(err))
	}
	if header.Get("User-Agent") == "" {
		header.Set("User-Agent", UserAgent)
	}
	req.Header = header
	resp, err := f.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fetchResult{}, ctxErr
		}
		return fetchResult{}, fmt.Errorf("request failed: %s", connectors.SanitizeError(err))
	}
	defer resp.Body.Close()
	// Redirects answer as their own status; only 2xx bodies are read.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body, err := readBounded(resp.Body, f.maxBodyBytes)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fetchResult{}, ctxErr
			}
			return fetchResult{}, fmt.Errorf("read response: %s", connectors.SanitizeError(err))
		}
		return fetchResult{Status: resp.StatusCode, Body: body, Header: resp.Header}, nil
	}
	return fetchResult{Status: resp.StatusCode, Header: resp.Header}, nil
}

// conditionalHeaders builds revalidation request headers.
func conditionalHeaders(etag, lastModified string) http.Header {
	header := http.Header{}
	header.Set("User-Agent", UserAgent)
	if etag != "" {
		header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		header.Set("If-Modified-Since", lastModified)
	}
	return header
}

// plainHeaders is a minimal header set without validators.
func plainHeaders() http.Header {
	header := http.Header{}
	header.Set("User-Agent", UserAgent)
	return header
}

// validatorsFrom copies the revalidation validators of a response.
func validatorsFrom(header http.Header) (etag, lastModified string) {
	return strings.TrimSpace(header.Get("ETag")), strings.TrimSpace(header.Get("Last-Modified"))
}

// httpTime formats t for If-Modified-Since/Last-Modified headers.
func httpTime(t time.Time) string {
	return t.UTC().Format(http.TimeFormat)
}

// capUTF8 cuts value to at most limit bytes without splitting a rune.
func capUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit]
}

// timePtr returns a pointer to the UTC value of t; zero stays nil.
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// laterTime returns the later of a and b (nil means unset).
func laterTime(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.After(*a):
		return b
	default:
		return a
	}
}
