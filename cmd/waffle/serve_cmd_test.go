package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matt-riley/waffle/internal/channel"
	chatpkg "github.com/matt-riley/waffle/internal/chat"
	"github.com/matt-riley/waffle/internal/chatwire"
	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/instance"
	"github.com/matt-riley/waffle/internal/localsocket"
)

// TestServeHelpPrintsUsageWithoutStartingDaemon verifies -h/--help return
// usage immediately without config, ownership, sockets, or background work (#127).
func TestServeHelpPrintsUsageWithoutStartingDaemon(t *testing.T) {
	// Point WAFFLE_HOME at a path that must not be opened for help.
	t.Setenv("WAFFLE_HOME", filepath.Join(t.TempDir(), "must-not-be-opened"))

	want := "Usage: waffle serve\n\n" +
		"Start the Waffle gateway daemon (Telegram, chat socket, cron, lifecycle).\n" +
		"Configuration is read from $WAFFLE_HOME/config.toml (default ~/.waffle).\n\n" +
		"Options:\n" +
		"  -h, --help    show this help\n"

	for _, args := range [][]string{{"--help"}, {"-h"}, {"--help", "ignored"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var buf bytes.Buffer
			// No config, no home setup: must return quickly without binding ports.
			if err := serveCmd(context.Background(), args, &buf); err != nil {
				t.Fatalf("serveCmd(%v): %v", args, err)
			}
			if buf.String() != want {
				t.Fatalf("serve help = %q, want %q", buf.String(), want)
			}
		})
	}
}

func TestServeCmdWithAdapterFactoryHelpSkipsFactory(t *testing.T) {
	t.Setenv("WAFFLE_HOME", filepath.Join(t.TempDir(), "must-not-be-opened"))
	var buf bytes.Buffer
	err := serveCmdWithAdapterFactory(context.Background(), []string{"--help"}, &buf, func(config.Config) ([]channel.Adapter, error) {
		t.Fatal("adapter factory must not run for --help")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("serveCmdWithAdapterFactory --help: %v", err)
	}
	if !strings.Contains(buf.String(), "Usage: waffle serve") {
		t.Fatalf("help output missing usage: %q", buf.String())
	}
}

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
		done <- serveCmdWithAdapterFactory(context.Background(), nil, &bytes.Buffer{}, func(config.Config) ([]channel.Adapter, error) {
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
	err = serveCmdWithAdapterFactory(context.Background(), nil, &bytes.Buffer{}, func(config.Config) ([]channel.Adapter, error) {
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
		done <- serveCmdWithAdapterFactory(ctx, nil, os.Stderr, func(config.Config) ([]channel.Adapter, error) {
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
	resp, err := client.Get("http://" + addr + "/desk/")
	if err != nil {
		cancel()
		t.Fatalf("GET /desk/: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		cancel()
		t.Fatalf("disabled dashboard GET /desk/ status = %d, want 404", resp.StatusCode)
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

func TestServeDashboardEnabledWrapsSharedListenerWithoutClaimingDeskRoute(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	configBody := "[gateway]\nstatus_listen = \"" + addr + "\"\n[dashboard]\nenabled = true\n[provider]\napi_key = \"test-key\"\n[agent]\nsubagents = false\nlearn = false\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("WAFFLE_HOME"), "config.toml"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveCmdWithAdapterFactory(ctx, nil, &bytes.Buffer{}, func(config.Config) ([]channel.Adapter, error) {
			return []channel.Adapter{blockingAdapter{}}, nil
		})
	}()
	client := &http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := client.Get("http://" + addr + "/status")
		if err == nil {
			if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
				_ = resp.Body.Close()
				t.Fatalf("X-Frame-Options = %q, want DENY", got)
			}
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("dashboard listener did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := client.Get("http://" + addr + "/desk/")
	if err != nil {
		cancel()
		t.Fatalf("GET /desk/: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		cancel()
		t.Fatalf("GET /desk/ status = %d, want 404", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveCmd() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveCmd did not return after cancellation")
	}
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
		done <- serveCmdWithAdapterFactory(ctx, nil, &logs, func(config.Config) ([]channel.Adapter, error) {
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

	err := serveCmdWithAdapterFactory(context.Background(), nil, &bytes.Buffer{}, func(config.Config) ([]channel.Adapter, error) {
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

	err := serveCmdWithAdapterFactory(context.Background(), nil, &bytes.Buffer{}, func(config.Config) ([]channel.Adapter, error) {
		return []channel.Adapter{blockingAdapter{}}, nil
	})
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "chat server") {
		t.Fatalf("serve chatwire failure = %v, want wrapped %v", err, want)
	}
}

func TestServeChatListenerCloseErrorFailsOtherwiseSuccessfulShutdown(t *testing.T) {
	home := unixServeTempDir(t)
	t.Setenv("WAFFLE_HOME", home)
	clearServeActivationEnvironment(t)
	writeServeTestConfig(t, home, unusedTCPAddress(t), filepath.Join(home, "chat.sock"))

	want := errors.New("forced listener close failure")
	listener := newCloseErrorListener(want)
	original := openChatListener
	openChatListener = func(string) (net.Listener, bool, error) { return listener, false, nil }
	defer func() { openChatListener = original }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveCmdWithAdapterFactory(ctx, nil, &bytes.Buffer{}, func(config.Config) ([]channel.Adapter, error) {
			return []channel.Adapter{blockingAdapter{}}, nil
		})
	}()
	select {
	case <-listener.acceptStarted:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("chat server did not start accepting")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, want) || !strings.Contains(err.Error(), "chat listener cleanup") {
			t.Fatalf("serve listener cleanup = %v, want wrapped %v", err, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return after listener close failure")
	}
}

func TestServeChatListenerCloseErrorDoesNotMaskChatServerError(t *testing.T) {
	home := unixServeTempDir(t)
	t.Setenv("WAFFLE_HOME", home)
	clearServeActivationEnvironment(t)
	writeServeTestConfig(t, home, unusedTCPAddress(t), filepath.Join(home, "chat.sock"))

	closeWant := errors.New("forced listener close failure")
	listener := newCloseErrorListener(closeWant)
	originalListener := openChatListener
	openChatListener = func(string) (net.Listener, bool, error) { return listener, false, nil }
	defer func() { openChatListener = originalListener }()
	serveWant := errors.New("forced chat server failure")
	originalServe := serveChat
	serveChat = func(_ context.Context, listener net.Listener, _ chatwire.Factory, _ chatwire.AuditFunc) error {
		_ = listener.Close()
		return serveWant
	}
	defer func() { serveChat = originalServe }()

	err := serveCmdWithAdapterFactory(context.Background(), nil, &bytes.Buffer{}, func(config.Config) ([]channel.Adapter, error) {
		return []channel.Adapter{blockingAdapter{}}, nil
	})
	if !errors.Is(err, serveWant) {
		t.Fatalf("serve error = %v, want chat server error %v", err, serveWant)
	}
	if errors.Is(err, closeWant) {
		t.Fatalf("listener cleanup error masked/joined earlier serve error: %v", err)
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
		_ = waitForServeWorkers(func() { close(stopCalled) }, func() {}, schedulerDrained, intakeDrained, chatDrained, nil)
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
		returned <- waitForServeWorkers(func() {}, func() {}, schedulerDrained, intakeDrained, chatDrained, nil)
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

// TestServeStopsWaitsForBrokerBeforeReturn ensures shutdown joins the broker
// ServeListener goroutine so a restart can rebind the same address (#109).
func TestServeStopsWaitsForBrokerBeforeReturn(t *testing.T) {
	schedulerDrained := make(chan error, 1)
	schedulerDrained <- nil
	intakeDrained := make(chan struct{})
	close(intakeDrained)
	chatDrained := make(chan error, 1)
	chatDrained <- nil
	brokerDrained := make(chan struct{})
	returned := make(chan struct{})

	go func() {
		_ = waitForServeWorkers(func() {}, func() {}, schedulerDrained, intakeDrained, chatDrained, brokerDrained)
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("shutdown returned before broker drained")
	case <-time.After(50 * time.Millisecond):
	}
	close(brokerDrained)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not return after broker drained")
	}
}

// TestServeCredentialBrokerBindFailureIsNotSwallowed occupies the broker
// listen address and asserts serve fails startup without logging success (#99).
func TestServeCredentialBrokerBindFailureIsNotSwallowed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	brokerAddr := held.Addr().String()
	statusAddr := unusedTCPAddress(t)

	configBody := fmt.Sprintf(
		"[gateway]\nstatus_listen = %q\n\n[broker]\nlisten = %q\n\n[provider]\napi_key = \"test-key\"\n\n[agent]\nsubagents = false\nlearn = false\n",
		statusAddr, brokerAddr,
	)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	err = serveCmdWithAdapterFactory(context.Background(), nil, &logs, func(config.Config) ([]channel.Adapter, error) {
		t.Fatal("adapter factory must not run after broker bind failure")
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "credential broker cannot bind") {
		t.Fatalf("serve broker bind = %v, want cannot bind error\nlogs:\n%s", err, logs.String())
	}
	if strings.Contains(logs.String(), "credential broker up") {
		t.Fatalf("logged broker success before bind failure:\n%s", logs.String())
	}
}

// TestServeCredentialBrokerPortReleasedOnShutdown starts serve with a broker,
// cancels it, and asserts the listen port is free — then rebinds in a loop so
// a fast restart cannot hit "address already in use" (#109).
func TestServeCredentialBrokerPortReleasedOnShutdown(t *testing.T) {
	brokerAddr := unusedTCPAddress(t)

	for i := 0; i < 8; i++ {
		home := t.TempDir()
		t.Setenv("WAFFLE_HOME", home)
		statusAddr := unusedTCPAddress(t)
		configBody := fmt.Sprintf(
			"[gateway]\nstatus_listen = %q\n\n[broker]\nlisten = %q\n\n[provider]\napi_key = \"test-key\"\n\n[agent]\nsubagents = false\nlearn = false\n",
			statusAddr, brokerAddr,
		)
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(configBody), 0o600); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- serveCmdWithAdapterFactory(ctx, nil, io.Discard, func(config.Config) ([]channel.Adapter, error) {
				return []channel.Adapter{blockingAdapter{}}, nil
			})
		}()

		deadline := time.Now().Add(3 * time.Second)
		for {
			conn, dialErr := net.DialTimeout("tcp", brokerAddr, 50*time.Millisecond)
			if dialErr == nil {
				_ = conn.Close()
				break
			}
			select {
			case err := <-done:
				cancel()
				t.Fatalf("serve exited before broker up (iteration %d): %v", i, err)
			default:
			}
			if time.Now().After(deadline) {
				cancel()
				t.Fatalf("broker did not start (iteration %d): last dial error: %v", i, dialErr)
			}
			time.Sleep(10 * time.Millisecond)
		}

		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("serve shutdown (iteration %d): %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("serve did not return after cancel (iteration %d)", i)
		}

		ln, err := net.Listen("tcp", brokerAddr)
		if err != nil {
			t.Fatalf("broker port still bound after serve return (iteration %d): %v", i, err)
		}
		_ = ln.Close()
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

type closeErrorListener struct {
	closeErr      error
	acceptStarted chan struct{}
	closed        chan struct{}
	acceptOnce    sync.Once
	closeOnce     sync.Once
}

func newCloseErrorListener(closeErr error) *closeErrorListener {
	return &closeErrorListener{closeErr: closeErr, acceptStarted: make(chan struct{}), closed: make(chan struct{})}
}

func (l *closeErrorListener) Accept() (net.Conn, error) {
	l.acceptOnce.Do(func() { close(l.acceptStarted) })
	<-l.closed
	return nil, net.ErrClosed
}

func (l *closeErrorListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return l.closeErr
}

func (l *closeErrorListener) Addr() net.Addr { return testListenerAddr("local") }

type testListenerAddr string

func (a testListenerAddr) Network() string { return "unix" }
func (a testListenerAddr) String() string  { return string(a) }

func (blockingAdapter) Name() string { return "test" }

func (blockingAdapter) Run(ctx context.Context, _ chan<- channel.Message) error {
	<-ctx.Done()
	return nil
}

func (blockingAdapter) Send(context.Context, string, string) error { return nil }
