package main

import (
	"strings"
	"testing"
)

// The pointer is generated in the app layer; this checks the CLI wiring and
// that it stays below the state and handoff blocks.
func TestBrainOpenShowsTheIdeasPointer(t *testing.T) {
	cfgPath, _, _ := sessionIdeasFixture(t)
	addSessionIdeas(t, cfgPath, "Ship the parser", "Write the docs")
	code, stdout, stderr := runForTest(t, "open", "--config", cfgPath, "--project", "demo")
	if code != 0 {
		t.Fatalf("brain open: %s", stderr)
	}
	if !strings.Contains(stdout, "ideas: 2 open in this project · 2 open overall") {
		t.Fatalf("brain open has no ideas pointer:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Ship the parser") && !strings.Contains(stdout, "Write the docs") {
		t.Fatalf("brain open has no project item:\n%s", stdout)
	}
	if strings.Index(stdout, "ideas:") < strings.Index(stdout, "## Latest handoff") {
		t.Fatalf("pointer is above the handoff:\n%s", stdout)
	}
}
