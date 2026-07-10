package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/channel"
	"github.com/matt-riley/waffle/internal/config"
)

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

type blockingAdapter struct{}

func (blockingAdapter) Name() string { return "test" }

func (blockingAdapter) Run(ctx context.Context, _ chan<- channel.Message) error {
	<-ctx.Done()
	return nil
}

func (blockingAdapter) Send(context.Context, string, string) error { return nil }
