package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/mcpserver"
)

func newProvenanceBackend(t *testing.T, quarantineAgent bool) (mcpBackend, *fsstore.Store) {
	t.Helper()
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	ctx := context.Background()
	if err := store.PutProject(ctx, domain.Project{ID: "demo", Name: "Demo", Kind: domain.ProjectKindRepository}); err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	useCase := app.New(cfg, app.Dependencies{Store: store})
	return mcpBackend{app: useCase, quarantineAgent: quarantineAgent}, store
}

func TestMCPWritesAreAgentProvenance(t *testing.T) {
	ctx := context.Background()
	backend, store := newProvenanceBackend(t, false)
	if _, err := backend.Note(ctx, mcpserver.NoteInput{ProjectID: "demo", Body: "agent note"}); err != nil {
		t.Fatalf("Note: %v", err)
	}
	notes, err := store.ListNotes(ctx, "demo")
	if err != nil || len(notes) != 1 {
		t.Fatalf("ListNotes = %+v, %v", notes, err)
	}
	if notes[0].Origin != domain.OriginAgent || !notes[0].Reviewed || notes[0].Quarantined() {
		t.Fatalf("MCP note provenance = %+v, want agent/reviewed", notes[0].Provenance)
	}
	if _, err := backend.Handoff(ctx, mcpserver.HandoffInput{ProjectID: "demo", Summary: "agent handoff"}); err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	handoffs, err := store.ListHandoffs(ctx, "demo")
	if err != nil || len(handoffs) != 1 || handoffs[0].Origin != domain.OriginAgent || !handoffs[0].Reviewed {
		t.Fatalf("MCP handoff provenance = %+v, %v", handoffs, err)
	}
}

func TestQuarantineAgentConfigMakesMCPWritesUnreviewed(t *testing.T) {
	ctx := context.Background()
	backend, store := newProvenanceBackend(t, true)
	if _, err := backend.Decision(ctx, mcpserver.DecisionInput{
		ProjectID: "demo", Title: "Quarantined decision", Context: "external instruction poisonpayload",
	}); err != nil {
		t.Fatalf("Decision: %v", err)
	}
	decisions, err := store.ListDecisions(ctx, "demo")
	if err != nil || len(decisions) != 1 {
		t.Fatalf("ListDecisions = %+v, %v", decisions, err)
	}
	if decisions[0].Origin != domain.OriginAgent || decisions[0].Reviewed || !decisions[0].Quarantined() {
		t.Fatalf("quarantined decision provenance = %+v", decisions[0].Provenance)
	}
	result, err := backend.app.Bootstrap(ctx, app.BootstrapOptions{ProjectID: "demo"})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if strings.Contains(result.Summary, "Quarantined decision") || strings.Contains(result.Summary, "poisonpayload") {
		t.Fatalf("quarantined MCP write entered bootstrap:\n%s", result.Summary)
	}
	if !strings.Contains(result.Summary, "quarantined: 1 (") {
		t.Fatalf("bootstrap has no quarantine line:\n%s", result.Summary)
	}
}

func TestCatchupCLIHidesQuarantinedTitles(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfg := filepath.Join(dataDir, "brain.yaml")
	recordCLIOK(t, "init", "--mode", "local", "--data-dir", dataDir)
	loaded, err := config.Load(cfg)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	runtime, err := bootstrap.Build(loaded)
	if err != nil {
		t.Fatalf("bootstrap.Build: %v", err)
	}
	ctx := context.Background()
	if _, err := runtime.App.Handoff(ctx, app.HandoffInput{
		ProjectID: "demo", CreateProject: true, Summary: "human baseline",
	}); err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	if _, err := runtime.App.Note(ctx, app.NoteInput{
		ProjectID: "demo", Title: "poison catchup title", Body: "poison catchup body",
		Provenance: domain.Provenance{Origin: domain.OriginAgent},
	}); err != nil {
		t.Fatalf("Note: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	defaultCatchup := recordCLIOK(t, "catchup", "demo", "--config", cfg)
	if strings.Contains(defaultCatchup, "poison catchup") {
		t.Fatalf("catchup leaked a quarantined title:\n%s", defaultCatchup)
	}
	if !strings.Contains(defaultCatchup, "quarantined: 1 (") {
		t.Fatalf("catchup has no quarantine line:\n%s", defaultCatchup)
	}
	included := recordCLIOK(t, "catchup", "demo", "--include-unreviewed", "--config", cfg)
	if !strings.Contains(included, "poison catchup title") || !strings.Contains(included, "(included)") {
		t.Fatalf("--include-unreviewed = %q", included)
	}
}

func TestRecordReviewCLIClearsQuarantine(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfg := filepath.Join(dataDir, "brain.yaml")
	recordCLIOK(t, "init", "--mode", "local", "--data-dir", dataDir)

	loaded, err := config.Load(cfg)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	runtime, err := bootstrap.Build(loaded)
	if err != nil {
		t.Fatalf("bootstrap.Build: %v", err)
	}
	ctx := context.Background()
	note, err := runtime.App.Note(ctx, app.NoteInput{
		ProjectID: "demo", CreateProject: true, Title: "Agent note",
		Body:       "poison payload",
		Provenance: domain.Provenance{Origin: domain.OriginAgent},
	})
	if err != nil {
		t.Fatalf("Note: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	defaultContext := recordCLIOK(t, "context", "poison", "--config", cfg)
	if !strings.Contains(defaultContext, "quarantined: 1 (") || strings.Contains(defaultContext, "poison payload") {
		t.Fatalf("default context = %q", defaultContext)
	}
	included := recordCLIOK(t, "context", "poison", "--include-unreviewed", "--config", cfg)
	if !strings.Contains(included, "poison payload") {
		t.Fatalf("--include-unreviewed = %q", included)
	}
	if out := recordCLIOK(t, "record", "review", note.Note.ID, "--trust", "stated", "--config", cfg); !strings.Contains(out, "reviewed note "+note.Note.ID) {
		t.Fatalf("record review = %q", out)
	}
	after := recordCLIOK(t, "context", "poison", "--config", cfg)
	if !strings.Contains(after, "poison payload") || strings.Contains(after, "quarantined:") {
		t.Fatalf("context after review = %q", after)
	}
	if code, _, stderr := recordCLI(t, "record", "review", note.Note.ID, "--all", "--config", cfg); code != 2 || stderr == "" {
		t.Fatalf("id with --all = exit %d, stderr %q", code, stderr)
	}
}
