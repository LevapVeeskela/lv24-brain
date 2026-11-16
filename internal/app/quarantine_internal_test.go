package app

import (
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestQuarantineReasonSeparatesUnknownOrigins(t *testing.T) {
	cases := []struct {
		name       string
		provenance domain.Provenance
		want       string
	}{
		{"unreviewed agent", domain.Provenance{Origin: domain.OriginAgent, Reviewed: false}, QuarantineReasonUnreviewed},
		{"unknown origin", domain.Provenance{Origin: domain.Origin("weird"), Reviewed: true}, QuarantineReasonUnknownOrigin},
		{"human default", domain.Provenance{}, QuarantineReasonUnreviewed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quarantineReason(tc.provenance); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}
