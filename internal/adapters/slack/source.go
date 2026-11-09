package slack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	checkpointVersion  = 1
	maxSlackPage       = 15
	defaultLimit       = 15
	maxConfiguredLimit = 30
	maxChannels        = 100
	maxHistoryPages    = 2
	maxReplies         = 5
	maxReplyBytes      = 16 << 10
	maxBodyBytes       = 256 << 10
	maxResponseBytes   = 8 << 20
	maxSeen            = 512
	pageOverlap        = 5 * time.Minute
	doneCursor         = "!slack-done"
)

// Descriptor is the static Slack source description. Channel IDs are an
// explicit operator opt-in: DMs and private channels are never discovered.
func Descriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider: Name, DisplayName: "Slack",
		Capabilities: domain.Capabilities{
			Backfill: true, Incremental: true, AuthModes: []domain.AuthMode{domain.AuthToken},
		},
		DefaultPublish: domain.PublishLocalOnly,
		ConfigFields: []domain.ConfigField{
			{Key: "token", Label: "Bot token", Required: true, Secret: true, Help: "Slack bot xoxb- token stored in the credential store."},
			{Key: "channels", Label: "Channels", Required: true, Help: "Comma or newline separated channel IDs. Listing any channel is explicit operator opt-in; DMs and private channels are never discovered automatically."},
			{Key: "threads", Label: "Include thread replies", Help: "Defaults to true; when enabled, include up to five replies per thread root."},
			{Key: "limit", Label: "Read limit", Help: "Optional item limit per Read (1–30); each Slack history request asks for at most 15 messages."},
		},
		Backends:     []domain.Backend{domain.BackendNative},
		PersonalData: true,
	}
}

// Source reads messages only from the explicitly configured channel IDs.
// Slack URLs use the generic app.slack.com host because no workspace host is
// configured. User tokens can be used with Slack's search.messages API, but
// this source intentionally requires a bot xoxb- token and does not use search.
type Source struct {
	connection  domain.Connection
	baseURL     string
	channels    []string
	threads     bool
	itemLimit   int
	credentials ports.CredentialProvider
	client      *http.Client
	now         func() time.Time
}

// channelCursor is the persisted per-channel scan position. nextCursor is
// Slack's opaque page cursor; offset makes a partially emitted response page
// replayable. A private sentinel in nextCursor marks a channel complete in the
// current pass, preventing small host batches from starving later channels.
type channelCursor struct {
	Oldest     string    `json:"oldest"`
	Latest     string    `json:"latest"`
	LastScan   time.Time `json:"lastScan"`
	NextCursor string    `json:"nextCursor"`
	Offset     int       `json:"offset"`
	Seen       []string  `json:"seen"`
}

type slackResponse struct {
	OK         bool             `json:"ok"`
	Error      string           `json:"error"`
	RetryAfter int              `json:"retry_after"`
	HasMore    bool             `json:"has_more"`
	Messages   []slackMessage   `json:"messages"`
	Metadata   responseMetadata `json:"response_metadata"`
}

type responseMetadata struct {
	NextCursor string `json:"next_cursor"`
}

type slackMessage struct {
	Type        string         `json:"type"`
	Text        string         `json:"text"`
	TS          string         `json:"ts"`
	User        string         `json:"user"`
	Username    string         `json:"username"`
	UserProfile profile        `json:"user_profile"`
	ReplyCount  int            `json:"reply_count"`
	ThreadTS    string         `json:"thread_ts"`
	Files       []slackFile    `json:"files,omitempty"`
	Messages    []slackMessage `json:"messages,omitempty"`
}

type slackFile struct {
	Name  string `json:"name"`
	Title string `json:"title"`
}

type profile struct {
	DisplayName string `json:"display_name"`
	RealName    string `json:"real_name"`
}

// NewSource builds a Slack source using conn/token from the credential store.
// BaseURL defaults to https://slack.com/api; plain HTTP is permitted only for
// loopback test servers. The token is sent only in the Authorization header.
func NewSource(conn domain.Connection, credentials ports.CredentialProvider, client *http.Client, now func() time.Time) (*Source, error) {
	if err := Descriptor().ValidateConnection(conn); err != nil {
		return nil, err
	}
	if credentials == nil {
		return nil, domain.NewValidationError("slack.credentials", "is required")
	}
	channels, err := parseChannels(conn.Settings["channels"])
	if err != nil {
		return nil, err
	}
	threads := true
	if value, ok := conn.Settings["threads"]; ok {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true":
			threads = true
		case "false":
			threads = false
		default:
			return nil, domain.NewValidationError("slack.threads", "must be true or false")
		}
	}
	itemLimit := defaultLimit
	if value := strings.TrimSpace(conn.Settings["limit"]); value != "" {
		itemLimit, err = strconv.Atoi(value)
		if err != nil || itemLimit < 1 || itemLimit > maxConfiguredLimit {
			return nil, domain.NewValidationError("slack.limit", "must be an integer from 1 to 30")
		}
	}
	baseURL, err := normalizeBaseURL(conn.BaseURL)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// Do not forward the bearer token across redirects.
	cloned := *client
	cloned.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	if now == nil {
		now = time.Now
	}
	return &Source{
		connection: conn, baseURL: baseURL, channels: channels, threads: threads,
		itemLimit: itemLimit, credentials: credentials, client: &cloned, now: now,
	}, nil
}

func normalizeBaseURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return DefaultBaseURL, nil
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return "", domain.NewValidationError("slack.baseUrl", "must be an https URL; http is allowed only for loopback hosts")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return "", domain.NewValidationError("slack.baseUrl", "must be an https URL; http is allowed only for loopback hosts")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func parseChannels(raw string) ([]string, error) {
	seen := make(map[string]bool)
	var channels []string
	for _, value := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		channel := strings.TrimSpace(value)
		if channel == "" || len(channel) > 80 || strings.ContainsAny(channel, " \t") {
			return nil, domain.NewValidationError("slack.channels", "must contain valid channel IDs separated by commas or newlines")
		}
		for _, r := range channel {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
				return nil, domain.NewValidationError("slack.channels", "must contain valid channel IDs separated by commas or newlines")
			}
		}
		if !seen[channel] {
			seen[channel] = true
			channels = append(channels, channel)
		}
	}
	if len(channels) == 0 {
		return nil, domain.NewValidationError("slack.channels", "must list at least one explicitly opted-in channel ID")
	}
	if len(channels) > maxChannels {
		return nil, domain.NewValidationError("slack.channels", "must list no more than 100 channels")
	}
	return channels, nil
}

// Descriptor returns the provider description.
func (s *Source) Descriptor() domain.SourceDescriptor { return Descriptor() }

// Check verifies the token with auth.test and requires workspace and user identity.
func (s *Source) Check(ctx context.Context) error {
	token, err := s.token(ctx)
	if err != nil {
		return err
	}
	var result struct {
		OK   bool   `json:"ok"`
		Team string `json:"team"`
		User string `json:"user"`
	}
	if err := s.call(ctx, token, "auth.test", nil, &result); err != nil {
		return err
	}
	if !result.OK || strings.TrimSpace(result.Team) == "" || strings.TrimSpace(result.User) == "" {
		return fmt.Errorf("slack check: auth.test did not return a valid team and user")
	}
	return nil
}

// Read returns a bounded batch from the opted-in channels and per-channel checkpoints.
func (s *Source) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, err
	}
	if req.Connection.ID != s.connection.ID || req.Connection.Provider != Name {
		return ports.ReadBatch{}, domain.NewValidationError("slack.connection", "does not match source connection")
	}
	token, err := s.token(ctx)
	if err != nil {
		return ports.ReadBatch{}, err
	}
	cursors, err := s.readCursors(req)
	if err != nil {
		return ports.ReadBatch{}, err
	}
	limit := s.itemLimit
	if req.Limit > 0 && req.Limit < limit {
		limit = req.Limit
	}
	items := make([]domain.Item, 0, limit)
	more := false
	for channelIndex, channel := range s.channels {
		if err := ctx.Err(); err != nil {
			return ports.ReadBatch{}, err
		}
		cursor := &cursors[channelIndex]
		if cursor.NextCursor == doneCursor {
			continue
		}
		calls := 0
		for calls < maxHistoryCalls(limit) && len(items) < limit {
			if err := ctx.Err(); err != nil {
				return ports.ReadBatch{}, err
			}
			requestCursor := cursor.NextCursor
			if requestCursor == doneCursor {
				break
			}
			latestBound := cursor.LastScan
			if latestBound.IsZero() {
				latestBound = s.now().UTC()
			}
			query := url.Values{
				"channel": {channel}, "oldest": {cursor.Oldest}, "latest": {slackTS(latestBound)},
				"inclusive": {"true"}, "limit": {strconv.Itoa(s.historyPageLimit())},
			}
			if requestCursor != "" {
				query.Set("cursor", requestCursor)
			}
			var page slackResponse
			if err := s.call(ctx, token, "conversations.history", query, &page); err != nil {
				return ports.ReadBatch{}, err
			}
			calls++
			if cursor.Offset < 0 || cursor.Offset > len(page.Messages) {
				return ports.ReadBatch{}, domain.NewValidationError("slack.checkpoint", "offset exceeds the Slack page")
			}
			pageMore := page.HasMore || page.Metadata.NextCursor != ""
			if pageMore && page.Metadata.NextCursor == "" {
				return ports.ReadBatch{}, fmt.Errorf("slack history: pagination response omitted its next cursor")
			}
			if pageMore && page.Metadata.NextCursor == requestCursor {
				return ports.ReadBatch{}, fmt.Errorf("slack history: pagination cursor did not advance")
			}
			for cursor.Offset < len(page.Messages) && len(items) < limit {
				message := page.Messages[cursor.Offset]
				cursor.Offset++
				if s.threads && message.ThreadTS != "" && message.ThreadTS != message.TS {
					continue
				}
				if !validMessage(message, cursor.Oldest, slackTS(latestBound)) {
					continue
				}
				id := channel + "/" + message.TS
				if contains(cursor.Seen, id) {
					continue
				}
				item, err := s.toItem(ctx, token, req.Connection.ID, channel, message)
				if err != nil {
					return ports.ReadBatch{}, err
				}
				items = append(items, item)
				if ts := parseSlackTS(message.TS); ts.After(parseSlackTS(cursor.Latest)) {
					cursor.Latest = message.TS
				}
				remember(cursor, id)
			}
			if cursor.Offset == len(page.Messages) {
				cursor.Offset = 0
				if pageMore {
					cursor.NextCursor = page.Metadata.NextCursor
				} else {
					cursor.NextCursor = doneCursor
					cursor.LastScan = s.now().UTC()
				}
			}
			if cursor.NextCursor == doneCursor {
				break
			}
		}
		if cursor.NextCursor != doneCursor {
			more = true
		}
		if len(items) >= limit && channelIndex < len(s.channels)-1 {
			more = true
		}
	}
	allDone := true
	for i := range cursors {
		if cursors[i].NextCursor != doneCursor {
			allDone = false
			more = true
			break
		}
	}
	if allDone {
		more = false
		for i := range cursors {
			watermark := parseSlackTS(cursors[i].Latest)
			if watermark.IsZero() {
				watermark = cursors[i].LastScan
			}
			if req.Since != nil && req.Since.After(watermark) {
				watermark = req.Since.UTC()
			}
			oldest := watermark.Add(-pageOverlap)
			if req.Since != nil {
				bound := req.Since.UTC().Add(-pageOverlap)
				if bound.After(oldest) {
					oldest = bound
				}
			}
			cursors[i] = channelCursor{Oldest: slackTS(oldest), Latest: slackTS(watermark), LastScan: s.now().UTC(), Seen: []string{}}
		}
	}
	checkpoints := make([]domain.Checkpoint, 0, len(s.channels))
	for i, channel := range s.channels {
		data, err := json.Marshal(cursors[i])
		if err != nil {
			return ports.ReadBatch{}, fmt.Errorf("slack checkpoint encode failed")
		}
		checkpoints = append(checkpoints, domain.Checkpoint{Stream: streamName(channel), Version: checkpointVersion, Data: data})
	}
	return ports.ReadBatch{Items: items, Checkpoints: checkpoints, More: more}, nil
}

func maxHistoryCalls(limit int) int {
	calls := (limit + maxSlackPage - 1) / maxSlackPage
	if calls < 1 {
		return 1
	}
	if calls > maxHistoryPages {
		return maxHistoryPages
	}
	return calls
}

func (s *Source) historyPageLimit() int {
	if s.itemLimit < maxSlackPage {
		return s.itemLimit
	}
	return maxSlackPage
}

func (s *Source) readCursors(req ports.ReadRequest) ([]channelCursor, error) {
	byStream := make(map[string]channelCursor, len(s.channels))
	for _, channel := range s.channels {
		cursor := channelCursor{Oldest: slackTS(s.sinceBound(req.Since)), LastScan: s.now().UTC(), Seen: []string{}}
		if req.Since != nil {
			cursor.Latest = slackTS(req.Since.UTC())
		}
		stream := streamName(channel)
		if checkpoint, ok := req.Checkpoints[stream]; ok {
			if err := checkpoint.Validate(); err != nil {
				return nil, domain.NewValidationError("slack.checkpoint", "is invalid")
			}
			if checkpoint.Stream != stream || checkpoint.Version != checkpointVersion {
				return nil, domain.NewValidationError("slack.checkpoint", "unsupported stream or version")
			}
			decoder := json.NewDecoder(strings.NewReader(string(checkpoint.Data)))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&cursor); err != nil {
				return nil, domain.NewValidationError("slack.checkpoint", "is invalid")
			}
			if decoder.Decode(new(any)) != io.EOF || !validCursor(cursor) || cursor.Offset > s.historyPageLimit() {
				return nil, domain.NewValidationError("slack.checkpoint", "position is invalid")
			}
			if req.Since != nil {
				bound := slackTS(s.sinceBound(req.Since))
				if parseSlackTS(bound).After(parseSlackTS(cursor.Oldest)) {
					cursor = channelCursor{Oldest: bound, Latest: slackTS(req.Since.UTC()), LastScan: s.now().UTC(), Seen: []string{}}
				}
			}
		}
		byStream[stream] = cursor
	}
	for key, checkpoint := range req.Checkpoints {
		if strings.HasPrefix(strings.ToLower(key), "slack-") || strings.HasPrefix(strings.ToLower(checkpoint.Stream), "slack-") {
			if key != checkpoint.Stream {
				return nil, domain.NewValidationError("slack.checkpoint", "stream key does not match checkpoint")
			}
			if _, ok := byStream[key]; !ok {
				return nil, domain.NewValidationError("slack.checkpoint", "does not match a configured channel")
			}
		}
	}
	out := make([]channelCursor, len(s.channels))
	for i, channel := range s.channels {
		out[i] = byStream[streamName(channel)]
	}
	return out, nil
}

func validCursor(cursor channelCursor) bool {
	if cursor.Oldest == "" || !validSlackTS(cursor.Oldest) || !validSlackTS(cursor.Latest) || cursor.LastScan.IsZero() || cursor.Offset < 0 || cursor.Offset > maxSlackPage || len(cursor.Seen) > maxSeen {
		return false
	}
	if cursor.NextCursor == doneCursor {
		return cursor.Offset == 0
	}
	if len(cursor.NextCursor) > 4096 || strings.ContainsAny(cursor.NextCursor, "\r\n\x00") {
		return false
	}
	for _, id := range cursor.Seen {
		if id == "" || len(id) > 160 || strings.ContainsAny(id, "\r\n\x00") {
			return false
		}
	}
	return true
}

func (s *Source) sinceBound(since *time.Time) time.Time {
	if since == nil {
		return time.Time{}
	}
	return since.UTC().Add(-pageOverlap)
}

func (s *Source) toItem(ctx context.Context, token, connectionID, channel string, root slackMessage) (domain.Item, error) {
	body := root.Text
	if s.threads && root.ReplyCount > 0 {
		query := url.Values{
			"channel": {channel}, "ts": {root.TS}, "limit": {strconv.Itoa(maxReplies)},
		}
		var response slackResponse
		if err := s.call(ctx, token, "conversations.replies", query, &response); err != nil {
			return domain.Item{}, err
		}
		replies := response.Messages
		if len(replies) > maxReplies {
			replies = replies[:maxReplies]
		}
		for _, reply := range replies {
			if reply.TS == root.TS || reply.ThreadTS != "" && reply.ThreadTS != root.TS {
				continue
			}
			entry := "\n\n---\n\n" + displayAuthor(reply) + "\n\n" + capUTF8(reply.Text, maxReplyBytes)
			body = appendBody(body, entry, maxBodyBytes)
			if len(body) >= maxBodyBytes {
				break
			}
		}
	}
	body = capUTF8(body, maxBodyBytes)
	title := firstLine(root.Text)
	if title == "" && len(root.Files) > 0 {
		title = strings.TrimSpace(root.Files[0].Title)
		if title == "" {
			title = strings.TrimSpace(root.Files[0].Name)
		}
	}
	if title == "" {
		title = "Slack message"
	}
	title = capUTF8(title, 16<<10)
	created := parseSlackTS(root.TS)
	item := domain.Item{
		Ref:  domain.ItemRef{ConnectionID: connectionID, ExternalID: channel + "/" + root.TS},
		Kind: domain.ItemMessage, Title: title, Body: body,
		URL: slackMessageURL(channel, root.TS), Author: displayAuthor(root),
		CreatedAt: created, UpdatedAt: created,
	}
	return item, nil
}

func (s *Source) token(ctx context.Context) (string, error) {
	token, err := s.credentials.Get(ctx, s.connection.ID, "token")
	if err != nil || strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("slack credential is unavailable")
	}
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "xoxb-") || strings.ContainsAny(token, "\r\n") {
		return "", domain.NewValidationError("slack.token", "must be a Slack bot token beginning xoxb-")
	}
	return token, nil
}

func (s *Source) call(ctx context.Context, token, method string, query url.Values, out any) error {
	requestURL := s.baseURL + "/" + method
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return fmt.Errorf("slack %s request could not be created", method)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("slack %s request failed", method)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After"))
		if retryAfter != "" {
			return fmt.Errorf("slack %s rate limited (retry-after %s)", method, safeRetryAfter(retryAfter))
		}
		return fmt.Errorf("slack %s rate limited (HTTP 429)", method)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack %s request failed (HTTP %d)", method, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return fmt.Errorf("slack %s response could not be read", method)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("slack %s response was invalid", method)
	}
	var envelope struct {
		OK         bool   `json:"ok"`
		Error      string `json:"error"`
		RetryAfter int    `json:"retry_after"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || !envelope.OK {
		if envelope.RetryAfter > 0 {
			return fmt.Errorf("slack %s rate limited (retry-after %d)", method, envelope.RetryAfter)
		}
		return fmt.Errorf("slack %s API returned an error", method)
	}
	return nil
}

func safeRetryAfter(value string) string {
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 && seconds <= 86400 {
		return strconv.Itoa(seconds) + "s"
	}
	if when, err := http.ParseTime(value); err == nil {
		return when.UTC().Format(time.RFC3339)
	}
	return "provided"
}

func streamName(channel string) string {
	sum := sha256.Sum256([]byte(channel))
	return "slack-" + hex.EncodeToString(sum[:8])
}

func validMessage(message slackMessage, oldest, latest string) bool {
	if message.TS == "" || !validSlackTS(message.TS) || message.Type != "" && message.Type != "message" {
		return false
	}
	ts := parseSlackTS(message.TS)
	return !ts.Before(parseSlackTS(oldest)) && !ts.After(parseSlackTS(latest))
}

func validSlackTS(value string) bool { return value == "" || !parseSlackTS(value).IsZero() }

func parseSlackTS(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	seconds, fraction, hasFraction := strings.Cut(value, ".")
	sec, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil || sec < 0 {
		return time.Time{}
	}
	nanos := int64(0)
	if hasFraction {
		if fraction == "" || len(fraction) > 9 {
			return time.Time{}
		}
		for _, r := range fraction {
			if r < '0' || r > '9' {
				return time.Time{}
			}
		}
		fraction += strings.Repeat("0", 9-len(fraction))
		nanos, err = strconv.ParseInt(fraction, 10, 64)
		if err != nil {
			return time.Time{}
		}
	}
	return time.Unix(sec, nanos).UTC()
}

func slackTS(at time.Time) string {
	if at.IsZero() {
		return "0.000000"
	}
	return fmt.Sprintf("%d.%06d", at.UTC().Unix(), at.UTC().Nanosecond()/1000)
}

func displayAuthor(message slackMessage) string {
	if value := strings.TrimSpace(message.UserProfile.DisplayName); value != "" {
		return value
	}
	if value := strings.TrimSpace(message.UserProfile.RealName); value != "" {
		return value
	}
	if value := strings.TrimSpace(message.Username); value != "" {
		return value
	}
	if value := strings.TrimSpace(message.User); value != "" {
		return value
	}
	return "Unknown Slack user"
}

func firstLine(value string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(value), "\n")
	return strings.TrimSpace(line)
}

func slackMessageURL(channel, ts string) string {
	return "https://app.slack.com/archives/" + url.PathEscape(channel) + "/p" + strings.ReplaceAll(ts, ".", "")
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

func appendBody(body, addition string, limit int) string {
	remaining := limit - len(body)
	if remaining <= 0 {
		return body
	}
	return body + capUTF8(addition, remaining)
}

func remember(cursor *channelCursor, id string) {
	cursor.Seen = append(cursor.Seen, id)
	if len(cursor.Seen) > maxSeen {
		cursor.Seen = append([]string(nil), cursor.Seen[len(cursor.Seen)-maxSeen:]...)
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

var _ ports.Source = (*Source)(nil)
