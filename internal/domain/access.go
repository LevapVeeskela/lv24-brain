package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// ProjectRole is a user's role inside one restricted project (server mode).
// It never raises the user's global role: a global viewer stays read-only in
// every project, and only global admins bypass project membership.
type ProjectRole string

const (
	ProjectRoleViewer     ProjectRole = "viewer"
	ProjectRoleMember     ProjectRole = "member"
	ProjectRoleMaintainer ProjectRole = "maintainer"
)

// Valid reports whether r is a known project role.
func (r ProjectRole) Valid() bool {
	switch r {
	case ProjectRoleViewer, ProjectRoleMember, ProjectRoleMaintainer:
		return true
	default:
		return false
	}
}

// ParseProjectRole converts a textual project role.
func ParseProjectRole(value string) (ProjectRole, error) {
	role := ProjectRole(strings.TrimSpace(value))
	if !role.Valid() {
		return "", NewValidationError("member.role", fmt.Sprintf("unknown project role %q", value))
	}
	return role, nil
}

// ProjectMember grants one user a role in one project.
type ProjectMember struct {
	ProjectID string      `json:"projectId"`
	UserID    string      `json:"userId"`
	Login     string      `json:"login,omitempty"`
	Role      ProjectRole `json:"role"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
}

// Validate checks required membership fields.
func (m ProjectMember) Validate() error {
	if err := ValidateRequired("member.projectId", m.ProjectID); err != nil {
		return err
	}
	if err := ValidateRequired("member.userId", m.UserID); err != nil {
		return err
	}
	if !m.Role.Valid() {
		return NewValidationError("member.role", fmt.Sprintf("unknown project role %q", m.Role))
	}
	return nil
}

// TokenScope limits what an API token may do on top of its owner's role.
type TokenScope string

const (
	// TokenScopeRead allows read-only requests (GET).
	TokenScopeRead TokenScope = "read"
	// TokenScopeWrite allows knowledge, state and project writes; implies read.
	TokenScopeWrite TokenScope = "write"
	// TokenScopeAdmin allows admin-only endpoints (users, memberships, audit,
	// connector runs, project deletion); implies read.
	TokenScopeAdmin TokenScope = "admin"
)

// Valid reports whether s is a known token scope.
func (s TokenScope) Valid() bool {
	switch s {
	case TokenScopeRead, TokenScopeWrite, TokenScopeAdmin:
		return true
	default:
		return false
	}
}

// NormalizeTokenScopes validates, de-duplicates and sorts scopes. An empty
// result means "unscoped" (the token carries its full role).
func NormalizeTokenScopes(values []string) ([]TokenScope, error) {
	out := make([]TokenScope, 0, len(values))
	for _, raw := range values {
		scope := TokenScope(strings.ToLower(strings.TrimSpace(raw)))
		if scope == "" {
			continue
		}
		if !scope.Valid() {
			return nil, NewValidationError("token.scopes", fmt.Sprintf("unknown scope %q", raw))
		}
		if !slices.Contains(out, scope) {
			out = append(out, scope)
		}
	}
	slices.Sort(out)
	return out, nil
}
