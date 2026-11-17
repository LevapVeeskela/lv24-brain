package app

import (
	"context"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/config"
)

// fake assembles a synthetic credential at runtime from fragments so that no
// complete credential-shaped literal appears in source (avoids secret scanners).
func fake(parts ...string) string { return strings.Join(parts, "") }

func newTestApp(t *testing.T) *App {
	t.Helper()
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	return New(cfg, Dependencies{Store: store})
}

func containsRedaction(text string) bool {
	return strings.Contains(text, redactedMarker)
}

func TestRedactSecrets(t *testing.T) {
	cases := []struct {
		in, keep string
	}{
		{fake("token: ", "ghp_", "0123456789abcdefghijklmnopqrstuvwxyzAB", " used"), "used"},
		{fake("pat ", "github_pat_", "11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOP", " done"), "done"},
		{fake("gitlab ", "glpat-", "abcdefghij0123456789", " ok"), "ok"},
		{fake("OPENAI ", "sk-", "proj-abcdefghijklmnopqrstuvwxyz0123", " end"), "end"},
		{fake("key ", "sk-", "ant-api03-abcdefghijklmnopqrstuvwxyz012345", " end"), "end"},
		{fake("slack ", "xoxb-", "1234567890-abcdefghij", " end"), "end"},
		{fake("aws ", "AKIA", "ABCDEFGHIJKLMNOP", " end"), "end"},
		{fake("jwt ", "eyJ", "hbGciOiJIUzI1NiJ9", ".", "eyJzdWIiOiIxMjM0NTY3ODkwIn0", ".", "dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", " end"), "end"},
		{fake("clone https://alice:", "s3cr3t", "Passw0rd", "@github.com/acme/x.git end"), "https://alice:[redacted]@github.com/acme/x.git"},
		{fake("DB_PASSWORD=", "hunter2hunter2", " next"), "DB_PASSWORD=[redacted] next"},
		{fake(`"api_key": "abcd`, `1234efgh5678"`), `"api_key": "[redacted]"`},
		{fake("-----BEGIN OPENSSH PRIVATE", " KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY----- after"), "after"},
	}
	for _, tc := range cases {
		got, count := Redact(tc.in)
		if count == 0 || !containsRedaction(got) || !strings.Contains(got, tc.keep) {
			t.Errorf("Redact(%q) = %q (%d)", tc.in, got, count)
		}
	}
	// Ordinary text is untouched, including words like "token" without a value
	// and short values.
	for _, clean := range []string{
		"Rotate the GitHub token monthly; see docs.",
		"password: yes",
		"Decision: keep sk-learn models in Python",
		"https://github.com/acme/x.git and git@github.com:acme/x.git",
	} {
		if got, count := Redact(clean); count != 0 || got != clean {
			t.Errorf("Redact(%q) = %q (%d), want unchanged", clean, got, count)
		}
	}
}

// Records never store a secret an agent pasted into a note or handoff.
func TestWritesRedactSecrets(t *testing.T) {
	ctx := context.Background()
	a := newTestApp(t)
	note, err := a.Note(ctx, NoteInput{ProjectID: "p", Title: "Deploy", Body: fake("used GH token ", "ghp_", "0123456789abcdefghijklmnopqrstuvwxyzAB"), CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(note.Note.Body, "ghp_") || !containsRedaction(note.Note.Body) {
		t.Fatalf("note body = %q", note.Note.Body)
	}
	handoff, err := a.Handoff(ctx, HandoffInput{ProjectID: "p", Summary: fake("[auto] set DB_PASSWORD=", "hunter2hunter2", " on staging")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(handoff.Handoff.Summary, "hunter2") {
		t.Fatalf("handoff summary = %q", handoff.Handoff.Summary)
	}
	alternatives := []string{fake("keep key ", "AKIA", "ABCDEFGHIJKLMNOP", " in env")}
	if _, err := a.Decision(ctx, DecisionInput{ProjectID: "p", Title: "Secrets", Alternatives: alternatives}); err != nil {
		t.Fatal(err)
	}
	if alternatives[0] != fake("keep key ", "AKIA", "ABCDEFGHIJKLMNOP", " in env") {
		t.Fatal("caller's slice was modified")
	}
}
