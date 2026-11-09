package sentry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	checkpointVersion = 1
	// apiPageSize is the per-request page size for the issues list (<=100).
	apiPageSize = 100
	// maxPagesPerRead bounds API round trips in one Read call.
	maxPagesPerRead = 10
	// defaultReadLimit is the item limit when the request sets none.
	defaultReadLimit = 25
	// maxReadLimit caps items returned by one Read call (<=100).
	maxReadLimit = 100
	// maxEventsPerIssue caps the per-issue events fan-out.
	maxEventsPerIssue = 3
	// maxEventFrames caps rendered stack frames per issue body.
	maxEventFrames = 10
	// maxBodyBytes caps the rendered item body.
	maxBodyBytes = 32 << 10
	// maxMetaValueBytes caps one rendered metadata value.
	maxMetaValueBytes = 1024
	// maxEventMessageBytes caps one rendered event message.
	maxEventMessageBytes = 4 << 10
	// maxStackLineBytes caps one rendered stack frame line.
	maxStackLineBytes = 512
	// overlap replays the last five minutes on every pass so items updated
	// while a pass was in flight are not lost to Link cursor movement.
	overlap = 5 * time.Minute
	// maxCursorBytes bounds the opaque provider cursor.
	maxCursorBytes = 1024
	// maxSeenCount bounds the carried item counter.
	maxSeenCount = 1 << 30
)

// Descriptor is the static Sentry source description used by the registry.
func Descriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider: Name, DisplayName: "Sentry",
		Capabilities: domain.Capabilities{
			Backfill: true, Incremental: true,
			AuthModes: []domain.AuthMode{domain.AuthToken},
		},
		DefaultPublish: domain.PublishShared,
		ConfigFields: []domain.ConfigField{
			{Key: "token", Label: "Auth token", Required: true, Secret: true, Help: "Sentry auth token (PAT or internal integration token)."},
			{Key: "org", Label: "Organization", Required: true, Help: "Sentry organization slug."},
			{Key: "projects", Label: "Projects", Help: "Comma separated Sentry project slugs or IDs. Empty means all projects."},
			{Key: "environment", Label: "Environment", Help: "Optional Sentry environment filter (e.g. production)."},
		},
		// Streams is empty: the checkpoint stream is derived per
		// configuration (sentry-<hash of org/projects/environment>), so the
		// host cannot validate it upfront.
	}
}

// Source reads Sentry issues (with bounded newest events) into raw items.
// It only issues authenticated GET requests and never stores credentials.
type Source struct {
	connection  domain.Connection
	baseURL     string
	userAgent   string
	org         string
	projects    []string
	environment string
	stream      string
	credentials ports.CredentialProvider
	http        *connectors.HTTP
	now         func() time.Time
}

// sentryCheckpoint is the resumable cursor for the single stream.
// Watermark is the last completed pass's high lastSeen and advances only
// then; High is the in-progress pass maximum merged into Watermark on
// completion. Cursor/Offset resume inside Link pagination when a Read hits
// its item limit, Seen counts the pass items, Done marks a completed pass.
type sentryCheckpoint struct {
	Watermark *time.Time `json:"watermark,omitempty"`
	High      *time.Time `json:"high,omitempty"`
	Cursor    string     `json:"cursor,omitempty"`
	Offset    int        `json:"offset,omitempty"`
	Seen      int        `json:"seen,omitempty"`
	Done      bool       `json:"done,omitempty"`
}

func (c sentryCheckpoint) validate() error {
	if c.Offset < 0 || c.Offset > apiPageSize {
		return domain.NewValidationError("sentry.checkpoint", "offset is invalid")
	}
	if c.Seen < 0 || c.Seen > maxSeenCount {
		return domain.NewValidationError("sentry.checkpoint", "seen count is invalid")
	}
	if len(c.Cursor) > maxCursorBytes {
		return domain.NewValidationError("sentry.checkpoint", "cursor is too long")
	}
	if c.Cursor != "" && strings.IndexFunc(c.Cursor, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}) >= 0 {
		return domain.NewValidationError("sentry.checkpoint", "cursor is invalid")
	}
	return nil
}

// NewSource constructs the native source using credentials under conn/token.
func NewSource(conn domain.Connection, credentials ports.CredentialProvider, client *http.Client, now func() time.Time) (*Source, error) {
	if err := Descriptor().ValidateConnection(conn); err != nil {
		return nil, err
	}
	if credentials == nil {
		return nil, domain.NewValidationError("sentry.credentials", "is required")
	}
	org := strings.TrimSpace(conn.Settings["org"])
	if org == "" {
		return nil, domain.NewValidationError("sentry.org", "is required")
	}
	if len(org) > 64 || !isSlug(org) {
		return nil, domain.NewValidationError("sentry.org", "must use letters, digits, '-', '_' or '.'")
	}
	projects, err := parseProjects(conn.Settings["projects"])
	if err != nil {
		return nil, err
	}
	environment := strings.TrimSpace(conn.Settings["environment"])
	if len(environment) > 64 || (environment != "" && !isSlug(environment)) {
		return nil, domain.NewValidationError("sentry.environment", "must use letters, digits, '-', '_' or '.'")
	}
	baseURL := strings.TrimSpace(conn.BaseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || !isAllowedBaseURL(parsed) || parsed.User != nil || parsed.Fragment != "" {
		return nil, domain.NewValidationError("sentry.baseUrl", "must be an https URL; http is allowed only for loopback hosts")
	}
	if now == nil {
		now = time.Now
	}
	return &Source{
		connection:  conn,
		baseURL:     strings.TrimRight(parsed.String(), "/"),
		userAgent:   DefaultUserAgent,
		org:         org,
		projects:    projects,
		environment: environment,
		stream:      streamName(org, projects, environment),
		credentials: credentials,
		http:        connectors.NewHTTP(connectors.HTTPOptions{Client: client}),
		now:         now,
	}, nil
}

func isSlug(value string) bool {
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func parseProjects(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	var projects []string
	for _, entry := range strings.Split(trimmed, ",") {
		project := strings.TrimSpace(entry)
		if project == "" {
			continue
		}
		if len(project) > 64 || !isSlug(project) {
			return nil, domain.NewValidationError("sentry.projects", fmt.Sprintf("project %q must use letters, digits, '-', '_' or '.'", project))
		}
		projects = append(projects, project)
	}
	sort.Strings(projects)
	out := projects[:0]
	for i, project := range projects {
		if i == 0 || project != projects[i-1] {
			out = append(out, project)
		}
	}
	return out, nil
}

// streamName derives the single per-configuration stream.
func streamName(org string, projects []string, environment string) string {
	sum := sha256.Sum256([]byte(org + "\x00" + strings.Join(projects, ",") + "\x00" + environment))
	return "sentry-" + hex.EncodeToString(sum[:])[:12]
}

// Descriptor returns the provider's static description.
func (s *Source) Descriptor() domain.SourceDescriptor { return Descriptor() }

// Check verifies configuration and credentials against the organization endpoint.
func (s *Source) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	token, err := s.token(ctx)
	if err != nil {
		return err
	}
	var org sentryOrg
	path := "/api/0/organizations/" + url.PathEscape(s.org) + "/"
	if err := s.get(ctx, token, path, nil, &org); err != nil {
		return fmt.Errorf("sentry check: %s", connectors.SanitizeError(err, token))
	}
	if strings.TrimSpace(org.Slug) == "" && strings.TrimSpace(org.Name) == "" {
		return fmt.Errorf("sentry check: organization %q not found", s.org)
	}
	return nil
}

// Read returns one bounded batch of issues and its resumable cursor.
func (s *Source) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, err
	}
	if req.Connection.ID != s.connection.ID || req.Connection.Provider != Name {
		return ports.ReadBatch{}, domain.NewValidationError("sentry.connection", "does not match source connection")
	}
	token, err := s.token(ctx)
	if err != nil {
		return ports.ReadBatch{}, err
	}
	var ck sentryCheckpoint
	if cp, ok := req.Checkpoints[s.stream]; ok {
		if cp.Stream != s.stream || cp.Version != checkpointVersion {
			return ports.ReadBatch{}, domain.NewValidationError("sentry.checkpoint", "unsupported stream or version")
		}
		if err := json.Unmarshal(cp.Data, &ck); err != nil {
			return ports.ReadBatch{}, domain.NewValidationError("sentry.checkpoint", "is invalid")
		}
		if err := ck.validate(); err != nil {
			return ports.ReadBatch{}, err
		}
	}
	limit := normalizeReadLimit(req.Limit)
	// The durable scan bound is the completed watermark minus the overlap;
	// req.Since only narrows it, never discards the durable position.
	effectiveSince := overlapBound(ck.Watermark)
	if req.Since != nil && (effectiveSince == nil || req.Since.After(*effectiveSince)) {
		effectiveSince = timePtr(req.Since.UTC())
	}
	query := buildIssuesQuery(s.environment, effectiveSince)
	items := make([]domain.Item, 0, limit)
	high := ck.High
	cursor := ck.Cursor
	offset := ck.Offset
	seen := ck.Seen
	pages := 0
	done := false
	for len(items) < limit && pages < maxPagesPerRead {
		if err := ctx.Err(); err != nil {
			return ports.ReadBatch{}, err
		}
		page, next, hasMore, err := s.fetchIssuesPage(ctx, token, cursor, query)
		if err != nil {
			return ports.ReadBatch{}, fmt.Errorf("sentry read: %s", connectors.SanitizeError(err, token))
		}
		pages++
		if offset > len(page) {
			return ports.ReadBatch{}, domain.NewValidationError("sentry.checkpoint", "offset exceeds provider page")
		}
		for offset < len(page) && len(items) < limit {
			issue := page[offset]
			offset++
			if effectiveSince != nil && !issue.LastSeen.IsZero() && issue.LastSeen.UTC().Before(*effectiveSince) {
				continue
			}
			events, err := s.listEvents(ctx, token, issue.ID)
			if err != nil {
				if ctx.Err() != nil {
					return ports.ReadBatch{}, ctx.Err()
				}
				// Events are enrichment: fall back to metadata/culprit.
				events = nil
			}
			item := s.toItem(req.Connection.ID, issue, events)
			items = append(items, item)
			if !item.UpdatedAt.IsZero() && (high == nil || item.UpdatedAt.After(*high)) {
				high = timePtr(item.UpdatedAt.UTC())
			}
		}
		if offset < len(page) {
			break // item limit hit mid-page; cursor/offset resume here.
		}
		if hasMore {
			if next == cursor {
				return ports.ReadBatch{}, domain.NewValidationError("sentry.checkpoint", "provider cursor did not advance")
			}
			cursor = next
			offset = 0
			continue
		}
		cursor = ""
		offset = 0
		done = true
		break
	}
	more := !done
	var nextCk sentryCheckpoint
	if done {
		watermark := ck.Watermark
		if high != nil && (watermark == nil || high.After(*watermark)) {
			watermark = high
		}
		nextCk = sentryCheckpoint{Watermark: watermark, Done: true}
	} else {
		nextCk = sentryCheckpoint{Watermark: ck.Watermark, High: high, Cursor: cursor, Offset: offset, Seen: seen + len(items)}
	}
	data, err := json.Marshal(nextCk)
	if err != nil {
		return ports.ReadBatch{}, fmt.Errorf("encode sentry checkpoint: %w", err)
	}
	return ports.ReadBatch{
		Items:       items,
		Checkpoints: []domain.Checkpoint{{Stream: s.stream, Version: checkpointVersion, Data: data}},
		More:        more,
	}, nil
}

// fetchIssuesPage lists one page of issues sorted ascending by lastSeen.
// The server-side query is a best-effort hint (Sentry statsPeriod only
// supports 24h/14d); the local lastSeen bound in Read is authoritative.
func (s *Source) fetchIssuesPage(ctx context.Context, token, cursor, query string) ([]sentryIssue, string, bool, error) {
	path := "/api/0/organizations/" + url.PathEscape(s.org) + "/issues/"
	params := url.Values{}
	params.Set("sort", "last_seen")
	params.Set("limit", strconv.Itoa(apiPageSize))
	if query != "" {
		params.Set("query", query)
	}
	for _, project := range s.projects {
		params.Add("project", project)
	}
	if cursor != "" {
		params.Set("cursor", cursor)
	}
	requestURL := s.baseURL + path + "?" + params.Encode()
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", s.userAgent)
	var issues []sentryIssue
	respHeaders, err := s.http.GetJSONWithHeaders(ctx, requestURL, headers, &issues)
	if err != nil {
		return nil, "", false, err
	}
	sort.SliceStable(issues, func(i, j int) bool {
		if !issues[i].LastSeen.Equal(issues[j].LastSeen) {
			return issues[i].LastSeen.Before(issues[j].LastSeen)
		}
		return issues[i].ID < issues[j].ID
	})
	next, hasMore, err := parseLinkNext(respHeaders, cursor)
	if err != nil {
		return nil, "", false, err
	}
	return issues, next, hasMore, nil
}

// listEvents returns at most maxEventsPerIssue events in endpoint order,
// which the API serves newest-first.
func (s *Source) listEvents(ctx context.Context, token, issueID string) ([]sentryEvent, error) {
	if strings.TrimSpace(issueID) == "" {
		return nil, nil
	}
	var events []sentryEvent
	path := "/api/0/issues/" + url.PathEscape(issueID) + "/events/"
	if err := s.get(ctx, token, path, nil, &events); err != nil {
		return nil, err
	}
	if len(events) > maxEventsPerIssue {
		events = events[:maxEventsPerIssue]
	}
	return events, nil
}

// parseLinkNext extracts the rel="next" cursor from Sentry Link headers.
// Absent Link (or results="false") means the pass is complete. A next cursor
// that is empty, overlong, non-opaque, or equal to the current cursor fails
// monotonic validation so a looping provider cannot spin the reader.
func parseLinkNext(header http.Header, current string) (string, bool, error) {
	values := header.Values("Link")
	if len(values) == 0 {
		return "", false, nil
	}
	raw := strings.Join(values, ",")
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "<") {
			continue
		}
		end := strings.Index(part, ">")
		if end < 0 {
			continue
		}
		rawURL := part[1:end]
		rel, results, cursorAttr := "", "", ""
		for _, param := range strings.Split(part[end+1:], ";") {
			key, value, _ := strings.Cut(param, "=")
			key = strings.ToLower(strings.TrimSpace(key))
			value = strings.Trim(strings.TrimSpace(value), `"`)
			switch key {
			case "rel":
				rel = strings.ToLower(value)
			case "results":
				results = strings.ToLower(value)
			case "cursor":
				cursorAttr = value
			}
		}
		if rel != "next" {
			continue
		}
		if results == "false" {
			return "", false, nil
		}
		next := cursorAttr
		if next == "" {
			parsed, err := url.Parse(rawURL)
			if err != nil {
				return "", false, domain.NewValidationError("sentry.checkpoint", "provider link is invalid")
			}
			next = parsed.Query().Get("cursor")
		}
		if next == "" || len(next) > maxCursorBytes || strings.IndexFunc(next, func(r rune) bool {
			return unicode.IsControl(r) || unicode.IsSpace(r)
		}) >= 0 {
			return "", false, domain.NewValidationError("sentry.checkpoint", "provider cursor is invalid")
		}
		if next == current {
			return "", false, domain.NewValidationError("sentry.checkpoint", "provider cursor did not advance")
		}
		return next, true, nil
	}
	return "", false, nil
}

// buildIssuesQuery renders the best-effort server-side search: environment
// scoping plus a lastSeen floor. Local filtering in Read stays authoritative.
func buildIssuesQuery(environment string, since *time.Time) string {
	var terms []string
	if environment != "" {
		terms = append(terms, "environment:"+environment)
	}
	if since != nil && !since.IsZero() {
		terms = append(terms, "lastSeen:>"+since.UTC().Format(time.RFC3339))
	}
	return strings.Join(terms, " ")
}

func overlapBound(watermark *time.Time) *time.Time {
	if watermark == nil {
		return nil
	}
	return timePtr(watermark.UTC().Add(-overlap))
}

func normalizeReadLimit(limit int) int {
	if limit <= 0 {
		return defaultReadLimit
	}
	if limit > maxReadLimit {
		return maxReadLimit
	}
	return limit
}

func (s *Source) toItem(connectionID string, issue sentryIssue, events []sentryEvent) domain.Item {
	project := strings.TrimSpace(issue.Project.Slug)
	if project == "" {
		project = strings.TrimSpace(issue.Project.ID)
	}
	if project == "" {
		project = "unknown"
	}
	id := strings.TrimSpace(issue.ID)
	if id == "" {
		id = strings.TrimSpace(issue.ShortID)
	}
	if id == "" {
		id = "unknown"
	}
	item := domain.Item{Ref: domain.ItemRef{ConnectionID: connectionID, ExternalID: s.org + "/" + project + "/" + id}}
	item.Kind = domain.ItemIssue
	title := strings.TrimSpace(issue.Title)
	if title == "" {
		title = "Sentry issue " + id
	}
	item.Title = title
	item.URL = strings.TrimSpace(issue.Permalink)
	item.Author = strings.TrimSpace(issue.Culprit)
	item.Labels = issueLabels(issue, project)
	if !issue.FirstSeen.IsZero() {
		item.CreatedAt = issue.FirstSeen.UTC()
	}
	if !issue.LastSeen.IsZero() {
		item.UpdatedAt = issue.LastSeen.UTC()
	}
	item.Body = buildBody(issue, project, events)
	if len(s.connection.Projects) == 1 {
		item.ProjectID = s.connection.Projects[0]
	}
	return item
}

func issueLabels(issue sentryIssue, project string) []string {
	var labels []string
	for _, value := range []string{issue.Level, project, issue.Logger} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			labels = append(labels, trimmed)
		}
	}
	sort.Strings(labels)
	out := labels[:0]
	for i, label := range labels {
		if i == 0 || label != labels[i-1] {
			out = append(out, label)
		}
	}
	return out
}

func buildBody(issue sentryIssue, project string, events []sentryEvent) string {
	var out strings.Builder
	fmt.Fprintf(&out, "**Project:** %s\n", project)
	if level := strings.TrimSpace(issue.Level); level != "" {
		fmt.Fprintf(&out, "**Level:** %s\n", level)
	}
	if culprit := strings.TrimSpace(issue.Culprit); culprit != "" {
		fmt.Fprintf(&out, "**Culprit:** %s\n", culprit)
	}
	if shortID := strings.TrimSpace(issue.ShortID); shortID != "" {
		fmt.Fprintf(&out, "**Short ID:** %s\n", shortID)
	}
	if !issue.FirstSeen.IsZero() {
		fmt.Fprintf(&out, "**First seen:** %s\n", issue.FirstSeen.UTC().Format(time.RFC3339))
	}
	if !issue.LastSeen.IsZero() {
		fmt.Fprintf(&out, "**Last seen:** %s\n", issue.LastSeen.UTC().Format(time.RFC3339))
	}
	if len(issue.Metadata) > 0 {
		out.WriteString("\n### Metadata\n")
		keys := make([]string, 0, len(issue.Metadata))
		for key := range issue.Metadata {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := strings.TrimSpace(fmt.Sprintf("%v", issue.Metadata[key]))
			if value == "" || value == "<nil>" {
				continue
			}
			fmt.Fprintf(&out, "- %s: %s\n", strings.TrimSpace(key), capUTF8(value, maxMetaValueBytes))
		}
	}
	if len(events) > 0 {
		fmt.Fprintf(&out, "\n### Latest events (up to %d)\n", maxEventsPerIssue)
		for _, event := range events {
			header := strings.TrimSpace(event.ID)
			if !event.DateCreated.IsZero() {
				if header != "" {
					header += " · "
				}
				header += event.DateCreated.UTC().Format(time.RFC3339)
			}
			if header == "" {
				header = "event"
			}
			fmt.Fprintf(&out, "\n#### %s\n", header)
			if message := eventMessage(event); message != "" {
				out.WriteString(capUTF8(message, maxEventMessageBytes) + "\n")
			}
			for _, line := range stackLines(event) {
				out.WriteString(line + "\n")
			}
		}
	} else {
		out.WriteString("\nNo recent events captured; metadata above is authoritative.\n")
	}
	return truncateUTF8(out.String(), maxBodyBytes)
}

// eventMessage prefers the event message, then title, then the first
// exception type/value pair.
func eventMessage(event sentryEvent) string {
	if message := strings.TrimSpace(event.Message); message != "" {
		return message
	}
	if title := strings.TrimSpace(event.Title); title != "" {
		return title
	}
	for _, entry := range event.Entries {
		for _, value := range entry.Data.Values {
			text := strings.TrimSpace(value.Value)
			if exception := strings.TrimSpace(value.Type); exception != "" {
				if text != "" {
					return exception + ": " + text
				}
				return exception
			}
			if text != "" {
				return text
			}
		}
	}
	return strings.TrimSpace(event.Culprit)
}

// stackLines renders at most maxEventFrames frame lines, keeping the tail of
// the collected frames (nearest the crash in oldest-first order).
func stackLines(event sentryEvent) []string {
	var frames []sentryFrame
	for _, entry := range event.Entries {
		for _, value := range entry.Data.Values {
			if value.Stacktrace == nil {
				continue
			}
			frames = append(frames, value.Stacktrace.Frames...)
		}
	}
	if len(frames) > maxEventFrames {
		frames = frames[len(frames)-maxEventFrames:]
	}
	lines := make([]string, 0, len(frames))
	for _, frame := range frames {
		function := strings.TrimSpace(frame.Function)
		if function == "" {
			function = strings.TrimSpace(frame.Module)
		}
		if function == "" {
			function = "unknown"
		}
		location := strings.TrimSpace(frame.Filename)
		if frame.Lineno > 0 {
			if location == "" {
				location = strconv.Itoa(frame.Lineno)
			} else {
				location += ":" + strconv.Itoa(frame.Lineno)
			}
		}
		line := "at " + function
		if location != "" {
			line += " (" + location + ")"
		}
		lines = append(lines, capUTF8(line, maxStackLineBytes))
	}
	return lines
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

func truncateUTF8(value string, limit int) string {
	return capUTF8(value, limit)
}

func timePtr(t time.Time) *time.Time { t = t.UTC(); return &t }

func (s *Source) token(ctx context.Context) (string, error) {
	token, err := s.credentials.Get(ctx, s.connection.ID, "token")
	if err != nil {
		return "", fmt.Errorf("read sentry credential: %s", connectors.SanitizeError(err))
	}
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("read sentry credential: token is empty")
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
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", s.userAgent)
	_, err := s.http.GetJSONWithHeaders(ctx, requestURL, headers, out)
	return err
}

var _ ports.Source = (*Source)(nil)
