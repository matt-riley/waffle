package policy

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matt-riley/waffle/internal/store"
)

func TestSplitCommandQuotes(t *testing.T) {
	got := SplitCommand(`echo "hello world" 'x y' z`)
	want := []string{"echo", "hello world", "x y", "z"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q want %q", i, got[i], want[i])
		}
	}
}

func TestMatchBashPrefixQuoted(t *testing.T) {
	if !matchBashPrefix(`rm -rf "/tmp/foo bar"`, `rm -rf`) {
		t.Fatal("expected prefix match on quoted path")
	}
	if matchBashPrefix(`echo rm -rf /`, `rm -rf`) {
		t.Fatal("should not match when rm is not leading")
	}
}

func TestDenyWithGuidanceFeedback(t *testing.T) {
	e := &Engine{
		Enforcer: EnforcerFeedback,
		Rules: []Rule{{
			Name:     "no-rm",
			Tool:     "bash",
			Match:    "rm -rf",
			Action:   ActionDeny,
			Guidance: "use safer cleanup",
		}},
	}
	if err := e.Rules[0].Compile(); err != nil {
		t.Fatal(err)
	}
	d := e.Check("bash", json.RawMessage(`{"command":"rm -rf /tmp/x"}`))
	if d.Allowed {
		t.Fatal("expected deny")
	}
	if !strings.Contains(d.Message, "safer cleanup") {
		t.Fatalf("message = %q", d.Message)
	}
}

func TestDenyWithoutFeedbackOmitsGuidance(t *testing.T) {
	e := &Engine{
		Enforcer: EnforcerNone,
		Rules: []Rule{{
			Name:     "no-rm",
			Tool:     "bash",
			Match:    "rm -rf",
			Action:   ActionDeny,
			Guidance: "use safer cleanup",
		}},
	}
	d := e.Check("bash", json.RawMessage(`{"command":"rm -rf /tmp/x"}`))
	if d.Allowed {
		t.Fatal("expected deny")
	}
	if strings.Contains(d.Message, "safer cleanup") {
		t.Fatalf("none enforcer should omit guidance, got %q", d.Message)
	}
}

func TestRegexRule(t *testing.T) {
	e := &Engine{
		Rules: []Rule{{
			Name:   "curl-http",
			Tool:   "bash",
			Regex:  `curl\s+http://`,
			Action: ActionDeny,
		}},
	}
	if err := e.Rules[0].Compile(); err != nil {
		t.Fatal(err)
	}
	d := e.Check("bash", json.RawMessage(`{"command":"curl http://evil.example"}`))
	if d.Allowed {
		t.Fatal("expected deny")
	}
	d2 := e.Check("bash", json.RawMessage(`{"command":"curl https://ok.example"}`))
	if !d2.Allowed {
		t.Fatal("https should be allowed")
	}
}

func TestToolOnlyDeny(t *testing.T) {
	e := &Engine{Rules: []Rule{{Name: "no-fetch", Tool: "fetch", Action: ActionDeny}}}
	d := e.Check("fetch", json.RawMessage(`{"url":"https://x"}`))
	if d.Allowed {
		t.Fatal("expected deny")
	}
	if d := e.Check("bash", json.RawMessage(`{"command":"echo hi"}`)); !d.Allowed {
		t.Fatal("bash should be allowed")
	}
}

func TestPolicyAuditRow(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := NewEngineFromStore(st, []Rule{{
		Name:   "no-rm",
		Tool:   "bash",
		Match:  "rm -rf",
		Action: ActionDeny,
	}}, EnforcerFeedback)
	e.SessionID = "sess-1"
	d := e.CheckAndAudit(ctx, "bash", json.RawMessage(`{"command":"rm -rf /tmp/x"}`))
	if d.Allowed {
		t.Fatal("expected deny")
	}
	var n int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM policy_audit WHERE session = ? AND verdict = 'deny' AND rule = 'no-rm'`, "sess-1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
}

func TestAllowWhenNoMatch(t *testing.T) {
	e := &Engine{Rules: []Rule{{Name: "no-rm", Tool: "bash", Match: "rm -rf", Action: ActionDeny}}}
	d := e.Check("bash", json.RawMessage(`{"command":"echo ok"}`))
	if !d.Allowed {
		t.Fatal("expected allow")
	}
}
