package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/matt-riley/waffle/internal/workset"
)

// WorkPacket is the typed subagent task contract (#78).
type WorkPacket struct {
	Task               string   `json:"task"`
	Role               string   `json:"role,omitempty"`
	Profile            string   `json:"profile,omitempty"` // named agent profile (#71)
	ContextRefs        []string `json:"context_refs,omitempty"`
	OwnedPaths         []string `json:"owned_paths,omitempty"`
	AcceptanceCriteria []string `json:"acceptance_criteria,omitempty"`
	VerifyCommands     []string `json:"verify_commands,omitempty"`
	ReadOnly           bool     `json:"read_only,omitempty"`
}

// Handoff is the validated child result contract (#78).
type Handoff struct {
	Status       string               `json:"status"` // done|partial|blocked|failed
	Summary      string               `json:"summary"`
	Findings     []Finding            `json:"findings,omitempty"`
	FilesChanged []string             `json:"files_changed,omitempty"`
	Verification []VerificationResult `json:"verification,omitempty"`
	Proposals    []workset.Proposal   `json:"proposals,omitempty"`
	Reasons      []string             `json:"reasons,omitempty"`
}

// Finding is one structured discovery.
type Finding struct {
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// VerificationResult records a check.
type VerificationResult struct {
	Command  string `json:"command"`
	Status   string `json:"status"`   // pass|fail|skipped
	Observed bool   `json:"observed"` // waffle-executed vs child-reported
	Output   string `json:"output,omitempty"`
}

// FramePacket renders the packet for the child system/user prompt.
func FramePacket(p WorkPacket) string {
	var b strings.Builder
	b.WriteString("<work_packet>\n")
	fmt.Fprintf(&b, "task: %s\n", p.Task)
	if p.Role != "" {
		fmt.Fprintf(&b, "role: %s\n", p.Role)
	}
	if len(p.ContextRefs) > 0 {
		fmt.Fprintf(&b, "context_refs: %s\n", strings.Join(p.ContextRefs, ", "))
	}
	if len(p.OwnedPaths) > 0 {
		fmt.Fprintf(&b, "owned_paths: %s\n", strings.Join(p.OwnedPaths, ", "))
	}
	if len(p.AcceptanceCriteria) > 0 {
		b.WriteString("acceptance_criteria:\n")
		for _, c := range p.AcceptanceCriteria {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
	}
	if len(p.VerifyCommands) > 0 {
		b.WriteString("verify_commands:\n")
		for _, c := range p.VerifyCommands {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
	}
	if p.ReadOnly {
		b.WriteString("read_only: true\n")
	}
	b.WriteString("End with a single JSON handoff object in a ```json fenced block with keys status, summary, and optional findings/files_changed/verification/proposals.\n")
	b.WriteString("</work_packet>\n")
	return b.String()
}

// ParseHandoff extracts a JSON handoff from assistant text.
func ParseHandoff(text string) (Handoff, error) {
	raw := extractJSONBlock(text)
	if raw == "" {
		return Handoff{}, fmt.Errorf("no handoff JSON block")
	}
	var h Handoff
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		return Handoff{}, err
	}
	switch h.Status {
	case "done", "partial", "blocked", "failed":
	default:
		return Handoff{}, fmt.Errorf("invalid status %q", h.Status)
	}
	if strings.TrimSpace(h.Summary) == "" {
		return Handoff{}, fmt.Errorf("summary required")
	}
	seenPaths := make(map[string]struct{}, len(h.FilesChanged))
	for _, path := range h.FilesChanged {
		if _, ok := seenPaths[path]; ok {
			return Handoff{}, fmt.Errorf("duplicate changed path %q", path)
		}
		seenPaths[path] = struct{}{}
	}
	return h, nil
}

func extractJSONBlock(text string) string {
	// Prefer fenced ```json ... ```
	if i := strings.Index(text, "```json"); i >= 0 {
		rest := text[i+7:]
		if j := strings.Index(rest, "```"); j >= 0 {
			return strings.TrimSpace(rest[:j])
		}
	}
	return ""
}

// NormalizeHandoff applies verification/scope rules (#78).
func NormalizeHandoff(h Handoff, p WorkPacket) Handoff {
	if h.Status == "done" && len(p.VerifyCommands) > 0 {
		seen := make(map[string]bool, len(h.Verification))
		for _, v := range h.Verification {
			seen[v.Command] = true
			if v.Status == "fail" || v.Status == "skipped" {
				h.Status = "partial"
				h.Reasons = append(h.Reasons, "verification incomplete or failed")
			}
		}
		for _, command := range p.VerifyCommands {
			if !seen[command] {
				h.Status = "partial"
				h.Reasons = append(h.Reasons, "requested verification missing: "+command)
			}
		}
	}
	if p.ReadOnly && len(h.FilesChanged) > 0 {
		h.Status = "blocked"
		h.Reasons = append(h.Reasons, "read_only packet reported file changes")
	}
	if len(p.OwnedPaths) > 0 {
		for _, f := range h.FilesChanged {
			if !pathUnderOwned(f, p.OwnedPaths) {
				h.Reasons = append(h.Reasons, "needs_supervisor_review: "+f)
				if h.Status == "done" {
					h.Status = "partial"
				}
			}
		}
	}
	// Validate proposals; drop invalid with reason.
	var good []workset.Proposal
	for _, prop := range h.Proposals {
		if err := workset.ValidateProposal(prop); err != nil {
			h.Reasons = append(h.Reasons, "rejected proposal: "+err.Error())
			continue
		}
		good = append(good, prop)
	}
	h.Proposals = good
	return h
}

func pathUnderOwned(path string, owned []string) bool {
	path = strings.TrimPrefix(path, "./")
	for _, o := range owned {
		o = strings.TrimPrefix(o, "./")
		if path == o || strings.HasPrefix(path, strings.TrimSuffix(o, "/")+"/") {
			return true
		}
	}
	return false
}

// FormatHandoffResult renders the tool result with not-applied proposal marker.
func FormatHandoffResult(h Handoff) string {
	b, _ := json.MarshalIndent(h, "", "  ")
	out := string(b)
	if len(h.Proposals) > 0 {
		out += "\n\n[WORKING_SET_PROPOSALS — not applied; parent/owner must accept explicitly]"
	}
	return out
}
