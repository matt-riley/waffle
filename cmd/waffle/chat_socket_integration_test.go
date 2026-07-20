//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/matt-riley/waffle/internal/secret"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

const (
	serverCredentialCanary = "SERVER-CREDENTIAL-CANARY"
	clientIdentityCanary   = "AGE-SECRET-KEY-1CLIENT-CANARY"
)

func TestChatSocketEndToEnd(t *testing.T) {
	serviceHome, err := os.MkdirTemp("/tmp", "waffle-CONFIG-CANARY-DB-CANARY-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(serviceHome) })

	var requestsMu sync.Mutex
	var requestedModels []string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("provider path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+serverCredentialCanary {
			t.Errorf("provider authorization = %q", got)
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := decodeJSONBody(r, &body); err != nil {
			t.Errorf("decode provider request: %v", err)
		}
		requestsMu.Lock()
		requestedModels = append(requestedModels, body.Model)
		requestsMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"managed socket answer\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(provider.Close)

	socketPath := filepath.Join(serviceHome, "chat.sock")
	configBody := fmt.Sprintf(`[gateway]
status_listen = "127.0.0.1:0"

[chat]
socket = %q

[providers.local]
type = "openai"
api_key = "secret://provider/local/api-key"
base_url = %q
max_tokens = 256

[models.writer]
provider = "local"
model = "writer-upstream"

[models.gpt]
provider = "local"
model = "gpt-upstream"

[agent]
default_model = "writer"
subagents = false
learn = false
`, socketPath, provider.URL+"/v1")
	if err := os.WriteFile(filepath.Join(serviceHome, "config.toml"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	serviceIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := secret.OpenFile(filepath.Join(serviceHome, "secrets.age"), serviceIdentity).Set("provider/local/api-key", serverCredentialCanary); err != nil {
		t.Fatal(err)
	}

	binary := waffleProcessBinary(t)
	serveCtx, cancelServe := context.WithCancel(context.Background())
	serve := exec.CommandContext(serveCtx, binary, "serve")
	serve.Env = cleanProcessEnv(map[string]string{
		"WAFFLE_HOME":         serviceHome,
		"WAFFLE_AGE_IDENTITY": serviceIdentity.String(),
		"OPENAI_API_KEY":      "",
	})
	var serveLog bytes.Buffer
	serve.Stdout = &serveLog
	serve.Stderr = &serveLog
	if err := serve.Start(); err != nil {
		cancelServe()
		t.Fatal(err)
	}
	serveDone := waitForProcess(serve)
	var stopServeOnce sync.Once
	stopServe := func() {
		stopServeOnce.Do(func() { stopProcess(t, serve, serveDone, cancelServe, &serveLog) })
	}
	t.Cleanup(stopServe)
	waitForUnixSocket(t, socketPath, serveDone, &serveLog)

	blockedParent := t.TempDir()
	if err := os.Chmod(blockedParent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blockedParent, 0o700) })
	clientHome := filepath.Join(blockedParent, "nonexistent-client-home")
	var stdout, stderr bytes.Buffer
	clientCtx, cancelClient := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClient()
	client := exec.CommandContext(clientCtx, binary, "chat", "--socket", socketPath, "--plain")
	client.Env = cleanProcessEnv(map[string]string{
		"WAFFLE_HOME":         clientHome,
		"WAFFLE_AGE_IDENTITY": clientIdentityCanary,
		"OPENAI_API_KEY":      "",
		"NO_COLOR":            "1",
	})
	client.Stdin = strings.NewReader("/models\n/model gpt\n/status\nhello socket\n/exit\n")
	client.Stdout = &stdout
	client.Stderr = &stderr
	if err := client.Run(); err != nil {
		t.Fatalf("managed plain client: %v\nstdout:\n%s\nstderr:\n%s\nserve:\n%s", err, stdout.String(), stderr.String(), serveLog.String())
	}
	if err := clientCtx.Err(); err != nil {
		t.Fatalf("managed plain client exceeded deadline: %v", err)
	}

	combined := stdout.String() + stderr.String()
	for _, want := range []string{
		"Configured models",
		"gpt via local (gpt-upstream)",
		"model=gpt",
		"connection=unix",
		"managed socket answer",
	} {
		if !strings.Contains(combined, want) {
			t.Errorf("client output missing %q:\n%s", want, combined)
		}
	}
	if got := strings.Count(combined, "managed socket answer"); got != 1 {
		t.Errorf("streamed answer count = %d, want 1:\n%s", got, combined)
	}
	for _, canary := range []string{serverCredentialCanary, clientIdentityCanary, serviceHome, clientHome, "CONFIG-CANARY", "DB-CANARY"} {
		if strings.Contains(combined, canary) {
			t.Errorf("client output leaked canary %q:\n%s", canary, combined)
		}
	}

	requestsMu.Lock()
	models := append([]string(nil), requestedModels...)
	requestsMu.Unlock()
	if len(models) == 0 || models[0] != "gpt-upstream" {
		t.Errorf("provider models = %v, want first request through selected gpt alias", models)
	}

	stopServe()
	st, err := store.Open(context.Background(), filepath.Join(serviceHome, "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	latest, err := session.New(st).Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if latest.ModelAlias != "gpt" {
		t.Errorf("persisted model alias = %q, want gpt", latest.ModelAlias)
	}
}

func waitForUnixSocket(t *testing.T, path string, processDone *processWait, logs fmt.Stringer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-processDone.done:
			t.Fatalf("serve exited before socket readiness: %v\n%s", processDone.err, logs.String())
		default:
		}
		conn, err := net.DialTimeout("unix", path, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket %s was not ready within five seconds\n%s", path, logs.String())
}

func stopProcess(t *testing.T, cmd *exec.Cmd, wait *processWait, cancel context.CancelFunc, logs fmt.Stringer) {
	t.Helper()
	if err := stopProcessWithin(cmd, wait, cancel, 5*time.Second); err != nil {
		t.Errorf("%v\n%s", err, logs.String())
	}
}

func stopProcessWithin(cmd *exec.Cmd, wait *processWait, cancel context.CancelFunc, timeout time.Duration) error {
	select {
	case <-wait.done:
		cancel()
		return nil
	default:
	}
	if cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
	}
	select {
	case <-wait.done:
		cancel()
		if wait.err != nil && !strings.Contains(wait.err.Error(), "signal: interrupt") {
			return fmt.Errorf("process shutdown: %w", wait.err)
		}
		return nil
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		cancel()
		select {
		case <-wait.done:
			return fmt.Errorf("process did not stop within %s; killed and reaped", timeout)
		case <-time.After(timeout):
			return fmt.Errorf("process did not stop within %s; kill issued but process was not reaped within %s", timeout, timeout)
		}
	}
}

func TestStopProcessTimeoutKillsAndReaps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() { _ = readyReader.Close() }()
	cmd := exec.CommandContext(ctx, "sh", "-c", `trap '' INT; printf x >&3; while :; do :; done`)
	cmd.ExtraFiles = []*os.File{readyWriter}
	if err := cmd.Start(); err != nil {
		_ = readyWriter.Close()
		cancel()
		t.Fatal(err)
	}
	_ = readyWriter.Close()
	wait := waitForProcess(cmd)
	ready := make(chan error, 1)
	go func() {
		var marker [1]byte
		_, readErr := io.ReadFull(readyReader, marker[:])
		ready <- readErr
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stubborn process did not install interrupt trap")
	}
	err = stopProcessWithin(cmd, wait, cancel, 25*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "killed and reaped") {
		t.Fatalf("stopProcessWithin error = %v, want killed-and-reaped timeout", err)
	}
	select {
	case <-wait.done:
	case <-time.After(5 * time.Second):
		t.Fatal("killed process was not reaped")
	}
}

type processWait struct {
	done chan struct{}
	err  error
}

func waitForProcess(cmd *exec.Cmd) *processWait {
	wait := &processWait{done: make(chan struct{})}
	go func() {
		wait.err = cmd.Wait()
		close(wait.done)
	}()
	return wait
}

func cleanProcessEnv(values map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(values))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		_, replaced := values[key]
		if !replaced && !strings.HasPrefix(key, "WAFFLE_") {
			env = append(env, entry)
		}
	}
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

func decodeJSONBody(r *http.Request, target any) error {
	defer func() { _ = r.Body.Close() }()
	return json.NewDecoder(r.Body).Decode(target)
}
