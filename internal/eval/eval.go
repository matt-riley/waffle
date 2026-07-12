// Package eval is a zero-network agent evaluation harness (#63).
// Deterministic cases use scripted providers only — no real network.
// An optional live tier runs only when WAFFLE_EVAL_LIVE=1 and a provider
// is configured (skipped otherwise).
package eval

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/agent"
	"github.com/matt-riley/waffle/internal/codeintel"
	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/llmtest"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/skill"
	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/tool"
)

// Case is one deterministic eval.
type Case struct {
	Name string
	// Run executes the case; return error on failure.
	Run func(ctx context.Context) error
}

// Result is pass/fail for one case.
type Result struct {
	Name    string
	Passed  bool
	Message string
}

// RunRecord is one persisted eval invocation (#63).
type RunRecord struct {
	ID         int64
	Version    string
	StartedAt  time.Time
	FinishedAt time.Time
	Passed     int
	Failed     int
	Report     string
}

// ScriptedProvider is retained for call-site compatibility; prefer llmtest.Script.
type ScriptedProvider = llmtest.Script

// TextResponse is a helper.
func TextResponse(s string) llm.Response { return llmtest.Text(s) }

// ToolCallResponse asks for a tool then finishes on next call via script.
func ToolCallResponse(name, id string, input string) llm.Response {
	return llmtest.ToolCall(name, id, input)
}

// SeedNames are the six named deterministic offline evals (#63).
var SeedNames = []string{
	"recall-planted-fact",
	"redaction",
	"loop-termination",
	"summarize-preserves-fact",
	"skill-invocation",
	"tool-policy",
}

// Registry holds the six seed deterministic offline evals (#63) plus
// code-intel structural coverage. All must pass without network.
func Registry() []Case {
	return []Case{
		{Name: "recall-planted-fact", Run: evalRecallPlantedFact},
		{Name: "redaction", Run: evalRedaction},
		{Name: "loop-termination", Run: evalLoopTermination},
		{Name: "summarize-preserves-fact", Run: evalSummarizePreservesFact},
		{Name: "skill-invocation", Run: evalSkillInvocation},
		{Name: "tool-policy", Run: evalToolPolicy},
		// Extra structural coverage (not one of the six seed TOML names).
		{Name: "codeintel_symbol_over_broad_read", Run: evalCodeIntelSymbol},
	}
}

// LiveRegistry returns opt-in live provider cases. Empty unless WAFFLE_EVAL_LIVE=1.
// Without a configured live provider, cases are not registered (skipped — AC: live
// opt-in skipped when no provider). No default network provider is wired in-process.
func LiveRegistry() []Case {
	if os.Getenv("WAFFLE_EVAL_LIVE") != "1" {
		return nil
	}
	// Live tier requires a real provider; none is registered here by default.
	return nil
}

// evalsDir returns the filesystem path to evals/*.toml.
func evalsDir() string {
	candidates := []string{"evals"}
	if _, file, _, ok := runtime.Caller(0); ok {
		// internal/eval/eval.go → repo root/evals
		candidates = append(candidates, filepath.Join(filepath.Dir(file), "..", "..", "evals"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return ""
}

// DiscoverTOMLNames lists stem names of evals/*.toml under dir (or the
// module-relative evals/ directory when dir is empty).
func DiscoverTOMLNames(dir string) ([]string, error) {
	if dir == "" {
		dir = evalsDir()
	}
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".toml") {
			continue
		}
		names = append(names, strings.TrimSuffix(name, ".toml"))
	}
	return names, nil
}

// EnsureTOMLCovered fails if any evals/*.toml stem lacks a matching Registry case.
func EnsureTOMLCovered(dir string) error {
	names, err := DiscoverTOMLNames(dir)
	if err != nil {
		return err
	}
	reg := map[string]bool{}
	for _, c := range Registry() {
		reg[c.Name] = true
	}
	var missing []string
	for _, n := range names {
		if !reg[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("evals/*.toml without matching Registry case: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Plant fact in MEMORY.md and assert recall/agent reply contains Friday.
func evalRecallPlantedFact(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "eval-recall-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	st, err := store.Open(ctx, filepath.Join(dir, "e.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	notes := &memory.NotesIndex{DB: st.DB}
	ws := memory.Workspace{Dir: dir, Notes: notes}
	if _, err := ws.Append("standup is every Friday at 10am"); err != nil {
		return err
	}
	recall := memory.RecallTool{Notes: notes, WS: ws}
	out, err := recall.Run(ctx, json.RawMessage(`{"query":"Friday standup","scope":"notes"}`))
	if err != nil {
		return err
	}
	if !strings.Contains(out, "Friday") {
		return fmt.Errorf("recall missing Friday: %q", out)
	}
	sys, err := ws.SystemContext()
	if err != nil {
		return err
	}
	if !strings.Contains(sys, "Friday") {
		return fmt.Errorf("system context missing Friday")
	}
	p := &llmtest.Script{Responses: []llm.Response{TextResponse("Standup is every Friday.")}}
	a := &agent.Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m", MaxTokens: 64, MaxIterations: 5, System: sys}
	hist, err := a.Run(ctx, []llm.Message{llm.UserText("What day is standup?")}, agent.Hooks{})
	if err != nil {
		return err
	}
	if !strings.Contains(hist[len(hist)-1].Text(), "Friday") {
		return fmt.Errorf("agent reply missing Friday: %q", hist[len(hist)-1].Text())
	}
	return nil
}

// Redact scrubs SECRET_TOKEN so the provider never sees it in subsequent messages.
func evalRedaction(ctx context.Context) error {
	const secret = "SECRET_TOKEN"
	echo := named{n: "echo_secret", run: func() string { return "leaked " + secret + " value" }}
	p := &llmtest.Script{Responses: []llm.Response{
		ToolCallResponse("echo_secret", "1", `{}`),
		TextResponse("ok"),
	}}
	a := &agent.Agent{
		Provider: p, Tools: tool.NewRegistry(echo), Model: "m", MaxTokens: 64, MaxIterations: 5,
		Redact: func(s string) string { return strings.ReplaceAll(s, secret, "[redacted]") },
	}
	hist, err := a.Run(ctx, []llm.Message{llm.UserText("run")}, agent.Hooks{})
	if err != nil {
		return err
	}
	for _, m := range hist {
		for _, b := range m.Blocks {
			if b.ToolResult != nil && strings.Contains(b.ToolResult.Content, secret) {
				return fmt.Errorf("secret leaked into transcript: %q", b.ToolResult.Content)
			}
		}
	}
	if len(p.Requests) < 2 {
		return fmt.Errorf("want at least 2 provider calls, got %d", len(p.Requests))
	}
	raw, _ := json.Marshal(p.Requests[1].Messages)
	if strings.Contains(string(raw), secret) {
		return fmt.Errorf("provider saw SECRET_TOKEN in messages")
	}
	return nil
}

// Tool that always returns; MaxIterations=3 must stop without hanging.
func evalLoopTermination(ctx context.Context) error {
	loop := named{n: "always", run: func() string { return "again" }}
	def := ToolCallResponse("always", "1", `{}`)
	p := &llmtest.Script{Default: &def}
	a := &agent.Agent{
		Provider: p, Tools: tool.NewRegistry(loop), Model: "m",
		MaxTokens: 64, MaxIterations: 3,
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := a.Run(ctx, []llm.Message{llm.UserText("loop")}, agent.Hooks{})
	if err == nil {
		return fmt.Errorf("expected iteration stop, got nil error")
	}
	if !errors.Is(err, agent.ErrMaxIterations) {
		return fmt.Errorf("want ErrMaxIterations, got %v", err)
	}
	if p.Calls != 3 {
		return fmt.Errorf("provider calls=%d want 3", p.Calls)
	}
	return nil
}

// Long history with fact early; force summarize; final answer uses fact.
func evalSummarizePreservesFact(ctx context.Context) error {
	const fact = "NIGHTJAR"
	var hist []llm.Message
	hist = append(hist, llm.UserText("Remember: project codename is "+fact))
	hist = append(hist, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: "Noted the codename."}}})
	for i := 0; i < 22; i++ {
		hist = append(hist, llm.UserText(fmt.Sprintf("filler turn %d", i)))
		hist = append(hist, llm.Message{Role: llm.RoleAssistant, Blocks: []llm.Block{{Type: llm.BlockText, Text: fmt.Sprintf("ack %d", i)}}})
	}
	hist = append(hist, llm.UserText("What is the project codename?"))
	p := &llmtest.Script{Responses: []llm.Response{
		// Summarizer includes the fact.
		TextResponse("User stated project codename is NIGHTJAR; later filler turns."),
		// Agent answers with the fact from summary context.
		TextResponse("The project codename is NIGHTJAR."),
	}}
	a := &agent.Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m", MaxTokens: 64, MaxIterations: 5}
	ctx = agent.WithSession(ctx, "eval-sum")
	out, err := a.Run(ctx, hist, agent.Hooks{})
	if err != nil {
		return err
	}
	if p.Calls < 2 {
		return fmt.Errorf("expected summarize+answer calls, got %d", p.Calls)
	}
	last := out[len(out)-1].Text()
	if !strings.Contains(last, fact) {
		return fmt.Errorf("final answer missing fact: %q", last)
	}
	// Main request should carry the summary with the planted fact.
	main := p.Requests[len(p.Requests)-1]
	if !strings.Contains(main.System, fact) {
		return fmt.Errorf("summary system missing fact %q: %q", fact, main.System)
	}
	return nil
}

// System prompt includes skill body; scripted reply uses skill content.
func evalSkillInvocation(ctx context.Context) error {
	const skillBody = "SKILL_SNIPPET: always greet with waffle-salute"
	dir, err := os.MkdirTemp("", "eval-skill-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	skillDir := filepath.Join(dir, "greet")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: greet\ndescription: greeting ritual\n---\n"+skillBody+"\n"), 0o600); err != nil {
		return err
	}
	skills, err := skill.Discover(dir)
	if err != nil {
		return err
	}
	if len(skills) != 1 {
		return fmt.Errorf("want 1 skill, got %d", len(skills))
	}
	body, err := skills[0].Body()
	if err != nil {
		return err
	}
	if !strings.Contains(body, "waffle-salute") {
		return fmt.Errorf("skill body missing snippet: %q", body)
	}
	sys := skill.Index(skills) + "\nActive skill body:\n" + body
	p := &llmtest.Script{Responses: []llm.Response{TextResponse("waffle-salute, owner")}}
	a := &agent.Agent{Provider: p, Tools: tool.NewRegistry(), Model: "m", MaxTokens: 64, MaxIterations: 5, System: sys}
	hist, err := a.Run(ctx, []llm.Message{llm.UserText("hi")}, agent.Hooks{})
	if err != nil {
		return err
	}
	if !strings.Contains(hist[len(hist)-1].Text(), "waffle-salute") {
		return fmt.Errorf("reply missing skill content: %q", hist[len(hist)-1].Text())
	}
	if len(p.Requests) == 0 || !strings.Contains(p.Requests[0].System, "waffle-salute") {
		return fmt.Errorf("system missing skill snippet")
	}
	return nil
}

// Denied tool is not executed.
func evalToolPolicy(ctx context.Context) error {
	ran := false
	bash := named{n: "bash", run: func() string {
		ran = true
		return "ran"
	}}
	tb := tool.Restrict(tool.NewRegistry(bash), tool.Policy{Deny: []string{"bash"}})
	p := &llmtest.Script{Responses: []llm.Response{
		ToolCallResponse("bash", "1", `{"command":"echo hi"}`),
		TextResponse("ok"),
	}}
	a := &agent.Agent{Provider: p, Tools: tb, Model: "m", MaxTokens: 64, MaxIterations: 5}
	out, err := a.Run(ctx, []llm.Message{llm.UserText("run")}, agent.Hooks{})
	if err != nil {
		return err
	}
	if ran {
		return fmt.Errorf("denied bash tool executed")
	}
	found := false
	for _, m := range out {
		for _, b := range m.Blocks {
			if b.Type == llm.BlockToolResult && b.ToolResult != nil &&
				(strings.Contains(b.ToolResult.Content, "not permitted") ||
					strings.Contains(b.ToolResult.Content, "error:") ||
					b.ToolResult.IsError) {
				found = true
			}
		}
	}
	if !found {
		return fmt.Errorf("expected policy denial in tool results")
	}
	return nil
}

type named struct {
	n   string
	run func() string
}

func (n named) Def() llm.Tool {
	return llm.Tool{Name: n.n, InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (n named) Run(context.Context, json.RawMessage) (string, error) {
	if n.run != nil {
		return n.run(), nil
	}
	return "ran", nil
}

func evalCodeIntelSymbol(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "eval-codeintel-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	src := "package p\n\nfunc Hello() {}\n\nfunc Other() { Hello() }\n"
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "noise.go"), []byte("package p\nfunc Noise() {}\n"), 0o600); err != nil {
		return err
	}
	svc := codeintel.NewService(dir, "eval/repo", "main")
	tb := codeintel.Toolbox(svc)
	found := false
	for _, d := range tb.Defs() {
		if d.Name == "code_find_symbol" {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("code_find_symbol not discoverable")
	}
	out, err := tb.Run(ctx, "code_find_symbol", json.RawMessage(`{"name":"Hello"}`))
	if err != nil {
		return err
	}
	if !strings.Contains(out, "Hello") || !strings.Contains(out, "a.go") {
		return fmt.Errorf("symbol result missing Hello/a.go: %s", out)
	}
	if !strings.Contains(out, "text-fallback") && !strings.Contains(out, "source") {
		return fmt.Errorf("missing source classification: %s", out)
	}
	if strings.Contains(out, "noise.go") {
		return fmt.Errorf("find_symbol should not require loading noise.go content into result")
	}
	return nil
}

// RunAll executes cases and writes a report. Returns non-zero failure count.
func RunAll(ctx context.Context, out io.Writer, cases []Case) int {
	fails := 0
	for _, c := range cases {
		err := c.Run(ctx)
		if err != nil {
			fails++
			fmt.Fprintf(out, "FAIL %s: %v\n", c.Name, err)
			continue
		}
		fmt.Fprintf(out, "PASS %s\n", c.Name)
	}
	fmt.Fprintf(out, "%d/%d passed\n", len(cases)-fails, len(cases))
	return fails
}

// RunReport is the structured outcome of one harness invocation.
type RunReport struct {
	Passed int
	Failed int
	Text   string
}

// Run collects a report string and failure count.
func Run(ctx context.Context, cases []Case) RunReport {
	var buf strings.Builder
	fails := RunAll(ctx, &buf, cases)
	return RunReport{
		Passed: len(cases) - fails,
		Failed: fails,
		Text:   buf.String(),
	}
}

// RecordRun persists an eval run with version and timestamps (#63).
func RecordRun(ctx context.Context, db *sql.DB, version string, started time.Time, report RunReport) error {
	if db == nil {
		return nil
	}
	if version == "" {
		version = "dev"
	}
	finished := time.Now().UTC()
	_, err := db.ExecContext(ctx, `
		INSERT INTO eval_runs (version, started_at, finished_at, passed, failed, report)
		VALUES (?, ?, ?, ?, ?, ?)`,
		version,
		started.UTC().Format(time.RFC3339Nano),
		finished.Format(time.RFC3339Nano),
		report.Passed,
		report.Failed,
		report.Text,
	)
	return err
}

// ListHistory returns recent eval runs, newest first.
func ListHistory(ctx context.Context, db *sql.DB, limit int) ([]RunRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, version, started_at, finished_at, passed, failed, report
		FROM eval_runs ORDER BY finished_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RunRecord
	for rows.Next() {
		var r RunRecord
		var started, finished string
		if err := rows.Scan(&r.ID, &r.Version, &started, &finished, &r.Passed, &r.Failed, &r.Report); err != nil {
			return nil, err
		}
		r.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
		r.FinishedAt, _ = time.Parse(time.RFC3339Nano, finished)
		out = append(out, r)
	}
	return out, rows.Err()
}

// FormatHistory writes human-readable eval history lines.
func FormatHistory(w io.Writer, records []RunRecord) {
	if len(records) == 0 {
		fmt.Fprintln(w, "no eval history yet — run: waffle eval")
		return
	}
	for _, r := range records {
		status := "ok"
		if r.Failed > 0 {
			status = "FAIL"
		}
		fmt.Fprintf(w, "%s  %s  %s  %d passed / %d failed  version=%s\n",
			r.FinishedAt.UTC().Format(time.RFC3339), status, fmt.Sprintf("#%d", r.ID), r.Passed, r.Failed, r.Version)
	}
}

// GuardHTTPTransport replaces http.DefaultTransport with one that fails every
// request. Used by tests to prove the deterministic tier is zero-network.
func GuardHTTPTransport(onDial func(req *http.Request)) (restore func()) {
	prev := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if onDial != nil {
			onDial(req)
		}
		return nil, fmt.Errorf("eval: network disabled (zero-network harness)")
	})
	return func() { http.DefaultTransport = prev }
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
