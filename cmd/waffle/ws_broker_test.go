package main

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"testing"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/store"
)

// TestStartWorkspaceBrokerFailsFastOnBusyAddress verifies the bind is
// synchronous: when the configured address is already held (the normal state
// when `waffle serve` is running), startWorkspaceBroker returns an error
// instead of proceeding with a broker URL nothing in this process serves (#48).
func TestStartWorkspaceBrokerFailsFastOnBusyAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Occupy an address to stand in for a running serve.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close() //nolint:errcheck // test teardown
	addr := held.Addr().String()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test teardown

	cfg := config.Config{}
	cfg.Broker.Listen = addr

	var stderr bytes.Buffer
	b, url, err := startWorkspaceBroker(ctx, cfg, st, &stderr)
	if err == nil {
		t.Fatal("startWorkspaceBroker succeeded on a busy address; want a bind error")
	}
	if b != nil || url != "" {
		t.Errorf("on failure want nil broker and empty url, got b=%v url=%q", b, url)
	}
}

// TestStartWorkspaceBrokerBindsFreeAddress is the happy path: a free address
// binds and the container-facing URL is returned.
func TestStartWorkspaceBrokerBindsFreeAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Find a free port, then release it for the broker to claim.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(probe.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = probe.Close()
	addr := net.JoinHostPort("127.0.0.1", port)

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test teardown

	cfg := config.Config{}
	cfg.Broker.Listen = addr

	var stderr bytes.Buffer
	b, url, err := startWorkspaceBroker(ctx, cfg, st, &stderr)
	if err != nil {
		t.Fatalf("startWorkspaceBroker on a free address: %v", err)
	}
	if b == nil {
		t.Fatal("want a broker, got nil")
	}
	if want := "http://waffle-host:" + port; url != want {
		t.Errorf("url = %q, want %q", url, want)
	}
}
