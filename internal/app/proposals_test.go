package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type proposalFixture struct {
	app         *app.App
	store       *fsstore.Store
	connections *fsstore.ConnectionStore
	items       *fsstore.ItemStore
	clock       time.Time
}

func newProposalFixture(t *testing.T, connectionProjects []string) *proposalFixture {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	connections, err := fsstore.NewConnectionStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.User = "alice"
	a := app.New(cfg, app.Dependencies{
		Store:       store,
		Connections: connections,
		Items:       store.Items(),
		Proposals:   store,
		Now:         func() time.Time { return clock },
	})
	if _, err := a.SaveProject(ctx, app.ProjectInput{ID: "demo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SaveProject(ctx, app.ProjectInput{ID: "other"}); err != nil {
		t.Fatal(err)
	}
	connection := domain.Connection{ID: "gh", Provider: "github", Projects: append([]string(nil), connectionProjects...)}
	if err := connections.Create(ctx, connection); err != nil {
		t.Fatal(err)
	}
	return &proposalFixture{
		app:         a,
		store:       store,
		connections: connections,
		items:       store.Items(),
		clock:       clock,
	}
}

func (f *proposalFixture) add(t *testing.T, item domain.Item) domain.Item {
	t.Helper()
	if item.Ref.ConnectionID == "" {
		item.Ref.ConnectionID = "gh"
	}
	if item.ObservedAt.IsZero() {
		item.ObservedAt = f.clock
	}
	item.Hash = item.ComputeHash()
	if _, err := f.items.Upsert(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	return item
}

func fixtureItem(externalID string, kind domain.ItemKind, title string) domain.Item {
	return domain.Item{
		Ref:       domain.ItemRef{ExternalID: externalID},
		Kind:      kind,
		Title:     title,
		ProjectID: "demo",
		URL:       "https://example.test/items/" + externalID,
	}
}

func TestProposalDigestPreviewWindowAndRules(t *testing.T) {
	ctx := context.Background()
	fixture := newProposalFixture(t, []string{"demo"})

	closed := fixtureItem("issue-closed", domain.ItemIssue, "Serialize writes")
	closed.State = domain.ItemStateClosed
	closed.Labels = []string{"bug"}
	closed.Body = "The race is fixed by serializing writes."
	closed.UpdatedAt = fixture.clock.Add(-time.Hour)
	fixture.add(t, closed)

	decision := fixtureItem("issue-decision", domain.ItemIssue, "Adopt SQLite")
	decision.Labels = []string{"architecture", "rfc"}
	fixture.add(t, decision)

	incident := fixtureItem("issue-incident", domain.ItemIssue, "API outage")
	incident.Labels = []string{"incident", "production"}
	fixture.add(t, incident)

	merged := fixtureItem("pr-merged", domain.ItemPullRequest, "Merge parser fix")
	merged.State = domain.ItemStateMerged
	merged.Body = "Parser now handles nested arrays."
	fixture.add(t, merged)

	commit := fixtureItem("commit-1", domain.ItemCommit, "fix: parser")
	fixture.add(t, commit)

	old := fixtureItem("commit-old", domain.ItemCommit, "old commit")
	old.UpdatedAt = fixture.clock.AddDate(0, 0, -40)
	fixture.add(t, old)

	unmapped := fixtureItem("issue-unmapped", domain.ItemIssue, "Not mapped")
	unmapped.ProjectID = ""
	fixture.add(t, unmapped)

	otherProject := fixtureItem("issue-other", domain.ItemIssue, "Other project")
	otherProject.ProjectID = "other"
	fixture.add(t, otherProject)

	preview, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.ProjectID != "demo" || preview.ConnectionID != "gh" || preview.Write {
		t.Fatalf("preview = %+v", preview)
	}
	if preview.Counts.Scanned != 8 || preview.Counts.Created != 5 || preview.Counts.Skipped != 3 ||
		preview.Counts.Updated != 0 || preview.Counts.Unchanged != 0 || preview.Counts.Truncated {
		t.Fatalf("counts = %+v, want scanned 8, created 5, skipped 3", preview.Counts)
	}
	if preview.Since == nil || !preview.Since.Equal(fixture.clock.Add(-app.DefaultProposalDigestWindow)) {
		t.Fatalf("since = %+v", preview.Since)
	}
	if preview.Written() != 0 {
		t.Fatalf("preview wrote %d proposals", preview.Written())
	}
	wantRules := map[string]bool{
		app.ProposalRuleClosedIssue:   true,
		app.ProposalRuleDecisionLabel: true,
		app.ProposalRuleIncident:      true,
		app.ProposalRuleMergedChange:  true,
		app.ProposalRuleCommit:        true,
	}
	for _, entry := range preview.Entries {
		if !wantRules[entry.Proposal.Rule] {
			t.Fatalf("unexpected rule %q", entry.Proposal.Rule)
		}
		if entry.Proposal.ID != domain.ProposalID("demo", "gh", entry.Proposal.ExternalID) {
			t.Fatalf("id %q is not deterministic", entry.Proposal.ID)
		}
		if entry.Proposal.ProjectID != "demo" || entry.Proposal.StatusOrPending() != domain.ProposalPending {
			t.Fatalf("proposal = %+v", entry.Proposal)
		}
	}
	if list, err := fixture.store.List(ctx, "demo", portsProposalListAll()); err != nil || len(list) != 0 {
		t.Fatalf("preview persisted proposals: %d, %v", len(list), err)
	}

	all, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh", All: true})
	if err != nil {
		t.Fatal(err)
	}
	if all.Since != nil || !all.All || all.Counts.Created != 6 || all.Counts.Skipped != 2 {
		t.Fatalf("all = %+v", all)
	}

	limited, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if limited.Counts.Scanned != 3 || !limited.Counts.Truncated {
		t.Fatalf("limited counts = %+v", limited.Counts)
	}
	full, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if full.Counts.Truncated {
		t.Fatalf("full counts = %+v", full.Counts)
	}

	filtered, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh", Rules: []string{"commit"}})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Counts.Created != 1 || len(filtered.Entries) != 1 || filtered.Entries[0].Proposal.Rule != app.ProposalRuleCommit {
		t.Fatalf("filtered = %+v", filtered)
	}
	if _, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh", Rules: []string{"nope"}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown rule error = %v", err)
	}
	if _, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh", Write: true, DryRun: true}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("write+dry-run error = %v", err)
	}
	if _, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh", Limit: -1}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative limit error = %v", err)
	}
}

func TestProposalDigestWriteIsIdempotentAndSkipsTerminal(t *testing.T) {
	ctx := context.Background()
	fixture := newProposalFixture(t, []string{"demo"})

	issue := fixtureItem("issue-closed", domain.ItemIssue, "Serialize writes")
	issue.State = domain.ItemStateClosed
	issue.Body = "First body."
	fixture.add(t, issue)
	fixture.add(t, func() domain.Item {
		item := fixtureItem("pr-merged", domain.ItemPullRequest, "Merge parser fix")
		item.State = domain.ItemStateMerged
		return item
	}())
	fixture.add(t, fixtureItem("commit-1", domain.ItemCommit, "fix: parser"))

	written, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh", Write: true})
	if err != nil {
		t.Fatal(err)
	}
	if written.Counts.Created != 3 || written.Written() != 3 {
		t.Fatalf("first write = %+v", written.Counts)
	}
	if list, err := fixture.store.List(ctx, "demo", portsProposalListAll()); err != nil || len(list) != 3 {
		t.Fatalf("stored proposals = %d, %v", len(list), err)
	}

	again, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh", Write: true})
	if err != nil {
		t.Fatal(err)
	}
	if again.Counts.Created != 0 || again.Counts.Updated != 0 || again.Counts.Unchanged != 3 {
		t.Fatalf("rerun = %+v", again.Counts)
	}
	if list, err := fixture.store.List(ctx, "demo", portsProposalListAll()); err != nil || len(list) != 3 {
		t.Fatalf("rerun duplicated proposals: %d, %v", len(list), err)
	}

	issue.Body = "Second body."
	issue.ObservedAt = fixture.clock.Add(time.Hour)
	fixture.add(t, issue)
	updated, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh", Write: true})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Counts.Updated != 1 || updated.Counts.Unchanged != 2 {
		t.Fatalf("updated counts = %+v", updated.Counts)
	}

	var issueProposal domain.Proposal
	for _, entry := range written.Entries {
		if entry.Proposal.ExternalID == "issue-closed" {
			issueProposal = entry.Proposal
		}
	}
	if issueProposal.ID == "" {
		t.Fatal("issue proposal not found")
	}
	if _, err := fixture.store.SetStatus(ctx, "demo", issueProposal.ID, domain.ProposalPromoted, "decision-promoted"); err != nil {
		t.Fatal(err)
	}
	terminal, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh", Write: true})
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Counts.Skipped != 1 || len(terminal.Entries) != 2 {
		t.Fatalf("terminal run = %+v", terminal)
	}
	stored, err := fixture.store.Get(ctx, "demo", issueProposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.ProposalPromoted || stored.PromotedRecordID != "decision-promoted" {
		t.Fatalf("terminal proposal was overwritten: %+v", stored)
	}
}

func TestProposalDigestProjectResolution(t *testing.T) {
	ctx := context.Background()
	fixture := newProposalFixture(t, []string{"demo", "other"})
	demoIssue := fixtureItem("issue-demo", domain.ItemIssue, "Demo issue")
	demoIssue.Labels = []string{"decision"}
	fixture.add(t, demoIssue)
	other := fixtureItem("issue-other", domain.ItemIssue, "Other issue")
	other.ProjectID = "other"
	fixture.add(t, other)

	if _, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh"}); err == nil ||
		!strings.Contains(err.Error(), "pass --project") {
		t.Fatalf("ambiguous project error = %v", err)
	}
	scoped, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh", ProjectID: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if scoped.ProjectID != "demo" || scoped.Counts.Created != 1 || scoped.Counts.Skipped != 1 {
		t.Fatalf("scoped = %+v", scoped)
	}
	if _, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh", ProjectID: "missing"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown project error = %v", err)
	}

	empty := newProposalFixture(t, nil)
	if _, err := empty.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh"}); err == nil ||
		!strings.Contains(err.Error(), "pass --project") {
		t.Fatalf("unmapped project error = %v", err)
	}
}

func TestProposalDigestWindowFallsBackToObservation(t *testing.T) {
	ctx := context.Background()
	fixture := newProposalFixture(t, []string{"demo"})

	recent := fixtureItem("commit-recent", domain.ItemCommit, "newly seen")
	recent.CreatedAt = fixture.clock.AddDate(0, 0, -40)
	recent.ObservedAt = fixture.clock.Add(-time.Hour)
	fixture.add(t, recent)
	preview, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Counts.Created != 1 {
		t.Fatalf("newly observed item with an old provider timestamp was skipped: %+v", preview.Counts)
	}

	old := fixtureItem("commit-old", domain.ItemCommit, "provider update is old")
	old.UpdatedAt = fixture.clock.AddDate(0, 0, -40)
	old.ObservedAt = fixture.clock.Add(-time.Hour)
	fixture.add(t, old)
	preview, err = fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Counts.Created != 1 || preview.Counts.Skipped != 1 {
		t.Fatalf("old provider update was not skipped: %+v", preview.Counts)
	}
}

func TestProposalDigestErrorsWithoutAdapters(t *testing.T) {
	ctx := context.Background()
	fixture := newProposalFixture(t, []string{"demo"})
	if _, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "missing"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing connection error = %v", err)
	}
	empty := app.New(config.Default(), app.Dependencies{})
	if _, err := empty.ProposalDigest(ctx, app.ProposalDigestOptions{ConnectionID: "gh"}); !errors.Is(err, app.ErrNotImplemented) {
		t.Fatalf("missing adapters error = %v", err)
	}
}

func portsProposalListAll() ports.ProposalListOptions {
	return ports.ProposalListOptions{}
}
