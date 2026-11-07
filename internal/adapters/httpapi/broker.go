package httpapi

import (
	"sync"
	"time"
)

// event is one published server revision notification.
type event struct {
	Revision   int64     `json:"revision"`
	EntityKind string    `json:"entityKind,omitempty"`
	EntityID   string    `json:"entityId,omitempty"`
	Action     string    `json:"action,omitempty"`
	At         time.Time `json:"at"`
}

type broker struct {
	mu     sync.Mutex
	nextID int
	subs   map[int]chan event
	done   chan struct{}
	closed bool
}

func newBroker() *broker {
	return &broker{
		subs: make(map[int]chan event),
		done: make(chan struct{}),
	}
}

func (b *broker) subscribe() (<-chan event, func()) {
	ch := make(chan event, 64)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ch, func() {}
	}
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subs, id)
	}
}

func (b *broker) publish(ev event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// A slow SSE client loses non-critical notifications and will
			// catch up on reconnect via Last-Event-ID.
		}
	}
}

func (b *broker) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	close(b.done)
}

// subscriberCount reports the number of connected event-stream subscribers.
func (b *broker) subscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
