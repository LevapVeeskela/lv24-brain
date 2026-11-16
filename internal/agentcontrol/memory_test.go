package agentcontrol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/localapi"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/daemon"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// memoryFixture is a real Markdown brain plus the memory service under test.
type memoryFixture struct {
	app         *app.App
	store       *fsstore.Store
	connections *fsstore.ConnectionStore
	clock       time.Time
	service     *memoryService
}

func newMemoryFixture(t *testing.T) *memoryFixture {
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
	index, err := sqliteindex.Open(filepath.Join(dataDir, "index", "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	clock := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.User = "owner"
	instance := app.New(cfg, app.Dependencies{
		Store: store, Index: index, Connections: connections,
		Items: store.Items(), Proposals: store,
		Now: func() time.Time { return clock },
	})
	if _, err := instance.SaveProject(ctx, app.ProjectInput{ID: "demo"}); err != nil {
		t.Fatal(err)
	}
	return &memoryFixture{
		app: instance, store: store, connections: connections, clock: clock,
		service: newMemoryService(instance, func() time.Time { return clock }),
	}
}

func (f *memoryFixture) note(t *testing.T, title, body string, provenance domain.Provenance) domain.Note {
	t.Helper()
	result, err := f.app.Note(context.Background(), app.NoteInput{
		ProjectID: "demo", Title: title, Body: body, AllowDuplicate: true, Provenance: provenance,
	})
	if err != nil {
		t.Fatalf("note %q: %v", title, err)
	}
	return result.Note
}

func (f *memoryFixture) addIssue(t *testing.T, externalID, title string) {
	t.Helper()
	item := domain.Item{
		Ref:        domain.ItemRef{ConnectionID: "gh", ExternalID: externalID},
		ProjectID:  "demo",
		Kind:       "issue",
		Title:      title,
		URL:        "https://example.test/issues/" + externalID,
		Labels:     []string{"decision"},
		ObservedAt: f.clock,
	}
	item.Hash = item.ComputeHash()
	if _, err := f.store.Items().Upsert(context.Background(), item); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryServiceRecordsDetailLinksAndSearch(t *testing.T) {
	fixture := newMemoryFixture(t)
	ctx := context.Background()
	note := fixture.note(t, "Cache design", "use one sqlite index for the search cache with bm25 ranking", domain.Provenance{})
	decisionResult, err := fixture.app.Decision(ctx, app.DecisionInput{
		ProjectID: "demo", Title: "Use SQLite", Status: domain.DecisionAccepted,
		Context: "see [[" + note.ID + "]] for the ranking",
	})
	if err != nil {
		t.Fatal(err)
	}
	backlink := fixture.note(t, "Follow up", "the decision [["+decisionResult.Decision.ID+"]] still holds", domain.Provenance{})

	rows, err := fixture.service.ListRecords(ctx, localapi.RecordQuery{ProjectID: "demo", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	byID := map[string]localapi.RecordView{}
	for _, row := range rows {
		byID[row.ID] = row
		if row.ProjectID != "demo" || row.Path == "" || !row.Validity.Current {
			t.Fatalf("row = %+v", row)
		}
		if row.Provenance.Origin != "human" || !row.Provenance.Reviewed || row.Provenance.Quarantined {
			t.Fatalf("provenance = %+v", row.Provenance)
		}
		if row.Review.ReviewAfter.IsZero() || row.Review.ReviewSource == "" {
			t.Fatalf("review = %+v", row.Review)
		}
	}
	decisionRow := byID[decisionResult.Decision.ID]
	if decisionRow.Kind != "decision" || decisionRow.Status != "accepted" {
		t.Fatalf("decision row = %+v", decisionRow)
	}
	if decisionRow.Path != "projects/demo/decisions/"+decisionResult.Decision.ID+".md" {
		t.Fatalf("decision path = %q", decisionRow.Path)
	}

	detail, err := fixture.service.RecordDetail(ctx, localapi.RecordDetailQuery{
		ProjectID: "demo", ID: decisionResult.Decision.ID, MaxTokens: 500, History: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if detail.Body == "" || !strings.Contains(detail.Body, "ranking") || detail.MaxTokens != 500 {
		t.Fatalf("detail body = %+v", detail)
	}
	if len(detail.Links) != 1 || detail.Links[0].State != "found" || detail.Links[0].Target == nil ||
		detail.Links[0].Target.ID != note.ID {
		t.Fatalf("links = %+v", detail.Links)
	}
	if len(detail.Backlinks) != 1 || detail.Backlinks[0].Source.ID != backlink.ID {
		t.Fatalf("backlinks = %+v", detail.Backlinks)
	}
	if detail.History == nil || detail.History.Total < 1 {
		t.Fatalf("history = %+v", detail.History)
	}
	if got, err := fixture.service.RecordDetail(ctx, localapi.RecordDetailQuery{ProjectID: "demo", ID: "decision-missing", MaxTokens: 100}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing detail = %+v, %v", got, err)
	}

	search, err := fixture.service.Search(ctx, localapi.SearchQuery{Text: "sqlite index", ProjectID: "demo", Limit: 10, Explain: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(search.Hits) == 0 {
		t.Fatal("search returned no hits")
	}
	for _, hit := range search.Hits {
		if hit.Preview == "" || len(hit.MatchReasons) == 0 {
			t.Fatalf("hit = %+v", hit)
		}
		if hit.Kind == "dump" || hit.Kind == "item" {
			t.Fatalf("raw hit leaked: %+v", hit)
		}
	}
	for _, kinds := range [][]string{{"dump"}, {"item"}, {"wrong"}} {
		if _, err := fixture.service.Search(ctx, localapi.SearchQuery{Text: "x", ProjectID: "demo", Kinds: kinds}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("kind %v = %v, want validation", kinds, err)
		}
	}
}

func TestMemoryServiceStateCASAcrossReads(t *testing.T) {
	fixture := newMemoryFixture(t)
	ctx := context.Background()
	state, err := fixture.service.State(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != "none" {
		t.Fatalf("empty revision = %q", state.Revision)
	}
	goal, next := "ship 1.2", "wire the API"
	updated, err := fixture.service.SetState(ctx, "demo", localapi.StateWrite{
		Goal: &goal, NextAction: &next, ExpectedRevision: state.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Goal != goal || updated.Revision == "none" {
		t.Fatalf("updated = %+v", updated)
	}
	_, err = fixture.service.SetState(ctx, "demo", localapi.StateWrite{Goal: &goal, ExpectedRevision: state.Revision})
	var conflict *app.StateRevisionConflict
	if !errors.As(err, &conflict) || conflict.Current != updated.Revision {
		t.Fatalf("stale write = %v", err)
	}
	fresh := "clear the blockers"
	if _, err := fixture.service.SetState(ctx, "demo", localapi.StateWrite{NextAction: &fresh, ExpectedRevision: updated.Revision}); err != nil {
		t.Fatal(err)
	}
	// fresh_until: an RFC3339 value sets it, an empty string clears it.
	until := fixture.clock.Add(48 * time.Hour).Format(time.RFC3339)
	value := until
	withFresh, err := fixture.service.SetState(ctx, "demo", localapi.StateWrite{FreshUntil: &value})
	if err != nil {
		t.Fatal(err)
	}
	if !withFresh.FreshUntil.Equal(time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("fresh_until = %s", withFresh.FreshUntil)
	}
	empty := ""
	cleared, err := fixture.service.SetState(ctx, "demo", localapi.StateWrite{FreshUntil: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if !cleared.FreshUntil.IsZero() {
		t.Fatalf("fresh_until was not cleared: %s", cleared.FreshUntil)
	}
	wrong := "tomorrow"
	if _, err := fixture.service.SetState(ctx, "demo", localapi.StateWrite{FreshUntil: &wrong}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid fresh_until = %v", err)
	}
}

func TestMemoryServiceReviewProposalsAndCatchup(t *testing.T) {
	fixture := newMemoryFixture(t)
	ctx := context.Background()
	quarantined := fixture.note(t, "Imported claim", "external text", domain.Provenance{
		Origin: domain.OriginAgent, Reviewed: false, Trust: domain.TrustStated,
	})
	reviewed := fixture.note(t, "Reviewed note", "owner text", domain.Provenance{})
	past := fixture.clock.Add(-24 * time.Hour)
	if _, err := fixture.app.ReviewRecords(ctx, app.ReviewRecordOptions{ProjectID: "demo", ID: reviewed.ID, ReviewAfter: &past}); err != nil {
		t.Fatal(err)
	}

	if err := fixture.connections.Create(ctx, domain.Connection{
		ID: "gh", Provider: "github", Projects: []string{"demo"}, Publish: domain.PublishLocalOnly,
	}); err != nil {
		t.Fatal(err)
	}
	fixture.addIssue(t, "issue-1", "Adopt SQLite")
	if _, err := fixture.app.ProposalDigest(ctx, app.ProposalDigestOptions{
		ConnectionID: "gh", ProjectID: "demo", All: true, Write: true,
	}); err != nil {
		t.Fatal(err)
	}

	review, err := fixture.service.Review(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if review.QuarantinedCount != 1 {
		t.Fatalf("quarantined = %d", review.QuarantinedCount)
	}
	if review.Proposals.PendingCount != 1 || len(review.Proposals.Items) != 1 {
		t.Fatalf("pending proposals = %+v", review.Proposals)
	}
	if len(review.DueReviews) == 0 {
		t.Fatal("due review was not reported")
	}
	dueRows, err := fixture.service.ListRecords(ctx, localapi.RecordQuery{ProjectID: "demo", DueReview: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(dueRows) != 1 || dueRows[0].ID != reviewed.ID || !dueRows[0].Review.Due {
		t.Fatalf("due records = %+v", dueRows)
	}
	foundQuarantine := false
	for _, finding := range review.Findings {
		if finding.Severity == "error" && finding.Kind == "quarantined" && finding.Why != "" && finding.Next != "" && finding.Confirm != "" {
			foundQuarantine = true
		}
	}
	if !foundQuarantine {
		t.Fatalf("quarantined finding = %+v", review.Findings)
	}
	if len(review.Findings) >= 2 && severityRank(review.Findings[0].Severity) > severityRank(review.Findings[1].Severity) {
		t.Fatalf("findings are not severity ordered: %+v", review.Findings)
	}

	page, err := fixture.service.ListProposals(ctx, localapi.ProposalQuery{ProjectID: "demo", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("proposal page = %+v", page)
	}
	proposal := page.Items[0]
	if proposal.Status != "pending" || !proposal.LocalOnly || proposal.SourceOrphan || proposal.SourceStale {
		t.Fatalf("proposal = %+v", proposal)
	}
	if _, err := fixture.service.ListProposals(ctx, localapi.ProposalQuery{ProjectID: "demo", Statuses: []string{"wrong"}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad status = %v", err)
	}

	// A local-only source is refused without yes and promoted with it.
	if _, err := fixture.service.Promote(ctx, localapi.ProposalDecision{ProjectID: "demo", ProposalID: proposal.ID}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("promote without yes = %v, want validation", err)
	}
	promoted, err := fixture.service.Promote(ctx, localapi.ProposalDecision{
		ProjectID: "demo", ProposalID: proposal.ID, Yes: true, Title: "Adopt SQLite locally",
	})
	if err != nil {
		t.Fatal(err)
	}
	if promoted.ProposalStatus != "promoted" || promoted.RecordID == "" || promoted.Already || promoted.Dismissed {
		t.Fatalf("promotion = %+v", promoted)
	}
	again, err := fixture.service.Promote(ctx, localapi.ProposalDecision{ProjectID: "demo", ProposalID: proposal.ID})
	if err != nil || !again.Already || again.RecordID != promoted.RecordID {
		t.Fatalf("idempotent promotion = %+v, %v", again, err)
	}
	// The engine keeps the CLI semantics: a terminal proposal is never moved
	// to another state. Dismissing a promoted proposal is a no-op receipt;
	// promoting a dismissed one is a conflict.
	noop, err := fixture.service.Promote(ctx, localapi.ProposalDecision{ProjectID: "demo", ProposalID: proposal.ID, Dismiss: true})
	if err != nil || !noop.Already || noop.Dismissed || noop.ProposalStatus != "promoted" {
		t.Fatalf("dismiss after promote = %+v, %v", noop, err)
	}
	if _, err := fixture.service.Promote(ctx, localapi.ProposalDecision{ProjectID: "demo", ProposalID: "proposal-gh-000000000000", Dismiss: true}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("dismiss missing = %v, want not found", err)
	}

	catchup, err := fixture.service.Catchup(ctx, localapi.CatchupQuery{
		ProjectID: "demo", Since: fixture.clock.Add(-time.Hour), Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if catchup.SinceSource != "explicit" || len(catchup.Notes) == 0 {
		t.Fatalf("catchup = %+v", catchup)
	}
	if catchup.QuarantinedCount == 0 {
		t.Fatalf("catchup quarantined = %+v", catchup.QuarantinedCount)
	}
	if catchup.State.Revision == "" {
		t.Fatal("catchup state has no revision")
	}
	if quarantined.ID == "" {
		t.Fatal("quarantined note was not created")
	}
}

func TestJournalViewKeepsOnlySafeCommitFields(t *testing.T) {
	entry := ports.JournalEntry{
		Time: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC), Event: "commit", Project: "demo",
		ID: "abc", Title: "commit",
		Fields: map[string]string{
			"branch": "main", "head": "abc123", "subject": "feat: x",
			"remote": "https://user:secret@example.test/repo.git",
		},
	}
	view := journalView(entry)
	if view.Fields["branch"] != "main" || view.Fields["head"] != "abc123" || view.Fields["subject"] != "feat: x" {
		t.Fatalf("journal view = %+v", view)
	}
	if _, ok := view.Fields["remote"]; ok {
		t.Fatalf("remote leaked into the journal view: %+v", view.Fields)
	}
}

// TestControlEndToEndMemoryRoutes proves the composition: a running agent with
// the memory use cases wired serves the record, state, review and catchup routes
// over the same authenticated loopback listener as the connection routes.
func TestControlEndToEndMemoryRoutes(t *testing.T) {
	dir := t.TempDir()
	store, err := fsstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	connections, err := fsstore.NewConnectionStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	index, err := sqliteindex.Open(filepath.Join(dir, "index", "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.User = "owner"
	instance := app.New(cfg, app.Dependencies{
		Store: store, Index: index, Connections: connections,
		Items: store.Items(), Proposals: store,
	})
	ctx := context.Background()
	if _, err := instance.SaveProject(ctx, app.ProjectInput{ID: "demo"}); err != nil {
		t.Fatal(err)
	}
	note, err := instance.Note(ctx, app.NoteInput{ProjectID: "demo", Title: "Memory route note", Body: "hello from the agent"})
	if err != nil {
		t.Fatal(err)
	}

	secrets := &fakeSecrets{}
	registry := connectorbuild.NewRegistry()
	if err := registry.Register(fixtureSource{}.Descriptor(), func(domain.Connection, connectorbuild.Deps) (ports.Source, error) {
		return fixtureSource{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	control, err := New(Options{
		DataDir: dir, Connections: connections, Secrets: secrets, Registry: registry,
		Deps:     connectorbuild.Deps{Credentials: connectorbuild.SecretCredentials{Store: secrets}},
		Projects: fakeProjects{projects: []domain.Project{{ID: "demo", Name: "Demo"}}},
		Updater:  fakeUpdater{}, Version: "9.9.9", App: instance,
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := daemon.New(daemon.Options{
		DataDir: dir, Version: "9.9.9", Connections: connections,
		Runner: &recordingRunner{}, Control: control,
	})
	if err != nil {
		t.Fatal(err)
	}
	control.BindAgent(engine)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop")
		}
	})
	runtime := waitRuntime(t, dir, done)
	api := apiClient{t: t, base: fmt.Sprintf("http://127.0.0.1:%d", runtime.Port), token: runtime.Token}

	resp, body := api.do(http.MethodGet, "/v1/records?project=demo&limit=10", "", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(note.Note.ID)) {
		t.Fatalf("records = %d %s", resp.StatusCode, body)
	}
	resp, body = api.do(http.MethodGet, "/v1/records/"+note.Note.ID+"?project=demo&max_tokens=200", "", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("hello from the agent")) {
		t.Fatalf("record detail = %d %s", resp.StatusCode, body)
	}
	resp, body = api.do(http.MethodGet, "/v1/search?project=demo&q=hello", "", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(note.Note.ID)) {
		t.Fatalf("search = %d %s", resp.StatusCode, body)
	}

	resp, body = api.do(http.MethodGet, "/v1/projects/demo/state", "", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"revision":"none"`)) {
		t.Fatalf("state = %d %s", resp.StatusCode, body)
	}
	resp, body = api.do(http.MethodPut, "/v1/projects/demo/state", `{"goal":"ship 1.2","expected_revision":"none"}`, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"ship 1.2"`)) {
		t.Fatalf("state put = %d %s", resp.StatusCode, body)
	}
	resp, body = api.do(http.MethodPut, "/v1/projects/demo/state", `{"goal":"stale","expected_revision":"none"}`, nil)
	if resp.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte(`"current_revision"`)) {
		t.Fatalf("state CAS = %d %s", resp.StatusCode, body)
	}

	resp, body = api.do(http.MethodGet, "/v1/review?project=demo", "", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"findings"`)) || !bytes.Contains(body, []byte(`"quarantined_count"`)) {
		t.Fatalf("review = %d %s", resp.StatusCode, body)
	}
	resp, body = api.do(http.MethodGet, "/v1/proposals?project=demo", "", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"items":[]`)) {
		t.Fatalf("proposals = %d %s", resp.StatusCode, body)
	}
	resp, body = api.do(http.MethodGet, "/v1/catchup?project=demo&since=24h", "", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"since_source":"explicit"`)) {
		t.Fatalf("catchup = %d %s", resp.StatusCode, body)
	}
	resp, _ = api.do(http.MethodPost, "/v1/proposals/demo/proposal-gh-000000000000/promote", "{}", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing proposal = %d", resp.StatusCode)
	}
}
