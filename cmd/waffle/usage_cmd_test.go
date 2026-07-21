package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/store"
	usagepkg "github.com/matt-riley/waffle/internal/usage"
)

func TestUsageJSONOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()

	st, err := store.Open(ctx, filepath.Join(home, "waffle.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := usagepkg.New(st).AddRequest(ctx, "sess-1", llm.Usage{InputTokens: 10, OutputTokens: 5}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"--json"}, {"ls", "--json"}, {"--json", "ls"}} {
		t.Run(joinArgs(args), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := usageCmd(ctx, args, &stdout, &stderr); err != nil {
				t.Fatalf("usage %v: %v", args, err)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			if !json.Valid(stdout.Bytes()) {
				t.Fatalf("stdout is not valid JSON: %s", stdout.String())
			}
			var rows []usageRowJSON
			if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(rows) == 0 {
				t.Fatal("want at least one usage row")
			}
			found := false
			for _, r := range rows {
				if r.SessionID == "sess-1" && r.Requests >= 1 && r.InputTokens >= 10 && r.OutputTokens >= 5 {
					found = true
					if r.Period == "" || r.PeriodStart == "" {
						t.Fatalf("row missing period fields: %+v", r)
					}
				}
			}
			if !found {
				t.Fatalf("rows = %+v, want sess-1 usage", rows)
			}
		})
	}
}

func TestUsageJSONEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	if err := usageCmd(ctx, []string{"--json"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var rows []usageRowJSON
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if rows == nil || len(rows) != 0 {
		t.Fatalf("rows = %+v, want empty non-nil array", rows)
	}
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}
