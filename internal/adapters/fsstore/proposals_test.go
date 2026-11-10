package fsstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
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
	wantPath := "proposals/" + project.ID + "/" + filepath.Base(badPath)
	if len(problems) != 1 || problems[0].Kind != "proposal" || problems[0].Path != wantPath {
		t.Fatalf("problems = %+v, want one relative path %q", problems, wantPath)
	}
	if problems[0].Reason != proposalReasonMalformed {
		t.Fatalf("reason = %q, want stable malformed code", problems[0].Reason)
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

func proposalTestPath(store *Store, projectID, id string) string {
	return filepath.Join(store.Root(), proposalsDir, projectID, id+proposalFileExt)
}

// editProposalFile rewrites the proposal file with edit applied to its lines.
func editProposalFile(t *testing.T, path string, edit func([]string) []string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read proposal: %v", err)
	}
	lines := edit(strings.Split(string(raw), "\n"))
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatalf("write proposal: %v", err)
	}
}

func removeProposalField(t *testing.T, path, field string) {
	t.Helper()
	editProposalFile(t, path, func(lines []string) []string {
		kept := make([]string, 0, len(lines))
		for _, line := range lines {
			if strings.HasPrefix(line, field+":") {
				continue
			}
			kept = append(kept, line)
		}
		return kept
	})
}

func replaceProposalField(t *testing.T, path, field, value string) {
	t.Helper()
	editProposalFile(t, path, func(lines []string) []string {
		for index, line := range lines {
			trimmed := strings.TrimLeft(line, " \t")
			if !strings.HasPrefix(trimmed, field+":") {
				continue
			}
			indent := line[:len(line)-len(trimmed)]
			lines[index] = indent + field + ": " + value
		}
		return lines
	})
}

// TestProposalNegativeZeroConfidenceRoundTrip: Validate accepts -0.0, the
// encoder omits a zero confidence, and the read-back hash must still match.
func TestProposalNegativeZeroConfidenceRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	proposal := testProposal(project.ID)
	proposal.Confidence = math.Copysign(0, -1)
	proposal.PayloadHash = proposal.ComputePayloadHash()

	stored, err := store.Put(ctx, proposal)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if math.Signbit(stored.Confidence) {
		t.Fatalf("Put kept negative zero in memory: %v", stored.Confidence)
	}
	got, err := store.Get(ctx, project.ID, proposal.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Confidence != 0 || got.PayloadHash != proposal.PayloadHash {
		t.Fatalf("read back confidence %v hash %s, want 0 and %s", got.Confidence, got.PayloadHash, proposal.PayloadHash)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("read back Validate: %v", err)
	}
}

// TestProposalPutPreservesDamagedTerminalFile: deleting the status line makes
// the file unreadable as a proposal. Put must not silently rewrite it as a
// fresh pending proposal in place.
func TestProposalPutPreservesDamagedTerminalFile(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	proposal := testProposal(project.ID)
	if _, err := store.Put(ctx, proposal); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := store.SetStatus(ctx, project.ID, proposal.ID, domain.ProposalPromoted, "decision-keep"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	path := proposalTestPath(store, project.ID, proposal.ID)
	removeProposalField(t, path, "status")

	changed := proposal
	changed.Title = "A digest must not reset this"
	changed.PayloadHash = changed.ComputePayloadHash()
	_, err := store.Put(ctx, changed)
	if err == nil {
		t.Fatal("Put rewrote a proposal file that no longer parses")
	}
	var invalid *proposalInvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("Put error = %v, want proposalInvalidError", err)
	}

	preserved, globErr := filepath.Glob(path + proposalBadInfix + "*")
	if globErr != nil {
		t.Fatalf("glob preserved: %v", globErr)
	}
	if len(preserved) != 1 {
		t.Fatalf("preserved files = %v, want exactly one", preserved)
	}
	raw, readErr := os.ReadFile(preserved[0])
	if readErr != nil {
		t.Fatalf("read preserved: %v", readErr)
	}
	if !strings.Contains(string(raw), "decision-keep") || !strings.Contains(string(raw), "promoted") {
		t.Fatalf("preserved file lost its terminal state:\n%s", raw)
	}
	if _, err := store.Get(ctx, project.ID, proposal.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Get after quarantine error = %v, want ErrNotFound", err)
	}

	// The next run is not permanently blocked: the path is free again and a
	// fresh pending proposal is written. The original is still preserved.
	recreated, err := store.Put(ctx, changed)
	if err != nil {
		t.Fatalf("Put after quarantine: %v", err)
	}
	if recreated.Status != domain.ProposalPending {
		t.Fatalf("recreated status = %q, want pending", recreated.Status)
	}
}

// TestProposalPutPropagatesIOErrors: a directory at the proposal path is an
// I/O failure, not parse damage, so Put must neither quarantine nor overwrite.
func TestProposalPutPropagatesIOErrors(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	proposal := testProposal(project.ID)
	path := proposalTestPath(store, project.ID, proposal.ID)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	_, err := store.Put(ctx, proposal)
	if err == nil {
		t.Fatal("Put ignored an I/O error")
	}
	var invalid *proposalInvalidError
	if errors.As(err, &invalid) {
		t.Fatalf("Put treated an I/O error as parse damage: %v", err)
	}
	info, statErr := os.Stat(path)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("Put disturbed the I/O failure at the path: %v %v", info, statErr)
	}
	preserved, globErr := filepath.Glob(path + proposalBadInfix + "*")
	if globErr != nil || len(preserved) != 0 {
		t.Fatalf("preserved files = %v, %v; want none", preserved, globErr)
	}
}

// TestProposalReadRequiresCanonicalFields: a hand edit that drops any canonical
// field must hide the proposal and be reported, not produce a zero-valued one.
func TestProposalReadRequiresCanonicalFields(t *testing.T) {
	for _, field := range []string{"status", "payload_hash", "created_at", "updated_at"} {
		t.Run(field, func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()
			project := seedProject(t, store)
			proposal := testProposal(project.ID)
			if _, err := store.Put(ctx, proposal); err != nil {
				t.Fatalf("Put: %v", err)
			}
			path := proposalTestPath(store, project.ID, proposal.ID)
			removeProposalField(t, path, field)

			if _, err := store.Get(ctx, project.ID, proposal.ID); err == nil {
				t.Fatalf("Get accepted a proposal without %s", field)
			}
			list, err := store.List(ctx, project.ID, ports.ProposalListOptions{})
			if err != nil || len(list) != 0 {
				t.Fatalf("List = %d, %v; want empty", len(list), err)
			}
			problems, err := store.ProposalProblems(ctx, project.ID)
			if err != nil {
				t.Fatalf("ProposalProblems: %v", err)
			}
			if len(problems) != 1 || problems[0].Reason != proposalReasonMissing {
				t.Fatalf("problems = %+v, want one missing-field problem", problems)
			}
		})
	}
}

// TestProposalOptionalTimestampsAreTolerant: space-separated or unparseable
// informational timestamps must not hide a proposal.
func TestProposalOptionalTimestampsAreTolerant(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	proposal := testProposal(project.ID)
	if _, err := store.Put(ctx, proposal); err != nil {
		t.Fatalf("Put: %v", err)
	}
	path := proposalTestPath(store, project.ID, proposal.ID)

	replaceProposalField(t, path, "observed_at", `"2026-11-04 08:15:30"`)
	got, err := store.Get(ctx, project.ID, proposal.ID)
	if err != nil {
		t.Fatalf("Get with space-separated observed_at: %v", err)
	}
	wantObserved := time.Date(2026, 11, 4, 8, 15, 30, 0, time.UTC)
	if len(got.Citations) != 1 || !got.Citations[0].ObservedAt.Equal(wantObserved) {
		t.Fatalf("citations = %+v, want observed_at %v", got.Citations, wantObserved)
	}

	replaceProposalField(t, path, "observed_at", `"not a timestamp"`)
	got, err = store.Get(ctx, project.ID, proposal.ID)
	if err != nil {
		t.Fatalf("Get with unparseable observed_at: %v", err)
	}
	if len(got.Citations) != 1 || !got.Citations[0].ObservedAt.IsZero() {
		t.Fatalf("citations = %+v, want a zero observed_at", got.Citations)
	}

	if _, err := store.SetStatus(ctx, project.ID, proposal.ID, domain.ProposalPromoted, "decision-1"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	replaceProposalField(t, path, "promoted_at", "2026-11-05T10:30:00Z")
	promoted, err := store.Get(ctx, project.ID, proposal.ID)
	if err != nil {
		t.Fatalf("Get with unquoted promoted_at: %v", err)
	}
	if !promoted.PromotedAt.Equal(testClock) {
		t.Fatalf("promoted_at = %v, want %v", promoted.PromotedAt, testClock)
	}
	replaceProposalField(t, path, "promoted_at", `"2026-11-05 10:30:00"`)
	promoted, err = store.Get(ctx, project.ID, proposal.ID)
	if err != nil {
		t.Fatalf("Get with space-separated promoted_at: %v", err)
	}
	if !promoted.PromotedAt.Equal(testClock) {
		t.Fatalf("promoted_at = %v, want %v", promoted.PromotedAt, testClock)
	}
	replaceProposalField(t, path, "promoted_at", `"not a timestamp"`)
	promoted, err = store.Get(ctx, project.ID, proposal.ID)
	if err != nil {
		t.Fatalf("Get with unparseable promoted_at: %v", err)
	}
	if !promoted.PromotedAt.IsZero() {
		t.Fatalf("promoted_at = %v, want zero", promoted.PromotedAt)
	}
}

// TestProposalSetStatusIgnoresRecordIDForNonPromoted: the port documents
// recordID as ignored outside promotion, so a stale caller must not fail and
// must not store a promoted record id.
func TestProposalSetStatusIgnoresRecordIDForNonPromoted(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	proposal := testProposal(project.ID)
	if _, err := store.Put(ctx, proposal); err != nil {
		t.Fatalf("Put: %v", err)
	}
	stale, err := store.SetStatus(ctx, project.ID, proposal.ID, domain.ProposalStale, "decision-ignored")
	if err != nil {
		t.Fatalf("SetStatus stale with recordID: %v", err)
	}
	if stale.Status != domain.ProposalStale || stale.PromotedRecordID != "" || !stale.PromotedAt.IsZero() {
		t.Fatalf("stale = %+v, want no promoted fields", stale)
	}
	again, err := store.SetStatus(ctx, project.ID, proposal.ID, domain.ProposalStale, "decision-other")
	if err != nil {
		t.Fatalf("repeated SetStatus stale: %v", err)
	}
	if !again.UpdatedAt.Equal(stale.UpdatedAt) {
		t.Fatalf("ignored recordID caused a rewrite: %v -> %v", stale.UpdatedAt, again.UpdatedAt)
	}
}

// TestProposalPutRecomputesHashAfterDerivingID: a caller may hash a proposal
// before Put derives its deterministic id; the hash covers the id, so Put must
// recompute it instead of rejecting the value.
func TestProposalPutRecomputesHashAfterDerivingID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	proposal := testProposal(project.ID)
	proposal.ID = ""
	proposal.PayloadHash = proposal.ComputePayloadHash()

	stored, err := store.Put(ctx, proposal)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if stored.ID != domain.ProposalID(project.ID, proposal.ConnectionID, proposal.ExternalID) {
		t.Fatalf("id = %q", stored.ID)
	}
	if stored.PayloadHash != stored.ComputePayloadHash() {
		t.Fatalf("payload hash = %q, want %q", stored.PayloadHash, stored.ComputePayloadHash())
	}
}

// TestProposalProblemsDoNotLeakPathsOrContent: diagnostics must use relative
// paths and stable codes, never the data root or hand-edited field text.
func TestProposalProblemsDoNotLeakPathsOrContent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	id := testProposal(project.ID).ID
	dir := filepath.Join(store.Root(), proposalsDir, project.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const marker = "secret-marker-do-not-leak"
	path := filepath.Join(dir, id+proposalFileExt)
	raw := fmt.Sprintf(`---
project_id: %q
kind: %q
updated_at: 2026-11-05T10:30:00Z
schema_version: 1
id: %q
proposal_kind: %q
rule: "closed-issue"
confidence: 0.5
connection_id: "gh-acme"
external_id: %q
title: %q
payload_hash: "sha256:%s"
status: "pending"
created_at: 2026-11-05T10:30:00Z
---
# Title
`, project.ID, proposalKindStorage, id, marker, marker, marker, strings.Repeat("0", 64))
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write leak proposal: %v", err)
	}

	_, err := store.Get(ctx, project.ID, id)
	if err == nil {
		t.Fatal("Get accepted a proposal with an unknown kind")
	}
	problems, problemsErr := store.ProposalProblems(ctx, project.ID)
	if problemsErr != nil {
		t.Fatalf("ProposalProblems: %v", problemsErr)
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want one", problems)
	}
	wantPath := proposalsDir + "/" + project.ID + "/" + id + proposalFileExt
	if problems[0].Path != wantPath {
		t.Fatalf("problem path = %q, want %q", problems[0].Path, wantPath)
	}
	for _, text := range []string{err.Error(), problems[0].Path, problems[0].Reason} {
		for _, leak := range []string{store.Root(), marker} {
			if strings.Contains(text, leak) {
				t.Fatalf("proposal diagnostics leaked %q in %q", leak, text)
			}
		}
	}
}

// TestProposalIgnoreFileSelfHeals: every proposal write verifies the catch-all
// rule, so a stale or missing .gitignore is repaired even by SetStatus.
func TestProposalIgnoreFileSelfHeals(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	proposal := testProposal(project.ID)
	if _, err := store.Put(ctx, proposal); err != nil {
		t.Fatalf("Put: %v", err)
	}
	ignorePath := filepath.Join(store.Root(), proposalsDir, proposalIgnoreFile)

	if err := os.WriteFile(ignorePath, []byte("# stale\n!*.md\n"), 0o644); err != nil {
		t.Fatalf("write stale ignore: %v", err)
	}
	changed := proposal
	changed.Title = "Repair the ignore file"
	changed.PayloadHash = changed.ComputePayloadHash()
	if _, err := store.Put(ctx, changed); err != nil {
		t.Fatalf("Put after stale ignore: %v", err)
	}
	raw, err := os.ReadFile(ignorePath)
	if err != nil || !proposalsIgnoreCoversAll(raw) {
		t.Fatalf("ignore after Put = %q, %v; want a catch-all rule", raw, err)
	}

	if err := os.Remove(ignorePath); err != nil {
		t.Fatalf("remove ignore: %v", err)
	}
	if _, err := store.SetStatus(ctx, project.ID, proposal.ID, domain.ProposalStale, ""); err != nil {
		t.Fatalf("SetStatus after missing ignore: %v", err)
	}
	raw, err = os.ReadFile(ignorePath)
	if err != nil || !proposalsIgnoreCoversAll(raw) {
		t.Fatalf("ignore after SetStatus = %q, %v; want a catch-all rule", raw, err)
	}
}

// TestProposalIgnoreFileKeepsProposalsOutOfGit proves the repaired ignore file
// is effective for a real repository, not just present on disk.
func TestProposalIgnoreFileKeepsProposalsOutOfGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	dir := filepath.Join(store.Root(), proposalsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	ignorePath := filepath.Join(dir, proposalIgnoreFile)
	if err := os.WriteFile(ignorePath, []byte("!*.md\n"), 0o644); err != nil {
		t.Fatalf("write stale ignore: %v", err)
	}
	runGit := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", store.Root()}, args...)...)
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return string(out)
	}
	runGit("init", "-q")
	status := func() string {
		return runGit("status", "--porcelain", "--untracked-files=all", "--", proposalsDir)
	}

	if _, err := store.Put(ctx, testProposal(project.ID)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if out := status(); strings.TrimSpace(out) != "" {
		t.Fatalf("proposals are visible to git after Put:\n%s", out)
	}

	if err := os.Remove(ignorePath); err != nil {
		t.Fatalf("remove ignore: %v", err)
	}
	proposal := testProposal(project.ID)
	if _, err := store.SetStatus(ctx, project.ID, proposal.ID, domain.ProposalStale, ""); err != nil {
		t.Fatalf("SetStatus after missing ignore: %v", err)
	}
	if out := status(); strings.TrimSpace(out) != "" {
		t.Fatalf("proposals are visible to git after SetStatus:\n%s", out)
	}
}
