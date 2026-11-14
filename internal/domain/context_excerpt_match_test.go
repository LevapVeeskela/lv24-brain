package domain

import "testing"

// TestContextGrantsMatchConnection covers the connection-level lookup the raw
// search gate uses: exact project and connection, independent of the kind
// filter, and never a nearby grant.
func TestContextGrantsMatchConnection(t *testing.T) {
	grants := ContextGrants{SchemaVersion: ContextAllowSchemaVersion, Grants: []ContextGrant{
		{Project: "p", Connection: "tracker", Kinds: []ItemKind{ItemIssue}},
		{Project: "p", Connection: "mail"},
	}}
	grants.Normalize()

	if grant, ok := grants.MatchConnection("p", "tracker"); !ok || len(grant.Kinds) != 1 || grant.Kinds[0] != ItemIssue {
		t.Fatalf("tracker = %+v, %v", grant, ok)
	}
	if grant, ok := grants.MatchConnection("p", "mail"); !ok || len(grant.Kinds) != 0 {
		t.Fatalf("mail = %+v, %v", grant, ok)
	}
	for _, pair := range [][2]string{{"other", "tracker"}, {"p", "gitlab"}, {"p", ""}} {
		if _, ok := grants.MatchConnection(pair[0], pair[1]); ok {
			t.Fatalf("MatchConnection(%q, %q) matched", pair[0], pair[1])
		}
	}
}
