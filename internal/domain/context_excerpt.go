package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Context-allow policy (Phase 4.2). A grant is the only way raw connector
// items may enter brain context; without a matching grant the raw zone is
// default-deny. Grants never carry secrets and are stored in the data
// directory as context-allow.json.
const (
	ContextAllowSchemaVersion = 1

	// MaxContextExcerpts bounds how many raw excerpts one context includes.
	MaxContextExcerpts = 5
	// MaxContextExcerptRunes bounds one excerpt body.
	MaxContextExcerptRunes = 600
	// MaxContextExcerptTotalRunes bounds all excerpt bodies together.
	MaxContextExcerptTotalRunes = 2000
)

// ContextGrant allows raw items of one connection and project into task
// context. Empty Kinds allows every item kind. MaxAgeDays zero disables the
// age filter. MaxExcerpts zero selects MaxContextExcerpts.
type ContextGrant struct {
	Project     string     `json:"project"`
	Connection  string     `json:"connection"`
	Kinds       []ItemKind `json:"kinds,omitempty"`
	MaxAgeDays  int        `json:"maxAgeDays,omitempty"`
	MaxExcerpts int        `json:"maxExcerpts,omitempty"`
}

// ContextGrants is the context-allow.json document.
type ContextGrants struct {
	SchemaVersion int            `json:"schemaVersion"`
	Grants        []ContextGrant `json:"grants,omitempty"`
}

// Normalize applies grant defaults in place.
func (g *ContextGrants) Normalize() {
	if g.SchemaVersion == 0 {
		g.SchemaVersion = ContextAllowSchemaVersion
	}
	for i := range g.Grants {
		grant := &g.Grants[i]
		grant.Project = strings.TrimSpace(grant.Project)
		grant.Connection = strings.TrimSpace(grant.Connection)
		if grant.MaxExcerpts == 0 {
			grant.MaxExcerpts = MaxContextExcerpts
		}
		kinds := make([]ItemKind, 0, len(grant.Kinds))
		for _, kind := range grant.Kinds {
			kind = ItemKind(strings.ToLower(strings.TrimSpace(string(kind))))
			if kind == "" || slices.Contains(kinds, kind) {
				continue
			}
			kinds = append(kinds, kind)
		}
		grant.Kinds = kinds
	}
}

// Validate checks the whole document. Validation is strict: a typo must fail
// instead of silently widening or disabling a grant.
func (g ContextGrants) Validate() error {
	if g.SchemaVersion != ContextAllowSchemaVersion {
		return NewValidationError("contextAllow.schemaVersion",
			fmt.Sprintf("must be %d", ContextAllowSchemaVersion))
	}
	if len(g.Grants) > 100 {
		return NewValidationError("contextAllow.grants", "must contain at most 100 grants")
	}
	seen := map[string]bool{}
	for i, grant := range g.Grants {
		field := fmt.Sprintf("contextAllow.grants[%d]", i)
		if strings.TrimSpace(grant.Project) == "" {
			return NewValidationError(field+".project", "is required")
		}
		if len(grant.Project) > 128 {
			return NewValidationError(field+".project", "must be at most 128 bytes")
		}
		if err := ValidateConnectionID(field+".connection", grant.Connection); err != nil {
			return err
		}
		key := grant.Project + "\x00" + grant.Connection
		if seen[key] {
			return NewValidationError(field, "duplicates another grant for the same project and connection")
		}
		seen[key] = true
		for j, kind := range grant.Kinds {
			if !kind.Valid() {
				return NewValidationError(fmt.Sprintf("%s.kinds[%d]", field, j),
					fmt.Sprintf("unknown item kind %q", kind))
			}
		}
		if grant.MaxAgeDays < 0 || grant.MaxAgeDays > 3650 {
			return NewValidationError(field+".maxAgeDays", "must be between 0 and 3650")
		}
		if grant.MaxExcerpts < 1 || grant.MaxExcerpts > MaxContextExcerpts {
			return NewValidationError(field+".maxExcerpts",
				fmt.Sprintf("must be between 1 and %d", MaxContextExcerpts))
		}
	}
	return nil
}

// Match returns the first grant for projectID and connectionID that allows
// kind. It never matches a different project or connection.
func (g ContextGrants) Match(projectID, connectionID string, kind ItemKind) (ContextGrant, bool) {
	for _, grant := range g.Grants {
		if grant.Project != projectID || grant.Connection != connectionID {
			continue
		}
		if len(grant.Kinds) > 0 && !slices.Contains(grant.Kinds, kind) {
			continue
		}
		return grant, true
	}
	return ContextGrant{}, false
}

// AllowsAge reports whether at passes the grant age filter. A zero at or a
// zero MaxAgeDays keeps the item.
func (g ContextGrant) AllowsAge(at, now time.Time) bool {
	if g.MaxAgeDays <= 0 || at.IsZero() {
		return true
	}
	return !at.Before(now.AddDate(0, 0, -g.MaxAgeDays))
}
