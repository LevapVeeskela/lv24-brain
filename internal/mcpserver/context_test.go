package mcpserver

import (
	"strings"
	"testing"
)

func TestMCPServerContextToolPassesArguments(t *testing.T) {
	// Arrange
	backend := &fakeBackend{}

	// Act
	result := toolCall(t, backend, "brain_context",
		`{"query":" cache layer ","project":"beta","budget":900,"kinds":["decision","note"],"since":"7d"}`)

	// Assert
	if result["isError"] != false {
		t.Fatalf("isError = %v: %v", result["isError"], result["content"])
	}
	in := backend.contextIn
	if in.ProjectID != "beta" || in.Query != "cache layer" || in.Budget != 900 || in.Since != "7d" ||
		len(in.Kinds) != 2 || in.Kinds[0] != "decision" || in.Kinds[1] != "note" {
		t.Fatalf("context input = %+v", in)
	}
	content := result["content"].([]any)[0].(map[string]any)
	if !strings.Contains(content["text"].(string), "beta — context") {
		t.Fatalf("text = %v", content["text"])
	}
	obj := structured(t, result)
	records := obj["records"].([]any)
	if obj["project_id"] != "beta" || len(records) != 1 || records[0].(map[string]any)["id"] != "decision-alpha-1" {
		t.Fatalf("structured = %v", obj)
	}
	for _, list := range []string{"failed_attempts", "findings", "skipped"} {
		if got, ok := obj[list].([]any); !ok || len(got) != 0 {
			t.Fatalf("%s = %v, want []", list, obj[list])
		}
	}
}

func TestMCPServerContextToolDefaultsToWorkingDirectory(t *testing.T) {
	// Arrange
	backend := &fakeBackend{}

	// Act
	toolCall(t, backend, "brain_context", `{"query":"cache"}`)

	// Assert
	if backend.contextIn.ProjectID != "alpha" {
		t.Fatalf("project = %q, want the working directory project", backend.contextIn.ProjectID)
	}
}

func TestMCPServerContextToolRejectsBadArguments(t *testing.T) {
	for name, args := range map[string]string{
		"missing query":   `{}`,
		"blank query":     `{"query":"  "}`,
		"negative budget": `{"query":"x","budget":-1}`,
		"unknown field":   `{"query":"x","limit":3}`,
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			backend := &fakeBackend{}

			// Act
			result := toolCall(t, backend, "brain_context", args)

			// Assert
			if result["isError"] != true {
				t.Fatalf("isError = %v, want true", result["isError"])
			}
			if backend.contextIn.Query != "" {
				t.Fatalf("backend was called: %+v", backend.contextIn)
			}
		})
	}
}
