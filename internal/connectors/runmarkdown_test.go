package connectors

import (
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestRenderRunMarkdown(t *testing.T) {
	started := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Minute)
	raw, err := RenderRunMarkdown(domain.ConnectorRun{
		ID:         "run-1",
		Source:     "github",
		StartedAt:  started,
		FinishedAt: &finished,
		Status:     domain.ConnectorPartial,
		Counts:     map[string]int{"repos": 1, "pulls": 2},
		Error:      "one repository failed",
	})
	if err != nil {
		t.Fatalf("RenderRunMarkdown: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		"kind: connector-run",
		"id: run-1",
		"source: github",
		"status: partial",
		"started_at: 2026-11-06T10:00:00Z",
		"finished_at: 2026-11-06T10:02:00Z",
		"# Connector run: github",
		"## Counts",
		"- pulls: 2",
		"- repos: 1",
		"## Error",
		"one repository failed",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("markdown missing %q:\n%s", want, text)
		}
	}
}

func TestDiscardSinkAcceptsDumps(t *testing.T) {
	if err := (DiscardSink{}).Write(t.Context(), "github/acme/widgets.md", []byte("content")); err != nil {
		t.Fatalf("Write: %v", err)
	}
}
