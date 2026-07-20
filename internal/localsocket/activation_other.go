//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package localsocket

import (
	"errors"
	"net"
	"os"
)

// Listener has no systemd or Unix-socket implementation on this platform.
func Listener(configuredPath string) (net.Listener, bool, error) {
	for _, name := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		_ = os.Unsetenv(name)
	}
	if configuredPath == "" {
		return nil, false, nil
	}
	return nil, false, errors.New("configured Unix chat sockets are unsupported on this platform")
}
