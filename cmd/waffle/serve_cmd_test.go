package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/channel"
	chatpkg "github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/chatwire"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/instance"
	"github.com/matt-riley/waffle/internal/localsocket"
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
	defer func() { _ = lease.Release() }()
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

func TestServeChatStartsConfiguredSocketAcceptsHandshakeAndRemovesOnShutdown(t *testing.T) {
	home := unixServeTempDir(t)
	t.Setenv("WAFFLE_HOME", home)
	clearServeActivationEnvironment(t)
	socketPath := filepath.Join(home, "chat.sock")
	statusAddr := unusedTCPAddress(t)
	writeServeTestConfig(t, home, statusAddr, socketPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var logs bytes.Buffer
	go func() {
		done <- serveCmdWithAdapterFactory(ctx, &logs, func(config.Config) ([]channel.Adapter, error) {
			return []channel.Adapter{blockingAdapter{}}, nil
		})
	}()

	client := dialChatUntilReady(t, socketPath)
	openCtx, openCancel := context.WithTimeout(context.Background(), 2*time.Second)
	state, err := client.Open(openCtx, chatpkg.OpenOptions{})
	openCancel()
	if err != nil {
		cancel()
		t.Fatalf("chat handshake: %v\nlogs:\n%s", err, logs.String())
	}
	if state.SessionID == "" {
		cancel()
		t.Fatalf("chat handshake returned no session: %+v", state)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve shutdown: %v\nlogs:\n%s", err, logs.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not stop after chat context cancellation")
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configured chat socket remained after shutdown: %v", err)
	}
}

func TestServeChatListenerErrorFailsStartup(t *testing.T) {
	home := unixServeTempDir(t)
	t.Setenv("WAFFLE_HOME", home)
	clearServeActivationEnvironment(t)
	socketPath := filepath.Join(home, "chat.sock")
	if err := os.WriteFile(socketPath, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeServeTestConfig(t, home, unusedTCPAddress(t), socketPath)

	err := serveCmdWithAdapterFactory(context.Background(), &bytes.Buffer{}, func(config.Config) ([]channel.Adapter, error) {
		return []channel.Adapter{blockingAdapter{}}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "chat listener") || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("serve listener error = %v", err)
	}
	contents, readErr := os.ReadFile(socketPath)
	if readErr != nil || string(contents) != "preserve" {
		t.Fatalf("listener failure changed configured path: %q, %v", contents, readErr)
	}
}

func TestServeChatwireFailureFailsServe(t *testing.T) {
	home := unixServeTempDir(t)
	t.Setenv("WAFFLE_HOME", home)
	clearServeActivationEnvironment(t)
	writeServeTestConfig(t, home, unusedTCPAddress(t), filepath.Join(home, "chat.sock"))

	want := errors.New("forced chat server failure")
	original := serveChat
	serveChat = func(context.Context, net.Listener, chatwire.Factory, chatwire.AuditFunc) error {
		return want
	}
	defer func() { serveChat = original }()

	err := serveCmdWithAdapterFactory(context.Background(), &bytes.Buffer{}, func(config.Config) ([]channel.Adapter, error) {
		return []channel.Adapter{blockingAdapter{}}, nil
	})
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "chat server") {
		t.Fatalf("serve chatwire failure = %v, want wrapped %v", err, want)
	}
}

func TestServeChatAuditCredentialFailureDoesNotRejectOrLeakDetails(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	canary := "/private/config.toml AGE-SECRET-KEY-1AUDIT"
	audit := newChatAudit(log, func(net.Conn) (localsocket.Peer, error) {
		return localsocket.Peer{}, fmt.Errorf("lookup failed near %s", canary)
	})
	left, right := net.Pipe()
	defer func() { _ = left.Close() }()
	defer func() { _ = right.Close() }()
	audit(context.Background(), left, "connected")

	got := logs.String()
	if !strings.Contains(got, "connected") || !strings.Contains(got, "unavailable") {
		t.Fatalf("audit log = %q, want lifecycle and unavailable marker", got)
	}
	if strings.Contains(got, canary) || strings.Contains(got, "config.toml") || strings.Contains(got, "AGE-SECRET") {
		t.Fatalf("audit leaked credential lookup details: %q", got)
	}
}

func TestServeChatAuditLogsOnlyLifecycleAndNumericPeer(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	audit := newChatAudit(log, func(net.Conn) (localsocket.Peer, error) {
		return localsocket.Peer{PID: 123, UID: 456, GID: 789, Available: true}, nil
	})
	left, right := net.Pipe()
	defer func() { _ = left.Close() }()
	defer func() { _ = right.Close() }()
	audit(context.Background(), left, "disconnected")

	got := logs.String()
	for _, want := range []string{"disconnected", "pid=123", "uid=456", "gid=789"} {
		if !strings.Contains(got, want) {
			t.Fatalf("audit log = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "pipe") || strings.Contains(got, "addr") || strings.Contains(got, "path") {
		t.Fatalf("audit logged connection details outside numeric identity: %q", got)
	}
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

	chatDrained := make(chan error, 1)
	chatDrained <- nil
	go func() {
		_ = waitForServeWorkers(func() { close(stopCalled) }, func() {}, schedulerDrained, intakeDrained, chatDrained)
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

func TestServeStopsWaitsForChatServerBeforeSharedCleanup(t *testing.T) {
	schedulerDrained := make(chan error, 1)
	schedulerDrained <- nil
	intakeDrained := make(chan struct{})
	close(intakeDrained)
	chatDrained := make(chan error, 1)
	returned := make(chan error, 1)

	go func() {
		returned <- waitForServeWorkers(func() {}, func() {}, schedulerDrained, intakeDrained, chatDrained)
	}()
	select {
	case err := <-returned:
		t.Fatalf("shutdown returned before chat server drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	want := errors.New("chat accept failed")
	chatDrained <- want
	select {
	case err := <-returned:
		if !errors.Is(err, want) {
			t.Fatalf("chat shutdown error = %v, want %v", err, want)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not return after chat server drained")
	}
}

func dialChatUntilReady(t *testing.T, path string) *chatwire.Client {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		client, err := chatwire.Dial(ctx, path)
		cancel()
		if err == nil {
			return client
		}
		if time.Now().After(deadline) {
			t.Fatalf("chat socket did not start at %s: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func unusedTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func writeServeTestConfig(t *testing.T, home, statusAddr, socketPath string) {
	t.Helper()
	body := fmt.Sprintf("[gateway]\nstatus_listen = %q\n\n[chat]\nsocket = %q\n\n[provider]\nname = \"openai\"\nmodel = \"test-model\"\napi_key = \"test-key\"\n\n[agent]\nsubagents = false\nlearn = false\n", statusAddr, socketPath)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func unixServeTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "waffle-serve-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func clearServeActivationEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		value, present := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(name, value)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
}

type blockingAdapter struct{}

func (blockingAdapter) Name() string { return "test" }

func (blockingAdapter) Run(ctx context.Context, _ chan<- channel.Message) error {
	<-ctx.Done()
	return nil
}

func (blockingAdapter) Send(context.Context, string, string) error { return nil }
