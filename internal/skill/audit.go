package skill

import (
	"regexp"
	"strings"

	"github.com/matt-riley/waffle/internal/textcut"
)

var (
	// toolErrorRE matches agent-loop tool error prefixes.
	toolErrorRE = regexp.MustCompile(`(?i)(?:error:\s*|exit status \d+\n)(.{8,160})`)
	// collapse space for fingerprinting.
	spaceRE = regexp.MustCompile(`\s+`)
	// drop volatile tokens (paths, numbers, hex ids).
	volatileRE = regexp.MustCompile(`(?i)(/[^\s]+)|(\b[0-9a-f]{8,}\b)|(\b\d+\b)`)
)

func fingerprintError(content string) (sig, sample string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return "", ""
	}
	sample = content
	if len(sample) > 120 {
		sample = textcut.Cut(sample, 120) + "…"
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
		raw = textcut.Cut(raw, 80)
	}
	return raw, sample
}
