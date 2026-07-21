package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

func TestSessionListJSONOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()

	st, err := store.Open(ctx, home+"/waffle.db")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := session.New(st).Create(ctx, "planning")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.New(st).SetSummary(ctx, sess.ID, "discussed roadmap"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"ls", "--json"}, {"--json"}, {"--json", "ls"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := sessionCmd(ctx, args, strings.NewReader(""), &stdout, &stderr); err != nil {
				t.Fatalf("session %v: %v", args, err)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			if !json.Valid(stdout.Bytes()) {
				t.Fatalf("stdout is not valid JSON: %s", stdout.String())
			}
			var list []sessionJSON
			if err := json.Unmarshal(stdout.Bytes(), &list); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(list) != 1 {
				t.Fatalf("list = %+v, want one", list)
			}
			if list[0].ID != sess.ID || list[0].Title != "planning" || list[0].Summary != "discussed roadmap" {
				t.Fatalf("session = %+v", list[0])
			}
		})
	}
}

func TestSessionListJSONEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WAFFLE_HOME", home)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	if err := sessionCmd(ctx, []string{"ls", "--json"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var list []sessionJSON
	if err := json.Unmarshal(stdout.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if list == nil || len(list) != 0 {
		t.Fatalf("list = %+v, want empty non-nil array", list)
	}
}
