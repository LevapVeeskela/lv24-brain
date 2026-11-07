package fsstore

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestDecisionPathsRoundTripAndUpdateKeepsThem(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	decision := editTestDecision(project.ID, "dec-paths", "Keep wire format")
	decision.Paths = []string{"internal/wire/", "cmd/brain/main.go"}
	if _, err := store.AppendDecision(ctx, decision); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	path := onlyFile(t, filepath.Join(store.Root(), "projects", project.ID, "decisions"))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "## Paths\n\n- internal/wire/\n- cmd/brain/main.go\n") {
		t.Fatalf("file has no Paths list:\n%s", raw)
	}

	before, err := readDecisionFile(path)
	if err != nil {
		t.Fatalf("readDecisionFile: %v", err)
	}
	if !reflect.DeepEqual(before.Paths, decision.Paths) {
		t.Fatalf("paths after read = %q, want %q", before.Paths, decision.Paths)
	}

	updated := before
	updated.Status = domain.DecisionAccepted
	if err := store.UpdateDecision(ctx, updated); err != nil {
		t.Fatalf("UpdateDecision: %v", err)
	}
	after, err := readDecisionFile(path)
	if err != nil {
		t.Fatalf("readDecisionFile after update: %v", err)
	}
	if !reflect.DeepEqual(after.Paths, decision.Paths) || after.Status != domain.DecisionAccepted {
		t.Fatalf("after update paths=%q status=%q", after.Paths, after.Status)
	}
}

func TestDecisionWithoutPathsRendersEmptySection(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	if _, err := store.AppendDecision(ctx, editTestDecision(project.ID, "dec-none", "No paths")); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	raw, err := os.ReadFile(onlyFile(t, filepath.Join(store.Root(), "projects", project.ID, "decisions")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "## Paths\n\n_None recorded._") {
		t.Fatalf("missing empty Paths section:\n%s", raw)
	}
	read, err := readDecisionFile(onlyFile(t, filepath.Join(store.Root(), "projects", project.ID, "decisions")))
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Paths) != 0 {
		t.Fatalf("paths = %q, want none", read.Paths)
	}
}
