package secret

import (
	"errors"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestInitIdentityRefusesWhenIdentityExists(t *testing.T) {
	t.Setenv(EnvIdentity, "")
	keyring.MockInit()

	id, err := InitIdentity(false)
	if err != nil {
		t.Fatalf("InitIdentity on empty keyring: %v", err)
	}

	if _, err := InitIdentity(false); err == nil ||
		!strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("second InitIdentity = %v, want refusal", err)
	}

	// The original identity must still be intact.
	stored, err := keyring.Get(keyringService, keyringUser)
	if err != nil {
		t.Fatalf("Get after refused init: %v", err)
	}
	if stored != id.String() {
		t.Fatal("stored identity was overwritten")
	}
}

func TestInitIdentityRefusesOnTransientKeyringError(t *testing.T) {
	t.Setenv(EnvIdentity, "")
	keyring.MockInitWithError(errors.New("dbus timeout"))

	// A keyring read failure means we cannot know whether an identity
	// exists, so init must refuse rather than risk overwriting one.
	// With printOnly nothing is stored, so a buggy guard would succeed.
	if _, err := InitIdentity(true); err == nil ||
		!strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("InitIdentity(printOnly) = %v, want refusal", err)
	}
	// Without printOnly a buggy guard would fall through to keyring.Set;
	// that path reports a "store identity" error, not a refusal.
	if _, err := InitIdentity(false); err == nil ||
		!strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("InitIdentity = %v, want refusal", err)
	}
}

func TestLoadIdentitySeparatesNotFoundFromUnavailable(t *testing.T) {
	t.Setenv(EnvIdentity, "")

	keyring.MockInit()
	if _, err := LoadIdentity(); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("LoadIdentity on empty keyring = %v, want ErrNoIdentity", err)
	}

	keyring.MockInitWithError(errors.New("keychain locked"))
	_, err := LoadIdentity()
	if err == nil || errors.Is(err, ErrNoIdentity) {
		t.Errorf("LoadIdentity on broken keyring = %v, want non-ErrNoIdentity error", err)
	}
}
