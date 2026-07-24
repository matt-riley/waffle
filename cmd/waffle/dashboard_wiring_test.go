package main

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/dashboard"
	"github.com/matt-riley/waffle/internal/workspace"
)

func TestNewDashboardProcessGenerationIsOpaqueAndExact(t *testing.T) {
	source := bytes.Repeat([]byte{0x5a}, dashboardProcessGenerationBytes)
	generation, err := newDashboardProcessGeneration(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(generation, "=") {
		t.Fatalf("generation %q is padded", generation)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(generation)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, source) {
		t.Fatalf("generation decoded to %x, want %x", decoded, source)
	}
}

func TestNewDashboardProcessGenerationFailsClosedOnShortEntropy(t *testing.T) {
	_, err := newDashboardProcessGeneration(bytes.NewReader(make([]byte, dashboardProcessGenerationBytes-1)))
	if err == nil || !strings.Contains(err.Error(), "process generation") {
		t.Fatalf("error = %v, want sanitized process generation failure", err)
	}
}

func TestDashboardRestartSchedulerSelectsManagedOnlyInsideSystemd(t *testing.T) {
	t.Setenv("INVOCATION_ID", "")
	if _, ok := dashboardRestartScheduler().(dashboard.StandaloneRestartScheduler); !ok {
		t.Fatalf("standalone scheduler = %T", dashboardRestartScheduler())
	}

	t.Setenv("INVOCATION_ID", "opaque-systemd-invocation")
	if _, ok := dashboardRestartScheduler().(dashboard.ManagedRestartScheduler); !ok {
		t.Fatalf("managed scheduler = %T", dashboardRestartScheduler())
	}
}

func TestConfigureServeWorkspaceManagerUsesBrokerForRestrictedEgress(t *testing.T) {
	for _, test := range []struct {
		name      string
		egress    string
		wantProxy string
	}{
		{name: "default", wantProxy: "http://waffle-host:8421/egress"},
		{name: "allowlist", egress: "allowlist", wantProxy: "http://waffle-host:8421/egress"},
		{name: "full", egress: "full"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := &workspace.Manager{}
			configureServeWorkspaceManager(
				config.Config{Workspace: config.Workspace{Egress: test.egress}},
				manager,
				"http://waffle-host:8421",
			)
			if manager.BrokerURL != "http://waffle-host:8421" || manager.ProxyURL != test.wantProxy {
				t.Fatalf("manager broker=%q proxy=%q", manager.BrokerURL, manager.ProxyURL)
			}
		})
	}
}

func TestServeBrokerURLUsesOnlyValidatedPort(t *testing.T) {
	if got := serveBrokerURL("127.0.0.1:8421"); got != "http://waffle-host:8421" {
		t.Fatalf("serveBrokerURL = %q", got)
	}
	for _, invalid := range []string{"", "127.0.0.1", "not-an-address"} {
		if got := serveBrokerURL(invalid); got != "" {
			t.Fatalf("serveBrokerURL(%q) = %q", invalid, got)
		}
	}
}

func TestConfigureServeWorkspaceManagerIgnoresMissingDependencies(t *testing.T) {
	configureServeWorkspaceManager(config.Config{}, nil, "http://waffle-host:8421")
	manager := &workspace.Manager{}
	configureServeWorkspaceManager(config.Config{}, manager, "")
	if manager.BrokerURL != "" || manager.ProxyURL != "" {
		t.Fatalf("manager changed without broker URL: %#v", manager)
	}
}

func TestNewDashboardProcessGenerationRejectsMissingRandomSource(t *testing.T) {
	_, err := newDashboardProcessGeneration(nil)
	if err == nil {
		t.Fatalf("error = %v, want failure", err)
	}
}
