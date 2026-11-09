package ingestiontest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// This file proves the suite fails broken sources: one variant per
// invariant (non-deterministic content, checkpoint before items, inferred
// deletion). Each test runs the relevant suite case directly and asserts it
// returns an error, so the failure is asserted, never left red. The check
// case must still pass for each variant, proving the failure is specific.

// nondetSource returns different content on every identical read: a hash
// (content) that is not deterministic per checkpoint.
type nondetSource struct {
	ports.Source
	n *atomic.Int64
}

func (s *nondetSource) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	batch, err := s.Source.Read(ctx, req)
	if err != nil {
		return batch, err
	}
	for i, item := range batch.Items {
		if item.Deleted {
			continue
		}
		item.Title = fmt.Sprintf("%s #%d", item.Title, s.n.Add(1))
		batch.Items[i] = item
	}
	return batch, nil
}

// earlyCheckpointSource honors Limit by truncating the items but keeps the
// full-advance checkpoint with More false: the checkpoint covers positions
// whose items are not in this batch or an earlier one.
type earlyCheckpointSource struct {
	ports.Source
}

func (s *earlyCheckpointSource) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	batch, err := s.Source.Read(ctx, req)
	if err != nil {
		return batch, err
	}
	if req.Limit > 0 && len(batch.Items) > 1 {
		batch.Items = batch.Items[:1]
		batch.More = false
	}
	return batch, nil
}

// duplicateItemSource repeats an item in every non-empty page. A host store
// keyed by ItemRef can hide this source bug, so the paging contract must check
// the raw stream before reducing it to a set.
type duplicateItemSource struct {
	ports.Source
}

func (s *duplicateItemSource) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	batch, err := s.Source.Read(ctx, req)
	if err != nil {
		return batch, err
	}
	if len(batch.Items) > 0 {
		batch.Items = append(batch.Items, batch.Items[0])
	}
	return batch, nil
}

// dropIncrementalItemsSource advances the source checkpoint but omits every
// incremental update, making the loss reproducible without changing the host.
type dropIncrementalItemsSource struct {
	ports.Source
}

func (s *dropIncrementalItemsSource) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	batch, err := s.Source.Read(ctx, req)
	if err == nil && len(req.Checkpoints) > 0 {
		batch.Items = nil
	}
	return batch, err
}

// dropTombstonesSource advances checkpoints but hides upstream deletion
// markers, leaving previously stored items live.
type dropTombstonesSource struct {
	ports.Source
}

func (s *dropTombstonesSource) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	batch, err := s.Source.Read(ctx, req)
	if err != nil {
		return batch, err
	}
	items := batch.Items[:0]
	for _, item := range batch.Items {
		if !item.Deleted {
			items = append(items, item)
		}
	}
	batch.Items = items
	return batch, nil
}

// inferDeleteSource emits tombstones for live items missing from a limited
// batch: it infers deletions from a filtered/partial read.
type inferDeleteSource struct {
	inner *refSource
}

func (s *inferDeleteSource) Descriptor() domain.SourceDescriptor { return s.inner.Descriptor() }

func (s *inferDeleteSource) Check(ctx context.Context) error { return s.inner.Check(ctx) }

func (s *inferDeleteSource) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	batch, err := s.inner.Read(ctx, req)
	if err != nil {
		return batch, err
	}
	if req.Limit <= 0 {
		return batch, nil
	}
	inBatch := make(map[string]bool, len(batch.Items))
	for _, item := range batch.Items {
		inBatch[item.Ref.ExternalID] = true
	}
	for _, id := range s.inner.upstream.liveIDs() {
		if inBatch[id] {
			continue
		}
		batch.Items = append(batch.Items, domain.Item{
			Ref:     domain.ItemRef{ConnectionID: req.Connection.ID, ExternalID: id},
			Kind:    domain.ItemDocument,
			Title:   "inferred",
			Deleted: true,
		})
	}
	return batch, nil
}

// runNamed runs one suite case by name against h.
func runNamed(t *testing.T, h Harness, name string) error {
	t.Helper()
	source := h.NewSource(h.Connection)
	if source == nil {
		t.Fatalf("harness NewSource returned nil")
	}
	desc := source.Descriptor()
	for _, c := range allCases(h, desc, h.pageSize()) {
		if c.name == name {
			return c.fn(context.Background(), h, desc, h.pageSize())
		}
	}
	t.Fatalf("unknown case %q", name)
	return nil
}

func mustPass(t *testing.T, h Harness, name string) {
	t.Helper()
	if err := runNamed(t, h, name); err != nil {
		var skip *skipError
		if errors.As(err, &skip) {
			t.Fatalf("case %q unexpectedly skipped: %v", name, err)
		}
		t.Fatalf("case %q unexpectedly failed: %v", name, err)
	}
}

func mustFail(t *testing.T, h Harness, name, want string) {
	t.Helper()
	err := runNamed(t, h, name)
	if err == nil {
		t.Fatalf("case %q passed on a broken source; want failure containing %q", name, want)
	}
	var skip *skipError
	if errors.As(err, &skip) {
		t.Fatalf("case %q skipped on a broken source; want failure containing %q", name, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("case %q failed with %q; want failure containing %q", name, err.Error(), want)
	}
}

// TestBrokenNondeterministicContent shows the suite fails a source whose
// content changes between identical reads.
func TestBrokenNondeterministicContent(t *testing.T) {
	h, _ := newRefHarness(t)
	inner := h.NewSource
	n := &atomic.Int64{}
	h.NewSource = func(c domain.Connection) ports.Source {
		return &nondetSource{Source: inner(c), n: n}
	}
	mustPass(t, h, "check")
	mustFail(t, h, "determinism", "deterministic")
	mustFail(t, h, "crash-replay", "replay diverged")
}

// TestBrokenCheckpointBeforeItems shows the suite fails a source whose
// checkpoint advances past items it did not return.
func TestBrokenCheckpointBeforeItems(t *testing.T) {
	h, _ := newRefHarness(t)
	inner := h.NewSource
	h.NewSource = func(c domain.Connection) ports.Source {
		return &earlyCheckpointSource{Source: inner(c)}
	}
	mustPass(t, h, "check")
	mustFail(t, h, "paging", "multiple batches")
}

// TestBrokenDuplicatePaging shows the suite detects repeated refs in raw
// pages, even though the item store would collapse them by key.
func TestBrokenDuplicatePaging(t *testing.T) {
	h, _ := newRefHarness(t)
	inner := h.NewSource
	h.NewSource = func(c domain.Connection) ports.Source {
		return &duplicateItemSource{Source: inner(c)}
	}
	mustPass(t, h, "check")
	mustFail(t, h, "paging", "duplicate")
}

func TestBrokenDroppedUpdate(t *testing.T) {
	h, _ := newRefHarness(t)
	inner := h.NewSource
	h.NewSource = func(c domain.Connection) ports.Source {
		return &dropIncrementalItemsSource{Source: inner(c)}
	}
	mustPass(t, h, "check")
	mustFail(t, h, "incremental", "not picked up")
}

func TestBrokenDroppedDeletion(t *testing.T) {
	h, _ := newRefHarness(t)
	inner := h.NewSource
	h.NewSource = func(c domain.Connection) ports.Source {
		return &dropTombstonesSource{Source: inner(c)}
	}
	mustPass(t, h, "check")
	mustFail(t, h, "deletes", "no tombstone")
}

// TestBrokenInferredDeletion shows the suite fails a source that tombstones
// items missing from a partial read.
func TestBrokenInferredDeletion(t *testing.T) {
	h, _ := newRefHarness(t)
	inner := h.NewSource
	h.NewSource = func(c domain.Connection) ports.Source {
		src, ok := inner(c).(*refSource)
		if !ok {
			t.Fatalf("reference harness did not return *refSource")
		}
		return &inferDeleteSource{inner: src}
	}
	mustPass(t, h, "check")
	mustFail(t, h, "incremental", "partial read")
}
