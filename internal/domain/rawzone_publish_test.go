package domain

import "testing"

// TestRawZonePublishRequiresExplicitShared: raw items and checkpoints are
// machine-local unless the connection itself says `publish: shared`, while the
// wider "may derived data be shared?" question keeps the descriptor fallback.
func TestRawZonePublishRequiresExplicitShared(t *testing.T) {
	openSource := SourceDescriptor{Provider: "github", DefaultPublish: PublishShared}
	privateSource := SourceDescriptor{Provider: "imap", DefaultPublish: PublishLocalOnly}
	cases := []struct {
		name       string
		connection Connection
		descriptor SourceDescriptor
		want       PublishPolicy
	}{
		{"unset with shared descriptor", Connection{}, openSource, PublishLocalOnly},
		{"unset with local descriptor", Connection{}, privateSource, PublishLocalOnly},
		{"explicit local-only", Connection{Publish: PublishLocalOnly}, openSource, PublishLocalOnly},
		{"explicit shared", Connection{Publish: PublishShared}, openSource, PublishShared},
		{"explicit shared over a local descriptor", Connection{Publish: PublishShared}, privateSource, PublishShared},
	}
	for _, tc := range cases {
		if got := tc.connection.RawZonePublish(); got != tc.want {
			t.Errorf("%s: RawZonePublish = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := (Connection{}).EffectivePublish(openSource); got != PublishShared {
		t.Fatalf("EffectivePublish = %q, want the descriptor default %q", got, PublishShared)
	}
	if got := (Connection{Publish: PublishLocalOnly}).EffectivePublish(openSource); got != PublishLocalOnly {
		t.Fatalf("EffectivePublish override = %q", got)
	}
}
