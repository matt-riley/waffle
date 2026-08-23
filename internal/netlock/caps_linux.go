//go:build linux

package netlock

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// capV3 is _LINUX_CAPABILITY_VERSION_3 (two data words, up to 64 caps).
const capV3 = 0x20080522

// DropCapabilities clears the effective, permitted, and inheritable
// capability sets of the calling thread. Dropping your own capabilities is
// always permitted, so after the network lockdown has been applied the runner
// can shed CAP_NET_ADMIN without holding CAP_SETPCAP. exec() preserves no
// capabilities for a binary without file capabilities, so the re-exec that
// runner_cmd performs yields a serving process with an empty set either way.
//
// Capability sets are per-thread: the caller must re-exec (as runner_cmd does)
// so the serving process and every tool it starts inherit an empty set.
func DropCapabilities() error {
	hdr := unix.CapUserHeader{Version: capV3}
	var data [2]unix.CapUserData
	err := capGet(&hdr, &data)
	if errors.Is(err, unix.EINVAL) {
		// The kernel rejected the version and wrote the one it supports back
		// into hdr; retry once with that.
		err = capGet(&hdr, &data)
	}
	if err != nil {
		return fmt.Errorf("netlock: capget: %w", err)
	}
	var zero [2]unix.CapUserData
	if _, _, errno := unix.Syscall(unix.SYS_CAPSET,
		uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&zero)), 0); errno != 0 {
		return fmt.Errorf("netlock: capset: %w", errno)
	}
	return nil
}

func capGet(hdr *unix.CapUserHeader, data *[2]unix.CapUserData) error {
	if _, _, errno := unix.Syscall(unix.SYS_CAPGET,
		uintptr(unsafe.Pointer(hdr)), uintptr(unsafe.Pointer(data)), 0); errno != 0 {
		return errno
	}
	return nil
}
