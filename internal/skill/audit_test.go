package skill

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
	"github.com/matt-riley/waffle/internal/store"
)

func TestMineToolErrorsAndPropose(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := session.New(st)
	sess, err := sessions.Create(ctx, "errs")
	if err != nil {
		t.Fatal(err)
	}
	errMsg := "error: exit status 1\nno such file or directory: /tmp/missing-dep"
	for i := 0; i < 3; i++ {
		if err := sessions.AppendTurn(ctx, sess.ID, llm.Message{
			Role: llm.RoleUser,
			Blocks: []llm.Block{{
				Type: llm.BlockToolResult,
				ToolResult: &llm.ToolResult{
					ToolUseID: "t1",
					Content:   errMsg,
					IsError:   true,
				},
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	patterns, err := MineToolErrors(ctx, sessions, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(patterns) == 0 {
		t.Fatal("expected patterns")
	}
	if patterns[0].Count < 2 {
		t.Fatalf("count = %d", patterns[0].Count)
	}

	ws := memory.Workspace{Dir: t.TempDir()}
	gate := &memory.Gate{Mode: "auto", WS: ws}
	n, err := ProposeSkills(ctx, patterns, gate, 2)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("proposed %d", n)
	}
	pending, err := gate.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) < 1 {
		t.Fatal("expected pending skill candidate")
	}
}

func TestFingerprintError(t *testing.T) {
	sig, sample := fingerprintError("error: open /tmp/foo123: no such file or directory")
	if sig == "" || sample == "" {
		t.Fatalf("sig=%q sample=%q", sig, sample)
	}
	sig2, _ := fingerprintError("error: open /var/bar999: no such file or directory")
	if sig != sig2 {
		t.Fatalf("expected same fingerprint: %q vs %q", sig, sig2)
	}
}
