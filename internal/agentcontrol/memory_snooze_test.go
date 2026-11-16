package agentcontrol

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/localapi"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestMemoryServiceRecordTagsListDetailAndEdit(t *testing.T) {
	fixture := newMemoryFixture(t)
	ctx := context.Background()
	created, err := fixture.app.Note(ctx, app.NoteInput{
		ProjectID: "demo", Title: "Cache design", Body: "one sqlite index for search",
		Tags: []string{"search", "design"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"search", "design"}

	rows, err := fixture.service.ListRecords(ctx, localapi.RecordQuery{ProjectID: "demo", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !slices.Equal(rows[0].Tags, want) {
		t.Fatalf("list rows = %+v", rows)
	}
	detail, err := fixture.service.RecordDetail(ctx, localapi.RecordDetailQuery{ProjectID: "demo", ID: created.Note.ID, MaxTokens: 500})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(detail.Tags, want) {
		t.Fatalf("detail tags = %#v, want %#v", detail.Tags, want)
	}

	tags := []string{"search", "sqlite"}
	edited, err := fixture.service.EditRecord(ctx, localapi.RecordEditRequest{
		ProjectID: "demo", ID: created.Note.ID, Tags: &tags,
	})
	if err != nil {
		t.Fatalf("EditRecord tags: %v", err)
	}
	if !slices.Equal(edited.Tags, tags) {
		t.Fatalf("edited detail tags = %#v, want %#v", edited.Tags, tags)
	}
	rows, err = fixture.service.ListRecords(ctx, localapi.RecordQuery{ProjectID: "demo", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !slices.Equal(rows[0].Tags, tags) {
		t.Fatalf("list rows after edit = %+v", rows)
	}
	detail, err = fixture.service.RecordDetail(ctx, localapi.RecordDetailQuery{ProjectID: "demo", ID: created.Note.ID, MaxTokens: 500})
	if err != nil || !slices.Equal(detail.Tags, tags) {
		t.Fatalf("detail after edit = %#v, %v", detail.Tags, err)
	}
}

func TestMemoryServiceProposalSnoozeListReviewAndDetail(t *testing.T) {
	fixture := newMemoryFixture(t)
	ctx := context.Background()
	if err := fixture.connections.Create(ctx, domain.Connection{
		ID: "gh", Provider: "github", Projects: []string{"demo"}, Publish: domain.PublishShared,
	}); err != nil {
		t.Fatal(err)
	}
	fixture.addIssue(t, "issue-1", "Adopt SQLite")
	if _, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{
		ConnectionID: "gh", ProjectID: "demo", All: true, Write: true,
	}); err != nil {
		t.Fatal(err)
	}
	page, err := fixture.service.ListProposals(ctx, localapi.ProposalQuery{ProjectID: "demo", Limit: 10})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("proposal page = %+v, %v", page, err)
	}
	proposal := page.Items[0]
	until := fixture.clock.Add(7 * 24 * time.Hour)

	receipt, err := fixture.service.SnoozeProposal(ctx, localapi.ProposalSnoozeRequest{
		ProjectID: "demo", ProposalID: proposal.ID, Until: until,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Snoozed || !receipt.SnoozedUntil.Equal(until) || receipt.Status != "pending" || receipt.UpdatedAt.IsZero() {
		t.Fatalf("snooze receipt = %+v", receipt)
	}

	hidden, err := fixture.service.ListProposals(ctx, localapi.ProposalQuery{ProjectID: "demo", Limit: 10})
	if err != nil || hidden.Total != 0 || len(hidden.Items) != 0 {
		t.Fatalf("default proposal page = %+v, %v", hidden, err)
	}
	withSnoozed, err := fixture.service.ListProposals(ctx, localapi.ProposalQuery{ProjectID: "demo", Limit: 10, IncludeSnoozed: true})
	if err != nil || withSnoozed.Total != 1 || !withSnoozed.Items[0].Snoozed ||
		!withSnoozed.Items[0].SnoozedUntil.Equal(until) {
		t.Fatalf("include_snoozed page = %+v, %v", withSnoozed, err)
	}

	review, err := fixture.service.Review(ctx, localapi.ReviewQuery{ProjectID: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if review.Proposals.PendingCount != 0 || review.Proposals.SnoozedCount != 1 || len(review.Proposals.Items) != 0 {
		t.Fatalf("review proposals = %+v", review.Proposals)
	}

	detail, err := fixture.service.ProposalDetail(ctx, localapi.ProposalDetailQuery{ProjectID: "demo", ID: proposal.ID, MaxTokens: 500})
	if err != nil {
		t.Fatal(err)
	}
	if !detail.Snoozed || !detail.SnoozedUntil.Equal(until) || detail.UpdatedAt.IsZero() {
		t.Fatalf("proposal detail = %+v", detail)
	}

	cleared, err := fixture.service.SnoozeProposal(ctx, localapi.ProposalSnoozeRequest{
		ProjectID: "demo", ProposalID: proposal.ID, Clear: true,
	})
	if err != nil || cleared.Snoozed || !cleared.SnoozedUntil.IsZero() {
		t.Fatalf("cleared receipt = %+v, %v", cleared, err)
	}
	visible, err := fixture.service.ListProposals(ctx, localapi.ProposalQuery{ProjectID: "demo", Limit: 10})
	if err != nil || visible.Total != 1 {
		t.Fatalf("visible page = %+v, %v", visible, err)
	}

	if _, err := fixture.service.Promote(ctx, localapi.ProposalDecision{ProjectID: "demo", ProposalID: proposal.ID, Dismiss: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.SnoozeProposal(ctx, localapi.ProposalSnoozeRequest{
		ProjectID: "demo", ProposalID: proposal.ID, Until: until,
	}); err == nil {
		t.Fatal("snoozing a dismissed proposal succeeded")
	}
}
