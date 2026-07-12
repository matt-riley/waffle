package session

import "context"

type sessionKey struct{}

// WithSession attaches a session id so tools and spill storage can scope
// writes to the active conversation.
func WithSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionKey{}, id)
}

// IDFromContext returns the id attached by WithSession, or "".
func IDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(sessionKey{}).(string)
	return v
}
