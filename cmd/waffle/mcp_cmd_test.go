package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestLoginAgainstUnconfiguredServerFailsWithClearMessage: login against a server that
// is not configured (or not url-based) fails with a clear message instead
// of prompting.
func TestLoginAgainstUnconfiguredServerFailsWithClearMessage(t *testing.T) {
	t.Setenv("WAFFLE_HOME", t.TempDir())
	var out, errOut bytes.Buffer
	err := mcpCmd(context.Background(), []string{"login", "github"}, strings.NewReader(""), &out, &errOut)
	if err == nil {
		t.Fatal("login succeeded without a configured server")
	}
	if !strings.Contains(err.Error(), `no remote (url) MCP server named "github"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestMCPCmdRejectsMissingArgumentsAndUnknownSubcommands(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := mcpCmd(context.Background(), nil, strings.NewReader(""), &out, &errOut); err == nil {
		t.Fatal("no subcommand succeeded")
	}
	if err := mcpCmd(context.Background(), []string{"login"}, strings.NewReader(""), &out, &errOut); err == nil {
		t.Fatal("login without a server succeeded")
	}
	if err := mcpCmd(context.Background(), []string{"logout"}, strings.NewReader(""), &out, &errOut); err == nil {
		t.Fatal("logout without a server succeeded")
	}
	if err := mcpCmd(context.Background(), []string{"bogus"}, strings.NewReader(""), &out, &errOut); err == nil {
		t.Fatal("unknown subcommand succeeded")
	}
	if err := mcpCmd(context.Background(), []string{"help"}, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatalf("help failed: %v", err)
	}
}

// TestCallbackRejectsWrongStateAndSurfacesDenial: the loopback callback verifies the state
// parameter (CSRF on the redirect), surfaces server-side denials, and
// returns the code on success.
func TestCallbackRejectsWrongStateAndSurfacesDenial(t *testing.T) {
	const state = "state-abc"

	// newCallback starts a fresh loopback listener + waiter for one attempt
	// (waitForCallback owns the listener lifecycle: Shutdown on return).
	newCallback := func() (string, chan error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			code, err := waitForCallback(context.Background(), ln, state, 10*time.Second)
			if err == nil && code != "code-123" {
				err = &errString{msg: "wrong code " + code}
			}
			done <- err
		}()
		return "http://" + ln.Addr().String(), done
	}
	get := func(base, path string) int {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	// Wrong state: refused, and the waiter reports the mismatch.
	base, done := newCallback()
	if status := get(base, "/callback?state=wrong&code=x"); status != http.StatusBadRequest {
		t.Fatalf("wrong-state status = %d", status)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("wrong-state error = %v", err)
	}

	// Server-side denial surfaces the reason.
	base, done = newCallback()
	if status := get(base, "/callback?state="+state+"&error=access_denied&error_description=nope"); status != http.StatusForbidden {
		t.Fatalf("denied status = %d", status)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("denied error = %v", err)
	}

	// Correct state + code succeeds.
	base, done = newCallback()
	if status := get(base, "/callback?state="+state+"&code=code-123"); status != http.StatusOK {
		t.Fatalf("success status = %d", status)
	}
	if err := <-done; err != nil && err.Error() != "code-123" {
		t.Fatalf("success error = %v", err)
	}
}

// errString is a minimal error carrying a message (used to smuggle the
// callback result through the error channel in tests).
type errString struct{ msg string }

func (e *errString) Error() string { return e.msg }
