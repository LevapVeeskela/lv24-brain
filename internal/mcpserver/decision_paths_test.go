package mcpserver

import (
	"slices"
	"testing"
)

func TestMCPServerToolsCallDecisionPaths(t *testing.T) {
	backend := &fakeBackend{}
	result := toolCall(t, backend, "brain_decision",
		`{"title":"Keep wire format","paths":["internal/wire/","cmd/brain/main.go"]}`)
	if result["isError"] != false {
		t.Fatalf("isError = %v", result["isError"])
	}
	if got := backend.decisions[0].Paths; !slices.Equal(got, []string{"internal/wire/", "cmd/brain/main.go"}) {
		t.Fatalf("paths = %q", got)
	}
}

func TestMCPServerDecisionSchemaHasPaths(t *testing.T) {
	responses := session(t, &fakeBackend{}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	for _, raw := range responses[0]["result"].(map[string]any)["tools"].([]any) {
		tool := raw.(map[string]any)
		if tool["name"] != "brain_decision" {
			continue
		}
		props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		if _, ok := props["paths"]; !ok {
			t.Fatal("brain_decision schema lacks paths")
		}
		return
	}
	t.Fatal("brain_decision tool not listed")
}
