// Package mcpsource implements a read-only MCP-backed ingestion source.
// Only tools explicitly enabled by configuration (or the documented
// conservative read-tool heuristic) are called. MCP servers can expose
// arbitrary personal data and this source intentionally marks it as such.
package mcpsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	Name                 = "mcp"
	streamVersion        = 1
	defaultMaxItems      = 100
	maximumMaxItems      = 500
	defaultTimeout       = 30 * time.Second
	maximumTimeout       = 5 * time.Minute
	maximumTools         = 16
	maximumListedTools   = 100
	maximumListPages     = 5
	maximumResponseBytes = 512 << 10
	maximumReadItems     = 500
	pageOverlap          = 5 * time.Minute
)

// Descriptor describes the native source. By default, only names containing
// search, list, recent, query, or find are enabled, and names containing any
// deny word are never called. Explicitly configured tools are still subject
// to the unconditional deny list. MCP tools rarely expose complete history;
// each item is marked as a recent-results-only observation.
func Descriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider: Name, DisplayName: "MCP server",
		Capabilities: domain.Capabilities{
			Backfill: true, Incremental: true,
			AuthModes: []domain.AuthMode{domain.AuthNone, domain.AuthToken},
		},
		DefaultPublish: domain.PublishLocalOnly,
		PersonalData:   true,
		ConfigFields: []domain.ConfigField{
			{Key: "transport", Label: "Transport", Required: true, Help: "stdio or http."},
			{Key: "command", Label: "Command", Secret: true, Help: "Required for stdio. Stored in the local credential store, never in the synced catalogue; executable path or PATH name, invoked without a shell."},
			{Key: "args", Label: "Arguments", Secret: true, Help: "Optional for stdio, stored in the local credential store. Newline-separated argv; each line is one literal argument, with no shell interpolation."},
			{Key: "url", Label: "Streamable HTTP URL", Help: "Required for http. HTTPS, or HTTP to loopback only; URL credentials, query and fragment are forbidden."},
			{Key: "token", Label: "Bearer token", Secret: true, Help: "Optional HTTP Bearer token. Never sent to stdio servers."},
			{Key: "tools", Label: "Read tools", Help: "Optional exact comma/newline-separated names (letters, digits, '_', '-', '.'). Without this, names matching search/list/recent/query/find are selected, except names containing destructive verbs (create/update/delete/remove/write/send/execute/run/call/set/add/insert/upsert/upload/post/put/patch/archive/purge/destroy/drop/truncate/approve/merge/assign/move/rename/edit/save/commit/push/reset/revoke/grant/publish/deploy/invite/share/cancel/close/reopen/disable/enable/lock/unlock/mark/mute/kick/ban/track/untrack) or a destructive annotation."},
			{Key: "maxitems", Label: "Maximum items per tool", Help: "Optional, default 100, maximum 500."},
			{Key: "query", Label: "Default query", Help: "Used only for required string inputs that have no JSON default. Defaults to *; never used to guess secrets."},
			{Key: "timeout", Label: "Timeout", Help: "Optional per MCP operation, default 30s, maximum 5m."},
		},
	}
}

// Source reads explicitly permitted MCP tools and maps their recent results
// into deterministic domain items. A fresh MCP client/session is built for
// each Read call and closed on every path.
type Source struct {
	connection  domain.Connection
	credentials ports.CredentialProvider
	now         func() time.Time
	settings    settings
	stream      string
	identity    string

	// transportFactory is a test seam. Production always uses the SDK's
	// command or Streamable HTTP transport.
	transportFactory func(context.Context, string) (mcp.Transport, func(), error)
	skipMu           sync.RWMutex
	skippedTools     []string
}

type settings struct {
	transport string
	url       string
	tools     []string
	query     string
	maxItems  int
	timeout   time.Duration
}

type cursor struct {
	Watermark *time.Time `json:"watermark"`
	LastScan  int        `json:"lastScan"`
}

// NewSource validates a connection and binds the credential provider. The
// optional token is retrieved only for HTTP; it is never passed to stdio.
func NewSource(conn domain.Connection, credentials ports.CredentialProvider, now func() time.Time) (*Source, error) {
	if err := Descriptor().ValidateConnection(conn); err != nil {
		return nil, err
	}
	parsed, err := parseSettings(conn)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	identity := parsed.transport
	if parsed.transport == "http" {
		identity += "\x00" + parsed.url
	} else {
		// The command is local-only; connection identity is the stable part.
		identity += "\x00" + conn.ID
	}
	return &Source{
		connection: conn, credentials: credentials, now: now,
		settings: parsed, identity: identity, stream: streamName(identity),
	}, nil
}

// Descriptor returns this source's static provider description.
func (s *Source) Descriptor() domain.SourceDescriptor { return Descriptor() }

// Check initializes the server and lists its tools without calling any tool.
func (s *Source) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	session, cleanup, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	defer session.Close()
	if _, err := s.listTools(ctx, session); err != nil {
		return err
	}
	return nil
}

// Read obtains one bounded scan of the configured read tools. Item-level
// batches resume with lastScan; watermark advances only after the whole result
// set has been emitted. Calls are read-only by policy, regardless of tool
// annotations or explicit configuration.
func (s *Source) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, err
	}
	if req.Connection.ID != s.connection.ID || req.Connection.Provider != Name {
		return ports.ReadBatch{}, domain.NewValidationError("mcp.connection", "does not match source connection")
	}
	cur, err := s.readCursor(req)
	if err != nil {
		return ports.ReadBatch{}, err
	}
	session, cleanup, err := s.connect(ctx)
	if err != nil {
		return ports.ReadBatch{}, err
	}
	defer cleanup()
	defer session.Close()

	tools, err := s.listTools(ctx, session)
	if err != nil {
		return ports.ReadBatch{}, err
	}
	selected, skipped := s.selectTools(tools)
	s.recordSkipped(skipped)
	items := make([]domain.Item, 0)
	seen := make(map[string]struct{})
	var newest time.Time
	now := s.now().UTC()
	futureBound := now.Add(pageOverlap)
	successes := 0
	for _, tool := range selected {
		if err := ctx.Err(); err != nil {
			return ports.ReadBatch{}, err
		}
		args, ok := deriveArguments(tool.InputSchema, s.settings.query, s.settings.maxItems)
		if !ok {
			skipped = append(skipped, tool.Name)
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, s.settings.timeout)
		result, callErr := session.CallTool(callCtx, &mcp.CallToolParams{Name: tool.Name, Arguments: args})
		cancel()
		if callErr != nil || result == nil || result.IsError {
			if ctx.Err() != nil {
				return ports.ReadBatch{}, ctx.Err()
			}
			// One broken tool must not block the others; report it and
			// continue. All tools failing is still an error.
			skipped = append(skipped, tool.Name)
			continue
		}
		successes++
		records, err := resultRecords(result)
		if err != nil {
			return ports.ReadBatch{}, errors.New("mcp read: tool result could not be normalized")
		}
		toolItems := make([]domain.Item, 0, len(records))
		for _, record := range records {
			toolItems = append(toolItems, s.item(req.Connection.ID, tool.Name, record))
		}
		// Keep the newest records when a tool returns more than maxitems;
		// this source is recent-results-only by contract. Otherwise preserve
		// the server's deterministic order.
		if len(toolItems) > s.settings.maxItems {
			sort.SliceStable(toolItems, func(i, j int) bool {
				return toolItems[i].UpdatedAt.After(toolItems[j].UpdatedAt)
			})
			toolItems = toolItems[:s.settings.maxItems]
		}
		for _, item := range toolItems {
			if item.UpdatedAt.After(futureBound) {
				// A hostile or broken server must not poison the watermark
				// with a far-future timestamp.
				continue
			}
			if olderThanBounds(item.UpdatedAt, cur.Watermark, req.Since) {
				continue
			}
			if _, duplicate := seen[item.Ref.ExternalID]; duplicate {
				continue
			}
			seen[item.Ref.ExternalID] = struct{}{}
			if item.UpdatedAt.After(newest) {
				newest = item.UpdatedAt.UTC()
			}
			items = append(items, item)
		}
	}
	if len(selected) > 0 && successes == 0 {
		s.recordSkipped(skipped)
		return ports.ReadBatch{}, errors.New("mcp read: all selected tools failed")
	}
	s.recordSkipped(skipped)

	if cur.LastScan > len(items) {
		// The server returned fewer records between batches. Restart the
		// pagination rather than failing forever; already stored items are
		// deduplicated by hash upstream.
		cur.LastScan = 0
	}
	limit := req.Limit
	if limit <= 0 || limit > maximumReadItems {
		limit = maximumReadItems
	}
	start := cur.LastScan
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	out := append([]domain.Item(nil), items[start:end]...)
	more := end < len(items)
	if more {
		cur.LastScan = end
	} else {
		cur.LastScan = 0
		if !newest.IsZero() {
			// Advance monotonically and never beyond the observed present.
			if newest.After(now) {
				newest = now
			}
			if cur.Watermark == nil || newest.After(*cur.Watermark) {
				cur.Watermark = timePointer(newest)
			}
		}
	}
	data, err := json.Marshal(cur)
	if err != nil {
		return ports.ReadBatch{}, errors.New("mcp read: checkpoint could not be encoded")
	}
	return ports.ReadBatch{
		Items:       out,
		Checkpoints: []domain.Checkpoint{{Stream: s.stream, Version: streamVersion, Data: data}},
		More:        more,
	}, nil
}

func (s *Source) readCursor(req ports.ReadRequest) (cursor, error) {
	cur := cursor{}
	cp, ok := req.Checkpoints[s.stream]
	if !ok {
		return cur, nil
	}
	if cp.Stream != s.stream || cp.Version != streamVersion || len(cp.Data) == 0 || len(cp.Data) > domain.MaxCheckpointBytes {
		return cursor{}, domain.NewValidationError("mcp.checkpoint", "unsupported stream, version, or empty data")
	}
	trimmed := strings.TrimSpace(string(cp.Data))
	if !strings.HasPrefix(trimmed, "{") {
		return cursor{}, domain.NewValidationError("mcp.checkpoint", "must be a JSON object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(cp.Data, &fields); err != nil {
		return cursor{}, domain.NewValidationError("mcp.checkpoint", "is malformed")
	}
	if _, ok := fields["watermark"]; !ok {
		return cursor{}, domain.NewValidationError("mcp.checkpoint", "watermark is missing")
	}
	if _, ok := fields["lastScan"]; !ok {
		return cursor{}, domain.NewValidationError("mcp.checkpoint", "lastScan is missing")
	}
	decoder := json.NewDecoder(strings.NewReader(string(cp.Data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cur); err != nil {
		return cursor{}, domain.NewValidationError("mcp.checkpoint", "is malformed")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return cursor{}, domain.NewValidationError("mcp.checkpoint", "contains trailing data")
	}
	if cur.LastScan < 0 || cur.LastScan > maximumTools*maximumMaxItems || (cur.Watermark != nil && cur.Watermark.IsZero()) {
		return cursor{}, domain.NewValidationError("mcp.checkpoint", "position or watermark is invalid")
	}
	if req.Since != nil && (cur.Watermark == nil || req.Since.After(*cur.Watermark)) {
		cur.Watermark = timePointer(req.Since.UTC())
		cur.LastScan = 0
	}
	return cur, nil
}

func (s *Source) connect(ctx context.Context) (*mcp.ClientSession, func(), error) {
	token := ""
	if s.settings.transport == "http" {
		var err error
		token, err = s.httpToken(ctx)
		if err != nil {
			return nil, func() {}, err
		}
	}
	var transport mcp.Transport
	cleanup := func() {}
	if s.transportFactory != nil {
		var err error
		transport, cleanup, err = s.transportFactory(ctx, token)
		if err != nil {
			if cleanup != nil {
				cleanup()
			}
			return nil, func() {}, errors.New("mcp connect: transport creation failed")
		}
		if cleanup == nil {
			cleanup = func() {}
		}
	} else if s.settings.transport == "stdio" {
		command, args, err := s.stdioConfig(ctx)
		if err != nil {
			return nil, func() {}, err
		}
		cmd := exec.CommandContext(ctx, command, args...)
		transport = &mcp.CommandTransport{Command: cmd}
	} else {
		httpClient := newHTTPClient(token)
		transport = &mcp.StreamableClientTransport{
			Endpoint: s.settings.url, HTTPClient: httpClient,
			DisableStandaloneSSE: true, MaxRetries: -1,
		}
		cleanup = httpClient.CloseIdleConnections
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "lv24-brain", Version: "1"}, nil)
	connectCtx, cancel := context.WithTimeout(ctx, s.settings.timeout)
	defer cancel()
	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		cleanup()
		return nil, func() {}, errors.New("mcp connect: initialization failed")
	}
	return session, cleanup, nil
}

func (s *Source) httpToken(ctx context.Context) (string, error) {
	if s.credentials == nil {
		return "", nil
	}
	token, err := s.credentials.Get(ctx, s.connection.ID, "token")
	if errors.Is(err, domain.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", errors.New("mcp credentials: token could not be retrieved")
	}
	token = strings.TrimSpace(token)
	if strings.ContainsAny(token, "\r\n\x00") {
		return "", domain.NewValidationError("mcp.credentials", "token contains invalid characters")
	}
	return token, nil
}

// stdioConfig loads the executable and argv from the local credential store.
// Keeping them out of Connection.Settings prevents a synced (shared) catalogue
// from introducing an executable command on another machine.
func (s *Source) stdioConfig(ctx context.Context) (string, []string, error) {
	if s.credentials == nil {
		return "", nil, errors.New("mcp stdio: credential store is unavailable")
	}
	command, err := s.credentials.Get(ctx, s.connection.ID, "command")
	if errors.Is(err, domain.ErrNotFound) {
		return "", nil, domain.NewValidationError("mcp.command", "is not configured in the local credential store")
	}
	if err != nil {
		return "", nil, errors.New("mcp stdio: command could not be retrieved")
	}
	command = strings.TrimSpace(command)
	if err := validateCommand(command); err != nil {
		return "", nil, err
	}
	argsRaw, err := s.credentials.Get(ctx, s.connection.ID, "args")
	if errors.Is(err, domain.ErrNotFound) {
		argsRaw = ""
	} else if err != nil {
		return "", nil, errors.New("mcp stdio: arguments could not be retrieved")
	}
	if len(argsRaw) > 64<<10 || strings.ContainsRune(argsRaw, 0) {
		return "", nil, domain.NewValidationError("mcp.args", "must be at most 65536 bytes and contain no NUL")
	}
	argsRaw = strings.ReplaceAll(strings.ReplaceAll(argsRaw, "\r\n", "\n"), "\r", "\n")
	argsRaw = strings.TrimRight(argsRaw, "\n")
	var args []string
	if argsRaw != "" {
		for _, arg := range strings.Split(argsRaw, "\n") {
			if len(arg) > 4096 || len(args) >= 64 {
				return "", nil, domain.NewValidationError("mcp.args", "at most 64 arguments of 4096 bytes each are allowed")
			}
			args = append(args, arg)
		}
	}
	return command, args, nil
}

func (s *Source) listTools(ctx context.Context, session *mcp.ClientSession) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	cursorToken := ""
	for page := 0; page < maximumListPages; page++ {
		listCtx, cancel := context.WithTimeout(ctx, s.settings.timeout)
		result, err := session.ListTools(listCtx, &mcp.ListToolsParams{Cursor: cursorToken})
		cancel()
		if err != nil || result == nil {
			return nil, errors.New("mcp read: tool listing failed")
		}
		for _, tool := range result.Tools {
			if tool != nil && validToolName(tool.Name) {
				tools = append(tools, tool)
				if len(tools) > maximumListedTools {
					return nil, errors.New("mcp read: server exposes too many tools")
				}
			}
		}
		if result.NextCursor == "" {
			return tools, nil
		}
		cursorToken = result.NextCursor
	}
	return nil, errors.New("mcp read: tool listing exceeded page limit")
}

// SkippedTools returns the bounded names of tools skipped by the most recent
// Read because they were explicitly denied or had unsynthesizable required
// input fields. It contains names only, never schemas or payloads.
func (s *Source) SkippedTools() []string {
	s.skipMu.RLock()
	defer s.skipMu.RUnlock()
	return append([]string(nil), s.skippedTools...)
}

func (s *Source) recordSkipped(names []string) {
	if len(names) > maximumTools {
		names = names[:maximumTools]
	}
	s.skipMu.Lock()
	s.skippedTools = append(s.skippedTools[:0], names...)
	s.skipMu.Unlock()
}

func (s *Source) selectTools(tools []*mcp.Tool) ([]*mcp.Tool, []string) {
	allow := make(map[string]bool, len(s.settings.tools))
	for _, name := range s.settings.tools {
		allow[name] = true
	}
	selected := make([]*mcp.Tool, 0, maximumTools)
	var skipped []string
	for _, tool := range tools {
		name := tool.Name
		if destructiveTool(tool) {
			if allow[name] {
				skipped = append(skipped, name)
			}
			continue
		}
		if len(allow) > 0 {
			if !allow[name] {
				continue
			}
		} else if !defaultReadTool(name) {
			continue
		}
		selected = append(selected, tool)
		if len(selected) == maximumTools {
			break
		}
	}
	return selected, skipped
}

func parseSettings(conn domain.Connection) (settings, error) {
	out := settings{
		transport: strings.TrimSpace(conn.Settings["transport"]),
		query:     conn.Settings["query"],
		maxItems:  defaultMaxItems,
		timeout:   defaultTimeout,
	}
	if out.query == "" {
		out.query = "*"
	}
	if len(out.query) > 4096 || strings.ContainsRune(out.query, 0) {
		return settings{}, domain.NewValidationError("mcp.query", "must be at most 4096 bytes and contain no NUL")
	}
	switch out.transport {
	case "stdio":
		if strings.TrimSpace(conn.Settings["command"]) != "" || strings.TrimSpace(conn.Settings["args"]) != "" {
			return settings{}, domain.NewValidationError("mcp.command", "must be stored in the credential store, not connection settings")
		}
		if strings.TrimSpace(conn.Settings["url"]) != "" {
			return settings{}, domain.NewValidationError("mcp.url", "must be empty for stdio")
		}
	case "http":
		rawURL := strings.TrimSpace(conn.Settings["url"])
		parsed, err := url.Parse(rawURL)
		if err != nil || !validEndpoint(parsed) {
			return settings{}, domain.NewValidationError("mcp.url", "must be an https URL (http only for loopback) without userinfo, query, or fragment")
		}
		out.url = parsed.String()
		if strings.TrimSpace(conn.Settings["command"]) != "" || strings.TrimSpace(conn.Settings["args"]) != "" {
			return settings{}, domain.NewValidationError("mcp.command", "must be stored in the credential store, not connection settings")
		}
	default:
		return settings{}, domain.NewValidationError("mcp.transport", "must be stdio or http")
	}
	if raw := strings.TrimSpace(conn.Settings["maxitems"]); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > maximumMaxItems {
			return settings{}, domain.NewValidationError("mcp.maxitems", "must be an integer from 1 to 500")
		}
		out.maxItems = value
	}
	if raw := strings.TrimSpace(conn.Settings["timeout"]); raw != "" {
		duration, err := time.ParseDuration(raw)
		if err != nil || duration <= 0 || duration > maximumTimeout {
			return settings{}, domain.NewValidationError("mcp.timeout", "must be a positive duration no greater than 5m")
		}
		out.timeout = duration
	}
	toolsRaw := conn.Settings["tools"]
	if len(toolsRaw) > 16<<10 {
		return settings{}, domain.NewValidationError("mcp.tools", "must be at most 16384 bytes")
	}
	seen := make(map[string]bool)
	for _, name := range strings.FieldsFunc(toolsRaw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		name = strings.TrimSpace(name)
		if !validToolName(name) {
			return settings{}, domain.NewValidationError("mcp.tools", "tool names must use letters, digits, '_', '-' or '.' (at most 128 bytes)")
		}
		if !seen[name] {
			seen[name] = true
			out.tools = append(out.tools, name)
		}
	}
	if len(out.tools) > maximumTools {
		return settings{}, domain.NewValidationError("mcp.tools", "at most 16 tools may be enabled")
	}
	return out, nil
}

func validateCommand(command string) error {
	if command == "" || strings.ContainsAny(command, "\x00\r\n") {
		return domain.NewValidationError("mcp.command", "is required and must not contain NUL or newlines")
	}
	// Bare executable names are resolved through PATH. Path-like commands must
	// be absolute so the configured executable cannot depend on a working dir.
	pathLike := strings.ContainsAny(command, `/\\`) || (runtime.GOOS == "windows" && strings.Contains(command, ":"))
	absolute := strings.HasPrefix(command, "/") || (len(command) >= 3 && command[1] == ':' && (command[2] == '\\' || command[2] == '/'))
	if runtime.GOOS == "windows" && strings.HasPrefix(command, `\\`) {
		absolute = true
	}
	if pathLike && !absolute {
		return domain.NewValidationError("mcp.command", "path-like commands must be absolute; bare PATH names are allowed")
	}
	return nil
}

func validEndpoint(parsed *url.URL) bool {
	if parsed == nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.ForceQuery {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return true
	case "http":
		host := strings.ToLower(parsed.Hostname())
		ip := net.ParseIP(host)
		return host == "localhost" || (ip != nil && ip.IsLoopback())
	default:
		return false
	}
}

func streamName(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return "mcp-" + hex.EncodeToString(sum[:16])
}

func deniedTool(name string) bool {
	lower := strings.ToLower(name)
	for _, word := range []string{
		"create", "update", "delete", "remove", "write", "send", "execute",
		"run", "call", "set", "add", "insert", "upsert", "upload", "post",
		"put", "patch", "archive", "purge", "destroy", "drop", "truncate",
		"approve", "merge", "assign", "move", "rename", "edit", "save",
		"commit", "push", "reset", "revoke", "grant", "publish", "deploy",
		"invite", "share", "cancel", "close", "reopen", "disable", "enable",
		"lock", "unlock", "mark", "mute", "kick", "ban", "track", "untrack",
	} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

// destructiveTool is the unconditional deny gate: a name marker or an
// explicit destructive annotation from the server both deny the call.
func destructiveTool(tool *mcp.Tool) bool {
	if deniedTool(tool.Name) {
		return true
	}
	return tool.Annotations != nil && tool.Annotations.DestructiveHint != nil && *tool.Annotations.DestructiveHint
}

func validToolName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return false
		}
	}
	return true
}

func defaultReadTool(name string) bool {
	if deniedTool(name) {
		return false
	}
	lower := strings.ToLower(name)
	for _, word := range []string{"search", "list", "recent", "query", "find"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

func deriveArguments(schema any, query string, maxItems int) (map[string]any, bool) {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, false
	}
	var root map[string]any
	if json.Unmarshal(encoded, &root) != nil {
		return nil, false
	}
	properties, _ := root["properties"].(map[string]any)
	required := make(map[string]bool)
	if entries, ok := root["required"].([]any); ok {
		for _, entry := range entries {
			if name, ok := entry.(string); ok {
				required[name] = true
			}
		}
	}
	args := make(map[string]any)
	for name, propertyValue := range properties {
		property, ok := propertyValue.(map[string]any)
		if !ok {
			continue
		}
		if sensitiveInput(name) || deniedTool(name) {
			if required[name] {
				return nil, false
			}
			continue
		}
		if value, ok := property["default"]; ok {
			// Only send defaults for required fields; optional flags such as
			// "confirm" or "method" must not be injected by a hostile schema.
			if required[name] {
				args[name] = value
			}
			continue
		}
		if required[name] {
			typeName, _ := property["type"].(string)
			if typeName != "string" || !searchFieldName(name) {
				return nil, false
			}
			args[name] = query
		}
	}
	for name := range required {
		if _, ok := args[name]; !ok {
			return nil, false
		}
	}
	_ = maxItems // Output caps are enforced locally; tool schemas may define incompatible limits.
	return args, true
}

func searchFieldName(name string) bool {
	name = normalizedField(name)
	if name == "q" || name == "query" {
		return true
	}
	for _, marker := range []string{"search", "keyword", "term", "text", "filter", "query", "name", "title"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

func sensitiveInput(name string) bool {
	name = strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(name))
	for _, marker := range []string{"token", "password", "secret", "apikey", "authorization", "credential", "privatekey"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return name == "key"
}

func (s *Source) item(connectionID, toolName string, record any) domain.Item {
	canonical := canonicalJSON(record)
	if len(canonical) > maximumResponseBytes {
		canonical = capUTF8(canonical, maximumResponseBytes)
	}
	values, _ := record.(map[string]any)
	name := firstString(values, "title", "name", "subject", "id")
	if name == "" {
		name = toolName
	}
	bodyValue := firstString(values, "text", "content", "description", "body", "summary")
	if bodyValue == "" {
		bodyValue = canonical
	}
	prefix := "MCP tool " + toolName + " (recent results only)\n\n"
	body := prefix + capUTF8(bodyValue, domain.MaxItemBodyBytes-len(prefix))
	if len(body) > domain.MaxItemBodyBytes {
		body = capUTF8(body, domain.MaxItemBodyBytes)
	}
	hash := sha256.Sum256([]byte(canonical))
	identityPart := ""
	if id := firstString(values, "id", "uuid", "key", "number", "slug"); id != "" {
		id = strings.Map(func(r rune) rune {
			if r < ' ' || r == 127 {
				return -1
			}
			return r
		}, id)
		if id != "" {
			identityPart = "id/" + url.PathEscape(capUTF8(id, 512))
		}
	}
	if identityPart == "" {
		identityPart = "sha256/" + hex.EncodeToString(hash[:])
	}
	externalID := "mcp/" + streamName(s.identity)[4:] + "/" + toolName + "/" + identityPart
	item := domain.Item{
		Ref:  domain.ItemRef{ConnectionID: connectionID, ExternalID: externalID},
		Kind: domain.ItemDocument, Title: capUTF8(name, 512), Body: body,
		URL: capUTF8(extractURL(values), 2048), Author: capUTF8(firstString(values, "author", "user", "owner", "creator"), 256),
	}
	item.CreatedAt = extractTime(values, "createdat", "created", "publishedat", "published")
	item.UpdatedAt = extractTime(values, "updatedat", "updated", "modifiedat", "modified", "lastmodified")
	return item
}

func olderThanBounds(updated time.Time, watermark, since *time.Time) bool {
	if updated.IsZero() {
		return false
	}
	var bound *time.Time
	if watermark != nil {
		bound = timePointer(watermark.UTC().Add(-pageOverlap))
	}
	if since != nil && (bound == nil || since.After(*bound)) {
		bound = timePointer(since.UTC())
	}
	return bound != nil && updated.Before(*bound)
}

func timePointer(value time.Time) *time.Time { return &value }

func newHTTPClient(token string) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{
		Transport:     authAndLimitRoundTripper{base: base, token: token},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type authAndLimitRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (rt authAndLimitRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	if rt.token != "" {
		clone.Header.Set("Authorization", "Bearer "+rt.token)
	}
	response, err := rt.base.RoundTrip(clone)
	if err != nil || response == nil || response.Body == nil {
		return response, err
	}
	response.Body = &boundedReadCloser{body: response.Body, reader: &limitedReader{reader: response.Body, remaining: maximumResponseBytes}}
	return response, nil
}

type boundedReadCloser struct {
	body   io.ReadCloser
	reader *limitedReader
}

func (b *boundedReadCloser) Read(p []byte) (int, error) { return b.reader.Read(p) }
func (b *boundedReadCloser) Close() error               { return b.body.Close() }

type limitedReader struct {
	reader    io.Reader
	remaining int64
}

func (r *limitedReader) Read(p []byte) (int, error) {
	if r.remaining < 0 {
		return 0, fmt.Errorf("mcp response exceeds %d bytes", maximumResponseBytes)
	}
	if int64(len(p)) > r.remaining+1 {
		p = p[:r.remaining+1]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	if r.remaining < 0 {
		return n, fmt.Errorf("mcp response exceeds %d bytes", maximumResponseBytes)
	}
	return n, err
}

var _ ports.Source = (*Source)(nil)
