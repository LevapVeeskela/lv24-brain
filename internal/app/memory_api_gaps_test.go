package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// putProposal stores one candidate directly, so a body test does not need a
// connector digest.
func putProposal(t *testing.T, store *fsstore.Store, projectID string, sections []domain.ProposalSection) domain.Proposal {
	t.Helper()
	stored, err := store.Put(context.Background(), domain.Proposal{
		ProjectID: projectID, ConnectionID: "gh", ExternalID: "issue-9",
		Kind: domain.ProposalDecision, Rule: "test", Title: "Adopt SQLite", Confidence: 0.7,
		Body:     "the source item asks for one index",
		Sections: sections,
	})
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestProposalBodiesAreBoundedAndRendered(t *testing.T) {
	fixture := newMemoryAPIFixture(t)
	ctx := context.Background()
	proposal := putProposal(t, fixture.store, "demo", []domain.ProposalSection{
		{Heading: "Context", Body: strings.Repeat("ranking detail ", 40)},
		{Heading: "Evidence", Body: "issue-9"},
	})

	listing, err := fixture.app.ListProposals(ctx, app.ProposalQuery{
		ProjectID: "demo", IncludeBody: true, BodyTokens: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Items) != 1 {
		t.Fatalf("items = %+v", listing.Items)
	}
	item := listing.Items[0]
	if !item.BodyTruncated || item.MaxTokens != 30 || item.BodyTokens > 30 || item.Body == "" {
		t.Fatalf("bounded body = %+v", item)
	}
	if !strings.Contains(item.Body, "Context:") {
		t.Fatalf("body lost its section heading: %q", item.Body)
	}

	whole, err := fixture.app.ProposalBody(ctx, "demo", proposal.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if whole.Truncated || !strings.Contains(whole.Body, "Evidence:") || whole.ID != proposal.ID || whole.Kind != domain.ProposalDecision {
		t.Fatalf("whole body = %+v", whole)
	}
	part, err := fixture.app.ProposalBody(ctx, "demo", proposal.ID, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !part.Truncated || part.Tokens > 8 || part.MaxTokens != 8 {
		t.Fatalf("truncated body = %+v", part)
	}
	if _, err := fixture.app.ProposalBody(ctx, "demo", "proposal-gh-000000000000", 10); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing proposal = %v", err)
	}
}

func TestListQuarantinedReturnsIdentityReasonAndLimit(t *testing.T) {
	fixture := newMemoryAPIFixture(t)
	ctx := context.Background()
	if _, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "demo", Title: "owner note", Body: "trusted"}); err != nil {
		t.Fatal(err)
	}
	quarantined, err := fixture.app.Note(ctx, app.NoteInput{
		ProjectID: "demo", Title: "imported", Body: "external",
		Provenance: domain.Provenance{Origin: domain.OriginImport, Reviewed: false, Trust: domain.TrustInferred},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := fixture.app.ListQuarantined(ctx, "demo", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("quarantine rows = %+v", rows)
	}
	row := rows[0]
	if row.ID != quarantined.Note.ID || row.Kind != "note" || row.ProjectID != "demo" ||
		row.Title == "" || row.Reason != app.QuarantineReasonUnreviewed || row.Origin != "import" {
		t.Fatalf("quarantine row = %+v", row)
	}
	if limited, err := fixture.app.ListQuarantined(ctx, "demo", 0); err != nil || len(limited) != 1 {
		t.Fatalf("default limit = %+v %v", limited, err)
	}
	if _, err := fixture.app.ListQuarantined(ctx, "missing", 10); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown project = %v", err)
	}
}

func TestRecordLinksCarryTargetKinds(t *testing.T) {
	fixture := newMemoryAPIFixture(t)
	ctx := context.Background()
	note, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "demo", Title: "note", Body: "text"})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := fixture.app.Decision(ctx, app.DecisionInput{
		ProjectID: "demo", Title: "decision", Context: "see [[" + note.Note.ID + "]]",
	})
	if err != nil {
		t.Fatal(err)
	}
	links, err := fixture.app.RecordLinks(ctx, "demo", decision.Decision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if links.TargetKinds[note.Note.ID] != "note" || links.TargetKinds[decision.Decision.ID] != "decision" {
		t.Fatalf("target kinds = %+v", links.TargetKinds)
	}
}

func TestPlanFixFindingsIsAReadOnlyDryRun(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t)
	next := "write the parser"
	questions := []string{"Which storage engine should the index use?", "Who owns the release key?"}
	if _, err := f.app.SetState(ctx, app.StateInput{ProjectID: "p", NextAction: &next, OpenQuestions: &questions}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if _, err := f.app.Handoff(ctx, app.HandoffInput{ProjectID: "p", Summary: "parser done", NextAction: "ship the release"}); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if _, err := f.app.Decision(ctx, app.DecisionInput{ProjectID: "p", Title: "Index storage engine: SQLite FTS5"}); err != nil {
		t.Fatal(err)
	}

	plan, err := f.app.PlanFixFindings(ctx, "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Findings) != 2 || len(plan.Changes) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	actions := map[string]string{}
	for _, change := range plan.Changes {
		actions[change.Action] = change.Value
	}
	if actions[app.FixSetNextAction] != "ship the release" ||
		actions[app.FixDropQuestion] != "Which storage engine should the index use?" {
		t.Fatalf("changes = %+v", plan.Changes)
	}
	_, state, err := f.app.GetState(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if state.NextAction != next || len(state.OpenQuestions) != 2 {
		t.Fatalf("dry run wrote state: %+v", state)
	}

	result, err := f.app.FixFindings(ctx, "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 2 || len(result.Changes) != 2 || result.State.NextAction != "ship the release" {
		t.Fatalf("applied = %+v", result)
	}
}
