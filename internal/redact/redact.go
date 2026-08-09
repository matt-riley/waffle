// Package redact holds the shared secret-scrubbing primitives used by every
// surface that projects model output or operator-facing errors. It exists so
// CLI, Desk, and chat wire never drift into dual implementations of the same
// security boundary (#289).
package redact

import (
	"errors"
	"regexp"
	"strings"
)

// residualPatterns matches well-known secret-format substrings that must
// never leak into projections: age identities, sk-* provider keys, and
// absolute waffle data roots.
var residualPatterns = []*regexp.Regexp{
	regexp.MustCompile(`AGE-SECRET-KEY-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`/var/lib/waffle(?:/[A-Za-z0-9._/-]+)?`),
}

// Residual scrubs known secret formats (and the WAFFLE_AGE_IDENTITY env
// literal) from a string. This is the shared residual boundary behind
// chatwire.sanitizeString and the dashboard's non-chat projections.
func Residual(value string) string {
	clean := strings.ReplaceAll(value, "WAFFLE_AGE_IDENTITY", "[redacted]")
	for _, pattern := range residualPatterns {
		clean = pattern.ReplaceAllString(clean, "[redacted]")
	}
	return clean
}

// Exact replaces every occurrence of each non-empty private value with
// "[REDACTED]". It is the exact-value scrubber used for catalogue models,
// provider errors, and chat projections.
func Exact(value string, private ...string) string {
	for _, privateValue := range private {
		if privateValue != "" {
			value = strings.ReplaceAll(value, privateValue, "[REDACTED]")
		}
	}
	return value
}

// RedactError returns an error whose message is passed through scrub while
// preserving errors.Is(err, context.Canceled) and errors.Is(err,
// context.DeadlineExceeded): callers that classify cancelled provider calls
// must keep working after redaction. The raw cause is never exposed through
// Unwrap, so tree-walking redaction tests cannot observe the credential;
// matching is delegated through Is instead (#289). An unchanged message
// returns err itself.
func RedactError(err error, scrub func(string) string) error {
	if err == nil {
		return nil
	}
	message := scrub(err.Error())
	if message == err.Error() {
		return err
	}
	return &redactedError{cause: err, message: message}
}

// redactedError presents the scrubbed message while still matching the
// original error's classification through Is. It deliberately has no Unwrap:
// the raw cause text must not surface anywhere a tree walk can observe it.
type redactedError struct {
	cause   error
	message string
}

func (e *redactedError) Error() string { return e.message }

// Is matches the same targets the cause matches (context.Canceled,
// context.DeadlineExceeded, and any sentinel the caller classifies).
func (e *redactedError) Is(target error) bool { return errors.Is(e.cause, target) }

// As lets callers find wrapped interfaces (e.g. chat cleanup's
// CleanupCompleted) through redaction, which Is cannot express.
func (e *redactedError) As(target any) bool { return errors.As(e.cause, target) }

// SafeMessage returns the scrubbed message; redacted errors are safe to show
// to operators, so chat wire can surface them without re-checking the cause.
func (e *redactedError) SafeMessage() string { return e.message }
