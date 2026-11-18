package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// LegacyConnection is a discovered legacy target. ID is assigned by migration;
// LegacySecrets maps descriptor secret fields to old key names, never values.
type LegacyConnection struct {
	Connection    domain.Connection
	LegacySecrets map[string]string
}

// LegacyCredentialMigration reports names and status only. Status is one of
// not-copied, copied, missing, or already-set. No credential value is retained.
type LegacyCredentialMigration struct {
	ConnectionID string `json:"connectionId"`
	Field        string `json:"field"`
	LegacyKey    string `json:"legacyKey"`
	Status       string `json:"status"`
}

// ConnectionMigrationReport includes progress even if a later operation fails.
// Existing identities are reported but their configuration is never rewritten.
type ConnectionMigrationReport struct {
	Created     []domain.Connection         `json:"created"`
	Existing    []domain.Connection         `json:"existing"`
	Credentials []LegacyCredentialMigration `json:"credentials"`
}

// ConnectionMigrator imports discovered targets without changing legacy runs.
// Discover belongs to the composition root (registry, mail file, env, remotes).
// CopyLegacySecrets is deliberately opt-in. Re-running it fills missing fields
// even for existing connections, but never overwrites a connection credential.
type ConnectionMigrator struct {
	Connections       ports.ConnectionStore
	Credentials       ports.CredentialStore
	LegacySecrets     ports.SecretStore
	Discover          func(context.Context) ([]LegacyConnection, error)
	CopyLegacySecrets bool
}

// MigrateLegacyConnectors groups by canonical provider/endpoint/account, checks
// every candidate against Builtin descriptors before writing, then creates only
// missing identities. Credential copies are separate, resumable operations.
func (m ConnectionMigrator) MigrateLegacyConnectors(ctx context.Context) (ConnectionMigrationReport, error) {
	report := ConnectionMigrationReport{}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if m.Connections == nil || m.Discover == nil {
		return report, domain.NewValidationError("migration", "connections and discovery are required")
	}
	if m.CopyLegacySecrets && (m.Credentials == nil || m.LegacySecrets == nil) {
		return report, domain.NewValidationError("migration.credentials", "both stores are required to copy legacy secrets")
	}
	seeds, err := m.Discover(ctx)
	if err != nil {
		return report, err
	}
	candidates, err := mergeLegacyConnections(seeds)
	if err != nil {
		return report, err
	}
	for _, candidate := range candidates {
		connection, created, err := m.createLegacyConnection(ctx, candidate.Connection)
		if err != nil {
			return report, err
		}
		if created {
			report.Created = append(report.Created, connection)
		} else {
			report.Existing = append(report.Existing, connection)
		}
		fields := make([]string, 0, len(candidate.LegacySecrets))
		for field := range candidate.LegacySecrets {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		for _, field := range fields {
			ref := LegacyCredentialMigration{ConnectionID: connection.ID, Field: field, LegacyKey: candidate.LegacySecrets[field], Status: "not-copied"}
			if m.CopyLegacySecrets {
				ref.Status, err = m.copyLegacyCredential(ctx, ref)
				if err != nil {
					return report, err
				}
			}
			report.Credentials = append(report.Credentials, ref)
		}
	}
	return report, nil
}

// IdentityMigration statuses. A dry run only reports; apply writes the secret
// first, verifies the read-back, then rewrites the catalogue.
const (
	IdentityStatusWouldMove  = "would-move"
	IdentityStatusMoved      = "moved"
	IdentityStatusMigrated   = "already-migrated"
	IdentityStatusMissing    = "missing-secret"
	IdentityStatusUnreadable = "unreadable-secret"
	IdentityStatusMismatched = "mismatched-secret"
)

// IdentityMigrationEntry is one connection account considered by the opt-in
// identity migration. Key is the credential-store name, never a value.
type IdentityMigrationEntry struct {
	ConnectionID string `json:"connectionId"`
	Field        string `json:"field"`
	Key          string `json:"key"`
	Status       string `json:"status"`
}

// IdentityMigrationReport is the dry-run or apply result. Entries always
// covers every connection with a stored account.
type IdentityMigrationReport struct {
	Applied bool                     `json:"applied"`
	Entries []IdentityMigrationEntry `json:"entries"`
}

// HasProblems reports markers that need an operator: a missing, unreadable or
// mismatched secret. Pending raw accounts are not a problem, only work.
func (r IdentityMigrationReport) HasProblems() bool {
	for _, entry := range r.Entries {
		switch entry.Status {
		case IdentityStatusMissing, IdentityStatusUnreadable, IdentityStatusMismatched:
			return true
		}
	}
	return false
}

// ConnectionIdentityMigrator moves raw identity values (the account and the
// provider's declared identity settings) into the secret store and replaces
// them with markers in the catalogue. Migration is opt-in: a dry run performs
// reads only, and a raw value remains a valid fallback forever.
type ConnectionIdentityMigrator struct {
	Connections ports.ConnectionStore
	Credentials ports.CredentialStore
}

// Migrate reports raw identity values and, when apply is true, moves each one.
// Every secret is written and read back before the catalogue row is rewritten,
// so a failed step never leaves a marker without a secret. Re-running is
// idempotent, and entries are reported per field.
func (m ConnectionIdentityMigrator) Migrate(ctx context.Context, apply bool) (IdentityMigrationReport, error) {
	report := IdentityMigrationReport{Applied: apply, Entries: []IdentityMigrationEntry{}}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if m.Connections == nil {
		return report, domain.NewValidationError("identityMigration.connections", "is required")
	}
	if apply && m.Credentials == nil {
		return report, domain.NewValidationError("identityMigration.credentials", "are required to store connection identity values")
	}
	connections, err := m.Connections.List(ctx)
	if err != nil {
		return report, err
	}
	for _, connection := range connections {
		fields := connectorbuild.IdentityFields(connection)
		if len(fields) == 0 {
			continue
		}
		indexes := make(map[string]int, len(fields))
		raw := make(map[string]string, len(fields))
		for _, field := range fields {
			key, err := domain.CredentialKey(connection.ID, field)
			if err != nil {
				return report, err
			}
			entry := IdentityMigrationEntry{ConnectionID: connection.ID, Field: field, Key: key}
			if marker, ok := connectorbuild.IdentityMarkerFor(connection, field); ok {
				entry.Status = m.markerStatus(ctx, connection, field, marker)
			} else {
				entry.Status = IdentityStatusWouldMove
				raw[field] = identityValue(connection, field)
			}
			indexes[field] = len(report.Entries)
			report.Entries = append(report.Entries, entry)
		}
		if !apply || len(raw) == 0 {
			continue
		}
		protected, err := connectorbuild.ProtectIdentity(ctx, connection, m.Credentials)
		if err != nil {
			return report, err
		}
		if err := m.persist(ctx, connection, protected, raw); err != nil {
			return report, err
		}
		for field := range raw {
			report.Entries[indexes[field]].Status = IdentityStatusMoved
		}
	}
	return report, nil
}

// persist writes a protected connection. The account is part of
// Connection.Identity(), and the store refuses to change an identity through
// Update or Patch, so a row whose account moved is replaced: check the current
// values, delete, create, and restore the previous row if the create fails.
// Settings are mutable, so a settings-only migration uses Patch.
func (m ConnectionIdentityMigrator) persist(ctx context.Context, before, after domain.Connection, raw map[string]string) error {
	if after.Account != before.Account {
		return m.replaceAccount(ctx, before, after, raw)
	}
	_, err := m.Connections.Patch(ctx, before.ID, func(current *domain.Connection) error {
		if err := unchangedIdentityValues(*current, raw); err != nil {
			return err
		}
		if current.Settings == nil {
			current.Settings = map[string]string{}
		}
		for field := range raw {
			current.Settings[field] = after.Settings[field]
		}
		return nil
	})
	return err
}

func (m ConnectionIdentityMigrator) replaceAccount(ctx context.Context, before, after domain.Connection, raw map[string]string) error {
	if after.ID != before.ID {
		return domain.NewValidationError("identityMigration.id", "must not change")
	}
	current, err := m.Connections.Get(ctx, before.ID)
	if err != nil {
		return err
	}
	if err := unchangedIdentityValues(current, raw); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	if err := m.Connections.Delete(ctx, before.ID); err != nil {
		return err
	}
	if err := m.Connections.Create(ctx, after); err != nil {
		if restoreErr := m.Connections.Create(ctx, before); restoreErr != nil {
			return fmt.Errorf("identity migration: rewriting connection %q failed and the previous entry could not be restored", before.ID)
		}
		return err
	}
	return nil
}

// unchangedIdentityValues guards a rewrite against a concurrent edit: every
// value observed by the dry run must still be stored.
func unchangedIdentityValues(current domain.Connection, raw map[string]string) error {
	for field, value := range raw {
		if identityValue(current, field) != value {
			return fmt.Errorf("%w: connection %q field %q changed during identity migration", domain.ErrConflict, current.ID, field)
		}
	}
	return nil
}

func (m ConnectionIdentityMigrator) markerStatus(ctx context.Context, connection domain.Connection, field string, marker connectorbuild.IdentityMarker) string {
	if m.Credentials == nil {
		return IdentityStatusUnreadable
	}
	value, err := m.Credentials.Get(ctx, connection.ID, field)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return IdentityStatusMissing
	case err != nil:
		return IdentityStatusUnreadable
	case strings.TrimSpace(value) == "":
		return IdentityStatusMissing
	case !marker.Matches(value):
		return IdentityStatusMismatched
	default:
		return IdentityStatusMigrated
	}
}

func identityValue(connection domain.Connection, field string) string {
	if field == connectorbuild.AccountField {
		return connection.Account
	}
	return connection.Settings[field]
}

// FindConnectionByIdentity returns the connection that already owns candidate's
// provider, endpoint and account. Raw legacy accounts and migrated markers
// compare as the same identity, so a create or a legacy import cannot introduce
// a duplicate mailbox across the opt-in migration boundary.
func FindConnectionByIdentity(ctx context.Context, store ports.ConnectionStore, candidate domain.Connection) (domain.Connection, bool, error) {
	if store == nil {
		return domain.Connection{}, false, domain.NewValidationError("connections.store", "is required")
	}
	connections, err := store.List(ctx)
	if err != nil {
		return domain.Connection{}, false, err
	}
	want := connectorbuild.IdentityKey(candidate)
	for _, existing := range connections {
		if connectorbuild.IdentityKey(existing) == want {
			return existing, true, nil
		}
	}
	return domain.Connection{}, false, nil
}

func mergeLegacyConnections(seeds []LegacyConnection) ([]LegacyConnection, error) {
	registry := connectorbuild.Builtin()
	groups := map[string]LegacyConnection{}
	for _, seed := range seeds {
		conn := seed.Connection
		conn.ID = legacyConnectionSlug(conn)
		entry, err := registry.Lookup(conn.Provider)
		if err != nil {
			return nil, err
		}
		if err := entry.Descriptor.ValidateConnection(conn); err != nil {
			return nil, err
		}
		for field, key := range seed.LegacySecrets {
			config, ok := entry.Descriptor.Field(field)
			if !ok || !config.Secret || strings.TrimSpace(key) == "" {
				return nil, domain.NewValidationError("migration.legacySecrets", "must name a descriptor secret field and a non-empty legacy key")
			}
		}
		identity := conn.Identity()
		group, exists := groups[identity]
		if !exists {
			group = LegacyConnection{Connection: conn, LegacySecrets: map[string]string{}}
			group.Connection.Projects = append([]string(nil), conn.Projects...)
			group.Connection.Settings = map[string]string{}
		}
		group.Connection.Projects = mergeLegacyList(group.Connection.Projects, conn.Projects, false)
		for key, value := range conn.Settings {
			prior := group.Connection.Settings[key]
			if key == "repos" || key == "projects" || key == "folders" {
				// Repository identity is case insensitive; IMAP folder names are not.
				group.Connection.Settings[key] = strings.Join(mergeLegacyList(strings.Split(prior, ","), strings.Split(value, ","), key != "folders"), ",")
			} else {
				if exists && prior != value {
					return nil, domain.NewValidationError("migration.settings."+key, "conflicting values for one connection identity")
				}
				group.Connection.Settings[key] = value
			}
		}
		for field, key := range seed.LegacySecrets {
			if prior, ok := group.LegacySecrets[field]; ok && prior != key {
				return nil, domain.NewValidationError("migration.legacySecrets", "conflicting legacy keys for one identity")
			}
			group.LegacySecrets[field] = key
		}
		groups[identity] = group
	}
	identities := make([]string, 0, len(groups))
	for identity := range groups {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	result := make([]LegacyConnection, 0, len(groups))
	for _, identity := range identities {
		group := groups[identity]
		entry, err := registry.Lookup(group.Connection.Provider)
		if err != nil {
			return nil, err
		}
		if err := entry.Descriptor.ValidateConnection(group.Connection); err != nil {
			return nil, err
		}
		result = append(result, group)
	}
	return result, nil
}

func mergeLegacyList(a, b []string, fold bool) []string {
	values := append(append([]string(nil), a...), b...)
	sort.Strings(values)
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		key := value
		if fold {
			key = strings.ToLower(key)
		}
		if value != "" && !seen[key] {
			seen[key] = true
			result = append(result, value)
		}
	}
	return result
}

func (m ConnectionMigrator) createLegacyConnection(ctx context.Context, conn domain.Connection) (domain.Connection, bool, error) {
	base := conn.ID
	// The store serializes writes. Refresh on conflicts so concurrent migration
	// either observes the winner's identity or chooses another free slug.
	for attempt := 0; attempt < 1000; attempt++ {
		connections, err := m.Connections.List(ctx)
		if err != nil {
			return domain.Connection{}, false, err
		}
		ids := map[string]bool{}
		for _, existing := range connections {
			if connectorbuild.IdentityKey(existing) == connectorbuild.IdentityKey(conn) {
				return existing, false, nil
			}
			ids[existing.ID] = true
		}
		conn.ID = base
		for suffix := 2; ids[conn.ID]; suffix++ {
			tail := "-" + strconv.Itoa(suffix)
			conn.ID = strings.TrimRight(base[:min(len(base), 64-len(tail))], "-") + tail
		}
		if err := m.Connections.Create(ctx, conn); err != nil {
			if errors.Is(err, domain.ErrConflict) {
				continue
			}
			return domain.Connection{}, false, err
		}
		return conn, true, nil
	}
	return domain.Connection{}, false, fmt.Errorf("%w: migration kept racing with catalogue updates", domain.ErrConflict)
}

func (m ConnectionMigrator) copyLegacyCredential(ctx context.Context, ref LegacyCredentialMigration) (string, error) {
	_, err := m.Credentials.Get(ctx, ref.ConnectionID, ref.Field)
	if err == nil {
		return "already-set", nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return "", safeMigrationCredentialError(ctx, "read destination", ref)
	}
	value, err := m.LegacySecrets.Get(ctx, ref.LegacyKey)
	if errors.Is(err, domain.ErrNotFound) || (err == nil && strings.TrimSpace(value) == "") {
		return "missing", nil
	}
	if err != nil {
		return "", safeMigrationCredentialError(ctx, "read legacy", ref)
	}
	if err := m.Credentials.Set(ctx, ref.ConnectionID, ref.Field, value); err != nil {
		return "", safeMigrationCredentialError(ctx, "write destination", ref)
	}
	return "copied", nil
}

func safeMigrationCredentialError(ctx context.Context, operation string, ref LegacyCredentialMigration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Backend errors can contain process output. Do not attach them (or values)
	// to a report that a CLI/UI may print.
	return fmt.Errorf("migration: %s credential failed for connection %q field %q", operation, ref.ConnectionID, ref.Field)
}

// Stable codes of a failed connection check, shared by the CLI exit report and
// the local API error envelope. Clients branch on the code, not the message.
const (
	ConnectionCodeCredentialMissing = "credential_missing"
	ConnectionCodeInvalid           = "connection_invalid"
	ConnectionCodeUnsupported       = "connection_unsupported"
	ConnectionCodeCheckFailed       = "connection_check_failed"
	ConnectionCodeUnavailable       = "connection_unavailable"
)

// ConnectionFailure is the sanitized, stable description of a connection
// failure. Message is fit for an end user; Cause is the redacted provider
// detail and may be empty. Neither field ever contains a credential.
type ConnectionFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Cause   string `json:"cause,omitempty"`
}

// SanitizeConnectorError turns a connector error into one redacted line fit
// for CLI output, run listings, logs and API messages. It removes provider
// credentials, collapses control characters and whitespace, and bounds the
// length so a dump or a multi-line process error cannot flood a terminal.
func SanitizeConnectorError(text string) string {
	const maxRunes = 300
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text, _ = RedactIngest(text)
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > maxRunes {
		text = string(runes[:maxRunes]) + "..."
	}
	return text
}

// DescribeConnectionCheckError classifies a failed Source.Check. Providers
// report a missing secret as domain.ErrNotFound, so that carries the specific
// credential_missing code instead of a generic failure.
func DescribeConnectionCheckError(err error) ConnectionFailure {
	failure := ConnectionFailure{Code: ConnectionCodeCheckFailed, Message: "the provider check failed"}
	switch {
	case errors.Is(err, domain.ErrNotFound):
		failure.Code = ConnectionCodeCredentialMissing
		failure.Message = "the connection is missing a required credential"
	case errors.Is(err, domain.ErrValidation):
		failure.Code = ConnectionCodeInvalid
		failure.Message = "the connection configuration is invalid"
	case errors.Is(err, domain.ErrUnsupported):
		failure.Code = ConnectionCodeUnsupported
		failure.Message = "the connection provider is not supported"
	}
	if err != nil {
		failure.Cause = SanitizeConnectorError(err.Error())
		if failure.Cause == failure.Message {
			failure.Cause = ""
		}
	}
	return failure
}

// DescribeConnectionBuildError classifies a failure to construct the source
// before Check runs. An unknown provider is unsupported, not missing data.
func DescribeConnectionBuildError(err error) ConnectionFailure {
	failure := ConnectionFailure{Code: ConnectionCodeUnavailable, Message: "the connection source could not be prepared"}
	switch {
	case errors.Is(err, domain.ErrValidation):
		failure.Code = ConnectionCodeInvalid
		failure.Message = "the connection configuration is invalid"
	case errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrUnsupported):
		failure.Code = ConnectionCodeUnsupported
		failure.Message = "the connection provider is not supported"
	}
	if err != nil {
		failure.Cause = SanitizeConnectorError(err.Error())
		if failure.Cause == failure.Message {
			failure.Cause = ""
		}
	}
	return failure
}

// ConnectionRunSummary is the newest recorded run of one connection, reduced
// to what a list view needs. Status is ok, failed or running.
type ConnectionRunSummary struct {
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
}

// ConnectionRunSummaries reads run history read-only and keeps the newest run
// per connection. Legacy runs without a ConnectionID are ignored. Errors in
// run records are sanitized before they can reach a terminal or API client.
func ConnectionRunSummaries(ctx context.Context, runs ports.ConnectorRunStore, limit int) (map[string]ConnectionRunSummary, error) {
	summaries := map[string]ConnectionRunSummary{}
	if runs == nil {
		return summaries, nil
	}
	if limit <= 0 {
		limit = 200
	}
	stored, err := runs.ListConnectorRuns(ctx, "", limit)
	if err != nil {
		return nil, err
	}
	for _, run := range stored {
		if run.ConnectionID == "" {
			continue
		}
		if _, seen := summaries[run.ConnectionID]; seen {
			continue
		}
		summaries[run.ConnectionID] = ConnectionRunSummary{
			StartedAt:  run.StartedAt,
			FinishedAt: run.FinishedAt,
			Status:     connectionRunOutcome(run.Status),
			Error:      SanitizeConnectorError(run.Error),
		}
	}
	return summaries, nil
}

func connectionRunOutcome(status domain.ConnectorStatus) string {
	switch status {
	case domain.ConnectorFailed:
		return "failed"
	case domain.ConnectorRunning:
		return "running"
	case domain.ConnectorSuccess, domain.ConnectorPartial:
		return "ok"
	default:
		return strings.TrimSpace(string(status))
	}
}

func legacyConnectionSlug(conn domain.Connection) string {
	name := conn.Provider
	endpoint, _ := url.Parse(conn.BaseURL)
	if conn.Provider == "imap" {
		name += "-" + conn.Account
	} else if !(conn.Provider == "github" && (endpoint == nil || endpoint.Hostname() == "" || strings.EqualFold(endpoint.Hostname(), "api.github.com"))) {
		if endpoint != nil && endpoint.Host != "" {
			name += "-" + endpoint.Host
		}
	}
	var slug strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			slug.WriteRune(r)
		} else if slug.Len() > 0 && !strings.HasSuffix(slug.String(), "-") {
			slug.WriteByte('-')
		}
	}
	return strings.Trim(slug.String()[:min(slug.Len(), 64)], "-")
}
