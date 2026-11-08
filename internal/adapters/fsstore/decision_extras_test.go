package fsstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Text an owner adds by hand outside the rendered sections survives a status
// change, and repeated updates do not duplicate it.
func TestUpdateDecisionKeepsHandWrittenText(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	original := editTestDecision(project.ID, "dec-hand", "Use SQLite")
	if _, err := store.AppendDecision(ctx, original); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(store.root, "projects", project.ID, decisionsDir, "*.md"))
	if len(files) != 1 {
		t.Fatalf("decision files = %v", files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(raw), "Status: proposed\n", "Status: proposed\n\nDiscussed with the team on Monday.\n", 1)
	text = strings.TrimRight(text, "\n") + "\n\n## Follow-up\n\nRevisit after the load test.\n"
	if err := os.WriteFile(files[0], []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, status := range []domain.DecisionStatus{domain.DecisionAccepted, domain.DecisionSuperseded} {
		decisions, err := store.ListDecisions(ctx, project.ID)
		if err != nil || len(decisions) != 1 {
			t.Fatalf("list = %+v, %v", decisions, err)
		}
		updated := decisions[0]
		updated.Status = status
		if err := store.UpdateDecision(ctx, updated); err != nil {
			t.Fatal(err)
		}
	}
	decisions, err := store.ListDecisions(ctx, project.ID)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("list = %+v, %v", decisions, err)
	}
	got := decisions[0]
	for _, want := range []string{"Discussed with the team on Monday.", "## Follow-up", "Revisit after the load test."} {
		if strings.Count(got.Context, want) != 1 {
			t.Fatalf("context after two updates = %q, want %q exactly once", got.Context, want)
		}
	}
	if got.Status != domain.DecisionSuperseded || len(got.Evidence) != 1 || len(got.Alternatives) != 1 {
		t.Fatalf("decision = %+v", got)
	}
}
