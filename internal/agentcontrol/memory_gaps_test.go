package agentcontrol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/localapi"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestMemoryServiceAllProjectsScope(t *testing.T) {
	fixture := newMemoryFixture(t)
	ctx := context.Background()
	if _, err := fixture.app.SaveProject(ctx, app.ProjectInput{ID: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.Note(ctx, app.NoteInput{ProjectID: "other", Title: "Other note", Body: "sqlite ranking on the other project"}); err != nil {
		t.Fatal(err)
	}
	fixture.note(t, "Demo note", "sqlite ranking on the demo project", domain.Provenance{})
	quarantined := fixture.note(t, "Imported", "external", domain.Provenance{Origin: domain.OriginAgent, Reviewed: false})

	rows, err := fixture.service.ListRecords(ctx, localapi.RecordQuery{AllProjects: true, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	projects := map[string]bool{}
	for _, row := range rows {
		projects[row.ProjectID] = true
	}
	if !projects["demo"] || !projects["other"] {
		t.Fatalf("all-projects rows = %+v", rows)
	}
	scoped, err := fixture.service.ListRecords(ctx, localapi.RecordQuery{
		AllProjects: true, ProjectIDs: []string{"demo"}, Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) == 0 {
		t.Fatal("scoped all-projects listing returned nothing")
	}
	for _, row := range scoped {
		if row.ProjectID != "demo" {
			t.Fatalf("scoped row escaped the scope: %+v", row)
		}
	}

	search, err := fixture.service.Search(ctx, localapi.SearchQuery{
		Text: "sqlite ranking", AllProjects: true, Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Hits) < 2 {
		t.Fatalf("all-projects search hits = %+v", search.Hits)
	}
	scopedSearch, err := fixture.service.Search(ctx, localapi.SearchQuery{
		Text: "sqlite ranking", AllProjects: true, ProjectIDs: []string{"other"}, Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range scopedSearch.Hits {
		if hit.ProjectID != "other" {
			t.Fatalf("scoped search escaped the scope: %+v", hit)
		}
	}

	review, err := fixture.service.Review(ctx, localapi.ReviewQuery{AllProjects: true})
	if err != nil {
		t.Fatal(err)
	}
	if review.ProjectID != localapi.AllProjectsProjectID || !review.AllProjects || len(review.ProjectIDs) != 2 {
		t.Fatalf("all-projects review = %+v", review)
	}
	found := false
	for _, finding := range review.Findings {
		if finding.ProjectID == "demo" && finding.Kind == string(app.FindingQuarantined) {
			found = true
		}
	}
	if !found {
		t.Fatalf("quarantined finding without its project: %+v", review.Findings)
	}
	if quarantined.ID == "" {
		t.Fatal("quarantined note missing")
	}
}

func TestMemoryServiceProposalBodiesDetailAndBudget(t *testing.T) {
	fixture := newMemoryFixture(t)
	ctx := context.Background()
	body := strings.Repeat("source excerpt detail ", 600)
	for i := 0; i < 9; i++ {
		if _, err := fixture.store.Put(ctx, domain.Proposal{
			ProjectID: "demo", ConnectionID: "gh", ExternalID: fmt.Sprintf("issue-%d", i),
			Kind: domain.ProposalNote, Rule: "test", Title: fmt.Sprintf("candidate %d", i), Confidence: 0.5,
			Body: body,
		}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := fixture.service.ListProposals(ctx, localapi.ProposalQuery{
		ProjectID: "demo", Limit: 20, IncludeBody: true, BodyTokens: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 9 {
		t.Fatalf("items = %d", len(page.Items))
	}
	omitted := 0
	for _, item := range page.Items {
		if item.BodyOmitted {
			omitted++
			if item.Body != "" || item.BodyTokens != 0 {
				t.Fatalf("omitted body leaked: %+v", item)
			}
			continue
		}
		if item.Body == "" || item.BodyTokens > 2000 {
			t.Fatalf("bounded body = %+v", item)
		}
	}
	if omitted == 0 {
		t.Fatalf("page budget never omitted a body: %+v", page.Items)
	}

	detail, err := fixture.service.ProposalDetail(ctx, localapi.ProposalDetailQuery{
		ProjectID: "demo", ID: page.Items[0].ID, MaxTokens: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !detail.BodyTruncated || detail.BodyTokens > 10 || detail.Body == "" {
		t.Fatalf("proposal detail = %+v", detail)
	}
	if _, err := fixture.service.ProposalDetail(ctx, localapi.ProposalDetailQuery{
		ProjectID: "demo", ID: "proposal-gh-000000000000", MaxTokens: 10,
	}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing proposal = %v", err)
	}
}

func TestMemoryServiceQuarantineTrashRestoreAndRelationKind(t *testing.T) {
	fixture := newMemoryFixture(t)
	ctx := context.Background()
	quarantined := fixture.note(t, "Imported claim", "external text", domain.Provenance{
		Origin: domain.OriginAgent, Reviewed: false, Trust: domain.TrustStated,
	})
	items, err := fixture.service.ListQuarantined(ctx, localapi.QuarantineQuery{ProjectID: "demo", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != quarantined.ID || items[0].Reason != app.QuarantineReasonUnreviewed || items[0].AgeSeconds < 0 {
		t.Fatalf("quarantine = %+v", items)
	}
	reviewed, err := fixture.service.ReviewQuarantine(ctx, localapi.QuarantineReviewDecision{
		ProjectID: "demo", ID: quarantined.ID, Trust: "verified",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reviewed.Record.Provenance.Quarantined || !reviewed.Record.Provenance.Reviewed ||
		reviewed.Provenance.Trust != "verified" {
		t.Fatalf("reviewed quarantine = %+v", reviewed)
	}
	if items, err := fixture.service.ListQuarantined(ctx, localapi.QuarantineQuery{ProjectID: "demo", Limit: 10}); err != nil || len(items) != 0 {
		t.Fatalf("quarantine after review = %+v %v", items, err)
	}
	if _, err := fixture.service.ReviewQuarantine(ctx, localapi.QuarantineReviewDecision{
		ProjectID: "demo", ID: quarantined.ID, Trust: "guessed",
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad trust = %v", err)
	}

	note := fixture.note(t, "Trash me", "reversible", domain.Provenance{})
	trashed, err := fixture.service.TrashRecord(ctx, "demo", note.ID)
	if err != nil || trashed.ID != note.ID {
		t.Fatalf("trash = %+v %v", trashed, err)
	}
	rows, err := fixture.service.ListRecords(ctx, localapi.RecordQuery{ProjectID: "demo", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == note.ID {
			t.Fatalf("trashed record stayed listed: %+v", row)
		}
	}
	restored, err := fixture.service.RestoreRecord(ctx, "demo", note.ID)
	if err != nil || restored.ID != note.ID {
		t.Fatalf("restore = %+v %v", restored, err)
	}
	if _, err := fixture.service.TrashRecord(ctx, "demo", "note-missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("trash missing = %v", err)
	}

	target := fixture.note(t, "Relation target", "target text", domain.Provenance{})
	source, err := fixture.app.Decision(ctx, app.DecisionInput{ProjectID: "demo", Title: "Relation source", Context: "context"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.JudgeRelation(ctx, app.JudgeOptions{
		ProjectID: "demo", From: source.Decision.ID, To: target.ID,
		Verdict: domain.RelationRelatesTo, Reason: "same topic", By: domain.VerdictByHuman,
	}); err != nil {
		t.Fatal(err)
	}
	detail, err := fixture.service.RecordDetail(ctx, localapi.RecordDetailQuery{
		ProjectID: "demo", ID: source.Decision.ID, MaxTokens: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Relations) != 1 || detail.Relations[0].TargetKind != "note" {
		t.Fatalf("relations = %+v", detail.Relations)
	}
}

func TestMemoryServiceReviewFixDryRunThenApply(t *testing.T) {
	fixture := newMemoryFixture(t)
	ctx := context.Background()
	next := "old step"
	questions := []string{"Which storage engine should the index use?"}
	if _, err := fixture.app.SetState(ctx, app.StateInput{ProjectID: "demo", NextAction: &next, OpenQuestions: &questions}); err != nil {
		t.Fatal(err)
	}
	fixture.clock = fixture.clock.Add(time.Minute)
	if _, err := fixture.app.Handoff(ctx, app.HandoffInput{ProjectID: "demo", Summary: "done", NextAction: "ship the release"}); err != nil {
		t.Fatal(err)
	}
	fixture.clock = fixture.clock.Add(time.Minute)
	if _, err := fixture.app.Decision(ctx, app.DecisionInput{ProjectID: "demo", Title: "Index storage engine: SQLite FTS5"}); err != nil {
		t.Fatal(err)
	}
	// The fixture clock is fixed, so age the state explicitly: the handoff must
	// be later than the state update for the next-action finding to exist.
	state, err := fixture.store.GetState(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	state.UpdatedAt = state.UpdatedAt.Add(-time.Minute)
	if _, err := fixture.store.PutState(ctx, state); err != nil {
		t.Fatal(err)
	}
	fixture.service.now = func() time.Time { return fixture.clock }

	dry, err := fixture.service.ReviewFix(ctx, localapi.ReviewFixRequest{ProjectID: "demo", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || len(dry.Findings) != 2 || len(dry.Changes) != 2 {
		t.Fatalf("dry run = %+v", dry)
	}
	_, state, err = fixture.app.GetState(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if state.NextAction != next || len(state.OpenQuestions) != 1 {
		t.Fatalf("dry run wrote state: %+v", state)
	}

	applied, err := fixture.service.ReviewFix(ctx, localapi.ReviewFixRequest{ProjectID: "demo", Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if applied.DryRun || applied.State.NextAction != "ship the release" || len(applied.State.OpenQuestions) != 0 {
		t.Fatalf("applied fix = %+v", applied)
	}
	if _, err := fixture.service.ReviewFix(ctx, localapi.ReviewFixRequest{
		ProjectID: "demo", Kinds: []string{"bogus"},
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad kind = %v", err)
	}
}
