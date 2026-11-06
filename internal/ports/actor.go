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
