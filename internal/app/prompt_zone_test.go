package app_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

type promptFixture struct {
	app   *app.App
	store *fsstore.Store
	clock time.Time
}

func newPromptFixture(t *testing.T, enabled bool, retentionDays *int) *promptFixture {
	t.Helper()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	index, err := sqliteindex.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("sqliteindex.Open: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.DataDir = dataDir
	cfg.Prompts.Enabled = enabled
	cfg.Prompts.RetentionDays = retentionDays
	deps := app.Dependencies{Store: store, Index: index, Now: func() time.Time { return clock }}
	return &promptFixture{app: app.New(cfg, deps), store: store, clock: clock}
}

func intPointer(value int) *int { return &value }

func TestPromptZoneRefusesWhenDisabled(t *testing.T) {
	fixture := newPromptFixture(t, false, nil)
	_, err := fixture.app.SavePrompt(context.Background(), app.PromptInput{ProjectID: "demo", Text: "secret"})
	if err == nil || !strings.Contains(err.Error(), "prompts.enabled") {
		t.Fatalf("SavePrompt disabled error = %v", err)
	}
	_, err = fixture.app.ListPrompts(context.Background(), app.PromptListOptions{ProjectID: "demo"})
	if err == nil || !strings.Contains(err.Error(), "prompts.enabled") {
		t.Fatalf("ListPrompts disabled error = %v", err)
	}
	doctor, err := fixture.app.Doctor(context.Background(), app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	check := promptDoctorCheck(doctor, "prompts")
	if check == nil || !strings.Contains(check.Message, "disabled") {
		t.Fatalf("doctor prompts check = %+v", check)
	}
}

func TestPromptZoneEnabledIsLocalOnlyAndUnsearchable(t *testing.T) {
	fixture := newPromptFixture(t, true, nil)
	if _, err := fixture.app.Note(context.Background(), app.NoteInput{
		ProjectID: "demo", CreateProject: true, Title: "Records", Body: "ordinary record text",
	}); err != nil {
		t.Fatalf("Note: %v", err)
	}
	before, err := fixture.store.CurrentRevision(context.Background())
	if err != nil {
		t.Fatalf("CurrentRevision: %v", err)
	}
	if _, err := fixture.app.SavePrompt(context.Background(), app.PromptInput{
		ProjectID: "demo", Text: "SUPERSECRETPROMPT", Session: "ses-42",
	}); err != nil {
		t.Fatalf("SavePrompt: %v", err)
	}
	prompts, err := fixture.app.ListPrompts(context.Background(), app.PromptListOptions{ProjectID: "demo"})
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	if len(prompts) != 1 || prompts[0].Text != "SUPERSECRETPROMPT" || prompts[0].Session != "ses-42" {
		t.Fatalf("prompts = %+v", prompts)
	}
	after, err := fixture.store.CurrentRevision(context.Background())
	if err != nil {
		t.Fatalf("CurrentRevision after: %v", err)
	}
	if before.Value != after.Value {
		t.Fatalf("a prompt changed the data revision: %s -> %s", before.Value, after.Value)
	}
	if _, err := fixture.app.Reindex(context.Background(), app.IndexOptions{}); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	hits, err := fixture.app.SearchWith(context.Background(), app.SearchOptions{Text: "SUPERSECRETPROMPT", ProjectID: "demo"})
	if err != nil {
		t.Fatalf("SearchWith: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("prompt was indexed: %+v", hits)
	}
	records, err := fixture.app.ListRecords(context.Background(), app.ListRecordsOptions{ProjectID: "demo"})
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %+v, %v", records, err)
	}
	doctor, err := fixture.app.Doctor(context.Background(), app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	check := promptDoctorCheck(doctor, "prompts")
	if check == nil || !strings.Contains(check.Message, "local-only") || !strings.Contains(check.Message, "retention: 30 days") {
		t.Fatalf("doctor prompts check = %+v", check)
	}
}

func TestPromptRetentionIsReportedNotEnforced(t *testing.T) {
	fixture := newPromptFixture(t, true, intPointer(1))
	ctx := context.Background()
	if err := fixture.store.PutProject(ctx, domain.Project{ID: "demo", Name: "Demo", Kind: domain.ProjectKindOther}); err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	old := fixture.clock.Add(-72 * time.Hour)
	if err := fixture.store.AppendPrompt(ctx, domain.Prompt{ProjectID: "demo", At: old, Text: "old prompt"}); err != nil {
		t.Fatalf("AppendPrompt: %v", err)
	}
	if _, err := fixture.app.SavePrompt(ctx, app.PromptInput{ProjectID: "demo", Text: "new prompt"}); err != nil {
		t.Fatalf("SavePrompt: %v", err)
	}
	doctor, err := fixture.app.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	check := promptDoctorCheck(doctor, "prompts")
	if check == nil || !strings.Contains(check.Message, "1 prompt(s) older than the window") {
		t.Fatalf("doctor prompts check = %+v", check)
	}
	prompts, err := fixture.app.ListPrompts(ctx, app.PromptListOptions{ProjectID: "demo"})
	if err != nil || len(prompts) != 2 {
		t.Fatalf("retention deleted a prompt: %+v, %v", prompts, err)
	}
}

func TestPromptListWithoutProjectCoversEveryProject(t *testing.T) {
	fixture := newPromptFixture(t, true, nil)
	ctx := context.Background()
	for _, project := range []string{"alpha", "beta"} {
		if err := fixture.store.PutProject(ctx, domain.Project{ID: project, Name: project, Kind: domain.ProjectKindOther}); err != nil {
			t.Fatalf("PutProject: %v", err)
		}
		if _, err := fixture.app.SavePrompt(ctx, app.PromptInput{ProjectID: project, Text: "prompt for " + project}); err != nil {
			t.Fatalf("SavePrompt: %v", err)
		}
	}
	prompts, err := fixture.app.ListPrompts(ctx, app.PromptListOptions{})
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("prompts = %+v", prompts)
	}
}

func promptDoctorCheck(report app.DoctorReport, name string) *app.DoctorCheck {
	for i := range report.Checks {
		if report.Checks[i].Name == name {
			return &report.Checks[i]
		}
	}
	return nil
}
