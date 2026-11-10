package domain

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

var proposalTestTime = time.Date(2026, 11, 9, 8, 30, 0, 0, time.UTC)

func validProposal() Proposal {
	proposal := Proposal{
		SchemaVersion: ProposalSchemaVersion,
		ProjectID:     "lv24-brain",
		Kind:          ProposalDecision,
		Rule:          "closed-issue",
		Confidence:    0.82,
		ConnectionID:  "gh-acme",
		ExternalID:    "acme/widgets#issue-7",
		Title:         "Adopt a retry budget",
		Body:          "The issue closes with a decision that needs to survive.",
		Sections: []ProposalSection{
			{Heading: "Context", Body: "Retries were unbounded."},
			{Heading: "Consequences", Body: "Cap retries at three."},
		},
		Citations: []ProposalCitation{{
			ConnectionID: "gh-acme",
			ItemKey:      "gh-acme/" + strings.Repeat("a", itemKeyHashLength),
			ExternalID:   "acme/widgets#issue-7",
			URL:          "https://github.test/acme/widgets/issues/7",
			Title:        "Unbounded retries",
			ObservedAt:   proposalTestTime,
		}},
		Status:    ProposalPending,
		CreatedAt: proposalTestTime,
		UpdatedAt: proposalTestTime,
	}
	proposal.ID = ProposalID(proposal.ProjectID, proposal.ConnectionID, proposal.ExternalID)
	proposal.PayloadHash = proposal.ComputePayloadHash()
	return proposal
}

func TestProposalIDIsDeterministicAndPathSafe(t *testing.T) {
	first := ProposalID("lv24-brain", "gh-acme", "acme/widgets#issue-7")
	second := ProposalID("lv24-brain", "gh-acme", "acme/widgets#issue-7")
	if first != second {
		t.Fatalf("ProposalID is not deterministic: %q vs %q", first, second)
	}
	if !strings.HasPrefix(first, "proposal-gh-acme-") || len(first) != len("proposal-gh-acme-")+proposalIDDigestLength {
		t.Fatalf("ProposalID = %q", first)
	}
	if err := ValidateProposalID("proposal.id", first); err != nil {
		t.Fatalf("ValidateProposalID: %v", err)
	}
	variants := map[string]string{
		"project":    ProposalID("lv24-other", "gh-acme", "acme/widgets#issue-7"),
		"connection": ProposalID("lv24-brain", "gh-other", "acme/widgets#issue-7"),
		"external":   ProposalID("lv24-brain", "gh-acme", "acme/widgets#issue-8"),
	}
	for name, variant := range variants {
		if variant == first {
			t.Fatalf("%s did not change the id", name)
		}
	}
	// "a/b" and "a-b" in different fields must not collide.
	if ProposalID("p", "a-b", "c") == ProposalID("p", "a", "b-c") {
		t.Fatal("NUL-separated id fields collided")
	}
	for _, bad := range []string{
		"",
		"proposal-gh-acme",
		"proposal-GH-acme-0123456789ab",
		"proposal-gh_acme-0123456789ab",
		"proposal-gh-acme-0123456789AB",
		"proposal-gh-acme-0123456789a",
		"proposal-gh-acme-0123456789abc",
		`proposal-gh-acme-0123456789ab/..`,
	} {
		if err := ValidateProposalID("proposal.id", bad); !errors.Is(err, ErrValidation) {
			t.Errorf("ValidateProposalID(%q) = %v, want validation error", bad, err)
		}
	}
}

func TestProposalPayloadHashExcludesStatusAndTimestamps(t *testing.T) {
	base := validProposal()
	promoted := base
	promoted.Status = ProposalPromoted
	promoted.PromotedRecordID = "decision-2026-11-09-adopt-a-retry-budget"
	promoted.PromotedAt = proposalTestTime.Add(2 * time.Hour)
	promoted.CreatedAt = proposalTestTime.Add(-time.Hour)
	promoted.UpdatedAt = proposalTestTime.Add(3 * time.Hour)
	promoted.Citations = append([]ProposalCitation(nil), base.Citations...)
	promoted.Citations[0].ObservedAt = proposalTestTime.Add(4 * time.Hour)

	if promoted.ComputePayloadHash() != base.PayloadHash {
		t.Fatalf("payload hash changed with status/timestamps: %s vs %s", promoted.ComputePayloadHash(), base.PayloadHash)
	}
	if err := promoted.Validate(); err != nil {
		t.Fatalf("promoted proposal validation: %v", err)
	}

	contentChanges := map[string]func(*Proposal){
		"title":       func(p *Proposal) { p.Title = "Adopt a different retry budget" },
		"body":        func(p *Proposal) { p.Body += " More detail." },
		"confidence":  func(p *Proposal) { p.Confidence = 0.5 },
		"rule":        func(p *Proposal) { p.Rule = "labeled-adr" },
		"kind":        func(p *Proposal) { p.Kind = ProposalNote },
		"section":     func(p *Proposal) { p.Sections[0].Body = "changed" },
		"section set": func(p *Proposal) { p.Sections = p.Sections[:1] },
		"citation":    func(p *Proposal) { p.Citations[0].URL = "https://github.test/acme/widgets/issues/8" },
	}
	for name, mutate := range contentChanges {
		t.Run(name, func(t *testing.T) {
			changed := validProposal()
			mutate(&changed)
			if changed.ComputePayloadHash() == base.PayloadHash {
				t.Fatalf("hash unchanged after %s change", name)
			}
		})
	}
}

func TestProposalHashTreatsUnsetSchemaAsCurrent(t *testing.T) {
	proposal := validProposal()
	proposal.SchemaVersion = 0
	unset := proposal.ComputePayloadHash()
	proposal.SchemaVersion = ProposalSchemaVersion
	if current := proposal.ComputePayloadHash(); current != unset {
		t.Fatalf("hash changed when Put normalizes the schema: %s vs %s", unset, current)
	}
}

// TestProposalHashTreatsNegativeZeroAsZero closes the roundtrip hole where
// Validate accepted -0.0, encoding dropped the sign, and the read-back hash
// mismatch rejected the stored proposal.
func TestProposalHashTreatsNegativeZeroAsZero(t *testing.T) {
	proposal := validProposal()
	proposal.Confidence = 0
	positive := proposal.ComputePayloadHash()
	proposal.Confidence = math.Copysign(0, -1)
	if negative := proposal.ComputePayloadHash(); negative != positive {
		t.Fatalf("negative zero hashed differently: %s vs %s", negative, positive)
	}
	proposal.PayloadHash = proposal.ComputePayloadHash()
	if err := proposal.Validate(); err != nil {
		t.Fatalf("negative zero proposal validation: %v", err)
	}
}

func TestProposalValidateAcceptsAndRejects(t *testing.T) {
	good := validProposal()
	if err := good.Validate(); err != nil {
		t.Fatalf("valid proposal: %v", err)
	}
	if got := good.StatusOrPending(); got != ProposalPending {
		t.Fatalf("StatusOrPending() = %q", got)
	}

	cases := map[string]func(*Proposal){
		"schema":              func(p *Proposal) { p.SchemaVersion = 99 },
		"missing project":     func(p *Proposal) { p.ProjectID = "" },
		"padded project":      func(p *Proposal) { p.ProjectID = " lv24-brain" },
		"bad id":              func(p *Proposal) { p.ID = "proposal-gh-acme-000000000000" },
		"bad kind":            func(p *Proposal) { p.Kind = "task" },
		"bad rule":            func(p *Proposal) { p.Rule = "Closed Issue" },
		"confidence low":      func(p *Proposal) { p.Confidence = -0.1 },
		"confidence high":     func(p *Proposal) { p.Confidence = 1.1 },
		"confidence NaN":      func(p *Proposal) { p.Confidence = math.NaN() },
		"missing connection":  func(p *Proposal) { p.ConnectionID = "" },
		"missing external":    func(p *Proposal) { p.ExternalID = "" },
		"missing title":       func(p *Proposal) { p.Title = "  " },
		"newline title":       func(p *Proposal) { p.Title = "one\ntwo" },
		"duplicate section":   func(p *Proposal) { p.Sections[1].Heading = "context" },
		"empty section":       func(p *Proposal) { p.Sections[0].Heading = " " },
		"hash mismatch":       func(p *Proposal) { p.PayloadHash = "sha256:" + strings.Repeat("0", 64) },
		"bad status":          func(p *Proposal) { p.Status = "accepted" },
		"promoted no record":  func(p *Proposal) { p.Status = ProposalPromoted },
		"record on pending":   func(p *Proposal) { p.PromotedRecordID = "decision-x" },
		"promoted at pending": func(p *Proposal) { p.PromotedAt = proposalTestTime },
		"updated before made": func(p *Proposal) { p.CreatedAt = proposalTestTime.Add(time.Hour) },
		"citation no identity": func(p *Proposal) {
			p.Citations = []ProposalCitation{{Title: "orphan"}}
		},
		"citation bad url": func(p *Proposal) {
			p.Citations = []ProposalCitation{{URL: "ftp://example.test/file"}}
		},
		"citation connection mismatch": func(p *Proposal) {
			p.Citations = []ProposalCitation{{
				ConnectionID: "gh-acme",
				ItemKey:      "gh-other/" + strings.Repeat("a", itemKeyHashLength),
			}}
		},
		"citation credentials": func(p *Proposal) {
			p.Citations = []ProposalCitation{{URL: "https://user:pass@example.test/file"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			proposal := validProposal()
			mutate(&proposal)
			if err := proposal.Validate(); !errors.Is(err, ErrValidation) {
				t.Fatalf("Validate() = %v, want validation error", err)
			}
		})
	}
}

func TestProposalJSONRoundTrip(t *testing.T) {
	proposal := validProposal()
	proposal.Status = ProposalPromoted
	proposal.PromotedRecordID = "decision-2026-11-09-adopt-a-retry-budget"
	proposal.PromotedAt = proposalTestTime.Add(time.Hour)
	raw, err := json.Marshal(proposal)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded Proposal
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.ID != proposal.ID || decoded.PayloadHash != proposal.PayloadHash || decoded.Status != proposal.Status {
		t.Fatalf("decoded proposal = %+v", decoded)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded Validate: %v", err)
	}
}
