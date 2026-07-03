// Package secret implements waffle's secret store (docs/plan.md, "Secret
// management"). One rule throughout: raw secrets exist only here; everything
// that leaves — config, logs, model context, sandboxes — carries references
// or short-lived derivatives.
package secret

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
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
