// Package policy implements host-side action-level tool rules (#66).
// Rules match tool name plus optional bash command prefix/regex, produce
// allow/deny/require verdicts with guidance, and optionally audit each decision.
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
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/matt-riley/waffle/internal/store"
	"github.com/matt-riley/waffle/internal/textcut"
)

// Action is allow, deny, or require.
const (
	ActionAllow   = "allow"
	ActionDeny    = "deny"
	ActionRequire = "require"
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
	// Tool is the tool name to match (e.g. "bash"). Empty means any tool only
	// when Match or Regex is set; a rule with Tool, Match, and Regex all empty
	// is invalid (Compile rejects it).
	Tool string
	// Match is a command prefix (bash tokens after quote-aware split).
	Match string
	// Regex, when set, matches the raw command string.
	Regex string
	// Action is "allow", "deny", or "require".
	Action string
	// Requires is the predicate event name (rule Name or satisfy key) that
	// must have occurred after the last write for ActionRequire rules.
	Requires string
	// Guidance is included in deny messages when enforcer=feedback (and
	// always included for require denials as "because" text).
	Guidance string

	re *regexp.Regexp
}

// Compile validates selectors and prepares Regex if present.
// A rule must set at least one of Tool, Match, or Regex.
func (r *Rule) Compile() error {
	if r.Tool == "" && r.Match == "" && r.Regex == "" {
		return fmt.Errorf("policy rule %q: need tool, match, or regex", r.Name)
	}
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

// SessionEvents is a per-session cross-event log used by require rules.
// Write tools bump writeSeq; successful predicate tools record satisfy
// keys at the current writeSeq so SatisfiedSinceWrite can check freshness.
type SessionEvents struct {
	mu sync.Mutex
	by map[string]*sessState
}

type sessState struct {
	writeSeq int
	// satisfy key -> last writeSeq when satisfied
	satisfy map[string]int
}

// NewSessionEvents returns an empty session event log.
func NewSessionEvents() *SessionEvents {
	return &SessionEvents{by: make(map[string]*sessState)}
}

func (s *SessionEvents) state(session string) *sessState {
	if s.by == nil {
		s.by = make(map[string]*sessState)
	}
	st, ok := s.by[session]
	if !ok {
		st = &sessState{satisfy: make(map[string]int)}
		s.by[session] = st
	}
	if st.satisfy == nil {
		st.satisfy = make(map[string]int)
	}
	return st
}

// NoteWrite records that a write/edit tool succeeded in session.
func (s *SessionEvents) NoteWrite(session string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(session)
	st.writeSeq++
}

// NoteSatisfy records that a predicate event key was satisfied in session.
func (s *SessionEvents) NoteSatisfy(session, key string) {
	if s == nil || key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(session)
	st.satisfy[key] = st.writeSeq
}

// SatisfiedSinceWrite reports whether key was satisfied at or after the most
// recent write (including "no writes yet" if the key was ever satisfied).
func (s *SessionEvents) SatisfiedSinceWrite(session, key string) bool {
	if s == nil || key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.by[session]
	if !ok {
		return false
	}
	seq, ok := st.satisfy[key]
	if !ok {
		return false
	}
	return seq >= st.writeSeq
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
	// Events is the per-session cross-event log for require rules.
	// Created automatically by NewEngine / NewEngineFromStore when nil.
	Events *SessionEvents
	// Log receives audit write failures. Nil falls back to slog.Default():
	// a lost audit row must never be silent (#297).
	Log *slog.Logger
}

// Decision is the outcome of evaluating a tool call.
type Decision struct {
	Allowed  bool
	Rule     string
	Verdict  string // allow | deny | require
	Message  string
	Guidance string
}

// NewEngine builds an engine with compiled rules and a session event log.
// Returns an error if any rule fails Compile (including empty selectors).
func NewEngine(rules []Rule, enforcer string) (*Engine, error) {
	e := &Engine{Rules: rules, Enforcer: enforcer, Events: NewSessionEvents()}
	if e.Enforcer == "" {
		e.Enforcer = EnforcerNone
	}
	for i := range e.Rules {
		if err := e.Rules[i].Compile(); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// Check evaluates rules against a tool invocation. name is the tool name;
// input is the JSON tool input (bash uses {"command":"..."}).
// Session-scoped require rules use CheckSession / CheckAndAuditSession.
func (e *Engine) Check(name string, input json.RawMessage) Decision {
	return e.CheckSession("", name, input)
}

// CheckSession is Check with an explicit session id for require evaluation.
func (e *Engine) CheckSession(session, name string, input json.RawMessage) Decision {
	d, _ := e.checkSessionCmd(session, name, input)
	return d
}

// checkSessionCmd is CheckSession but also returns the extracted command, so
// callers that need it for audit logging don't re-extract it.
func (e *Engine) checkSessionCmd(session, name string, input json.RawMessage) (Decision, string) {
	if e.Events == nil {
		e.Events = NewSessionEvents()
	}
	cmd := extractCommand(name, input)
	for _, r := range e.Rules {
		if !r.matches(name, cmd) {
			continue
		}
		switch r.Action {
		case ActionRequire:
			d := Decision{
				Rule:     r.Name,
				Verdict:  ActionRequire,
				Guidance: r.Guidance,
			}
			if e.Events.SatisfiedSinceWrite(session, r.Requires) {
				d.Allowed = true
				d.Verdict = ActionAllow
				return d, cmd
			}
			d.Allowed = false
			d.Verdict = ActionDeny
			d.Message = e.requireDenyMessage(r)
			return d, cmd
		case ActionDeny:
			d := Decision{
				Rule:     r.Name,
				Verdict:  ActionDeny,
				Guidance: r.Guidance,
				Allowed:  false,
			}
			d.Message = e.denyMessage(r, cmd)
			return d, cmd
		default: // allow (and unknown treated as allow after validation)
			return Decision{
				Allowed:  true,
				Rule:     r.Name,
				Verdict:  ActionAllow,
				Guidance: r.Guidance,
			}, cmd
		}
	}
	return Decision{Allowed: true, Verdict: ActionAllow}, cmd
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

// requireDenyMessage always includes rule name and guidance as "because" text.
// Format: blocked by policy "name": guidance
func (e *Engine) requireDenyMessage(r Rule) string {
	msg := fmt.Sprintf("blocked by policy %q", r.Name)
	if r.Guidance != "" {
		msg += ": " + r.Guidance
	} else if r.Requires != "" {
		msg += fmt.Sprintf(": require %q after last edit", r.Requires)
	}
	return msg
}

// ObserveSuccess records post-tool success for require predicates and writes.
// Call after a tool runs successfully (not on denial/error).
func (e *Engine) ObserveSuccess(session, tool string, input json.RawMessage) {
	if e == nil {
		return
	}
	if e.Events == nil {
		e.Events = NewSessionEvents()
	}
	switch tool {
	case "write_file", "edit_file":
		e.Events.NoteWrite(session)
	}
	cmd := extractCommand(tool, input)
	for _, r := range e.Rules {
		// Matching allow (or any matching rule name) satisfies by rule name.
		if r.matches(tool, cmd) {
			if r.Name != "" {
				e.Events.NoteSatisfy(session, r.Name)
			}
		}
		// Also: successful bash whose command matches a Requires string as prefix
		// satisfies that key (e.g. Requires="go test" or rule name elsewhere).
		if tool == "bash" && r.Requires != "" && MatchBashPrefix(cmd, r.Requires) {
			e.Events.NoteSatisfy(session, r.Requires)
		}
		// Successful bash matching another rule's Match when that rule is the
		// require target (already handled via r.matches + Name above).
	}
}

// CheckAndAudit is Check plus optional policy_audit write.
func (e *Engine) CheckAndAudit(ctx context.Context, name string, input json.RawMessage) Decision {
	return e.CheckAndAuditSession(ctx, e.SessionID, name, input)
}

// CheckAndAuditSession is CheckAndAudit with an explicit session id (preferred
// under concurrent serve).
func (e *Engine) CheckAndAuditSession(ctx context.Context, sessionID, name string, input json.RawMessage) Decision {
	d, cmd := e.checkSessionCmd(sessionID, name, input)
	if e.AuditDB != nil && (d.Rule != "" || !d.Allowed) {
		err := LogAudit(ctx, e.AuditDB, sessionID, name, cmd, d)
		ReportAuditFailure(e.Log, err, sessionID, name, d.Verdict)
	}
	return d
}

// ErrAuditNotRecorded marks an admitted, durable mutation whose policy_audit
// row was lost (#297). Callers that already committed wrap their write failure
// with it so the result can be reported as unaudited rather than as a clean
// success.
var ErrAuditNotRecorded = errors.New("policy audit record not written")

// ReportAuditFailure logs a lost policy_audit write (#297). The repository
// documents every matching policy decision and admitted mutation as audited,
// so a dropped audit row is itself a security-relevant event and must never be
// silent — callers without a logger fall back to slog.Default().
//
// operation carries a caller-constructed label that is known not to contain
// audited content — a verdict, a route, a stage id. Never pass the audited
// command or any other caller-supplied value through it: bash commands, skill
// names, and source refs can carry secrets that do not belong in the host log.
func ReportAuditFailure(log *slog.Logger, err error, session, tool, operation string) {
	if err == nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	log.Error("policy audit write failed",
		"session", session,
		"tool", tool,
		"operation", operation,
		"err", err,
	)
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

// AuditEntry is one recorded policy decision, read back so a denied tool call
// can be traced to the rule that denied it (#193).
type AuditEntry struct {
	At      string
	Session string
	Tool    string
	Command string
	Rule    string
	Verdict string
	Detail  string
}

// RecentDenials returns the most recent non-allow decisions, newest first.
// session narrows to one conversation; empty returns denials across all
// sessions. limit is clamped to a sane page so a caller cannot ask for the
// whole table.
func RecentDenials(ctx context.Context, db *sql.DB, session string, limit int) ([]AuditEntry, error) {
	if db == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := `
		SELECT at, session, tool, command, rule, verdict, detail
		FROM policy_audit
		WHERE verdict <> ?`
	args := []any{ActionAllow}
	if session != "" {
		query += ` AND session = ?`
		args = append(args, session)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read policy audit: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []AuditEntry
	for rows.Next() {
		var entry AuditEntry
		if err := rows.Scan(&entry.At, &entry.Session, &entry.Tool,
			&entry.Command, &entry.Rule, &entry.Verdict, &entry.Detail); err != nil {
			return nil, fmt.Errorf("read policy audit: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read policy audit: %w", err)
	}
	return entries, nil
}

// LogMutation always records a non-tool mutation into policy_audit.
// Desk and skillinstall call this so their surfaces share the same audit trail
// as tool decisions, even when no [[policy.rule]] matched.
func LogMutation(ctx context.Context, db *sql.DB, session, tool, command, detail string) error {
	if tool == "" {
		tool = "mutation"
	}
	return LogAudit(ctx, db, session, tool, command, Decision{
		Allowed: true,
		Verdict: ActionAllow,
		Rule:    tool,
		Message: detail,
	})
}

// NewEngineFromStore builds an engine with optional audit persistence.
// Returns an error if any rule fails Compile (including empty selectors).
func NewEngineFromStore(st *store.Store, rules []Rule, enforcer string) (*Engine, error) {
	e, err := NewEngine(rules, enforcer)
	if err != nil {
		return nil, err
	}
	if st != nil {
		e.AuditDB = st.DB
	}
	return e, nil
}

// Narrow merges child rules under a parent so the child may only *narrow*
// the parent domain (hierarchical-domain rule). A child rule that allows a
// tool+match the parent denies is rejected at construction.
func Narrow(parent, child []Rule) ([]Rule, error) {
	// Compile regexes for matching checks.
	pRules := append([]Rule(nil), parent...)
	cRules := append([]Rule(nil), child...)
	for i := range pRules {
		if err := pRules[i].Compile(); err != nil {
			return nil, err
		}
	}
	for i := range cRules {
		if err := cRules[i].Compile(); err != nil {
			return nil, err
		}
	}
	for _, c := range cRules {
		if c.Action != ActionAllow {
			continue
		}
		// Regex allows cannot be soundly probed as a subset of the parent
		// domain (and would first-match-win over later parent denials). Fail closed.
		if c.Regex != "" {
			return nil, fmt.Errorf("child policy %q: regex allow rules cannot be verified as narrowing parent (refusing unverifiable widen)", c.Name)
		}
		// Probe: does any parent deny rule match the same domain as this allow?
		// Use child Match (or empty cmd for tool-only) as the command probe.
		probeCmd := c.Match
		toolName := c.Tool
		if toolName == "" {
			toolName = "bash"
		}
		for _, p := range pRules {
			if p.Action != ActionDeny {
				continue
			}
			if !p.matches(toolName, probeCmd) {
				// Also try whether child match would be denied under parent when
				// parent is tool-only deny.
				if p.Tool != "" && p.Tool == toolName && p.Match == "" && p.Regex == "" {
					return nil, fmt.Errorf("child policy %q allows %s which parent rule %q denies", c.Name, toolName, p.Name)
				}
				continue
			}
			return nil, fmt.Errorf("child policy %q allows %s %q which parent rule %q denies", c.Name, toolName, probeCmd, p.Name)
		}
	}
	// Child rules first (more specific), then parent — first match wins at runtime.
	out := make([]Rule, 0, len(cRules)+len(pRules))
	out = append(out, cRules...)
	out = append(out, pRules...)
	return out, nil
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
	return MatchBashPrefix(cmd, r.Match)
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

// MatchBashPrefix reports whether cmd's leading tokens match the prefix string
// (itself split with the same quote rules).
func MatchBashPrefix(cmd, prefix string) bool {
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
	return textcut.Cut(s, n) + "…"
}
