package server

import (
	"errors"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// TestResolvePublishPolicy covers the server-side raw-zone admission rule:
// a shared-default provider becomes explicitly shared, while any local-only
// connection needs the explicit admin opt-in.
func TestResolvePublishPolicy(t *testing.T) {
	runner := &connectorRunner{}
	cases := []struct {
		name       string
		provider   string
		publish    domain.PublishPolicy
		allow      bool
		want       domain.PublishPolicy
		wantDenied bool
	}{
		{"github default shared", "github", "", false, domain.PublishShared, false},
		{"github explicit shared", "github", domain.PublishShared, false, domain.PublishShared, false},
		{"github explicit local-only needs opt-in", "github", domain.PublishLocalOnly, false, "", true},
		{"github local-only with opt-in", "github", domain.PublishLocalOnly, true, domain.PublishLocalOnly, false},
		{"gitlab default shared", "gitlab", "", false, domain.PublishShared, false},
		{"imap default local-only needs opt-in", "imap", "", false, "", true},
		{"imap local-only with opt-in", "imap", "", true, domain.PublishLocalOnly, false},
		{"imap explicit shared", "imap", domain.PublishShared, false, domain.PublishShared, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := domain.Connection{Provider: tc.provider, Publish: tc.publish}
			err := runner.resolvePublishPolicy(&conn, tc.allow)
			if tc.wantDenied {
				if !errors.Is(err, domain.ErrForbidden) {
					t.Fatalf("err = %v, want forbidden", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if conn.Publish != tc.want {
				t.Fatalf("publish = %q, want %q", conn.Publish, tc.want)
			}
		})
	}
}
