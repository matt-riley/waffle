package chat

// StableError is a backend failure that is safe to surface to a chat client:
// a stable machine-readable Code plus a Message containing no paths, hosts,
// or tokens. chatwire substitutes it for the generic "chat command failed"
// fallback whenever the command error chain contains one, so the person who
// typed the command learns what went wrong without the wire leaking detail
// (#243). The raw detail belongs on the host: Error() includes Cause, which
// is what the host log records, and SafeMessage() is the only part that
// reaches the client.
type StableError struct {
	// Code is a stable machine-readable identifier the client can match on
	// (for example "repo_clone_failed").
	Code string
	// Message is the human-readable message shown to the client. It must
	// never contain paths, hosts, or tokens.
	Message string
	// Cause is the underlying error, kept for the host log and Unwrap. It may
	// name paths, hosts, or tokens; it never reaches the client.
	Cause error
}

func (e *StableError) Error() string {
	if e.Cause == nil {
		return e.Message
	}
	return e.Message + ": " + e.Cause.Error()
}

// Unwrap exposes Cause for errors.Is/errors.As chains.
func (e *StableError) Unwrap() error { return e.Cause }

// ErrorCode satisfies the stableBackendError contract chatwire checks for.
func (e *StableError) ErrorCode() string { return e.Code }

// SafeMessage satisfies the stableBackendError contract chatwire checks for.
func (e *StableError) SafeMessage() string { return e.Message }
