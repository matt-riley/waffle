package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/spill"
	"github.com/matt-riley/waffle/internal/textcut"
	"github.com/matt-riley/waffle/internal/tool"
	"github.com/matt-riley/waffle/internal/usage"
)

// mutationTools names tools that write state (files, workspace, memory) or
// run arbitrary commands. Read-only subagent packets deny these, and profile
// widening checks treat them as the set that must not silently reappear via
// a wildcard allow.
var mutationTools = []string{"write_file", "edit_file", "bash", "workspace_update", "remember", "distill_skill", "memory_update"}

// ChildProfile is a named specialist posture for spawn_subagent (#71).
// Tools may only tighten the parent's toolbox (deny more / narrow allow).
// Explicit allow entries outside the parent toolbox reject the profile.
type ChildProfile struct {
	// Name is recorded in handoff/logs.
	Name string
	// System replaces the default subagent system prompt when non-empty
	// (packet framing and working-set broadcast are still appended).
	System string
	// Model overrides the parent model when non-empty.
	Model string
	// Tools is intersected with the parent toolbox (tighten-only); explicit
	// allow entries outside the parent toolbox are rejected before child setup.
	Tools tool.Policy
	// RequestedTools preserves the profile's configured policy before parent
	// denials are inherited. Widening checks use this policy so narrowing cannot
	// erase an explicit request for capabilities the parent does not have.
	RequestedTools tool.Policy
	// MaxTokens overrides parent when > 0.
	MaxTokens int
}

// SubagentTool spawns a fresh agent for an isolated task and returns its
// final answer (docs/plan.md, "subagents ... reporting back to a parent").
// The subagent shares the parent's provider by default but starts with a
// clean history. Named profiles (#71) may override system/model/tools
// without widening privilege beyond the parent toolbox.
//
// Nested spawn_subagent is omitted from child toolboxes. If that ever
// changes, children must inherit the same read-only working-set broadcast
// snapshot — never a widened mutation authority (#68).
type SubagentTool struct {
	Provider  llm.Provider
	Tools     tool.Toolbox
	Model     string
	MaxTokens int
	Redact    func(string) string
	Depth     int
	// WorkingSetBroadcast is optional rendered parent working set (#68).
	// When non-empty it is used as-is (frozen snapshot from the parent).
	WorkingSetBroadcast string
	BroadcastWorkingSet bool
	// WorkingSetSnapshot, when set and WorkingSetBroadcast is empty, is called
	// once per Run to capture the parent set as of dispatch (#68). For parallel
	// spawns, prefer a pre-frozen WorkingSetBroadcast so all children share one view.
	WorkingSetSnapshot func(ctx context.Context) string
	Spill              *spill.Store
	// Profiles maps profile name → child posture (#71).
	Profiles map[string]ChildProfile
	// AllowedProfiles, when non-empty, is the only set the parent may
	// delegate to. Empty means any configured profile (or none).
	AllowedProfiles []string
	// Persist records packet+handoff against a child session when set (#78).
	Persist func(ctx context.Context, parentSession, childSession string, packet WorkPacket, handoff Handoff) error
	// PersistTimeout bounds the detached persistence write so a blocked or
	// busy store cannot hang the parent run after cancellation (#298).
	// Zero uses the 30s default.
	PersistTimeout time.Duration
	// NewChildSession creates a session for the child when Persist is set.
	NewChildSession func(ctx context.Context, title string) (sessionID string, err error)
	// Usage and Limits mirror the parent Agent so child runs share budget checks
	// and accounting (#96). When a child session is attached, spend is still
	// charged to the parent budget key via usage.WithBudgetKey.
	Usage  *usage.Store
	Limits usage.Limits
	Log    *slog.Logger
}

func (t SubagentTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "spawn_subagent",
		Description: "Delegate a self-contained subtask to a fresh agent with its own context, and get back a structured handoff. Prefer a full work packet (task + optional owned_paths, acceptance_criteria, verify_commands). Legacy {\"task\":\"...\"} still works. Optional profile selects a named agent profile when configured.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"task": {"type": "string", "description": "The complete, self-contained instruction for the subagent"},
				"role": {"type": "string"},
				"context_refs": {"type": "array", "items": {"type": "string"}},
				"owned_paths": {"type": "array", "items": {"type": "string"}},
				"acceptance_criteria": {"type": "array", "items": {"type": "string"}},
				"verify_commands": {"type": "array", "items": {"type": "string"}},
				"read_only": {"type": "boolean"},
				"profile": {"type": "string", "description": "optional named agent profile (#71)"}
			},
			"required": ["task"]
		}`),
	}
}

func (t SubagentTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	if t.Depth >= 3 {
		return "", fmt.Errorf("subagent depth limit reached")
	}
	var p WorkPacket
	p, err := ParseWorkPacket(input)
	if err != nil {
		return "", fmt.Errorf("bad input: %w", err)
	}

	profileName := strings.TrimSpace(p.Profile)
	if profileName == "" {
		profileName = strings.TrimSpace(p.Role) // Role may map to a profile name
	}
	var profile ChildProfile
	if profileName != "" {
		if len(t.AllowedProfiles) > 0 && !slices.Contains(t.AllowedProfiles, profileName) {
			return "", fmt.Errorf("profile %q is not in the parent's allowed child set", profileName)
		}
		cp, ok := t.Profiles[profileName]
		if !ok {
			return "", fmt.Errorf("unknown agent profile %q", profileName)
		}
		profile = cp
		profile.Name = profileName
		requestedTools := profile.RequestedTools
		if requestedTools.IsZero() {
			// Preserve compatibility for callers that construct ChildProfile
			// directly rather than through config policy resolution.
			requestedTools = profile.Tools
		}
		if widened := profileWideningTool(t.Tools, requestedTools); widened != "" {
			return "", fmt.Errorf("profile %q requests tool %q outside the parent toolbox", profileName, widened)
		}
	}
	if t.Log != nil {
		t.Log.Info("subagent spawn", "profile", effectiveProfile(profileName))
	}

	sys := "You are a waffle subagent handling one self-contained task. Do the work with your tools and end with a concise report of what you found or did. You have no access to the parent conversation."
	if profile.System != "" {
		sys = profile.System
	}
	broadcast := t.WorkingSetBroadcast
	if t.BroadcastWorkingSet && broadcast == "" && t.WorkingSetSnapshot != nil {
		broadcast = t.WorkingSetSnapshot(ctx)
	}
	if t.BroadcastWorkingSet && broadcast != "" {
		sys += "\n\n" + broadcast
		sys += "\nThe working set above is read-only. To suggest changes, include proposals in your JSON handoff; they are NOT applied automatically."
	}
	sys += "\n\n" + FramePacket(p)
	if profile.Name != "" {
		sys += "\n[child_profile=" + profile.Name + "]\n"
	}

	// Subagent toolboxes must never include workspace_update or spawn (#68).
	childTools := t.Tools
	if childTools != nil {
		childProfile := effectiveProfile(profileName)
		deny := tool.Policy{Deny: []string{"workspace_update", "spawn_subagent"}, Profile: childProfile}
		if !profile.Tools.IsZero() {
			// Tighten-only: apply profile policy, then force parent denials.
			profilePolicy := profile.Tools
			profilePolicy.Profile = childProfile
			childTools = tool.Restrict(childTools, profilePolicy)
		}
		childTools = tool.Restrict(childTools, deny)
	}

	// Read-only packets strip mutation tools.
	if p.ReadOnly && childTools != nil {
		childTools = tool.Restrict(childTools, tool.Policy{
			Deny:    mutationTools,
			Profile: effectiveProfile(profileName),
		})
	}

	model := t.Model
	if profile.Model != "" {
		model = profile.Model
	}
	maxTok := t.MaxTokens
	if profile.MaxTokens > 0 {
		maxTok = profile.MaxTokens
	}

	var childSession string
	if t.NewChildSession != nil {
		if id, err := t.NewChildSession(ctx, "subagent: "+truncate(p.Task, 40)); err == nil {
			childSession = id
		}
	}

	sub := &Agent{
		Provider:      t.Provider,
		Tools:         childTools,
		System:        sys,
		Model:         model,
		MaxTokens:     maxTok,
		Redact:        t.Redact,
		Spill:         t.Spill,
		Usage:         t.Usage,
		Limits:        t.Limits,
		MaxIterations: 30,
		Profile:       effectiveProfile(profileName),
		Log:           t.Log,
	}
	runCtx := ctx
	if childSession != "" {
		// Keep transcript isolation on the child session, but charge spend to
		// the parent budget key so parent limits still apply (#96).
		parentBudget := usage.BudgetKey(ctx, SessionID(ctx))
		runCtx = WithSession(ctx, childSession)
		runCtx = usage.WithBudgetKey(runCtx, parentBudget)
	}
	history, err := sub.Run(runCtx, []llm.Message{llm.UserText(p.Task)}, Hooks{})
	if err != nil {
		h := Handoff{Status: "failed", Summary: err.Error()}
		if profile.Name != "" {
			h.Reasons = append(h.Reasons, "profile="+profile.Name)
		}
		return FormatHandoffResult(h), nil
	}
	var text string
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == llm.RoleAssistant {
			text = history[i].Text()
			break
		}
	}
	if text == "" {
		return FormatHandoffResult(Handoff{Status: "failed", Summary: "subagent produced no answer"}), nil
	}

	h, err := ParseHandoff(text)
	if err != nil {
		// One repair attempt: ask the model for a valid JSON handoff (#78).
		repaired, rerr := t.repairHandoff(runCtx, sub, text)
		if rerr != nil {
			h = Handoff{Status: "failed", Summary: text, Reasons: []string{"malformed handoff; repair failed: " + rerr.Error()}}
		} else {
			h = repaired
		}
	}
	// Observed verification: run VerifyCommands through the child toolbox (#78).
	if len(p.VerifyCommands) > 0 && childTools != nil {
		h.Verification = append(h.Verification, runObservedVerification(runCtx, childTools, p.VerifyCommands)...)
	}
	h = NormalizeHandoff(h, p)
	if profile.Name != "" {
		h.Reasons = append(h.Reasons, "profile="+profile.Name)
	}

	if t.Persist != nil && childSession != "" {
		// Persist on a bounded detached context: the parent run may already
		// be canceled, and an unbounded write must not hold the tool forever.
		// A failed write means the handoff will not survive a restart, so the
		// parent must not see a plain success (#298).
		timeout := t.PersistTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		if err := t.Persist(persistCtx, SessionID(ctx), childSession, p, h); err != nil {
			if t.Log != nil {
				t.Log.Error("persist subagent handoff", "err", err, "parent_session", SessionID(ctx), "child_session", childSession)
			}
			return "", fmt.Errorf("persist subagent handoff: %w", err)
		}
	}
	return FormatHandoffResult(h), nil
}

func profileWideningTool(parent tool.Toolbox, policy tool.Policy) string {
	if parent == nil || len(policy.Allow) == 0 {
		return ""
	}
	available := make(map[string]struct{})
	for _, def := range parent.Defs() {
		available[def.Name] = struct{}{}
	}
	wildcard := false
	for _, name := range policy.Allow {
		if name == "*" {
			wildcard = true
			continue
		}
		if _, ok := available[name]; !ok {
			return name
		}
	}
	if wildcard {
		for _, name := range mutationTools {
			if _, ok := available[name]; !ok && policy.Permits(name) {
				return name
			}
		}
	}
	return ""
}

func (t SubagentTool) repairHandoff(ctx context.Context, sub *Agent, broken string) (Handoff, error) {
	if sub.Provider == nil {
		return Handoff{}, fmt.Errorf("no provider for repair")
	}
	// Match Agent.Run budget/pause checks so repair Completes cannot bypass limits (#96).
	if sub.Usage != nil {
		if paused, err := sub.Usage.Paused(ctx); err != nil {
			return Handoff{}, err
		} else if paused {
			return Handoff{}, errors.New("waffle is paused")
		}
		if err := sub.Usage.Check(ctx, usage.BudgetKey(ctx, SessionID(ctx)), sub.Limits, time.Now()); err != nil {
			return Handoff{}, err
		}
	}
	prompt := llm.UserText("Your previous reply was not a valid handoff JSON object. Reply with ONLY a ```json fenced object with status (done|partial|blocked|failed) and summary. Previous reply was:\n" + truncate(broken, 2000))
	resp, err := sub.Provider.Complete(ctx, llm.Request{
		Model:     sub.Model,
		System:    "Emit only a valid waffle handoff JSON object.",
		Messages:  []llm.Message{prompt},
		MaxTokens: 512,
	}, nil)
	if err != nil {
		return Handoff{}, err
	}
	if sub.Usage != nil {
		_ = sub.Usage.AddRequest(ctx, usage.BudgetKey(ctx, SessionID(ctx)), resp.Usage)
	}
	return ParseHandoff(resp.Message.Text())
}

func runObservedVerification(ctx context.Context, tb tool.Toolbox, cmds []string) []VerificationResult {
	var out []VerificationResult
	for _, cmd := range cmds {
		input, _ := json.Marshal(map[string]string{"command": cmd})
		res, err := tb.Run(ctx, "bash", input)
		vr := VerificationResult{Command: cmd, Observed: true}
		if err != nil {
			vr.Status = "fail"
			vr.Output = err.Error()
		} else {
			vr.Status = "pass"
			vr.Output = truncate(res, 500)
			// Heuristic: non-empty "error:" prefix counts as fail.
			if strings.HasPrefix(strings.ToLower(res), "error:") {
				vr.Status = "fail"
			}
		}
		out = append(out, vr)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary so multi-byte text (emoji, accents) stays valid UTF-8 (#107).
	return textcut.Cut(s, n) + "…"
}
