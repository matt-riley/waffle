// Package netutil holds network classification helpers shared by outbound
// guards (the fetch tool and the broker's egress dialer).
package netutil

import "net/netip"

// specialPrefixes are special-purpose ranges that netip.Addr.IsPrivate does
// not cover (it only knows RFC 1918 and IPv6 ULA) but that outbound guards
// must treat as unsafe by default. Most important is RFC 6598 shared address
// space 100.64.0.0/10 — the CGNAT range Tailscale assigns to every tailnet
// node, so on a tailnet-connected host it is the private network in
// practice. 64:ff9b::/96 is the RFC 6052 NAT64 well-known prefix: a DNS name
// can smuggle an embedded private IPv4 through it.
var specialPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),   // RFC 6598 shared address space (CGNAT / Tailscale)
	netip.MustParsePrefix("192.0.0.0/24"),    // RFC 6890 IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),   // RFC 2544 benchmarking
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1 (RFC 5737)
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2 (RFC 5737)
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3 (RFC 5737)
	netip.MustParsePrefix("64:ff9b::/96"),    // RFC 6052 NAT64 well-known prefix
}

// IsSpecialOrPrivate reports whether addr is loopback, link-local,
// unspecified, private (RFC 1918 / IPv6 ULA), or any of the special-purpose
// ranges in specialPrefixes. Outbound guards block this set by default; an
// operator can opt specific ranges back in (e.g. the fetch tool's
// allow_private) — deny-by-default stays intact.
func IsSpecialOrPrivate(addr netip.Addr) bool {
	// Normalize IPv4-mapped IPv6 addresses: ::ffff:100.64.0.1 is the same
	// address as 100.64.0.1 and must hit the same ranges (net.IP from
	// LookupIP arrives in mapped form; Prefix.Contains does not unmap).
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() || addr.IsPrivate() {
		return true
	}
	for _, p := range specialPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
