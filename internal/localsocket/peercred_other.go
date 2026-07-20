//go:build !linux

package localsocket

import "net"

// Peer is unavailable where Linux SO_PEERCRED is not supported.
type Peer struct {
	PID       int32
	UID       uint32
	GID       uint32
	Available bool
}

// PeerCredentials leaves filesystem authorization intact and reports that
// supplemental kernel credentials are unavailable on this platform.
func PeerCredentials(net.Conn) (Peer, error) {
	return Peer{}, nil
}
