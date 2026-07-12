// Package policy implements host-side action-level tool rules (#66).
// Rules match tool name plus optional bash command prefix/regex, produce
// allow/deny verdicts with guidance, and optionally audit each decision.
//
// Shell matching is best-effort: commands are split respecting quotes so a
// prefix can match the first tokens, but shell indirection (eval, variables,
// $(), aliases, redirection-only wrappers) is not fully analyzed — operators
// should not rely on policy alone for high-assurance isolation.
package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/matt-riley/waffle/internal/store"
)

// Action is allow or deny.
const (
	ActionAllow = "allow"
	ActionDeny  = "deny"
)

// Enforcer modes under [sandbox] enforcer (#66).
const (
	EnforcerNone     = "none"
	EnforcerFeedback = "feedback"
)

// Rule is one configured action-level policy rule.
type Rule struct {
	// Name is a stable audit label.
	Name string
	// Tool is the tool name to match (e.g. "bash"). Empty matches any tool.
	Tool string
	// Match is a command prefix (bash tokens after quote-aware split).
	Match string
	// Regex, when set, matches the raw command string.
	Regex string
	// Action is "allow" or "deny".
	Action string
	// Guidance is included in deny messages when enforcer=feedback.
	Guidance string

	re *regexp.Regexp
}

// Compile prepares Regex if present.
func (r *Rule) Compile() error {
	if r.Regex == "" {
		return nil
	}
	re, err := regexp.Compile(r.Regex)
	if err != nil {
		return fmt.Errorf("policy rule %q: bad regex: %w", r.Name, err)
	}
	r.re = re
	return nil
}

// Engine evaluates rules in order; first matching rule wins. Deny wins when
// multiple rules match only because evaluation stops at the first match —
// put more specific rules first.
type Engine struct {
	Rules []Rule
	// Enforcer is none|feedback (default none).
	Enforcer string
	// AuditDB optional; when set, decisions are written to policy_audit.
	AuditDB *sql.DB
	// SessionID is recorded on audit rows when set.
	SessionID string
}

// Decision is the outcome of evaluating a tool call.
type Decision struct {
	Allowed  bool
	Rule     string
	Verdict  string // allow | deny
	Message  string
	Guidance string
}

// Check evaluates rules against a tool invocation. name is the tool name;
// input is the JSON tool input (bash uses {"command":"..."}).
func (e *Engine) Check(name string, input json.RawMessage) Decision {
	cmd := extractCommand(name, input)
	for _, r := range e.Rules {
		if !r.matches(name, cmd) {
			continue
		}
		d := Decision{
			Rule:     r.Name,
			Verdict:  r.Action,
			Guidance: r.Guidance,
			Allowed:  r.Action != ActionDeny,
		}
		if !d.Allowed {
			d.Message = e.denyMessage(r, cmd)
		}
		return d
	}
	return Decision{Allowed: true, Verdict: ActionAllow}
}

func (e *Engine) denyMessage(r Rule, cmd string) string {
	base := fmt.Sprintf("tool call denied by policy rule %q", r.Name)
	if r.Tool != "" {
		base = fmt.Sprintf("%s call denied by policy rule %q", r.Tool, r.Name)
	}
	if e.Enforcer == EnforcerFeedback && r.Guidance != "" {
		return base + " — " + r.Guidance
	}
	if e.Enforcer == EnforcerFeedback && cmd != "" {
		return base + " (command matched policy)"
	}
	return base
}

// CheckAndAudit is Check plus optional policy_audit write.
func (e *Engine) CheckAndAudit(ctx context.Context, name string, input json.RawMessage) Decision {
	return e.CheckAndAuditSession(ctx, e.SessionID, name, input)
}

// CheckAndAuditSession is CheckAndAudit with an explicit session id (preferred
// under concurrent serve).
func (e *Engine) CheckAndAuditSession(ctx context.Context, sessionID, name string, input json.RawMessage) Decision {
	d := e.Check(name, input)
	if e.AuditDB != nil && (d.Rule != "" || !d.Allowed) {
		_ = LogAudit(ctx, e.AuditDB, sessionID, name, extractCommand(name, input), d)
	}
	return d
}

// LogAudit inserts a policy_audit row.
func LogAudit(ctx context.Context, db *sql.DB, session, tool, command string, d Decision) error {
	if db == nil {
		return nil
	}
	detail := d.Message
	if detail == "" {
		detail = d.Guidance
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO policy_audit (at, session, tool, command, rule, verdict, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano),
		session, tool, truncate(command, 500), d.Rule, d.Verdict, truncate(detail, 1000))
	return err
}

// NewEngineFromStore builds an engine with optional audit persistence.
func NewEngineFromStore(st *store.Store, rules []Rule, enforcer string) *Engine {
	e := &Engine{Rules: rules, Enforcer: enforcer}
	if e.Enforcer == "" {
		e.Enforcer = EnforcerNone
	}
	if st != nil {
		e.AuditDB = st.DB
	}
	for i := range e.Rules {
		_ = e.Rules[i].Compile()
	}
	return e
}

func (r Rule) matches(tool, cmd string) bool {
	if r.Tool != "" && r.Tool != tool {
		return false
	}
	if r.re != nil {
		return r.re.MatchString(cmd)
	}
	if r.Match == "" {
		// Tool-only rule.
		return r.Tool != ""
	}
	if tool != "bash" {
		// Prefix/match only applies to bash command text.
		return strings.HasPrefix(strings.TrimSpace(cmd), r.Match)
	}
	return matchBashPrefix(cmd, r.Match)
}

func extractCommand(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	if name == "bash" {
		var in struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return ""
		}
		return in.Command
	}
	return string(input)
}

// SplitCommand splits a shell command into tokens respecting single/double
// quotes and backslash escapes outside quotes. It does not expand variables
// or evaluate substitutions — see package docs on indirection limits.
func SplitCommand(cmd string) []string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return nil
	}
	var (
		tokens []string
		cur    strings.Builder
		quote  rune // 0, '\'' or '"'
		esc    bool
	)
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(cmd); {
		r, size := utf8.DecodeRuneInString(cmd[i:])
		i += size
		if esc {
			cur.WriteRune(r)
			esc = false
			continue
		}
		if quote == 0 && r == '\\' {
			esc = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case ' ', '\t', '\n', '\r':
			flush()
		default:
			if unicode.IsSpace(r) {
				flush()
			} else {
				cur.WriteRune(r)
			}
		}
	}
	flush()
	return tokens
}

// matchBashPrefix reports whether cmd's leading tokens match the prefix string
// (itself split with the same quote rules).
func matchBashPrefix(cmd, prefix string) bool {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return false
	}
	// Fast path: raw string prefix after trim (covers simple cases).
	if strings.HasPrefix(strings.TrimSpace(cmd), prefix) {
		return true
	}
	want := SplitCommand(prefix)
	got := SplitCommand(cmd)
	if len(want) == 0 || len(got) < len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
