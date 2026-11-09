package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

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
			if existing.Identity() == conn.Identity() {
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
