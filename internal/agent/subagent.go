package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/spill"
	"github.com/matt-riley/waffle/internal/tool"
)

// ChildProfile is a named specialist posture for spawn_subagent (#71).
// Tools may only tighten the parent's toolbox (deny more / narrow allow).
type ChildProfile struct {
	// Name is recorded in handoff/logs.
	Name string
	// System replaces the default subagent system prompt when non-empty
	// (packet framing and working-set broadcast are still appended).
	System string
	// Model overrides the parent model when non-empty.
	Model string
	// Tools is intersected with the parent toolbox (tighten-only).
	Tools tool.Policy
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
	// NewChildSession creates a session for the child when Persist is set.
	NewChildSession func(ctx context.Context, title string) (sessionID string, err error)
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
		if len(t.AllowedProfiles) > 0 && !containsStr(t.AllowedProfiles, profileName) {
			return "", fmt.Errorf("profile %q is not in the parent's allowed child set", profileName)
		}
		cp, ok := t.Profiles[profileName]
		if !ok {
			return "", fmt.Errorf("unknown agent profile %q", profileName)
		}
		profile = cp
		profile.Name = profileName
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
		deny := tool.Policy{Deny: []string{"workspace_update", "spawn_subagent"}}
		if !profile.Tools.IsZero() {
			// Tighten-only: apply profile policy, then force parent denials.
			childTools = tool.Restrict(childTools, profile.Tools)
		}
		childTools = tool.Restrict(childTools, deny)
	}

	// Read-only packets strip mutation tools.
	if p.ReadOnly && childTools != nil {
		childTools = tool.Restrict(childTools, tool.Policy{Deny: []string{
			"write_file", "edit_file", "bash", "workspace_update", "remember", "distill_skill", "memory_update",
		}})
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
		MaxIterations: 30,
	}
	runCtx := ctx
	if childSession != "" {
		runCtx = WithSession(ctx, childSession)
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
		_ = t.Persist(context.WithoutCancel(ctx), sessionID(ctx), childSession, p, h)
	}
	return FormatHandoffResult(h), nil
}

func (t SubagentTool) repairHandoff(ctx context.Context, sub *Agent, broken string) (Handoff, error) {
	if sub.Provider == nil {
		return Handoff{}, fmt.Errorf("no provider for repair")
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

func containsStr(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
