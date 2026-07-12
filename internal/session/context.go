package session

import "context"

type sessionKey struct{}
type originKey struct{}

// Origin carries runtime provenance for writes made during a conversation.
type Origin struct {
	SessionID string
	Channel   string
	Untrusted bool
}

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

// WithOrigin attaches mutable per-run provenance. Tool results may taint the
// origin before a later model-invoked write in the same run.
func WithOrigin(ctx context.Context, sessionID, channel string) context.Context {
	ctx = WithSession(ctx, sessionID)
	return context.WithValue(ctx, originKey{}, &Origin{SessionID: sessionID, Channel: channel})
}

// OriginFromContext returns a snapshot of runtime provenance.
func OriginFromContext(ctx context.Context) Origin {
	o, _ := ctx.Value(originKey{}).(*Origin)
	if o == nil {
		return Origin{SessionID: IDFromContext(ctx)}
	}
	return *o
}

// MarkUntrusted records that subsequent writes can depend on untrusted tool output.
func MarkUntrusted(ctx context.Context) {
	if o, _ := ctx.Value(originKey{}).(*Origin); o != nil {
		o.Untrusted = true
	}
}
