package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestReviewDefaultsResolveToDocumentedWindows(t *testing.T) {
	windows := BrainConfig{}.EffectiveReviewWindows()
	cases := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"decision", windows.Decision, domain.DefaultReviewDecision},
		{"attempt", windows.Attempt, domain.DefaultReviewAttempt},
		{"note", windows.Note, domain.DefaultReviewNote},
		{"handoff", windows.Handoff, domain.DefaultReviewHandoff},
		{"state", windows.State, domain.DefaultReviewState},
		{"proposal", windows.Proposal, domain.DefaultReviewProposal},
	}
	for _, test := range cases {
		if test.got != test.want {
			t.Fatalf("%s window = %s, want %s", test.name, test.got, test.want)
		}
	}
}

func TestReviewDefaultsOverridePerKind(t *testing.T) {
	cfg := Default()
	cfg.DataDir = t.TempDir()
	cfg.Review = ReviewConfig{Defaults: ReviewDefaults{Decision: "30d", Note: "48h"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	windows := cfg.EffectiveReviewWindows()
	if windows.Decision != 30*24*time.Hour {
		t.Fatalf("decision window = %s", windows.Decision)
	}
	if windows.Note != 48*time.Hour {
		t.Fatalf("note window = %s", windows.Note)
	}
	if windows.Attempt != domain.DefaultReviewAttempt {
		t.Fatalf("attempt window = %s, want the default", windows.Attempt)
	}
}

func TestReviewDefaultsRejectInvalidWindows(t *testing.T) {
	for _, value := range []string{"0d", "-1d", "next month", "999999999999d"} {
		t.Run(value, func(t *testing.T) {
			cfg := Default()
			cfg.DataDir = t.TempDir()
			cfg.Review = ReviewConfig{Defaults: ReviewDefaults{Note: value}}
			err := cfg.Validate()
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Validate(%q) error = %v, want validation", value, err)
			}
			if !strings.Contains(err.Error(), "config.review.defaults.note") {
				t.Fatalf("error %q does not name the key", err)
			}
		})
	}
}

func TestReviewBlockOmittedFromSavedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brain.yaml")
	cfg := Default()
	cfg.DataDir = t.TempDir()
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "review:") {
		t.Fatalf("unconfigured review block was written:\n%s", raw)
	}
}

func TestReviewBlockRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brain.yaml")
	cfg := Default()
	cfg.DataDir = t.TempDir()
	cfg.Review = ReviewConfig{Defaults: ReviewDefaults{Decision: "45d"}}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := loaded.EffectiveReviewWindows().Decision; got != 45*24*time.Hour {
		t.Fatalf("decision window after round-trip = %s", got)
	}
}
