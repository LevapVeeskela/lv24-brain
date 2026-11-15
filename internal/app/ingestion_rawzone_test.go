package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// TestIngestionPublishPolicyTable: an explicit connection setting always wins;
// a raw zone that git already tracks is grandfathered as shared without
// touching the marker or the index; a new zone is local-only; and a server host
// (no machine-local zone) keeps the descriptor sensitivity policy.
func TestIngestionPublishPolicyTable(t *testing.T) {
	descriptor := publishTableDescriptor()
	cases := []struct {
		name        string
		setting     domain.PublishPolicy
		initial     domain.PublishPolicy
		tracked     *bool // nil: no RawZoneTracked hook (local mode)
		trackErr    error
		server      bool
		wantPolicy  domain.PublishPolicy
		wantNotice  bool
		wantPrepare []domain.PublishPolicy
		wantSet     []domain.PublishPolicy
	}{
		{
			name: "explicit shared", setting: domain.PublishShared, initial: domain.PublishLocalOnly,
			tracked: boolPtr(false), wantPolicy: domain.PublishShared,
			wantPrepare: []domain.PublishPolicy{domain.PublishShared},
			wantSet:     []domain.PublishPolicy{domain.PublishShared},
		},
		{
			name: "explicit local-only", setting: domain.PublishLocalOnly, initial: domain.PublishShared,
			tracked: boolPtr(true), wantPolicy: domain.PublishLocalOnly,
			wantPrepare: []domain.PublishPolicy{domain.PublishLocalOnly},
			wantSet:     []domain.PublishPolicy{domain.PublishLocalOnly},
		},
		{
			name: "unset, nothing tracked", initial: domain.PublishShared,
			tracked: boolPtr(false), wantPolicy: domain.PublishLocalOnly,
			wantPrepare: []domain.PublishPolicy{domain.PublishLocalOnly},
			wantSet:     []domain.PublishPolicy{domain.PublishLocalOnly},
		},
		{
			name: "unset, tracked", initial: domain.PublishShared,
			tracked: boolPtr(true), wantPolicy: domain.PublishShared, wantNotice: true,
		},
		{
			name: "unset, index unreadable", initial: domain.PublishShared,
			tracked: boolPtr(false), trackErr: errors.New("index read failed"),
			wantPolicy: domain.PublishShared, wantNotice: true,
		},
		{
			name: "unset, no git hook (local mode)", initial: domain.PublishShared,
			wantPolicy:  domain.PublishLocalOnly,
			wantPrepare: []domain.PublishPolicy{domain.PublishLocalOnly},
			wantSet:     []domain.PublishPolicy{domain.PublishLocalOnly},
		},
		{
			name: "server host keeps the descriptor policy", initial: domain.PublishLocalOnly, server: true,
			wantPolicy:  domain.PublishShared,
			wantPrepare: []domain.PublishPolicy{domain.PublishShared},
			wantSet:     []domain.PublishPolicy{domain.PublishShared},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			connection := domain.Connection{ID: "gh-work", Provider: "sample", Publish: tc.setting}
			items := &policyRecordingStore{publishAwareItemStore: &publishAwareItemStore{
				recordingItemStore: &recordingItemStore{},
				policy:             tc.initial,
			}}
			application := newPublishPolicyFixture(t, descriptor, connection, items, tc.tracked, tc.trackErr)
			application.DisallowLocalOnlyItems = tc.server
			var prepared []domain.PublishPolicy
			application.PreparePublish = func(_ context.Context, id string, policy domain.PublishPolicy) error {
				if id != connection.ID {
					t.Fatalf("PreparePublish id = %q", id)
				}
				prepared = append(prepared, policy)
				return nil
			}

			result, err := application.RunConnection(context.Background(), RunConnectionOptions{ConnectionID: connection.ID})
			if err != nil {
				t.Fatal(err)
			}
			if result.Publish != tc.wantPolicy {
				t.Fatalf("publish = %q, want %q", result.Publish, tc.wantPolicy)
			}
			if (result.PublishNotice != "") != tc.wantNotice {
				t.Fatalf("notice = %q, want notice=%t", result.PublishNotice, tc.wantNotice)
			}
			if !samePolicies(prepared, tc.wantPrepare) {
				t.Fatalf("prepare policies = %v, want %v", prepared, tc.wantPrepare)
			}
			if !samePolicies(items.set, tc.wantSet) {
				t.Fatalf("set policies = %v, want %v", items.set, tc.wantSet)
			}
			if len(tc.wantSet) > 0 && items.policy != tc.wantSet[len(tc.wantSet)-1] {
				t.Fatalf("store policy = %q, want %q", items.policy, tc.wantSet[len(tc.wantSet)-1])
			}
		})
	}
}

// TestGrandfatheredRawZoneNeverTouchesTheMarker: a tracked zone without an
// explicit policy keeps the marker it had, even when the stored policy is
// "local-only" (an older engine left no marker). No git hook may run.
func TestGrandfatheredRawZoneNeverTouchesTheMarker(t *testing.T) {
	descriptor := publishTableDescriptor()
	connection := domain.Connection{ID: "gh-work", Provider: "sample"}
	items := &policyRecordingStore{publishAwareItemStore: &publishAwareItemStore{
		recordingItemStore: &recordingItemStore{},
		policy:             domain.PublishLocalOnly,
	}}
	application := newPublishPolicyFixture(t, descriptor, connection, items, boolPtr(true), nil)
	application.PreparePublish = func(context.Context, string, domain.PublishPolicy) error {
		t.Fatal("PreparePublish ran for a grandfathered connection")
		return nil
	}
	result, err := application.RunConnection(context.Background(), RunConnectionOptions{ConnectionID: connection.ID})
	if err != nil {
		t.Fatal(err)
	}
	if result.Publish != domain.PublishShared || result.PublishChanged || result.PublishNotice == "" {
		t.Fatalf("result = %+v", result)
	}
	if len(items.set) != 0 || items.policy != domain.PublishLocalOnly {
		t.Fatalf("grandfathered zone was rewritten: set=%v policy=%q", items.set, items.policy)
	}
}

type policyRecordingStore struct {
	*publishAwareItemStore
	set []domain.PublishPolicy
}

func (s *policyRecordingStore) SetPublishPolicy(ctx context.Context, id string, policy domain.PublishPolicy) error {
	s.set = append(s.set, policy)
	return s.publishAwareItemStore.SetPublishPolicy(ctx, id, policy)
}

func publishTableDescriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider: "sample", DisplayName: "Sample",
		Capabilities:   domain.Capabilities{AuthModes: []domain.AuthMode{domain.AuthNone}},
		DefaultPublish: domain.PublishShared,
		Streams:        []string{"inbox"},
	}
}

// newPublishPolicyFixture builds the host with the optional git hooks a case
// needs: RawZoneTracked when tracked is non-nil.
func newPublishPolicyFixture(t *testing.T, descriptor domain.SourceDescriptor, connection domain.Connection, items ports.ItemStore, tracked *bool, trackErr error) *Ingestion {
	t.Helper()
	registry := connectorbuild.NewRegistry()
	source := &sequenceSource{descriptor: descriptor, batches: []ports.ReadBatch{{Items: []domain.Item{{
		Ref:   domain.ItemRef{ExternalID: "1"},
		Kind:  domain.ItemIssue,
		Title: "title",
	}}}}}
	if err := registry.Register(descriptor, func(domain.Connection, connectorbuild.Deps) (ports.Source, error) {
		return source, nil
	}); err != nil {
		t.Fatal(err)
	}
	host := &Ingestion{
		Connections: oneConnectionStore{connection: connection},
		Registry:    registry,
		Items:       items, Checkpoints: &recordingCheckpointStore{}, Locker: &fakeLocker{},
		Now: func() time.Time { return ingestionTestTime },
	}
	if tracked != nil {
		host.RawZoneTracked = func(context.Context, string) (bool, error) {
			return *tracked, trackErr
		}
	}
	return host
}

func boolPtr(value bool) *bool { return &value }

func samePolicies(got, want []domain.PublishPolicy) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
