//go:build linux

package netlock

import (
	"os"
	"strings"
	"testing"
)

func TestDropCapabilitiesClearsTheEffectiveSet(t *testing.T) {
	if err := DropCapabilities(); err != nil {
		t.Fatalf("DropCapabilities: %v", err)
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if eff, ok := strings.CutPrefix(line, "CapEff:"); ok {
			if got := strings.TrimSpace(eff); got != "0000000000000000" {
				t.Fatalf("CapEff after drop = %s, want zero", got)
			}
			return
		}
	}
	t.Fatal("CapEff line not found in /proc/self/status")
}
