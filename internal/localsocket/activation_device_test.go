//go:build darwin || linux

package localsocket

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestConfiguredUnixRejectsExistingDeviceWhenPlatformAllowsCreation(t *testing.T) {
	parent := filepath.Join(unixTempDir(t), "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "chat.sock")
	if err := unix.Mknod(path, unix.S_IFCHR|0o600, 0); err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.ENOTSUP) {
			t.Skipf("device creation unavailable: %v", err)
		}
		t.Fatal(err)
	}
	listener, inherited, err := Listener(path)
	if err == nil || listener != nil || inherited {
		t.Fatalf("Listener = %v, %v, %v; want device refusal", listener, inherited, err)
	}
	info, statErr := os.Lstat(path)
	if statErr != nil || info.Mode()&os.ModeDevice == 0 {
		t.Fatalf("refused device changed: %v, %v", info, statErr)
	}
}
