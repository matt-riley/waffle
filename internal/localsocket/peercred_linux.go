//go:build linux

package localsocket

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// Peer is the numeric kernel identity associated with a local connection.
type Peer struct {
	PID       int32
	UID       uint32
	GID       uint32
	Available bool
}

// PeerCredentials reads SO_PEERCRED from a Linux Unix-domain connection.
func PeerCredentials(conn net.Conn) (Peer, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return Peer{}, fmt.Errorf("peer credentials require a Unix connection, got %T", conn)
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return Peer{}, fmt.Errorf("access Unix connection descriptor: %w", err)
	}
	var credentials *unix.Ucred
	var credentialErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, credentialErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return Peer{}, fmt.Errorf("inspect Unix connection descriptor: %w", err)
	}
	if credentialErr != nil {
		return Peer{}, fmt.Errorf("read Unix peer credentials: %w", credentialErr)
	}
	return Peer{PID: credentials.Pid, UID: credentials.Uid, GID: credentials.Gid, Available: true}, nil
}
