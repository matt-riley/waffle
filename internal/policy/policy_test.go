package policy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
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
	if !MatchBashPrefix(`rm -rf "/tmp/foo bar"`, `rm -rf`) {
		t.Fatal("expected prefix match on quoted path")
	}
	if MatchBashPrefix(`echo rm -rf /`, `rm -rf`) {
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
	e, err := NewEngineFromStore(st, []Rule{{
		Name:   "no-rm",
		Tool:   "bash",
		Match:  "rm -rf",
		Action: ActionDeny,
	}}, EnforcerFeedback)
	if err != nil {
		t.Fatal(err)
	}
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

func TestTestsBeforeCommitRequireE2E(t *testing.T) {
	// Full tests-before-commit scenario (#66):
	// edit → commit blocked → test passes → commit allowed → write again → commit denied.
	e, err := NewEngine([]Rule{
		{
			Name:   "go-test-green",
			Tool:   "bash",
			Match:  "go test",
			Action: ActionAllow,
		},
		{
			Name:     "tests-before-commit",
			Tool:     "bash",
			Match:    "git commit",
			Action:   ActionRequire,
			Requires: "go-test-green",
			Guidance: "run `go test ./...` after your last edit and before committing",
		},
	}, EnforcerFeedback)
	if err != nil {
		t.Fatal(err)
	}
	const sess = "sess-require-e2e"
	commitIn := json.RawMessage(`{"command":"git commit -m ok"}`)
	testIn := json.RawMessage(`{"command":"go test ./..."}`)
	writeIn := json.RawMessage(`{"path":"x.go","content":"package x"}`)

	// 1. Simulate write_file success → Observe
	e.ObserveSuccess(sess, "write_file", writeIn)

	// 2. git commit Check → denied
	d := e.CheckSession(sess, "bash", commitIn)
	if d.Allowed {
		t.Fatal("commit should be blocked after write without tests")
	}
	if !strings.Contains(d.Message, "tests-before-commit") {
		t.Fatalf("deny message missing rule name: %q", d.Message)
	}
	if !strings.Contains(d.Message, "go test") {
		t.Fatalf("deny message missing guidance: %q", d.Message)
	}

	// 3. go test success → Observe
	e.ObserveSuccess(sess, "bash", testIn)

	// 4. git commit Check → allowed
	d = e.CheckSession(sess, "bash", commitIn)
	if !d.Allowed {
		t.Fatalf("commit should be allowed after green tests: %q", d.Message)
	}

	// 5. write again → commit denied again
	e.ObserveSuccess(sess, "edit_file", writeIn)
	d = e.CheckSession(sess, "bash", commitIn)
	if d.Allowed {
		t.Fatal("commit should be blocked again after subsequent edit")
	}
}

func TestChildPolicyCannotWidenParent(t *testing.T) {
	parent := []Rule{{
		Name:   "no-curl",
		Tool:   "bash",
		Match:  "curl",
		Action: ActionDeny,
	}}
	child := []Rule{{
		Name:   "allow-curl",
		Tool:   "bash",
		Match:  "curl",
		Action: ActionAllow,
	}}
	_, err := Narrow(parent, child)
	if err == nil {
		t.Fatal("expected child allow of parent-denied match to be rejected")
	}
	if !strings.Contains(err.Error(), "allow-curl") || !strings.Contains(err.Error(), "no-curl") {
		t.Fatalf("error should name both rules: %v", err)
	}

	// Child denying further is fine (narrowing).
	narrower := []Rule{{
		Name:   "no-wget",
		Tool:   "bash",
		Match:  "wget",
		Action: ActionDeny,
	}}
	merged, err := Narrow(parent, narrower)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 2 {
		t.Fatalf("merged len = %d", len(merged))
	}

	// Parent tool-only deny: child cannot allow that tool.
	parentTool := []Rule{{Name: "no-bash", Tool: "bash", Action: ActionDeny}}
	childAllow := []Rule{{Name: "yes-bash", Tool: "bash", Match: "echo", Action: ActionAllow}}
	if _, err := Narrow(parentTool, childAllow); err == nil {
		t.Fatal("child allow of parent-denied tool should fail")
	}
}

func TestNarrowRejectsUnverifiableChildRegexAllow(t *testing.T) {
	parent := []Rule{{Name: "no-curl", Tool: "bash", Match: "curl", Action: ActionDeny}}
	child := []Rule{{Name: "allow-curl-re", Tool: "bash", Regex: `curl\s+https://`, Action: ActionAllow}}
	if _, err := Narrow(parent, child); err == nil {
		t.Fatal("expected error for regex allow")
	} else if !strings.Contains(err.Error(), "allow-curl-re") {
		t.Fatalf("error should name child rule: %v", err)
	} else if !strings.Contains(err.Error(), "regex allow") {
		t.Fatalf("error should mention regex allow: %v", err)
	}

	// Child regex deny still allowed (deny only narrows).
	childDeny := []Rule{{Name: "deny-curl-re", Tool: "bash", Regex: `curl\s+https://`, Action: ActionDeny}}
	merged, err := Narrow(parent, childDeny)
	if err != nil {
		t.Fatalf("child regex deny should narrow successfully: %v", err)
	}
	if len(merged) != 2 {
		t.Fatalf("merged len = %d", len(merged))
	}

	// Exact Match allow that does not conflict with parent still succeeds.
	parentOther := []Rule{{Name: "no-wget", Tool: "bash", Match: "wget", Action: ActionDeny}}
	childMatchAllow := []Rule{{Name: "allow-echo", Tool: "bash", Match: "echo", Action: ActionAllow}}
	if _, err := Narrow(parentOther, childMatchAllow); err != nil {
		t.Fatalf("exact Match allow that does not widen parent should succeed: %v", err)
	}

	// Regex allow is rejected even when parent has no conflicting denials
	// (unverifiable subset — fail closed for future repo-policy use).
	emptyParent := []Rule{}
	if _, err := Narrow(emptyParent, child); err == nil {
		t.Fatal("expected regex allow to be rejected even without parent denials")
	}
}

func TestSessionEventsSatisfiedSinceWrite(t *testing.T) {
	ev := NewSessionEvents()
	if ev.SatisfiedSinceWrite("s", "k") {
		t.Fatal("empty should be unsatisfied")
	}
	ev.NoteSatisfy("s", "k")
	if !ev.SatisfiedSinceWrite("s", "k") {
		t.Fatal("satisfy without write should hold")
	}
	ev.NoteWrite("s")
	if ev.SatisfiedSinceWrite("s", "k") {
		t.Fatal("write invalidates prior satisfy")
	}
	ev.NoteSatisfy("s", "k")
	if !ev.SatisfiedSinceWrite("s", "k") {
		t.Fatal("re-satisfy after write should hold")
	}
}

func TestCompileRejectsEmptySelectors(t *testing.T) {
	r := Rule{Name: "global-deny", Action: ActionDeny}
	err := r.Compile()
	if err == nil {
		t.Fatal("expected Compile to reject empty tool/match/regex")
	}
	msg := err.Error()
	if !strings.Contains(msg, "global-deny") {
		t.Fatalf("error should name the rule: %q", msg)
	}
	if !strings.Contains(msg, "tool") || !strings.Contains(msg, "match") || !strings.Contains(msg, "regex") {
		t.Fatalf("error should mention tool/match/regex: %q", msg)
	}

	// Empty Tool is valid when Match is set (any-tool prefix match).
	ok := Rule{Name: "any-rm", Match: "rm -rf", Action: ActionDeny}
	if err := ok.Compile(); err != nil {
		t.Fatalf("match-only rule should compile: %v", err)
	}
	// Empty Tool is valid when Regex is set.
	okRE := Rule{Name: "any-curl", Regex: `curl\s+http://`, Action: ActionDeny}
	if err := okRE.Compile(); err != nil {
		t.Fatalf("regex-only rule should compile: %v", err)
	}
}

func TestNewEngineRejectsEmptySelectors(t *testing.T) {
	_, err := NewEngine([]Rule{{Name: "noop-global", Action: ActionDeny}}, EnforcerNone)
	if err == nil {
		t.Fatal("NewEngine should reject all-empty selectors")
	}
	if !strings.Contains(err.Error(), "noop-global") {
		t.Fatalf("error should name the rule: %v", err)
	}
	if !strings.Contains(err.Error(), "tool") {
		t.Fatalf("error should mention tool: %v", err)
	}

	_, err = NewEngineFromStore(nil, []Rule{{Name: "noop-store", Action: ActionDeny}}, EnforcerNone)
	if err == nil {
		t.Fatal("NewEngineFromStore should reject all-empty selectors")
	}
	if !strings.Contains(err.Error(), "noop-store") {
		t.Fatalf("error should name the rule: %v", err)
	}
}

func TestLogMutationAlwaysWritesAuditRow(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "mutation-audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := LogMutation(ctx, st.DB, "sess-desk", "desk.mutation", "POST /api/v1/desk/memory/attach", "status=200"); err != nil {
		t.Fatal(err)
	}
	var tool, command, rule, verdict, detail string
	if err := st.DB.QueryRowContext(ctx, `
		SELECT tool, command, rule, verdict, detail FROM policy_audit WHERE session = ?`, "sess-desk").
		Scan(&tool, &command, &rule, &verdict, &detail); err != nil {
		t.Fatal(err)
	}
	if tool != "desk.mutation" || command != "POST /api/v1/desk/memory/attach" || rule != "desk.mutation" || verdict != ActionAllow {
		t.Fatalf("audit row = tool=%q command=%q rule=%q verdict=%q", tool, command, rule, verdict)
	}
	if detail != "status=200" {
		t.Fatalf("detail = %q", detail)
	}
}

// closedAuditDB returns a policy_audit database handle that is already closed,
// standing in for the busy/closed/cancelled cases that lose an audit write.
func closedAuditDB(t *testing.T) *sql.DB {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "closed-audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return st.DB
}

func TestCheckAndAuditSessionReportsFailedAuditWrite(t *testing.T) {
	var logs bytes.Buffer
	e, err := NewEngine([]Rule{{
		Name:   "no-rm",
		Tool:   "bash",
		Match:  "rm -rf",
		Action: ActionDeny,
	}}, EnforcerFeedback)
	if err != nil {
		t.Fatal(err)
	}
	e.AuditDB = closedAuditDB(t)
	e.Log = slog.New(slog.NewTextHandler(&logs, nil))

	d := e.CheckAndAuditSession(context.Background(), "sess-1", "bash", json.RawMessage(`{"command":"rm -rf /tmp/SECRET_PATH"}`))
	if d.Allowed {
		t.Fatal("a denial must still deny when its audit row is lost")
	}
	body := logs.String()
	for _, want := range []string{"msg=\"policy audit write failed\"", "session=sess-1", "tool=bash", "operation=deny"} {
		if !strings.Contains(body, want) {
			t.Fatalf("logs missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "SECRET_PATH") {
		t.Fatalf("audited command leaked into the failure log: %s", body)
	}
}

func TestReportAuditFailureIgnoresSuccess(t *testing.T) {
	var logs bytes.Buffer
	ReportAuditFailure(slog.New(slog.NewTextHandler(&logs, nil)), nil, "sess", "bash", "allow")
	if logs.Len() != 0 {
		t.Fatalf("successful audit write logged: %s", logs.String())
	}
}
