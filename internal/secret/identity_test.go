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

func TestInitIdentityPrintsOnTransientKeyringErrorWithoutStoring(t *testing.T) {
	t.Setenv(EnvIdentity, "")
	keyring.MockInitWithError(errors.New("dbus timeout"))

	// --print never writes to the keyring, so it can generate a candidate for
	// headless bootstrap even when the desktop keyring is unavailable.
	id, err := InitIdentity(true)
	if err != nil {
		t.Fatalf("InitIdentity(printOnly) = %v, want generated identity", err)
	}
	if id == nil {
		t.Fatal("InitIdentity(printOnly) returned a nil identity")
	}

	// Without --print, an unreadable keyring must still refuse because the
	// existing identity cannot be distinguished from an absent one.
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
