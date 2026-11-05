package oidc

import (
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestMapRole(t *testing.T) {
	mapping := map[string]domain.Role{
		"brain-admins": domain.RoleAdmin,
		"brain-team":   domain.RoleMember,
		"brain-view":   domain.RoleViewer,
	}
	tests := []struct {
		name   string
		groups []string
		want   domain.Role
	}{
		{name: "no groups", groups: nil, want: domain.RoleViewer},
		{name: "member", groups: []string{"brain-team"}, want: domain.RoleMember},
		{name: "highest wins", groups: []string{"brain-team", "brain-admins"}, want: domain.RoleAdmin},
		{name: "unknown group", groups: []string{"other"}, want: domain.RoleViewer},
		{name: "trimmed", groups: []string{" brain-admins "}, want: domain.RoleAdmin},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MapRole(tt.groups, mapping, domain.RoleViewer); got != tt.want {
				t.Fatalf("MapRole(%v) = %q, want %q", tt.groups, got, tt.want)
			}
		})
	}
}

func TestPKCEChallengeLength(t *testing.T) {
	challenge := PKCEChallenge("verifier-value")
	if len(challenge) != 43 {
		t.Fatalf("challenge length = %d, want 43", len(challenge))
	}
	if challenge == PKCEChallenge("another-verifier") {
		t.Fatal("challenge must depend on the verifier")
	}
}
