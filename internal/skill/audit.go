package skill

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/matt-riley/waffle/internal/llm"
	"github.com/matt-riley/waffle/internal/memory"
	"github.com/matt-riley/waffle/internal/session"
)

// ErrorPattern is a recurring tool-error signature found in turns (#65).
type ErrorPattern struct {
	// Signature is a normalized error fingerprint.
	Signature string
	Count     int
	// Samples are short example lines.
	Samples []string
}

var (
	// toolErrorRE matches agent-loop tool error prefixes.
	toolErrorRE = regexp.MustCompile(`(?i)(?:error:\s*|exit status \d+\n)(.{8,160})`)
	// collapse space for fingerprinting.
	spaceRE = regexp.MustCompile(`\s+`)
	// drop volatile tokens (paths, numbers, hex ids).
	volatileRE = regexp.MustCompile(`(?i)(/[^\s]+)|(\b[0-9a-f]{8,}\b)|(\b\d+\b)`)
)

// MineToolErrors scans recent session turns for tool-error patterns (#65).
func MineToolErrors(ctx context.Context, sessions *session.Store, sessionLimit int) ([]ErrorPattern, error) {
	if sessions == nil {
		return nil, fmt.Errorf("no session store")
	}
	if sessionLimit <= 0 {
		sessionLimit = 20
	}
	list, err := sessions.List(ctx, sessionLimit)
	if err != nil {
		return nil, err
	}
	counts := map[string]*ErrorPattern{}
	for _, sess := range list {
		turns, err := sessions.Turns(ctx, sess.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range turns {
			for _, b := range m.Blocks {
				if b.Type != llm.BlockToolResult || b.ToolResult == nil {
					continue
				}
				if !b.ToolResult.IsError && !strings.Contains(strings.ToLower(b.ToolResult.Content), "error:") {
					continue
				}
				content := b.ToolResult.Content
				sig, sample := fingerprintError(content)
				if sig == "" {
					continue
				}
				p := counts[sig]
				if p == nil {
					p = &ErrorPattern{Signature: sig}
					counts[sig] = p
				}
				p.Count++
				if len(p.Samples) < 3 {
					p.Samples = append(p.Samples, sample)
				}
			}
		}
	}
	out := make([]ErrorPattern, 0, len(counts))
	for _, p := range counts {
		if p.Count >= 2 { // only recurring
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Signature < out[j].Signature
	})
	return out, nil
}

func fingerprintError(content string) (sig, sample string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", ""
	}
	sample = content
	if len(sample) > 120 {
		sample = sample[:120] + "…"
	}
	m := toolErrorRE.FindStringSubmatch(content)
	raw := content
	if len(m) > 1 {
		raw = m[1]
	}
	raw = volatileRE.ReplaceAllString(raw, "#")
	raw = spaceRE.ReplaceAllString(strings.ToLower(raw), " ")
	raw = strings.TrimSpace(raw)
	if len(raw) < 8 {
		return "", ""
	}
	if len(raw) > 80 {
		raw = raw[:80]
	}
	return raw, sample
}

// ProposeSkills writes pending skill candidates for recurring error patterns
// via the memory write gate (#65). Returns how many candidates were submitted.
func ProposeSkills(ctx context.Context, patterns []ErrorPattern, gate *memory.Gate, minCount int) (int, error) {
	if gate == nil {
		return 0, fmt.Errorf("no gate")
	}
	if minCount <= 0 {
		minCount = 2
	}
	n := 0
	for _, p := range patterns {
		if p.Count < minCount {
			continue
		}
		name := skillNameFromSignature(p.Signature)
		body := fmt.Sprintf("# Recover from: %s\n\nSeen %d times recently.\n\n## Samples\n\n", p.Signature, p.Count)
		for _, s := range p.Samples {
			body += "- " + s + "\n"
		}
		body += "\n## Suggested approach\n\n1. Reproduce with the same tool input.\n2. Fix the root cause (permissions, missing deps, bad path).\n3. Re-run and confirm the error is gone.\n"
		c := memory.Candidate{
			Kind:        "skill",
			Name:        name,
			Description: "auto-mined recovery for recurring tool error: " + p.Signature,
			Body:        body,
			Provenance: memory.Provenance{
				SourceKind: "reflection",
				TrustClass: "model_derived",
			},
		}
		// Always pending for mined skills: use untrusted so gate requires review.
		c.Provenance.TrustClass = "untrusted_derived"
		if _, err := gate.SubmitForReview(c); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func skillNameFromSignature(sig string) string {
	s := strings.ToLower(sig)
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "tool-error"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return "recover-" + s
}
