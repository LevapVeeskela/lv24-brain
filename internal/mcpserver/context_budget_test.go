package mcpserver

import (
	"strconv"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestMCPServerContextBudgetDefaultsAndLimits(t *testing.T) {
	// Arrange
	backend := &fakeBackend{}

	// Act: no budget asks for the default.
	toolCall(t, backend, "brain_context", `{"query":"cache","explain":true}`)

	// Assert
	if backend.contextIn.Budget != domain.DefaultContextBudgetTokens {
		t.Fatalf("default budget = %d, want %d", backend.contextIn.Budget, domain.DefaultContextBudgetTokens)
	}
	if !backend.contextIn.Explain {
		t.Fatal("explain was not passed to the backend")
	}
}

func TestMCPServerContextRejectsOversizedBudget(t *testing.T) {
	for name, args := range map[string]string{
		"negative":  `{"query":"x","budget":-1}`,
		"too large": `{"query":"x","budget":` + strconv.Itoa(domain.MaxContextBudgetTokens+1) + `}`,
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

func TestMCPServerContextAcceptsMaximumBudget(t *testing.T) {
	// Arrange
	backend := &fakeBackend{}

	// Act
	result := toolCall(t, backend, "brain_context", `{"query":"x","budget":`+strconv.Itoa(domain.MaxContextBudgetTokens)+`}`)

	// Assert
	if result["isError"] != false || backend.contextIn.Budget != domain.MaxContextBudgetTokens {
		t.Fatalf("isError = %v budget = %d", result["isError"], backend.contextIn.Budget)
	}
	obj := structured(t, result)
	for _, key := range []string{"sections", "truncated", "total_tokens", "quarantined_count"} {
		if _, ok := obj[key]; !ok {
			t.Fatalf("structured result lacks %q: %v", key, obj)
		}
	}
}
