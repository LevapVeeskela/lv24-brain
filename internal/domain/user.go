package domain

import "fmt"

// Role is the server-side permission level of a user.
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleMember, RoleViewer:
		return true
	default:
		return false
	}
}

// ParseRole converts a textual role into a Role.
func ParseRole(value string) (Role, error) {
	role := Role(value)
	if !role.Valid() {
		return "", NewValidationError("user.role", fmt.Sprintf("unknown role %q", value))
	}
	return role, nil
}

// User is a brain principal.
type User struct {
	ID          string `json:"id"`
	Login       string `json:"login"`
	DisplayName string `json:"displayName,omitempty"`
	Role        Role   `json:"role"`
	AuthSource  string `json:"authSource,omitempty"`
}

// Validate checks required user fields.
func (u User) Validate() error {
	if err := ValidateRequired("user.id", u.ID); err != nil {
		return err
	}
	if err := ValidateRequired("user.login", u.Login); err != nil {
		return err
	}
	if !u.Role.Valid() {
		return NewValidationError("user.role", fmt.Sprintf("unknown role %q", u.Role))
	}
	return nil
}
