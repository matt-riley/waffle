package secret

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"filippo.io/age"
	"github.com/zalando/go-keyring"
)

const (
	// EnvIdentity lets headless machines (CI, containers) supply the age
	// identity without an OS keyring.
	EnvIdentity = "WAFFLE_AGE_IDENTITY"

	keyringService = "waffle"
	keyringUser    = "age-identity"
)

// ErrNoIdentity is returned when no identity is configured anywhere.
var ErrNoIdentity = errors.New(
	"no secret-store identity: run `waffle secret init`, or set " + EnvIdentity)

// LoadIdentity resolves the age identity that unlocks the secret store:
// $WAFFLE_AGE_IDENTITY first, then the OS keyring.
func LoadIdentity() (*age.X25519Identity, error) {
	if v := os.Getenv(EnvIdentity); v != "" {
		id, err := age.ParseX25519Identity(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", EnvIdentity, err)
		}
		return id, nil
	}
	v, err := keyring.Get(keyringService, keyringUser)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil, ErrNoIdentity
		}
		return nil, fmt.Errorf("keyring unavailable: %w", err)
	}
	id, err := age.ParseX25519Identity(strings.TrimSpace(v))
	if err != nil {
		return nil, fmt.Errorf("parse identity from keyring: %w", err)
	}
	return id, nil
}

// InitIdentity generates a fresh identity and, unless printOnly, stores it
// in the OS keyring. The identity string is returned either way so the CLI
// can show it once for backup; it is never written to disk by waffle.
func InitIdentity(printOnly bool) (*age.X25519Identity, error) {
	_, err := LoadIdentity()
	if err == nil {
		return nil, errors.New("an identity already exists; refusing to overwrite it")
	}
	// --print never writes to the keyring, so it must remain usable on
	// headless machines where the keyring service is unavailable. A normal
	// init still refuses unless absence was confirmed, because storing a new
	// identity over an unreadable existing one would destroy access.
	if !printOnly && !errors.Is(err, ErrNoIdentity) {
		return nil, fmt.Errorf(
			"cannot verify whether an identity already exists (%w); refusing to overwrite", err)
	}

	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("generate identity: %w", err)
	}
	if !printOnly {
		if err := keyring.Set(keyringService, keyringUser, id.String()); err != nil {
			return nil, fmt.Errorf(
				"store identity in OS keyring: %w (on headless machines use --print and set %s)",
				err, EnvIdentity)
		}
	}
	return id, nil
}

// ImportIdentity validates and stores an identity in the OS keyring. It never
// replaces an existing identity; callers must explicitly remove it first.
func ImportIdentity(value string) error {
	id, err := age.ParseX25519Identity(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("parse identity: %w", err)
	}
	if _, err := LoadIdentity(); err == nil {
		return errors.New("an identity already exists; refusing to overwrite it")
	} else if !errors.Is(err, ErrNoIdentity) {
		return fmt.Errorf("cannot verify whether an identity exists (%w); refusing to overwrite it", err)
	}
	if err := keyring.Set(keyringService, keyringUser, id.String()); err != nil {
		return fmt.Errorf("store identity in OS keyring: %w", err)
	}
	return nil
}
