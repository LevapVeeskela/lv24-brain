package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/mcpserver"
)

type contextJSONBudget struct {
	Data struct {
		Budget      int `json:"budget"`
		TotalTokens int `json:"total_tokens"`
		Sections    []struct {
			Name    string `json:"name"`
			Cap     int    `json:"cap"`
			Tokens  int    `json:"tokens"`
			Records []struct {
				ID     string  `json:"id"`
				Kind   string  `json:"kind"`
				Title  string  `json:"title"`
				Path   string  `json:"path"`
				Tokens int     `json:"tokens"`
				Score  float64 `json:"score"`
			} `json:"records"`
		} `json:"sections"`
		Truncated []struct {
			ID      string `json:"id"`
			Section string `json:"section"`
			Reason  string `json:"reason"`
		} `json:"truncated"`
		QuarantinedCount int      `json:"quarantined_count"`
		Explain          []string `json:"explain"`
		Summary          string   `json:"summary"`
	} `json:"data"`
}

func TestContextCommandBudgetJSON(t *testing.T) {
	// Arrange
	cfgPath := setupContextBrain(t)

	// Act
	code, stdout, stderr := runForTest(t, "context", "parser", "--config", cfgPath, "--project", "demo",
		"--budget", "6000", "--json", "--explain")

	// Assert
	if code != 0 {
		t.Fatalf("context --json: exit %d: %s", code, stderr)
	}
	var envelope contextJSONBudget
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	data := envelope.Data
	if data.Budget != 6000 || data.TotalTokens <= 0 || data.TotalTokens > 6000 {
		t.Fatalf("budget = %d total = %d", data.Budget, data.TotalTokens)
	}
	if len(data.Sections) == 0 || data.Sections[0].Name != "core" {
		t.Fatalf("sections = %+v, want core first", data.Sections)
	}
	sum := 0
	for _, section := range data.Sections {
		sum += section.Tokens
		if section.Cap <= 0 {
			t.Fatalf("%s cap = %d", section.Name, section.Cap)
		}
		for _, record := range section.Records {
			if record.ID == "" || record.Kind == "" || record.Title == "" || record.Path == "" || record.Tokens <= 0 {
				t.Fatalf("%s record = %+v", section.Name, record)
			}
		}
	}
	if sum != data.TotalTokens {
		t.Fatalf("section sum = %d, total = %d", sum, data.TotalTokens)
	}
	if len(data.Explain) == 0 || !strings.Contains(strings.Join(data.Explain, "\n"), "core:") {
		t.Fatalf("explain = %v", data.Explain)
	}
	if data.Summary == "" || data.QuarantinedCount != 0 {
		t.Fatalf("summary = %q quarantined = %d", data.Summary, data.QuarantinedCount)
	}
}

func TestContextCommandUsesConfiguredBudget(t *testing.T) {
	// Arrange
	cfgPath := setupContextBrain(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Context.Budget = 900
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	// Act
	code, stdout, stderr := runForTest(t, "context", "parser", "--config", cfgPath, "--project", "demo", "--json")

	// Assert
	if code != 0 {
		t.Fatalf("context: exit %d: %s", code, stderr)
	}
	var envelope contextJSONBudget
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if envelope.Data.Budget != 900 || envelope.Data.TotalTokens > 900 {
		t.Fatalf("budget = %d total = %d, want the configured 900", envelope.Data.Budget, envelope.Data.TotalTokens)
	}
}

func TestContextCommandRejectsOversizedBudget(t *testing.T) {
	cfgPath := setupContextBrain(t)
	if code, _, stderr := runForTest(t, "context", "x", "--config", cfgPath, "--budget", "32001"); code != 2 ||
		!strings.Contains(stderr, "at most 32000") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
	if code, _, stderr := runForTest(t, "open", "--config", cfgPath, "--budget", "32001"); code != 2 ||
		!strings.Contains(stderr, "at most 32000") {
		t.Fatalf("open exit %d stderr %q", code, stderr)
	}
}

func TestMCPContextBudgetEnforced(t *testing.T) {
	// Arrange
	cfgPath := setupContextBrain(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.Close() }()
	backend := mcpBackend{app: runtime.App}

	// Act
	result, err := backend.Context(context.Background(), mcpserver.ContextInput{
		ProjectID: "demo", Query: "parser", Budget: 1200,
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if result.Budget != 1200 || result.TotalTokens <= 0 || result.TotalTokens > 1200 {
		t.Fatalf("budget = %d total = %d", result.Budget, result.TotalTokens)
	}
	if len(result.Sections) == 0 || result.Sections[0].Name != "core" {
		t.Fatalf("sections = %+v", result.Sections)
	}
	if result.TotalTokens > result.Budget {
		t.Fatalf("total %d exceeds budget %d", result.TotalTokens, result.Budget)
	}
}
