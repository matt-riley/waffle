//go:build linux

package netlock

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// LockdownExceptHost drops the IPv4 default route and installs a host route
// for hostName (typically "waffle-host") via the former default gateway.
// That leaves the host broker reachable while blocking general internet
// egress on the container's primary interface (#95).
//
// Requires CAP_NET_ADMIN. Safe when already locked down (no default route).
func LockdownExceptHost(hostName string) error {
	if hostName == "" {
		hostName = "waffle-host"
	}
	hostIP, err := resolveIPv4(hostName)
	if err != nil {
		return fmt.Errorf("netlock: resolve %s: %w", hostName, err)
	}
	gw, ifindex, err := defaultGateway()
	if err == nil {
		if err := deleteDefaultRoute(gw, ifindex); err != nil && !isNoRoute(err) {
			return fmt.Errorf("netlock: delete default route: %w", err)
		}
	} else if !strings.Contains(err.Error(), "no default route") {
		return fmt.Errorf("netlock: read default route: %w", err)
	}
	// IPv6 is a second egress path: a v6-enabled container keeps internet
	// access through ::/0 even with the IPv4 default route gone.
	disableIPv6AcceptRA()
	if err := deleteIPv6DefaultRoutes(); err != nil {
		return fmt.Errorf("netlock: delete ipv6 default route: %w", err)
	}
	if gw == nil {
		// No IPv4 default route existed — already isolated apart from the
		// host route, which needs the former gateway only when one existed.
		return nil
	}
	if err := ensureHostRoute(hostIP, gw, ifindex); err != nil {
		return fmt.Errorf("netlock: host route: %w", err)
	}
	return nil
}

// disableIPv6AcceptRA best-effort stops the kernel from re-installing IPv6
// default routes via router advertisements after the lockdown. Failures are
// ignored: IPv6 may be disabled entirely, and the capability drop after this
// point prevents any userspace RA daemon from restoring routes either.
func disableIPv6AcceptRA() {
	for _, path := range []string{
		"/proc/sys/net/ipv6/conf/all/accept_ra",
		"/proc/sys/net/ipv6/conf/default/accept_ra",
	} {
		_ = os.WriteFile(path, []byte("0"), 0o644)
	}
}

// deleteIPv6DefaultRoutes removes every ::/0 route from the main table.
// Absent routes are fine; anything else is a hard failure so the lockdown
// never reports success with an uncertain IPv6 posture.
func deleteIPv6DefaultRoutes() error {
	for range 16 {
		err := netlinkRoute6(unix.RTM_DELROUTE, net.IPv6zero, 0)
		if err == nil {
			continue
		}
		if isNoRoute(err) {
			return nil
		}
		return err
	}
	return errors.New("ipv6 default routes did not drain")
}

func isNoRoute(err error) bool {
	return err == syscall.ESRCH || err == unix.ESRCH || err == syscall.ENOENT || err == unix.ENOENT
}

func resolveIPv4(host string) (net.IP, error) {
	if f, err := os.Open("/etc/hosts"); err == nil {
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			ip := net.ParseIP(fields[0])
			if ip == nil || ip.To4() == nil {
				continue
			}
			for _, name := range fields[1:] {
				if name == host {
					return ip.To4(), nil
				}
			}
		}
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return v4, nil
		}
	}
	return nil, fmt.Errorf("no IPv4 for %s", host)
}

func defaultGateway() (gw net.IP, ifindex int, err error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return nil, 0, fmt.Errorf("no default route")
	}
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		dest, err1 := parseHexIP(fields[1])
		gateway, err2 := parseHexIP(fields[2])
		if err1 != nil || err2 != nil {
			continue
		}
		if !dest.Equal(net.IPv4zero) {
			continue
		}
		ifi, err := net.InterfaceByName(fields[0])
		if err != nil {
			return gateway, 0, nil
		}
		return gateway, ifi.Index, nil
	}
	return nil, 0, fmt.Errorf("no default route")
}

func parseHexIP(s string) (net.IP, error) {
	var v uint32
	if _, err := fmt.Sscanf(s, "%x", &v); err != nil {
		return nil, err
	}
	ip := make(net.IP, 4)
	binary.LittleEndian.PutUint32(ip, v)
	return ip, nil
}

func deleteDefaultRoute(gw net.IP, ifindex int) error {
	return netlinkRoute(unix.RTM_DELROUTE, net.IPv4zero, 0, gw, ifindex)
}

func ensureHostRoute(host, gw net.IP, ifindex int) error {
	if host == nil {
		return fmt.Errorf("nil host")
	}
	err := netlinkRoute(unix.RTM_NEWROUTE, host, 32, gw, ifindex)
	if err == nil || err == syscall.EEXIST || err == unix.EEXIST {
		return nil
	}
	return err
}

func netlinkRoute(msgType int, dst net.IP, prefixLen int, gw net.IP, ifindex int) error {
	dst4 := dst.To4()
	if dst4 == nil {
		return fmt.Errorf("dst not IPv4")
	}

	s, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(s) }()

	sa := &unix.SockaddrNetlink{Family: unix.AF_NETLINK}
	if err := unix.Bind(s, sa); err != nil {
		return err
	}

	const (
		nlmsgHdrLen = 16
		rtMsgLen    = 12
	)
	attrBytes := make([]byte, 0, 64)
	attrBytes = appendRtAttr(attrBytes, unix.RTA_DST, dst4)
	if gw != nil {
		if g4 := gw.To4(); g4 != nil {
			attrBytes = appendRtAttr(attrBytes, unix.RTA_GATEWAY, g4)
		}
	}
	if ifindex > 0 {
		var b [4]byte
		binary.NativeEndian.PutUint32(b[:], uint32(ifindex))
		attrBytes = appendRtAttr(attrBytes, unix.RTA_OIF, b[:])
	}

	msgLen := nlmsgHdrLen + rtMsgLen + len(attrBytes)
	buf := make([]byte, msgLen)
	binary.NativeEndian.PutUint32(buf[0:4], uint32(msgLen))
	binary.LittleEndian.PutUint16(buf[4:6], uint16(msgType))
	flags := unix.NLM_F_REQUEST | unix.NLM_F_ACK
	if msgType == unix.RTM_NEWROUTE {
		flags |= unix.NLM_F_CREATE | unix.NLM_F_REPLACE
	}
	binary.LittleEndian.PutUint16(buf[6:8], uint16(flags))
	binary.NativeEndian.PutUint32(buf[8:12], 1)
	binary.NativeEndian.PutUint32(buf[12:16], uint32(os.Getpid()))

	buf[16] = unix.AF_INET
	buf[17] = uint8(prefixLen)
	buf[18] = 0
	buf[19] = 0
	buf[20] = unix.RT_TABLE_MAIN
	buf[21] = unix.RTPROT_BOOT
	buf[22] = unix.RT_SCOPE_UNIVERSE
	buf[23] = unix.RTN_UNICAST
	binary.NativeEndian.PutUint32(buf[24:28], 0)
	copy(buf[28:], attrBytes)

	if err := unix.Sendto(s, buf, 0, sa); err != nil {
		return err
	}
	return netlinkAck(s)
}

// netlinkAck reads one netlink ACK reply and maps NLMSG_ERROR to an error.
func netlinkAck(s int) error {
	ack := make([]byte, 1024)
	n, _, err := unix.Recvfrom(s, ack, 0)
	if err != nil {
		return err
	}
	if n < 16 {
		return fmt.Errorf("short netlink ack")
	}
	if binary.LittleEndian.Uint16(ack[4:6]) == unix.NLMSG_ERROR {
		if n < 20 {
			return fmt.Errorf("short NLMSG_ERROR")
		}
		errno := int32(binary.NativeEndian.Uint32(ack[16:20]))
		if errno == 0 {
			return nil
		}
		return syscall.Errno(-errno)
	}
	return nil
}

func appendRtAttr(b []byte, typ uint16, data []byte) []byte {
	l := 4 + len(data)
	aligned := (l + 3) &^ 3
	start := len(b)
	b = append(b, make([]byte, aligned)...)
	binary.LittleEndian.PutUint16(b[start:start+2], uint16(l))
	binary.LittleEndian.PutUint16(b[start+2:start+4], typ)
	copy(b[start+4:], data)
	return b
}

// netlinkRoute6 sends an IPv6 route change. Deleting by destination prefix
// needs no gateway or interface: the kernel matches ::/0 in the main table.
func netlinkRoute6(msgType int, dst net.IP, prefixLen int) error {
	dst6 := dst.To16()
	if dst6 == nil {
		return fmt.Errorf("dst not IPv6")
	}
	s, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(s) }()
	if err := unix.Bind(s, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	const (
		nlmsgHdrLen = 16
		rtMsgLen    = 12
	)
	attrBytes := appendRtAttr(nil, unix.RTA_DST, dst6)
	msgLen := nlmsgHdrLen + rtMsgLen + len(attrBytes)
	buf := make([]byte, msgLen)
	binary.NativeEndian.PutUint32(buf[0:4], uint32(msgLen))
	binary.LittleEndian.PutUint16(buf[4:6], uint16(msgType))
	binary.LittleEndian.PutUint16(buf[6:8], uint16(unix.NLM_F_REQUEST|unix.NLM_F_ACK))
	binary.NativeEndian.PutUint32(buf[8:12], 1)
	binary.NativeEndian.PutUint32(buf[12:16], uint32(os.Getpid()))
	buf[16] = unix.AF_INET6
	buf[17] = uint8(prefixLen)
	buf[20] = unix.RT_TABLE_MAIN
	buf[21] = unix.RTPROT_BOOT
	buf[22] = unix.RT_SCOPE_UNIVERSE
	buf[23] = unix.RTN_UNICAST
	copy(buf[28:], attrBytes)
	if err := unix.Sendto(s, buf, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	return netlinkAck(s)
}
