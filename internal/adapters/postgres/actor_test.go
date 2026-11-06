package postgres

import (
	"context"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// TestActorSharesPortsContextKey guards the contract that the HTTP layer
// (which sets the actor through ports.WithActor) and this adapter agree on
// one context key; before, every Postgres revision had an empty actor.
func TestActorSharesPortsContextKey(t *testing.T) {
	ctx := ports.WithActor(context.Background(), "alice")
	if got := ActorFromContext(ctx); got != "alice" {
		t.Fatalf("ActorFromContext = %q, want alice", got)
	}
	if got := ports.ActorFromContext(WithActor(context.Background(), "bob")); got != "bob" {
		t.Fatalf("ports.ActorFromContext = %q, want bob", got)
	}
}
