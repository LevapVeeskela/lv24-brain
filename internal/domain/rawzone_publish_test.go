package domain

import "testing"

// TestResolveRawZonePolicy: an explicit setting always wins; without one a raw
// zone that git already tracks stays shared (grandfathered) so an upgrade never
// untracks published data, and a zone with nothing tracked is local-only.
func TestResolveRawZonePolicy(t *testing.T) {
	cases := []struct {
		name      string
		setting   PublishPolicy
		tracked   bool
		policy    PublishPolicy
		explicit  bool
		grandfath bool
	}{
		{"unset, untracked", "", false, PublishLocalOnly, false, false},
		{"unset, tracked", "", true, PublishShared, false, true},
		{"explicit local-only, untracked", PublishLocalOnly, false, PublishLocalOnly, true, false},
		{"explicit local-only, tracked", PublishLocalOnly, true, PublishLocalOnly, true, false},
		{"explicit shared, untracked", PublishShared, false, PublishShared, true, false},
		{"explicit shared, tracked", PublishShared, true, PublishShared, true, false},
		{"unknown setting counts as unset", PublishPolicy("public"), true, PublishShared, false, true},
	}
	for _, tc := range cases {
		resolution := ResolveRawZonePolicy(tc.setting, tc.tracked)
		if resolution.Policy != tc.policy || resolution.Explicit != tc.explicit || resolution.Grandfathered != tc.grandfath {
			t.Errorf("%s: %+v, want policy=%q explicit=%t grandfathered=%t",
				tc.name, resolution, tc.policy, tc.explicit, tc.grandfath)
		}
	}
}

// TestEffectivePublishKeepsDescriptorFallback: the wider "may derived data be
// shared?" question is unchanged, so promotion and server sensitivity keep
// working while the raw zone follows ResolveRawZonePolicy.
func TestEffectivePublishKeepsDescriptorFallback(t *testing.T) {
	openSource := SourceDescriptor{Provider: "github", DefaultPublish: PublishShared}
	if got := (Connection{}).EffectivePublish(openSource); got != PublishShared {
		t.Fatalf("EffectivePublish = %q, want the descriptor default %q", got, PublishShared)
	}
	if got := (Connection{Publish: PublishLocalOnly}).EffectivePublish(openSource); got != PublishLocalOnly {
		t.Fatalf("EffectivePublish override = %q", got)
	}
}
