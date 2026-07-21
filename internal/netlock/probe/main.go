// Command probe is a linux-only helper used by Docker-gated tests to drive
// the shipped netlock.LockdownExceptHost path (no shell reimplementation).
//
//	GOOS=linux go build -o probe ./internal/netlock/probe
//	docker run --cap-add NET_ADMIN --add-host waffle-host:host-gateway \
//	  -e BROKER_URL=... -e EXTERNAL_URL=http://1.1.1.1/ -v $PWD/probe:/probe:ro debian:bookworm-slim /probe
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/matt-riley/waffle/internal/netlock"
)

func main() {
	if err := netlock.LockdownExceptHost("waffle-host"); err != nil {
		fmt.Fprintf(os.Stderr, "lockdown failed: %v\n", err)
		os.Exit(2)
	}
	client := &http.Client{Timeout: 4 * time.Second}
	broker := os.Getenv("BROKER_URL")
	if broker == "" {
		fmt.Fprintln(os.Stderr, "BROKER_URL required")
		os.Exit(2)
	}
	resp, err := client.Get(broker + "/")
	if err != nil {
		fmt.Fprintf(os.Stderr, "broker get: %v\n", err)
		os.Exit(3)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "broker-ok" {
		fmt.Fprintf(os.Stderr, "broker status=%d body=%q\n", resp.StatusCode, body)
		os.Exit(3)
	}
	fmt.Println("broker=ok")

	ext := os.Getenv("EXTERNAL_URL")
	if ext == "" {
		ext = "http://1.1.1.1/"
	}
	resp2, err := client.Get(ext)
	if err == nil {
		_ = resp2.Body.Close()
		fmt.Fprintf(os.Stderr, "external unexpectedly succeeded status=%d\n", resp2.StatusCode)
		os.Exit(4)
	}
	fmt.Println("external=blocked")
	fmt.Fprintf(os.Stderr, "external err (expected): %v\n", err)
}
