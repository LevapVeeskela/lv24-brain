package app_test

import (
	"context"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// staticGrants is a fixed context-allow policy for search tests.
type staticGrants struct {
	grants domain.ContextGrants
	err    error
}

var _ ports.ContextGrantStore = staticGrants{}

func (s staticGrants) ContextGrants(context.Context) (domain.ContextGrants, error) {
	return s.grants, s.err
}

// scopeGrants authorizes the raw connections the index fixtures use. Every
// other connection (and every project mismatch) stays default-deny.
func scopeGrants() staticGrants {
	grants := domain.ContextGrants{
		SchemaVersion: domain.ContextAllowSchemaVersion,
		Grants: []domain.ContextGrant{
			{Project: "p1", Connection: "tracker"},
			{Project: "p1", Connection: "dropme"},
			{Project: "p1", Connection: "keepme"},
			{Project: "connection:keepme", Connection: "keepme"},
		},
	}
	grants.Normalize()
	return staticGrants{grants: grants}
}

// TestScopeGrantsAcceptEveryFixtureGrant fails loudly when the shared fixture
// grants drift away from the connections itemindex_test indexes.
func TestScopeGrantsAcceptEveryFixtureGrant(t *testing.T) {
	grants := scopeGrants().grants
	if err := grants.Validate(); err != nil {
		t.Fatalf("fixture grants invalid: %v", err)
	}
	for _, pair := range [][2]string{
		{"p1", "tracker"}, {"p1", "dropme"}, {"p1", "keepme"}, {"connection:keepme", "keepme"},
	} {
		if _, ok := grants.Match(pair[0], pair[1], domain.ItemIssue); !ok {
			t.Fatalf("fixture grant %v missing", pair)
		}
	}
}
