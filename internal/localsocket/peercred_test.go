//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package localsocket

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestPeerCredentialsReportsLinuxUnixPeer(t *testing.T) {
	path := filepath.Join(unixTempDir(t), "peer.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	dialed := make(chan error, 1)
	go func() {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			defer func() { _ = conn.Close() }()
		}
		dialed <- err
	}()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := <-dialed; err != nil {
		t.Fatal(err)
	}

	peer, err := PeerCredentials(conn)
	if runtime.GOOS != "linux" {
		if err != nil || peer.Available {
			t.Fatalf("PeerCredentials = %+v, %v; want unavailable platform fallback", peer, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if !peer.Available || peer.PID != int32(os.Getpid()) || peer.UID != uint32(os.Getuid()) || peer.GID != uint32(os.Getgid()) {
		t.Fatalf("PeerCredentials = %+v; want pid=%d uid=%d gid=%d", peer, os.Getpid(), os.Getuid(), os.Getgid())
	}
}

func TestPeerCredentialsRejectsNonUnixConnectionOnLinux(t *testing.T) {
	left, right := net.Pipe()
	defer func() { _ = left.Close() }()
	defer func() { _ = right.Close() }()
	peer, err := PeerCredentials(left)
	if runtime.GOOS == "linux" {
		if err == nil || peer.Available {
			t.Fatalf("PeerCredentials = %+v, %v; want non-Unix error", peer, err)
		}
		return
	}
	if err != nil || peer.Available {
		t.Fatalf("PeerCredentials = %+v, %v; want unavailable platform fallback", peer, err)
	}
}
