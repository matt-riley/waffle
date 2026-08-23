package netutil

import (
	"net/netip"
	"testing"
)

// TestIsSpecialOrPrivate pins the full blocked set: stdlib classes plus the
// special-purpose ranges netip does not know about (CGNAT/Tailscale above
// all), and confirms public addresses are never blocked (#593).
func TestIsSpecialOrPrivate(t *testing.T) {
	for _, raw := range []string{
		// stdlib classes.
		"127.0.0.1", "::1", "169.254.1.1", "fe80::1",
		"10.0.0.1", "172.16.0.1", "192.168.1.1", "fc00::1", "0.0.0.0", "::",
		// RFC 6598 shared address space (CGNAT / Tailscale): 100.64.0.0/10.
		"100.64.0.1", "100.127.255.254",
		// RFC 6890 IETF protocol assignments.
		"192.0.0.1", "192.0.0.9",
		// RFC 2544 benchmarking.
		"198.18.0.1", "198.19.255.255",
		// TEST-NET (RFC 5737).
		"192.0.2.1", "198.51.100.1", "203.0.113.1",
		// RFC 6052 NAT64 well-known prefix (embedded v4 reaches through it).
		"64:ff9b::192.0.2.1", "64:ff9b::a00:1",
	} {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !IsSpecialOrPrivate(addr) {
			t.Errorf("%s was not blocked", raw)
		}
	}
	for _, raw := range []string{
		"8.8.8.8", "1.1.1.1", "93.184.216.34", "2001:4860:4860::8888", "2606:4700:4700::1111",
	} {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatal(err)
		}
		if IsSpecialOrPrivate(addr) {
			t.Errorf("public %s was blocked", raw)
		}
	}
}
