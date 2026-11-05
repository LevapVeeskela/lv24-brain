package templates

import (
	"strings"
	"testing"
)

func TestEmbeddedTemplates(t *testing.T) {
	for _, name := range []string{"state.md", "decision.md", "attempt.md", "handoff.md", "AGENTS.md"} {
		raw, err := FS.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", name, err)
		}
		if !strings.Contains(string(raw), "{{") {
			t.Fatalf("%s has no template placeholders", name)
		}
	}
}
