package main

import (
	"strings"
	"testing"
)

// An npm-managed brain must not replace its own files; npm updates it.
func TestUpdateRefusesNPMManagedInstall(t *testing.T) {
	t.Setenv("BRAIN_INSTALL_SOURCE", "npm")
	code, _, stderr := runForTest(t, "update", "--yes")
	if code != 2 || !strings.Contains(stderr, "npm i -g lv24-brain@latest") {
		t.Fatalf("update --yes = %d %q", code, stderr)
	}
	if code, _, _ := runForTest(t, "update", "--rollback"); code != 2 {
		t.Fatalf("update --rollback = %d, want 2", code)
	}
	_, stdout, _ := runForTest(t, "version")
	if !strings.Contains(stdout, "installed via npm") {
		t.Fatalf("version = %q", stdout)
	}
}

func TestStandaloneInstallHasNoSource(t *testing.T) {
	t.Setenv("BRAIN_INSTALL_SOURCE", "")
	if got := installSource(); got != "" {
		t.Fatalf("installSource() = %q for a test binary outside node_modules", got)
	}
	if managerUpdateCommand("") != "" {
		t.Fatal("standalone install has an update command")
	}
}
