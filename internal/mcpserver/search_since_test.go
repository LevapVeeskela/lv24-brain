package mcpserver

import "testing"

func TestMCPServerToolsCallSearchPassesSince(t *testing.T) {
	backend := &fakeBackend{}
	toolCall(t, backend, "brain_search", `{"query":"cache","since":" 7d "}`)
	if backend.search.Since != "7d" {
		t.Fatalf("Since = %q, want 7d", backend.search.Since)
	}
}
