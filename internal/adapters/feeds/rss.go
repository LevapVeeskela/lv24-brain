package feeds

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// The rss source reads RSS 2.0 and Atom feeds into document items: one
// dynamic stream per feed URL (rss-<hash>), conditional GET revalidation and
// a bounded per-feed entry budget per Read. No credentials, no tombstones:
// entries disappear from a feed without becoming tombstones, so the streams
// only ever upsert.

// RSSDescriptor returns the static provider description.
func RSSDescriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider: NameRSS, DisplayName: "RSS/Atom",
		Capabilities: domain.Capabilities{
			Backfill: true, Incremental: true,
			AuthModes: []domain.AuthMode{domain.AuthNone},
		},
		DefaultPublish: domain.PublishShared,
		ConfigFields: []domain.ConfigField{
			{Key: "urls", Label: "Feed URLs", Required: true,
				Help: "Comma or newline separated RSS 2.0/Atom feed URLs (https, or http on loopback for tests)."},
			{Key: "limit", Label: "Entries per read",
				Help: "Optional per-feed entry cap per Read batch; 1-500, default 100."},
		},
	}
}

// rssCursor is the checkpoint payload of one feed stream:
// {etag,lastModified,watermark,offset}.
type rssCursor struct {
	// ETag and LastModified carry the feed validators of the last 200
	// response for conditional revalidation.
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	// Watermark is the newest entry update time consumed so far; re-scans
	// skip entries older than watermark minus the 5m overlap.
	Watermark *time.Time `json:"watermark,omitempty"`
	// Offset is the next entry index of the scan in progress; it resets to
	// 0 once a feed is fully consumed, so the next scan starts from the top
	// and re-enters through validators and the overlap window.
	Offset int `json:"offset"`
}

// RSSSource reads configured feeds into raw items.
type RSSSource struct {
	connection domain.Connection
	feedURLs   []string
	// entryLimit caps one feed's entries per Read batch.
	entryLimit int
	http       *fetcher
}

// NewRSS validates the connection and builds the feed source.
func NewRSS(conn domain.Connection, client *http.Client) (*RSSSource, error) {
	if err := RSSDescriptor().ValidateConnection(conn); err != nil {
		return nil, err
	}
	feedURLs, err := parseFeedURLs(conn.Settings["urls"])
	if err != nil {
		return nil, err
	}
	entryLimit, err := parseCount("connection.settings.limit", conn.Settings["limit"], defaultFeedEntries, maxFeedEntries)
	if err != nil {
		return nil, err
	}
	return &RSSSource{
		connection: conn,
		feedURLs:   feedURLs,
		entryLimit: entryLimit,
		http:       newFetcher(client, maxFeedBytes),
	}, nil
}

// parseFeedURLs normalizes, validates, dedupes and sorts the configured feed
// URLs so streams and scan order stay deterministic.
func parseFeedURLs(raw string) ([]string, error) {
	var feedURLs []string
	for _, entry := range parseList(raw) {
		normalized, err := validateSourceURL("connection.settings.urls", entry)
		if err != nil {
			return nil, err
		}
		feedURLs = append(feedURLs, normalized)
	}
	if len(feedURLs) == 0 {
		return nil, domain.NewValidationError("connection.settings.urls", "must contain at least one feed URL")
	}
	if len(feedURLs) > maxFeedURLs {
		return nil, domain.NewValidationError("connection.settings.urls", fmt.Sprintf("must contain at most %d feeds", maxFeedURLs))
	}
	return sortedUnique(feedURLs), nil
}

func sortedUnique(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// streamOf returns the dynamic stream name of one feed or sitemap URL.
func streamOf(prefix, raw string) string { return prefix + hash16(raw) }

// Descriptor returns the static provider description.
func (s *RSSSource) Descriptor() domain.SourceDescriptor { return RSSDescriptor() }

// Check fetches every feed with a bounded response and validates that it
// parses as RSS 2.0 or Atom. No credentials exist in this mode.
func (s *RSSSource) Check(ctx context.Context) error {
	for _, feedURL := range s.feedURLs {
		stream := streamOf(rssStreamPrefix, feedURL)
		result, err := s.http.fetch(ctx, feedURL, plainHeaders())
		if err != nil {
			return fmt.Errorf("rss check %s: %s", stream, connectors.SanitizeError(err))
		}
		if result.Status >= 300 && result.Status < 400 {
			return fmt.Errorf("rss check %s: feed returned http %d; redirects are not followed", stream, result.Status)
		}
		if result.Status < 200 || result.Status >= 300 {
			return fmt.Errorf("rss check %s: http %d", stream, result.Status)
		}
		if _, err := parseFeedXML(result.Body); err != nil {
			return fmt.Errorf("rss check %s: %s", stream, connectors.SanitizeError(err))
		}
	}
	return nil
}

// Read returns one bounded page of feed entries with the resumable cursors
// of the feeds it touched.
func (s *RSSSource) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, err
	}
	if req.Connection.ID != s.connection.ID || req.Connection.Provider != NameRSS {
		return ports.ReadBatch{}, domain.NewValidationError("rss.connection", "does not match source connection")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultReadItems
	}
	if err := s.validateCheckpoints(req.Checkpoints); err != nil {
		return ports.ReadBatch{}, err
	}
	items := make([]domain.Item, 0, min(limit, 32))
	var checkpoints []domain.Checkpoint
	for _, feedURL := range s.feedURLs {
		if err := ctx.Err(); err != nil {
			return ports.ReadBatch{}, err
		}
		stream := streamOf(rssStreamPrefix, feedURL)
		cursor, err := loadRSSCursor(req.Checkpoints, stream)
		if err != nil {
			return ports.ReadBatch{}, err
		}
		result, err := s.http.fetch(ctx, feedURL, conditionalHeaders(cursor.ETag, cursor.LastModified))
		if err != nil {
			return ports.ReadBatch{}, fmt.Errorf("rss read %s: %s", stream, connectors.SanitizeError(err))
		}
		if result.Status == http.StatusNotModified {
			if cursor.Offset == 0 {
				// Unchanged feed: neither entries nor validators move and
				// the persisted cursor stays exactly as it is.
				continue
			}
			// A pending page needs the body: a conditional request must not
			// short-circuit resumption in the middle of a scan.
			result, err = s.http.fetch(ctx, feedURL, plainHeaders())
			if err != nil {
				return ports.ReadBatch{}, fmt.Errorf("rss read %s: %s", stream, connectors.SanitizeError(err))
			}
		}
		if result.Status == http.StatusNotModified {
			// Unchanged feed: neither entries nor validators move and the
			// persisted cursor stays exactly as it is.
			continue
		}
		if result.Status >= 300 && result.Status < 400 {
			return ports.ReadBatch{}, fmt.Errorf("rss read %s: feed returned http %d; redirects are not followed", stream, result.Status)
		}
		if result.Status < 200 || result.Status >= 300 {
			return ports.ReadBatch{}, fmt.Errorf("rss read %s: http %d", stream, result.Status)
		}
		entries, err := parseFeedXML(result.Body)
		if err != nil {
			return ports.ReadBatch{}, fmt.Errorf("rss read %s: %s", stream, connectors.SanitizeError(err))
		}
		if cursor.Offset > len(entries) {
			// The feed shrank or reordered; the watermark keeps older
			// entries out of the scan while it re-reads from the top.
			cursor.Offset = 0
		}
		var scanSince *time.Time
		if cursor.Watermark != nil {
			since := cursor.Watermark.Add(-entryOverlap)
			scanSince = &since
		}
		etag, lastModified := validatorsFrom(result.Header)
		watermark := cursor.Watermark
		maxSeen := watermark
		entryBudget := s.entryLimit
		pendingIndex := -1
		for index := cursor.Offset; index < len(entries); index++ {
			entry := entries[index]
			updatedAt := entry.updated
			if !filterEntry(updatedAt, scanSince, req.Since) {
				continue
			}
			if len(items) >= limit || entryBudget <= 0 {
				// An entry is pending; the batch stops here and the next
				// Read resumes at this entry.
				cursor.Offset = index
				pendingIndex = index
				break
			}
			entryBudget--
			items = append(items, entry.item(s.connection, feedURL))
			cursor.Offset = index + 1
			maxSeen = laterTime(maxSeen, timePtr(updatedAt))
		}
		var data []byte
		if pendingIndex < 0 {
			// Feed fully consumed: wrap to the top; the next scan re-enters
			// through validators and the overlap window. Only a complete
			// pass may move the watermark; a pending page is resumed by
			// offset and must keep the previous scan frontier.
			cursor.Offset = 0
			watermark = maxSeen
		}
		data, err = json.Marshal(rssCursor{ETag: etag, LastModified: lastModified, Watermark: watermark, Offset: cursor.Offset})
		if err != nil {
			return ports.ReadBatch{}, fmt.Errorf("rss read %s: %w", stream, err)
		}
		checkpoints = append(checkpoints, domain.Checkpoint{Stream: stream, Version: checkpointV1, Data: data})
		if pendingIndex >= 0 {
			return ports.ReadBatch{Items: items, Checkpoints: checkpoints, More: true}, nil
		}
	}
	return ports.ReadBatch{Items: items, Checkpoints: checkpoints, More: false}, nil
}

// validateCheckpoints rejects cursors for streams this connection does not
// own; silently ignoring them would hide a corrupted or mismatched catalogue.
func (s *RSSSource) validateCheckpoints(checkpoints map[string]domain.Checkpoint) error {
	allowed := make(map[string]struct{}, len(s.feedURLs))
	for _, feedURL := range s.feedURLs {
		allowed[streamOf(rssStreamPrefix, feedURL)] = struct{}{}
	}
	for key, checkpoint := range checkpoints {
		if _, ok := allowed[key]; !ok || checkpoint.Stream != key {
			return domain.NewValidationError("rss.checkpoint", "does not match a configured feed")
		}
	}
	return nil
}

// loadRSSCursor loads and validates the persisted cursor of one stream.
func loadRSSCursor(checkpoints map[string]domain.Checkpoint, stream string) (rssCursor, error) {
	cursor := rssCursor{}
	checkpoint, ok := checkpoints[stream]
	if !ok {
		return cursor, nil
	}
	if checkpoint.Stream != stream || checkpoint.Version != checkpointV1 {
		return cursor, domain.NewValidationError("rss.checkpoint", "unsupported stream or version")
	}
	if err := json.Unmarshal(checkpoint.Data, &cursor); err != nil {
		return cursor, domain.NewValidationError("rss.checkpoint", "is not a valid feed cursor")
	}
	if cursor.Offset < 0 || cursor.Offset > maxFeedEntries || len(cursor.ETag) > maxURLBytes || len(cursor.LastModified) > maxURLBytes {
		return cursor, domain.NewValidationError("rss.checkpoint", "cursor position is invalid")
	}
	return cursor, nil
}

// filterEntry reports whether the entry passes the scan window: the run's
// Since bound is absolute, the watermark window only applies to dated
// entries before watermark minus the overlap.
func filterEntry(updatedAt time.Time, scanSince, since *time.Time) bool {
	if since != nil && (updatedAt.IsZero() || updatedAt.Before(*since)) {
		return false
	}
	if scanSince != nil && !updatedAt.IsZero() && updatedAt.Before(*scanSince) {
		return false
	}
	return true
}

// feedEntry is one normalized feed item.
type feedEntry struct {
	id        string
	title     string
	body      string
	link      string
	author    string
	published time.Time
	updated   time.Time
}

// item converts the entry into a domain document item.
func (e feedEntry) item(conn domain.Connection, feedURL string) domain.Item {
	item := domain.Item{Kind: domain.ItemDocument}
	if len(conn.Projects) == 1 {
		item.ProjectID = conn.Projects[0]
	}
	item.Ref = domain.ItemRef{ConnectionID: conn.ID, ExternalID: e.externalID(feedURL)}
	item.Title = e.itemTitle()
	item.Body = capUTF8(htmlToText(e.body), maxBodyBytes)
	item.URL = e.link
	item.Author = capUTF8(rssTitle(e.author), 256)
	item.CreatedAt = e.published.UTC()
	item.UpdatedAt = e.updated.UTC()
	return item
}

// externalID is <feed-hash>/<entry-id>; the entry id falls back to the link
// and to a content hash when the feed provides nothing stable.
func (e feedEntry) externalID(feedURL string) string {
	feedHash := hash16(feedURL)
	id := firstNonEmpty(e.id, e.link)
	id = strings.TrimSpace(id)
	if id == "" || strings.ContainsRune(id, 0) || len(feedHash)+1+len(id) > maxExternalIDBytes {
		id = "hash-" + hash16(e.title+"\x00"+e.body)
	}
	return feedHash + "/" + id
}

// itemTitle keeps Validate's non-empty title invariant deterministic.
func (e feedEntry) itemTitle() string {
	title := capUTF8(strings.TrimSpace(rssTitle(e.title)), maxTitleBytes)
	if title == "" {
		title = capUTF8(strings.TrimSpace(e.link), maxTitleBytes)
	}
	if title == "" {
		line, _, _ := strings.Cut(e.body, "\n")
		title = capUTF8(rssTitle(line), maxTitleBytes)
	}
	if title == "" {
		title = "Feed entry"
	}
	return title
}

// rssXML parses RSS 2.0 documents; the XMLName tag doubles as the root check
// (a non-rss root fails the decode).
type rssXML struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Title string `xml:"title"`
		Items []struct {
			GUID        string `xml:"guid"`
			Link        string `xml:"link"`
			Title       string `xml:"title"`
			Description string `xml:"description"`
			Content     string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
			PubDate     string `xml:"pubDate"`
			Date        string `xml:"http://purl.org/dc/elements/1.1/ date"`
			Creator     string `xml:"http://purl.org/dc/elements/1.1/ creator"`
			Author      string `xml:"author"`
		} `xml:"item"`
	} `xml:"channel"`
}

type atomXML struct {
	XMLName xml.Name `xml:"feed"`
	Title   string   `xml:"title"`
	Entries []struct {
		ID        string `xml:"id"`
		Title     string `xml:"title"`
		Content   string `xml:"content"`
		Summary   string `xml:"summary"`
		Published string `xml:"published"`
		Issued    string `xml:"issued"`
		Updated   string `xml:"updated"`
		Modified  string `xml:"modified"`
		Links     []struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr"`
		} `xml:"link"`
		Author struct {
			Name  string `xml:"name"`
			Email string `xml:"email"`
		} `xml:"author"`
	} `xml:"entry"`
}

// parseFeedXML parses a bounded body as RSS 2.0 or Atom.
func parseFeedXML(body []byte) ([]feedEntry, error) {
	if entries, err := parseRSS2(body); err == nil {
		return entries, nil
	}
	if entries, err := parseAtom(body); err == nil {
		return entries, nil
	}
	return nil, fmt.Errorf("malformed feed: not RSS 2.0 or Atom")
}

func parseRSS2(body []byte) ([]feedEntry, error) {
	var document rssXML
	decoder := xml.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("malformed feed: %w", err)
	}
	entries := make([]feedEntry, 0, len(document.Channel.Items))
	for _, raw := range document.Channel.Items {
		title := raw.Title
		if title == "" {
			title = document.Channel.Title
		}
		published, _ := parseFeedTime(firstNonEmpty(raw.PubDate, raw.Date))
		entries = append(entries, feedEntry{
			id:        raw.GUID,
			title:     title,
			body:      firstNonEmpty(raw.Content, raw.Description),
			link:      strings.TrimSpace(raw.Link),
			author:    firstNonEmpty(raw.Creator, raw.Author),
			published: published,
			updated:   published,
		})
	}
	return entries, nil
}

func parseAtom(body []byte) ([]feedEntry, error) {
	var document atomXML
	decoder := xml.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("malformed feed: %w", err)
	}
	entries := make([]feedEntry, 0, len(document.Entries))
	for _, raw := range document.Entries {
		link := ""
		for _, candidate := range raw.Links {
			if candidate.Rel == "" || strings.EqualFold(candidate.Rel, "alternate") {
				link = strings.TrimSpace(candidate.Href)
				break
			}
		}
		author := firstNonEmpty(raw.Author.Name, raw.Author.Email)
		title := raw.Title
		if title == "" {
			title = document.Title
		}
		published, _ := parseFeedTime(firstNonEmpty(raw.Published, raw.Issued))
		updated, _ := parseFeedTime(firstNonEmpty(raw.Updated, raw.Modified, raw.Published, raw.Issued))
		entries = append(entries, feedEntry{
			id:        raw.ID,
			title:     title,
			body:      firstNonEmpty(raw.Content, raw.Summary),
			link:      link,
			author:    author,
			published: published,
			updated:   updated,
		})
	}
	return entries, nil
}

var feedTimeLayouts = []string{
	time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822,
	time.RFC3339, time.RFC3339Nano,
	// Alternates publishers use: single-digit day, ISO with offset.
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"2 Jan 2006 15:04:05 -0700",
}

// parseFeedTime accepts the date forms feeds use (RFC 822/1123 variants and
// Atom's RFC 3339); unknown forms are treated as absent.
func parseFeedTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	for _, layout := range feedTimeLayouts {
		if value, err := time.Parse(layout, raw); err == nil {
			return value.UTC(), true
		}
	}
	return time.Time{}, false
}

// firstNonEmpty returns the first non-empty trimmed string.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

var _ ports.Source = (*RSSSource)(nil)
