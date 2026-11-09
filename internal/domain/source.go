package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Integration contracts (contracts-v1). These values describe external
// sources, the connections a user configures for them and the items and
// checkpoints a source produces. They are pure: no I/O, no clocks, no secrets.

const (
	maxConnectionIDLength = 64
	maxProviderLength     = 64
	maxAccountLength      = 256
	maxExternalIDLength   = 1024
	maxStreamLength       = 128
	maxConfigKeyLength    = 32
	maxTagLength          = 256
	minScheduleInterval   = time.Minute

	// MaxCheckpointBytes caps one checkpoint payload. A checkpoint is a
	// cursor, not data; anything larger is a source bug.
	MaxCheckpointBytes = 64 << 10

	// MaxItemBodyBytes caps Item.Body. The host truncates longer bodies (and
	// records the truncation) before Validate; sources may return more.
	MaxItemBodyBytes = 512 << 10

	// ItemHashPrefix prefixes every Item.Hash value.
	ItemHashPrefix = "sha256:"

	itemKeyHashLength = 40
)

// PublishPolicy decides where items of a connection may travel.
type PublishPolicy string

const (
	// PublishLocalOnly keeps items on this machine: never committed to the
	// shared data repository and never uploaded to a server (chat, mail).
	PublishLocalOnly PublishPolicy = "local-only"
	// PublishShared allows items to be synchronized like other brain data.
	PublishShared PublishPolicy = "shared"
)

// Valid reports whether p is a known non-empty policy.
func (p PublishPolicy) Valid() bool {
	return p == PublishLocalOnly || p == PublishShared
}

// AuthMode is one way a source authenticates.
type AuthMode string

const (
	AuthToken       AuthMode = "token"
	AuthBasic       AuthMode = "basic"
	AuthOAuthPKCE   AuthMode = "oauth-pkce"
	AuthOAuthDevice AuthMode = "oauth-device"
	AuthNone        AuthMode = "none"
)

// Valid reports whether m is a known auth mode.
func (m AuthMode) Valid() bool {
	switch m {
	case AuthToken, AuthBasic, AuthOAuthPKCE, AuthOAuthDevice, AuthNone:
		return true
	default:
		return false
	}
}

// Backend says how a connection reaches its provider.
type Backend string

const (
	// BackendNative is a compiled-in Go client (the default).
	BackendNative Backend = "native"
	// BackendMCP reads through an MCP server (actions, live reads, long tail).
	BackendMCP Backend = "mcp"
	// BackendProcess runs an external connector process over a JSON protocol.
	BackendProcess Backend = "process"
)

// Valid reports whether b is a known backend.
func (b Backend) Valid() bool {
	return b == BackendNative || b == BackendMCP || b == BackendProcess
}

// ItemKind classifies an external item.
type ItemKind string

const (
	ItemIssue        ItemKind = "issue"
	ItemPullRequest  ItemKind = "pull-request"
	ItemMergeRequest ItemKind = "merge-request"
	ItemCommit       ItemKind = "commit"
	ItemMessage      ItemKind = "message"
	ItemPage         ItemKind = "page"
	ItemDocument     ItemKind = "document"
	ItemEvent        ItemKind = "event"
	ItemSchema       ItemKind = "schema"
	ItemOther        ItemKind = "other"
)

// Valid reports whether k is a known item kind.
func (k ItemKind) Valid() bool {
	switch k {
	case ItemIssue, ItemPullRequest, ItemMergeRequest, ItemCommit, ItemMessage,
		ItemPage, ItemDocument, ItemEvent, ItemSchema, ItemOther:
		return true
	default:
		return false
	}
}

// Capabilities declares what a source can do. The host uses them to choose a
// run strategy; it never assumes a capability that is not declared.
type Capabilities struct {
	// Backfill: the source can read history from the beginning.
	Backfill bool `json:"backfill"`
	// Incremental: the source returns checkpoints and resumes from them.
	Incremental bool `json:"incremental"`
	// Deletes: the source emits tombstones (Item.Deleted) for removed items.
	// Without it the host never infers deletions from absence.
	Deletes bool `json:"deletes"`
	// AuthModes lists supported authentication modes, preferred first.
	AuthModes []AuthMode `json:"authModes"`
	// Actions: the source exposes write actions (separate, audited contract).
	Actions bool `json:"actions"`
}

// Validate checks that at least one known auth mode is declared.
func (c Capabilities) Validate() error {
	if len(c.AuthModes) == 0 {
		return NewValidationError("capabilities.authModes", "is required")
	}
	seen := make(map[AuthMode]bool, len(c.AuthModes))
	for _, mode := range c.AuthModes {
		if !mode.Valid() {
			return NewValidationError("capabilities.authModes", fmt.Sprintf("unknown auth mode %q", mode))
		}
		if seen[mode] {
			return NewValidationError("capabilities.authModes", fmt.Sprintf("duplicate auth mode %q", mode))
		}
		seen[mode] = true
	}
	return nil
}

// ConfigField describes one provider-specific connection setting. Non-secret
// values live in Connection.Settings; secret values live only in the
// credential store under CredentialKey(connectionID, Key).
type ConfigField struct {
	// Key matches [a-z][a-z0-9]* (max 32). No separators, so a credential
	// name conn/<id>/<key> stays unambiguous after any '/', '-', '.' -> '_'
	// normalization by a secret backend.
	Key      string `json:"key"`
	Label    string `json:"label"`
	Required bool   `json:"required"`
	Secret   bool   `json:"secret"`
	Help     string `json:"help,omitempty"`
}

// Validate checks the field key and label.
func (f ConfigField) Validate() error {
	if err := ValidateConfigKey("configField.key", f.Key); err != nil {
		return err
	}
	return ValidateRequired("configField.label", strings.TrimSpace(f.Label))
}

// SourceDescriptor is the static description of one provider: identity,
// capabilities, default publish policy and the settings a connection needs.
type SourceDescriptor struct {
	Provider       string        `json:"provider"`
	DisplayName    string        `json:"displayName"`
	Capabilities   Capabilities  `json:"capabilities"`
	DefaultPublish PublishPolicy `json:"defaultPublish"`
	ConfigFields   []ConfigField `json:"configFields,omitempty"`
	// Backends lists the supported backends, preferred first; empty means
	// native only.
	Backends []Backend `json:"backends,omitempty"`
	// Streams lists the source's fixed checkpoint streams ("issues",
	// "commits"); empty means the source names streams dynamically (mail
	// folders, channels), so the host cannot validate a stream reset upfront.
	Streams []string `json:"streams,omitempty"`
	// PersonalData marks sources whose items routinely hold personal data
	// (mail, chat): the host applies its stricter PII policy on ingest.
	PersonalData bool `json:"personalData,omitempty"`
}

// SupportsBackend reports whether b (empty = native) is supported.
func (d SourceDescriptor) SupportsBackend(b Backend) bool {
	if b == "" {
		b = BackendNative
	}
	if len(d.Backends) == 0 {
		return b == BackendNative
	}
	for _, supported := range d.Backends {
		if supported == b {
			return true
		}
	}
	return false
}

// Validate checks the descriptor and that config field keys are unique.
func (d SourceDescriptor) Validate() error {
	if err := ValidateProvider("descriptor.provider", d.Provider); err != nil {
		return err
	}
	if err := ValidateRequired("descriptor.displayName", strings.TrimSpace(d.DisplayName)); err != nil {
		return err
	}
	if err := d.Capabilities.Validate(); err != nil {
		return err
	}
	if !d.DefaultPublish.Valid() {
		return NewValidationError("descriptor.defaultPublish", fmt.Sprintf("unknown policy %q", d.DefaultPublish))
	}
	seen := make(map[string]bool, len(d.ConfigFields))
	for _, field := range d.ConfigFields {
		if err := field.Validate(); err != nil {
			return err
		}
		if seen[field.Key] {
			return NewValidationError("descriptor.configFields", fmt.Sprintf("duplicate key %q", field.Key))
		}
		seen[field.Key] = true
	}
	backends := make(map[Backend]bool, len(d.Backends))
	for _, backend := range d.Backends {
		if !backend.Valid() {
			return NewValidationError("descriptor.backends", fmt.Sprintf("unknown backend %q", backend))
		}
		if backends[backend] {
			return NewValidationError("descriptor.backends", fmt.Sprintf("duplicate backend %q", backend))
		}
		backends[backend] = true
	}
	streams := make(map[string]bool, len(d.Streams))
	for _, stream := range d.Streams {
		if err := ValidateStream("descriptor.streams", stream); err != nil {
			return err
		}
		if streams[strings.ToLower(stream)] {
			return NewValidationError("descriptor.streams", fmt.Sprintf("duplicate stream %q", stream))
		}
		streams[strings.ToLower(stream)] = true
	}
	return nil
}

// Field returns the config field with key.
func (d SourceDescriptor) Field(key string) (ConfigField, bool) {
	for _, field := range d.ConfigFields {
		if field.Key == key {
			return field, true
		}
	}
	return ConfigField{}, false
}

// ValidateConnection checks c against the descriptor: matching provider,
// required non-secret settings present, no unknown settings and no secret
// values stored in Settings. Required secret fields are checked by
// Source.Check through the credential provider, never here.
func (d SourceDescriptor) ValidateConnection(c Connection) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Provider != d.Provider {
		return NewValidationError("connection.provider", fmt.Sprintf("is %q, descriptor is %q", c.Provider, d.Provider))
	}
	if !d.SupportsBackend(c.Backend) {
		return NewValidationError("connection.backend", fmt.Sprintf("%q is not supported by %s", c.Backend, d.Provider))
	}
	for key := range c.Settings {
		field, ok := d.Field(key)
		if !ok {
			return NewValidationError("connection.settings."+key, "is not a setting of "+d.Provider)
		}
		if field.Secret {
			return NewValidationError("connection.settings."+key, "is secret; store it in the credential store")
		}
	}
	for _, field := range d.ConfigFields {
		if field.Required && !field.Secret && strings.TrimSpace(c.Settings[field.Key]) == "" {
			return NewValidationError("connection.settings."+field.Key, "is required")
		}
	}
	return nil
}

// Connection is one configured account of one provider. It never contains
// secrets. ID is unique per data directory (enforced by ConnectionStore) and
// is safe as a path segment and as a credential-name segment.
type Connection struct {
	// ID matches [a-z0-9]([a-z0-9-]*[a-z0-9])? (max 64).
	ID string `json:"id"`
	// Provider is a registered SourceDescriptor.Provider.
	Provider string `json:"provider"`
	// Backend selects how the provider is reached; empty means native.
	Backend Backend `json:"backend,omitempty"`
	// Account is a display/identity label (login, mailbox, workspace).
	Account string `json:"account,omitempty"`
	// BaseURL is the provider endpoint; empty means the provider default.
	// Userinfo and fragments are rejected so credentials never land here.
	BaseURL string `json:"baseUrl,omitempty"`
	// Projects are brain project IDs this connection feeds.
	Projects []string `json:"projects,omitempty"`
	// Schedule is "", "manual", "@hourly", "@daily" or "@every <duration>"
	// (minimum 1m). Empty and "manual" mean run only on request.
	Schedule string `json:"schedule,omitempty"`
	// Publish overrides SourceDescriptor.DefaultPublish when non-empty.
	Publish PublishPolicy `json:"publish,omitempty"`
	// Settings are non-secret provider settings keyed by ConfigField.Key.
	Settings map[string]string `json:"settings,omitempty"`
}

// Validate checks the connection without consulting a descriptor.
func (c Connection) Validate() error {
	if err := ValidateConnectionID("connection.id", c.ID); err != nil {
		return err
	}
	if err := ValidateProvider("connection.provider", c.Provider); err != nil {
		return err
	}
	if c.Backend != "" && !c.Backend.Valid() {
		return NewValidationError("connection.backend", fmt.Sprintf("unknown backend %q", c.Backend))
	}
	if err := validateLabel("connection.account", c.Account, maxAccountLength); err != nil {
		return err
	}
	if _, err := canonicalBaseURL(c.BaseURL); err != nil {
		return err
	}
	if err := validateProjects(c.Projects); err != nil {
		return err
	}
	if err := ValidateSchedule(c.Schedule); err != nil {
		return err
	}
	if c.Publish != "" && !c.Publish.Valid() {
		return NewValidationError("connection.publish", fmt.Sprintf("unknown policy %q", c.Publish))
	}
	for key := range c.Settings {
		if err := ValidateConfigKey("connection.settings", key); err != nil {
			return err
		}
	}
	return nil
}

// EffectivePublish returns the connection override or the descriptor default.
func (c Connection) EffectivePublish(d SourceDescriptor) PublishPolicy {
	if c.Publish != "" {
		return c.Publish
	}
	return d.DefaultPublish
}

// Identity is the canonical provider|endpoint|account triple. Two
// connections with the same identity read the same data; ConnectionStore
// rejects such duplicates and treats the identity as immutable. Invalid base
// URLs yield the raw trimmed value; call Validate first.
func (c Connection) Identity() string {
	base, err := canonicalBaseURL(c.BaseURL)
	if err != nil {
		base = strings.TrimSpace(c.BaseURL)
	}
	return c.Provider + "|" + base + "|" + strings.ToLower(strings.TrimSpace(c.Account))
}

// CredentialKey returns the credential-store name conn/<id>/<field>.
//
// Secret backends may fold '/', '-', '.' and '_' into '_' and ignore case.
// The name stays unambiguous after that folding: IDs use only [a-z0-9-], field
// keys only [a-z0-9] with no separator, so the last separator always splits id
// from field, and no legacy name starts with "conn".
func CredentialKey(connectionID, field string) (string, error) {
	if err := ValidateConnectionID("credential.connectionId", connectionID); err != nil {
		return "", err
	}
	if err := ValidateConfigKey("credential.field", field); err != nil {
		return "", err
	}
	return "conn/" + connectionID + "/" + field, nil
}

// ItemRef identifies one external item: the connection it came from plus the
// provider's own stable identifier (issue node ID, Message-ID, page ID...).
type ItemRef struct {
	ConnectionID string `json:"connectionId"`
	ExternalID   string `json:"externalId"`
}

// Validate checks both parts. ExternalID is any valid UTF-8 without NUL.
func (r ItemRef) Validate() error {
	if err := ValidateConnectionID("item.connectionId", r.ConnectionID); err != nil {
		return err
	}
	if r.ExternalID == "" {
		return NewValidationError("item.externalId", "is required")
	}
	if len(r.ExternalID) > maxExternalIDLength {
		return NewValidationError("item.externalId", fmt.Sprintf("must be at most %d bytes", maxExternalIDLength))
	}
	if !utf8.ValidString(r.ExternalID) || strings.ContainsRune(r.ExternalID, 0) {
		return NewValidationError("item.externalId", "must be valid UTF-8 without NUL")
	}
	return nil
}

// Key is the stable storage key "<connectionID>/<40 hex>". The hash covers
// the exact ExternalID, so IDs that would sanitize to the same file name
// ("a/b", "a_b", "A/B") never collide, and the key is path safe on every OS.
// Different hosts or accounts are always different connections, so keys never
// collide across providers, hosts or accounts.
func (r ItemRef) Key() string {
	sum := sha256.Sum256([]byte(r.ConnectionID + "\x00" + r.ExternalID))
	return r.ConnectionID + "/" + hex.EncodeToString(sum[:])[:itemKeyHashLength]
}

// Item is one normalized external object in the raw zone. A tombstone has
// Deleted set; only Ref and ObservedAt matter for it.
type Item struct {
	Ref    ItemRef  `json:"ref"`
	Kind   ItemKind `json:"kind"`
	Title  string   `json:"title"`
	Body   string   `json:"body,omitempty"` // Markdown
	URL    string   `json:"url,omitempty"`
	Author string   `json:"author,omitempty"`
	Labels []string `json:"labels,omitempty"`
	// CreatedAt and UpdatedAt are provider times; zero when unknown.
	CreatedAt time.Time `json:"createdAt,omitzero"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
	// ObservedAt is when the host received the item; set by the host.
	ObservedAt time.Time `json:"observedAt,omitzero"`
	// Hash is ComputeHash(); empty from a source, set by the host.
	Hash string `json:"hash,omitempty"`
	// ACL holds upstream visibility tags (e.g. "public", "group:eng").
	ACL     []string `json:"acl,omitempty"`
	Deleted bool     `json:"deleted,omitempty"`
	// ProjectID is the brain project the item maps to; empty when unmapped.
	ProjectID string `json:"projectId,omitempty"`
}

// Key returns Ref.Key().
func (i Item) Key() string { return i.Ref.Key() }

// Validate checks the item. A non-empty Hash must equal ComputeHash().
func (i Item) Validate() error {
	if err := i.Ref.Validate(); err != nil {
		return err
	}
	if !i.Deleted {
		if !i.Kind.Valid() {
			return NewValidationError("item.kind", fmt.Sprintf("unknown kind %q", i.Kind))
		}
		if strings.TrimSpace(i.Title) == "" {
			return NewValidationError("item.title", "is required")
		}
		if !utf8.ValidString(i.Title) || !utf8.ValidString(i.Body) {
			return NewValidationError("item.body", "must be valid UTF-8")
		}
		if len(i.Body) > MaxItemBodyBytes {
			return NewValidationError("item.body", fmt.Sprintf("must be at most %d bytes; the host truncates longer bodies", MaxItemBodyBytes))
		}
		if err := validateTags("item.labels", i.Labels); err != nil {
			return err
		}
		if err := validateTags("item.acl", i.ACL); err != nil {
			return err
		}
	}
	if i.Hash != "" && i.Hash != i.ComputeHash() {
		return NewValidationError("item.hash", "does not match content")
	}
	return nil
}

// ComputeHash returns "sha256:<hex>" over normalized content: kind, title,
// body, URL, author, sorted labels and ACL, provider times, project and the
// deleted flag. ObservedAt, Ref and Hash are excluded, so re-reading an
// unchanged item yields the same hash. Body line endings are normalized to
// LF, trailing spaces per line and leading/trailing blank lines are dropped;
// title, author and tags are whitespace-collapsed.
func (i Item) ComputeHash() string {
	var buf bytes.Buffer
	writeField := func(value string) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		buf.Write(size[:])
		buf.WriteString(value)
	}
	writeField("item-v1")
	writeField(boolString(i.Deleted))
	if !i.Deleted {
		writeField(string(i.Kind))
		writeField(collapseSpace(i.Title))
		writeField(NormalizeMarkdown(i.Body))
		writeField(strings.TrimSpace(i.URL))
		writeField(collapseSpace(i.Author))
		writeField(strings.Join(normalizeTags(i.Labels), "\x00"))
		writeField(strings.Join(normalizeTags(i.ACL), "\x00"))
		writeField(timeString(i.CreatedAt))
		writeField(timeString(i.UpdatedAt))
		writeField(strings.TrimSpace(i.ProjectID))
	}
	sum := sha256.Sum256(buf.Bytes())
	return ItemHashPrefix + hex.EncodeToString(sum[:])
}

// WithHash returns a copy of i with Hash set to ComputeHash().
func (i Item) WithHash() Item {
	i.Hash = i.ComputeHash()
	return i
}

// NormalizeMarkdown converts CRLF/CR to LF, drops trailing whitespace on
// each line and leading/trailing blank lines.
func NormalizeMarkdown(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	lines := strings.Split(body, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimRightFunc(line, unicode.IsSpace)
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// Checkpoint is an opaque, source-versioned cursor for one stream of one
// connection. The host stores and returns Data byte-for-byte and never
// interprets it; Version lets a source migrate or reject old formats.
type Checkpoint struct {
	// Stream names an independent cursor inside a source ("issues",
	// "INBOX"); matches [a-z0-9][a-z0-9._-]* case-insensitively, max 128.
	Stream  string `json:"stream"`
	Version int    `json:"version"`
	Data    []byte `json:"data"`
}

// Validate checks the stream name, version and payload size.
func (c Checkpoint) Validate() error {
	if err := ValidateStream("checkpoint.stream", c.Stream); err != nil {
		return err
	}
	if c.Version < 1 {
		return NewValidationError("checkpoint.version", "must be at least 1")
	}
	if len(c.Data) > MaxCheckpointBytes {
		return NewValidationError("checkpoint.data", fmt.Sprintf("must be at most %d bytes", MaxCheckpointBytes))
	}
	return nil
}

// Clone returns a deep copy so callers never share Data.
func (c Checkpoint) Clone() Checkpoint {
	if c.Data != nil {
		c.Data = bytes.Clone(c.Data)
	}
	return c
}

// Equal reports byte-for-byte equality.
func (c Checkpoint) Equal(other Checkpoint) bool {
	return c.Stream == other.Stream && c.Version == other.Version && bytes.Equal(c.Data, other.Data)
}

// TruncationReason says why an item-level run stopped before the source was
// exhausted. Empty means not truncated.
type TruncationReason string

const (
	TruncatedLimit     TruncationReason = "limit"
	TruncatedDeadline  TruncationReason = "deadline"
	TruncatedRateLimit TruncationReason = "rate-limit"
	TruncatedSizeCap   TruncationReason = "size-cap"
	TruncatedCanceled  TruncationReason = "canceled"
)

// Valid reports whether r is empty or a known reason.
func (r TruncationReason) Valid() bool {
	switch r {
	case "", TruncatedLimit, TruncatedDeadline, TruncatedRateLimit, TruncatedSizeCap, TruncatedCanceled:
		return true
	default:
		return false
	}
}

// RunStats counts item-level work in one run. Seen = Upserted + Unchanged +
// Deleted + Skipped.
type RunStats struct {
	Seen      int `json:"seen"`
	Upserted  int `json:"upserted"`
	Unchanged int `json:"unchanged"`
	Deleted   int `json:"deleted"`
	Skipped   int `json:"skipped"`
	// Batches is the number of Source.Read calls whose items were stored.
	Batches   int              `json:"batches"`
	Truncated TruncationReason `json:"truncated,omitempty"`
}

// Add accumulates other into s; the first non-empty Truncated wins.
func (s RunStats) Add(other RunStats) RunStats {
	s.Seen += other.Seen
	s.Upserted += other.Upserted
	s.Unchanged += other.Unchanged
	s.Deleted += other.Deleted
	s.Skipped += other.Skipped
	s.Batches += other.Batches
	if s.Truncated == "" {
		s.Truncated = other.Truncated
	}
	return s
}

// Validate checks counts are non-negative and consistent.
func (s RunStats) Validate() error {
	for _, value := range []int{s.Seen, s.Upserted, s.Unchanged, s.Deleted, s.Skipped, s.Batches} {
		if value < 0 {
			return NewValidationError("runStats", "counts must not be negative")
		}
	}
	if s.Upserted+s.Unchanged+s.Deleted+s.Skipped != s.Seen {
		return NewValidationError("runStats.seen", "must equal upserted+unchanged+deleted+skipped")
	}
	if !s.Truncated.Valid() {
		return NewValidationError("runStats.truncated", fmt.Sprintf("unknown reason %q", s.Truncated))
	}
	return nil
}

// ValidateConnectionID checks [a-z0-9]([a-z0-9-]*[a-z0-9])?, max 64.
func ValidateConnectionID(field, id string) error {
	return validateSlug(field, id, maxConnectionIDLength)
}

// ValidateProvider checks a provider name; same alphabet as connection IDs.
func ValidateProvider(field, provider string) error {
	return validateSlug(field, provider, maxProviderLength)
}

// ValidateConfigKey checks [a-z][a-z0-9]*, max 32.
func ValidateConfigKey(field, key string) error {
	if key == "" {
		return NewValidationError(field, "is required")
	}
	if len(key) > maxConfigKeyLength {
		return NewValidationError(field, fmt.Sprintf("must be at most %d characters", maxConfigKeyLength))
	}
	for index, r := range key {
		if (r >= 'a' && r <= 'z') || (index > 0 && r >= '0' && r <= '9') {
			continue
		}
		return NewValidationError(field, fmt.Sprintf("%q must match [a-z][a-z0-9]*", key))
	}
	return nil
}

// ValidateStream checks [a-zA-Z0-9][a-zA-Z0-9._-]*, max 128.
func ValidateStream(field, stream string) error {
	if stream == "" {
		return NewValidationError(field, "is required")
	}
	if len(stream) > maxStreamLength {
		return NewValidationError(field, fmt.Sprintf("must be at most %d characters", maxStreamLength))
	}
	for index, r := range stream {
		if isASCIIAlnum(r) || (index > 0 && (r == '.' || r == '_' || r == '-')) {
			continue
		}
		return NewValidationError(field, fmt.Sprintf("%q must match [A-Za-z0-9][A-Za-z0-9._-]*", stream))
	}
	return nil
}

// ValidateSchedule accepts "", "manual", "@hourly", "@daily" and
// "@every <Go duration>" of at least one minute.
func ValidateSchedule(schedule string) error {
	switch schedule {
	case "", "manual", "@hourly", "@daily":
		return nil
	}
	raw, ok := strings.CutPrefix(schedule, "@every ")
	if !ok {
		return NewValidationError("connection.schedule", `must be "", "manual", "@hourly", "@daily" or "@every <duration>"`)
	}
	interval, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return NewValidationError("connection.schedule", "invalid duration: "+err.Error())
	}
	if interval < minScheduleInterval {
		return NewValidationError("connection.schedule", "interval must be at least 1m")
	}
	return nil
}

func validateSlug(field, value string, maxLength int) error {
	if value == "" {
		return NewValidationError(field, "is required")
	}
	if len(value) > maxLength {
		return NewValidationError(field, fmt.Sprintf("must be at most %d characters", maxLength))
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return NewValidationError(field, fmt.Sprintf("%q must use lowercase a-z, 0-9 and '-'", value))
	}
	if value[0] == '-' || value[len(value)-1] == '-' {
		return NewValidationError(field, fmt.Sprintf("%q must not start or end with '-'", value))
	}
	return nil
}

func validateLabel(field, value string, maxLength int) error {
	if len(value) > maxLength {
		return NewValidationError(field, fmt.Sprintf("must be at most %d bytes", maxLength))
	}
	if !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return NewValidationError(field, "must be valid UTF-8 without control characters")
	}
	return nil
}

func validateProjects(projects []string) error {
	seen := make(map[string]bool, len(projects))
	for _, project := range projects {
		if strings.TrimSpace(project) == "" || project != strings.TrimSpace(project) {
			return NewValidationError("connection.projects", "entries must be non-empty and trimmed")
		}
		if seen[project] {
			return NewValidationError("connection.projects", fmt.Sprintf("duplicate project %q", project))
		}
		seen[project] = true
	}
	return nil
}

func validateTags(field string, tags []string) error {
	for _, tag := range tags {
		if strings.TrimSpace(tag) == "" {
			return NewValidationError(field, "entries must not be empty")
		}
		if err := validateLabel(field, tag, maxTagLength); err != nil {
			return err
		}
	}
	return nil
}

// canonicalBaseURL lowercases scheme and host, drops default ports and a
// trailing slash. Empty stays empty.
func canonicalBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", NewValidationError("connection.baseUrl", "must be an absolute URL such as https://host or imaps://host:993")
	}
	if parsed.User != nil {
		return "", NewValidationError("connection.baseUrl", "must not contain credentials")
	}
	if parsed.Fragment != "" {
		return "", NewValidationError("connection.baseUrl", "must not contain a fragment")
	}
	if parsed.RawQuery != "" {
		// The catalogue is documented as secret-free and is committed to the
		// data repository; query parameters commonly carry tokens.
		return "", NewValidationError("connection.baseUrl", "must not contain a query string")
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := parsed.Port(); port != "" && port != defaultPort(scheme) {
		host += ":" + port
	}
	canonical := scheme + "://" + host + strings.TrimRight(parsed.EscapedPath(), "/")
	return canonical, nil
}

func defaultPort(scheme string) string {
	switch scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	case "imap":
		return "143"
	case "imaps":
		return "993"
	default:
		return ""
	}
}

func normalizeTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if value := collapseSpace(tag); value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func collapseSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func timeString(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

func boolString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}
