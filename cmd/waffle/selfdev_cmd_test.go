package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/config"
)

func TestUpgradeNoVerifyIsOptInUnsafeAndDocumented(t *testing.T) {
	ref, noVerify, help, err := parseUpgradeArgs(nil)
	if err != nil || ref != "" || noVerify || help {
		t.Fatalf("default parse ref=%q noVerify=%v help=%v err=%v", ref, noVerify, help, err)
	}
	ref, noVerify, help, err = parseUpgradeArgs([]string{"v1", "--no-verify"})
	if err != nil || ref != "v1" || !noVerify || help {
		t.Fatalf("explicit parse ref=%q noVerify=%v help=%v err=%v", ref, noVerify, help, err)
	}
	if !config.Default().Selfdev.Verify {
		t.Fatal("selfdev verification must default enabled")
	}
	_, file, _, _ := runtime.Caller(0)
	for _, path := range []string{filepath.Join(filepath.Dir(file), "main.go"), filepath.Join(filepath.Dir(file), "..", "..", "docs", "plan.md")} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "--no-verify") || !strings.Contains(strings.ToLower(string(raw)), "unsafe") {
			t.Fatalf("%s lacks unsafe --no-verify documentation", path)
		}
	}
}

func TestParseUpgradeArgsHelp(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"-h"},
		{"help"},
	} {
		ref, noVerify, help, err := parseUpgradeArgs(args)
		if err != nil {
			t.Fatalf("parseUpgradeArgs(%v): %v", args, err)
		}
		if !help {
			t.Fatalf("parseUpgradeArgs(%v): help=false, want true", args)
		}
		if ref != "" || noVerify {
			t.Fatalf("parseUpgradeArgs(%v): ref=%q noVerify=%v, want empty ref and noVerify=false", args, ref, noVerify)
		}
	}
}

func TestUpgradeCmdHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := upgradeCmd(context.Background(), []string{"--help"}, &stdout, &stderr); err != nil {
		t.Fatalf("upgradeCmd --help: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "Usage: waffle upgrade") {
		t.Fatalf("stdout missing usage header:\n%s", out)
	}
	if !strings.Contains(out, "--no-verify") {
		t.Fatalf("stdout missing --no-verify docs:\n%s", out)
	}
	if !strings.Contains(out, "-h, --help") {
		t.Fatalf("stdout missing help flag docs:\n%s", out)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}
