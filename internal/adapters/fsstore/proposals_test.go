package fsstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func testProposal(projectID string) domain.Proposal {
	proposal := domain.Proposal{
		SchemaVersion: domain.ProposalSchemaVersion,
		ProjectID:     projectID,
		Kind:          domain.ProposalDecision,
		Rule:          "closed-issue",
		Confidence:    0.82,
		ConnectionID:  "gh-acme",
		ExternalID:    "acme/widgets#issue-7",
		Title:         "Adopt a retry budget",
		Body:          "The closed issue contains a decision.",
		Sections: []domain.ProposalSection{
			{Heading: "Context", Body: "Retries were unbounded."},
			{Heading: "Consequences", Body: "Cap retries at three."},
		},
		Citations: []domain.ProposalCitation{{
			ConnectionID: "gh-acme",
			ItemKey:      "gh-acme/" + strings.Repeat("a", 40),
			ExternalID:   "acme/widgets#issue-7",
			URL:          "https://github.test/acme/widgets/issues/7",
			Title:        "Unbounded retries",
			ObservedAt:   testClock.Add(-time.Hour),
		}},
		Status:    domain.ProposalPending,
		CreatedAt: testClock.Add(-time.Hour),
		UpdatedAt: testClock.Add(-time.Hour),
	}
	proposal.ID = domain.ProposalID(proposal.ProjectID, proposal.ConnectionID, proposal.ExternalID)
	proposal.PayloadHash = proposal.ComputePayloadHash()
	return proposal
}

func TestProposalStoreRoundTripAndLayout(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	proposal := testProposal(project.ID)

	stored, err := store.Put(ctx, proposal)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if stored.ID != proposal.ID || stored.Status != domain.ProposalPending || stored.PayloadHash != proposal.PayloadHash {
		t.Fatalf("stored proposal = %+v", stored)
	}
	if !stored.CreatedAt.Equal(proposal.CreatedAt) || !stored.UpdatedAt.Equal(testClock) {
		t.Fatalf("stored times = created %v updated %v", stored.CreatedAt, stored.UpdatedAt)
	}

	path := filepath.Join(store.Root(), "proposals", project.ID, proposal.ID+".md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read proposal file: %v", err)
	}
	var front proposalFrontmatter
	body, err := markdown.Parse(raw, &front)
	if err != nil {
		t.Fatalf("parse proposal: %v", err)
	}
	if front.Kind != "proposal" || front.ProposalKind != domain.ProposalDecision || front.Status != domain.ProposalPending {
		t.Fatalf("proposal frontmatter = %+v", front)
	}
	if front.ProjectID != project.ID || front.SchemaVersion != domain.ProposalSchemaVersion || front.PayloadHash != proposal.PayloadHash {
		t.Fatalf("proposal identity frontmatter = %+v", front)
	}
	if !strings.Contains(body, "## Context") || !strings.Contains(body, "Retries were unbounded.") {
		t.Fatalf("proposal body = %q", body)
	}
	ignore, err := os.ReadFile(filepath.Join(store.Root(), "proposals", ".gitignore"))
	if err != nil {
		t.Fatalf("read proposals ignore: %v", err)
	}
	if !strings.Contains(string(ignore), "*") {
		t.Fatalf("proposals ignore = %q", ignore)
	}

	got, err := store.Get(ctx, project.ID, proposal.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != proposal.Title || got.Kind != proposal.Kind || got.Rule != proposal.Rule || got.Confidence != proposal.Confidence {
		t.Fatalf("Get = %+v", got)
	}
	if len(got.Sections) != 2 || got.Sections[0].Heading != "Context" || got.Sections[1].Body != "Cap retries at three." {
		t.Fatalf("sections = %+v", got.Sections)
	}
	if len(got.Citations) != 1 || got.Citations[0].URL != proposal.Citations[0].URL || !got.Citations[0].ObservedAt.Equal(proposal.Citations[0].ObservedAt) {
		t.Fatalf("citations = %+v", got.Citations)
	}

	list, err := store.List(ctx, project.ID, ports.ProposalListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != proposal.ID {
		t.Fatalf("List = %+v", list)
	}
	for name, opts := range map[string]ports.ProposalListOptions{
		"status":     {Statuses: []domain.ProposalStatus{domain.ProposalPending}},
		"kind":       {Kind: domain.ProposalDecision},
		"connection": {ConnectionID: "gh-acme"},
		"rule":       {Rule: "closed-issue"},
		"limit":      {Limit: 1},
	} {
		got, err := store.List(ctx, project.ID, opts)
		if err != nil || len(got) != 1 {
			t.Fatalf("%s filter = %+v, %v", name, got, err)
		}
	}
	for name, opts := range map[string]ports.ProposalListOptions{
		"status":     {Statuses: []domain.ProposalStatus{domain.ProposalPromoted}},
		"kind":       {Kind: domain.ProposalNote},
		"connection": {ConnectionID: "gl-other"},
		"rule":       {Rule: "merged-pr"},
		"limit":      {Limit: 0, Statuses: []domain.ProposalStatus{domain.ProposalDismissed}},
	} {
		got, err := store.List(ctx, project.ID, opts)
		if err != nil || len(got) != 0 {
			t.Fatalf("%s filter = %+v, %v; want empty", name, got, err)
		}
	}
}

func TestProposalStoresDelimiterLinesInBody(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	proposal := testProposal(project.ID)
	proposal.Body = "before\n\n---\n\nafter"
	proposal.Sections[0].Body = "one\n\n---\n\ntwo"
	proposal.PayloadHash = proposal.ComputePayloadHash()

	if _, err := store.Put(ctx, proposal); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, project.ID, proposal.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Body != proposal.Body || got.Sections[0].Body != proposal.Sections[0].Body {
		t.Fatalf("body lost delimiter lines: %q / %q", got.Body, got.Sections[0].Body)
	}
	raw, err := os.ReadFile(filepath.Join(store.Root(), "proposals", project.ID, proposal.ID+".md"))
	if err != nil {
		t.Fatalf("read proposal: %v", err)
	}
	var front proposalFrontmatter
	body, err := markdown.Parse(raw, &front)
	if err != nil {
		t.Fatalf("parse proposal with delimiter lines: %v", err)
	}
	if !strings.Contains(body, "# Adopt a retry budget") || !strings.Contains(body, "---") {
		t.Fatalf("rendered body = %q", body)
	}
}

func TestProposalPutNormalizesDefaults(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	proposal := testProposal(project.ID)
	proposal.ID = ""
	proposal.SchemaVersion = 0
	proposal.Status = ""
	proposal.PayloadHash = ""
	proposal.CreatedAt = time.Time{}

	stored, err := store.Put(ctx, proposal)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if stored.ID != domain.ProposalID(project.ID, proposal.ConnectionID, proposal.ExternalID) {
		t.Fatalf("id = %q", stored.ID)
	}
	if stored.SchemaVersion != domain.ProposalSchemaVersion || stored.Status != domain.ProposalPending {
		t.Fatalf("normalized proposal = %+v", stored)
	}
	if stored.PayloadHash == "" || stored.PayloadHash != stored.ComputePayloadHash() {
		t.Fatalf("payload hash = %q", stored.PayloadHash)
	}
	if !stored.CreatedAt.Equal(testClock) || !stored.UpdatedAt.Equal(testClock) {
		t.Fatalf("times = created %v updated %v", stored.CreatedAt, stored.UpdatedAt)
	}
	if _, err := store.Get(ctx, project.ID, stored.ID); err != nil {
		t.Fatalf("Get normalized: %v", err)
	}
}

func TestProposalPutPreservesCreatedAtAndFreezesTerminal(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	proposal := testProposal(project.ID)

	if _, err := store.Put(ctx, proposal); err != nil {
		t.Fatalf("Put: %v", err)
	}
	first, err := store.Get(ctx, project.ID, proposal.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// An unchanged payload is idempotent: no rewrite and no new updated_at.
	later := testClock.Add(time.Hour)
	store.now = func() time.Time { return later }
	if _, err := store.Put(ctx, proposal); err != nil {
		t.Fatalf("Put again: %v", err)
	}
	again, err := store.Get(ctx, project.ID, proposal.ID)
	if err != nil {
		t.Fatalf("Get again: %v", err)
	}
	if !again.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("idempotent Put changed updated_at: %v -> %v", first.UpdatedAt, again.UpdatedAt)
	}

	// A changed payload rewrites the pending proposal but keeps created_at.
	changed := proposal
	changed.Title = "Adopt a smaller retry budget"
	changed.PayloadHash = changed.ComputePayloadHash()
	rewritten, err := store.Put(ctx, changed)
	if err != nil {
		t.Fatalf("Put changed: %v", err)
	}
	if !rewritten.CreatedAt.Equal(first.CreatedAt) || !rewritten.UpdatedAt.Equal(later) {
		t.Fatalf("rewritten times = created %v updated %v", rewritten.CreatedAt, rewritten.UpdatedAt)
	}
	if rewritten.PayloadHash == first.PayloadHash {
		t.Fatal("changed payload kept the old hash")
	}

	promoted, err := store.SetStatus(ctx, project.ID, proposal.ID, domain.ProposalPromoted, "decision-2026-11-09-retry-budget")
	if err != nil {
		t.Fatalf("SetStatus promoted: %v", err)
	}
	if promoted.Status != domain.ProposalPromoted || promoted.PromotedRecordID != "decision-2026-11-09-retry-budget" || promoted.PromotedAt.IsZero() {
		t.Fatalf("promoted = %+v", promoted)
	}

	store.now = func() time.Time { return later.Add(time.Hour) }
	terminal := changed
	terminal.Title = "A digest must not overwrite this"
	terminal.PayloadHash = terminal.ComputePayloadHash()
	effective, err := store.Put(ctx, terminal)
	if err != nil {
		t.Fatalf("Put terminal: %v", err)
	}
	if effective.Status != domain.ProposalPromoted || effective.Title != rewritten.Title {
		t.Fatalf("Put overwrote a terminal proposal: %+v", effective)
	}
	if _, err := store.SetStatus(ctx, project.ID, proposal.ID, domain.ProposalDismissed, ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("terminal status change error = %v, want ErrConflict", err)
	}

	// Repeating the same terminal status repairs the promoted record id.
	repaired, err := store.SetStatus(ctx, project.ID, proposal.ID, domain.ProposalPromoted, "decision-repaired")
	if err != nil {
		t.Fatalf("SetStatus repair: %v", err)
	}
	if repaired.PromotedRecordID != "decision-repaired" {
		t.Fatalf("repaired record id = %q", repaired.PromotedRecordID)
	}
}

func TestProposalSetStatusStaleAndRevive(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	proposal := testProposal(project.ID)
	if _, err := store.Put(ctx, proposal); err != nil {
		t.Fatalf("Put: %v", err)
	}
	stale, err := store.SetStatus(ctx, project.ID, proposal.ID, domain.ProposalStale, "")
	if err != nil {
		t.Fatalf("SetStatus stale: %v", err)
	}
	if stale.Status != domain.ProposalStale {
		t.Fatalf("stale status = %q", stale.Status)
	}
	// The same payload keeps an explicit stale marker.
	store.now = func() time.Time { return testClock.Add(time.Hour) }
	kept, err := store.Put(ctx, proposal)
	if err != nil {
		t.Fatalf("Put stale: %v", err)
	}
	if kept.Status != domain.ProposalStale {
		t.Fatalf("idempotent Put cleared stale: %q", kept.Status)
	}
	// New content revives a stale proposal as pending.
	changed := proposal
	changed.Confidence = 0.9
	changed.PayloadHash = changed.ComputePayloadHash()
	revived, err := store.Put(ctx, changed)
	if err != nil {
		t.Fatalf("Put revived: %v", err)
	}
	if revived.Status != domain.ProposalPending || !revived.CreatedAt.Equal(proposal.CreatedAt) {
		t.Fatalf("revived = %+v", revived)
	}
}

func TestProposalConcurrentPut(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	base := testProposal(project.ID)

	const writers = 8
	var wait sync.WaitGroup
	errs := make(chan error, writers)
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			proposal := base
			proposal.Title = fmt.Sprintf("Concurrent proposal %d", index)
			proposal.Confidence = float64(index) / 10
			proposal.PayloadHash = proposal.ComputePayloadHash()
			if _, err := store.Put(ctx, proposal); err != nil {
				errs <- err
			}
		}(index)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Put: %v", err)
	}
	got, err := store.Get(ctx, project.ID, base.ID)
	if err != nil {
		t.Fatalf("Get after concurrent Put: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("stored proposal is invalid: %v", err)
	}
	if !strings.HasPrefix(got.Title, "Concurrent proposal ") {
		t.Fatalf("final title = %q", got.Title)
	}
	if !got.CreatedAt.Equal(base.CreatedAt) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, base.CreatedAt)
	}
}

func TestProposalProblemsReportsMalformedFiles(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	dir := filepath.Join(store.Root(), "proposals", project.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	badPath := filepath.Join(dir, "proposal-gh-bad-0123456789ab.md")
	if err := os.WriteFile(badPath, []byte("# not a proposal\n"), 0o644); err != nil {
		t.Fatalf("write malformed proposal: %v", err)
	}
	if _, err := store.Put(ctx, testProposal(project.ID)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	list, err := store.List(ctx, project.ID, ports.ProposalListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List returned %d proposals, want the one valid proposal", len(list))
	}
	problems, err := store.ProposalProblems(ctx, project.ID)
	if err != nil {
		t.Fatalf("ProposalProblems: %v", err)
	}
	if len(problems) != 1 || problems[0].Kind != "proposal" || problems[0].Path != badPath {
		t.Fatalf("problems = %+v", problems)
	}
}

func TestProposalStoreErrors(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	proposal := testProposal(project.ID)

	if _, err := store.Get(ctx, project.ID, proposal.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get missing error = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(ctx, "missing-project", proposal.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get missing project error = %v, want ErrNotFound", err)
	}
	if _, err := store.List(ctx, "missing-project", ports.ProposalListOptions{}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("List missing project error = %v, want ErrNotFound", err)
	}
	if _, err := store.SetStatus(ctx, project.ID, proposal.ID, domain.ProposalPromoted, "decision-x"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("SetStatus missing error = %v, want ErrNotFound", err)
	}
	terminal := proposal
	terminal.Status = domain.ProposalPromoted
	terminal.PromotedRecordID = "decision-x"
	if _, err := store.Put(ctx, terminal); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Put terminal error = %v, want validation error", err)
	}
	if _, err := store.List(ctx, project.ID, ports.ProposalListOptions{Limit: -1}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("List negative limit error = %v, want validation error", err)
	}
	if _, err := store.List(ctx, project.ID, ports.ProposalListOptions{Kind: "task"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("List bad kind error = %v, want validation error", err)
	}
}

func TestCurrentRevisionIgnoresProposals(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	before, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision before: %v", err)
	}
	if _, err := store.Put(ctx, testProposal(project.ID)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	after, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision after: %v", err)
	}
	if before.Value != after.Value {
		t.Fatalf("proposals changed the curated revision: %s -> %s", before.Value, after.Value)
	}
}
