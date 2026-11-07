package app

import (
	"context"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/config"
)

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
		{"token: ghp_0123456789abcdefghijklmnopqrstuvwxyzAB used", "used"},
		{"pat github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOP done", "done"},
		{"gitlab glpat-abcdefghij0123456789 ok", "ok"},
		{"OPENAI sk-proj-abcdefghijklmnopqrstuvwxyz0123 end", "end"},
		{"key sk-ant-api03-abcdefghijklmnopqrstuvwxyz012345 end", "end"},
		{"slack xoxb-1234567890-abcdefghij end", "end"},
		{"aws AKIAABCDEFGHIJKLMNOP end", "end"},
		{"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U end", "end"},
		{"clone https://alice:s3cr3tPassw0rd@github.com/acme/x.git end", "https://alice:[redacted]@github.com/acme/x.git"},
		{"DB_PASSWORD=hunter2hunter2 next", "DB_PASSWORD=[redacted] next"},
		{`"api_key": "abcd1234efgh5678"`, `"api_key": "[redacted]"`},
		{"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY----- after", "after"},
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
	note, err := a.Note(ctx, NoteInput{ProjectID: "p", Title: "Deploy", Body: "used GH token ghp_0123456789abcdefghijklmnopqrstuvwxyzAB", CreateProject: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(note.Note.Body, "ghp_") || !containsRedaction(note.Note.Body) {
		t.Fatalf("note body = %q", note.Note.Body)
	}
	handoff, err := a.Handoff(ctx, HandoffInput{ProjectID: "p", Summary: "[auto] set DB_PASSWORD=hunter2hunter2 on staging"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(handoff.Handoff.Summary, "hunter2") {
		t.Fatalf("handoff summary = %q", handoff.Handoff.Summary)
	}
	alternatives := []string{"keep key AKIAABCDEFGHIJKLMNOP in env"}
	if _, err := a.Decision(ctx, DecisionInput{ProjectID: "p", Title: "Secrets", Alternatives: alternatives}); err != nil {
		t.Fatal(err)
	}
	if alternatives[0] != "keep key AKIAABCDEFGHIJKLMNOP in env" {
		t.Fatal("caller's slice was modified")
	}
}
