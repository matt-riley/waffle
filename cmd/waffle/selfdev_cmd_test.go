package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/config"
)

func TestUpgradeNoVerifyIsOptInUnsafeAndDocumented(t *testing.T) {
	ref, noVerify, err := parseUpgradeArgs(nil)
	if err != nil || ref != "" || noVerify {
		t.Fatalf("default parse ref=%q noVerify=%v err=%v", ref, noVerify, err)
	}
	ref, noVerify, err = parseUpgradeArgs([]string{"v1", "--no-verify"})
	if err != nil || ref != "v1" || !noVerify {
		t.Fatalf("explicit parse ref=%q noVerify=%v err=%v", ref, noVerify, err)
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
