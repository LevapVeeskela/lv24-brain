package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var ingestionTestTime = time.Date(2026, 11, 9, 12, 0, 0, 123456789, time.FixedZone("test", 3*60*60))

func TestIngestPolicyRedactsPIITruncatesAndNormalizesTimes(t *testing.T) {
	policy := newIngestPolicy(domain.Connection{ID: "mail-work", Projects: []string{"lv24"}}, domain.SourceDescriptor{PersonalData: true})
	item := domain.Item{
		Ref: domain.ItemRef{ExternalID: "m-1"}, Kind: domain.ItemMessage,
		Title: "From alice@example.com", Body: "token=abcdefghijklmnop alice@example.com " + strings.Repeat("x", domain.MaxItemBodyBytes),
		URL: "https://example.test/?access_token=abcdefghijklmnop", Author: "alice@example.com",
		UpdatedAt: ingestionTestTime,
	}
	got, report, err := policy.normalizeItem(item, ingestionTestTime)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref.ConnectionID != "mail-work" || got.ProjectID != "lv24" {
		t.Fatalf("host fields not normalized: %+v", got)
	}
	if got.CreatedAt.Location() != time.UTC || got.UpdatedAt.Location() != time.UTC || got.ObservedAt.Location() != time.UTC || got.UpdatedAt.Nanosecond()%1000 != 0 {
		t.Fatalf("timestamps not normalized to UTC microseconds: created=%s updated=%s observed=%s", got.CreatedAt, got.UpdatedAt, got.ObservedAt)
	}
	if strings.Contains(got.Body, "abcdefghijklmnop") || strings.Contains(got.Body, "alice@example.com") || !strings.Contains(got.Body, "[truncated by lv24-brain") {
		t.Fatalf("body policy not applied: suffix %q", got.Body[len(got.Body)-100:])
	}
	if strings.Contains(got.Title, "alice@example.com") || strings.Contains(got.Author, "alice@example.com") || strings.Contains(got.URL, "abcdefghijklmnop") {
		t.Fatalf("PII or secret remained: %+v", got)
	}
	if report.bodyTruncated != 1 || report.emails != 3 || report.redactions != 2 {
		t.Fatalf("policy report = %+v", report)
	}
	if got.Hash != got.ComputeHash() || got.Validate() != nil {
		t.Fatalf("normalized item hash invalid: %+v", got)
	}
}

func TestIngestionPersistsPayloadBeforeCheckpointAndReportsPartialRead(t *testing.T) {
	ctx := context.Background()
	connection := domain.Connection{ID: "mail-work", Provider: "sample", Projects: []string{"lv24"}}
	checkpoint := domain.Checkpoint{Stream: "inbox", Version: 1, Data: []byte("next")}
	item := domain.Item{Ref: domain.ItemRef{ConnectionID: connection.ID, ExternalID: "m-1"}, Kind: domain.ItemMessage, Title: "token=abcdefghijklmnop alice@example.com"}
	item.Body = strings.Repeat("x", domain.MaxItemBodyBytes+16)
	source := &sequenceSource{descriptor: ingestionDescriptor(true), batches: []ports.ReadBatch{{Items: []domain.Item{item}, Checkpoints: []domain.Checkpoint{checkpoint}, More: true}}, errAt: 1, err: errors.New("later page failed")}
	store := &recordingItemStore{}
	checkpoints := &recordingCheckpointStore{events: &[]string{}}
	store.events = checkpoints.events
	application := ingestionFixture(t, connection, source, store, checkpoints, nil)

	result, err := application.RunConnection(ctx, RunConnectionOptions{ConnectionID: connection.ID})
	if err != nil {
		t.Fatalf("partial read returned error: %v", err)
	}
	if result.Run.Status != domain.ConnectorPartial || result.Run.Stats == nil {
		t.Fatalf("run = %+v", result.Run)
	}
	if err := result.Run.Stats.Validate(); err != nil {
		t.Fatalf("run stats invalid: %+v: %v", result.Run.Stats, err)
	}
	if result.Run.Counts[CountBodiesTruncated] != 1 || result.Run.Counts[CountEmailsHashed] != 1 || result.Run.Counts[CountRedactions] != 1 {
		t.Fatalf("policy counters = %+v", result.Run.Counts)
	}
	if len(store.items) != 1 || strings.Contains(store.items[0].Title, "abcdefghijklmnop") || strings.Contains(store.items[0].Title, "alice@example.com") {
		t.Fatalf("stored items = %+v", store.items)
	}
	if len(checkpoints.values) != 1 || *checkpoints.events == nil || strings.Join(*checkpoints.events, ",") != "payload,checkpoint" {
		t.Fatalf("events=%v checkpoints=%+v", *checkpoints.events, checkpoints.values)
	}
}

func TestPreparePublishRunsAfterPolicyAndBeforePayloadEveryRun(t *testing.T) {
	connection := domain.Connection{ID: "mail-work", Provider: "sample"}
	source := &sequenceSource{descriptor: ingestionDescriptor(false), batches: []ports.ReadBatch{{Items: []domain.Item{{Ref: domain.ItemRef{ConnectionID: connection.ID, ExternalID: "m-1"}, Kind: domain.ItemMessage, Title: "subject"}}}}}
	events := []string{}
	items := &publishAwareItemStore{recordingItemStore: &recordingItemStore{events: &events}, policy: domain.PublishShared}
	checkpoints := &recordingCheckpointStore{events: &events}
	application := ingestionFixture(t, connection, source, items, checkpoints, nil)
	application.PreparePublish = func(_ context.Context, id string, policy domain.PublishPolicy) error {
		if id != connection.ID || policy != domain.PublishLocalOnly {
			t.Fatalf("PreparePublish(%q, %q)", id, policy)
		}
		events = append(events, "prepare")
		return nil
	}
	result, err := application.RunConnection(context.Background(), RunConnectionOptions{ConnectionID: connection.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(events, ","); got != "policy,prepare,payload" {
		t.Fatalf("event order = %s", got)
	}
	if result.Run.Status != domain.ConnectorSuccess {
		t.Fatalf("run status = %s", result.Run.Status)
	}
	events = nil
	if _, err := application.RunConnection(context.Background(), RunConnectionOptions{ConnectionID: connection.ID}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(events, ","); got != "prepare" {
		t.Fatalf("unchanged policy hook events = %s", got)
	}
}

func TestPreparePublishRunsWithoutPolicyStore(t *testing.T) {
	connection := domain.Connection{ID: "gh-work", Provider: "sample"}
	source := &sequenceSource{descriptor: ingestionDescriptor(false), batches: []ports.ReadBatch{{Items: []domain.Item{{Ref: domain.ItemRef{ConnectionID: connection.ID, ExternalID: "1"}, Kind: domain.ItemIssue, Title: "title"}}}}}
	events := []string{}
	application := ingestionFixture(t, connection, source, &recordingItemStore{events: &events}, &recordingCheckpointStore{}, nil)
	application.PreparePublish = func(_ context.Context, id string, policy domain.PublishPolicy) error {
		if id != connection.ID || policy != domain.PublishLocalOnly {
			t.Fatalf("PreparePublish(%q, %q)", id, policy)
		}
		events = append(events, "prepare")
		return nil
	}
	if _, err := application.RunConnection(context.Background(), RunConnectionOptions{ConnectionID: connection.ID}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(events, ","); got != "prepare,payload" {
		t.Fatalf("event order = %s", got)
	}
}

func TestIngestionCheckpointFailureKeepsDurablePayloadStatsValid(t *testing.T) {
	connection := domain.Connection{ID: "gh-work", Provider: "sample"}
	source := &sequenceSource{descriptor: ingestionDescriptor(false), batches: []ports.ReadBatch{{Items: []domain.Item{{Ref: domain.ItemRef{ConnectionID: connection.ID, ExternalID: "1"}, Kind: domain.ItemIssue, Title: "title"}}, Checkpoints: []domain.Checkpoint{{Stream: "inbox", Version: 1}}}}}
	checkpoints := &recordingCheckpointStore{putErr: errors.New("checkpoint disk full")}
	application := ingestionFixture(t, connection, source, &recordingItemStore{}, checkpoints, nil)
	result, err := application.RunConnection(context.Background(), RunConnectionOptions{ConnectionID: connection.ID})
	if err != nil || result.Run.Status != domain.ConnectorPartial {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if err := result.Run.Stats.Validate(); err != nil {
		t.Fatalf("partial run has invalid stats: %+v: %v", result.Run.Stats, err)
	}
}

func TestIngestionFailedApplyDoesNotProduceInvalidRunStats(t *testing.T) {
	connection := domain.Connection{ID: "gh-work", Provider: "sample"}
	source := &sequenceSource{descriptor: ingestionDescriptor(false), batches: []ports.ReadBatch{{Items: []domain.Item{{Ref: domain.ItemRef{ConnectionID: connection.ID, ExternalID: "1"}, Kind: domain.ItemIssue, Title: "title"}}}}}
	store := &recordingItemStore{applyErr: errors.New("disk full")}
	application := ingestionFixture(t, connection, source, store, &recordingCheckpointStore{}, nil)
	result, err := application.RunConnection(context.Background(), RunConnectionOptions{ConnectionID: connection.ID})
	if err == nil || result.Run.Status != domain.ConnectorFailed || result.Run.Stats == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if err := result.Run.Stats.Validate(); err != nil {
		t.Fatalf("failed run has invalid stats: %+v: %v", result.Run.Stats, err)
	}
}

func TestIngestionBusyLockDoesNotBuildOrReadSource(t *testing.T) {
	connection := domain.Connection{ID: "gh-work", Provider: "sample"}
	source := &sequenceSource{descriptor: ingestionDescriptor(false)}
	locker := fakeLocker{err: domain.ErrConflict}
	application := ingestionFixture(t, connection, source, &recordingItemStore{}, &recordingCheckpointStore{}, &locker)
	result, err := application.RunConnection(context.Background(), RunConnectionOptions{ConnectionID: connection.ID})
	if err != nil || !result.Busy || source.reads != 0 {
		t.Fatalf("result=%+v err=%v source reads=%d", result, err, source.reads)
	}
}

func TestIngestionReloadsConnectionUnderLock(t *testing.T) {
	connection := domain.Connection{ID: "mail-work", Provider: "sample", Publish: domain.PublishShared}
	connections := &mutableConnectionStore{connection: connection}
	locker := lockingMutation{apply: func() {
		// Simulate an update (shared -> local-only) racing the first read.
		connections.connection.Publish = domain.PublishLocalOnly
	}}
	var prepared domain.PublishPolicy
	items := &publishAwareItemStore{recordingItemStore: &recordingItemStore{}, policy: domain.PublishShared}
	application := ingestionFixture(t, connection, &sequenceSource{descriptor: ingestionDescriptor(false)}, items, &recordingCheckpointStore{}, locker)
	application.Connections = connections
	application.PreparePublish = func(_ context.Context, id string, policy domain.PublishPolicy) error {
		if id != connection.ID {
			t.Fatalf("PreparePublish id = %q", id)
		}
		prepared = policy
		return nil
	}
	result, err := application.RunConnection(context.Background(), RunConnectionOptions{ConnectionID: connection.ID})
	if err != nil {
		t.Fatal(err)
	}
	if connections.gets < 2 {
		t.Fatalf("connection read %d times, want a reload under the lock", connections.gets)
	}
	if result.Publish != domain.PublishLocalOnly || prepared != domain.PublishLocalOnly {
		t.Fatalf("stale publish policy: result=%q prepared=%q", result.Publish, prepared)
	}
}

func TestIngestionReturnedFailureIsRedacted(t *testing.T) {
	connection := domain.Connection{ID: "gh-work", Provider: "sample"}
	secret := "ghp_" + strings.Repeat("a", 36)
	source := &sequenceSource{descriptor: ingestionDescriptor(false), errAt: 0, err: errors.New("provider rejected " + secret)}
	application := ingestionFixture(t, connection, source, &recordingItemStore{}, &recordingCheckpointStore{}, nil)
	result, err := application.RunConnection(context.Background(), RunConnectionOptions{ConnectionID: connection.ID})
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(result.Run.Error, secret) {
		t.Fatalf("secret leaked in run error: run=%q err=%v", result.Run.Error, err)
	}
}

type sequenceSource struct {
	descriptor domain.SourceDescriptor
	batches    []ports.ReadBatch
	errAt      int
	err        error
	reads      int
}

func (s *sequenceSource) Descriptor() domain.SourceDescriptor { return s.descriptor }
func (s *sequenceSource) Check(context.Context) error         { return nil }
func (s *sequenceSource) Read(context.Context, ports.ReadRequest) (ports.ReadBatch, error) {
	s.reads++
	if s.err != nil && s.reads > s.errAt {
		return ports.ReadBatch{}, s.err
	}
	if s.reads > len(s.batches) {
		return ports.ReadBatch{}, nil
	}
	return s.batches[s.reads-1], nil
}

type recordingItemStore struct {
	items    []domain.Item
	events   *[]string
	applyErr error
}

type publishAwareItemStore struct {
	*recordingItemStore
	policy domain.PublishPolicy
}

func (s *publishAwareItemStore) PublishPolicy(context.Context, string) (domain.PublishPolicy, error) {
	return s.policy, nil
}
func (s *publishAwareItemStore) SetPublishPolicy(_ context.Context, _ string, policy domain.PublishPolicy) error {
	if s.events != nil {
		*s.events = append(*s.events, "policy")
	}
	s.policy = policy
	return nil
}

func (s *recordingItemStore) Upsert(context.Context, domain.Item) (ports.UpsertOutcome, error) {
	panic("unused")
}
func (s *recordingItemStore) Delete(context.Context, domain.ItemRef, time.Time) (bool, error) {
	panic("unused")
}
func (s *recordingItemStore) Get(context.Context, domain.ItemRef) (domain.Item, error) {
	panic("unused")
}
func (s *recordingItemStore) List(context.Context, string, ports.ItemListOptions) (ports.ItemPage, error) {
	panic("unused")
}
func (s *recordingItemStore) Purge(context.Context, string) error { panic("unused") }
func (s *recordingItemStore) Apply(_ context.Context, items []domain.Item) ([]ports.UpsertOutcome, error) {
	if s.applyErr != nil {
		return nil, s.applyErr
	}
	if s.events != nil {
		*s.events = append(*s.events, "payload")
	}
	s.items = append(s.items, items...)
	outcomes := make([]ports.UpsertOutcome, len(items))
	for index := range outcomes {
		outcomes[index] = ports.UpsertCreated
	}
	return outcomes, nil
}

type recordingCheckpointStore struct {
	values []domain.Checkpoint
	events *[]string
	putErr error
}

func (s *recordingCheckpointStore) Get(context.Context, string, string) (domain.Checkpoint, error) {
	panic("unused")
}
func (s *recordingCheckpointStore) List(context.Context, string) ([]domain.Checkpoint, error) {
	return append([]domain.Checkpoint(nil), s.values...), nil
}
func (s *recordingCheckpointStore) Put(_ context.Context, _ string, checkpoint domain.Checkpoint) error {
	if s.putErr != nil {
		return s.putErr
	}
	if s.events != nil {
		*s.events = append(*s.events, "checkpoint")
	}
	s.values = append(s.values, checkpoint)
	return nil
}
func (s *recordingCheckpointStore) Delete(context.Context, string, string) error { panic("unused") }

type fakeLocker struct{ err error }

func (l *fakeLocker) TryLock(context.Context, string) (func() error, error) {
	if l.err != nil {
		return nil, l.err
	}
	return func() error { return nil }, nil
}

type lockingMutation struct{ apply func() }

func (l lockingMutation) TryLock(context.Context, string) (func() error, error) {
	if l.apply != nil {
		l.apply()
	}
	return func() error { return nil }, nil
}

type mutableConnectionStore struct {
	connection domain.Connection
	gets       int
}

func (s *mutableConnectionStore) List(context.Context) ([]domain.Connection, error) {
	return []domain.Connection{s.connection}, nil
}
func (s *mutableConnectionStore) Get(_ context.Context, id string) (domain.Connection, error) {
	s.gets++
	if id != s.connection.ID {
		return domain.Connection{}, domain.ErrNotFound
	}
	return s.connection, nil
}
func (*mutableConnectionStore) Create(context.Context, domain.Connection) error { panic("unused") }
func (*mutableConnectionStore) Update(context.Context, domain.Connection) error { panic("unused") }
func (*mutableConnectionStore) Delete(context.Context, string) error            { panic("unused") }

type oneConnectionStore struct{ connection domain.Connection }

func (s oneConnectionStore) List(context.Context) ([]domain.Connection, error) {
	return []domain.Connection{s.connection}, nil
}
func (s oneConnectionStore) Get(_ context.Context, id string) (domain.Connection, error) {
	if id != s.connection.ID {
		return domain.Connection{}, domain.ErrNotFound
	}
	return s.connection, nil
}
func (oneConnectionStore) Create(context.Context, domain.Connection) error { panic("unused") }
func (oneConnectionStore) Update(context.Context, domain.Connection) error { panic("unused") }
func (oneConnectionStore) Delete(context.Context, string) error            { panic("unused") }

func ingestionDescriptor(personal bool) domain.SourceDescriptor {
	return domain.SourceDescriptor{Provider: "sample", DisplayName: "Sample", Capabilities: domain.Capabilities{AuthModes: []domain.AuthMode{domain.AuthNone}}, DefaultPublish: domain.PublishLocalOnly, Streams: []string{"inbox"}, PersonalData: personal}
}

func ingestionFixture(t *testing.T, connection domain.Connection, source ports.Source, items ports.ItemStore, checkpoints ports.CheckpointStore, locker ports.ConnectionLocker) *Ingestion {
	t.Helper()
	registry := connectorbuild.NewRegistry()
	if err := registry.Register(ingestionDescriptor(source.Descriptor().PersonalData), func(domain.Connection, connectorbuild.Deps) (ports.Source, error) { return source, nil }); err != nil {
		t.Fatal(err)
	}
	if locker == nil {
		locker = &fakeLocker{}
	}
	return &Ingestion{Connections: oneConnectionStore{connection: connection}, Registry: registry, Items: items, Checkpoints: checkpoints, Locker: locker, Now: func() time.Time { return ingestionTestTime }}
}
