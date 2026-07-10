package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatusRendersActiveRecentAndTotals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			t.Fatalf("path = %q, want /status", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"active": [{"id":"run-active","session_id":"s1","source":"gateway","phase":"agent","elapsed_ms":1500,"input_tokens":120,"output_tokens":45}],
			"recent": [{"id":"run-recent","session_id":"s2","source":"cron","phase":"agent","outcome":"ok","runtime_ms":3000,"input_tokens":200,"output_tokens":100}],
			"retry_queue": []
		}`))
	}))
	defer server.Close()

	var stdout bytes.Buffer
	err := statusCmdWithClient(context.Background(), server.URL, server.Client(), &stdout)
	if err != nil {
		t.Fatalf("statusCmdWithClient() error = %v", err)
	}

	for _, want := range []string{
		"Active runs (1):",
		"run-active  gateway/agent  elapsed=1.5s  tokens=120 in / 45 out",
		"Recent runs (1):",
		"run-recent  cron/agent  outcome=ok  runtime=3s  tokens=200 in / 100 out",
		"Totals: 2 runs (1 active), 320 input tokens, 145 output tokens, 4.5s runtime",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("status output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestStatusReportsUnavailableEndpoint(t *testing.T) {
	var stdout bytes.Buffer
	err := statusCmdWithClient(context.Background(), "http://127.0.0.1:1", http.DefaultClient, &stdout)
	if err == nil {
		t.Fatal("statusCmdWithClient() error = nil, want unavailable endpoint error")
	}
	if !strings.Contains(err.Error(), "waffle serve") || !strings.Contains(err.Error(), "status_listen") {
		t.Errorf("error = %q, want actionable waffle serve and status_listen guidance", err)
	}
}

func TestStatusTimesOutWhenGatewayStalls(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()

	t.Setenv("WAFFLE_HOME", t.TempDir())
	config := "[gateway]\nstatus_listen = \"" + strings.TrimPrefix(server.URL, "http://") + "\"\n"
	if err := os.WriteFile(filepath.Join(os.Getenv("WAFFLE_HOME"), "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		result <- statusCmd(context.Background(), nil, &bytes.Buffer{}, &bytes.Buffer{})
	}()

	var err error
	select {
	case err = <-result:
	case <-time.After(3 * time.Second):
		close(release)
		<-result
		t.Fatal("statusCmd() did not return within 3s for a stalled gateway")
	}
	close(release)
	if err == nil {
		t.Fatal("statusCmd() error = nil, want actionable timeout error")
	}
	if !strings.Contains(err.Error(), "waffle serve") || !strings.Contains(err.Error(), "status_listen") {
		t.Errorf("error = %q, want actionable waffle serve and status_listen guidance", err)
	}
}
