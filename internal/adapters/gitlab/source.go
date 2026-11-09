package gitlab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	apiPath              = "/api/v4"
	sourcePageSize       = 100
	sourceDefaultLimit   = 25
	sourceMaxLimit       = 500
	checkpointVersion    = 1
	checkpointTimeFormat = time.RFC3339Nano

	// sourceMaxPagesPerRead bounds the provider requests of one Read call, so
	// a batch never turns into an unbounded crawl. The cursor keeps the page
	// position, so the next batch continues without re-reading finished
	// pages.
	sourceMaxPagesPerRead = 10
	// sourceMaxPagesPerStream is a last-resort guard against a provider that
	// keeps announcing a next page forever. It is far above any realistic
	// project history (100 pages = 10k items per stream).
	sourceMaxPagesPerStream = 10 * 1000

	// maxNotesPerItem, maxNoteBodyBytes and maxSourceBodyBytes bound the
	// comments embedded in one item body: at most five notes, 16 KiB each,
	// and 256 KiB for the whole body (domain.MaxItemBodyBytes is 512 KiB).
	maxNotesPerItem    = 5
	maxNoteBodyBytes   = 16 << 10
	maxSourceBodyBytes = 256 << 10

	// pageOverlap replays a short window before the durable watermark on the
	// next pass. GitLab pages are numbered, so an item updated between two
	// page fetches can move across a page boundary; the overlap re-reads the
	// newest window and turns a possible missed update into an idempotent
	// upsert. See the Read documentation for the remaining limitation.
	pageOverlap = 5 * time.Minute

	// boundSlack turns GitLab's exclusive updated_after into an inclusive
	// lower bound: the query asks for updates after (bound - 1s) and the
	// source keeps items with updated_at >= bound. GitLab timestamps are
	// second-granular, so no item exactly at the bound is lost.
	boundSlack = time.Second
)

// Stream kinds. A checkpoint stream is per project because one connection
// can read several projects, so the stream names are dynamic
// ("p-<project hash>-issues" and so on) and Descriptor.Streams stays empty,
// as the contract allows for sources that name streams dynamically.
const (
	streamKindIssues        = "issues"
	streamKindMergeRequests = "merge_requests"
	streamKindCommits       = "commits"
)

// Source is the item-level read-only GitLab integration. Each configured
// project has independent issue, merge-request and commit streams.
type Source struct {
	connection  domain.Connection
	baseURL     string
	projects    []string
	credentials ports.CredentialProvider
	http        *connectors.HTTP
}

// Descriptor describes the native GitLab API source. Project paths are
// connection settings and the access token is held only by the credential
// provider.
//
// Streams is deliberately empty: checkpoint streams are per project
// (p-<project hash>-issues|merge_requests|commits) because one connection may
// read several projects, so there is no provider-wide stream list the host
// could validate a reset against.
func Descriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider:       Name,
		DisplayName:    "GitLab",
		Capabilities:   domain.Capabilities{Backfill: true, Incremental: true, AuthModes: []domain.AuthMode{domain.AuthToken, domain.AuthOAuthPKCE}},
		DefaultPublish: domain.PublishShared,
		ConfigFields: []domain.ConfigField{
			{Key: "projects", Label: "Projects", Required: true, Help: "Comma-separated GitLab project paths or numeric IDs."},
			{Key: "token", Label: "Personal access token", Required: true, Secret: true},
		},
		Backends: []domain.Backend{domain.BackendNative},
	}
}

// NewSource validates a GitLab connection and builds its native item source.
// conn.Settings["projects"] accepts comma or newline separated project paths
// and numeric IDs; the token is read from credentials under field "token".
func NewSource(conn domain.Connection, credentials ports.CredentialProvider, client *http.Client, now func() time.Time) (*Source, error) {
	if credentials == nil {
		return nil, domain.NewValidationError("gitlab.credentials", "are required")
	}
	if err := Descriptor().ValidateConnection(conn); err != nil {
		return nil, err
	}
	projects, err := sourceProjects(conn.Settings["projects"])
	if err != nil {
		return nil, err
	}
	baseURL, err := sourceBaseURL(conn.BaseURL)
	if err != nil {
		return nil, err
	}
	return &Source{
		connection:  conn,
		baseURL:     baseURL,
		projects:    projects,
		credentials: credentials,
		http:        connectors.NewHTTP(connectors.HTTPOptions{Client: client, Now: now}),
	}, nil
}

func sourceProjects(raw string) ([]string, error) {
	var projects []string
	seen := make(map[string]struct{})
	for _, value := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		project, err := normalizeProject(value)
		if err != nil {
			return nil, err
		}
		if project == "" {
			continue
		}
		if _, ok := seen[project]; ok {
			continue
		}
		seen[project] = struct{}{}
		projects = append(projects, project)
	}
	if len(projects) == 0 {
		return nil, domain.NewValidationError("gitlab.projects", "must contain at least one project path or ID")
	}
	sort.Strings(projects)
	return projects, nil
}

// sourceBaseURL normalizes the API base: the default is gitlab.com, a
// self-hosted instance may live under a subpath, and /api/v4 is appended
// exactly once. Userinfo, fragments and query strings are rejected so a
// token can never be smuggled in through the URL.
func sourceBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = DefaultBaseURL
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || !isAllowedBaseURL(parsed) || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return "", domain.NewValidationError("gitlab.baseURL", "must be an https URL without credentials or query; http is allowed only for loopback hosts")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(parsed.Path, apiPath) {
		parsed.Path += apiPath
	}
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

// Descriptor returns the source's static provider description.
func (s *Source) Descriptor() domain.SourceDescriptor { return Descriptor() }

// Check validates the token with GitLab's authenticated-user endpoint. It is
// a single read-only request, never includes the token or the response body
// in an error, and makes no claim about project access: a token that is valid
// but cannot see a private project fails on the first Read with HTTP 404.
func (s *Source) Check(ctx context.Context) error {
	token, err := s.token(ctx)
	if err != nil {
		return fmt.Errorf("gitlab check: %s", connectors.SanitizeError(err))
	}
	var identity struct {
		ID       int    `json:"id"`
		Username string `json:"username"`
	}
	if _, err := s.getURL(ctx, token, s.baseURL+"/user", &identity); err != nil {
		return fmt.Errorf("gitlab check: %s", connectors.SanitizeError(err, token))
	}
	if identity.ID <= 0 || strings.TrimSpace(identity.Username) == "" {
		return errors.New("gitlab check: authenticated user response is incomplete")
	}
	return nil
}

// Read returns the next bounded page across the configured project streams.
//
// Cursor semantics:
//   - each (project, kind) stream has an opaque checkpoint with a durable
//     watermark, the numbered page position and an offset inside that page;
//   - a page position only advances after every item of the page was
//     returned, so a crash replays at most the current page and a persisted
//     position never covers unstored items;
//   - More is true exactly while at least one stream still has a page or
//     offset pending;
//   - a completed pass rewinds to page 1 and the next pass starts from the
//     stream watermark minus a five-minute overlap.
//
// Time bounds:
//   - ReadRequest.Since and the durable watermark are inclusive lower bounds
//     on provider update time (for commits: the committer date). GitLab's
//     updated_after query parameter is exclusive, so the request asks for
//     updated_after = bound - 1s and the source keeps items whose timestamp
//     is not before the bound; an item exactly at the bound is returned.
//   - the source never filters a whole project when the bound is absent, and
//     it never reports an item missing from a filtered read as deleted.
//
// Known limitation (documented, not fixed): GitLab offers numbered pages
// only. An item updated *while an initial backfill is running* can move across
// a page boundary; the cursor stores the IDs already returned from the current
// page and replays that page when its prefix changes, so a shift inside the
// current page cannot skip a record (replayed items are idempotent upserts).
// A shift that moves an item backwards into a fully consumed earlier page can
// still be missed for that pass; the five-minute watermark overlap repairs the
// usual recent case, but there is no forced full re-backfill. The source never
// silently claims a full page is complete without pagination information.
func (s *Source) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, err
	}
	if req.Connection.ID != s.connection.ID || req.Connection.Provider != Name {
		return ports.ReadBatch{}, domain.NewValidationError("gitlab.connection", "does not match this source")
	}
	token, err := s.token(ctx)
	if err != nil {
		return ports.ReadBatch{}, fmt.Errorf("gitlab credentials: %s", connectors.SanitizeError(err))
	}
	limit := sourceLimit(req.Limit)
	streams := s.streams()
	states := make([]sourceCursor, len(streams))
	allDone := len(streams) > 0
	for index, stream := range streams {
		state, err := readSourceCursor(req.Checkpoints, stream)
		if err != nil {
			return ports.ReadBatch{}, err
		}
		states[index] = state
		allDone = allDone && state.Done
	}
	if allDone {
		// A completed pass starts the next incremental pass. Watermarks stay
		// durable; only the page walk restarts.
		for index := range states {
			states[index].Page, states[index].Offset, states[index].Done = 1, 0, false
			states[index].PassMax = time.Time{}
		}
	}
	batch := ports.ReadBatch{}
	pagesRead := 0
	// Read pages round-robin across streams until the item limit or the page
	// budget is reached. A page that yields no item (everything before the
	// lower bound) still counts as progress because its position advances.
	for len(batch.Items) < limit && pagesRead < sourceMaxPagesPerRead {
		progressed := false
		for index, stream := range streams {
			if len(batch.Items) >= limit || pagesRead >= sourceMaxPagesPerRead {
				break
			}
			state := states[index]
			if state.Done {
				continue
			}
			if state.Page > sourceMaxPagesPerStream {
				return ports.ReadBatch{}, fmt.Errorf("gitlab %s: pagination exceeded %d pages", stream.name, sourceMaxPagesPerStream)
			}
			pagesRead++
			progressed = true
			page, err := s.readStreamPage(ctx, token, stream, state.Page, sourceScanBound(state, req.Since))
			if err != nil {
				return ports.ReadBatch{}, fmt.Errorf("gitlab %s: %s", stream.name, connectors.SanitizeError(err, token))
			}
			pageIDs := make([]string, len(page.records))
			for index := range page.records {
				pageIDs[index] = sourceRecordID(stream.project, page.records[index])
			}
			// Numbered pages are mutable. Only trust the saved offset when the
			// fetched page starts with exactly the records already returned;
			// otherwise replay the page. Duplicates are idempotent, a skipped
			// record would be lost.
			if state.Offset > 0 && !samePagePrefix(pageIDs, state.Seen) {
				state.Offset, state.Seen = 0, nil
			}
			if state.Offset > len(page.records) {
				state.Offset = len(page.records)
			}
			for state.Offset < len(page.records) && len(batch.Items) < limit {
				recordIndex := state.Offset
				item, err := s.buildItem(ctx, token, stream, page.records[recordIndex])
				if err != nil {
					return ports.ReadBatch{}, fmt.Errorf("gitlab %s: %s", stream.name, connectors.SanitizeError(err, token))
				}
				state.Offset++
				if state.Offset > len(state.Seen) {
					state.Seen = append(state.Seen, pageIDs[recordIndex])
				}
				batch.Items = append(batch.Items, item)
			}
			if state.Offset < len(page.records) {
				states[index] = state
				continue
			}
			// The whole page was returned: this position may advance.
			state.PassMax = laterTime(state.PassMax, page.newest)
			next, ok, err := sourceNextPage(s.baseURL, state.Page, page.full, page.headers)
			if err != nil {
				return ports.ReadBatch{}, fmt.Errorf("gitlab %s: %s", stream.name, connectors.SanitizeError(err))
			}
			if ok {
				state.Page, state.Offset, state.Seen = next, 0, nil
				states[index] = state
				continue
			}
			states[index] = sourceCursor{
				Watermark: laterTime(state.Watermark, state.PassMax),
				Page:      1,
				Done:      true,
			}
		}
		if !progressed {
			break
		}
	}
	batch.Checkpoints = make([]domain.Checkpoint, 0, len(streams))
	for index, stream := range streams {
		batch.Checkpoints = append(batch.Checkpoints, encodeSourceCursor(stream.name, states[index]))
		if !states[index].Done {
			batch.More = true
		}
	}
	return batch, nil
}

type sourceStream struct {
	project string
	kind    string
	name    string
}

func (s *Source) streams() []sourceStream {
	streams := make([]sourceStream, 0, len(s.projects)*3)
	for _, project := range s.projects {
		sum := sha256.Sum256([]byte(project))
		prefix := "p-" + hex.EncodeToString(sum[:8])
		for _, kind := range []string{streamKindIssues, streamKindMergeRequests, streamKindCommits} {
			streams = append(streams, sourceStream{project: project, kind: kind, name: prefix + "-" + kind})
		}
	}
	return streams
}

type sourceCursor struct {
	// Watermark is the newest provider timestamp of the last completed pass.
	Watermark time.Time `json:"watermark,omitempty"`
	// PassMax is the newest timestamp returned in the current pass. It is
	// folded into Watermark when the pass completes.
	PassMax time.Time `json:"passMax,omitempty"`
	Page    int       `json:"page,omitempty"`
	Offset  int       `json:"offset,omitempty"`
	// Seen lists the record IDs already returned from the current page in
	// order. Numbered pages can shift when an item is updated or removed
	// during a backfill; a resume compares this prefix with the fetched page
	// and restarts the page (duplicates are idempotent) instead of trusting a
	// stale offset and skipping a record.
	Seen []string `json:"seen,omitempty"`
	Done bool     `json:"done,omitempty"`
}

// readSourceCursor decodes the checkpoint of one stream. Any malformed or
// unsupported cursor is rejected explicitly: silently restarting a stream
// would re-read history and could hide corruption.
func readSourceCursor(checkpoints map[string]domain.Checkpoint, stream sourceStream) (sourceCursor, error) {
	checkpoint, ok := checkpoints[stream.name]
	if !ok {
		return sourceCursor{Page: 1}, nil
	}
	if checkpoint.Stream != stream.name || checkpoint.Version != checkpointVersion {
		return sourceCursor{}, domain.NewValidationError("gitlab.checkpoint",
			fmt.Sprintf("stream %q version %d is not supported", checkpoint.Stream, checkpoint.Version))
	}
	var state sourceCursor
	if err := json.Unmarshal(checkpoint.Data, &state); err != nil {
		return sourceCursor{}, domain.NewValidationError("gitlab.checkpoint", "is not valid JSON")
	}
	if state.Page < 1 || state.Page > sourceMaxPagesPerStream || state.Offset < 0 || state.Offset > sourcePageSize || len(state.Seen) > sourcePageSize {
		return sourceCursor{}, domain.NewValidationError("gitlab.checkpoint", "position is invalid")
	}
	return state, nil
}

func encodeSourceCursor(stream string, state sourceCursor) domain.Checkpoint {
	data, _ := json.Marshal(state)
	return domain.Checkpoint{Stream: stream, Version: checkpointVersion, Data: data}
}

// sourceScanBound is the inclusive lower bound of the current pass: the
// durable watermark minus the overlap window, narrowed by ReadRequest.Since.
func sourceScanBound(state sourceCursor, since *time.Time) *time.Time {
	var bound *time.Time
	if !state.Watermark.IsZero() {
		at := state.Watermark.Add(-pageOverlap).UTC()
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

func sourceLimit(limit int) int {
	if limit <= 0 {
		return sourceDefaultLimit
	}
	if limit > sourceMaxLimit {
		return sourceMaxLimit
	}
	return limit
}

func laterTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right.UTC()
	}
	return left
}

type sourceIssue struct {
	IID         int       `json:"iid"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	State       string    `json:"state"`
	WebURL      string    `json:"web_url"`
	Author      author    `json:"author"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Labels      []string  `json:"labels"`
}

type sourceMergeRequest struct {
	IID         int       `json:"iid"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	State       string    `json:"state"`
	WebURL      string    `json:"web_url"`
	Author      author    `json:"author"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Labels      []string  `json:"labels"`
}

type sourceCommit struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Message       string    `json:"message"`
	AuthorName    string    `json:"author_name"`
	AuthorEmail   string    `json:"author_email"`
	CommittedDate time.Time `json:"committed_date"`
	AuthoredDate  time.Time `json:"authored_date"`
	WebURL        string    `json:"web_url"`
}

type sourceNote struct {
	ID        int       `json:"id"`
	Body      string    `json:"body"`
	Note      string    `json:"note"`
	Author    author    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	System    bool      `json:"system"`
}

// sourceRecord is one raw provider row of a stream page.
type sourceRecord struct {
	issue  *sourceIssue
	merge  *sourceMergeRequest
	commit *sourceCommit
}

// updatedAt is the provider update time used for filtering and watermarks.
// GitLab commits have no updated_at; the committer date is their identity in
// time.
func (r sourceRecord) updatedAt() time.Time {
	switch {
	case r.issue != nil:
		return r.issue.UpdatedAt
	case r.merge != nil:
		return r.merge.UpdatedAt
	case r.commit != nil:
		if !r.commit.CommittedDate.IsZero() {
			return r.commit.CommittedDate
		}
		return r.commit.AuthoredDate
	default:
		return time.Time{}
	}
}

// sourceRecordID is the stable external ID of a raw provider record. It does
// not fetch notes, so the cursor can fingerprint a page cheaply.
func sourceRecordID(project string, record sourceRecord) string {
	switch {
	case record.issue != nil:
		return project + "#issue-" + strconv.Itoa(record.issue.IID)
	case record.merge != nil:
		return project + "!mr-" + strconv.Itoa(record.merge.IID)
	case record.commit != nil:
		return project + "#commit-" + record.commit.ID
	default:
		return ""
	}
}

// samePagePrefix reports whether ids starts with the previously returned IDs.
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

type sourcePage struct {
	records []sourceRecord
	// newest is the newest provider timestamp of the returned records.
	newest time.Time
	// full reports whether the provider returned a full page (per_page
	// records before local filtering).
	full    bool
	headers http.Header
}

// readStreamPage fetches one numbered page of one stream. Records before the
// lower bound (or without a timestamp while a bound is set) are dropped
// before the caller sees them; the page still reports its newest timestamp
// so the pass watermark advances monotonically.
func (s *Source) readStreamPage(ctx context.Context, token string, stream sourceStream, page int, bound *time.Time) (sourcePage, error) {
	query := url.Values{"page": {strconv.Itoa(page)}, "per_page": {strconv.Itoa(sourcePageSize)}}
	endpoint := projectAPIPath(stream.project)
	updatedParam := "updated_after"
	switch stream.kind {
	case streamKindIssues:
		endpoint += "/issues"
		query.Set("state", "all")
		query.Set("scope", "all")
		query.Set("order_by", "updated_at")
		query.Set("sort", "asc")
	case streamKindMergeRequests:
		endpoint += "/merge_requests"
		query.Set("state", "all")
		query.Set("scope", "all")
		query.Set("order_by", "updated_at")
		query.Set("sort", "asc")
	case streamKindCommits:
		// GitLab lists commits newest first and has no sort parameter; the
		// since filter keeps incremental passes bounded.
		endpoint += "/repository/commits"
		updatedParam = "since"
	default:
		return sourcePage{}, fmt.Errorf("unknown stream kind %q", stream.kind)
	}
	if bound != nil {
		query.Set(updatedParam, bound.Add(-boundSlack).UTC().Format(checkpointTimeFormat))
	}
	requestURL := s.baseURL + endpoint + "?" + query.Encode()
	var records []sourceRecord
	var headers http.Header
	switch stream.kind {
	case streamKindIssues:
		var values []sourceIssue
		got, err := s.getURL(ctx, token, requestURL, &values)
		if err != nil {
			return sourcePage{}, err
		}
		headers = got
		for index := range values {
			records = append(records, sourceRecord{issue: &values[index]})
		}
	case streamKindMergeRequests:
		var values []sourceMergeRequest
		got, err := s.getURL(ctx, token, requestURL, &values)
		if err != nil {
			return sourcePage{}, err
		}
		headers = got
		for index := range values {
			records = append(records, sourceRecord{merge: &values[index]})
		}
	default:
		var values []sourceCommit
		got, err := s.getURL(ctx, token, requestURL, &values)
		if err != nil {
			return sourcePage{}, err
		}
		headers = got
		for index := range values {
			records = append(records, sourceRecord{commit: &values[index]})
		}
	}
	full := len(records) >= sourcePageSize
	kept := records[:0]
	var newest time.Time
	for _, record := range records {
		at := record.updatedAt()
		if bound != nil && (at.IsZero() || at.Before(*bound)) {
			continue
		}
		kept = append(kept, record)
		newest = laterTime(newest, at)
	}
	if len(kept) == 0 && len(records) > 0 {
		// Every record was older than the bound. Preserve the page's newest
		// timestamp so the pass watermark does not move backwards.
		for _, record := range records {
			newest = laterTime(newest, record.updatedAt())
		}
	}
	return sourcePage{records: kept, newest: newest, full: full, headers: headers}, nil
}

// buildItem converts one provider row into an item. Comments are fetched
// here, after the caller decided to return the item, so a batch only pays for
// the notes of the items it actually yields.
func (s *Source) buildItem(ctx context.Context, token string, stream sourceStream, record sourceRecord) (domain.Item, error) {
	endpoint := projectAPIPath(stream.project)
	projectID := s.itemProjectID()
	switch {
	case record.issue != nil:
		notes, err := s.readNotes(ctx, token, endpoint+"/issues/"+strconv.Itoa(record.issue.IID)+"/notes")
		if err != nil {
			return domain.Item{}, err
		}
		return sourceIssueItem(s.connection.ID, projectID, stream.project, *record.issue, notes), nil
	case record.merge != nil:
		notes, err := s.readNotes(ctx, token, endpoint+"/merge_requests/"+strconv.Itoa(record.merge.IID)+"/notes")
		if err != nil {
			return domain.Item{}, err
		}
		return sourceMergeRequestItem(s.connection.ID, projectID, stream.project, *record.merge, notes), nil
	case record.commit != nil:
		notes, err := s.readNotes(ctx, token, endpoint+"/repository/commits/"+url.PathEscape(record.commit.ID)+"/comments")
		if err != nil {
			return domain.Item{}, err
		}
		return sourceCommitItem(s.connection.ID, projectID, stream.project, *record.commit, notes), nil
	default:
		return domain.Item{}, errors.New("empty GitLab record")
	}
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

func sourceIssueItem(connectionID, projectID, project string, record sourceIssue, notes []sourceNote) domain.Item {
	return domain.Item{
		Ref:       domain.ItemRef{ConnectionID: connectionID, ExternalID: project + "#issue-" + strconv.Itoa(record.IID)},
		Kind:      domain.ItemIssue,
		Title:     record.Title,
		Body:      sourceBody(record.Description, notes),
		URL:       record.WebURL,
		Author:    sourceAuthor(record.Author),
		Labels:    sortedLabels(record.Labels),
		CreatedAt: record.CreatedAt,
		UpdatedAt: record.UpdatedAt,
		ProjectID: projectID,
	}
}

func sourceMergeRequestItem(connectionID, projectID, project string, record sourceMergeRequest, notes []sourceNote) domain.Item {
	return domain.Item{
		Ref:       domain.ItemRef{ConnectionID: connectionID, ExternalID: project + "!mr-" + strconv.Itoa(record.IID)},
		Kind:      domain.ItemMergeRequest,
		Title:     record.Title,
		Body:      sourceBody(record.Description, notes),
		URL:       record.WebURL,
		Author:    sourceAuthor(record.Author),
		Labels:    sortedLabels(record.Labels),
		CreatedAt: record.CreatedAt,
		UpdatedAt: record.UpdatedAt,
		ProjectID: projectID,
	}
}

func sourceCommitItem(connectionID, projectID, project string, record sourceCommit, notes []sourceNote) domain.Item {
	authorName := strings.TrimSpace(record.AuthorName)
	if record.AuthorEmail != "" {
		authorName += " <" + record.AuthorEmail + ">"
	}
	at := record.CommittedDate
	if at.IsZero() {
		at = record.AuthoredDate
	}
	title := strings.TrimSpace(record.Title)
	if title == "" {
		title = firstLine(record.Message)
	}
	return domain.Item{
		Ref:       domain.ItemRef{ConnectionID: connectionID, ExternalID: project + "#commit-" + record.ID},
		Kind:      domain.ItemCommit,
		Title:     title,
		Body:      sourceBody(record.Message, notes),
		URL:       record.WebURL,
		Author:    authorName,
		CreatedAt: at,
		UpdatedAt: at,
		ProjectID: projectID,
	}
}

// readNotes fetches at most maxNotesPerItem newest user comments in one
// request. One page of notes is requested and any extra rows are dropped, so
// the call is bounded regardless of how many notes the item has. System
// notes (state changes) are not comments and are skipped. GitLab returns the
// raw text; redaction is the host's job.
func (s *Source) readNotes(ctx context.Context, token, path string) ([]sourceNote, error) {
	query := url.Values{
		"order_by": {"created_at"},
		"sort":     {"desc"},
		"per_page": {strconv.Itoa(maxNotesPerItem)},
		"page":     {"1"},
	}
	requestURL := s.baseURL + path + "?" + query.Encode()
	var notes []sourceNote
	if _, err := s.getURL(ctx, token, requestURL, &notes); err != nil {
		return nil, err
	}
	kept := make([]sourceNote, 0, len(notes))
	for _, note := range notes {
		if note.System {
			continue
		}
		if strings.TrimSpace(note.Body) == "" && strings.TrimSpace(note.Note) == "" {
			continue
		}
		kept = append(kept, note)
	}
	if len(kept) > maxNotesPerItem {
		kept = kept[:maxNotesPerItem]
	}
	// The API returns newest first; render chronologically with a stable
	// tie-breaker so identical upstream data always produces identical text.
	sort.SliceStable(kept, func(i, j int) bool {
		if !kept[i].CreatedAt.Equal(kept[j].CreatedAt) {
			return kept[i].CreatedAt.Before(kept[j].CreatedAt)
		}
		return kept[i].ID < kept[j].ID
	})
	return kept, nil
}

func sourceBody(description string, notes []sourceNote) string {
	var body strings.Builder
	appendBounded(&body, strings.TrimSpace(description), maxSourceBodyBytes)
	if len(notes) == 0 {
		return body.String()
	}
	var comments strings.Builder
	comments.WriteString("\n\n### Comments\n")
	for _, note := range notes {
		text := note.Body
		if text == "" {
			text = note.Note
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		header := fmt.Sprintf("\n- **%s** (%s", sourceAuthor(note.Author), connectors.DisplayTime(note.CreatedAt, "unknown"))
		if note.ID > 0 {
			header += fmt.Sprintf(", note %d", note.ID)
		}
		if !note.UpdatedAt.IsZero() && note.UpdatedAt.After(note.CreatedAt) {
			header += ", edited " + connectors.FormatTime(note.UpdatedAt)
		}
		header += "): "
		comments.WriteString(header)
		comments.WriteString(capUTF8(text, maxNoteBodyBytes))
		comments.WriteString("\n")
	}
	appendBounded(&body, comments.String(), maxSourceBodyBytes)
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

func sourceAuthor(value author) string {
	if username := strings.TrimSpace(value.Username); username != "" {
		return username
	}
	if name := strings.TrimSpace(value.Name); name != "" {
		return name
	}
	return "unknown"
}

func sortedLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if text := strings.TrimSpace(label); text != "" {
			out = append(out, text)
		}
	}
	sort.Strings(out)
	return out
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

func (s *Source) getURL(ctx context.Context, token string, requestURL string, out any) (http.Header, error) {
	return s.http.GetJSONWithHeaders(ctx, requestURL, sourceHeaders(token), out)
}

func sourceHeaders(token string) http.Header {
	header := make(http.Header)
	header.Set("PRIVATE-TOKEN", token)
	header.Set("Accept", "application/json")
	header.Set("User-Agent", DefaultUserAgent)
	return header
}

// sourceNextPage returns the next page number when the response announces
// one. X-Next-Page is authoritative; a Link rel="next" is used only when the
// header is absent. A malformed or non-monotonic header is an error: the run
// must not loop forever and must not silently claim the stream is complete. A
// full page with no pagination information is also an error rather than a
// silent truncation. A Link to another scheme or host ends the stream so the
// token is never sent elsewhere.
func sourceNextPage(baseURL string, currentPage int, pageFull bool, headers http.Header) (int, bool, error) {
	nextValues := headers.Values("X-Next-Page")
	headerPresent := len(nextValues) > 0
	raw := ""
	if headerPresent {
		raw = strings.TrimSpace(nextValues[0])
	}
	if raw != "" {
		page, err := strconv.Atoi(raw)
		if err != nil || page < 1 {
			return 0, false, fmt.Errorf("invalid X-Next-Page header %q", raw)
		}
		if page <= currentPage {
			return 0, false, fmt.Errorf("X-Next-Page %d does not advance past page %d", page, currentPage)
		}
		return page, true, nil
	}
	target, ok := linkNext(strings.Join(headers.Values("Link"), ","))
	if !ok {
		// An explicitly empty X-Next-Page is GitLab's end marker, even when
		// the page happens to be full. A missing header on a full page is a
		// broken intermediary: fail instead of truncating silently.
		if !headerPresent && pageFull {
			return 0, false, errors.New("a full page announced no next page; pagination headers are missing")
		}
		return 0, false, nil
	}
	same, ok := sameAPIHost(baseURL, target)
	if !ok {
		return 0, false, nil
	}
	parsed, err := url.Parse(same)
	if err != nil {
		return 0, false, fmt.Errorf("invalid Link next page: %w", err)
	}
	page, err := strconv.Atoi(parsed.Query().Get("page"))
	if err != nil || page < 1 {
		return 0, false, fmt.Errorf("Link next page has no valid page parameter")
	}
	if page <= currentPage {
		return 0, false, fmt.Errorf("Link next page %d does not advance past page %d", page, currentPage)
	}
	return page, true, nil
}

var _ ports.Source = (*Source)(nil)
