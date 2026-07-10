package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/config"
	"github.com/matt-riley/waffle/internal/observability"
)

// statusCmd prints the local gateway's current and recently completed runs.
func statusCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) != 0 {
		statusUsage(stderr)
		return errUsage
	}

	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	return statusCmdWithClient(ctx, "http://"+cfg.Gateway.StatusListen, http.DefaultClient, stdout)
}

// statusCmdWithClient fetches and renders a status snapshot. Its endpoint and
// client are explicit so callers can use an in-process test server.
func statusCmdWithClient(ctx context.Context, baseURL string, client *http.Client, stdout io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/status", nil)
	if err != nil {
		return fmt.Errorf("create status request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return statusUnavailable(baseURL, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body
	if resp.StatusCode != http.StatusOK {
		return statusUnavailable(baseURL, fmt.Errorf("HTTP %s", resp.Status))
	}

	var snapshot observability.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snapshot); err != nil {
		return fmt.Errorf("decode status response from %s: %w", baseURL, err)
	}
	renderStatus(stdout, snapshot)
	return nil
}

func statusUnavailable(baseURL string, err error) error {
	return fmt.Errorf("status endpoint unavailable at %s: %w; start `waffle serve` or check [gateway] status_listen in config.toml", baseURL, err)
}

func renderStatus(w io.Writer, snapshot observability.Snapshot) {
	if len(snapshot.Active) == 0 {
		fmt.Fprintln(w, "Active runs: none")
	} else {
		fmt.Fprintf(w, "Active runs (%d):\n", len(snapshot.Active))
		for _, run := range snapshot.Active {
			fmt.Fprintf(w, "  %s  %s/%s  elapsed=%s  tokens=%d in / %d out\n",
				run.ID, run.Source, run.Phase, formatRunDuration(run.ElapsedMS), run.InputTokens, run.OutputTokens)
		}
	}

	if len(snapshot.Recent) == 0 {
		fmt.Fprintln(w, "Recent runs: none")
	} else {
		fmt.Fprintf(w, "Recent runs (%d):\n", len(snapshot.Recent))
		for _, run := range snapshot.Recent {
			fmt.Fprintf(w, "  %s  %s/%s  outcome=%s  runtime=%s  tokens=%d in / %d out\n",
				run.ID, run.Source, run.Phase, run.Outcome, formatRunDuration(run.RuntimeMS), run.InputTokens, run.OutputTokens)
		}
	}

	totalInput, totalOutput, totalRuntime := 0, 0, int64(0)
	for _, run := range snapshot.Active {
		totalInput += run.InputTokens
		totalOutput += run.OutputTokens
		totalRuntime += run.ElapsedMS
	}
	for _, run := range snapshot.Recent {
		totalInput += run.InputTokens
		totalOutput += run.OutputTokens
		totalRuntime += run.RuntimeMS
	}
	fmt.Fprintf(w, "Totals: %d runs (%d active), %d input tokens, %d output tokens, %s runtime\n",
		len(snapshot.Active)+len(snapshot.Recent), len(snapshot.Active), totalInput, totalOutput, formatRunDuration(totalRuntime))
}

func formatRunDuration(milliseconds int64) string {
	return (time.Duration(milliseconds) * time.Millisecond).String()
}

func statusUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: waffle status")
}
