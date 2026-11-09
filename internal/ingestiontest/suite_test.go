package ingestiontest

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// This file holds the fake reference source that proves the suite: a small
// scripted Source over an in-memory upstream supporting everything the
// suite exercises (backfill, incremental resumes, deletes, paging).

const (
	refProvider = "reftest"
	refStream   = "items"
)

// refChange is one upstream change. The log only grows; seq numbers are the
// checkpoint cursor.
type refChange struct {
	seq     int
	id      string
	title   string
	body    string
	deleted bool
	at      time.Time
}

// refUpstream is the in-memory upstream behind the reference source.
type refUpstream struct {
	mu      sync.Mutex
	changes []refChange
	created map[string]time.Time
	seq     int
	next    int
	clock   time.Time
}

func newRefUpstream() *refUpstream {
	return &refUpstream{
		created: make(map[string]time.Time),
		clock:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (u *refUpstream) tick() time.Time {
	u.seq++
	u.clock = u.clock.Add(time.Second)
	return u.clock
}

// add appends a live item and returns its stable ExternalID.
func (u *refUpstream) add(title, body string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.next++
	id := fmt.Sprintf("doc-%d", u.next)
	at := u.tick()
	u.changes = append(u.changes, refChange{seq: u.seq, id: id, title: title, body: body, at: at})
	u.created[id] = at
	return id
}

// addAuto appends an item with a unique generated title.
func (u *refUpstream) addAuto(prefix string) string {
	u.mu.Lock()
	n := u.next + 1
	u.mu.Unlock()
	return u.add(fmt.Sprintf("%s %d", prefix, n), fmt.Sprintf("body of %s %d", prefix, n))
}

// updateFirst changes the content of the first live item. It reports
// whether a live item existed.
func (u *refUpstream) updateFirst() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	id := u.firstLiveLocked()
	if id == "" {
		return false
	}
	at := u.tick()
	var title string
	for i := len(u.changes) - 1; i >= 0; i-- {
		if u.changes[i].id == id && !u.changes[i].deleted {
			title = u.changes[i].title
			break
		}
	}
	u.changes = append(u.changes, refChange{
		seq: u.seq, id: id, title: title + " (updated)", body: fmt.Sprintf("revised body at seq %d", u.seq), at: at,
	})
	return true
}

// deleteFirst removes the first live item. It reports whether one existed.
func (u *refUpstream) deleteFirst() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	id := u.firstLiveLocked()
	if id == "" {
		return false
	}
	at := u.tick()
	u.changes = append(u.changes, refChange{seq: u.seq, id: id, deleted: true, at: at})
	return true
}

func (u *refUpstream) firstLiveLocked() string {
	ids := u.liveIDsLocked()
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// liveIDs returns the sorted ExternalIDs whose latest change is live.
func (u *refUpstream) liveIDs() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.liveIDsLocked()
}

func (u *refUpstream) liveIDsLocked() []string {
	last := make(map[string]refChange)
	for _, change := range u.changes {
		last[change.id] = change
	}
	var ids []string
	for id, change := range last {
		if !change.deleted {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// snapshot returns a copy of the change log ordered by seq.
func (u *refUpstream) snapshot() []refChange {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]refChange, len(u.changes))
	copy(out, u.changes)
	return out
}

func (u *refUpstream) createdAt(id string) time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.created[id]
}

// refSource is the scripted reference Source. It is deterministic per
// checkpoint: the same checkpoints always yield the same batch.
type refSource struct {
	upstream   *refUpstream
	connID     string
	descriptor domain.SourceDescriptor
	failCheck  bool
}

func refDescriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider:    refProvider,
		DisplayName: "Reference source",
		Capabilities: domain.Capabilities{
			Backfill:    true,
			Incremental: true,
			Deletes:     true,
			AuthModes:   []domain.AuthMode{domain.AuthToken},
		},
		DefaultPublish: domain.PublishShared,
		Streams:        []string{refStream},
	}
}

func (s *refSource) Descriptor() domain.SourceDescriptor {
	out := s.descriptor
	out.Capabilities.AuthModes = append([]domain.AuthMode(nil), s.descriptor.Capabilities.AuthModes...)
	out.Streams = append([]string(nil), s.descriptor.Streams...)
	return out
}

func (s *refSource) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.failCheck {
		return fmt.Errorf("reftest: invalid credentials for connection %q", s.connID)
	}
	return nil
}

func (s *refSource) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, err
	}
	if req.Connection.ID != s.connID {
		return ports.ReadBatch{}, fmt.Errorf("reftest: wrong connection %q", req.Connection.ID)
	}
	after := 0
	for _, checkpoint := range req.Checkpoints {
		if checkpoint.Stream != refStream {
			continue
		}
		if checkpoint.Version != 1 {
			return ports.ReadBatch{}, fmt.Errorf("reftest: unsupported checkpoint version %d", checkpoint.Version)
		}
		seq, err := decodeSeq(checkpoint.Data)
		if err != nil {
			return ports.ReadBatch{}, err
		}
		if seq > after {
			after = seq
		}
	}
	latestByID := make(map[string]refChange)
	for _, change := range s.upstream.snapshot() {
		if change.seq <= after {
			continue
		}
		if req.Since != nil && change.at.Before(*req.Since) {
			continue
		}
		latestByID[change.id] = change
	}
	// Model item-oriented connectors: when several revisions of one item are
	// pending, emit its latest revision once rather than repeating the same
	// stable ref on successive pages.
	eligible := make([]refChange, 0, len(latestByID))
	for _, change := range latestByID {
		eligible = append(eligible, change)
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].seq < eligible[j].seq })
	emitted := eligible
	limited := false
	if req.Limit > 0 && len(emitted) > req.Limit {
		emitted = emitted[:req.Limit]
		limited = true
	}
	// Last entry wins for a repeated ref inside one batch.
	index := make(map[string]int, len(emitted))
	var deduped []refChange
	for _, change := range emitted {
		if i, ok := index[change.id]; ok {
			deduped[i] = change
			continue
		}
		index[change.id] = len(deduped)
		deduped = append(deduped, change)
	}
	var batch ports.ReadBatch
	for _, change := range deduped {
		if change.deleted {
			batch.Items = append(batch.Items, domain.Item{
				Ref:  domain.ItemRef{ConnectionID: s.connID, ExternalID: change.id},
				Kind: domain.ItemDocument, Title: "tombstone", Deleted: true,
			})
			continue
		}
		batch.Items = append(batch.Items, domain.Item{
			Ref:       domain.ItemRef{ConnectionID: s.connID, ExternalID: change.id},
			Kind:      domain.ItemDocument,
			Title:     change.title,
			Body:      change.body,
			URL:       "https://example.test/docs/" + change.id,
			Author:    "reftest",
			Labels:    []string{"seed"},
			CreatedAt: s.upstream.createdAt(change.id),
			UpdatedAt: change.at,
		})
	}
	if len(emitted) > 0 {
		batch.Checkpoints = []domain.Checkpoint{{Stream: refStream, Version: 1, Data: encodeSeq(emitted[len(emitted)-1].seq)}}
	}
	if limited {
		batch.More = true
	} else if len(emitted) > 0 {
		last := emitted[len(emitted)-1].seq
		for _, change := range s.upstream.snapshot() {
			if change.seq > last {
				batch.More = true
				break
			}
		}
	}
	return batch, nil
}

func encodeSeq(seq int) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(seq))
	return buf[:]
}

func decodeSeq(data []byte) (int, error) {
	if len(data) != 8 {
		return 0, fmt.Errorf("reftest: malformed checkpoint data (%d bytes)", len(data))
	}
	return int(binary.BigEndian.Uint64(data)), nil
}

type seedItem struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func loadSeed(t *testing.T) []seedItem {
	t.Helper()
	data, err := os.ReadFile("testdata/seed.json")
	if err != nil {
		t.Fatalf("read seed fixture: %v", err)
	}
	var seed []seedItem
	if err := json.Unmarshal(data, &seed); err != nil {
		t.Fatalf("parse seed fixture: %v", err)
	}
	if len(seed) == 0 {
		t.Fatal("seed fixture is empty")
	}
	return seed
}

// newRefHarness builds a Harness around a fresh reference upstream seeded
// from testdata/seed.json.
func newRefHarness(t *testing.T) (Harness, *refUpstream) {
	t.Helper()
	upstream := newRefUpstream()
	for _, item := range loadSeed(t) {
		upstream.add(item.Title, item.Body)
	}
	conn := domain.Connection{ID: "reftest-conn", Provider: refProvider, Account: "tester"}
	bad := conn
	bad.Account = "bad"
	h := Harness{
		Connection: conn,
		NewSource: func(c domain.Connection) ports.Source {
			if c.Account == "bad" {
				return &refSource{upstream: upstream, connID: c.ID, descriptor: refDescriptor(), failCheck: true}
			}
			return &refSource{upstream: upstream, connID: c.ID, descriptor: refDescriptor()}
		},
		AddItem: func() { upstream.addAuto("Extra") },
		UpdateItem: func() {
			if !upstream.updateFirst() {
				upstream.addAuto("Extra")
				upstream.updateFirst()
			}
		},
		DeleteItem: func() {
			if !upstream.deleteFirst() {
				upstream.addAuto("Extra")
				upstream.deleteFirst()
			}
		},
		AddMany: func(n int) {
			for range n {
				upstream.addAuto("Bulk")
			}
		},
		BadConnection: &bad,
		PageSize:      4,
	}
	return h, upstream
}

// TestReferenceSourceContract proves the suite: the reference source passes
// every case.
func TestReferenceSourceContract(t *testing.T) {
	h, _ := newRefHarness(t)
	RunSourceSuite(t, h)
}
