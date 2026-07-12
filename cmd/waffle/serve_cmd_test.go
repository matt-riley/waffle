package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/channel"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/instance"
)

func TestServeStopsWhenOwnershipHeartbeatIsLost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	original := makeServeOwnerCoordinator
	makeServeOwnerCoordinator = func() (instance.Coordinator, error) {
		c := instance.Default(filepath.Join(home, "serve.lock"))
		c.HeartbeatInterval = time.Millisecond
		return c, nil
	}
	defer func() { makeServeOwnerCoordinator = original }()

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()
	configBody := "[gateway]\nstatus_listen = \"" + addr + "\"\n[provider]\napi_key = \"test-key\"\n[agent]\nsubagents = false\nlearn = false\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- serveCmdWithAdapterFactory(context.Background(), &bytes.Buffer{}, func(config.Config) ([]channel.Adapter, error) {
			return []channel.Adapter{blockingAdapter{}}, nil
		})
	}()
	lockPath := filepath.Join(home, "serve.lock")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(lockPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve owner record was not created")
		}
		time.Sleep(time.Millisecond)
	}
	writeServeOwnerRecord(t, lockPath, instance.Record{PID: 999, Owner: "stolen", Heartbeat: time.Now()})
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "serve ownership lost") {
			t.Fatalf("serve ownership loss = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not stop after ownership loss")
	}
}

func writeServeOwnerRecord(t *testing.T, path string, record instance.Record) {
	t.Helper()
	b, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestServeRefusesLiveOwnerBeforeDatabaseMutation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	lease, err := acquireServeOwner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	err = serveCmdWithAdapterFactory(context.Background(), &bytes.Buffer{}, func(config.Config) ([]channel.Adapter, error) {
		t.Fatal("adapter factory called while owner lock held")
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "serve owner lock is held") {
		t.Fatalf("second serve = %v, want held-owner refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, "waffle.db")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refused second serve mutated database: %v", statErr)
	}
}

func TestServeStartsConfiguredStatusListenerAndShutsItDown(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}

	tomlConfig := "[gateway]\nstatus_listen = \"" + addr + "\"\n\n[provider]\napi_key = \"test-key\"\n\n[agent]\nsubagents = false\nlearn = false\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("WAFFLE_HOME"), "config.toml"), []byte(tomlConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveCmdWithAdapterFactory(ctx, os.Stderr, func(config.Config) ([]channel.Adapter, error) {
			return []channel.Adapter{blockingAdapter{}}, nil
		})
	}()

	client := &http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := client.Get("http://" + addr + "/status")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status response = %s, want 200 OK", resp.Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("configured status listener did not start at %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveCmd() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveCmd did not return after context cancellation")
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("status listener still bound after serve shutdown: %v", err)
	}
	_ = listener.Close()
}

// TestAcceptanceIssue10ShutdownWaitsForInFlightCronBeforeCleanup models the
// SIGTERM boundary after the gateway has stopped accepting work. Cleanup must
// not run until the scheduler reports that cron.Stop drained its active job.
func TestAcceptanceIssue10ShutdownWaitsForInFlightCronBeforeCleanup(t *testing.T) {
	stopCalled := make(chan struct{})
	schedulerDrained := make(chan error, 1)
	intakeDrained := make(chan struct{})
	close(intakeDrained)
	returned := make(chan struct{})

	go func() {
		waitForServeWorkers(func() { close(stopCalled) }, func() {}, schedulerDrained, intakeDrained)
		close(returned)
	}()

	select {
	case <-stopCalled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not signal scheduler")
	}
	select {
	case <-returned:
		t.Fatal("shutdown returned before in-flight cron job drained; cleanup could close shared resources")
	case <-time.After(50 * time.Millisecond):
	}
	schedulerDrained <- nil
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not return after scheduler drain")
	}
}

type blockingAdapter struct{}

func (blockingAdapter) Name() string { return "test" }

func (blockingAdapter) Run(ctx context.Context, _ chan<- channel.Message) error {
	<-ctx.Done()
	return nil
}

func (blockingAdapter) Send(context.Context, string, string) error { return nil }
