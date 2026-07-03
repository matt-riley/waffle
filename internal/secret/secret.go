// Package secret implements waffle's secret store (docs/plan.md, "Secret
// management"). One rule throughout: raw secrets exist only here; everything
// that leaves — config, logs, model context, sandboxes — carries references
// or short-lived derivatives.
package secret

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/matt-riley/waffle/internal/config"
)

// ErrNotFound is returned when a named secret does not exist.
var ErrNotFound = errors.New("secret not found")

// Store is the interface every backend implements. The default backend is
// the age-encrypted file (FileStore); env/1Password/Vault backends can be
// added behind the same interface.
type Store interface {
	Set(name, value string) error
	Get(name string) (string, error)
	Delete(name string) error
	List() ([]string, error)
}

// nameRE constrains secret names to lowercase path-ish identifiers, e.g.
// "anthropic/api-key" or "telegram/bot-token".
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$`)

// ValidName reports whether name is an acceptable secret name.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// refPrefix is how config files reference secrets without containing them:
//
//	api_key = "secret://anthropic/api-key"
const refPrefix = "secret://"

// IsRef reports whether s is a secret:// reference.
func IsRef(s string) bool { return strings.HasPrefix(s, refPrefix) }

// Resolve returns s itself for plain values, or the referenced secret's
// value when s is a secret:// reference. Resolution is the gateway's job;
// resolved values must never be written back to config or logs.
func Resolve(store Store, s string) (string, error) {
	if !IsRef(s) {
		return s, nil
	}
	name := strings.TrimPrefix(s, refPrefix)
	if !ValidName(name) {
		return "", fmt.Errorf("invalid secret reference %q", s)
	}
	v, err := store.Get(name)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", s, err)
	}
	return v, nil
}

// TryOpen returns a Store if an identity can be loaded from keyring or env,
// otherwise (nil, nil) so callers fall back to environment variables.
// Load failures (bad identity etc) are treated as no-store to match prior
// resolver fallback semantics. Path errors after successful load are returned.
func TryOpen() (Store, error) {
	id, err := LoadIdentity()
	if err != nil {
		return nil, nil
	}
	path, err := config.SecretsPath()
	if err != nil {
		return nil, err
	}
	return OpenFile(path, id), nil
}

// ResolveRef resolves a secret:// reference (or plain literal value).
// If s is a reference and the store cannot satisfy it (no identity, or
// ErrNotFound), it falls back to os.Getenv(envVar). When a not-found ref
// has no env fallback, returns the wrapped ErrNotFound plus hint.
func ResolveRef(s, envVar string) (string, error) {
	if !IsRef(s) {
		if s != "" {
			return s, nil
		}
		return os.Getenv(envVar), nil
	}
	// reference
	store, err := TryOpen()
	if err != nil {
		return "", err
	}
	if store == nil {
		return os.Getenv(envVar), nil
	}
	v, err := Resolve(store, s)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			if v := os.Getenv(envVar); v != "" {
				return v, nil
			}
			return "", fmt.Errorf("%w — store it with: printf '%%s' VALUE | waffle secret set %s", err, strings.TrimPrefix(s, refPrefix))
		}
		return "", err
	}
	return v, nil
}

// RedactorFor returns a redaction function (or nil) that will redact the
// given value under secretName (plus any values from store). If value is
// empty and store is nil, returns nil, nil.
func RedactorFor(store Store, secretName, value string) (func(string) string, error) {
	if value == "" && store == nil {
		return nil, nil
	}
	r, err := NewRedactorWith(store, NamedValue{Name: secretName, Value: value})
	if err != nil {
		return nil, err
	}
	return r.Redact, nil
}
