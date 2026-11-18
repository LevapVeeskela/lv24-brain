package agentcontrol

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/journal"
	"github.com/levapveeskela/lv24-brain/internal/adapters/localapi"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func newWorkspaceFixture(t *testing.T) (*memoryFixture, *workspaceService) {
	t.Helper()
	fixture := newMemoryFixture(t)
	return fixture, newWorkspaceService(fixture.app, fixture.store.Root(), func() time.Time { return fixture.clock })
}

func TestWorkspaceMergePlanTokenApplyAndRollback(t *testing.T) {
	fixture, service := newWorkspaceFixture(t)
	ctx := context.Background()
	survivor := fixture.note(t, "Survivor", "keep this text", domain.Provenance{})
	duplicate := fixture.note(t, "Duplicate", "absorb this text", domain.Provenance{})

	plan, err := service.PlanMerge(ctx, localapi.MergePlanRequest{
		ProjectID: "demo", Survivor: survivor.ID, Duplicates: []string{duplicate.ID},
	})
	if err != nil {
		t.Fatalf("PlanMerge: %v", err)
	}
	if plan.PlanID == "" || plan.Survivor.ID != survivor.ID || len(plan.Duplicates) != 1 ||
		plan.Duplicates[0].ID != duplicate.ID || !plan.Changed {
		t.Fatalf("plan = %+v", plan)
	}

	result, err := service.ApplyMerge(ctx, localapi.MergeApplyRequest{PlanID: plan.PlanID})
	if err != nil {
		t.Fatalf("ApplyMerge: %v", err)
	}
	if !result.Applied || result.Manifest == "" || len(result.Files) == 0 {
		t.Fatalf("apply = %+v", result)
	}
	if result.PlanID == plan.PlanID {
		t.Fatal("apply must return a rollback token")
	}
	if allowed, err := service.ApplyMerge(ctx, localapi.MergeApplyRequest{
		PlanID: plan.PlanID, AllowedProjects: []string{"other"},
	}); err == nil || !errors.Is(err, domain.ErrForbidden) || allowed.Applied {
		t.Fatalf("out-of-scope apply = %+v, %v", allowed, err)
	}

	rolled, err := service.RollbackMerge(ctx, localapi.MergeRollbackRequest{PlanID: result.PlanID})
	if err != nil {
		t.Fatalf("RollbackMerge: %v", err)
	}
	if rolled.Manifest != result.Manifest || rolled.AlreadyRolledBack {
		t.Fatalf("rollback = %+v", rolled)
	}
	if _, err := fixture.app.GetRecord(ctx, "demo", duplicate.ID); err != nil {
		t.Fatalf("duplicate was not restored: %v", err)
	}
}

func TestWorkspaceMergeRefusesAStalePlanWithoutWriting(t *testing.T) {
	fixture, service := newWorkspaceFixture(t)
	ctx := context.Background()
	survivor := fixture.note(t, "Survivor", "keep this text", domain.Provenance{})
	duplicate := fixture.note(t, "Duplicate", "absorb this text", domain.Provenance{})
	plan, err := service.PlanMerge(ctx, localapi.MergePlanRequest{
		ProjectID: "demo", Survivor: survivor.ID, Duplicates: []string{duplicate.ID},
	})
	if err != nil {
		t.Fatalf("PlanMerge: %v", err)
	}
	body := "edited after the plan"
	if _, err := fixture.app.EditRecord(ctx, app.RecordEdit{
		ProjectID: "demo", ID: duplicate.ID, Body: &body,
	}); err != nil {
		t.Fatalf("EditRecord: %v", err)
	}

	result, err := service.ApplyMerge(ctx, localapi.MergeApplyRequest{PlanID: plan.PlanID})
	var coded interface{ Code() string }
	if err == nil || !errors.As(err, &coded) || coded.Code() != "stale_plan" || result.Applied || result.Manifest != "" {
		t.Fatalf("stale apply = %+v, %v", result, err)
	}
	if _, err := fixture.app.GetRecord(ctx, "demo", duplicate.ID); err != nil {
		t.Fatalf("stale apply wrote: %v", err)
	}
}

func TestWorkspaceDigestIsDryByDefaultAndWritesOnRequest(t *testing.T) {
	fixture, service := newWorkspaceFixture(t)
	ctx := context.Background()
	fixture.note(t, "New note", "digest activity", domain.Provenance{})

	report, err := service.Digest(ctx, localapi.DigestRequest{ProjectID: "demo"})
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if !report.DryRun || report.WrittenTo != "" || len(report.Projects) == 0 {
		t.Fatalf("dry digest = %+v", report)
	}
	if report.Projects[0].Changes == 0 {
		t.Fatalf("digest lost the activity: %+v", report.Projects[0])
	}

	written, err := service.Digest(ctx, localapi.DigestRequest{ProjectID: "demo", Write: true})
	if err != nil {
		t.Fatalf("Digest write: %v", err)
	}
	if written.DryRun || written.WrittenTo != "digests/2026-11-15.md" {
		t.Fatalf("written digest = %+v", written)
	}
	path := filepath.Join(fixture.store.Root(), filepath.FromSlash(written.WrittenTo))
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read digest: %v", err)
	}
	if !strings.Contains(string(content), "brain digest") || !strings.Contains(string(content), "demo") {
		t.Fatalf("digest content = %q", content)
	}
}

func TestWorkspaceAddProjectPathAndAdopt(t *testing.T) {
	fixture, service := newWorkspaceFixture(t)
	ctx := context.Background()
	checkout := t.TempDir()

	project, err := service.AddProjectPath(ctx, localapi.AddProjectPathRequest{ProjectID: "demo", Path: checkout})
	if err != nil {
		t.Fatalf("AddProjectPath: %v", err)
	}
	if project.ID != "demo" || len(project.Paths) != 1 || project.Paths[0] != checkout {
		t.Fatalf("project = %+v", project)
	}
	again, err := service.AddProjectPath(ctx, localapi.AddProjectPathRequest{ProjectID: "demo", Path: checkout})
	if err != nil || len(again.Paths) != 1 {
		t.Fatalf("idempotent add = %+v, %v", again, err)
	}
	if _, err := service.AddProjectPath(ctx, localapi.AddProjectPathRequest{ProjectID: "demo", Path: "relative/dir"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("relative path error = %v", err)
	}

	legacyPath := t.TempDir()
	if err := fixture.store.PutProject(ctx, domain.Project{
		ID: "legacy", Name: "Legacy", Kind: domain.ProjectKindOther, Paths: []string{legacyPath},
	}); err != nil {
		t.Fatalf("seed legacy project: %v", err)
	}
	report, err := service.AdoptLocalPaths(ctx, false)
	if err != nil {
		t.Fatalf("AdoptLocalPaths dry: %v", err)
	}
	if report.Applied || report.Stamped != 1 {
		t.Fatalf("dry adopt = %+v", report)
	}
	applied, err := service.AdoptLocalPaths(ctx, true)
	if err != nil || !applied.Applied || applied.Stamped != 1 {
		t.Fatalf("apply adopt = %+v, %v", applied, err)
	}
	stored, err := fixture.store.GetProject(ctx, "legacy")
	if err != nil || stored.PathProvenance[legacyPath] == "" {
		t.Fatalf("adopted provenance = %+v, %v", stored.PathProvenance, err)
	}
}

func TestWorkspaceJournalPageUsesLimitPlusOne(t *testing.T) {
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	activity := journal.New(dataDir)
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.User = "owner"
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	instance := app.New(cfg, app.Dependencies{
		Store: store, Journal: activity, Now: func() time.Time { return now },
	})
	ctx := context.Background()
	for _, title := range []string{"first", "second"} {
		if err := activity.Append(ctx, ports.JournalEntry{
			Time: now, Event: "note", User: "owner", Project: "demo", ID: title, Title: title,
		}); err != nil {
			t.Fatalf("append journal: %v", err)
		}
	}
	service := newWorkspaceService(instance, dataDir, func() time.Time { return now })
	page, err := service.Journal(ctx, localapi.JournalQuery{ProjectID: "demo", Limit: 1})
	if err != nil {
		t.Fatalf("Journal: %v", err)
	}
	if len(page.Items) != 1 || !page.HasMore || page.Limit != 1 {
		t.Fatalf("journal page = %+v", page)
	}
	if page.Items[0].Event != "note" || page.Items[0].Project != "demo" {
		t.Fatalf("journal item = %+v", page.Items[0])
	}
}
