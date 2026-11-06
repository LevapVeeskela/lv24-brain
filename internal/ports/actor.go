package ports

import "context"

type actorContextKey struct{}

// WithActor attaches the acting principal (a login) to a context. The HTTP
// layer sets it after authentication and every store adapter reads it with
// ActorFromContext, so revision and audit rows record who made a change.
// Keeping the key here, rather than in one adapter, is what lets the HTTP
// layer and the Postgres adapter agree on it.
func WithActor(ctx context.Context, actor string) context.Context {
	if actor == "" {
		return ctx
	}
	return context.WithValue(ctx, actorContextKey{}, actor)
}

// ActorFromContext returns the actor stored by WithActor, or an empty string.
func ActorFromContext(ctx context.Context) string {
	actor, _ := ctx.Value(actorContextKey{}).(string)
	return actor
}

type actorIDContextKey struct{}

// WithActorID attaches the acting user's stable ID. Stores record it as the
// owner (created_by) of new knowledge records; unlike the login it survives
// a rename.
func WithActorID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, actorIDContextKey{}, id)
}

// ActorIDFromContext returns the ID stored by WithActorID, or "".
func ActorIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(actorIDContextKey{}).(string)
	return id
}
