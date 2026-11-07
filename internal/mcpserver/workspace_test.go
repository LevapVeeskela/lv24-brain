package mcpserver

import (
	"context"
	"strings"
	"testing"
)

type workspaceFake struct {
	fakeBackend
}

func (w *workspaceFake) Resolve(context.Context, string) (Target, error) {
	return Target{}, &WorkspaceOverview{Text: "# Company Brain workspace: /ws\n- alpha\n- beta\n", Message: "/ws is a workspace"}
}

// An agent opened at a workspace root gets the project list, not an error.
func TestOpenAtWorkspaceRootReturnsOverview(t *testing.T) {
	got := callTool(t, &workspaceFake{}, "brain_open", `{}`)
	if got["isError"] == true {
		t.Fatalf("brain_open at a workspace root failed: %+v", got)
	}
	structured, _ := got["structuredContent"].(map[string]any)
	if structured["workspace"] != true || !strings.Contains(structured["overview"].(string), "- beta") {
		t.Fatalf("structured = %+v", structured)
	}
}
