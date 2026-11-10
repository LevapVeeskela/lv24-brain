package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type promoteFixture struct {
	app         *app.App
	store       *fsstore.Store
	connections *fsstore.ConnectionStore
	items       *fsstore.ItemStore
	dataDir     string
	clock       time.Time
}

func newPromoteFixture(t *testing.T, publish domain.PublishPolicy) *promoteFixture {
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
	clock := time.Now().UTC().Truncate(time.Second)
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.User = "alice"
	fixture := &promoteFixture{
		store: store, connections: connections, items: store.Items(),
		dataDir: dataDir, clock: clock,
	}
	fixture.app = app.New(cfg, app.Dependencies{
		Store:       store,
		Connections: connections,
		Items:       store.Items(),
		Proposals:   store,
		Now:         func() time.Time { return fixture.clock },
	})
	if _, err := fixture.app.SaveProject(ctx, app.ProjectInput{ID: "demo"}); err != nil {
		t.Fatal(err)
	}
	connection := domain.Connection{
		ID: "gh", Provider: "github", Projects: []string{"demo"}, Publish: publish,
	}
	if err := connections.Create(ctx, connection); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *promoteFixture) add(t *testing.T, item domain.Item) domain.Item {
	t.Helper()
	if item.Ref.ConnectionID == "" {
		item.Ref.ConnectionID = "gh"
	}
	if item.ProjectID == "" {
		item.ProjectID = "demo"
	}
	if item.ObservedAt.IsZero() {
		item.ObservedAt = f.clock
	}
	if item.URL == "" {
		item.URL = "https://example.test/items/" + item.Ref.ExternalID
	}
	item.Hash = item.ComputeHash()
	if _, err := f.items.Upsert(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	return item
}

func (f *promoteFixture) digest(t *testing.T) {
	t.Helper()
	if _, err := f.app.ProposalDigest(context.Background(), app.ProposalDigestOptions{
		ConnectionID: "gh", ProjectID: "demo", All: true, Write: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *promoteFixture) pending(t *testing.T) []domain.Proposal {
	t.Helper()
	proposals, err := f.store.List(context.Background(), "demo", ports.ProposalListOptions{
		Statuses: []domain.ProposalStatus{domain.ProposalPending, domain.ProposalStale},
	})
	if err != nil {
		t.Fatal(err)
	}
	return proposals
}

func (f *promoteFixture) proposal(t *testing.T, externalID string) domain.Proposal {
	t.Helper()
	id := domain.ProposalID("demo", "gh", externalID)
	proposal, err := f.store.Get(context.Background(), "demo", id)
	if err != nil {
		t.Fatalf("get proposal %s: %v", id, err)
	}
	return proposal
}

func promoteIssueItem(externalID, title string, labels ...string) domain.Item {
	return domain.Item{
		Ref:    domain.ItemRef{ExternalID: externalID},
		Kind:   domain.ItemIssue,
		Title:  title,
		Labels: labels,
	}
}

func promoteClosedIssue(externalID, title string) domain.Item {
	item := promoteIssueItem(externalID, title)
	item.State = domain.ItemStateClosed
	item.Body = "The work is complete."
	return item
}

func TestPromoteDecisionAttemptAndNote(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)

	closed := promoteIssueItem("issue-closed", "Serialize writes")
	closed.State = domain.ItemStateClosed
	closed.Body = "Fixed by serializing writes."
	fixture.add(t, closed)

	incident := promoteIssueItem("issue-incident", "API outage", "incident", "production")
	incident.Body = "The API returned 500s for ten minutes."
	fixture.add(t, incident)

	merged := domain.Item{Ref: domain.ItemRef{ExternalID: "pr-merged"}, Kind: domain.ItemPullRequest, Title: "Merge parser fix", State: domain.ItemStateMerged, Body: "Parser fix."}
	fixture.add(t, merged)
	fixture.digest(t)

	decisionProposal := fixture.proposal(t, "issue-closed")
	decision, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: decisionProposal.ID})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Already || decision.Record.Kind != domain.ProposalDecision ||
		!strings.HasPrefix(decision.Record.ID, "decision-") || decision.Record.Title != "Serialize writes" {
		t.Fatalf("decision result = %+v", decision)
	}
	if decision.Proposal.PromotedRecordID != decision.Record.ID || decision.Proposal.Status != domain.ProposalPromoted {
		t.Fatalf("proposal after promotion = %+v", decision.Proposal)
	}
	decisions, err := fixture.store.ListDecisions(ctx, "demo")
	if err != nil || len(decisions) != 1 {
		t.Fatalf("decisions = %+v, %v", decisions, err)
	}
	source := decisions[0].Source
	if source == nil || source.ProposalID != decisionProposal.ID || source.ConnectionID != "gh" ||
		source.ExternalID != "issue-closed" || source.ItemKey == "" || source.URL == "" ||
		source.ObservedAt.IsZero() || source.Confidence <= 0 {
		t.Fatalf("decision source = %+v", source)
	}
	if decisions[0].Status != domain.DecisionProposed {
		t.Fatalf("decision status = %s, want proposed", decisions[0].Status)
	}

	attemptProposal := fixture.proposal(t, "issue-incident")
	attempt, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: attemptProposal.ID})
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Record.Kind != domain.ProposalAttempt || !strings.HasPrefix(attempt.Record.ID, "attempt-") {
		t.Fatalf("attempt result = %+v", attempt)
	}
	attempts, err := fixture.store.ListAttempts(ctx, "demo")
	if err != nil || len(attempts) != 1 || attempts[0].Source == nil ||
		attempts[0].Source.ProposalID != attemptProposal.ID || attempts[0].Hypothesis == "" ||
		attempts[0].Reason == "" {
		t.Fatalf("attempts = %+v, %v", attempts, err)
	}

	noteProposal := fixture.proposal(t, "pr-merged")
	note, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: noteProposal.ID})
	if err != nil {
		t.Fatal(err)
	}
	if note.Record.Kind != domain.ProposalNote || !strings.HasPrefix(note.Record.ID, "note-") {
		t.Fatalf("note result = %+v", note)
	}
	notes, err := fixture.store.ListNotes(ctx, "demo")
	if err != nil || len(notes) != 1 || notes[0].Source == nil ||
		notes[0].Source.ProposalID != noteProposal.ID || !strings.Contains(notes[0].Body, "Parser") {
		t.Fatalf("notes = %+v, %v", notes, err)
	}

	again, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: decisionProposal.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Already || again.Record.ID != decision.Record.ID {
		t.Fatalf("idempotent rerun = %+v", again)
	}
	decisions, _ = fixture.store.ListDecisions(ctx, "demo")
	if len(decisions) != 1 {
		t.Fatalf("idempotent rerun created %d decisions", len(decisions))
	}
}

func TestPromoteOverridesAndStatus(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)
	fixture.add(t, promoteIssueItem("issue-decision", "Adopt SQLite", "architecture"))
	fixture.digest(t)

	proposal := fixture.proposal(t, "issue-decision")
	result, err := fixture.app.Promote(ctx, app.PromoteOptions{
		ProposalID: proposal.ID,
		Status:     domain.DecisionAccepted,
		Title:      "Adopt SQLite for the index",
	})
	if err != nil {
		t.Fatal(err)
	}
	decisions, err := fixture.store.ListDecisions(ctx, "demo")
	if err != nil || len(decisions) != 1 {
		t.Fatalf("decisions = %+v, %v", decisions, err)
	}
	if decisions[0].Status != domain.DecisionAccepted || decisions[0].Title != "Adopt SQLite for the index" {
		t.Fatalf("decision = %+v", decisions[0])
	}
	if result.Record.Title != "Adopt SQLite for the index" {
		t.Fatalf("result title = %q", result.Record.Title)
	}

	second := newPromoteFixture(t, domain.PublishShared)
	second.add(t, promoteIssueItem("issue-as", "Try the parser", "decision"))
	second.digest(t)
	asProposal := second.proposal(t, "issue-as")
	overridden, err := second.app.Promote(ctx, app.PromoteOptions{
		ProposalID: asProposal.ID,
		As:         domain.ProposalAttempt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if overridden.Record.Kind != domain.ProposalAttempt {
		t.Fatalf("--as result = %+v", overridden)
	}

	third := newPromoteFixture(t, domain.PublishShared)
	third.add(t, promoteIssueItem("issue-status", "Status check", "incident"))
	third.digest(t)
	statusProposal := third.proposal(t, "issue-status")
	if _, err := third.app.Promote(ctx, app.PromoteOptions{
		ProposalID: statusProposal.ID,
		Status:     domain.DecisionAccepted,
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("status on attempt error = %v, want validation", err)
	}
}

func TestPromoteDismissAndDryRun(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)
	fixture.add(t, promoteClosedIssue("issue-closed", "Serialize writes"))
	fixture.digest(t)
	closed := fixture.pending(t)
	if len(closed) != 1 {
		t.Fatalf("pending = %d", len(closed))
	}

	proposal := fixture.proposal(t, "issue-closed")
	dry, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: proposal.ID, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || dry.Record.Kind != domain.ProposalDecision || dry.Record.ID == "" {
		t.Fatalf("dry run = %+v", dry)
	}
	if stored, _ := fixture.store.Get(ctx, "demo", proposal.ID); stored.StatusOrPending() != domain.ProposalPending {
		t.Fatalf("dry run changed status to %s", stored.Status)
	}
	if decisions, _ := fixture.store.ListDecisions(ctx, "demo"); len(decisions) != 0 {
		t.Fatalf("dry run wrote %d decisions", len(decisions))
	}

	dryDismiss, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: proposal.ID, Dismiss: true, DryRun: true})
	if err != nil || !dryDismiss.Dismissed || !dryDismiss.DryRun {
		t.Fatalf("dry dismiss = %+v, %v", dryDismiss, err)
	}

	dismissed, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: proposal.ID, Dismiss: true})
	if err != nil {
		t.Fatal(err)
	}
	if !dismissed.Dismissed || dismissed.Already || dismissed.Proposal.Status != domain.ProposalDismissed {
		t.Fatalf("dismiss = %+v", dismissed)
	}
	again, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: proposal.ID, Dismiss: true})
	if err != nil || !again.Already || !again.Dismissed {
		t.Fatalf("idempotent dismiss = %+v, %v", again, err)
	}
	if _, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: proposal.ID}); err == nil ||
		errors.Is(err, domain.ErrValidation) {
		t.Fatalf("promote dismissed error = %v, want terminal error", err)
	}
}

func TestPromoteLocalOnlyRequiresYes(t *testing.T) {
	fixture := newPromoteFixture(t, domain.PublishLocalOnly)
	fixture.add(t, promoteClosedIssue("issue-closed", "Serialize writes"))
	fixture.digest(t)
	proposal := fixture.proposal(t, "issue-closed")

	_, err := fixture.app.Promote(context.Background(), app.PromoteOptions{ProposalID: proposal.ID})
	if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("local-only error = %v, want validation mentioning --yes", err)
	}
	result, err := fixture.app.Promote(context.Background(), app.PromoteOptions{ProposalID: proposal.ID, Yes: true})
	if err != nil || result.Record.Kind != domain.ProposalDecision {
		t.Fatalf("local-only --yes = %+v, %v", result, err)
	}

	dismissFixture := newPromoteFixture(t, domain.PublishLocalOnly)
	dismissFixture.add(t, promoteClosedIssue("issue-local-dismiss", "Serialize writes"))
	dismissFixture.digest(t)
	dismissProposal := dismissFixture.proposal(t, "issue-local-dismiss")
	dismissed, err := dismissFixture.app.Promote(context.Background(), app.PromoteOptions{
		ProposalID: dismissProposal.ID, Dismiss: true,
	})
	if err != nil || !dismissed.Dismissed {
		t.Fatalf("local-only dismiss = %+v, %v", dismissed, err)
	}
}

func TestPromoteStaleOrphanNeedsForce(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)
	fixture.add(t, promoteClosedIssue("issue-closed", "Serialize writes"))
	fixture.digest(t)
	proposal := fixture.proposal(t, "issue-closed")

	changed := promoteClosedIssue("issue-closed", "Serialize writes everywhere")
	changed.Body = "The scope changed."
	fixture.add(t, changed)
	if _, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: proposal.ID}); err == nil ||
		errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("stale error = %v, want force hint", err)
	}
	staleForced, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: proposal.ID, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if staleForced.Already {
		t.Fatalf("forced promotion = %+v", staleForced)
	}

	orphanFixture := newPromoteFixture(t, domain.PublishShared)
	orphanFixture.add(t, promoteClosedIssue("issue-orphan", "Serialize writes"))
	orphanFixture.digest(t)
	orphanProposal := orphanFixture.proposal(t, "issue-orphan")
	if deleted, err := orphanFixture.items.Delete(ctx, domain.ItemRef{ConnectionID: "gh", ExternalID: "issue-orphan"}, orphanFixture.clock); err != nil || !deleted {
		t.Fatalf("delete item = %v, %v", deleted, err)
	}
	if _, err := orphanFixture.app.Promote(ctx, app.PromoteOptions{ProposalID: orphanProposal.ID}); err == nil ||
		errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("orphan error = %v, want force hint", err)
	}
	if _, err := orphanFixture.app.Promote(ctx, app.PromoteOptions{ProposalID: orphanProposal.ID, Force: true}); err != nil {
		t.Fatalf("forced orphan promotion: %v", err)
	}
}

func TestPromoteRepairsRecordFirstCrash(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)
	fixture.add(t, promoteClosedIssue("issue-closed", "Serialize writes"))
	fixture.digest(t)
	pending := fixture.pending(t)
	if len(pending) != 1 {
		t.Fatalf("pending = %d", len(pending))
	}
	proposal := pending[0]
	first, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: proposal.ID})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a crash after the record write and before SetStatus: the
	// proposal file is replaced by the pending snapshot.
	path := filepath.Join(fixture.dataDir, "proposals", "demo", proposal.ID+".md")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Put(ctx, proposal); err != nil {
		t.Fatal(err)
	}
	repaired, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: proposal.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !repaired.Already || repaired.Record.ID != first.Record.ID {
		t.Fatalf("repair = %+v", repaired)
	}
	stored, err := fixture.store.Get(ctx, "demo", proposal.ID)
	if err != nil || stored.Status != domain.ProposalPromoted || stored.PromotedRecordID != first.Record.ID {
		t.Fatalf("repaired proposal = %+v, %v", stored, err)
	}
	if decisions, _ := fixture.store.ListDecisions(ctx, "demo"); len(decisions) != 1 {
		t.Fatalf("repair duplicated decisions: %d", len(decisions))
	}
}

func TestPromoteIdenticalTitlesStaySeparate(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)
	fixture.add(t, promoteClosedIssue("issue-a", "Serialize writes"))
	fixture.add(t, promoteClosedIssue("issue-b", "Serialize writes"))
	fixture.digest(t)

	first, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: domain.ProposalID("demo", "gh", "issue-a")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: domain.ProposalID("demo", "gh", "issue-b")})
	if err != nil {
		t.Fatal(err)
	}
	if second.Already || second.Record.ID == first.Record.ID {
		t.Fatalf("identical titles collapsed: %+v / %+v", first, second)
	}
	decisions, err := fixture.store.ListDecisions(ctx, "demo")
	if err != nil || len(decisions) != 2 {
		t.Fatalf("decisions = %+v, %v", decisions, err)
	}
}

func TestPromoteNotFoundAndDismissFlagConflicts(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)
	if _, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: domain.ProposalID("demo", "gh", "missing")}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("not found error = %v, want ErrNotFound", err)
	}
	fixture.add(t, promoteClosedIssue("issue-closed", "Serialize writes"))
	fixture.digest(t)
	proposal := fixture.proposal(t, "issue-closed")
	for _, opts := range []app.PromoteOptions{
		{ProposalID: proposal.ID, Dismiss: true, Title: "nope"},
		{ProposalID: proposal.ID, Dismiss: true, As: domain.ProposalDecision},
		{ProposalID: proposal.ID, Dismiss: true, Status: domain.DecisionAccepted},
		{ProposalID: proposal.ID, As: "handoff"},
		{ProposalID: proposal.ID, Status: "rejected"},
	} {
		if _, err := fixture.app.Promote(ctx, opts); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%+v error = %v, want validation", opts, err)
		}
	}
}
